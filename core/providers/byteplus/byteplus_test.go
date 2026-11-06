package byteplus_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/internal/llmtests"
	"github.com/capsohq/bifrost/core/network/proxytest"
	"github.com/capsohq/bifrost/core/providers/byteplus"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

// TestByteplus is named to match the make test-core recipe, which derives TestByteplus from PROVIDER=byteplus.
func TestByteplus(t *testing.T) {
	t.Parallel()
	if strings.TrimSpace(os.Getenv("BYTEPLUS_API_KEY")) == "" {
		t.Skip("Skipping BytePlus tests because BYTEPLUS_API_KEY is not set")
	}

	client, ctx, cancel, err := llmtests.SetupTest()
	if err != nil {
		t.Fatalf("Error initializing test setup: %v", err)
	}
	defer cancel()
	defer client.Shutdown()

	testConfig := llmtests.ComprehensiveTestConfig{
		Provider:  schemas.BytePlus,
		ChatModel: "deepseek-v4-flash-260425",
		Fallbacks: []schemas.Fallback{
			{Provider: schemas.BytePlus, Model: "deepseek-v4-flash-260425"},
		},
		ReasoningModel:       "deepseek-v4-flash-260425",
		EmbeddingModel:       "skylark-embedding-vision-251215",
		ImageGenerationModel: "dola-seedream-5-0-pro-260628",
		VideoGenerationModel: "dreamina-seedance-2-5-260628",
		Scenarios: llmtests.TestScenarios{
			SimpleChat:            true,
			CompletionStream:      true,
			MultiTurnConversation: true,
			ToolCalls:             true,
			ToolCallsStreaming:    true,
			MultipleToolCalls:     true,
			End2EndToolCalling:    true,
			AutomaticFunctionCall: true,
			ImageURL:              false, // deepseek-v4-flash rejects image input
			ImageBase64:           false,
			CompleteEnd2End:       true,
			ListModels:            true,
			Reasoning:             true,
			Embedding:             true,
			ImageGeneration:       true,
			// The shared image edit test always sends a mask, which Seedream rejects.
			// Mask-free edits are covered by TestBytePlusImageGenerationAndEdit and the harness.
			ImageEdit:       false,
			VideoGeneration: os.Getenv("BYTEPLUS_RUN_VIDEO_TESTS") == "1", // Seedance bills per generated video
		},
	}

	t.Run("ByteplusTests", func(t *testing.T) {
		llmtests.RunAllComprehensiveTests(t, client, ctx, testConfig)
	})
}

type capturedRequest struct {
	method string
	path   string
	query  string
	auth   string
	body   []byte
}

// newMockBytePlus starts a server that records each request and replies with the response registered for "METHOD /path".
func newMockBytePlus(t *testing.T, responses map[string]func(w http.ResponseWriter, r *http.Request)) (*byteplus.BytePlusProvider, func() []capturedRequest, string) {
	t.Helper()
	var mu sync.Mutex
	var captured []capturedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		mu.Lock()
		captured = append(captured, capturedRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization"), body: body})
		mu.Unlock()
		if respond, ok := responses[r.Method+" "+r.URL.Path]; ok {
			respond(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	provider, err := byteplus.NewBytePlusProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{
			BaseURL:                        server.URL + "/",
			DefaultRequestTimeoutInSeconds: 10,
			AllowPrivateNetwork:            true,
		},
	}, bifrost.NewDefaultLogger(schemas.LogLevelError))
	if err != nil {
		t.Fatalf("NewBytePlusProvider: %v", err)
	}
	return provider, func() []capturedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]capturedRequest(nil), captured...)
	}, server.URL
}

func jsonResponse(status int, body string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
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

func userChatRequest(params *schemas.ChatParameters) *schemas.BifrostChatRequest {
	return &schemas.BifrostChatRequest{
		Provider: schemas.BytePlus,
		Model:    "seed-2-0-lite-260228",
		Input:    []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("hi")}}},
		Params:   params,
	}
}

const chatSuccessBody = `{"id":"c1","object":"chat.completion","created":1,"model":"seed-2-0-lite-260228","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hello","reasoning_content":"thinking"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5,"completion_tokens_details":{"reasoning_tokens":1}}}`

func TestBytePlusChatUsesArkPathAndReadsReasoningContent(t *testing.T) {
	provider, captured, _ := newMockBytePlus(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/v3/chat/completions": jsonResponse(http.StatusOK, chatSuccessBody),
	})
	params := &schemas.ChatParameters{ExtraParams: map[string]interface{}{"thinking": map[string]interface{}{"type": "enabled"}}}

	resp, bifrostErr := provider.ChatCompletion(testContext(t), testKey(), userChatRequest(params))
	if bifrostErr != nil {
		t.Fatalf("ChatCompletion error: %+v", bifrostErr.Error)
	}
	reqs := captured()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 upstream request, got %d", len(reqs))
	}
	if reqs[0].auth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", reqs[0].auth)
	}
	if got := gjson.GetBytes(reqs[0].body, "thinking.type").String(); got != "enabled" {
		t.Errorf("thinking extra param not passed through, got %q: %s", got, reqs[0].body)
	}
	msg := resp.Choices[0].ChatNonStreamResponseChoice.Message
	if msg.ChatAssistantMessage == nil || msg.ChatAssistantMessage.Reasoning == nil || *msg.ChatAssistantMessage.Reasoning != "thinking" {
		t.Errorf("reasoning_content not mapped to Reasoning: %+v", msg.ChatAssistantMessage)
	}
}

// Ark rejects reasoning_effort "none", and returns 400 when reasoning_effort is sent with thinking disabled.
func TestBytePlusChatReasoningEffortAndThinking(t *testing.T) {
	tests := []struct {
		name         string
		effort       string
		thinking     string // caller's thinking.type, "" for none
		wantEffort   string // "" means reasoning_effort must be absent
		wantThinking string // "" means thinking must be absent
	}{
		{name: "none disables thinking", effort: "none", wantThinking: "disabled"},
		{name: "effort dropped when caller disabled thinking", effort: "high", thinking: "disabled", wantThinking: "disabled"},
		{name: "effort kept with thinking enabled", effort: "low", thinking: "enabled", wantEffort: "low", wantThinking: "enabled"},
		{name: "none dropped when caller enabled thinking", effort: "none", thinking: "enabled", wantThinking: "enabled"},
		{name: "none dropped when caller set auto thinking", effort: "none", thinking: "auto", wantThinking: "auto"},
		{name: "minimal is kept", effort: "minimal", wantEffort: "minimal"},
		{name: "xhigh clamps to high", effort: "xhigh", wantEffort: "high"},
		{name: "max clamps to high", effort: "max", wantEffort: "high"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, captured, _ := newMockBytePlus(t, map[string]func(http.ResponseWriter, *http.Request){
				"POST /api/v3/chat/completions": jsonResponse(http.StatusOK, chatSuccessBody),
			})
			reasoning := &schemas.ChatReasoning{Effort: schemas.Ptr(tt.effort)}
			params := &schemas.ChatParameters{Reasoning: reasoning}
			if tt.thinking != "" {
				params.ExtraParams = map[string]interface{}{"thinking": map[string]interface{}{"type": tt.thinking}}
			}

			if _, bifrostErr := provider.ChatCompletion(testContext(t), testKey(), userChatRequest(params)); bifrostErr != nil {
				t.Fatalf("ChatCompletion error: %+v", bifrostErr.Error)
			}
			body := captured()[0].body
			effort := gjson.GetBytes(body, "reasoning_effort")
			if tt.wantEffort == "" && effort.Exists() {
				t.Errorf("reasoning_effort should be absent: %s", body)
			}
			if tt.wantEffort != "" && effort.String() != tt.wantEffort {
				t.Errorf("reasoning_effort = %q, want %q: %s", effort.String(), tt.wantEffort, body)
			}
			if got := gjson.GetBytes(body, "thinking.type").String(); got != tt.wantThinking {
				t.Errorf("thinking.type = %q, want %q: %s", got, tt.wantThinking, body)
			}
			if reasoning.Effort == nil || *reasoning.Effort != tt.effort {
				t.Errorf("caller's reasoning params were mutated: %+v", reasoning)
			}
		})
	}
}

func TestBytePlusErrorKeepsArkCode(t *testing.T) {
	provider, _, _ := newMockBytePlus(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/v3/chat/completions": jsonResponse(http.StatusNotFound, `{"error":{"code":"ModelNotOpen","message":"Your account has not activated the model seed-2-0-lite-260228.","param":"","type":"NotFound"}}`),
	})

	_, bifrostErr := provider.ChatCompletion(testContext(t), testKey(), userChatRequest(nil))
	if bifrostErr == nil {
		t.Fatal("expected an error")
	}
	if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusNotFound {
		t.Errorf("status code = %v, want 404", bifrostErr.StatusCode)
	}
	if bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != "ModelNotOpen" {
		t.Errorf("error code = %v, want ModelNotOpen", bifrostErr.Error.Code)
	}
	if !strings.Contains(bifrostErr.Error.Message, "has not activated") {
		t.Errorf("error message = %q", bifrostErr.Error.Message)
	}
}

