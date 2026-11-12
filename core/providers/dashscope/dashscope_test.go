package dashscope

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

func newTestProvider(t *testing.T, network schemas.NetworkConfig) *DashScopeProvider {
	t.Helper()
	provider, err := NewDashScopeProvider(&schemas.ProviderConfig{NetworkConfig: network}, nil)
	if err != nil {
		t.Fatalf("NewDashScopeProvider: %v", err)
	}
	return provider
}

// TestDashScopeBases pins which base each operation family uses: the Singapore site by
// default, the configured regional base when set, and the workspace base for video only.
func TestDashScopeBases(t *testing.T) {
	const workspace = "https://ws-1.ap-southeast-1.maas.aliyuncs.com"
	cases := []struct {
		name          string
		network       schemas.NetworkConfig
		wantRegional  string
		wantWorkspace string
	}{
		{name: "defaults to the Singapore site", wantRegional: DefaultBaseURL, wantWorkspace: DefaultBaseURL},
		{name: "configured regional base, no workspace base", network: schemas.NetworkConfig{BaseURL: "https://dashscope.example.com/"}, wantRegional: "https://dashscope.example.com", wantWorkspace: "https://dashscope.example.com"},
		{name: "workspace base for video", network: schemas.NetworkConfig{WorkspaceBaseURL: workspace + "/"}, wantRegional: DefaultBaseURL, wantWorkspace: workspace},
	}
	regional := []schemas.RequestType{schemas.ImageGenerationRequest, schemas.ImageEditRequest, schemas.SpeechRequest, schemas.SpeechStreamRequest, schemas.TranscriptionRequest}
	video := []schemas.RequestType{schemas.VideoGenerationRequest, schemas.VideoRetrieveRequest, schemas.VideoCancelRequest, schemas.VideoDownloadRequest, schemas.VideoListRequest, schemas.VideoDeleteRequest}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := newTestProvider(t, tc.network)
			for _, rt := range regional {
				if got := provider.baseURLFor(rt); got != tc.wantRegional {
					t.Errorf("%s base = %q, want %q", rt, got, tc.wantRegional)
				}
			}
			for _, rt := range video {
				if got := provider.baseURLFor(rt); got != tc.wantWorkspace {
					t.Errorf("%s base = %q, want %q", rt, got, tc.wantWorkspace)
				}
			}
		})
	}
}

// TestDashScopeRequestHeaders pins DashScope's auth header, and that an extra header named
// Authorization cannot replace the key.
func TestDashScopeRequestHeaders(t *testing.T) {
	provider := newTestProvider(t, schemas.NetworkConfig{ExtraHeaders: map[string]schemas.SecretVar{
		"Authorization": *schemas.NewSecretVar("Bearer extra"),
		"X-Trace":       *schemas.NewSecretVar("t-1"),
	}})
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)

	provider.setRequestHeaders(schemas.NewBifrostContext(t.Context(), schemas.NoDeadline), req, schemas.Key{Value: *schemas.NewSecretVar("sk-dash")})

	if got := string(req.Header.Peek("Authorization")); got != "Bearer sk-dash" {
		t.Errorf("Authorization = %q, want the key as a bearer token", got)
	}
	if got := string(req.Header.Peek("X-Trace")); got != "t-1" {
		t.Errorf("X-Trace = %q, want the extra header", got)
	}
}

// dashScopeImplementedOperations are the Provider methods the provider implements; the video
// tests are in videos_test.go and the image tests below.
var dashScopeImplementedOperations = map[string]bool{
	"ImageGeneration": true,
	"ImageEdit":       true,
	"VideoGeneration": true,
	"VideoRetrieve":   true,
	"VideoDownload":   true,
	"VideoList":       true,
}

