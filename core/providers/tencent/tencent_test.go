package tencent_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/providers/tencent"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

// Response bodies follow the examples of TokenHub's "Hy 生图调用指南"
// (https://cloud.tencent.com/document/product/1823/135745, updated 2026-10-12), trimmed of
// assembled_history and the signed URL query strings.
const (
	v3SuccessBody = `{"id":"4-WandImage-a786becfdc80433b8cff4aa344c8fd3d","created":1762000000,` +
		`"data":[{"url":"https://aigc-image.cos.myqcloud.com/xxx/result.png","revised_prompt":"a cat on a sofa, warm light"}],` +
		`"request_id":"req-v3","tokenhub_usage":{"total_tokens":20000}}`
	v35SuccessBody = `{"id":"1374200352-WandImage-085edfe2367d4a688f68e813af3665a5","object":"image.chat.completion.chunk",` +
		`"created":1762000001,"model":"hy-image-v3.5-preview-20261001","round":1,"request_id":"req-v35",` +
		`"choices":[{"index":0,"finish_reason":null,"delta":{"type":"image","image":{"url":"https://aigc-output-image-file-1326893053.cos.ap-guangzhou.myqcloud.com/xxx/main.png","width":4096,"height":4096,"source":"generate","tool_call_id":"call_1"}}}],` +
		`"usage":{"total_tokens":20000},"tokenhub_usage":{"total_tokens":20000}}`
	v35BlockedBody = `{"id":"1374200352-WandImage-blocked","object":"image.chat.completion.chunk","created":1762000002,` +
		`"model":"hy-image-v3.5-preview-20261001","round":1,"choices":[{"index":0,"delta":{},"finish_reason":"error"}],` +
		`"error":{"type":"invalid_request_error","code":"content_filter","message":"input moderation rejected","request_id":"req-blocked"}}`
)

type capturedRequest struct {
	path string
	auth string
	body []byte
}

func newMockTencent(t *testing.T, config *schemas.ProviderConfig, status int, body string) (*tencent.TencentProvider, func() []capturedRequest) {
	t.Helper()
	var mu sync.Mutex
	var captured []capturedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ := io.ReadAll(r.Body)
		mu.Lock()
		captured = append(captured, capturedRequest{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: reqBody})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	if config == nil {
		config = &schemas.ProviderConfig{}
	}
	config.NetworkConfig.BaseURL = server.URL
	config.NetworkConfig.DefaultRequestTimeoutInSeconds = 10
	config.NetworkConfig.AllowPrivateNetwork = true
	provider, err := tencent.NewTencentProvider(config, bifrost.NewDefaultLogger(schemas.LogLevelError))
	if err != nil {
		t.Fatalf("NewTencentProvider: %v", err)
	}
	return provider, func() []capturedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]capturedRequest(nil), captured...)
	}
}