func TestBytePlusResponsesUsesNativeEndpoint(t *testing.T) {
	provider, captured, _ := newMockBytePlus(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/v3/responses": jsonResponse(http.StatusOK, `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"seed-2-0-lite-260228","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`),
	})

	resp, bifrostErr := provider.Responses(testContext(t), testKey(), &schemas.BifrostResponsesRequest{
		Provider: schemas.BytePlus,
		Model:    "seed-2-0-lite-260228",
		Input:    []schemas.ResponsesMessage{llmtests.CreateBasicResponsesMessage("hi")},
	})
	if bifrostErr != nil {
		t.Fatalf("Responses error: %+v", bifrostErr.Error)
	}
	if reqs := captured(); len(reqs) != 1 || reqs[0].auth != "Bearer test-key" {
		t.Fatalf("unexpected upstream requests: %+v", reqs)
	}
	if len(resp.Output) != 1 {
		t.Errorf("expected 1 output item, got %d", len(resp.Output))
	}
}

// Ark's Responses decoder rejects reasoning.summary outright ("unknown field \"summary\""), even as null.
func TestBytePlusResponsesOmitsReasoningSummary(t *testing.T) {
	const responseBody = `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"deepseek-v4-flash-260425","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`
	for _, summary := range []*string{nil, schemas.Ptr("detailed")} {
		provider, captured, _ := newMockBytePlus(t, map[string]func(http.ResponseWriter, *http.Request){
			"POST /api/v3/responses": jsonResponse(http.StatusOK, responseBody),
		})
		_, bifrostErr := provider.Responses(testContext(t), testKey(), &schemas.BifrostResponsesRequest{
			Provider: schemas.BytePlus,
			Model:    "deepseek-v4-flash-260425",
			Input:    []schemas.ResponsesMessage{llmtests.CreateBasicResponsesMessage("hi")},
			Params:   &schemas.ResponsesParameters{Reasoning: &schemas.ResponsesParametersReasoning{Effort: schemas.Ptr("high"), Summary: summary}},
		})
		if bifrostErr != nil {
			t.Fatalf("Responses error: %+v", bifrostErr.Error)
		}
		body := captured()[0].body
		if gjson.GetBytes(body, "reasoning.summary").Exists() {
			t.Errorf("reasoning.summary must not be sent (summary=%v): %s", summary, body)
		}
		if got := gjson.GetBytes(body, "reasoning.effort").String(); got != "high" {
			t.Errorf("reasoning.effort = %q, want high: %s", got, body)
		}
	}
}

func TestBytePlusListModelsComesFromKeyConfig(t *testing.T) {
	provider, captured, _ := newMockBytePlus(t, nil)
	key := schemas.Key{Value: *schemas.NewSecretVar("test-key"), Models: []string{"seed-2-0-lite-260228", "seedream-4-5-251128"}}

	resp, bifrostErr := provider.ListModels(testContext(t), []schemas.Key{key}, &schemas.BifrostListModelsRequest{Provider: schemas.BytePlus})
	if bifrostErr != nil {
		t.Fatalf("ListModels error: %+v", bifrostErr.Error)
	}
	if len(captured()) != 0 {
		t.Errorf("ListModels should not call upstream, got %d requests", len(captured()))
	}
	ids := make([]string, 0, len(resp.Data))
	for _, model := range resp.Data {
		ids = append(ids, model.ID)
	}
	joined := strings.Join(ids, ",")
	if !strings.Contains(joined, "seed-2-0-lite-260228") || !strings.Contains(joined, "seedream-4-5-251128") {
		t.Errorf("models = %v, want both configured models", ids)
	}
}

func embeddingRequest(items ...schemas.EmbeddingInputItem) *schemas.BifrostEmbeddingRequest {
	return &schemas.BifrostEmbeddingRequest{Provider: schemas.BytePlus, Model: "skylark-embedding-vision-250615", Input: items}
}

func textItem(text string) schemas.EmbeddingInputItem {
	return schemas.EmbeddingInputItem{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: schemas.Ptr(text)}}}
}

// Per-item params override the request-level ones for dimensions; task_type and title have no Ark field and are not forwarded.
func TestBytePlusEmbeddingPerItemParams(t *testing.T) {
	provider, captured, _ := newMockBytePlus(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/v3/embeddings/multimodal": jsonResponse(http.StatusOK, `{"id":"e1","model":"skylark-embedding-vision-250615","object":"list","data":{"embedding":[0.1,0.2],"object":"embedding"},"usage":{"prompt_tokens":4,"total_tokens":4,"prompt_tokens_details":{"text_tokens":4,"image_tokens":0}}}`),
	})
	item := textItem("first")
	item.Params = &schemas.EmbeddingParameters{Dimensions: schemas.Ptr(1024), TaskType: schemas.Ptr("retrieval_query"), Title: schemas.Ptr("a title")}
	req := embeddingRequest(item)
	req.Params = &schemas.EmbeddingParameters{Dimensions: schemas.Ptr(2048)}

	if _, bifrostErr := provider.Embedding(testContext(t), testKey(), req); bifrostErr != nil {
		t.Fatalf("Embedding error: %+v", bifrostErr.Error)
	}
	body := captured()[0].body
	if gjson.GetBytes(body, "dimensions").Int() != 1024 {
		t.Errorf("per-item dimensions should override the request-level value: %s", body)
	}
	if gjson.GetBytes(body, "task_type").Exists() || gjson.GetBytes(body, "title").Exists() {
		t.Errorf("task_type and title are not forwarded to Ark: %s", body)
	}
}

// Ark's multimodal endpoint fuses a request's parts into one vector, so each input item is its own call.
func TestBytePlusEmbeddingFansOutOneCallPerItem(t *testing.T) {
	provider, captured, _ := newMockBytePlus(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/v3/embeddings/multimodal": func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			vector := `[0.1,0.2]`
			if gjson.GetBytes(body, "input.0.text").String() == "second" {
				vector = `[0.3,0.4]`
			}
			jsonResponse(http.StatusOK, `{"id":"e1","model":"skylark-embedding-vision-250615","object":"list","data":{"embedding":`+vector+`,"object":"embedding"},"usage":{"prompt_tokens":4,"total_tokens":4,"prompt_tokens_details":{"text_tokens":4,"image_tokens":0}}}`)(w, r)
		},
	})
	req := embeddingRequest(textItem("first"), textItem("second"))
	req.Params = &schemas.EmbeddingParameters{Dimensions: schemas.Ptr(1024)}

	resp, bifrostErr := provider.Embedding(testContext(t), testKey(), req)
	if bifrostErr != nil {
		t.Fatalf("Embedding error: %+v", bifrostErr.Error)
	}
	reqs := captured()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 upstream requests, got %d", len(reqs))
	}
	for _, r := range reqs {
		if gjson.GetBytes(r.body, "input.#").Int() != 1 || gjson.GetBytes(r.body, "input.0.type").String() != "text" {
			t.Errorf("each call should carry exactly one text part: %s", r.body)
		}
		if gjson.GetBytes(r.body, "dimensions").Int() != 1024 {
			t.Errorf("dimensions not forwarded: %s", r.body)
		}
	}
	if len(resp.Data) != 2 || resp.Data[0].Index != 0 || resp.Data[1].Index != 1 {
		t.Fatalf("unexpected data: %+v", resp.Data)
	}
	if got := resp.Data[1].Embedding.EmbeddingArray; len(got) != 2 || got[0] != 0.3 {
		t.Errorf("second vector out of order: %v", got)
	}
	if resp.Usage.PromptTokens != 8 || resp.Usage.TotalTokens != 8 {
		t.Errorf("usage not summed: %+v", resp.Usage)
	}
}

func TestToBytePlusMultimodalEmbeddingRequest(t *testing.T) {
	t.Run("image part becomes a data uri", func(t *testing.T) {
		item := schemas.EmbeddingInputItem{Content: schemas.EmbeddingContent{
			{Type: schemas.EmbeddingContentPartTypeText, Text: schemas.Ptr("caption")},
			{Type: schemas.EmbeddingContentPartTypeImage, Image: &schemas.EmbeddingMediaPart{Data: schemas.Ptr("aGVsbG8="), MIMEType: schemas.Ptr("image/png")}},
		}}
		req, err := byteplus.ToBytePlusMultimodalEmbeddingRequest("m", item, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(req.Input) != 2 || req.Input[1].Type != "image_url" || req.Input[1].ImageURL.URL != "data:image/png;base64,aGVsbG8=" {
			t.Errorf("unexpected input: %+v", req.Input)
		}
	})
	t.Run("item params override request params", func(t *testing.T) {
		item := textItem("x")
		item.Params = &schemas.EmbeddingParameters{Dimensions: schemas.Ptr(2048)}
		req, err := byteplus.ToBytePlusMultimodalEmbeddingRequest("m", item, &schemas.EmbeddingParameters{Dimensions: schemas.Ptr(1024)})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if req.Dimensions == nil || *req.Dimensions != 2048 {
			t.Errorf("dimensions = %v, want 2048", req.Dimensions)
		}
	})
	t.Run("rejects non-float encodings", func(t *testing.T) {
		_, err := byteplus.ToBytePlusMultimodalEmbeddingRequest("m", textItem("x"), &schemas.EmbeddingParameters{EncodingFormat: schemas.Ptr("base64")})
		if err == nil {
			t.Fatal("expected an error for base64 encoding")
		}
	})
	t.Run("rejects token parts", func(t *testing.T) {
		item := schemas.EmbeddingInputItem{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeTokens, Tokens: []int{1, 2}}}}
		if _, err := byteplus.ToBytePlusMultimodalEmbeddingRequest("m", item, nil); err == nil {
			t.Fatal("expected an error for token parts")
		}
	})
}