// TestDashScopeOperationsAreUnsupported pins that every operation the provider does not
// implement answers the unsupported-operation error, naming its own request type, without
// calling upstream.
func TestDashScopeOperationsAreUnsupported(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	provider := newTestProvider(t, schemas.NetworkConfig{BaseURL: server.URL, WorkspaceBaseURL: server.URL, AllowPrivateNetwork: true})
	var _ schemas.Provider = provider

	iface := reflect.TypeOf((*schemas.Provider)(nil)).Elem()
	value := reflect.ValueOf(provider)
	for i := 0; i < iface.NumMethod(); i++ {
		method := iface.Method(i)
		if method.Name == "GetProviderKey" || dashScopeImplementedOperations[method.Name] {
			continue
		}
		fn := value.MethodByName(method.Name)
		args := make([]reflect.Value, fn.Type().NumIn())
		for j := range args {
			args[j] = reflect.Zero(fn.Type().In(j))
		}
		out := fn.Call(args)
		bifrostErr, _ := out[len(out)-1].Interface().(*schemas.BifrostError)
		if bifrostErr == nil || bifrostErr.Error == nil || bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != "unsupported_operation" {
			t.Errorf("%s: got %+v, want the unsupported-operation error", method.Name, bifrostErr)
			continue
		}
		if bifrostErr.ExtraFields.RequestType == "" || bifrostErr.ExtraFields.Provider != schemas.DashScope {
			t.Errorf("%s: extra fields %+v, want provider dashscope and the request type", method.Name, bifrostErr.ExtraFields)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("unsupported operations made %d upstream calls", n)
	}
}

// TestDashScopeErrorParsing pins that DashScope's documented error bodies keep their status
// and code and land in the right error_type family.
func TestDashScopeErrorParsing(t *testing.T) {
	cases := []struct {
		status      int
		code        string
		requestType schemas.RequestType
		want        schemas.ErrorType
	}{
		{400, "InvalidParameter", schemas.ImageGenerationRequest, schemas.ErrorTypeCallerInvalidRequest},
		{400, "Arrearage", schemas.ImageGenerationRequest, schemas.ErrorTypeProviderBilling},
		{403, "AllocationQuota.FreeTierOnly", schemas.SpeechRequest, schemas.ErrorTypeProviderBilling},
		{401, "InvalidApiKey", schemas.VideoGenerationRequest, schemas.ErrorTypeProviderAuthFailed},
		{403, "AccessDenied.Unpurchased", schemas.VideoGenerationRequest, schemas.ErrorTypeProviderAuthFailed},
		{403, "Model.AccessDenied", schemas.TranscriptionRequest, schemas.ErrorTypeProviderAuthFailed},
		{404, "ModelNotFound", schemas.VideoGenerationRequest, schemas.ErrorTypeCallerModelUnknown},
		{429, "Throttling.RateQuota", schemas.ImageGenerationRequest, schemas.ErrorTypeProviderRateLimited},
		{429, "Throttling.AllocationQuota", schemas.VideoGenerationRequest, schemas.ErrorTypeProviderRateLimited},
		{500, "InternalError.Algo", schemas.SpeechRequest, schemas.ErrorTypeProviderServerError},
		{503, "ServiceUnavailable", schemas.SpeechRequest, schemas.ErrorTypeProviderOverloaded},
		{400, "DataInspectionFailed", schemas.ImageGenerationRequest, schemas.ErrorTypeCallerContentFiltered},
		{400, "data_inspection_failed", schemas.SpeechRequest, schemas.ErrorTypeCallerContentFiltered},
		{400, "IPInfringementSuspect", schemas.ImageGenerationRequest, schemas.ErrorTypeCallerContentFiltered},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			resp := fasthttp.AcquireResponse()
			defer fasthttp.ReleaseResponse(resp)
			resp.SetStatusCode(tc.status)
			resp.Header.SetContentType("application/json")
			resp.SetBodyString(`{"request_id":"req-1","code":"` + tc.code + `","message":"vendor says ` + tc.code + `"}`)

			bifrostErr := parseDashScopeError(resp)

			if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != tc.status {
				t.Errorf("status = %v, want %d", bifrostErr.StatusCode, tc.status)
			}
			if bifrostErr.Error == nil || bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != tc.code {
				t.Errorf("error.code = %+v, want %q verbatim", bifrostErr.Error, tc.code)
			}
			if bifrostErr.Error.Message != "vendor says "+tc.code {
				t.Errorf("error.message = %q, want the vendor's message", bifrostErr.Error.Message)
			}
			if got := schemas.ClassifyErrorType(bifrostErr, tc.requestType); got != tc.want {
				t.Errorf("error_type = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("body without a DashScope shape", func(t *testing.T) {
		resp := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseResponse(resp)
		resp.SetStatusCode(502)
		resp.SetBodyString("<html>bad gateway</html>")

		bifrostErr := parseDashScopeError(resp)

		if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 502 || bifrostErr.Error == nil || bifrostErr.Error.Message == "" {
			t.Fatalf("got %+v, want a 502 with a message", bifrostErr)
		}
		if got := schemas.ClassifyErrorType(bifrostErr, schemas.VideoGenerationRequest); got != schemas.ErrorTypeProviderServerError {
			t.Errorf("error_type = %q, want provider_server_error", got)
		}
	})
}

// Response bodies follow the samples of Alibaba's Qwen-Image, Qwen-Image edit and Z-Image API
// references (alibabacloud.com/help/en/model-studio/qwen-image-api, qwen-image-edit-api,
// z-image-api-reference, read 2026-11-11), with the signed query strings trimmed.
const (
	qwenImageSuccessBody = `{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":[` +
		`{"image":"https://dashscope-result-sh.oss-cn-shanghai.aliyuncs.com/xxx/1.png?Expires=xxx"},` +
		`{"image":"https://dashscope-result-sh.oss-cn-shanghai.aliyuncs.com/xxx/2.png?Expires=xxx"}]}}]},` +
		`"usage":{"width":1536,"height":1024,"image_count":2},"request_id":"bf37ca26-0abe-98e4-8065-xxxxxx"}`
	zImageSuccessBody = `{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":[` +
		`{"image":"https://dashscope-result-sh.oss-cn-shanghai.aliyuncs.com/xxx/z.png?Expires=xxx"},` +
		`{"text":"a cat on a sofa, warm afternoon light, 35mm"},{"reasoning_content":"the user wants a cat"}]}}]},` +
		`"usage":{"width":1024,"height":1536,"image_count":1,"input_tokens":120,"output_tokens":310,"total_tokens":430,` +
		`"output_tokens_details":{"reasoning_tokens":250}},"request_id":"req-z"}`
)

type capturedImageRequest struct {
	path string
	auth string
	body []byte
}

// newMockDashScope serves one fixed answer and records every request it receives.
func newMockDashScopeImages(t *testing.T, status int, body string) (*DashScopeProvider, func() []capturedImageRequest) {
	t.Helper()
	var mu sync.Mutex
	var captured []capturedImageRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ := io.ReadAll(r.Body)
		mu.Lock()
		captured = append(captured, capturedImageRequest{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: reqBody})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req-header-1")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	provider := newTestProvider(t, schemas.NetworkConfig{BaseURL: server.URL, AllowPrivateNetwork: true, DefaultRequestTimeoutInSeconds: 10})
	return provider, func() []capturedImageRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]capturedImageRequest(nil), captured...)
	}
}