func testContext(t *testing.T) *schemas.BifrostContext {
	t.Helper()
	ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func testKey() schemas.Key {
	return schemas.Key{Value: *schemas.NewSecretVar("test-key"), Models: []string{"*"}}
}

func imageRequest(model string, params *schemas.ImageGenerationParameters) *schemas.BifrostImageGenerationRequest {
	return &schemas.BifrostImageGenerationRequest{
		Provider: schemas.Tencent,
		Model:    model,
		Input:    &schemas.ImageGenerationInput{Prompt: "a cat on a sofa"},
		Params:   params,
	}
}

// HunyuanImage 3.0: the unified prompt, size, seed and input images reach v3-generation unchanged,
// and the response carries data[], output_images and TokenHub's image tokens.
func TestTencentImageGenerationV3(t *testing.T) {
	provider, captured := newMockTencent(t, nil, http.StatusOK, v3SuccessBody)

	resp, bifrostErr := provider.ImageGeneration(testContext(t), testKey(), imageRequest("hy-image-v3", &schemas.ImageGenerationParameters{
		N:           schemas.Ptr(1),
		Size:        schemas.Ptr("1024x768"),
		Seed:        schemas.Ptr(42),
		InputImages: []string{"https://example.com/ref.png", "data:image/png;base64,iVBORw0KGgo="},
	}))
	if bifrostErr != nil {
		t.Fatalf("ImageGeneration error: %+v", bifrostErr.Error)
	}

	reqs := captured()
	if len(reqs) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(reqs))
	}
	if reqs[0].path != "/v1/wand/hunyuan-image/v3-generation" {
		t.Errorf("path = %q", reqs[0].path)
	}
	if reqs[0].auth != "Bearer test-key" {
		t.Errorf("Authorization = %q", reqs[0].auth)
	}
	body := gjson.ParseBytes(reqs[0].body)
	if body.Get("model").String() != "hy-image-v3" || body.Get("prompt").String() != "a cat on a sofa" {
		t.Errorf("model/prompt not sent: %s", reqs[0].body)
	}
	if body.Get("size").String() != "1024x768" || body.Get("seed").Int() != 42 {
		t.Errorf("size/seed not sent unchanged: %s", reqs[0].body)
	}
	if imgs := body.Get("images").Array(); len(imgs) != 2 || imgs[1].String() != "data:image/png;base64,iVBORw0KGgo=" {
		t.Errorf("images = %s", body.Get("images").Raw)
	}
	if body.Get("n").Exists() || body.Get("messages").Exists() {
		t.Errorf("v3 body carries a field TokenHub does not take: %s", reqs[0].body)
	}

	if len(resp.Data) != 1 || resp.Data[0].URL != "https://aigc-image.cos.myqcloud.com/xxx/result.png" || resp.Data[0].RevisedPrompt != "a cat on a sofa, warm light" {
		t.Errorf("data = %+v", resp.Data)
	}
	if resp.ID != "4-WandImage-a786becfdc80433b8cff4aa344c8fd3d" || resp.Created != 1762000000 || resp.Model != "hy-image-v3" {
		t.Errorf("id/created/model = %q/%d/%q", resp.ID, resp.Created, resp.Model)
	}
	assertTokenHubUsage(t, resp.Usage, 1, 20000)
}

// Hy Image 3.5 Preview: the prompt and input images become one user message of v35-generation, and
// the image's pixel size is reported as the response size (4096x4096 is the 4K price tier).
func TestTencentImageGenerationV35(t *testing.T) {
	provider, captured := newMockTencent(t, nil, http.StatusOK, v35SuccessBody)

	resp, bifrostErr := provider.ImageGeneration(testContext(t), testKey(), imageRequest("hy-image-v3.5-preview", &schemas.ImageGenerationParameters{
		Size:           schemas.Ptr("4096x4096"),
		ResponseFormat: schemas.Ptr("url"),
		InputImages:    []string{"https://example.com/ref.png"},
	}))
	if bifrostErr != nil {
		t.Fatalf("ImageGeneration error: %+v", bifrostErr.Error)
	}

	reqs := captured()
	if len(reqs) != 1 || reqs[0].path != "/v1/wand/hunyuan-image/v35-generation" {
		t.Fatalf("upstream calls = %+v, want one to v35-generation", reqs)
	}
	body := gjson.ParseBytes(reqs[0].body)
	if body.Get("model").String() != "hy-image-v3.5-preview" || body.Get("size").String() != "4096x4096" {
		t.Errorf("model/size not sent unchanged: %s", reqs[0].body)
	}
	if body.Get("prompt").Exists() || body.Get("response_format").Exists() {
		t.Errorf("v3.5 body carries a field TokenHub does not take: %s", reqs[0].body)
	}
	msgs := body.Get("messages").Array()
	if len(msgs) != 1 || msgs[0].Get("role").String() != "user" {
		t.Fatalf("messages = %s", body.Get("messages").Raw)
	}
	parts := msgs[0].Get("content").Array()
	if len(parts) != 2 ||
		parts[0].Get("type").String() != "text" || parts[0].Get("text").String() != "a cat on a sofa" ||
		parts[1].Get("type").String() != "image_url" || parts[1].Get("image_url.url").String() != "https://example.com/ref.png" {
		t.Errorf("content = %s", msgs[0].Get("content").Raw)
	}

	if len(resp.Data) != 1 || !strings.HasPrefix(resp.Data[0].URL, "https://aigc-output-image-file-1326893053.cos.ap-guangzhou.myqcloud.com/") {
		t.Errorf("data = %+v", resp.Data)
	}
	if resp.ImageGenerationResponseParameters == nil || resp.ImageGenerationResponseParameters.Size != "4096x4096" {
		t.Errorf("size = %+v, want 4096x4096", resp.ImageGenerationResponseParameters)
	}
	if resp.Model != "hy-image-v3.5-preview" {
		t.Errorf("model = %q, want the requested name", resp.Model)
	}
	assertTokenHubUsage(t, resp.Usage, 1, 20000)
}