func TestToBytePlusImageGenerationRequest(t *testing.T) {
	req, err := byteplus.ToBytePlusImageGenerationRequest(&schemas.BifrostImageGenerationRequest{
		Model: "seedream-4-5-251128",
		Input: &schemas.ImageGenerationInput{Prompt: "a cat"},
		Params: &schemas.ImageGenerationParameters{
			N:              schemas.Ptr(3),
			Size:           schemas.Ptr("2048x2048"),
			ResponseFormat: schemas.Ptr("b64_json"),
			InputImages:    []string{"https://example.com/ref.png"},
			ExtraParams:    map[string]interface{}{"watermark": false, "guidance_scale": 5.5},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.SequentialImageGeneration == nil || *req.SequentialImageGeneration != "auto" || req.SequentialImageGenerationOptions == nil || req.SequentialImageGenerationOptions.MaxImages != 3 {
		t.Errorf("n=3 should enable sequential generation with max_images 3: %+v %+v", req.SequentialImageGeneration, req.SequentialImageGenerationOptions)
	}
	if req.Size == nil || *req.Size != "2048x2048" || req.ResponseFormat == nil || *req.ResponseFormat != "b64_json" {
		t.Errorf("size/response_format not mapped: %+v %+v", req.Size, req.ResponseFormat)
	}
	if len(req.Image) != 1 || req.Image[0] != "https://example.com/ref.png" {
		t.Errorf("input images not mapped: %v", req.Image)
	}
	if req.Watermark == nil || *req.Watermark {
		t.Errorf("watermark not lifted: %v", req.Watermark)
	}
	if _, ok := req.ExtraParams["guidance_scale"]; !ok {
		t.Errorf("unknown extra params should pass through: %v", req.ExtraParams)
	}
}

func TestBytePlusImageGenerationAndEdit(t *testing.T) {
	provider, captured, _ := newMockBytePlus(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/v3/images/generations": jsonResponse(http.StatusOK, `{"model":"seedream-4-5-251128","created":1757321139,"data":[{"url":"https://cdn.example/1.png","size":"2048x2048"},{"error":{"code":"OutputImageSensitiveContentDetected","message":"blocked"}}],"usage":{"generated_images":1,"output_tokens":16384,"total_tokens":16384}}`),
	})
	ctx := testContext(t)

	resp, bifrostErr := provider.ImageGeneration(ctx, testKey(), &schemas.BifrostImageGenerationRequest{
		Provider: schemas.BytePlus,
		Model:    "seedream-4-5-251128",
		Input:    &schemas.ImageGenerationInput{Prompt: "a cat"},
	})
	if bifrostErr != nil {
		t.Fatalf("ImageGeneration error: %+v", bifrostErr.Error)
	}
	if len(resp.Data) != 1 || resp.Data[0].URL != "https://cdn.example/1.png" {
		t.Errorf("failed images should be skipped, got %+v", resp.Data)
	}
	if resp.Usage == nil || resp.Usage.OutputTokens != 16384 || resp.Created != 1757321139 {
		t.Errorf("usage/created not mapped: %+v created=%d", resp.Usage, resp.Created)
	}

	if _, bifrostErr := provider.ImageEdit(ctx, testKey(), &schemas.BifrostImageEditRequest{
		Provider: schemas.BytePlus,
		Model:    "seedream-4-5-251128",
		Input:    &schemas.ImageEditInput{Prompt: "make it blue", Images: []schemas.ImageInput{{Image: []byte("\x89PNG\r\n\x1a\n0000")}, {URL: "https://example.com/b.png"}}},
	}); bifrostErr != nil {
		t.Fatalf("ImageEdit error: %+v", bifrostErr.Error)
	}
	edit := captured()[1].body
	if got := gjson.GetBytes(edit, "image.0").String(); !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Errorf("image bytes should be sent as a data uri, got %q", got)
	}
	if got := gjson.GetBytes(edit, "image.1").String(); got != "https://example.com/b.png" {
		t.Errorf("image url not forwarded, got %q", got)
	}
}

// Converter refusals are the caller's mistake, so they must surface as a 400 with the reason, not a conversion failure.
// background is a typed OpenAI-shaped field, so the HTTP layer never puts it in ExtraParams.
func TestToBytePlusImageGenerationRequestDropsInvalidSequentialOptions(t *testing.T) {
	for name, invalid := range map[string]interface{}{"string": "bogus", "zero max_images": map[string]interface{}{"max_images": 0}} {
		t.Run(name, func(t *testing.T) {
			req, err := byteplus.ToBytePlusImageGenerationRequest(&schemas.BifrostImageGenerationRequest{
				Model: "seedream-4-5-251128",
				Input: &schemas.ImageGenerationInput{Prompt: "a cat"},
				Params: &schemas.ImageGenerationParameters{
					N:           schemas.Ptr(2),
					ExtraParams: map[string]interface{}{"sequential_image_generation_options": invalid},
				},
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if req.SequentialImageGenerationOptions == nil || req.SequentialImageGenerationOptions.MaxImages != 2 {
				t.Errorf("n=2 should keep max_images 2: %+v", req.SequentialImageGenerationOptions)
			}
			if _, ok := req.ExtraParams["sequential_image_generation_options"]; ok {
				t.Errorf("invalid passthrough value would overwrite the typed options at merge time: %v", req.ExtraParams)
			}
		})
	}
}

func TestBytePlusImageTypedBackgroundReachesArk(t *testing.T) {
	gen, err := byteplus.ToBytePlusImageGenerationRequest(&schemas.BifrostImageGenerationRequest{
		Model:  "dola-seedream-5-0-pro-260628",
		Input:  &schemas.ImageGenerationInput{Prompt: "a logo"},
		Params: &schemas.ImageGenerationParameters{Background: schemas.Ptr("transparent")},
	})
	if err != nil {
		t.Fatalf("generation convert: %v", err)
	}
	genBody, _ := json.Marshal(gen)
	if gjson.GetBytes(genBody, "background").String() != "transparent" {
		t.Errorf("generation background should reach Ark, got %s", genBody)
	}
	edit, err := byteplus.ToBytePlusImageEditRequest(&schemas.BifrostImageEditRequest{
		Model:  "dola-seedream-5-0-pro-260628",
		Input:  &schemas.ImageEditInput{Prompt: "a logo", Images: []schemas.ImageInput{{URL: "https://example.com/a.png"}}},
		Params: &schemas.ImageEditParameters{Background: schemas.Ptr("transparent")},
	})
	if err != nil {
		t.Fatalf("edit convert: %v", err)
	}
	editBody, _ := json.Marshal(edit)
	if gjson.GetBytes(editBody, "background").String() != "transparent" {
		t.Errorf("edit background should reach Ark, got %s", editBody)
	}
}

func TestBytePlusConverterRefusalsAreBadRequests(t *testing.T) {
	provider, captured, _ := newMockBytePlus(t, nil)
	ctx := testContext(t)
	check := func(name string, bifrostErr *schemas.BifrostError, wantMessage string) {
		t.Helper()
		if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: expected a 400, got %+v", name, bifrostErr)
			return
		}
		if !strings.Contains(bifrostErr.Error.Message, wantMessage) {
			t.Errorf("%s: message = %q, want it to mention %q", name, bifrostErr.Error.Message, wantMessage)
		}
	}

	_, bifrostErr := provider.ImageEdit(ctx, testKey(), &schemas.BifrostImageEditRequest{
		Provider: schemas.BytePlus,
		Model:    "dola-seedream-5-0-pro-260628",
		Input:    &schemas.ImageEditInput{Prompt: "make it blue", Images: []schemas.ImageInput{{URL: "https://example.com/a.png"}}},
		Params:   &schemas.ImageEditParameters{Mask: []byte("mask")},
	})
	check("image edit mask", bifrostErr, "mask is not supported")

	_, bifrostErr = provider.VideoGeneration(ctx, testKey(), &schemas.BifrostVideoGenerationRequest{
		Provider: schemas.BytePlus,
		Model:    "dreamina-seedance-2-5-260628",
		Input:    &schemas.VideoGenerationInput{Prompt: "a dog", InputReference: schemas.Ptr("https://example.com/first.png")},
		Params:   &schemas.VideoGenerationParameters{ExtraParams: map[string]any{"reference_images": []string{"https://example.com/r.png"}}},
	})
	check("video reference_images with first frame", bifrostErr, "cannot be combined")

	if len(captured()) != 0 {
		t.Errorf("refused requests should not reach Ark, got %d calls", len(captured()))
	}
}

func TestBytePlusImageGenerationAllImagesFailed(t *testing.T) {
	provider, _, _ := newMockBytePlus(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/v3/images/generations": jsonResponse(http.StatusOK, `{"model":"seedream-4-5-251128","created":1,"data":[{"error":{"code":"OutputImageSensitiveContentDetected","message":"blocked"}}]}`),
	})

	_, bifrostErr := provider.ImageGeneration(testContext(t), testKey(), &schemas.BifrostImageGenerationRequest{
		Provider: schemas.BytePlus,
		Model:    "seedream-4-5-251128",
		Input:    &schemas.ImageGenerationInput{Prompt: "a cat"},
	})
	if bifrostErr == nil {
		t.Fatal("expected an error when every image failed")
	}
	if bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != "OutputImageSensitiveContentDetected" {
		t.Errorf("error code = %v", bifrostErr.Error.Code)
	}
}

func TestToBytePlusVideoGenerationRequest(t *testing.T) {
	req, err := byteplus.ToBytePlusVideoGenerationRequest(&schemas.BifrostVideoGenerationRequest{
		Model: "seedance-1-0-pro-fast-251015",
		Input: &schemas.VideoGenerationInput{Prompt: "a dog", InputReference: schemas.Ptr("https://example.com/first.png")},
		Params: &schemas.VideoGenerationParameters{
			Seconds:     schemas.Ptr("5"),
			Size:        "1280x720",
			Seed:        schemas.Ptr(7),
			Audio:       schemas.Ptr(true),
			ExtraParams: map[string]any{"last_frame": "https://example.com/last.png", "camera_fixed": true},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(req.Content) != 3 || req.Content[1].Role != "first_frame" || req.Content[2].Role != "last_frame" {
		t.Errorf("unexpected content: %+v", req.Content)
	}
	if req.Duration == nil || *req.Duration != 5 || req.Resolution == nil || *req.Resolution != "720p" || req.Ratio == nil || *req.Ratio != "16:9" {
		t.Errorf("duration/resolution/ratio not mapped: %v %v %v", req.Duration, req.Resolution, req.Ratio)
	}
	if req.GenerateAudio == nil || !*req.GenerateAudio || req.Seed == nil || *req.Seed != 7 {
		t.Errorf("audio/seed not mapped: %v %v", req.GenerateAudio, req.Seed)
	}
	if req.ExtraParams["camera_fixed"] != true {
		t.Errorf("camera_fixed should pass through: %v", req.ExtraParams)
	}

	_, err = byteplus.ToBytePlusVideoGenerationRequest(&schemas.BifrostVideoGenerationRequest{
		Model:  "seedance-1-0-pro-fast-251015",
		Input:  &schemas.VideoGenerationInput{Prompt: "a dog", InputReference: schemas.Ptr("https://example.com/first.png")},
		Params: &schemas.VideoGenerationParameters{ExtraParams: map[string]any{"reference_images": []string{"https://example.com/r.png"}}},
	})
	if err == nil {
		t.Error("reference_images combined with a first frame should be rejected")
	}
}

func TestBytePlusVideoLifecycle(t *testing.T) {
	const taskID = "cgt-20250331175019-68d9t"
	// Fork: the download goes through the SSRF-checked media fetcher, which refuses the loopback
	// mock, so the file is served for a public address by a forward proxy.
	const videoURL = "http://203.0.113.10/files/v.mp4"
	videoProxy := proxytest.ForwardProxy(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "203.0.113.10" || r.URL.Path != "/files/v.mp4" {
			http.NotFound(w, r)
			return
		}
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("the download sent credentials to the file host: %q", auth)
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("mp4data"))
	})
	provider, captured, _ := newMockBytePlus(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/v3/contents/generations/tasks": jsonResponse(http.StatusOK, `{"id":"`+taskID+`"}`),
		"GET /api/v3/contents/generations/tasks/" + taskID: func(w http.ResponseWriter, r *http.Request) {
			jsonResponse(http.StatusOK, `{"id":"`+taskID+`","model":"seedance-1-0-pro-fast-251015","status":"succeeded","content":{"video_url":"`+videoURL+`"},"usage":{"completion_tokens":108900,"total_tokens":108900},"created_at":100,"updated_at":200,"resolution":"720p","ratio":"16:9","duration":5}`)(w, r)
		},
		"GET /api/v3/contents/generations/tasks":              jsonResponse(http.StatusOK, `{"items":[{"id":"`+taskID+`","model":"seedance-1-0-pro-fast-251015","status":"running","created_at":100},{"id":"cgt-2","model":"seedance-1-0-pro-fast-251015","status":"failed","error":{"code":"InputTextSensitiveContentDetected","message":"blocked"},"created_at":90}],"total":5}`),
		"DELETE /api/v3/contents/generations/tasks/" + taskID: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
	})
	ctx := testContext(t)
	ctx.SetValue(schemas.BifrostContextKeyProviderProxyConfig, &schemas.ProxyConfig{Type: schemas.HTTPProxy, URL: schemas.NewSecretVar(videoProxy.URL)})

	created, bifrostErr := provider.VideoGeneration(ctx, testKey(), &schemas.BifrostVideoGenerationRequest{
		Provider: schemas.BytePlus,
		Model:    "seedance-1-0-pro-fast-251015",
		Input:    &schemas.VideoGenerationInput{Prompt: "a dog"},
		Params:   &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("5")},
	})
	if bifrostErr != nil {
		t.Fatalf("VideoGeneration error: %+v", bifrostErr.Error)
	}
	if created.Status != schemas.VideoStatusQueued || !strings.HasPrefix(created.ID, taskID) || created.ID == taskID {
		t.Errorf("unexpected create response: id=%s status=%s", created.ID, created.Status)
	}
	if created.Seconds == nil || *created.Seconds != "5" {
		t.Errorf("seconds = %v, want 5", created.Seconds)
	}

	retrieved, bifrostErr := provider.VideoRetrieve(ctx, testKey(), &schemas.BifrostVideoRetrieveRequest{Provider: schemas.BytePlus, ID: created.ID})
	if bifrostErr != nil {
		t.Fatalf("VideoRetrieve error: %+v", bifrostErr.Error)
	}
	if retrieved.Status != schemas.VideoStatusCompleted || retrieved.ID != created.ID || retrieved.CompletedAt == nil || *retrieved.CompletedAt != 200 {
		t.Errorf("unexpected retrieve response: %+v", retrieved)
	}
	if len(retrieved.Videos) != 1 || retrieved.Videos[0].URL == nil || !strings.HasSuffix(*retrieved.Videos[0].URL, "/files/v.mp4") {
		t.Errorf("unexpected videos: %+v", retrieved.Videos)
	}
	// Fork: the settled size is "WxH" (720p at 16:9), not Ark's tier label.
	if retrieved.Size != "1280x720" {
		t.Errorf("size = %q, want 1280x720", retrieved.Size)
	}

	downloaded, bifrostErr := provider.VideoDownload(ctx, testKey(), &schemas.BifrostVideoDownloadRequest{Provider: schemas.BytePlus, ID: created.ID})
	if bifrostErr != nil {
		t.Fatalf("VideoDownload error: %+v", bifrostErr.Error)
	}
	if string(downloaded.Content) != "mp4data" || downloaded.ContentType != "video/mp4" {
		t.Errorf("unexpected download: %q %s", downloaded.Content, downloaded.ContentType)
	}

	listed, bifrostErr := provider.VideoList(ctx, testKey(), &schemas.BifrostVideoListRequest{Provider: schemas.BytePlus, Limit: schemas.Ptr(2)})
	if bifrostErr != nil {
		t.Fatalf("VideoList error: %+v", bifrostErr.Error)
	}
	if len(listed.Data) != 2 || listed.Data[0].Status != schemas.VideoStatusInProgress || listed.Data[1].Status != schemas.VideoStatusFailed {
		t.Errorf("unexpected list data: %+v", listed.Data)
	}
	if listed.Data[1].Error == nil || listed.Data[1].Error.Code != "InputTextSensitiveContentDetected" {
		t.Errorf("failed task error not mapped: %+v", listed.Data[1].Error)
	}
	if listed.HasMore == nil || *listed.HasMore || listed.Data[0].ID == taskID {
		t.Errorf("has_more must be false because no after cursor is accepted, and id must carry the suffix: has_more=%v id=%s", listed.HasMore, listed.Data[0].ID)
	}

	deleted, bifrostErr := provider.VideoDelete(ctx, testKey(), &schemas.BifrostVideoDeleteRequest{Provider: schemas.BytePlus, ID: created.ID})
	if bifrostErr != nil {
		t.Fatalf("VideoDelete error: %+v", bifrostErr.Error)
	}
	if !deleted.Deleted || deleted.ID != created.ID {
		t.Errorf("unexpected delete response: %+v", deleted)
	}

	for _, r := range captured() {
		if r.method == http.MethodGet && r.path == "/api/v3/contents/generations/tasks" && r.query != "page_num=1&page_size=2" {
			t.Errorf("list query = %q, want page_num=1&page_size=2", r.query)
		}
		if r.method == http.MethodPost && gjson.GetBytes(r.body, "content.0.text").String() != "a dog" {
			t.Errorf("content[0].text not sent: %s", r.body)
		}
	}
}

