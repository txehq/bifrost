package tokun

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/gif"  // registers the GIF decoder for image.DecodeConfig
	_ "image/jpeg" // registers the JPEG decoder
	_ "image/png"  // registers the PNG decoder
	"math"
	"net/http"
	"strings"
	"unsafe"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/tidwall/gjson"
	_ "golang.org/x/image/webp" // registers the WebP decoder

	"github.com/capsohq/bifrost/core/schemas"

	routingv1 "github.com/capsohq/txe/tokun/proto/gen/go/tokun/routing/v1"
)

func init() {
	// pdfcpu's default configuration reads and writes a configuration directory under the
	// user's home; counting a PDF's pages needs none of it.
	api.DisableConfigDir()
}

// messagesCountTokensRoute is how the route template of every Anthropic Messages token count
// ends, under each prefix that registers one (/anthropic, /cursor, /langchain, /litellm,
// /pydanticai).
const messagesCountTokensRoute = "/v1/messages/count_tokens"

// nativeCountRoutes are how the route templates of the token counts of the vendor native formats
// end, under each prefix that registers one: Gemini's countTokens (the only count_tokens request of
// its models route, under none, /genai and /cursor), Bedrock's count-tokens (under none, /bedrock
// and /cursor) and Cohere's tokenize (under none, /cohere and /cursor). They are the count
// operations of tokun's endpoint types of those formats (Tokun 多模态和端点接入计划, text.chat.gemini,
// text.chat.bedrock, text.chat.cohere).
var nativeCountRoutes = []struct{ suffix, format string }{
	{"/v1beta/models/{model:*}", "Gemini"},
	{"/model/{modelId}/count-tokens", "Bedrock"},
	{"/v1/tokenize", "Cohere"},
}

// estimateTokens answers a count_tokens request with the local count of RoutePlan.token_estimate
// (Hot Path · tokun 现有的格式转换和去处, CountTokens): the plan has no upstream with a
// count_tokens operation. It reads the caller's body in the request's own format, without
// converting it, so the count covers every part of the request the format has. A request the
// upstream's own count would refuse (an image or a document given by URL or file id, one that
// cannot be read) is refused with 400; a plan without its settings fails with 500.
//
// The Anthropic Messages count is counted here; the counts of the vendor native formats are
// batch 3's (estimateNativeFormatTokens). A count of any other route is no count operation of a
// tokun endpoint type, the Responses input_tokens among them, so no plan should route one: it
// fails with 404 tokun_route_not_offered.
func estimateTokens(state *requestState) (*schemas.BifrostCountTokensResponse, *schemas.BifrostError) {
	settings := state.plan.GetTokenEstimate()
	if fault := checkTokenEstimate(settings); fault != nil {
		return nil, fault
	}
	if len(state.body) == 0 {
		return nil, sendError(http.StatusInternalServerError, "tokun_token_estimate_failed",
			"the route kept no request body to count tokens from")
	}
	var tokens int64
	var fault *schemas.BifrostError
	if strings.HasSuffix(state.route, messagesCountTokensRoute) {
		tokens, fault = estimateMessagesTokens(state.body, settings)
	} else if format := nativeCountFormat(state.route); format != "" {
		tokens, fault = estimateNativeFormatTokens(format, state.body, settings)
	} else {
		fault = sendError(http.StatusNotFound, "tokun_route_not_offered",
			fmt.Sprintf("%s is not a token count of an endpoint type tokun offers", state.route))
	}
	if fault != nil {
		return nil, fault
	}
	return &schemas.BifrostCountTokensResponse{
		Model:       gjson.GetBytes(state.body, "model").String(),
		InputTokens: int(tokens),
	}, nil
}

// nativeCountFormat returns the vendor native format whose token count route is route, or "" when
// it is none of nativeCountRoutes.
func nativeCountFormat(route string) string {
	for _, r := range nativeCountRoutes {
		if strings.HasSuffix(route, r.suffix) {
			return r.format
		}
	}
	return ""
}