func assertTokenHubUsage(t *testing.T, usage *schemas.ImageUsage, images, tokens int) {
	t.Helper()
	if usage == nil {
		t.Fatal("usage is nil")
	}
	if usage.OutputImages == nil || *usage.OutputImages != images {
		t.Errorf("output_images = %v, want %d", usage.OutputImages, images)
	}
	if usage.TotalTokens != tokens || usage.OutputTokens != tokens {
		t.Errorf("total_tokens/output_tokens = %d/%d, want %d", usage.TotalTokens, usage.OutputTokens, tokens)
	}
	if usage.OutputTokensDetails == nil || usage.OutputTokensDetails.ImageTokens != tokens {
		t.Errorf("output_tokens_details = %+v, want image_tokens %d", usage.OutputTokensDetails, tokens)
	}
	if usage.InputTokens != 0 || usage.InputTokensDetails != nil || usage.VendorCost != nil {
		t.Errorf("usage carries a quantity TokenHub did not report: %+v", usage)
	}
}

// A response without a usage block leaves the tokens empty rather than filling a default.
func TestTencentImageGenerationWithoutUsageLeavesTokensEmpty(t *testing.T) {
	provider, _ := newMockTencent(t, nil, http.StatusOK, `{"id":"x","created":1,"data":[{"url":"https://cdn.example/a.png"}]}`)
	resp, bifrostErr := provider.ImageGeneration(testContext(t), testKey(), imageRequest("hy-image-v3", nil))
	if bifrostErr != nil {
		t.Fatalf("ImageGeneration error: %+v", bifrostErr.Error)
	}
	if resp.Usage.TotalTokens != 0 || resp.Usage.OutputTokensDetails != nil {
		t.Errorf("tokens filled without a vendor report: %+v", resp.Usage)
	}
}

// v3.5 reports a moderation block as finish_reason "error" with code content_filter on a 200. It
// becomes 400 caller_content_filtered with the vendor's code and body kept (contract C6).
func TestTencentImageGenerationBlockedOn200(t *testing.T) {
	provider, _ := newMockTencent(t, &schemas.ProviderConfig{SendBackRawResponse: true}, http.StatusOK, v35BlockedBody)

	_, bifrostErr := provider.ImageGeneration(testContext(t), testKey(), imageRequest("hy-image-v3.5-preview", nil))
	if bifrostErr == nil {
		t.Fatal("expected an error for a blocked image")
	}
	if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %v, want 400", bifrostErr.StatusCode)
	}
	if bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeCallerContentFiltered {
		t.Errorf("error_type = %q, want caller_content_filtered", bifrostErr.ExtraFields.ErrorType)
	}
	if bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != "content_filter" || bifrostErr.Error.Message != "input moderation rejected" {
		t.Errorf("error = %+v, want the vendor's code and message", bifrostErr.Error)
	}
	if bifrostErr.IsBifrostError {
		t.Error("a vendor block must not be marked as Bifrost's own failure")
	}
	assertRawResponseKept(t, bifrostErr, "req-blocked")
}