func imageTestContext(t *testing.T) *schemas.BifrostContext {
	t.Helper()
	return schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
}

func imageTestKey() schemas.Key {
	return schemas.Key{Value: *schemas.NewSecretVar("sk-dash")}
}

// TestDashScopeQwenImageGeneration pins the documented multimodal-generation body for a
// Qwen-Image request, and that the answer's links, image count and size reach the caller.
func TestDashScopeQwenImageGeneration(t *testing.T) {
	provider, requests := newMockDashScopeImages(t, http.StatusOK, qwenImageSuccessBody)
	before := time.Now().Unix()

	resp, bifrostErr := provider.ImageGeneration(imageTestContext(t), imageTestKey(), &schemas.BifrostImageGenerationRequest{
		Provider: schemas.DashScope,
		Model:    "qwen-image-2.0-pro",
		Input:    &schemas.ImageGenerationInput{Prompt: "a cat on a sofa"},
		Params: &schemas.ImageGenerationParameters{
			N:              schemas.Ptr(2),
			Size:           schemas.Ptr("1536x1024"),
			Seed:           schemas.Ptr(42),
			NegativePrompt: schemas.Ptr("low quality"),
			ResponseFormat: schemas.Ptr("url"),
			ExtraParams:    map[string]interface{}{"prompt_extend": true, "watermark": false},
		},
	})
	if bifrostErr != nil {
		t.Fatalf("ImageGeneration: %+v", bifrostErr.Error)
	}

	got := requests()
	if len(got) != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1", len(got))
	}
	if got[0].path != "/api/v1/services/aigc/multimodal-generation/generation" {
		t.Errorf("path = %q, want the multimodal-generation endpoint", got[0].path)
	}
	if got[0].auth != "Bearer sk-dash" {
		t.Errorf("Authorization = %q, want the key as a bearer token", got[0].auth)
	}
	assertJSONEqual(t, string(got[0].body), `{"model":"qwen-image-2.0-pro",
		"input":{"messages":[{"role":"user","content":[{"text":"a cat on a sofa"}]}]},
		"parameters":{"n":2,"size":"1536*1024","negative_prompt":"low quality","seed":42,"prompt_extend":true,"watermark":false}}`)

	if len(resp.Data) != 2 || !strings.HasSuffix(resp.Data[0].URL, "/1.png?Expires=xxx") || !strings.HasSuffix(resp.Data[1].URL, "/2.png?Expires=xxx") {
		t.Fatalf("data = %+v, want DashScope's two links in order", resp.Data)
	}
	if resp.Data[0].Index != 0 || resp.Data[1].Index != 1 || resp.Data[0].B64JSON != "" {
		t.Errorf("data = %+v, want indexed link items", resp.Data)
	}
	if resp.Usage == nil || resp.Usage.OutputImages == nil || *resp.Usage.OutputImages != 2 {
		t.Fatalf("usage = %+v, want output_images 2 from image_count", resp.Usage)
	}
	if resp.Usage.InputTokens != 0 || resp.Usage.OutputTokens != 0 || resp.Usage.TotalTokens != 0 || resp.Usage.OutputTokensDetails != nil {
		t.Errorf("usage = %+v, want no token counts: DashScope reported none", resp.Usage)
	}
	if resp.ImageGenerationResponseParameters == nil || resp.Size != "1536x1024" {
		t.Errorf("size = %+v, want 1536x1024 from usage.width and usage.height", resp.ImageGenerationResponseParameters)
	}
	if resp.ID != "bf37ca26-0abe-98e4-8065-xxxxxx" || resp.Model != "qwen-image-2.0-pro" {
		t.Errorf("id = %q, model = %q, want the request id and the requested model", resp.ID, resp.Model)
	}
	if resp.Created < before || resp.Created > time.Now().Unix() {
		t.Errorf("created = %d, want the time the answer was read", resp.Created)
	}
}

// TestDashScopeQwenImageGenerationWithoutParams pins that a request with no parameters sends
// no parameters object, so DashScope's own defaults apply.
func TestDashScopeQwenImageGenerationWithoutParams(t *testing.T) {
	provider, requests := newMockDashScopeImages(t, http.StatusOK, qwenImageSuccessBody)

	if _, bifrostErr := provider.ImageGeneration(imageTestContext(t), imageTestKey(), &schemas.BifrostImageGenerationRequest{
		Model: "qwen-image-3.0-pro",
		Input: &schemas.ImageGenerationInput{Prompt: "a cat"},
	}); bifrostErr != nil {
		t.Fatalf("ImageGeneration: %+v", bifrostErr.Error)
	}
	assertJSONEqual(t, string(requests()[0].body), `{"model":"qwen-image-3.0-pro","input":{"messages":[{"role":"user","content":[{"text":"a cat"}]}]}}`)
}

