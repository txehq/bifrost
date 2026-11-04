package tokun

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/pkoukk/tiktoken-go"
	tiktokenloader "github.com/pkoukk/tiktoken-go-loader"

	routingv1 "github.com/capsohq/txe/tokun/proto/gen/go/tokun/routing/v1"
)

// testTokenEstimate is the initial values of the estimate's runtime settings (txe migrations 0157
// and 0163).
func testTokenEstimate() *routingv1.TokenEstimate {
	return &routingv1.TokenEstimate{
		InputBytesPerToken:  3,
		InputOverheadTokens: 512,
		ImagePatchPixels:    28,
		ImageMaxEdgePixels:  2576,
		ImageMaxTokens:      4784,
		DocumentPageTokens:  4568,
	}
}

// encodedImage is a width x height image encoded by encode, in base64.
func encodedImage(t testing.TB, width, height int, encode func(*bytes.Buffer, image.Image) error) string {
	t.Helper()
	var buf bytes.Buffer
	if err := encode(&buf, image.NewGray(image.Rect(0, 0, width, height))); err != nil {
		t.Fatalf("encode a %dx%d image: %v", width, height, err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func pngImage(buf *bytes.Buffer, img image.Image) error  { return png.Encode(buf, img) }
func jpegImage(buf *bytes.Buffer, img image.Image) error { return jpeg.Encode(buf, img, nil) }
func gifImage(buf *bytes.Buffer, img image.Image) error  { return gif.Encode(buf, img, nil) }

// pdfDocument is a PDF of the given number of empty pages, in base64: a catalog, a page tree and
// the pages, with a cross-reference table of their offsets.
func pdfDocument(pages int) string {
	var b bytes.Buffer
	var offsets []int
	object := func(body string) {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	b.WriteString("%PDF-1.4\n")
	kids := make([]string, pages)
	for i := range kids {
		kids[i] = fmt.Sprintf("%d 0 R", 3+i)
	}
	object("<< /Type /Catalog /Pages 2 0 R >>")
	object(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), pages))
	for range pages {
		object("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>")
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return base64.StdEncoding.EncodeToString(b.Bytes())
}

// jsonString is s as a JSON string.
func jsonString(t testing.TB, s string) string {
	t.Helper()
	encoded, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// The image tokens follow the examples of Anthropic's vision guide on the high-resolution tier
// (platform.claude.com/docs/en/build-with-claude/vision): 1000 x 1000 is 1,296 tokens, 1920 x
// 1080 is 2,691, and 3840 x 2160, scaled down to 2576 x 1449, is 4,784.
func TestImageTokensFollowTheVisionGuide(t *testing.T) {
	settings := testTokenEstimate()
	for _, tc := range []struct {
		width, height int
		tokens        int64
	}{
		{1000, 1000, 1296},
		{1920, 1080, 2691},
		{3840, 2160, 4784},
		{2160, 3840, 4784},
		{200, 100, 32},
		{1, 1, 1},
	} {
		if got := imageTokens(tc.width, tc.height, settings); got != tc.tokens {
			t.Errorf("imageTokens(%d, %d) = %d, want %d", tc.width, tc.height, got, tc.tokens)
		}
	}
	// The cap on patches applies when the edge alone does not bring the image under it.
	capped := testTokenEstimate()
	capped.ImageMaxTokens = 1000
	if got := imageTokens(1000, 1000, capped); got != 1000 {
		t.Errorf("imageTokens under a cap of 1000 = %d, want 1000", got)
	}
}

// Each part of a Messages request counts as the method says: the text the model reads by its
// bytes, every tool definition and tool input as its JSON, a block of an unknown type as its
// JSON, each base64 image by its size in every format the upstream takes, each base64 PDF by its
// pages, and the overhead once.
func TestEstimateMessagesTokensCountsEachPart(t *testing.T) {
	settings := testTokenEstimate()
	const (
		system    = "You are a careful assistant."
		question  = "What is in these files? 请逐个说明。"
		thinking  = "The user sent two images and a report."
		toolName  = "read_file"
		toolInput = `{"path":"/srv/report.pdf","lines":[1,40]}`
		result    = "Report: revenue rose 12% in the third quarter."
		docTitle  = "Q3 report"
		docText   = "Plain text document body."
		docCtx    = "Shared by finance."
		unknown   = `{"type":"container_upload","file_id":"file_123"}`
		tool      = `{"name":"read_file","description":"Read a file from the workspace.","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}`
	)
	png1000 := encodedImage(t, 1000, 1000, pngImage)
	jpeg1920 := encodedImage(t, 1920, 1080, jpegImage)
	gif200 := encodedImage(t, 200, 100, gifImage)
	body := fmt.Sprintf(`{"model":"tk/claude","system":[{"type":"text","text":%s}],"tools":[%s],"messages":[
		{"role":"user","content":[{"type":"text","text":%s},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":%q}},
			{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":%q}},
			{"type":"document","title":%s,"context":%s,"source":{"type":"base64","media_type":"application/pdf","data":%q}},
			{"type":"document","source":{"type":"text","media_type":"text/plain","data":%s}},
			%s]},
		{"role":"assistant","content":[{"type":"thinking","thinking":%s,"signature":"sig"},{"type":"tool_use","id":"toolu_1","name":%s,"input":%s}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":%s},
			{"type":"image","source":{"type":"base64","media_type":"image/gif","data":%q}}]}]}]}`,
		jsonString(t, system), tool, jsonString(t, question), png1000, jpeg1920, jsonString(t, docTitle), jsonString(t, docCtx),
		pdfDocument(3), jsonString(t, docText), unknown, jsonString(t, thinking), jsonString(t, toolName), toolInput,
		jsonString(t, result), gif200)

	got, fault := estimateMessagesTokens([]byte(body), settings)
	if fault != nil {
		t.Fatalf("estimate: %+v", fault)
	}
	textBytes := len(system) + len(tool) + len(question) + len(docTitle) + len(docCtx) + len(docText) + len(unknown) +
		len(thinking) + len(toolName) + len(toolInput) + len(result)
	media := int64(1296+2691+32) + 3*settings.GetDocumentPageTokens()
	want := int64(math.Ceil(float64(textBytes)/3)) + media + 512
	if got != want {
		t.Fatalf("estimate %d, want %d: %d text bytes, %d media tokens, 512 overhead", got, want, textBytes, media)
	}

	// A system prompt and message content as strings count the same as text blocks.
	plain := `{"model":"m","system":"abc","messages":[{"role":"user","content":"defghi"},{"role":"assistant","content":"jkl"}]}`
	if got, fault := estimateMessagesTokens([]byte(plain), settings); fault != nil || got != 4+512 {
		t.Fatalf("string contents: estimate %d, %+v; want %d", got, fault, 4+512)
	}
}

// The local count refuses what the upstream's own count refuses, with 400 invalid_request_error:
// an image or a document given by URL or file id, and one that cannot be read.
func TestEstimateRefusesWhatTheUpstreamCountRefuses(t *testing.T) {
	for _, tc := range []struct{ name, block string }{
		{"an image by URL", `{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}`},
		{"an image by file id", `{"type":"image","source":{"type":"file","file_id":"file_1"}}`},
		{"a document by URL", `{"type":"document","source":{"type":"url","url":"https://example.com/a.pdf"}}`},
		{"an image that is not one", `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"bm90IGFuIGltYWdl"}}`},
		{"a document that is not a PDF", `{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"bm90IGEgcGRm"}}`},
		{"an image inside a tool result", `{"type":"tool_result","tool_use_id":"t","content":[{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","messages":[{"role":"user","content":[` + tc.block + `]}]}`
			_, fault := estimateMessagesTokens([]byte(body), testTokenEstimate())
			if fault == nil || fault.StatusCode == nil || *fault.StatusCode != http.StatusBadRequest ||
				fault.Error == nil || fault.Error.Type == nil || *fault.Error.Type != "invalid_request_error" {
				t.Fatalf("estimate = %+v, want 400 invalid_request_error", fault)
			}
		})
	}
}

// A plan without the estimate's settings, or with one that is not positive, fails with 500; the
// counts of the Gemini, Bedrock and Cohere formats answer 501 until batch 3, under every prefix
// that registers them; a count of any other route, the Responses input_tokens among them, is no
// count operation of a tokun endpoint type and fails with 404.
func TestEstimateTokensChecksItsSettings(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	zero := testTokenEstimate()
	zero.ImagePatchPixels = 0
	for _, tc := range []struct {
		name     string
		settings *routingv1.TokenEstimate
		route    string
		status   int
		code     string
	}{
		{"no settings", nil, "/anthropic/v1/messages/count_tokens", http.StatusInternalServerError, "tokun_token_estimate_invalid"},
		{"a setting of 0", zero, "/anthropic/v1/messages/count_tokens", http.StatusInternalServerError, "tokun_token_estimate_invalid"},
		{"the Gemini count", testTokenEstimate(), "/genai/v1beta/models/{model:*}", http.StatusNotImplemented, "tokun_token_estimate_not_implemented"},
		{"the Gemini count under /cursor", testTokenEstimate(), "/cursor/v1beta/models/{model:*}", http.StatusNotImplemented, "tokun_token_estimate_not_implemented"},
		{"the Bedrock count", testTokenEstimate(), "/bedrock/model/{modelId}/count-tokens", http.StatusNotImplemented, "tokun_token_estimate_not_implemented"},
		{"the Bedrock count without a prefix", testTokenEstimate(), "/model/{modelId}/count-tokens", http.StatusNotImplemented, "tokun_token_estimate_not_implemented"},
		{"the Cohere count", testTokenEstimate(), "/cohere/v1/tokenize", http.StatusNotImplemented, "tokun_token_estimate_not_implemented"},
		{"the Responses count", testTokenEstimate(), "/openai/v1/responses/input_tokens", http.StatusNotFound, "tokun_route_not_offered"},
		{"the Responses count without a prefix", testTokenEstimate(), "/v1/responses/input_tokens", http.StatusNotFound, "tokun_route_not_offered"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := &requestState{plan: &routingv1.RoutePlan{TokenEstimate: tc.settings}, route: tc.route, body: body}
			count, fault := estimateTokens(state)
			if count != nil || fault == nil || *fault.StatusCode != tc.status || *fault.Type != tc.code {
				t.Fatalf("estimateTokens = %+v, %+v; want %d %s", count, fault, tc.status, tc.code)
			}
		})
	}
	state := &requestState{plan: &routingv1.RoutePlan{TokenEstimate: testTokenEstimate()}, route: "/cursor/v1/messages/count_tokens", body: body}
	if count, fault := estimateTokens(state); fault != nil || count.InputTokens != 1+512 {
		t.Fatalf("a Messages count under another prefix: %+v, %+v", count, fault)
	}
}

// estimateCorpus is text of the kinds a count_tokens request carries.
var estimateCorpus = []struct{ name, text string }{
	{"English prose", "The gateway keeps one copy of every request body. When the body fits in the pod's memory headroom it stays in memory; otherwise it is written to the pod's emptyDir volume and read back through mmap, with no hand-set threshold between the two. Every rewrite splices the original bytes with small replacement pieces instead of encoding the whole document again, so a request of a gigabyte costs the same number of copies as a request of a kilobyte. Operators can see the size of each request in the trace, and the planner reads the features it needs from the single scan that validated the JSON, recorded the offsets of the top-level fields and measured the inline images and documents."},
	{"Chinese prose", "路由插件按选择曲线把规划器的分数、不确定度、阈值和每把密钥的份额换算成这次请求的主选和后备顺序，交给 Bifrost 自己的后备机制执行。网关负责准备候选、调用规划器，把请求速率、余量和每个接口的请求改写补进路由计划，并在响应回来以后按插件报告的每次尝试记账。开始输出以后不再切换，流式请求也一样。"},
	{"Go source", "func (p *Plugin) route(ctx *schemas.BifrostContext, req *schemas.BifrostRequest, state *requestState) {\n\tstate.order = drawOrder(state.plan, p.uniform)\n\tmaxAttempts := int(state.plan.GetMaxAttempts())\n\ttargets := make([]schemas.Fallback, 0, len(state.order))\n\tfor i, e := range state.order {\n\t\tif maxAttempts > 0 && len(targets) == maxAttempts {\n\t\t\tbreak\n\t\t}\n\t\ttarget, skipReason := p.bifrostTarget(e)\n\t\tif skipReason != \"\" {\n\t\t\tstate.skipped = append(state.skipped, skippedEntry{index: i, reason: skipReason})\n\t\t\tcontinue\n\t\t}\n\t\ttargets = append(targets, target)\n\t}\n}\n"},
	{"a tool definition", `{"name":"edit_file","description":"Replace an exact string in a file with another string. The old string must appear exactly once.","input_schema":{"type":"object","properties":{"path":{"type":"string","description":"Absolute path of the file"},"old_string":{"type":"string"},"new_string":{"type":"string"},"replace_all":{"type":"boolean","default":false}},"required":["path","old_string","new_string"]}}`},
	{"a shell session", "$ go test ./plugins/tokun/...\nok  \tgithub.com/capsohq/bifrost/plugins/tokun\t0.171s\n$ git status --short\n M plugins/tokun/route.go\n?? plugins/tokun/estimate.go\n$ ls -la /srv/data\ntotal 48\ndrwxr-x--- 2 app app  4096 Nov  4 08:31 .\n-rw-r----- 1 app app 37316 Nov  4 08:31 events.jsonl\n"},
}

// The text count against a real tokenizer, OpenAI's o200k_base (tiktoken), at the initial 3 bytes
// per token and without the overhead. Measured on estimateCorpus, the count is this many times the
// tokenizer's: English prose 1.65, Chinese prose 1.23, Go source 1.20, a tool definition 1.50, a
// shell session 0.83 (digits and runs of spaces are dense in tokens), and 1.28 on the five
// together. The test holds each kind within [0.75, 1.75] and the five together within [1, 1.5].
// Anthropic writes that Claude 4.7 and later count about 30 percent more tokens than earlier Claude
// models for the same text, so against those models the count is lower relative to theirs. The
// whole request adds input_overhead_tokens (512) once, which is most of the difference on a
// request of a few words.
func TestEstimateTextAgainstARealTokenizer(t *testing.T) {
	tiktoken.SetBpeLoader(tiktokenloader.NewOfflineLoader())
	encoding, err := tiktoken.GetEncoding(tiktoken.MODEL_O200K_BASE)
	if err != nil {
		t.Fatalf("load o200k_base: %v", err)
	}
	settings := testTokenEstimate()
	settings.InputOverheadTokens = 0
	count := func(text string) (estimate int64, real int) {
		t.Helper()
		body := `{"model":"m","messages":[{"role":"user","content":` + jsonString(t, text) + `}]}`
		estimate, fault := estimateMessagesTokens([]byte(body), settings)
		if fault != nil {
			t.Fatalf("estimate: %+v", fault)
		}
		return estimate, len(encoding.Encode(text, nil, nil))
	}
	var all strings.Builder
	for _, sample := range estimateCorpus {
		all.WriteString(sample.text)
		estimate, real := count(sample.text)
		ratio := float64(estimate) / float64(real)
		t.Logf("%s: %d bytes, o200k_base %d tokens, estimate %d, ratio %.2f", sample.name, len(sample.text), real, estimate, ratio)
		if ratio < 0.75 || ratio > 1.75 {
			t.Errorf("%s: the estimate is %.2f times the tokenizer's count, outside [0.75, 1.75]", sample.name, ratio)
		}
	}
	estimate, real := count(all.String())
	ratio := float64(estimate) / float64(real)
	t.Logf("the five together: %d bytes, o200k_base %d tokens, estimate %d, ratio %.2f", all.Len(), real, estimate, ratio)
	if ratio < 1 || ratio > 1.5 {
		t.Errorf("the five together: the estimate is %.2f times the tokenizer's count, outside [1, 1.5]", ratio)
	}
}

// BenchmarkEstimateTokens measures the local count of a coding agent's count_tokens request: a
// system prompt of 8 KiB, ten tool definitions and 40 messages of text, tool calls and results.
func BenchmarkEstimateTokens(b *testing.B) {
	var messages []string
	for i := range 20 {
		messages = append(messages,
			fmt.Sprintf(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_%d","content":%s}]}`, i, jsonString(b, estimateCorpus[i%len(estimateCorpus)].text)),
			fmt.Sprintf(`{"role":"assistant","content":[{"type":"text","text":%s},{"type":"tool_use","id":"toolu_%d","name":"edit_file","input":{"path":"/srv/a.go","old_string":"x","new_string":"y"}}]}`, jsonString(b, estimateCorpus[0].text), i+1))
	}
	tools := make([]string, 10)
	for i := range tools {
		tools[i] = estimateCorpus[3].text
	}
	body := []byte(fmt.Sprintf(`{"model":"tk/claude","system":%s,"tools":[%s],"messages":[%s]}`,
		jsonString(b, strings.Repeat("You are a coding agent. ", 340)), strings.Join(tools, ","), strings.Join(messages, ",")))
	state := &requestState{plan: &routingv1.RoutePlan{TokenEstimate: testTokenEstimate()}, route: "/anthropic/v1/messages/count_tokens", body: body}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if _, fault := estimateTokens(state); fault != nil {
			b.Fatal(fault)
		}
	}
}