// TokenHub documents HTTP 422 for an input or output moderation block. The status is kept and the
// error is caller_content_filtered.
func TestTencentImageGenerationBlockedOn422(t *testing.T) {
	body := `{"error":{"type":"invalid_request_error","code":"content_filter","message":"output moderation rejected","request_id":"req-422"}}`
	provider, _ := newMockTencent(t, &schemas.ProviderConfig{SendBackRawResponse: true}, http.StatusUnprocessableEntity, body)

	_, bifrostErr := provider.ImageGeneration(testContext(t), testKey(), imageRequest("hy-image-v3", nil))
	if bifrostErr == nil {
		t.Fatal("expected an error")
	}
	if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %v, want the vendor's 422", bifrostErr.StatusCode)
	}
	if bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeCallerContentFiltered {
		t.Errorf("error_type = %q, want caller_content_filtered", bifrostErr.ExtraFields.ErrorType)
	}
	if bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != "content_filter" {
		t.Errorf("code = %v", bifrostErr.Error.Code)
	}
	assertRawResponseKept(t, bifrostErr, "req-422")
}

// A non-moderation vendor error keeps its status and is not classified as a block.
func TestTencentImageGenerationVendorErrorKeepsStatus(t *testing.T) {
	body := `{"error":{"type":"invalid_request_error","code":"invalid_size","message":"size out of range"}}`
	provider, _ := newMockTencent(t, nil, http.StatusBadRequest, body)

	_, bifrostErr := provider.ImageGeneration(testContext(t), testKey(), imageRequest("hy-image-v3", &schemas.ImageGenerationParameters{Size: schemas.Ptr("9000x9000")}))
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected the vendor's 400, got %+v", bifrostErr)
	}
	if bifrostErr.ExtraFields.ErrorType == schemas.ErrorTypeCallerContentFiltered {
		t.Error("a parameter error was classified as a content block")
	}
	if bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != "invalid_size" {
		t.Errorf("code = %v", bifrostErr.Error.Code)
	}
}

// A 200 failure that is not a block is the vendor's: 502 with its code verbatim.
func TestTencentImageGenerationVendorFailureOn200(t *testing.T) {
	body := `{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"error"}],"error":{"type":"server_error","code":"internal_error","message":"inference failed"}}`
	provider, _ := newMockTencent(t, nil, http.StatusOK, body)

	_, bifrostErr := provider.ImageGeneration(testContext(t), testKey(), imageRequest("hy-image-v3.5-preview", nil))
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %+v", bifrostErr)
	}
	if bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != "internal_error" {
		t.Errorf("code = %v", bifrostErr.Error.Code)
	}
}

// A 200 with no image and no error object is provider_empty_result (contract C6).
func TestTencentImageGenerationEmptyResult(t *testing.T) {
	provider, _ := newMockTencent(t, nil, http.StatusOK, `{"id":"x","created":1,"data":[]}`)

	_, bifrostErr := provider.ImageGeneration(testContext(t), testKey(), imageRequest("hy-image-v3", nil))
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %+v", bifrostErr)
	}
	if bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeProviderEmptyResult {
		t.Errorf("error_type = %q, want provider_empty_result", bifrostErr.ExtraFields.ErrorType)
	}
}

func assertRawResponseKept(t *testing.T, bifrostErr *schemas.BifrostError, requestID string) {
	t.Helper()
	raw, err := schemas.MarshalSorted(bifrostErr.ExtraFields.RawResponse)
	if err != nil || !strings.Contains(string(raw), requestID) {
		t.Errorf("raw_response = %s, want the vendor's body (request_id %s)", raw, requestID)
	}
}