// TestDashScopeZImageGeneration pins Z-Image's narrower parameter set (no n, no
// negative_prompt) and that its token counts, reasoning tokens and rewritten prompt are kept.
func TestDashScopeZImageGeneration(t *testing.T) {
	provider, requests := newMockDashScopeImages(t, http.StatusOK, zImageSuccessBody)

	resp, bifrostErr := provider.ImageGeneration(imageTestContext(t), imageTestKey(), &schemas.BifrostImageGenerationRequest{
		Model: "z-image-turbo",
		Input: &schemas.ImageGenerationInput{Prompt: "a cat"},
		Params: &schemas.ImageGenerationParameters{
			N:              schemas.Ptr(1),
			Size:           schemas.Ptr("1024*1536"),
			NegativePrompt: schemas.Ptr("blurry"),
			Seed:           schemas.Ptr(7),
			ExtraParams:    map[string]interface{}{"prompt_extend": true},
		},
	})
	if bifrostErr != nil {
		t.Fatalf("ImageGeneration: %+v", bifrostErr.Error)
	}
	assertJSONEqual(t, string(requests()[0].body), `{"model":"z-image-turbo",
		"input":{"messages":[{"role":"user","content":[{"text":"a cat"}]}]},
		"parameters":{"size":"1024*1536","seed":7,"prompt_extend":true}}`)

	if len(resp.Data) != 1 || resp.Data[0].RevisedPrompt != "a cat on a sofa, warm afternoon light, 35mm" {
		t.Fatalf("data = %+v, want one link with the rewritten prompt", resp.Data)
	}
	usage := resp.Usage
	if usage == nil || usage.OutputImages == nil || *usage.OutputImages != 1 || usage.InputTokens != 120 || usage.OutputTokens != 310 || usage.TotalTokens != 430 {
		t.Fatalf("usage = %+v, want DashScope's image count and token counts", usage)
	}
	if usage.OutputTokensDetails == nil || usage.OutputTokensDetails.ReasoningTokens == nil || *usage.OutputTokensDetails.ReasoningTokens != 250 {
		t.Errorf("output_tokens_details = %+v, want reasoning_tokens 250", usage.OutputTokensDetails)
	}
}

// TestDashScopeImageEdit pins the edit body: a link is sent unchanged, bytes become a base64
// data URI, the images keep their order and the instruction follows them.
func TestDashScopeImageEdit(t *testing.T) {
	provider, requests := newMockDashScopeImages(t, http.StatusOK, qwenImageSuccessBody)
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")

	resp, bifrostErr := provider.ImageEdit(imageTestContext(t), imageTestKey(), &schemas.BifrostImageEditRequest{
		Model: "qwen-image-2.0",
		Input: &schemas.ImageEditInput{
			Prompt: "make the sofa red",
			Images: []schemas.ImageInput{
				{URL: "https://example.com/in put.png?sig=a%2Fb"},
				{Image: png},
				{URL: "data:image/jpeg;base64,/9j/4AAQ"},
			},
		},
		Params: &schemas.ImageEditParameters{N: schemas.Ptr(2), Size: schemas.Ptr("1024x1536")},
	})
	if bifrostErr != nil {
		t.Fatalf("ImageEdit: %+v", bifrostErr.Error)
	}
	got := requests()
	if len(got) != 1 || got[0].path != "/api/v1/services/aigc/multimodal-generation/generation" {
		t.Fatalf("requests = %+v, want one call to the multimodal-generation endpoint", got)
	}
	assertJSONEqual(t, string(got[0].body), `{"model":"qwen-image-2.0",
		"input":{"messages":[{"role":"user","content":[
			{"image":"https://example.com/in put.png?sig=a%2Fb"},
			{"image":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="},
			{"image":"data:image/jpeg;base64,/9j/4AAQ"},
			{"text":"make the sofa red"}]}]},
		"parameters":{"n":2,"size":"1024*1536"}}`)
	if len(resp.Data) != 2 || resp.Usage == nil || resp.Usage.OutputImages == nil || *resp.Usage.OutputImages != 2 {
		t.Errorf("response = %+v, want two links and output_images 2", resp)
	}
}