func TestBytePlusVideoListRejectsAfterCursor(t *testing.T) {
	provider, captured, _ := newMockBytePlus(t, nil)

	_, bifrostErr := provider.VideoList(testContext(t), testKey(), &schemas.BifrostVideoListRequest{Provider: schemas.BytePlus, After: schemas.Ptr("cgt-1")})
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected a 400 for an after cursor, got %+v", bifrostErr)
	}
	if len(captured()) != 0 {
		t.Errorf("rejected list should not call upstream")
	}
}

func TestToBifrostVideoGenerationResponseTerminalStatuses(t *testing.T) {
	for _, status := range []byteplus.BytePlusVideoTaskStatus{byteplus.BytePlusVideoTaskStatusFailed, byteplus.BytePlusVideoTaskStatusCancelled, byteplus.BytePlusVideoTaskStatusExpired} {
		resp, bifrostErr := byteplus.ToBifrostVideoGenerationResponse(&byteplus.BytePlusVideoTask{ID: "t", Status: status})
		if bifrostErr != nil {
			t.Fatalf("unexpected error: %+v", bifrostErr)
		}
		if resp.Status != schemas.VideoStatusFailed || resp.Error == nil || resp.Error.Code != string(status) {
			t.Errorf("%s: status=%s error=%+v", status, resp.Status, resp.Error)
		}
	}
}

func passthroughPostHook(_ *schemas.BifrostContext, resp *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
	return resp, err
}

func sseResponse(events ...string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, event := range events {
			_, _ = w.Write([]byte("data: " + event + "\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}
}

func drainStream(t *testing.T, stream chan *schemas.BifrostStreamChunk) []*schemas.BifrostStreamChunk {
	t.Helper()
	var chunks []*schemas.BifrostStreamChunk
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case chunk, ok := <-stream:
			if !ok {
				return chunks
			}
			if chunk != nil {
				chunks = append(chunks, chunk)
			}
		case <-timeout.C:
			t.Fatal("stream did not close")
		}
	}
}

const (
	seedreamStreamImage0 = `{"type":"image_generation.partial_succeeded","model":"seedream-4-5-251128","created":1757396757,"image_index":0,"url":"https://cdn.example/0.jpeg","size":"2048x2048"}`
	seedreamStreamFailed = `{"type":"image_generation.partial_failed","model":"seedream-4-5-251128","created":1757396757,"image_index":1,"error":{"code":"OutputImageSensitiveContentDetected","message":"blocked"}}`
	seedreamStreamImage2 = `{"type":"image_generation.partial_succeeded","model":"seedream-4-5-251128","created":1757396758,"image_index":2,"url":"https://cdn.example/2.jpeg","size":"2048x2048"}`
	seedreamStreamDone   = `{"type":"image_generation.completed","model":"seedream-4-5-251128","created":1757396759,"usage":{"generated_images":2,"output_tokens":32768,"total_tokens":32768}}`
)

// Seedream streams each finished image as its own event and ends with a usage-only completed event.
func TestBytePlusImageGenerationStream(t *testing.T) {
	provider, captured, _ := newMockBytePlus(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/v3/images/generations": sseResponse(seedreamStreamImage0, seedreamStreamFailed, seedreamStreamImage2, seedreamStreamDone),
	})

	stream, bifrostErr := provider.ImageGenerationStream(testContext(t), passthroughPostHook, nil, testKey(), &schemas.BifrostImageGenerationRequest{
		Provider: schemas.BytePlus,
		Model:    "seedream-4-5-251128",
		Input:    &schemas.ImageGenerationInput{Prompt: "four seasons"},
		Params:   &schemas.ImageGenerationParameters{N: schemas.Ptr(3)},
	})
	if bifrostErr != nil {
		t.Fatalf("ImageGenerationStream error: %+v", bifrostErr.Error)
	}
	chunks := drainStream(t, stream)

	body := captured()[0].body
	if !gjson.GetBytes(body, "stream").Bool() || gjson.GetBytes(body, "sequential_image_generation").String() != "auto" {
		t.Errorf("stream and sequential generation should be sent, got %s", body)
	}
	if len(chunks) != 3 {
		t.Fatalf("want 2 image chunks and 1 completed chunk, got %d", len(chunks))
	}
	for i, want := range []struct {
		index int
		url   string
	}{{0, "https://cdn.example/0.jpeg"}, {2, "https://cdn.example/2.jpeg"}} {
		chunk := chunks[i].BifrostImageGenerationStreamResponse
		if chunk == nil || chunk.Type != schemas.ImageGenerationEventTypePartial || chunk.Index != want.index || chunk.URL != want.url || chunk.Size != "2048x2048" {
			t.Errorf("chunk %d = %+v, want partial image %d at %s", i, chunk, want.index, want.url)
		}
	}
	final := chunks[2].BifrostImageGenerationStreamResponse
	if final == nil || final.Type != schemas.ImageGenerationEventTypeCompleted {
		t.Fatalf("last chunk should be completed, got %+v", chunks[2])
	}
	if final.Usage == nil || final.Usage.OutputTokens != 32768 || final.Usage.OutputTokensDetails == nil || final.Usage.OutputTokensDetails.NImages != 2 {
		t.Errorf("completed usage not mapped: %+v", final.Usage)
	}
	if final.URL != "https://cdn.example/2.jpeg" {
		t.Errorf("completed chunk should carry the last image, got %q", final.URL)
	}
}

func TestBytePlusImageEditStreamUsesEditEvents(t *testing.T) {
	provider, captured, _ := newMockBytePlus(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/v3/images/generations": sseResponse(seedreamStreamImage0, seedreamStreamDone),
	})

	stream, bifrostErr := provider.ImageEditStream(testContext(t), passthroughPostHook, nil, testKey(), &schemas.BifrostImageEditRequest{
		Provider: schemas.BytePlus,
		Model:    "seedream-4-5-251128",
		Input:    &schemas.ImageEditInput{Prompt: "make it blue", Images: []schemas.ImageInput{{URL: "https://example.com/a.png"}}},
	})
	if bifrostErr != nil {
		t.Fatalf("ImageEditStream error: %+v", bifrostErr.Error)
	}
	chunks := drainStream(t, stream)

	if body := captured()[0].body; !gjson.GetBytes(body, "stream").Bool() || gjson.GetBytes(body, "image.0").String() != "https://example.com/a.png" {
		t.Errorf("edit stream body = %s", body)
	}
	if len(chunks) != 2 || chunks[0].BifrostImageGenerationStreamResponse == nil || chunks[0].BifrostImageGenerationStreamResponse.Type != schemas.ImageEditEventTypePartial ||
		chunks[1].BifrostImageGenerationStreamResponse == nil || chunks[1].BifrostImageGenerationStreamResponse.Type != schemas.ImageEditEventTypeCompleted {
		t.Fatalf("edit stream should use image_edit events, got %+v", chunks)
	}
}

