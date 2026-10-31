package volcengine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

type testLogger struct{}

func (l *testLogger) Debug(msg string, args ...any)                     {}
func (l *testLogger) Info(msg string, args ...any)                      {}
func (l *testLogger) Warn(msg string, args ...any)                      {}
func (l *testLogger) Error(msg string, args ...any)                     {}
func (l *testLogger) Fatal(msg string, args ...any)                     {}
func (l *testLogger) SetLevel(level schemas.LogLevel)                   {}
func (l *testLogger) SetOutputType(outputType schemas.LoggerOutputType) {}
func (l *testLogger) LogHTTPRequest(level schemas.LogLevel, msg string) schemas.LogEventBuilder {
	return &noopLogEventBuilder{}
}

type noopLogEventBuilder struct{}

func (b *noopLogEventBuilder) Str(key, val string) schemas.LogEventBuilder         { return b }
func (b *noopLogEventBuilder) Int(key string, val int) schemas.LogEventBuilder     { return b }
func (b *noopLogEventBuilder) Int64(key string, val int64) schemas.LogEventBuilder { return b }
func (b *noopLogEventBuilder) Send()                                               {}

func newTestVolcengineCompatibleProvider(baseURL string, providerKey schemas.ModelProvider) *VolcengineProvider {
	return &VolcengineProvider{
		logger: &testLogger{},
		client: &fasthttp.Client{
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 5 * time.Second,
		},
		networkConfig: schemas.NetworkConfig{
			BaseURL: baseURL,
		},
		providerKey: providerKey,
	}
}

func newTestVolcengineProvider(baseURL string) *VolcengineProvider {
	return newTestVolcengineCompatibleProvider(baseURL, schemas.Volcengine)
}

func TestNewModelArkProvider_Defaults(t *testing.T) {
	t.Parallel()

	provider, err := NewModelArkProvider(&schemas.ProviderConfig{}, &testLogger{})
	if err != nil {
		t.Fatalf("NewModelArkProvider returned error: %v", err)
	}

	if provider.GetProviderKey() != schemas.ModelArk {
		t.Fatalf("expected provider key %s, got %s", schemas.ModelArk, provider.GetProviderKey())
	}
	if provider.networkConfig.BaseURL != "https://ark.ap-southeast.bytepluses.com/api/v3" {
		t.Fatalf("unexpected default base URL: %s", provider.networkConfig.BaseURL)
	}
}