// TestDashScopeImageRefusals pins every request the sync image routes refuse, that each refusal
// names the field and carries the contract's error_type, and that none reaches DashScope.
func TestDashScopeImageRefusals(t *testing.T) {
	generation := func(model string, params *schemas.ImageGenerationParameters) func(*DashScopeProvider, *schemas.BifrostContext) *schemas.BifrostError {
		return func(provider *DashScopeProvider, ctx *schemas.BifrostContext) *schemas.BifrostError {
			_, bifrostErr := provider.ImageGeneration(ctx, imageTestKey(), &schemas.BifrostImageGenerationRequest{
				Model: model, Input: &schemas.ImageGenerationInput{Prompt: "a cat"}, Params: params,
			})
			return bifrostErr
		}
	}
	edit := func(model string, params *schemas.ImageEditParameters) func(*DashScopeProvider, *schemas.BifrostContext) *schemas.BifrostError {
		return func(provider *DashScopeProvider, ctx *schemas.BifrostContext) *schemas.BifrostError {
			_, bifrostErr := provider.ImageEdit(ctx, imageTestKey(), &schemas.BifrostImageEditRequest{
				Model:  model,
				Input:  &schemas.ImageEditInput{Prompt: "red sofa", Images: []schemas.ImageInput{{URL: "https://example.com/in.png"}}},
				Params: params,
			})
			return bifrostErr
		}
	}
	extra := func(name string, value any) *schemas.ImageGenerationParameters {
		return &schemas.ImageGenerationParameters{ExtraParams: map[string]interface{}{name: value}}
	}
	cases := []struct {
		name      string
		call      func(*DashScopeProvider, *schemas.BifrostContext) *schemas.BifrostError
		wantType  schemas.ErrorType
		wantParam string
	}{
		{"callback_url", generation("qwen-image-2.0", extra("callback_url", "https://hooks.tokun.sh/v/t1")), schemas.ErrorTypeCallerUnsupportedParameter, "callback_url"},
		{"client_reference_id", generation("qwen-image-2.0", extra("client_reference_id", "t1")), schemas.ErrorTypeCallerUnsupportedParameter, "client_reference_id"},
		{"expires_after_seconds", generation("z-image-turbo", extra("expires_after_seconds", 3600)), schemas.ErrorTypeCallerUnsupportedParameter, "expires_after_seconds"},
		{"output_storage_uri", generation("qwen-image-2.0", extra("output_storage_uri", "oss://b/t1")), schemas.ErrorTypeCallerUnsupportedParameter, "output_storage_uri"},
		{"task control field in another case", generation("qwen-image-2.0", extra("Callback_URL", "https://hooks.tokun.sh/v/t1")), schemas.ErrorTypeCallerUnsupportedParameter, "callback_url"},
		{"task control field inside an extra parameters object", generation("qwen-image-2.0", extra("parameters", map[string]interface{}{"callback_url": "https://hooks.tokun.sh/v/t1"})), schemas.ErrorTypeCallerUnsupportedParameter, "callback_url"},
		{"task control field in camel case inside an extra parameters object", edit("qwen-image-2.0", &schemas.ImageEditParameters{ExtraParams: map[string]interface{}{"Parameters": map[string]interface{}{"clientReferenceId": "t1"}}}), schemas.ErrorTypeCallerUnsupportedParameter, "client_reference_id"},
		{"task control field on edit", edit("qwen-image-2.0", &schemas.ImageEditParameters{ExtraParams: map[string]interface{}{"callback_url": "https://hooks.tokun.sh/v/t1"}}), schemas.ErrorTypeCallerUnsupportedParameter, "callback_url"},
		{"response_format b64_json", generation("qwen-image-2.0", &schemas.ImageGenerationParameters{ResponseFormat: schemas.Ptr("b64_json")}), schemas.ErrorTypeCallerUnsupportedParameter, "response_format"},
		{"response_format b64_json on edit", edit("qwen-image-2.0", &schemas.ImageEditParameters{ResponseFormat: schemas.Ptr("b64_json")}), schemas.ErrorTypeCallerUnsupportedParameter, "response_format"},
		{"size auto", generation("qwen-image-2.0", &schemas.ImageGenerationParameters{Size: schemas.Ptr("auto")}), schemas.ErrorTypeCallerUnsupportedParameter, "size"},
		{"size tier", generation("qwen-image-3.0-pro", &schemas.ImageGenerationParameters{Size: schemas.Ptr("2K")}), schemas.ErrorTypeCallerUnsupportedParameter, "size"},
		{"size zero", edit("qwen-image-2.0", &schemas.ImageEditParameters{Size: schemas.Ptr("0x1024")}), schemas.ErrorTypeCallerUnsupportedParameter, "size"},
		{"z-image n", generation("z-image-turbo", &schemas.ImageGenerationParameters{N: schemas.Ptr(2)}), schemas.ErrorTypeCallerUnsupportedParameter, "n"},
		{"z-image input_images", generation("z-image-turbo", &schemas.ImageGenerationParameters{InputImages: []string{"https://example.com/in.png"}}), schemas.ErrorTypeCallerUnsupportedParameter, "input_images"},
		{"bare base64 input image", generation("qwen-image-2.0", &schemas.ImageGenerationParameters{InputImages: []string{"iVBORw0KGgo="}}), schemas.ErrorTypeCallerUnsupportedParameter, "input_images"},
		{"prompt_extend not a boolean", generation("qwen-image-2.0", extra("prompt_extend", "yes")), schemas.ErrorTypeCallerInvalidRequest, "prompt_extend"},
		{"watermark not a boolean", generation("qwen-image-2.0", extra("watermark", 1)), schemas.ErrorTypeCallerInvalidRequest, "watermark"},
		{"extra param naming a converted parameter", generation("qwen-image-2.0", &schemas.ImageGenerationParameters{Size: schemas.Ptr("1024x1024"), ExtraParams: map[string]interface{}{"Size": "4096*4096"}}), schemas.ErrorTypeCallerInvalidRequest, "Size"},
		{"extra param naming n when the request left it out", generation("qwen-image-2.0", extra("N", 6)), schemas.ErrorTypeCallerInvalidRequest, "N"},
		{"extra param naming the model", generation("qwen-image-2.0", extra("Model", "qwen-image-3.0-pro")), schemas.ErrorTypeCallerInvalidRequest, "Model"},
		{"extra param turning streaming on", generation("qwen-image-2.0", extra("stream", true)), schemas.ErrorTypeCallerInvalidRequest, "stream"},
		{"extra parameters object turning streaming on", generation("qwen-image-2.0", extra("parameters", map[string]interface{}{"stream": true})), schemas.ErrorTypeCallerInvalidRequest, "stream"},
		{"extra param naming the model under a vendor name", edit("qwen-image-2.0", &schemas.ImageEditParameters{ExtraParams: map[string]interface{}{"model_id": "qwen-image-3.0-pro"}}), schemas.ErrorTypeCallerInvalidRequest, "model_id"},
		{"extra param naming the input", generation("qwen-image-2.0", extra("input", map[string]interface{}{"messages": []interface{}{}})), schemas.ErrorTypeCallerInvalidRequest, "input.messages"},
		{"extra parameters object naming a converted parameter", generation("qwen-image-2.0", &schemas.ImageGenerationParameters{N: schemas.Ptr(1), ExtraParams: map[string]interface{}{"parameters": map[string]interface{}{"n": 6}}}), schemas.ErrorTypeCallerInvalidRequest, "parameters.n"},
		{"z-image edit", edit("z-image-turbo", nil), schemas.ErrorTypeCallerInvalidRequest, "model"},
		{"wan on the sync generation route", generation("wan2.6-t2i", nil), schemas.ErrorTypeCallerInvalidRequest, "model"},
		{"wan on the sync edit route", edit("wan2.6-image", nil), schemas.ErrorTypeCallerInvalidRequest, "model"},
		{"unknown model", generation("qwen3-max", nil), schemas.ErrorTypeCallerModelUnknown, "model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider, requests := newMockDashScopeImages(t, http.StatusOK, qwenImageSuccessBody)
			ctx := imageTestContext(t)
			ctx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)

			bifrostErr := tc.call(provider, ctx)

			if bifrostErr == nil || bifrostErr.Error == nil {
				t.Fatalf("got %+v, want a refusal", bifrostErr)
			}
			if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 {
				t.Errorf("status = %v, want 400", bifrostErr.StatusCode)
			}
			if bifrostErr.ExtraFields.ErrorType != tc.wantType {
				t.Errorf("error_type = %q, want %q", bifrostErr.ExtraFields.ErrorType, tc.wantType)
			}
			if bifrostErr.Error.Param != tc.wantParam {
				t.Errorf("error.param = %v, want %q", bifrostErr.Error.Param, tc.wantParam)
			}
			if n := len(requests()); n != 0 {
				t.Errorf("a refused request made %d upstream calls", n)
			}
		})
	}
}