// A value TokenHub cannot produce is refused with caller_unsupported_parameter and no upstream call.
func TestTencentImageGenerationRefusesUndeliverableValues(t *testing.T) {
	cases := []struct {
		name   string
		params *schemas.ImageGenerationParameters
		param  string
	}{
		{"n above one", &schemas.ImageGenerationParameters{N: schemas.Ptr(2)}, "n"},
		{"base64 output", &schemas.ImageGenerationParameters{ResponseFormat: schemas.Ptr("b64_json")}, "response_format"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider, captured := newMockTencent(t, nil, http.StatusOK, v3SuccessBody)
			_, bifrostErr := provider.ImageGeneration(testContext(t), testKey(), imageRequest("hy-image-v3", tc.params))
			assertUnsupportedParameter(t, bifrostErr, tc.param)
			if n := len(captured()); n != 0 {
				t.Errorf("upstream calls = %d, want 0", n)
			}
		})
	}
}

// Every unified image field outside the declared list is refused by core's declaration check.
func TestTencentDeclaresDeliverableImageFields(t *testing.T) {
	provider, _ := newMockTencent(t, nil, http.StatusOK, v3SuccessBody)
	supported, declared := provider.SupportedUnifiedFields(schemas.ImageGenerationRequest)
	if !declared {
		t.Fatal("image generation must be declared")
	}
	for _, want := range []string{"n", "size", "seed", "input_images", "response_format"} {
		if !containsString(supported, want) {
			t.Errorf("%q is delivered but not declared", want)
		}
	}
	if _, declared := provider.SupportedUnifiedFields(schemas.ImageEditRequest); declared {
		t.Error("image edit is not served and must not be declared")
	}

	for field, params := range map[string]*schemas.ImageGenerationParameters{
		"quality":       {Quality: schemas.Ptr("high")},
		"aspect_ratio":  {AspectRatio: schemas.Ptr("16:9")},
		"resolution":    {Resolution: "4K"},
		"output_format": {OutputFormat: schemas.Ptr("png")},
	} {
		req := &schemas.BifrostRequest{RequestType: schemas.ImageGenerationRequest, ImageGenerationRequest: imageRequest("hy-image-v3.5-preview", params)}
		assertUnsupportedParameter(t, schemas.CheckDeclaredUnifiedFields(provider, req), field)
	}
	ok := &schemas.BifrostRequest{RequestType: schemas.ImageGenerationRequest, ImageGenerationRequest: imageRequest("hy-image-v3", &schemas.ImageGenerationParameters{
		N: schemas.Ptr(1), Size: schemas.Ptr("1024x1024"), Seed: schemas.Ptr(1), InputImages: []string{"https://example.com/a.png"}, ResponseFormat: schemas.Ptr("url"),
	})}
	if err := schemas.CheckDeclaredUnifiedFields(provider, ok); err != nil {
		t.Errorf("declared fields refused: %+v", err.Error)
	}
}

func assertUnsupportedParameter(t *testing.T, bifrostErr *schemas.BifrostError, param string) {
	t.Helper()
	if bifrostErr == nil {
		t.Fatalf("%s: expected caller_unsupported_parameter, got nil", param)
	}
	if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
		t.Errorf("%s: status = %v, want 400", param, bifrostErr.StatusCode)
	}
	if bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeCallerUnsupportedParameter || bifrostErr.Error.Param != param {
		t.Errorf("error_type/param = %q/%q, want caller_unsupported_parameter/%s", bifrostErr.ExtraFields.ErrorType, bifrostErr.Error.Param, param)
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// A model with no known TokenHub shape is refused before any call: the two endpoints take
// different bodies.
func TestTencentImageGenerationUnknownModel(t *testing.T) {
	provider, captured := newMockTencent(t, nil, http.StatusOK, v3SuccessBody)
	_, bifrostErr := provider.ImageGeneration(testContext(t), testKey(), imageRequest("hunyuan-lite", nil))
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %+v", bifrostErr)
	}
	if bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeCallerModelUnknown {
		t.Errorf("error_type = %q, want caller_model_unknown", bifrostErr.ExtraFields.ErrorType)
	}
	if n := len(captured()); n != 0 {
		t.Errorf("upstream calls = %d, want 0", n)
	}
}