func TestMultiModalEmbedding_TextAndImage(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings/multimodal" {
			t.Errorf("expected path /embeddings/multimodal, got %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", r.Header.Get("Content-Type"))
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("expected Authorization Bearer test-key, got %s", r.Header.Get("Authorization"))
		}
		var requestBody struct {
			Model           string `json:"model"`
			Instructions    string `json:"instructions"`
			EncodingFormat  string `json:"encoding_format"`
			Dimensions      int    `json:"dimensions"`
			SparseEmbedding struct {
				Type string `json:"type"`
			} `json:"sparse_embedding"`
		}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Fatalf("failed to decode request body: %v", err)
		}
		if requestBody.Model != "doubao-embedding-vision-251215" {
			t.Errorf("expected model doubao-embedding-vision-251215, got %s", requestBody.Model)
		}
		if requestBody.Instructions != "Target_modality: text and video.\nInstruction:Compress the text\\video into one word.\nQuery:" {
			t.Errorf("unexpected instructions: %s", requestBody.Instructions)
		}
		if requestBody.EncodingFormat != "float" {
			t.Errorf("expected encoding_format float, got %s", requestBody.EncodingFormat)
		}
		if requestBody.Dimensions != 2048 {
			t.Errorf("expected dimensions 2048, got %d", requestBody.Dimensions)
		}
		if requestBody.SparseEmbedding.Type != "" {
			t.Errorf("expected sparse_embedding not set, got %s", requestBody.SparseEmbedding.Type)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"data": {
				"embedding": [0.1, 0.2, 0.3, 0.4],
				"object": "embedding"
			},
			"id": "test-id",
			"created": 1752133360,
			"model": "doubao-embedding-vision-251215",
			"object": "list",
			"usage": {
				"prompt_tokens": 42,
				"prompt_tokens_details": {"text_tokens": 42, "image_tokens": 0},
				"total_tokens": 42
			}
		}`)
	}))
	defer server.Close()

	provider := newTestVolcengineProvider(server.URL)

	text := "a photo of a cat"
	instructions := "Target_modality: text and video.\nInstruction:Compress the text\\video into one word.\nQuery:"
	encodingFormat := "float"
	request := &schemas.BifrostEmbeddingRequest{
		Provider: schemas.Volcengine,
		Model:    "doubao-embedding-vision-251215",
		Input: flatInput(
			schemas.MultiModalEmbeddingInput{Type: schemas.MultiModalEmbeddingText, Text: &text},
			schemas.MultiModalEmbeddingInput{Type: schemas.MultiModalEmbeddingImageURL, ImageURL: &schemas.MultiModalEmbeddingMediaURL{URL: "https://example.com/cat.jpg"}},
		),
		Params: &schemas.EmbeddingParameters{
			Instructions:   &instructions,
			EncodingFormat: &encodingFormat,
			Dimensions:     intPtr(2048),
		},
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	resp, bifrostErr := provider.Embedding(ctx, schemas.Key{Value: *schemas.NewSecretVar("test-key")}, request)
	if bifrostErr != nil {
		t.Fatalf("Embedding returned error: %v", bifrostErr.Error)
	}

	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if len(resp.Data) != 1 {
		t.Fatalf("expected 1 embedding data, got %d", len(resp.Data))
	}
	if len(resp.Data[0].Embedding.EmbeddingArray) != 4 {
		t.Fatalf("expected 4-dim embedding, got %d", len(resp.Data[0].Embedding.EmbeddingArray))
	}
	if math.Abs(resp.Data[0].Embedding.EmbeddingArray[0]-0.1) > 1e-6 {
		t.Fatalf("expected first value close to 0.1, got %f", resp.Data[0].Embedding.EmbeddingArray[0])
	}
	if resp.Data[0].Object != "embedding" {
		t.Fatalf("expected object 'embedding', got '%s'", resp.Data[0].Object)
	}
	if len(resp.Data[0].SparseEmbedding) != 0 {
		t.Fatalf("expected 0 sparse embedding entries, got %d", len(resp.Data[0].SparseEmbedding))
	}
	if resp.Model != "doubao-embedding-vision-251215" {
		t.Fatalf("expected model 'doubao-embedding-vision-251215', got '%s'", resp.Model)
	}
	if resp.Object != "list" {
		t.Fatalf("expected object 'list', got '%s'", resp.Object)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 42 {
		t.Fatalf("expected usage.total_tokens=42, got %v", resp.Usage)
	}
	if resp.Usage == nil || resp.Usage.PromptTokens != 42 {
		t.Fatalf("expected usage.prompt_tokens=42, got %v", resp.Usage)
	}
	if resp.Usage == nil || resp.Usage.PromptTokensDetails == nil || resp.Usage.PromptTokensDetails.TextTokens != 42 {
		t.Fatalf("expected usage.prompt_tokens_details.text_tokens=42, got %v", resp.Usage)
	}
	if resp.ExtraFields.Provider != schemas.Volcengine {
		t.Fatalf("expected provider volcengine, got %s", resp.ExtraFields.Provider)
	}
	if resp.ExtraFields.RequestType != schemas.EmbeddingRequest {
		t.Fatalf("expected request type embedding, got %s", resp.ExtraFields.RequestType)
	}
}

func TestMultiModalEmbedding_VideoInput(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings/multimodal" {
			t.Errorf("expected path /embeddings/multimodal, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"data": {"embedding": [0.5, 0.6]},
			"model": "doubao-embedding-vision-250615"
		}`)
	}))
	defer server.Close()

	provider := newTestVolcengineProvider(server.URL)

	request := &schemas.BifrostEmbeddingRequest{
		Provider: schemas.Volcengine,
		Model:    "doubao-embedding-vision-250615",
		Input: flatInput(
			schemas.MultiModalEmbeddingInput{Type: schemas.MultiModalEmbeddingVideoURL, VideoURL: &schemas.MultiModalEmbeddingMediaURL{URL: "https://example.com/video.mp4"}},
		),
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	resp, bifrostErr := provider.Embedding(ctx, schemas.Key{Value: *schemas.NewSecretVar("key")}, request)
	if bifrostErr != nil {
		t.Fatalf("Embedding returned error: %v", bifrostErr.Error)
	}

	if len(resp.Data) != 1 {
		t.Fatalf("expected 1 embedding, got %d", len(resp.Data))
	}
	if len(resp.Data[0].Embedding.EmbeddingArray) != 2 {
		t.Fatalf("expected 2-dim embedding, got %d", len(resp.Data[0].Embedding.EmbeddingArray))
	}
	// Usage should be nil when not returned
	if resp.Usage != nil {
		t.Fatalf("expected nil usage when not in response, got %v", resp.Usage)
	}
}