// TODO(tokun-B3): 按 Gemini、Bedrock 和 Cohere 格式的正文在本地估算 token 数
// 阶段：B1 放入；B3-11「Gemini 原生格式」、B3-12「Bedrock 原生格式」、B3-13「Cohere 原生格式」完成时删除这两行和占位
func estimateNativeFormatTokens(format string, _ []byte, _ *routingv1.TokenEstimate) (int64, *schemas.BifrostError) {
	return 0, sendError(http.StatusNotImplemented, "tokun_token_estimate_not_implemented",
		fmt.Sprintf("the local token count of the %s format is not implemented", format))
}

// checkTokenEstimate fails a plan whose token_estimate is missing, or would divide by zero or
// count nothing for an image or a page.
func checkTokenEstimate(s *routingv1.TokenEstimate) *schemas.BifrostError {
	var problem string
	switch {
	case s == nil:
		problem = "the route plan of a count_tokens request without a count_tokens upstream carries no token_estimate"
	case s.GetInputBytesPerToken() <= 0 || s.GetInputOverheadTokens() < 0 || s.GetImagePatchPixels() <= 0 ||
		s.GetImageMaxEdgePixels() <= 0 || s.GetImageMaxTokens() <= 0 || s.GetDocumentPageTokens() <= 0:
		problem = fmt.Sprintf("the route plan's token_estimate %v has a setting that is not positive", s)
	default:
		return nil
	}
	return sendError(http.StatusInternalServerError, "tokun_token_estimate_invalid", problem)
}

// tokenTally adds up a request's input: the UTF-8 bytes of its text, and the tokens of its
// images and documents.
type tokenTally struct {
	settings    *routingv1.TokenEstimate
	textBytes   int64
	mediaTokens int64
}

// estimateMessagesTokens counts an Anthropic Messages request (the body of /v1/messages and its
// count_tokens) in one pass over its top-level fields:
//   - text, by its UTF-8 bytes at input_bytes_per_token: system, the text of every message block,
//     thinking and redacted thinking, the name and the input of a tool call, the content of a
//     tool result, a text document, a search result's source, title and text, and every tool
//     definition as its whole JSON; a block of another type counts as its whole JSON;
//   - a base64 image by its size (imageTokens);
//   - a base64 PDF by its pages, document_page_tokens each;
//   - input_overhead_tokens once, for what the upstream adds: role markers and the tool-use
//     system prompt.
//
// Like the upstream's count, it refuses an image or a document given by URL or file id.
func estimateMessagesTokens(body []byte, settings *routingv1.TokenEstimate) (int64, *schemas.BifrostError) {
	t := tokenTally{settings: settings}
	var fault *schemas.BifrostError
	// The body is read in place, not copied: nothing read from it outlives this call, and the
	// buffer is not written while the request is handled.
	gjson.Parse(unsafe.String(unsafe.SliceData(body), len(body))).ForEach(func(key, value gjson.Result) bool {
		switch key.Str {
		case "system":
			fault = t.content(value)
		case "messages":
			value.ForEach(func(_, message gjson.Result) bool {
				fault = t.content(message.Get("content"))
				return fault == nil
			})
		case "tools":
			value.ForEach(func(_, tool gjson.Result) bool {
				t.textBytes += int64(len(tool.Raw))
				return true
			})
		}
		return fault == nil
	})
	if fault != nil {
		return 0, fault
	}
	text := int64(math.Ceil(float64(t.textBytes) / settings.GetInputBytesPerToken()))
	return text + t.mediaTokens + settings.GetInputOverheadTokens(), nil
}

// text adds the bytes of a string.
func (t *tokenTally) text(value gjson.Result) {
	if value.Type == gjson.String {
		t.textBytes += int64(len(value.Str))
	}
}

// content adds a content: a string, or an array of blocks. Any other value counts as its JSON.
func (t *tokenTally) content(value gjson.Result) *schemas.BifrostError {
	switch {
	case value.Type == gjson.String:
		t.textBytes += int64(len(value.Str))
	case value.IsArray():
		var fault *schemas.BifrostError
		value.ForEach(func(_, block gjson.Result) bool {
			fault = t.block(block)
			return fault == nil
		})
		return fault
	default:
		t.textBytes += int64(len(value.Raw))
	}
	return nil
}