// Ark stops the stream after an internal error, so it ends the Bifrost stream too. A moderation miss on one image does not.
func TestBytePlusImageGenerationStreamErrors(t *testing.T) {
	cases := []struct {
		name     string
		events   []string
		wantCode string
		wantType string
	}{
		{"internal error ends the stream", []string{seedreamStreamImage0, `{"type":"image_generation.partial_failed","model":"seedream-4-5-251128","created":1,"image_index":1,"error":{"code":"InternalServiceError","message":"internal"}}`}, "InternalServiceError", "server_error"},
		{"every image failed", []string{seedreamStreamFailed, `{"type":"image_generation.completed","model":"seedream-4-5-251128","created":1,"usage":{"generated_images":0,"output_tokens":0,"total_tokens":0}}`}, "OutputImageSensitiveContentDetected", "content_filter"},
		{"request-level error event", []string{`{"error":{"code":"BadRequest","message":"missing required parameter"}}`}, "BadRequest", "invalid_request_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider, _, _ := newMockBytePlus(t, map[string]func(http.ResponseWriter, *http.Request){
				"POST /api/v3/images/generations": sseResponse(tc.events...),
			})
			stream, bifrostErr := provider.ImageGenerationStream(testContext(t), passthroughPostHook, nil, testKey(), &schemas.BifrostImageGenerationRequest{
				Provider: schemas.BytePlus,
				Model:    "seedream-4-5-251128",
				Input:    &schemas.ImageGenerationInput{Prompt: "a cat"},
			})
			if bifrostErr != nil {
				t.Fatalf("ImageGenerationStream error: %+v", bifrostErr.Error)
			}
			chunks := drainStream(t, stream)
			if len(chunks) == 0 {
				t.Fatal("expected an error chunk")
			}
			last := chunks[len(chunks)-1].BifrostError
			if last == nil || last.Error == nil || last.Error.Code == nil || *last.Error.Code != tc.wantCode {
				t.Fatalf("last chunk should be an error with code %s, got %+v", tc.wantCode, chunks[len(chunks)-1])
			}
			if last.Error.Type == nil || *last.Error.Type != tc.wantType {
				t.Errorf("error type = %v, want %s", last.Error.Type, tc.wantType)
			}
		})
	}
}

// Seedream-only fields Bifrost has no typed slot for must reach Ark without the passthrough header.
func TestBytePlusImageExtraParamsReachArk(t *testing.T) {
	provider, captured, _ := newMockBytePlus(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/v3/images/generations": jsonResponse(http.StatusOK, `{"model":"dola-seedream-5-0-pro-260628","created":1,"data":[{"url":"https://cdn.example/1.png"}]}`),
	})
	if _, bifrostErr := provider.ImageGeneration(testContext(t), testKey(), &schemas.BifrostImageGenerationRequest{
		Provider: schemas.BytePlus,
		Model:    "dola-seedream-5-0-pro-260628",
		Input:    &schemas.ImageGenerationInput{Prompt: "a logo"},
		Params:   &schemas.ImageGenerationParameters{ExtraParams: map[string]interface{}{"background": "transparent", "optimize_prompt_options": map[string]interface{}{"mode": "fast"}}},
	}); bifrostErr != nil {
		t.Fatalf("ImageGeneration error: %+v", bifrostErr.Error)
	}
	body := captured()[0].body
	if gjson.GetBytes(body, "background").String() != "transparent" || gjson.GetBytes(body, "optimize_prompt_options.mode").String() != "fast" {
		t.Errorf("Seedream extra params should reach Ark, got %s", body)
	}
}

func TestToBytePlusVideoGenerationRequestOmniReferences(t *testing.T) {
	req, err := byteplus.ToBytePlusVideoGenerationRequest(&schemas.BifrostVideoGenerationRequest{
		Model: "dreamina-seedance-2-5-260628",
		Input: &schemas.VideoGenerationInput{Prompt: "dance to the beat", VideoURI: schemas.Ptr("https://example.com/a.mp4")},
		Params: &schemas.VideoGenerationParameters{
			OutputFormat: schemas.Ptr("mov"),
			ExtraParams: map[string]any{
				"reference_videos": []string{"https://example.com/b.mp4", "asset://asset-123"},
				"reference_audios": []string{"https://example.com/beat.mp3"},
				"reference_images": []string{"asset://asset-456"},
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var videos, audios, images []string
	for _, item := range req.Content {
		switch item.Role {
		case "reference_video":
			videos = append(videos, item.VideoURL.URL)
		case "reference_audio":
			if item.Type != "audio_url" || item.AudioURL == nil {
				t.Errorf("reference audio should be an audio_url item: %+v", item)
				continue
			}
			audios = append(audios, item.AudioURL.URL)
		case "reference_image":
			images = append(images, item.ImageURL.URL)
		}
	}
	if strings.Join(videos, ",") != "https://example.com/a.mp4,https://example.com/b.mp4,asset://asset-123" {
		t.Errorf("reference videos = %v", videos)
	}
	if strings.Join(audios, ",") != "https://example.com/beat.mp3" || strings.Join(images, ",") != "asset://asset-456" {
		t.Errorf("reference audios = %v, images = %v", audios, images)
	}
	if req.OutputFormat == nil || *req.OutputFormat != "mov" {
		t.Errorf("output_format not mapped: %v", req.OutputFormat)
	}
	for _, key := range []string{"reference_videos", "reference_audios", "reference_images"} {
		if _, ok := req.ExtraParams[key]; ok {
			t.Errorf("%s should be lifted into content, not passed through", key)
		}
	}
}

// A draft is turned into a final video by sending its task id, as Bifrost returned it; the prompt is optional then.
func TestToBytePlusVideoGenerationRequestDraftTask(t *testing.T) {
	req, err := byteplus.ToBytePlusVideoGenerationRequest(&schemas.BifrostVideoGenerationRequest{
		Model:  "dreamina-seedance-2-5-260628",
		Input:  &schemas.VideoGenerationInput{},
		Params: &schemas.VideoGenerationParameters{ExtraParams: map[string]any{"draft_task_id": "cgt-draft-1:byteplus"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(req.Content) != 1 || req.Content[0].Type != "draft_task" || req.Content[0].DraftTask == nil || req.Content[0].DraftTask.ID != "cgt-draft-1" {
		t.Errorf("draft task content = %+v", req.Content)
	}

	if _, err := byteplus.ToBytePlusVideoGenerationRequest(&schemas.BifrostVideoGenerationRequest{Model: "m", Input: &schemas.VideoGenerationInput{}}); err == nil {
		t.Error("a request with neither a prompt nor a draft task should be rejected")
	}
}

// Seedance task options with no Bifrost field must reach Ark, and a requested last frame must come back.
func TestBytePlusVideoExtraParamsAndLastFrame(t *testing.T) {
	const taskID = "cgt-lastframe"
	provider, captured, _ := newMockBytePlus(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/v3/contents/generations/tasks":          jsonResponse(http.StatusOK, `{"id":"`+taskID+`"}`),
		"GET /api/v3/contents/generations/tasks/" + taskID: jsonResponse(http.StatusOK, `{"id":"`+taskID+`","model":"dreamina-seedance-2-5-260628","status":"succeeded","content":{"video_url":"https://cdn.example/v.mov","last_frame_url":"https://cdn.example/last.jpeg"},"created_at":100,"updated_at":200,"resolution":"720p","duration":5}`),
	})
	ctx := testContext(t)

	created, bifrostErr := provider.VideoGeneration(ctx, testKey(), &schemas.BifrostVideoGenerationRequest{
		Provider: schemas.BytePlus,
		Model:    "dreamina-seedance-2-5-260628",
		Input:    &schemas.VideoGenerationInput{Prompt: "a dog"},
		Params: &schemas.VideoGenerationParameters{ExtraParams: map[string]any{
			"return_last_frame": true, "service_tier": "flex", "draft": true, "priority": 5, "omni_reference_task_type": "extend",
		}},
	})
	if bifrostErr != nil {
		t.Fatalf("VideoGeneration error: %+v", bifrostErr.Error)
	}
	body := captured()[0].body
	for _, field := range []string{"return_last_frame", "service_tier", "draft", "priority", "omni_reference_task_type"} {
		if !gjson.GetBytes(body, field).Exists() {
			t.Errorf("%s should reach Ark, got %s", field, body)
		}
	}

	retrieved, bifrostErr := provider.VideoRetrieve(ctx, testKey(), &schemas.BifrostVideoRetrieveRequest{Provider: schemas.BytePlus, ID: created.ID})
	if bifrostErr != nil {
		t.Fatalf("VideoRetrieve error: %+v", bifrostErr.Error)
	}
	if len(retrieved.Videos) != 2 {
		t.Fatalf("want the video and its last frame, got %+v", retrieved.Videos)
	}
	if retrieved.Videos[0].ContentType != "video/quicktime" || *retrieved.Videos[0].URL != "https://cdn.example/v.mov" {
		t.Errorf("video output = %+v", retrieved.Videos[0])
	}
	if retrieved.Videos[1].ContentType != "image/jpeg" || *retrieved.Videos[1].URL != "https://cdn.example/last.jpeg" {
		t.Errorf("last frame output = %+v", retrieved.Videos[1])
	}
}

func BenchmarkToBytePlusVideoGenerationRequest(b *testing.B) {
	req := &schemas.BifrostVideoGenerationRequest{
		Model:  "dreamina-seedance-2-5-260628",
		Input:  &schemas.VideoGenerationInput{Prompt: "a dog", InputReference: schemas.Ptr("https://example.com/first.png")},
		Params: &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("5"), Size: "1280x720"},
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := byteplus.ToBytePlusVideoGenerationRequest(req); err != nil {
			b.Fatal(err)
		}
	}
}

// BytePlus is not a base for custom providers (core refuses base_provider_type byteplus), so it has
// no custom-provider name to carry in video IDs and no allowed_requests to enforce. Adding it to
// SupportedBaseProviders needs both wired into the provider first, as the fork did for MiniMax.
func TestBytePlusIsNotACustomProviderBase(t *testing.T) {
	if bifrost.IsSupportedBaseProvider(schemas.BytePlus) {
		t.Fatal("byteplus became a custom-provider base: the provider must report the custom name and enforce allowed_requests first")
	}
	provider, _, _ := newMockBytePlus(t, nil)
	if got := provider.GetProviderKey(); got != schemas.BytePlus {
		t.Errorf("provider key = %q, want byteplus", got)
	}
}

// The settled size must be "WxH": video pricing bands read the short edge of that string, so Ark's
// tier label ("720p") selects no band and, being non-empty, would replace the size captured from
// the request. An unknown tier or ratio reports no size, so the captured one is kept.
func TestBytePlusVideoSizeIsWidthByHeight(t *testing.T) {
	cases := []struct{ resolution, ratio, want string }{
		{"720p", "16:9", "1280x720"},
		{"480p", "9:16", "480x853"},
		{"1080p", "1:1", "1080x1080"},
		{"1080p", "21:9", "2520x1080"},
		{"480P", " 4:3 ", "640x480"},
		{"720p", "3:4", "720x960"},
		{"2k", "16:9", "2560x1440"},
		{"720p", "adaptive", ""},
		{"720p", "", ""},
		{"", "16:9", ""},
		{"hd", "16:9", ""},
		{"720p", "0:9", ""},
	}
	for _, tc := range cases {
		task := &byteplus.BytePlusVideoTask{ID: "cgt-1", Status: byteplus.BytePlusVideoTaskStatusSucceeded, Resolution: tc.resolution, Ratio: tc.ratio, Duration: 5}
		resp, bifrostErr := byteplus.ToBifrostVideoGenerationResponse(task)
		if bifrostErr != nil {
			t.Fatalf("%s %s: %+v", tc.resolution, tc.ratio, bifrostErr.Error)
		}
		if resp.Size != tc.want {
			t.Errorf("retrieve: resolution %q ratio %q: size = %q, want %q", tc.resolution, tc.ratio, resp.Size, tc.want)
		}
		if obj := byteplus.ToBifrostVideoObject(task, schemas.BytePlus); obj.Size != tc.want {
			t.Errorf("list: resolution %q ratio %q: size = %q, want %q", tc.resolution, tc.ratio, obj.Size, tc.want)
		}
	}
}

// The download URL comes from Ark's task response, so a task pointing at a loopback or private
// address must be refused without that address ever being dialed, and the error must not carry
// the URL's signed query.
func TestBytePlusVideoDownloadRefusesNonPublicURL(t *testing.T) {
	var internalHits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		internalHits.Add(1)
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("internal secret"))
	}))
	t.Cleanup(internal.Close)
	provider, _, _ := newMockBytePlus(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api/v3/contents/generations/tasks/cgt-ssrf": jsonResponse(http.StatusOK, `{"id":"cgt-ssrf","model":"seedance-1-0-pro-fast-251015","status":"succeeded","content":{"video_url":"`+internal.URL+`/v.mp4?X-Tos-Signature=abc123"},"resolution":"720p","ratio":"16:9","duration":5}`),
	})

	_, bifrostErr := provider.VideoDownload(testContext(t), testKey(), &schemas.BifrostVideoDownloadRequest{Provider: schemas.BytePlus, ID: "cgt-ssrf"})
	if got := internalHits.Load(); got != 0 {
		t.Fatalf("download dialed the loopback URL %d times", got)
	}
	if bifrostErr == nil {
		t.Fatal("expected the loopback download URL to be refused")
	}
	msg := bifrostErr.Error.Message
	if bifrostErr.Error.Error != nil {
		msg += " " + bifrostErr.Error.Error.Error()
	}
	if strings.Contains(msg, "abc123") {
		t.Errorf("error leaks the signed query: %s", msg)
	}
}

// newProxiedBytePlus builds a provider whose every request, to Ark and to the file host a task
// names, leaves through one forward proxy that serves handler. The context carries the same proxy,
// as bifrost sets it on each attempt, so the media fetcher uses it too.
func newProxiedBytePlus(t *testing.T, handler http.HandlerFunc) (*byteplus.BytePlusProvider, *schemas.BifrostContext) {
	t.Helper()
	proxy := proxytest.ForwardProxy(t, handler)
	proxyConfig := &schemas.ProxyConfig{Type: schemas.HTTPProxy, URL: schemas.NewSecretVar(proxy.URL)}
	provider, err := byteplus.NewBytePlusProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: "http://203.0.113.20", DefaultRequestTimeoutInSeconds: 30},
		ProxyConfig:   proxyConfig,
	}, bifrost.NewDefaultLogger(schemas.LogLevelError))
	if err != nil {
		t.Fatalf("NewBytePlusProvider: %v", err)
	}
	ctx := streamTestContext(t)
	ctx.SetValue(schemas.BifrostContextKeyProviderProxyConfig, proxyConfig)
	return provider, ctx
}