func TestMultiModalEmbedding_ModelArkMetadata(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings/multimodal" {
			t.Errorf("expected path /embeddings/multimodal, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"data": {"embedding": [0.5, 0.6], "object": "embedding"},
			"model": "doubao-embedding-vision-251215",
			"object": "list",
			"usage": {"prompt_tokens": 2, "total_tokens": 2}
		}`)
	}))
	defer server.Close()

	provider := newTestVolcengineCompatibleProvider(server.URL, schemas.ModelArk)
	text := "hello"
	instructions := "Target_modality: text.\nInstruction:Retrieve semantically similar text\nQuery:"
	request := &schemas.BifrostEmbeddingRequest{
		Provider: schemas.ModelArk,
		Model:    "doubao-embedding-vision-251215",
		Input: flatInput(
			schemas.MultiModalEmbeddingInput{Type: schemas.MultiModalEmbeddingText, Text: &text},
		),
		Params: &schemas.EmbeddingParameters{
			Instructions: &instructions,
		},
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	resp, bifrostErr := provider.Embedding(ctx, schemas.Key{}, request)
	if bifrostErr != nil {
		t.Fatalf("Embedding returned error: %v", bifrostErr.Error)
	}

	if resp.ExtraFields.Provider != schemas.ModelArk {
		t.Fatalf("expected provider %s, got %s", schemas.ModelArk, resp.ExtraFields.Provider)
	}
}

func TestMultiModalEmbedding_FallsBackToTextEmbedding(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Text-only requests should go to /embeddings, not /embeddings/multimodal
		if r.URL.Path != "/embeddings" {
			t.Errorf("expected path /embeddings for text input, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"data": [{"index": 0, "object": "embedding", "embedding": [0.1, 0.2]}],
			"model": "doubao-embedding-large-text-240915",
			"object": "list",
			"usage": {"prompt_tokens": 5, "total_tokens": 5}
		}`)
	}))
	defer server.Close()

	provider := newTestVolcengineProvider(server.URL)

	text := "hello world"
	request := &schemas.BifrostEmbeddingRequest{
		Provider: schemas.Volcengine,
		Model:    "doubao-embedding-large-text-240915",
		Input:    textInput(text),
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	resp, bifrostErr := provider.Embedding(ctx, schemas.Key{Value: *schemas.NewSecretVar("key")}, request)
	if bifrostErr != nil {
		t.Fatalf("Embedding returned error: %v", bifrostErr.Error)
	}

	if len(resp.Data) != 1 {
		t.Fatalf("expected 1 embedding, got %d", len(resp.Data))
	}
}