// TestDashScopeDeclaresDeliverableImageFields pins the unified fields core lets through for the
// sync image operations, and that resolution (no documented Qwen-Image parameter) and the other
// undeliverable fields are refused with caller_unsupported_parameter.
func TestDashScopeDeclaresDeliverableImageFields(t *testing.T) {
	provider := newTestProvider(t, schemas.NetworkConfig{})
	generation := func(params schemas.ImageGenerationParameters) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: schemas.ImageGenerationRequest, ImageGenerationRequest: &schemas.BifrostImageGenerationRequest{
			Model: "qwen-image-2.0", Input: &schemas.ImageGenerationInput{Prompt: "a cat"}, Params: &params,
		}}
	}
	edit := func(params schemas.ImageEditParameters) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: schemas.ImageEditRequest, ImageEditRequest: &schemas.BifrostImageEditRequest{
			Model: "qwen-image-2.0", Input: &schemas.ImageEditInput{Prompt: "red sofa"}, Params: &params,
		}}
	}
	cases := []struct {
		name      string
		req       *schemas.BifrostRequest
		wantParam string
	}{
		{"generation: every delivered field", generation(schemas.ImageGenerationParameters{
			N: schemas.Ptr(2), Size: schemas.Ptr("1024x1024"), ResponseFormat: schemas.Ptr("url"), Seed: schemas.Ptr(1),
			NegativePrompt: schemas.Ptr("blurry"), InputImages: []string{"https://example.com/in.png"},
		}), ""},
		{"generation: resolution", generation(schemas.ImageGenerationParameters{Resolution: "2K"}), "resolution"},
		{"generation: aspect_ratio", generation(schemas.ImageGenerationParameters{AspectRatio: schemas.Ptr("16:9")}), "aspect_ratio"},
		{"generation: quality", generation(schemas.ImageGenerationParameters{Quality: schemas.Ptr("high")}), "quality"},
		{"generation: output_format", generation(schemas.ImageGenerationParameters{OutputFormat: schemas.Ptr("jpeg")}), "output_format"},
		{"generation: background", generation(schemas.ImageGenerationParameters{Background: schemas.Ptr("transparent")}), "background"},
		{"edit: every delivered field", edit(schemas.ImageEditParameters{
			N: schemas.Ptr(2), Size: schemas.Ptr("1024x1024"), ResponseFormat: schemas.Ptr("url"), Seed: schemas.Ptr(1), NegativePrompt: schemas.Ptr("blurry"),
		}), ""},
		{"edit: resolution", edit(schemas.ImageEditParameters{Resolution: "1K"}), "resolution"},
		{"edit: mask", edit(schemas.ImageEditParameters{Mask: []byte{1}}), "mask"},
		{"edit: aspect_ratio", edit(schemas.ImageEditParameters{AspectRatio: schemas.Ptr("1:1")}), "aspect_ratio"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bifrostErr := schemas.CheckDeclaredUnifiedFields(provider, tc.req)
			if tc.wantParam == "" {
				if bifrostErr != nil {
					t.Fatalf("refused %+v, want every field delivered", bifrostErr.Error)
				}
				return
			}
			if bifrostErr == nil || bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeCallerUnsupportedParameter || bifrostErr.Error.Param != tc.wantParam {
				t.Fatalf("got %+v, want caller_unsupported_parameter naming %q", bifrostErr, tc.wantParam)
			}
		})
	}
	if _, declared := provider.SupportedUnifiedFields(schemas.TranscriptionRequest); declared {
		t.Error("dashscope declares unified fields for an operation it does not implement")
	}
}