// The downloaded bytes are served on the gateway's own origin with the type returned here, so an
// upstream that serves HTML at the task's URL must not get it rendered as a document. A video/*
// type is kept; anything else becomes the type the output format implies (mov is QuickTime).
func TestBytePlusVideoDownloadPinsVideoContentType(t *testing.T) {
	cases := []struct{ path, served, want string }{
		{"/html.mp4", "text/html; charset=utf-8", "video/mp4"},
		{"/svg.mp4", "image/svg+xml", "video/mp4"},
		{"/none.mp4", "", "video/mp4"},
		{"/odd.mp4", "video/mp4<script>", "video/mp4"},
		{"/webm.mp4", "video/webm", "video/webm"},
		{"/html.mov", "text/html", "video/quicktime"},
		{"/qt.mov", "video/quicktime", "video/quicktime"},
	}
	for _, tc := range cases {
		t.Run(strings.TrimPrefix(tc.path, "/"), func(t *testing.T) {
			provider, ctx := newProxiedBytePlus(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Host == "203.0.113.20" && r.URL.Path == "/api/v3/contents/generations/tasks/cgt-mime":
					jsonResponse(http.StatusOK, `{"id":"cgt-mime","model":"seedance-1-0-pro-fast-251015","status":"succeeded","content":{"video_url":"http://203.0.113.10`+tc.path+`"},"resolution":"720p","ratio":"16:9","duration":5}`)(w, r)
				case r.URL.Host == "203.0.113.10" && r.URL.Path == tc.path:
					if tc.served == "" {
						w.Header()["Content-Type"] = nil
					} else {
						w.Header().Set("Content-Type", tc.served)
					}
					_, _ = w.Write([]byte("<html><script>fetch('/api/governance/virtual-keys')</script></html>"))
				default:
					http.NotFound(w, r)
				}
			})
			resp, bifrostErr := provider.VideoDownload(ctx, testKey(), &schemas.BifrostVideoDownloadRequest{Provider: schemas.BytePlus, ID: "cgt-mime"})
			if bifrostErr != nil {
				t.Fatalf("VideoDownload: %+v", bifrostErr.Error)
			}
			if resp.ContentType != tc.want {
				t.Errorf("content type = %q, want %q", resp.ContentType, tc.want)
			}
			if len(resp.Content) == 0 {
				t.Error("no content")
			}
		})
	}
}

// streamCloseLimit bounds how long a test waits for a stream channel to close. A closing stream
// takes milliseconds; the limit only exists so a stream that never closes fails the test instead
// of hanging it, and is far above anything a loaded host needs.
const streamCloseLimit = 30 * time.Second

// newParkedBytePlus serves events on the image endpoint and then keeps the body open, sending SSE
// heartbeats until the client goes away or holdOpen elapses, the way an upstream can park a
// connection after its last event. With holdOpen zero the handler returns after the events, which
// ends the body and leaves the connection kept alive. handlerDone closes when the handler returns.
func newParkedBytePlus(t *testing.T, holdOpen time.Duration, events ...string) (*byteplus.BytePlusProvider, <-chan struct{}) {
	t.Helper()
	handlerDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/images/generations" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		defer close(handlerDone)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, e := range events {
			_, _ = w.Write([]byte("data: " + e + "\n\n"))
		}
		flusher.Flush()
		if holdOpen <= 0 {
			return
		}
		deadline := time.After(holdOpen)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-deadline:
				return
			case <-ticker.C:
				if _, err := w.Write([]byte(": ping\n\n")); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(server.Close)
	// The request timeout is far above every wait in these tests, and a stream has no read
	// timeout, so nothing but the stream's own handling ends it.
	provider, err := byteplus.NewBytePlusProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, DefaultRequestTimeoutInSeconds: 300, StreamIdleTimeoutInSeconds: 300, AllowPrivateNetwork: true},
	}, bifrost.NewDefaultLogger(schemas.LogLevelError))
	if err != nil {
		t.Fatalf("NewBytePlusProvider: %v", err)
	}
	return provider, handlerDone
}

// collectStreamWithin drains stream, failing the test if it does not close within limit.
func collectStreamWithin(t *testing.T, stream chan *schemas.BifrostStreamChunk, limit time.Duration) []*schemas.BifrostStreamChunk {
	t.Helper()
	var chunks []*schemas.BifrostStreamChunk
	timeout := time.NewTimer(limit)
	defer timeout.Stop()
	for {
		select {
		case chunk, ok := <-stream:
			if !ok {
				return chunks
			}
			if chunk != nil {
				chunks = append(chunks, chunk)
			}
		case <-timeout.C:
			t.Fatalf("stream did not close within %s (cleanup is draining a body the upstream holds open); got %d chunks", limit, len(chunks))
			return chunks
		}
	}
}

// waitFinalizedOnce waits for the stream finalizer, which runs in the stream goroutine's deferred
// cleanup just after the channel closes, and reports how many times it ran.
func waitFinalizedOnce(t *testing.T, finalized *atomic.Int32) {
	t.Helper()
	deadline := time.Now().Add(streamCloseLimit)
	for finalized.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := finalized.Load(); got != 1 {
		t.Errorf("finalizer ran %d times, want 1", got)
	}
}

func streamErrors(chunks []*schemas.BifrostStreamChunk) []*schemas.BifrostError {
	var errs []*schemas.BifrostError
	for _, c := range chunks {
		if c.BifrostError != nil {
			errs = append(errs, c.BifrostError)
		}
	}
	return errs
}

func imageStreamRequest() *schemas.BifrostImageGenerationRequest {
	return &schemas.BifrostImageGenerationRequest{
		Provider: schemas.BytePlus,
		Model:    "seedream-4-5-251128",
		Input:    &schemas.ImageGenerationInput{Prompt: "four seasons"},
	}
}

// streamTestContext outlives every wait in the fork's tests, so a context deadline never ends a
// stream or a download that a test expects the code under test to end.
func streamTestContext(t *testing.T) *schemas.BifrostContext {
	t.Helper()
	ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// A terminal Seedream event ends the stream even when Ark keeps the connection open: the channel
// closes, the finalizer runs once, the terminal usage or error is delivered once, and the parked
// connection is abandoned rather than drained.
func TestBytePlusImageStreamTerminalEventWithParkedBody(t *testing.T) {
	cases := []struct {
		name          string
		events        []string
		wantErrStatus int
		wantErrCode   string
		wantChunks    int
	}{
		{name: "completed", events: []string{seedreamStreamImage0, seedreamStreamDone}, wantChunks: 2},
		{
			name:          "internal error ends the stream",
			events:        []string{seedreamStreamImage0, `{"type":"image_generation.partial_failed","model":"seedream-4-5-251128","created":1,"image_index":1,"error":{"code":"InternalServiceError","message":"internal"}}`},
			wantErrStatus: http.StatusInternalServerError, wantErrCode: "InternalServiceError", wantChunks: 2,
		},
		{
			name:          "every image failed",
			events:        []string{seedreamStreamFailed, `{"type":"image_generation.completed","model":"seedream-4-5-251128","created":1,"usage":{"generated_images":0,"output_tokens":0,"total_tokens":0}}`},
			wantErrStatus: http.StatusBadRequest, wantErrCode: "OutputImageSensitiveContentDetected", wantChunks: 1,
		},
		{
			name:          "request-level error event",
			events:        []string{`{"error":{"code":"BadRequest","message":"missing required parameter"}}`},
			wantErrStatus: http.StatusBadRequest, wantErrCode: "BadRequest", wantChunks: 1,
		},
		// Ark's [DONE] without a completed event ends the stream with what was delivered and no
		// error, as upstream wrote it; the body is still open, so it must be abandoned too.
		{name: "done marker without completed", events: []string{seedreamStreamImage0, "[DONE]"}, wantChunks: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			provider, handlerDone := newParkedBytePlus(t, 3*time.Minute, tc.events...)
			var finalized atomic.Int32
			stream, bifrostErr := provider.ImageGenerationStream(streamTestContext(t), passthroughPostHook, func(context.Context) { finalized.Add(1) }, testKey(), imageStreamRequest())
			if bifrostErr != nil {
				t.Fatalf("stream setup error: %+v", bifrostErr.Error)
			}
			chunks := collectStreamWithin(t, stream, streamCloseLimit)
			if len(chunks) != tc.wantChunks {
				t.Fatalf("got %d chunks, want %d: %+v", len(chunks), tc.wantChunks, chunks)
			}
			errs := streamErrors(chunks)
			switch {
			case tc.wantErrStatus != 0:
				if len(errs) != 1 {
					t.Fatalf("got %d error chunks, want exactly 1", len(errs))
				}
				if errs[0].StatusCode == nil || *errs[0].StatusCode != tc.wantErrStatus {
					t.Errorf("error status = %v, want %d (%+v)", errs[0].StatusCode, tc.wantErrStatus, errs[0].Error)
				}
				if errs[0].Error.Code == nil || *errs[0].Error.Code != tc.wantErrCode {
					t.Errorf("error code = %v, want %s", errs[0].Error.Code, tc.wantErrCode)
				}
			case len(errs) != 0:
				t.Fatalf("unexpected error chunk (a 499 or truncation would show here): %+v", errs[0].Error)
			}
			if tc.name == "completed" {
				final := chunks[len(chunks)-1].BifrostImageGenerationStreamResponse
				if final == nil || final.Type != schemas.ImageGenerationEventTypeCompleted || final.Usage == nil || final.Usage.OutputTokens != 32768 {
					t.Errorf("final chunk is not a completed event with its usage: %+v", chunks[len(chunks)-1])
				}
			}
			waitFinalizedOnce(t, &finalized)
			select {
			case <-handlerDone:
			case <-time.After(streamCloseLimit):
				t.Errorf("upstream handler still running %s after the stream closed: the parked connection was not abandoned", streamCloseLimit)
			}
		})
	}
}