func TestMultiModalEmbedding_ModelArkTextOnlyVisionUsesMultimodalEndpoint(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings/multimodal" {
			t.Errorf("expected path /embeddings/multimodal for skylark vision text input, got %s", r.URL.Path)
		}

		var requestBody struct {
			Model string `json:"model"`
			Input []struct {
				Type string  `json:"type"`
				Text *string `json:"text,omitempty"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Fatalf("failed to decode request body: %v", err)
		}
		if requestBody.Model != "skylark-embedding-vision-251215" {
			t.Fatalf("expected model skylark-embedding-vision-251215, got %s", requestBody.Model)
		}
		if len(requestBody.Input) != 1 || requestBody.Input[0].Type != string(schemas.MultiModalEmbeddingText) {
			t.Fatalf("expected one typed text multimodal input, got %+v", requestBody.Input)
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"data": {"embedding": [0.1, 0.2], "object": "embedding"},
			"model": "skylark-embedding-vision-251215",
			"object": "list",
			"usage": {"prompt_tokens": 2, "total_tokens": 2}
		}`)
	}))
	defer server.Close()

	provider := newTestVolcengineCompatibleProvider(server.URL, schemas.ModelArk)
	text := "hello world"
	instructions := "Target_modality: text.\nInstruction:Retrieve semantically similar text\nQuery:"
	request := &schemas.BifrostEmbeddingRequest{
		Provider: schemas.ModelArk,
		Model:    "skylark-embedding-vision-251215",
		Input:    textInput(text),
		Params: &schemas.EmbeddingParameters{
			Instructions: &instructions,
		},
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	resp, bifrostErr := provider.Embedding(ctx, schemas.Key{Value: *schemas.NewSecretVar("test-key")}, request)
	if bifrostErr != nil {
		t.Fatalf("Embedding returned error: %v", bifrostErr.Error)
	}
	if resp.ExtraFields.Provider != schemas.ModelArk {
		t.Fatalf("expected provider %s, got %s", schemas.ModelArk, resp.ExtraFields.Provider)
	}
}

func TestMultiModalEmbedding_ServerError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"invalid model","type":"invalid_request_error"}}`)
	}))
	defer server.Close()

	provider := newTestVolcengineProvider(server.URL)

	text := "test"
	request := &schemas.BifrostEmbeddingRequest{
		Provider: schemas.Volcengine,
		Model:    "nonexistent-model",
		Input: flatInput(
			schemas.MultiModalEmbeddingInput{Type: schemas.MultiModalEmbeddingText, Text: &text},
		),
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, bifrostErr := provider.Embedding(ctx, schemas.Key{Value: *schemas.NewSecretVar("key")}, request)
	if bifrostErr == nil {
		t.Fatal("expected error for server error response")
	}
}

func TestMultiModalEmbedding_MixedInputs(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings/multimodal" {
			t.Errorf("expected path /embeddings/multimodal, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"data": {"embedding": [0.1, 0.2, 0.3]},
			"model": "doubao-embedding-vision-250615",
			"usage": {"total_tokens": 100}
		}`)
	}))
	defer server.Close()

	provider := newTestVolcengineProvider(server.URL)

	text := "a cat playing"
	request := &schemas.BifrostEmbeddingRequest{
		Provider: schemas.Volcengine,
		Model:    "doubao-embedding-vision-250615",
		Input: flatInput(
			schemas.MultiModalEmbeddingInput{Type: schemas.MultiModalEmbeddingText, Text: &text},
			schemas.MultiModalEmbeddingInput{Type: schemas.MultiModalEmbeddingImageURL, ImageURL: &schemas.MultiModalEmbeddingMediaURL{URL: "https://example.com/cat.jpg"}},
			schemas.MultiModalEmbeddingInput{Type: schemas.MultiModalEmbeddingVideoURL, VideoURL: &schemas.MultiModalEmbeddingMediaURL{URL: "https://example.com/cat.mp4"}},
		),
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	resp, bifrostErr := provider.Embedding(ctx, schemas.Key{Value: *schemas.NewSecretVar("key")}, request)
	if bifrostErr != nil {
		t.Fatalf("Embedding returned error: %v", bifrostErr.Error)
	}

	if len(resp.Data) != 1 {
		t.Fatalf("expected 1 embedding, got %d", len(resp.Data))
	}
	if len(resp.Data[0].Embedding.EmbeddingArray) != 3 {
		t.Fatalf("expected 3-dim embedding, got %d", len(resp.Data[0].Embedding.EmbeddingArray))
	}
}