// TestDashScopeImageExtraParams pins where an extra param lands: inside parameters, the only
// object DashScope reads switches from, and only when the caller asked for the passthrough.
func TestDashScopeImageExtraParams(t *testing.T) {
	request := func() *schemas.BifrostImageGenerationRequest {
		return &schemas.BifrostImageGenerationRequest{
			Model: "qwen-image-2.0",
			Input: &schemas.ImageGenerationInput{Prompt: "a cat"},
			Params: &schemas.ImageGenerationParameters{
				N:           schemas.Ptr(1),
				ExtraParams: map[string]interface{}{"watermark": true, "future_switch": "on", "parameters": map[string]interface{}{"nested_switch": 2}},
			},
		}
	}
	t.Run("with the passthrough header", func(t *testing.T) {
		provider, requests := newMockDashScopeImages(t, http.StatusOK, qwenImageSuccessBody)
		ctx := imageTestContext(t)
		ctx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)
		if _, bifrostErr := provider.ImageGeneration(ctx, imageTestKey(), request()); bifrostErr != nil {
			t.Fatalf("ImageGeneration: %+v", bifrostErr.Error)
		}
		assertJSONEqual(t, string(requests()[0].body), `{"model":"qwen-image-2.0",
			"input":{"messages":[{"role":"user","content":[{"text":"a cat"}]}]},
			"parameters":{"n":1,"watermark":true,"future_switch":"on","nested_switch":2}}`)
	})
	t.Run("without it", func(t *testing.T) {
		provider, requests := newMockDashScopeImages(t, http.StatusOK, qwenImageSuccessBody)
		if _, bifrostErr := provider.ImageGeneration(imageTestContext(t), imageTestKey(), request()); bifrostErr != nil {
			t.Fatalf("ImageGeneration: %+v", bifrostErr.Error)
		}
		assertJSONEqual(t, string(requests()[0].body), `{"model":"qwen-image-2.0",
			"input":{"messages":[{"role":"user","content":[{"text":"a cat"}]}]},
			"parameters":{"n":1,"watermark":true}}`)
	})
}

// TestDashScopeImageVendorErrors pins how DashScope's failures reach the caller: an error status
// keeps its status and code, and a 200 without an image is never a success.
func TestDashScopeImageVendorErrors(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantStatus int
		wantType   schemas.ErrorType
		wantCode   string
	}{
		{"moderation refusal", 400, `{"request_id":"r1","code":"DataInspectionFailed","message":"Input data may contain inappropriate content."}`, 400, schemas.ErrorTypeCallerContentFiltered, "DataInspectionFailed"},
		{"parameter error", 400, `{"request_id":"r2","code":"InvalidParameter","message":"num_images_per_prompt must be 1"}`, 400, schemas.ErrorTypeCallerInvalidRequest, "InvalidParameter"},
		{"invalid key", 401, `{"request_id":"r3","code":"InvalidApiKey","message":"Invalid API-key provided."}`, 401, schemas.ErrorTypeProviderAuthFailed, "InvalidApiKey"},
		{"200 with no image and no error", 200, `{"output":{"choices":[]},"request_id":"r4"}`, 502, schemas.ErrorTypeProviderEmptyResult, ""},
		{"200 with a moderation error object", 200, `{"request_id":"r5","code":"IPInfringementSuspect","message":"suspected infringement"}`, 400, schemas.ErrorTypeCallerContentFiltered, "IPInfringementSuspect"},
		{"200 with a vendor failure object", 200, `{"request_id":"r6","code":"InternalError.Algo","message":"inference failed"}`, 502, schemas.ErrorTypeProviderServerError, "InternalError.Algo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider, requests := newMockDashScopeImages(t, tc.status, tc.body)

			resp, bifrostErr := provider.ImageGeneration(imageTestContext(t), imageTestKey(), &schemas.BifrostImageGenerationRequest{
				Model: "qwen-image-2.0", Input: &schemas.ImageGenerationInput{Prompt: "a cat"},
			})

			if resp != nil || bifrostErr == nil || bifrostErr.Error == nil {
				t.Fatalf("got response %+v, error %+v, want an error", resp, bifrostErr)
			}
			if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != tc.wantStatus {
				t.Errorf("status = %v, want %d", bifrostErr.StatusCode, tc.wantStatus)
			}
			if got := schemas.ClassifyErrorType(bifrostErr, schemas.ImageGenerationRequest); got != tc.wantType {
				t.Errorf("error_type = %q, want %q", got, tc.wantType)
			}
			if tc.wantCode != "" && (bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != tc.wantCode) {
				t.Errorf("error.code = %v, want %q verbatim", bifrostErr.Error.Code, tc.wantCode)
			}
			if n := len(requests()); n != 1 {
				t.Errorf("upstream calls = %d, want exactly 1 (no retry)", n)
			}
		})
	}
}