// Extra fields reach TokenHub's body when the caller sends x-bf-passthrough-extra-params (C4
// agreement two): footnote and generate_max_pixels are TokenHub-only switches.
func TestTencentImageGenerationPassesExtraParams(t *testing.T) {
	provider, captured := newMockTencent(t, nil, http.StatusOK, v35SuccessBody)
	ctx := testContext(t)
	ctx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)

	_, bifrostErr := provider.ImageGeneration(ctx, testKey(), imageRequest("hy-image-v3.5-preview", &schemas.ImageGenerationParameters{
		ExtraParams: map[string]interface{}{"footnote": "tokun", "generate_max_pixels": 4194304},
	}))
	if bifrostErr != nil {
		t.Fatalf("ImageGeneration error: %+v", bifrostErr.Error)
	}
	body := gjson.ParseBytes(captured()[0].body)
	if body.Get("footnote").String() != "tokun" || body.Get("generate_max_pixels").Int() != 4194304 {
		t.Errorf("extra params not merged: %s", captured()[0].body)
	}
}

// Tokun configures Tencent as a custom provider built on tencent (contract C12). The custom
// provider reports its own name, honours allowed_requests and request_path_overrides.
func TestTencentAsCustomProviderBase(t *testing.T) {
	if !bifrost.IsSupportedBaseProvider(schemas.Tencent) {
		t.Fatal("tencent must be accepted as base_provider_type")
	}

	provider, captured := newMockTencent(t, &schemas.ProviderConfig{CustomProviderConfig: &schemas.CustomProviderConfig{
		CustomProviderKey:    "tkp_tencent",
		BaseProviderType:     schemas.Tencent,
		AllowedRequests:      &schemas.AllowedRequests{ImageGeneration: true},
		RequestPathOverrides: map[schemas.RequestType]string{schemas.ImageGenerationRequest: "/v1/wand/hunyuan-image/v3-generation-canary"},
	}}, http.StatusOK, v3SuccessBody)
	if got := provider.GetProviderKey(); got != "tkp_tencent" {
		t.Errorf("provider key = %q, want tkp_tencent", got)
	}
	if _, bifrostErr := provider.ImageGeneration(testContext(t), testKey(), imageRequest("hy-image-v3", nil)); bifrostErr != nil {
		t.Fatalf("ImageGeneration error: %+v", bifrostErr.Error)
	}
	if got := captured()[0].path; got != "/v1/wand/hunyuan-image/v3-generation-canary" {
		t.Errorf("path = %q, want the override", got)
	}

	gated, gatedCaptured := newMockTencent(t, &schemas.ProviderConfig{CustomProviderConfig: &schemas.CustomProviderConfig{
		CustomProviderKey: "tkp_tencent_chat",
		BaseProviderType:  schemas.Tencent,
		AllowedRequests:   &schemas.AllowedRequests{ChatCompletion: true},
	}}, http.StatusOK, v3SuccessBody)
	if _, bifrostErr := gated.ImageGeneration(testContext(t), testKey(), imageRequest("hy-image-v3", nil)); bifrostErr == nil {
		t.Error("image generation ran on a custom provider that does not allow it")
	}
	if n := len(gatedCaptured()); n != 0 {
		t.Errorf("upstream calls = %d, want 0", n)
	}
}

// Operations TokenHub does not serve through this provider answer unsupported without a call.
func TestTencentUnsupportedOperations(t *testing.T) {
	provider, captured := newMockTencent(t, nil, http.StatusOK, v3SuccessBody)
	if _, bifrostErr := provider.ChatCompletion(testContext(t), testKey(), &schemas.BifrostChatRequest{Model: "hunyuan-turbos-latest"}); bifrostErr == nil {
		t.Error("chat must be unsupported: Tencent chat runs on an openai-based custom provider")
	}
	if _, bifrostErr := provider.ImageEdit(testContext(t), testKey(), &schemas.BifrostImageEditRequest{Model: "hy-image-v3.5-preview"}); bifrostErr == nil {
		t.Error("image edit must be unsupported")
	}
	if n := len(captured()); n != 0 {
		t.Errorf("upstream calls = %d, want 0", n)
	}
}