// block adds one content block by its type.
func (t *tokenTally) block(block gjson.Result) *schemas.BifrostError {
	switch block.Get("type").Str {
	case "text":
		t.text(block.Get("text"))
	case "thinking":
		t.text(block.Get("thinking"))
	case "redacted_thinking":
		t.text(block.Get("data"))
	case "tool_use", "server_tool_use", "mcp_tool_use":
		t.text(block.Get("name"))
		t.textBytes += int64(len(block.Get("input").Raw))
	case "tool_result", "mcp_tool_result":
		return t.content(block.Get("content"))
	case "image":
		return t.image(block.Get("source"))
	case "document":
		return t.document(block)
	case "search_result":
		t.text(block.Get("source"))
		t.text(block.Get("title"))
		return t.content(block.Get("content"))
	case "web_fetch_tool_result":
		// The fetched page is a document block inside the result.
		if document := block.Get("content.content"); document.Get("type").Str == "document" {
			return t.document(document)
		}
		t.textBytes += int64(len(block.Raw))
	default:
		t.textBytes += int64(len(block.Raw))
	}
	return nil
}

// image adds an image block's source: a base64 image by its size.
func (t *tokenTally) image(source gjson.Result) *schemas.BifrostError {
	if kind := source.Get("type").Str; kind != "base64" {
		return refuseCount(fmt.Sprintf("an image given as %q cannot be counted; count_tokens takes images as base64", kind))
	}
	width, height, err := imageSize(source.Get("data").Str)
	if err != nil {
		return refuseCount(fmt.Sprintf("an image cannot be read: %v", err))
	}
	t.mediaTokens += imageTokens(width, height, t.settings)
	return nil
}

// document adds a document block: a base64 PDF by its pages, a text document as text, a
// content document by its blocks, and its title and context as text.
func (t *tokenTally) document(block gjson.Result) *schemas.BifrostError {
	t.text(block.Get("title"))
	t.text(block.Get("context"))
	source := block.Get("source")
	switch kind := source.Get("type").Str; kind {
	case "base64":
		pages, err := pdfPages(source.Get("data").Str)
		if err != nil {
			return refuseCount(fmt.Sprintf("a document cannot be read as a PDF: %v", err))
		}
		t.mediaTokens += int64(pages) * t.settings.GetDocumentPageTokens()
	case "text":
		t.text(source.Get("data"))
	case "content":
		return t.content(source.Get("content"))
	default:
		return refuseCount(fmt.Sprintf("a document given as %q cannot be counted; count_tokens takes documents as base64, text or content", kind))
	}
	return nil
}

// imageSize reads the width and height from the header of a base64 JPEG, PNG, GIF or WebP
// image, without decoding its pixels.
func imageSize(data string) (int, int, error) {
	config, _, err := image.DecodeConfig(base64.NewDecoder(base64.StdEncoding, strings.NewReader(data)))
	if err != nil {
		return 0, 0, err
	}
	return config.Width, config.Height, nil
}

// imageTokens is the tokens of an image of width x height pixels: scaled down, keeping its
// aspect ratio, until its longer edge is at most image_max_edge_pixels, then one token for each
// square patch of image_patch_pixels it covers, and at most image_max_tokens.
func imageTokens(width, height int, s *routingv1.TokenEstimate) int64 {
	w, h := float64(width), float64(height)
	if edge, longer := float64(s.GetImageMaxEdgePixels()), max(w, h); longer > edge {
		w, h = w*edge/longer, h*edge/longer
	}
	patch := float64(s.GetImagePatchPixels())
	return min(int64(math.Ceil(w/patch))*int64(math.Ceil(h/patch)), s.GetImageMaxTokens())
}

// pdfPages counts the pages of a base64 PDF. The PDF is decoded into memory for the time of
// the count, and read without validating its objects: the page count is the page tree's.
func pdfPages(data string) (int, error) {
	pdf, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return 0, err
	}
	conf := model.NewDefaultConfiguration()
	conf.ValidationMode = model.ValidationRelaxed
	ctx, err := api.ReadContext(bytes.NewReader(pdf), conf)
	if err != nil {
		return 0, err
	}
	if err := ctx.EnsurePageCount(); err != nil {
		return 0, err
	}
	return ctx.PageCount, nil
}

// refuseCount refuses a count_tokens request the local count cannot count, as the upstream's
// own count would: 400, invalid_request_error.
func refuseCount(message string) *schemas.BifrostError {
	return sendError(http.StatusBadRequest, "invalid_request_error", message)
}