// TestToDashScopeImageSize pins the size conversion and what it refuses to guess.
func TestToDashScopeImageSize(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"1024x1024", "1024*1024", true},
		{"2688X1536", "2688*1536", true},
		{"1536*2688", "1536*2688", true},
		{" 512x512 ", "512*512", true},
		{"auto", "", false},
		{"2K", "", false},
		{"1024", "", false},
		{"x1024", "", false},
		{"1024x", "", false},
		{"1024x-1", "", false},
		{"1024x1024x3", "", false},
		{"16:9", "", false},
	}
	for _, tc := range cases {
		if got, ok := toDashScopeImageSize(tc.in); got != tc.want || ok != tc.ok {
			t.Errorf("toDashScopeImageSize(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// BenchmarkDashScopeImageConversion measures what the provider adds to an image request outside
// the upstream call: the parameter checks, the request conversion and the response conversion.
func BenchmarkDashScopeImageConversion(b *testing.B) {
	request := &schemas.BifrostImageGenerationRequest{
		Model: "qwen-image-2.0-pro",
		Input: &schemas.ImageGenerationInput{Prompt: "a cat on a sofa"},
		Params: &schemas.ImageGenerationParameters{
			N: schemas.Ptr(2), Size: schemas.Ptr("1536x1024"), Seed: schemas.Ptr(42), NegativePrompt: schemas.Ptr("low quality"),
			ExtraParams: map[string]interface{}{"prompt_extend": true, "watermark": false},
		},
	}
	var response DashScopeImageResponse
	if err := json.Unmarshal([]byte(qwenImageSuccessBody), &response); err != nil {
		b.Fatal(err)
	}
	now := time.Now()
	b.ReportAllocs()
	for b.Loop() {
		p := request.Params
		if bifrostErr := validateDashScopeImageParams(dashScopeImageQwen, p.N, p.Size, p.ResponseFormat, p.ExtraParams); bifrostErr != nil {
			b.Fatal(bifrostErr.Error.Message)
		}
		if _, err := ToDashScopeImageGenerationRequest(request); err != nil {
			b.Fatal(err)
		}
		if _, bifrostErr := ToBifrostImageGenerationResponse(&response, request.Model, now); bifrostErr != nil {
			b.Fatal(bifrostErr.Error.Message)
		}
	}
}

// TestDashScopeImageRawRequestBody pins raw-request-body mode: the caller's body reaches
// DashScope as written, and the typed fields, which are not sent, refuse nothing.
func TestDashScopeImageRawRequestBody(t *testing.T) {
	const raw = `{"model":"qwen-image-2.0","input":{"messages":[{"role":"user","content":[{"text":"raw"}]}]},"parameters":{"size":"1024*1024"}}`
	rawContext := func(t *testing.T) *schemas.BifrostContext {
		ctx := imageTestContext(t)
		ctx.SetValue(schemas.BifrostContextKeyUseRawRequestBody, true)
		return ctx
	}
	t.Run("generation", func(t *testing.T) {
		provider, requests := newMockDashScopeImages(t, http.StatusOK, qwenImageSuccessBody)
		resp, bifrostErr := provider.ImageGeneration(rawContext(t), imageTestKey(), &schemas.BifrostImageGenerationRequest{
			Model:          "qwen-image-2.0",
			Input:          &schemas.ImageGenerationInput{Prompt: "typed"},
			Params:         &schemas.ImageGenerationParameters{Size: schemas.Ptr("auto"), ResponseFormat: schemas.Ptr("b64_json")},
			RawRequestBody: []byte(raw),
		})
		if bifrostErr != nil || len(resp.Data) != 2 {
			t.Fatalf("got %+v, %+v; want the raw body sent and the answer converted", resp, bifrostErr)
		}
		if got := requests(); len(got) != 1 || string(got[0].body) != raw {
			t.Errorf("requests = %+v, want the raw body byte for byte", got)
		}
	})
	t.Run("edit", func(t *testing.T) {
		provider, requests := newMockDashScopeImages(t, http.StatusOK, qwenImageSuccessBody)
		_, bifrostErr := provider.ImageEdit(rawContext(t), imageTestKey(), &schemas.BifrostImageEditRequest{
			Model:          "qwen-image-2.0",
			Input:          &schemas.ImageEditInput{Prompt: "typed"},
			Params:         &schemas.ImageEditParameters{Size: schemas.Ptr("auto")},
			RawRequestBody: []byte(raw),
		})
		if bifrostErr != nil {
			t.Fatalf("ImageEdit: %+v", bifrostErr.Error)
		}
		if got := requests(); len(got) != 1 || string(got[0].body) != raw {
			t.Errorf("requests = %+v, want the raw body byte for byte", got)
		}
	})
	t.Run("raw mode without a body is refused", func(t *testing.T) {
		provider, requests := newMockDashScopeImages(t, http.StatusOK, qwenImageSuccessBody)
		_, bifrostErr := provider.ImageGeneration(rawContext(t), imageTestKey(), &schemas.BifrostImageGenerationRequest{
			Model: "qwen-image-2.0", Input: &schemas.ImageGenerationInput{Prompt: "typed"},
		})
		if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 || len(requests()) != 0 {
			t.Fatalf("got %+v with %d upstream calls, want a 400 and none", bifrostErr, len(requests()))
		}
	})
}

// TestDashScopeImageKeepsProviderResponseHeaders pins that DashScope's response headers reach
// the context the transport forwards them from, on a success and on a vendor error.
func TestDashScopeImageKeepsProviderResponseHeaders(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"success", http.StatusOK, qwenImageSuccessBody},
		{"throttled", http.StatusTooManyRequests, `{"request_id":"r1","code":"Throttling.RateQuota","message":"Requests rate limit exceeded"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider, _ := newMockDashScopeImages(t, tc.status, tc.body)
			ctx := imageTestContext(t)
			_, _ = provider.ImageGeneration(ctx, imageTestKey(), &schemas.BifrostImageGenerationRequest{
				Model: "qwen-image-2.0", Input: &schemas.ImageGenerationInput{Prompt: "a cat"},
			})
			headers, _ := ctx.Value(schemas.BifrostContextKeyProviderResponseHeaders).(map[string]string)
			found := map[string]string{}
			for name, value := range headers {
				found[strings.ToLower(name)] = value
			}
			if found["retry-after"] != "7" || found["x-request-id"] != "req-header-1" {
				t.Fatalf("provider response headers = %v, want Retry-After and X-Request-Id kept", headers)
			}
		})
	}
}