func TestMultiModalEmbedding_RawRequestResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data": {"embedding": [0.1]}, "model": "test"}`)
	}))
	defer server.Close()

	provider := &VolcengineProvider{
		logger: &testLogger{},
		client: &fasthttp.Client{
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 5 * time.Second,
		},
		networkConfig: schemas.NetworkConfig{
			BaseURL: server.URL,
		},
		sendBackRawRequest:  true,
		sendBackRawResponse: true,
	}

	text := "test"
	request := &schemas.BifrostEmbeddingRequest{
		Provider: schemas.Volcengine,
		Model:    "doubao-embedding-vision-250615",
		Input: flatInput(
			schemas.MultiModalEmbeddingInput{Type: schemas.MultiModalEmbeddingText, Text: &text},
		),
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	resp, bifrostErr := provider.Embedding(ctx, schemas.Key{Value: *schemas.NewSecretVar("key")}, request)
	if bifrostErr != nil {
		t.Fatalf("Embedding returned error: %v", bifrostErr.Error)
	}

	if resp.ExtraFields.RawRequest == nil {
		t.Fatal("expected RawRequest to be set")
	}
	if resp.ExtraFields.RawResponse == nil {
		t.Fatal("expected RawResponse to be set")
	}
}

func TestMultiModalEmbedding_TextOnlySparse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var requestBody struct {
			SparseEmbedding struct {
				Type string `json:"type"`
			} `json:"sparse_embedding"`
		}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Fatalf("failed to decode request body: %v", err)
		}
		if requestBody.SparseEmbedding.Type != "enabled" {
			t.Fatalf("expected sparse_embedding.type enabled, got %s", requestBody.SparseEmbedding.Type)
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"data": {
				"embedding": [0.1, 0.2],
				"sparse_embedding": [{"index": 1, "value": 0.0887451171875}]
			},
			"model": "doubao-embedding-vision-251215",
			"object": "list",
			"usage": {"prompt_tokens": 5, "total_tokens": 5}
		}`)
	}))
	defer server.Close()

	provider := newTestVolcengineProvider(server.URL)
	text := "the sky is blue"
	instructions := "Target_modality: text.\nInstruction:Retrieve semantically similar text\nQuery:"
	request := &schemas.BifrostEmbeddingRequest{
		Provider: schemas.Volcengine,
		Model:    "doubao-embedding-vision-251215",
		Input: flatInput(
			schemas.MultiModalEmbeddingInput{Type: schemas.MultiModalEmbeddingText, Text: &text},
		),
		Params: &schemas.EmbeddingParameters{
			Instructions: &instructions,
			SparseEmbedding: map[string]interface{}{
				"type": "enabled",
			},
		},
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	resp, bifrostErr := provider.Embedding(ctx, schemas.Key{Value: *schemas.NewSecretVar("key")}, request)
	if bifrostErr != nil {
		t.Fatalf("Embedding returned error: %v", bifrostErr.Error)
	}
	if len(resp.Data) != 1 || len(resp.Data[0].SparseEmbedding) != 1 {
		t.Fatalf("expected sparse embedding in response, got %v", resp.Data)
	}
}

func TestMultiModalEmbedding_TextInputUsesMultimodalEndpointAndHelperConfig(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings/multimodal" {
			t.Fatalf("expected path /embeddings/multimodal, got %s", r.URL.Path)
		}

		var requestBody struct {
			Input        []schemas.MultiModalEmbeddingInput `json:"input"`
			Instructions string                             `json:"instructions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Fatalf("failed to decode request body: %v", err)
		}
		if len(requestBody.Input) != 1 || requestBody.Input[0].Type != schemas.MultiModalEmbeddingText || requestBody.Input[0].Text == nil || *requestBody.Input[0].Text != "hello" {
			t.Fatalf("unexpected multimodal input payload: %+v", requestBody.Input)
		}
		expectedInstructions := "Instruction:Compress the text into one word.\nQuery:"
		if requestBody.Instructions != expectedInstructions {
			t.Fatalf("unexpected generated instructions.\nexpected: %s\ngot: %s", expectedInstructions, requestBody.Instructions)
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"embedding":[0.1,0.2]},"model":"doubao-embedding-vision-251215","object":"list","usage":{"total_tokens":2}}`)
	}))
	defer server.Close()

	provider := newTestVolcengineProvider(server.URL)
	text := "hello"
	request := &schemas.BifrostEmbeddingRequest{
		Provider: schemas.Volcengine,
		Model:    "doubao-embedding-vision-251215",
		Input:    textInput(text),
		Params: &schemas.EmbeddingParameters{
			ExtraParams: map[string]interface{}{
				"volcengine_instructions_config": map[string]interface{}{
					"task_type":         "retrieval/ranking",
					"role":              "corpus",
					"target_modality":   "text",
					"validate_template": true,
				},
			},
		},
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	resp, bifrostErr := provider.Embedding(ctx, schemas.Key{Value: *schemas.NewSecretVar("key")}, request)
	if bifrostErr != nil {
		t.Fatalf("Embedding returned error: %v", bifrostErr.Error)
	}
	if len(resp.Data) != 1 || len(resp.Data[0].Embedding.EmbeddingArray) != 2 {
		t.Fatalf("expected embedding response, got %+v", resp)
	}
}

func TestMultiModalEmbedding_HelperConfigAcceptsRetrievalQueryAlias(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var requestBody struct {
			Instructions string `json:"instructions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Fatalf("failed to decode request body: %v", err)
		}
		expected := "Target_modality: image.\nInstruction:Find me an everyday image that matches the given caption\nQuery:"
		if requestBody.Instructions != expected {
			t.Fatalf("unexpected generated instructions.\nexpected: %s\ngot: %s", expected, requestBody.Instructions)
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"embedding":[0.1]},"model":"doubao-embedding-vision-251215","object":"list","usage":{"total_tokens":1}}`)
	}))
	defer server.Close()

	provider := newTestVolcengineProvider(server.URL)
	text := "blue sea view"
	request := &schemas.BifrostEmbeddingRequest{
		Provider: schemas.Volcengine,
		Model:    "doubao-embedding-vision-251215",
		Input: flatInput(
			schemas.MultiModalEmbeddingInput{Type: schemas.MultiModalEmbeddingText, Text: &text},
		),
		Params: &schemas.EmbeddingParameters{
			ExtraParams: map[string]interface{}{
				"volcengine_instructions_config": map[string]interface{}{
					"task_type":       "retrieval_query",
					"role":            "query",
					"target_modality": "image",
					"instruction":     "Find me an everyday image that matches the given caption",
				},
			},
		},
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	if _, bifrostErr := provider.Embedding(ctx, schemas.Key{Value: *schemas.NewSecretVar("key")}, request); bifrostErr != nil {
		t.Fatalf("Embedding returned error: %v", bifrostErr.Error)
	}
}

func TestMultiModalEmbedding_InstructionRequiredFor251215(t *testing.T) {
	t.Parallel()

	provider := newTestVolcengineProvider("http://localhost")
	text := "query"
	request := &schemas.BifrostEmbeddingRequest{
		Provider: schemas.Volcengine,
		Model:    "doubao-embedding-vision-251215",
		Input: flatInput(
			schemas.MultiModalEmbeddingInput{Type: schemas.MultiModalEmbeddingText, Text: &text},
		),
		Params: &schemas.EmbeddingParameters{},
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, bifrostErr := provider.Embedding(ctx, schemas.Key{}, request)
	if bifrostErr == nil {
		t.Fatal("expected instructions-required error")
	}
	if bifrostErr.Error == nil || !strings.Contains(strings.ToLower(bifrostErr.Error.Message), "instructions are required") {
		t.Fatalf("unexpected error message: %+v", bifrostErr)
	}
}

func TestMultiModalEmbedding_InstructionUnsupportedBefore251215(t *testing.T) {
	t.Parallel()

	provider := newTestVolcengineProvider("http://localhost")
	text := "query"
	instructions := "Target_modality: text.\nInstruction:Retrieve semantically similar text\nQuery:"
	request := &schemas.BifrostEmbeddingRequest{
		Provider: schemas.Volcengine,
		Model:    "doubao-embedding-vision-250615",
		Input: flatInput(
			schemas.MultiModalEmbeddingInput{Type: schemas.MultiModalEmbeddingText, Text: &text},
		),
		Params: &schemas.EmbeddingParameters{
			Instructions: &instructions,
		},
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, bifrostErr := provider.Embedding(ctx, schemas.Key{}, request)
	if bifrostErr == nil {
		t.Fatal("expected instructions-unsupported error")
	}
	if bifrostErr.Error == nil || !strings.Contains(strings.ToLower(bifrostErr.Error.Message), "supported only") {
		t.Fatalf("unexpected error message: %+v", bifrostErr)
	}
}

func TestMultiModalEmbedding_HelperConfigGeneratesQueryInstructions(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var requestBody struct {
			Instructions string `json:"instructions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Fatalf("failed to decode request body: %v", err)
		}
		expected := "Target_modality: text/image.\nInstruction:根据这个问题，找到能回答这个问题的相应文本或图片\nQuery:"
		if requestBody.Instructions != expected {
			t.Fatalf("unexpected generated instructions.\nexpected: %s\ngot: %s", expected, requestBody.Instructions)
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"embedding":[0.1]},"model":"doubao-embedding-vision-251215","object":"list","usage":{"total_tokens":1}}`)
	}))
	defer server.Close()

	provider := newTestVolcengineProvider(server.URL)
	text := "who can answer this?"
	request := &schemas.BifrostEmbeddingRequest{
		Provider: schemas.Volcengine,
		Model:    "doubao-embedding-vision-251215",
		Input: flatInput(
			schemas.MultiModalEmbeddingInput{Type: schemas.MultiModalEmbeddingText, Text: &text},
		),
		Params: &schemas.EmbeddingParameters{
			ExtraParams: map[string]interface{}{
				"volcengine_instructions_config": map[string]interface{}{
					"task_type":       "retrieval_ranking",
					"role":            "query",
					"target_modality": "text/image",
					"instruction":     "根据这个问题，找到能回答这个问题的相应文本或图片",
				},
			},
		},
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	if _, bifrostErr := provider.Embedding(ctx, schemas.Key{}, request); bifrostErr != nil {
		t.Fatalf("Embedding returned error: %v", bifrostErr.Error)
	}
}

func TestMultiModalEmbedding_TemplateValidation(t *testing.T) {
	t.Parallel()

	provider := newTestVolcengineProvider("http://localhost")
	text := "test"
	badInstructions := "Instruction:freeform"
	request := &schemas.BifrostEmbeddingRequest{
		Provider: schemas.Volcengine,
		Model:    "doubao-embedding-vision-251215",
		Input: flatInput(
			schemas.MultiModalEmbeddingInput{Type: schemas.MultiModalEmbeddingText, Text: &text},
		),
		Params: &schemas.EmbeddingParameters{
			Instructions: &badInstructions,
			ExtraParams: map[string]interface{}{
				"volcengine_instructions_config": map[string]interface{}{
					"validate_template": true,
				},
			},
		},
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, bifrostErr := provider.Embedding(ctx, schemas.Key{}, request)
	if bifrostErr == nil {
		t.Fatal("expected template-validation error")
	}
	if bifrostErr.Error == nil || !strings.Contains(strings.ToLower(bifrostErr.Error.Message), "template") {
		t.Fatalf("unexpected error message: %+v", bifrostErr)
	}
}

func TestMultiModalEmbedding_SparseTextOnlyValidation(t *testing.T) {
	t.Parallel()

	provider := newTestVolcengineProvider("http://localhost")
	text := "query"
	instructions := "Target_modality: image.\nInstruction:Find me an everyday image that matches the given caption\nQuery:"
	request := &schemas.BifrostEmbeddingRequest{
		Provider: schemas.Volcengine,
		Model:    "doubao-embedding-vision-251215",
		Input: flatInput(
			schemas.MultiModalEmbeddingInput{Type: schemas.MultiModalEmbeddingText, Text: &text},
			schemas.MultiModalEmbeddingInput{Type: schemas.MultiModalEmbeddingImageURL, ImageURL: &schemas.MultiModalEmbeddingMediaURL{URL: "https://example.com/cat.jpg"}},
		),
		Params: &schemas.EmbeddingParameters{
			Instructions: &instructions,
			SparseEmbedding: map[string]interface{}{
				"type": "enabled",
			},
		},
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, bifrostErr := provider.Embedding(ctx, schemas.Key{}, request)
	if bifrostErr == nil {
		t.Fatal("expected sparse text-only validation error")
	}
	if bifrostErr.Error == nil || !strings.Contains(strings.ToLower(bifrostErr.Error.Message), "text-only") {
		t.Fatalf("unexpected error message: %+v", bifrostErr)
	}
}

func intPtr(v int) *int {
	return &v
}

// flatInput builds the one input item the fork's flat multimodal list becomes.
func flatInput(entries ...schemas.MultiModalEmbeddingInput) []schemas.EmbeddingInputItem {
	content := make(schemas.EmbeddingContent, len(entries))
	for i, entry := range entries {
		part, err := entry.ToEmbeddingContentPart()
		if err != nil {
			panic(err)
		}
		content[i] = part
	}
	return []schemas.EmbeddingInputItem{{Content: content}}
}

// textInput builds a single plain-text input item ("input": "text").
func textInput(text string) []schemas.EmbeddingInputItem {
	return []schemas.EmbeddingInputItem{{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &text}}}}
}

// TestMultiModalEmbedding_VisionMultipleTextPartsUseMultimodalEndpoint pins that one input
// item with several text parts to a vision model stays on /embeddings/multimodal, for
// Volcengine and ModelArk alike: /embeddings would join the parts and drop instructions
// and sparse_embedding.
func TestMultiModalEmbedding_VisionMultipleTextPartsUseMultimodalEndpoint(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		provider schemas.ModelProvider
		model    string
	}{
		"volcengine": {provider: schemas.Volcengine, model: "doubao-embedding-vision-251215"},
		"modelark":   {provider: schemas.ModelArk, model: "skylark-embedding-vision-251215"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			type sentBody struct {
				Model string `json:"model"`
				Input []struct {
					Type string  `json:"type"`
					Text *string `json:"text,omitempty"`
				} `json:"input"`
				Instructions    *string                `json:"instructions"`
				SparseEmbedding map[string]interface{} `json:"sparse_embedding"`
			}
			sent := make(chan struct {
				path string
				body sentBody
			}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body sentBody
				_ = json.NewDecoder(r.Body).Decode(&body)
				sent <- struct {
					path string
					body sentBody
				}{r.URL.Path, body}
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/embeddings/multimodal" {
					fmt.Fprintf(w, `{"data":{"embedding":[0.1,0.2],"sparse_embedding":[{"index":1,"value":0.5}]},"model":%q,"object":"list","usage":{"prompt_tokens":5,"total_tokens":5}}`, tc.model)
					return
				}
				fmt.Fprintf(w, `{"data":[{"index":0,"object":"embedding","embedding":[0.1,0.2]}],"model":%q,"object":"list","usage":{"prompt_tokens":5,"total_tokens":5}}`, tc.model)
			}))
			defer server.Close()

			provider := newTestVolcengineCompatibleProvider(server.URL, tc.provider)
			first, second := "the sky", "is blue"
			instructions := "Target_modality: text.\nInstruction:Retrieve semantically similar text\nQuery:"
			request := &schemas.BifrostEmbeddingRequest{
				Provider: tc.provider,
				Model:    tc.model,
				Input: []schemas.EmbeddingInputItem{{Content: schemas.EmbeddingContent{
					{Type: schemas.EmbeddingContentPartTypeText, Text: &first},
					{Type: schemas.EmbeddingContentPartTypeText, Text: &second},
				}}},
				Params: &schemas.EmbeddingParameters{
					Instructions:    &instructions,
					SparseEmbedding: map[string]interface{}{"type": "enabled"},
				},
			}

			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			_, bifrostErr := provider.Embedding(ctx, schemas.Key{Value: *schemas.NewSecretVar("key")}, request)
			if bifrostErr != nil {
				t.Fatalf("Embedding returned error: %v", bifrostErr.Error)
			}
			got := <-sent
			if got.path != "/embeddings/multimodal" {
				t.Fatalf("path = %s, want /embeddings/multimodal for a single vision input with two text parts", got.path)
			}
			if len(got.body.Input) != 2 || got.body.Input[0].Text == nil || *got.body.Input[0].Text != first ||
				got.body.Input[1].Text == nil || *got.body.Input[1].Text != second {
				t.Errorf("input = %+v, want both text parts in order", got.body.Input)
			}
			if got.body.Instructions == nil || *got.body.Instructions != instructions {
				t.Errorf("instructions = %v, want them forwarded", got.body.Instructions)
			}
			if got.body.SparseEmbedding["type"] != "enabled" {
				t.Errorf("sparse_embedding = %v, want it forwarded", got.body.SparseEmbedding)
			}
		})
	}
}