// A body that ends before Ark's completed event is a truncated stream: one error after the image
// already delivered, on a connection the server keeps alive.
func TestBytePlusImageStreamTruncatedBody(t *testing.T) {
	provider, handlerDone := newParkedBytePlus(t, 0, seedreamStreamImage0)
	var finalized atomic.Int32
	stream, bifrostErr := provider.ImageGenerationStream(streamTestContext(t), passthroughPostHook, func(context.Context) { finalized.Add(1) }, testKey(), imageStreamRequest())
	if bifrostErr != nil {
		t.Fatalf("stream setup error: %+v", bifrostErr.Error)
	}
	chunks := collectStreamWithin(t, stream, streamCloseLimit)
	if len(chunks) != 2 || chunks[0].BifrostImageGenerationStreamResponse == nil {
		t.Fatalf("want the delivered image and one error, got %+v", chunks)
	}
	if errs := streamErrors(chunks); len(errs) != 1 || chunks[1].BifrostError == nil {
		t.Fatalf("want exactly one error chunk, last: %+v", chunks)
	}
	waitFinalizedOnce(t, &finalized)
	<-handlerDone
}

// deadlineOnDemand is a parent context whose deadline passes when the test closes done, so the
// deadline case does not depend on how long a loaded host takes to deliver the first event.
type deadlineOnDemand struct {
	context.Context
	done chan struct{}
}

func (d *deadlineOnDemand) Done() <-chan struct{} { return d.done }

func (d *deadlineOnDemand) Err() error {
	select {
	case <-d.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// A client that goes away, or a request deadline that passes, while Ark is still generating ends
// the stream with exactly one error of that kind through the post-hook, which is what logging and
// billing see. Delivery of that last chunk is best effort once the context is done (the shared
// send path selects against it), so the channel may carry it or not, but never twice. The
// provider's own timeouts are minutes long here, so only the context can be what ended it.
func TestBytePlusImageStreamContextEnds(t *testing.T) {
	cases := []struct {
		name       string
		newContext func(t *testing.T) (*schemas.BifrostContext, func())
		wantStatus int
		wantType   string
	}{
		{
			name: "cancelled",
			newContext: func(t *testing.T) (*schemas.BifrostContext, func()) {
				ctx, cancel := schemas.NewBifrostContextWithCancel(context.Background())
				t.Cleanup(cancel)
				return ctx, cancel
			},
			wantStatus: 499, wantType: schemas.RequestCancelled,
		},
		{
			name: "deadline",
			newContext: func(t *testing.T) (*schemas.BifrostContext, func()) {
				parent := &deadlineOnDemand{Context: context.Background(), done: make(chan struct{})}
				ctx, cancel := schemas.NewBifrostContextWithCancel(parent)
				t.Cleanup(cancel)
				return ctx, func() { close(parent.done) }
			},
			wantStatus: http.StatusGatewayTimeout, wantType: schemas.RequestTimedOut,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			provider, handlerDone := newParkedBytePlus(t, 3*time.Minute, seedreamStreamImage0)
			ctx, end := tc.newContext(t)
			var finalized atomic.Int32
			var mu sync.Mutex
			var hookErrs []*schemas.BifrostError
			postHook := func(_ *schemas.BifrostContext, resp *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
				if err != nil {
					mu.Lock()
					hookErrs = append(hookErrs, err)
					mu.Unlock()
				}
				return resp, err
			}
			stream, bifrostErr := provider.ImageGenerationStream(ctx, postHook, func(context.Context) { finalized.Add(1) }, testKey(), imageStreamRequest())
			if bifrostErr != nil {
				t.Fatalf("stream setup error: %+v", bifrostErr.Error)
			}
			var first *schemas.BifrostStreamChunk
			select {
			case first = <-stream:
			case <-time.After(streamCloseLimit):
				t.Fatal("no first chunk")
			}
			if first == nil || first.BifrostImageGenerationStreamResponse == nil {
				t.Fatalf("first chunk should be the delivered image, got %+v", first)
			}
			end()
			chunks := collectStreamWithin(t, stream, streamCloseLimit)
			if delivered := streamErrors(chunks); len(chunks) > 1 || len(delivered) != len(chunks) {
				t.Fatalf("want at most one error chunk after the image, got %+v", chunks)
			}
			waitFinalizedOnce(t, &finalized)
			mu.Lock()
			defer mu.Unlock()
			if len(hookErrs) != 1 {
				t.Fatalf("post-hook saw %d errors, want exactly 1: %+v", len(hookErrs), hookErrs)
			}
			if got := hookErrs[0]; got.StatusCode == nil || *got.StatusCode != tc.wantStatus || got.Error.Type == nil || *got.Error.Type != tc.wantType {
				t.Errorf("error = status %v type %v, want %d %s", got.StatusCode, got.Error.Type, tc.wantStatus, tc.wantType)
			}
			select {
			case <-handlerDone:
			case <-time.After(streamCloseLimit):
				t.Errorf("upstream handler still running %s after the stream ended: the connection was not closed", streamCloseLimit)
			}
		})
	}
}

// One failed call fails the whole embedding request, so no further calls are started: only the
// calls already in flight (the concurrency window) reach Ark, not one per remaining input.
func TestBytePlusEmbeddingStopsFanOutAfterAFailure(t *testing.T) {
	const inputs = 40
	const window = 8 // bytePlusEmbeddingConcurrency
	var calls atomic.Int32
	provider, _, _ := newMockBytePlus(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/v3/embeddings/multimodal": func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			jsonResponse(http.StatusTooManyRequests, `{"error":{"code":"RateLimitExceeded.EndpointRPMExceeded","message":"rate limited","type":"TooManyRequests"}}`)(w, r)
		},
	})
	items := make([]schemas.EmbeddingInputItem, inputs)
	for i := range items {
		items[i] = schemas.EmbeddingInputItem{Content: []schemas.EmbeddingContentPart{{Type: schemas.EmbeddingContentPartTypeText, Text: schemas.Ptr("text")}}}
	}
	_, bifrostErr := provider.Embedding(testContext(t), testKey(), &schemas.BifrostEmbeddingRequest{
		Provider: schemas.BytePlus,
		Model:    "skylark-embedding-vision-251215",
		Input:    items,
	})
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("want the upstream 429, got %+v", bifrostErr)
	}
	if got := calls.Load(); got > window {
		t.Errorf("%d of %d inputs reached Ark after the first call failed, want at most the %d already in flight", got, inputs, window)
	}
}

// benchBytePlus starts a local Ark stand-in and a provider pointed at it.
func benchBytePlus(b *testing.B, handler http.HandlerFunc) *byteplus.BytePlusProvider {
	b.Helper()
	server := httptest.NewServer(handler)
	b.Cleanup(server.Close)
	provider, err := byteplus.NewBytePlusProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, DefaultRequestTimeoutInSeconds: 30, AllowPrivateNetwork: true},
	}, bifrost.NewDefaultLogger(schemas.LogLevelError))
	if err != nil {
		b.Fatalf("NewBytePlusProvider: %v", err)
	}
	return provider
}

// BenchmarkBytePlusChatCompletion is the provider's chat round trip against a local server: the
// OpenAI converter with the BytePlus compatibility pass, the request and the response decode.
// The provider is new, so there is no earlier implementation to compare against.
func BenchmarkBytePlusChatCompletion(b *testing.B) {
	provider := benchBytePlus(b, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		jsonResponse(http.StatusOK, chatSuccessBody)(w, r)
	})
	key := testKey()
	req := userChatRequest(nil)
	b.ReportAllocs()
	for b.Loop() {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		if _, bifrostErr := provider.ChatCompletion(ctx, key, req); bifrostErr != nil {
			b.Fatalf("ChatCompletion: %+v", bifrostErr.Error)
		}
		ctx.Cancel()
	}
}

// imageStreamBench holds the one measurement BenchmarkBytePlusImageGenerationStream takes per process.
var imageStreamBench struct {
	once                             sync.Once
	nsPerOp, bytesPerOp, allocsPerOp float64
	err                              string
}

// BenchmarkBytePlusImageGenerationStream is one streamed Seedream generation (two images, one
// refused, the completed event) against a local server, from the request to the closed channel.
//
// It measures a fixed batch of streams once per process and reports the per-stream figures under
// the standard units, whatever b.N and -count are. A stream ends on its completed event, where
// the fork abandons the connection instead of draining it, so every stream opens a connection,
// and a closed loopback connection keeps its ephemeral port in TIME_WAIT for tens of seconds: a
// loop of b.N streams exhausts the host's ports within seconds ("can't assign requested
// address"), for every process on it. The figures include that connection and, as in the other
// round-trip benchmarks here, the in-process test server.
func BenchmarkBytePlusImageGenerationStream(b *testing.B) {
	const streams = 300
	imageStreamBench.once.Do(func() {
		provider := benchBytePlus(b, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			sseResponse(seedreamStreamImage0, seedreamStreamFailed, seedreamStreamImage2, seedreamStreamDone)(w, r)
		})
		key := testKey()
		req := imageStreamRequest()
		runOne := func() string {
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			defer ctx.Cancel()
			stream, bifrostErr := provider.ImageGenerationStream(ctx, passthroughPostHook, nil, key, req)
			if bifrostErr != nil {
				return "ImageGenerationStream: " + bifrostErr.Error.Message
			}
			chunks := 0
			for range stream {
				chunks++
			}
			if chunks != 3 {
				return "got " + strconv.Itoa(chunks) + " chunks, want 3"
			}
			return ""
		}
		if imageStreamBench.err = runOne(); imageStreamBench.err != "" { // warm up outside the measurement
			return
		}
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		start := time.Now()
		for range streams {
			if imageStreamBench.err = runOne(); imageStreamBench.err != "" {
				return
			}
		}
		elapsed := time.Since(start)
		runtime.ReadMemStats(&after)
		imageStreamBench.nsPerOp = float64(elapsed.Nanoseconds()) / streams
		imageStreamBench.bytesPerOp = float64(after.TotalAlloc-before.TotalAlloc) / streams
		imageStreamBench.allocsPerOp = float64(after.Mallocs-before.Mallocs) / streams
	})
	if imageStreamBench.err != "" {
		b.Fatal(imageStreamBench.err)
	}
	b.ReportMetric(imageStreamBench.nsPerOp, "ns/op")
	b.ReportMetric(imageStreamBench.bytesPerOp, "B/op")
	b.ReportMetric(imageStreamBench.allocsPerOp, "allocs/op")
}

// BenchmarkBytePlusEmbedding is an embedding request of one input and of eight, each input its
// own Ark call, against a local server.
func BenchmarkBytePlusEmbedding(b *testing.B) {
	provider := benchBytePlus(b, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		jsonResponse(http.StatusOK, `{"id":"e1","model":"skylark-embedding-vision-251215","data":{"embedding":[0.1,0.2,0.3,0.4]},"usage":{"prompt_tokens":3,"total_tokens":3,"prompt_tokens_details":{"text_tokens":3,"image_tokens":0}}}`)(w, r)
	})
	key := testKey()
	for _, inputs := range []int{1, 8} {
		items := make([]schemas.EmbeddingInputItem, inputs)
		for i := range items {
			items[i] = schemas.EmbeddingInputItem{Content: []schemas.EmbeddingContentPart{{Type: schemas.EmbeddingContentPartTypeText, Text: schemas.Ptr("text")}}}
		}
		req := &schemas.BifrostEmbeddingRequest{Provider: schemas.BytePlus, Model: "skylark-embedding-vision-251215", Input: items}
		b.Run(strconv.Itoa(inputs)+"_inputs", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
				resp, bifrostErr := provider.Embedding(ctx, key, req)
				if bifrostErr != nil || len(resp.Data) != inputs {
					b.Fatalf("Embedding: %+v", bifrostErr)
				}
				ctx.Cancel()
			}
		})
	}
}

// BenchmarkToBifrostVideoGenerationResponse is the conversion of a settled Seedance task,
// including the fork's "WxH" size built from the task's tier and ratio.
func BenchmarkToBifrostVideoGenerationResponse(b *testing.B) {
	var task byteplus.BytePlusVideoTask
	if err := json.Unmarshal([]byte(`{"id":"cgt-1","model":"seedance-1-0-pro-fast-251015","status":"succeeded","content":{"video_url":"https://cdn.example/v.mp4"},"created_at":100,"updated_at":200,"resolution":"720p","ratio":"16:9","duration":5}`), &task); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if resp, bifrostErr := byteplus.ToBifrostVideoGenerationResponse(&task); bifrostErr != nil || resp.Size != "1280x720" {
			b.Fatalf("size=%q err=%v", resp.Size, bifrostErr)
		}
	}
}

// Extra params are merged over the converted request body, so one named "model" would send Ark a
// different model than the one the request was authorized, routed and priced for. Every operation
// that merges extra params refuses it with a 400 before calling Ark.
func TestBytePlusRefusesModelInExtraParams(t *testing.T) {
	provider, captured, _ := newMockBytePlus(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/v3/chat/completions": jsonResponse(http.StatusOK, chatSuccessBody),
	})
	key := testKey()
	override := map[string]interface{}{"model": "blocked-model"}
	text := []schemas.EmbeddingContentPart{{Type: schemas.EmbeddingContentPartTypeText, Text: schemas.Ptr("text")}}
	image := &schemas.ImageGenerationInput{Prompt: "a cat"}
	edit := &schemas.ImageEditInput{Prompt: "make it blue", Images: []schemas.ImageInput{{URL: "https://example.com/a.png"}}}
	responsesReq := &schemas.BifrostResponsesRequest{
		Provider: schemas.BytePlus, Model: "seed-2-0-lite-260228",
		Input:  []schemas.ResponsesMessage{llmtests.CreateBasicResponsesMessage("hi")},
		Params: &schemas.ResponsesParameters{ExtraParams: override},
	}
	imageReq := &schemas.BifrostImageGenerationRequest{Provider: schemas.BytePlus, Model: "seedream-4-5-251128", Input: image, Params: &schemas.ImageGenerationParameters{ExtraParams: override}}
	editReq := &schemas.BifrostImageEditRequest{Provider: schemas.BytePlus, Model: "seedream-4-5-251128", Input: edit, Params: &schemas.ImageEditParameters{ExtraParams: override}}
	operations := map[string]func(ctx *schemas.BifrostContext) *schemas.BifrostError{
		"chat": func(ctx *schemas.BifrostContext) *schemas.BifrostError {
			_, e := provider.ChatCompletion(ctx, key, userChatRequest(&schemas.ChatParameters{ExtraParams: override}))
			return e
		},
		"chat stream": func(ctx *schemas.BifrostContext) *schemas.BifrostError {
			_, e := provider.ChatCompletionStream(ctx, passthroughPostHook, nil, key, userChatRequest(&schemas.ChatParameters{ExtraParams: override}))
			return e
		},
		"responses": func(ctx *schemas.BifrostContext) *schemas.BifrostError {
			_, e := provider.Responses(ctx, key, responsesReq)
			return e
		},
		"responses stream": func(ctx *schemas.BifrostContext) *schemas.BifrostError {
			_, e := provider.ResponsesStream(ctx, passthroughPostHook, nil, key, responsesReq)
			return e
		},
		"embedding": func(ctx *schemas.BifrostContext) *schemas.BifrostError {
			_, e := provider.Embedding(ctx, key, &schemas.BifrostEmbeddingRequest{
				Provider: schemas.BytePlus, Model: "skylark-embedding-vision-251215",
				Input:  []schemas.EmbeddingInputItem{{Content: text}},
				Params: &schemas.EmbeddingParameters{ExtraParams: override},
			})
			return e
		},
		"embedding item": func(ctx *schemas.BifrostContext) *schemas.BifrostError {
			_, e := provider.Embedding(ctx, key, &schemas.BifrostEmbeddingRequest{
				Provider: schemas.BytePlus, Model: "skylark-embedding-vision-251215",
				Input: []schemas.EmbeddingInputItem{{Content: text}, {Content: text, Params: &schemas.EmbeddingParameters{ExtraParams: override}}},
			})
			return e
		},
		"image": func(ctx *schemas.BifrostContext) *schemas.BifrostError {
			_, e := provider.ImageGeneration(ctx, key, imageReq)
			return e
		},
		"image stream": func(ctx *schemas.BifrostContext) *schemas.BifrostError {
			_, e := provider.ImageGenerationStream(ctx, passthroughPostHook, nil, key, imageReq)
			return e
		},
		"image edit": func(ctx *schemas.BifrostContext) *schemas.BifrostError {
			_, e := provider.ImageEdit(ctx, key, editReq)
			return e
		},
		"image edit stream": func(ctx *schemas.BifrostContext) *schemas.BifrostError {
			_, e := provider.ImageEditStream(ctx, passthroughPostHook, nil, key, editReq)
			return e
		},
		"video": func(ctx *schemas.BifrostContext) *schemas.BifrostError {
			_, e := provider.VideoGeneration(ctx, key, &schemas.BifrostVideoGenerationRequest{
				Provider: schemas.BytePlus, Model: "seedance-1-0-pro-fast-251015",
				Input:  &schemas.VideoGenerationInput{Prompt: "a dog"},
				Params: &schemas.VideoGenerationParameters{ExtraParams: override},
			})
			return e
		},
	}
	for name, call := range operations {
		bifrostErr := call(streamTestContext(t))
		if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: want a 400 for extra params naming a model, got %+v", name, bifrostErr)
			continue
		}
		if !strings.Contains(bifrostErr.Error.Message, "model") {
			t.Errorf("%s: error should name the model field: %q", name, bifrostErr.Error.Message)
		}
	}
	// The key is refused in any case: "Model" would sit beside "model" in the body, and a JSON
	// decoder that folds case could read either.
	for _, spelling := range []string{"Model", "MODEL", "mOdEl"} {
		_, bifrostErr := provider.ChatCompletion(streamTestContext(t), key, userChatRequest(&schemas.ChatParameters{ExtraParams: map[string]interface{}{spelling: "blocked-model"}}))
		if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
			t.Errorf("extra param %q: want a 400, got %+v", spelling, bifrostErr)
		}
	}
	for _, r := range captured() {
		t.Errorf("%s %s reached Ark with body %s", r.method, r.path, r.body)
	}

	// A raw "content" in a video create's extra params would replace the converter's content array
	// and carry a draft task or media URL past the converter and governance's ownership check.
	for _, content := range []interface{}{
		[]interface{}{map[string]interface{}{"type": "draft_task", "draft_task": map[string]interface{}{"id": "cgt-other"}}},
		"anything",
	} {
		_, bifrostErr := provider.VideoGeneration(streamTestContext(t), key, &schemas.BifrostVideoGenerationRequest{
			Provider: schemas.BytePlus, Model: "seedance-1-0-pro-fast-251015",
			Input:  &schemas.VideoGenerationInput{Prompt: "a dog"},
			Params: &schemas.VideoGenerationParameters{ExtraParams: map[string]interface{}{"content": content}},
		})
		if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest || !strings.Contains(bifrostErr.Error.Message, "content") {
			t.Errorf("video create with extra param content: want a 400 naming content, got %+v", bifrostErr)
		}
	}
	for _, r := range captured() {
		t.Errorf("%s %s reached Ark with body %s", r.method, r.path, r.body)
	}

	// Other extra params still reach Ark, under the request's own model.
	if _, bifrostErr := provider.ChatCompletion(streamTestContext(t), key, userChatRequest(&schemas.ChatParameters{ExtraParams: map[string]interface{}{"service_tier": "flex"}})); bifrostErr != nil {
		t.Fatalf("chat with an ordinary extra param: %+v", bifrostErr.Error)
	}
	reqs := captured()
	if len(reqs) != 1 || gjson.GetBytes(reqs[0].body, "model").String() != "seed-2-0-lite-260228" || gjson.GetBytes(reqs[0].body, "service_tier").String() != "flex" {
		t.Errorf("want one call with the request's model and the extra param, got %d: %s", len(reqs), reqs[len(reqs)-1].body)
	}
}
