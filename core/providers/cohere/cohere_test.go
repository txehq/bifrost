package cohere_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/internal/llmtests"
	"github.com/capsohq/bifrost/core/providers/cohere"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

func TestCohere(t *testing.T) {
	t.Parallel()
	if strings.TrimSpace(os.Getenv("COHERE_API_KEY")) == "" {
		t.Skip("Skipping Cohere tests because COHERE_API_KEY is not set")
	}

	client, ctx, cancel, err := llmtests.SetupTest()
	if err != nil {
		t.Fatalf("Error initializing test setup: %v", err)
	}
	defer cancel()
	defer client.Shutdown()

	testConfig := llmtests.ComprehensiveTestConfig{
		Provider:                 schemas.Cohere,
		ChatModel:                "command-a-03-2025",
		VisionModel:              "command-a-vision-07-2025", // Cohere's latest vision model
		TextModel:                "",                         // Cohere focuses on chat
		EmbeddingModel:           "embed-v4.0",
		MultimodalEmbeddingModel: "embed-v4.0",
		EmbeddingTaskType:        "search_document",
		RerankModel:              "rerank-v3.5",
		ReasoningModel:           "command-a-reasoning-08-2025",
		Scenarios: llmtests.TestScenarios{
			TextCompletion:             false, // Not typical for Cohere
			SimpleChat:                 true,
			CompletionStream:           true,
			MultiTurnConversation:      true,
			ToolCalls:                  true,
			ToolCallsStreaming:         true,
			MultipleToolCalls:          true,
			MultipleToolCallsStreaming: true,
			End2EndToolCalling:         true,
			AutomaticFunctionCall:      true,  // May not support automatic
			ImageURL:                   false, // Supported by c4ai-aya-vision-8b model
			ImageBase64:                true,  // Supported by c4ai-aya-vision-8b model
			MultipleImages:             false, // Supported by c4ai-aya-vision-8b model
			FileBase64:                 false, // Not supported
			FileURL:                    false, // Not supported
			CompleteEnd2End:            false,
			Embedding:                  true,
			MultimodalEmbedding:        true,
			Rerank:                     true,
			Reasoning:                  true,
			ListModels:                 true,
			CountTokens:                true,
		},
	}

	t.Run("CohereTests", func(t *testing.T) {
		llmtests.RunAllComprehensiveTests(t, client, ctx, testConfig)
	})
}

// A retrieve must land on /v1/models/{model} with the key's bearer token, carry the
// deprecation flag through, surface an upstream 404, and refuse a path-shaping id.
func TestCohereModelRetrieve(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		mu.Unlock()
		if r.URL.Path == "/v1/models/nope" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"model 'nope' not found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"name":"command-r-08-2024","is_deprecated":true,"endpoints":["chat","generate"],"finetuned":false,"context_length":128000,"tokenizer_url":"https://example.invalid/tokenizer.json","default_endpoints":["chat"],"features":["tools","json_mode"]}`))
	}))
	defer server.Close()

	provider, err := cohere.NewCohereProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, DefaultRequestTimeoutInSeconds: 30},
	}, bifrost.NewNoOpLogger())
	require.NoError(t, err)

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	key := schemas.Key{Value: *schemas.NewSecretVar("co-test")}

	response, bifrostErr := provider.ModelRetrieve(ctx, key, &schemas.BifrostModelRetrieveRequest{Model: "command-r-08-2024"})
	require.Nil(t, bifrostErr)

	mu.Lock()
	path, auth := gotPath, gotAuth
	mu.Unlock()
	require.Equal(t, "/v1/models/command-r-08-2024", path)
	require.Equal(t, "Bearer co-test", auth)

	require.Equal(t, "cohere/command-r-08-2024", response.ID)
	require.Equal(t, schemas.Ptr(128000), response.ContextLength)
	require.True(t, response.IsDeprecated)
	require.Equal(t, []string{"chat", "generate"}, response.SupportedMethods)

	_, bifrostErr = provider.ModelRetrieve(ctx, key, &schemas.BifrostModelRetrieveRequest{Model: "nope"})
	require.NotNil(t, bifrostErr)
	require.Equal(t, schemas.Ptr(http.StatusNotFound), bifrostErr.StatusCode)

	_, bifrostErr = provider.ModelRetrieve(ctx, key, &schemas.BifrostModelRetrieveRequest{Model: "../models"})
	require.NotNil(t, bifrostErr)
	require.Equal(t, schemas.Ptr(http.StatusBadRequest), bifrostErr.StatusCode)
}

// TestToCohereResponsesRequest_InstructionsLeadSystemInput: top-level instructions and
// system/developer input both reach the system turn, instructions first (OpenAI places
// instructions ahead of input). Previously instructions were dropped whenever the input
// carried system content.
func TestToCohereResponsesRequest_InstructionsLeadSystemInput(t *testing.T) {
	user := schemas.ResponsesMessage{
		Type:    schemas.Ptr(schemas.ResponsesMessageTypeMessage),
		Role:    schemas.Ptr(schemas.ResponsesInputMessageRoleUser),
		Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr("hi")},
	}
	developer := schemas.ResponsesMessage{
		Type:    schemas.Ptr(schemas.ResponsesMessageTypeMessage),
		Role:    schemas.Ptr(schemas.ResponsesInputMessageRoleDeveloper),
		Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr("DEVELOPER_MUST_SURVIVE")},
	}

	tests := []struct {
		name         string
		instructions *string
		input        []schemas.ResponsesMessage
		want         string
	}{
		{"instructions and developer input both reach system, instructions first", schemas.Ptr("BASE_MUST_SURVIVE"), []schemas.ResponsesMessage{developer, user}, "BASE_MUST_SURVIVE\nDEVELOPER_MUST_SURVIVE"},
		{"instructions alone", schemas.Ptr("BASE_MUST_SURVIVE"), []schemas.ResponsesMessage{user}, "BASE_MUST_SURVIVE"},
		{"developer input alone", nil, []schemas.ResponsesMessage{developer, user}, "DEVELOPER_MUST_SURVIVE"},
		{"empty instructions add nothing", schemas.Ptr(""), []schemas.ResponsesMessage{developer, user}, "DEVELOPER_MUST_SURVIVE"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := cohere.ToCohereResponsesRequest(&schemas.BifrostResponsesRequest{
				Provider: schemas.Cohere,
				Model:    "command-a-03-2025",
				Input:    tc.input,
				Params:   &schemas.ResponsesParameters{Instructions: tc.instructions},
			})
			require.NoError(t, err)
			require.Len(t, out.Messages, 2)
			require.Equal(t, "system", out.Messages[0].Role)
			require.NotNil(t, out.Messages[0].Content)
			require.NotNil(t, out.Messages[0].Content.StringContent)
			require.Equal(t, tc.want, *out.Messages[0].Content.StringContent)
			require.Equal(t, "user", out.Messages[1].Role)
		})
	}
}

// Cohere's documented error statuses (docs.cohere.com/reference/errors), each with the flat
// {"id", "message"} body the v2 API returns. Chat, Responses, their streams and embeddings
// all parse errors in one place; each must carry the matching extra_fields.error_type and keep
// Cohere's status unchanged (contract C6).
func TestCohereErrorsCarryErrorType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status int
		want   schemas.ErrorType
	}{
		{400, schemas.ErrorTypeCallerInvalidRequest},
		{401, schemas.ErrorTypeProviderAuthFailed},
		{402, schemas.ErrorTypeProviderBilling},
		{404, schemas.ErrorTypeCallerModelUnknown},
		{422, schemas.ErrorTypeCallerInvalidRequest},
		{429, schemas.ErrorTypeProviderRateLimited},
		{498, schemas.ErrorTypeProviderAuthFailed}, // invalid token: the generic 4xx rule would blame the caller
		{499, schemas.ErrorTypeCallerCancelled},
		{500, schemas.ErrorTypeProviderServerError},
		{501, schemas.ErrorTypeProviderServerError},
		{503, schemas.ErrorTypeProviderOverloaded},
		{504, schemas.ErrorTypeProviderTimeout},
	}
	calls := map[string]func(p *cohere.CohereProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError{
		"chat": func(p *cohere.CohereProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.ChatCompletion(ctx, key, &schemas.BifrostChatRequest{
				Provider: schemas.Cohere,
				Model:    "command-a-03-2025",
				Input:    []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("hi")}}},
			})
			return err
		},
		"chat stream": func(p *cohere.CohereProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.ChatCompletionStream(ctx, func(_ *schemas.BifrostContext, r *schemas.BifrostResponse, e *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
				return r, e
			}, func(context.Context) {}, key, &schemas.BifrostChatRequest{
				Provider: schemas.Cohere,
				Model:    "command-a-03-2025",
				Input:    []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("hi")}}},
			})
			return err
		},
		"responses": func(p *cohere.CohereProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.Responses(ctx, key, cohereErrorResponsesRequest())
			return err
		},
		"responses stream": func(p *cohere.CohereProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.ResponsesStream(ctx, func(_ *schemas.BifrostContext, r *schemas.BifrostResponse, e *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
				return r, e
			}, func(context.Context) {}, key, cohereErrorResponsesRequest())
			return err
		},
		"embedding": func(p *cohere.CohereProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.Embedding(ctx, key, &schemas.BifrostEmbeddingRequest{
				Provider: schemas.Cohere,
				Model:    "embed-v4.0",
				Input: []schemas.EmbeddingInputItem{
					{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: schemas.Ptr("hi")}}},
				},
				Params: &schemas.EmbeddingParameters{TaskType: schemas.Ptr("search_document")},
			})
			return err
		},
	}
	for _, tc := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"id":"8a5e9f3c-1b2d-4e6f-9a0b-c1d2e3f4a5b6","message":"cohere says no"}`))
		}))
		provider, err := cohere.NewCohereProvider(&schemas.ProviderConfig{
			NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, DefaultRequestTimeoutInSeconds: 30},
		}, bifrost.NewNoOpLogger())
		require.NoError(t, err)
		for name, call := range calls {
			t.Run(fmt.Sprintf("%s/%d", name, tc.status), func(t *testing.T) {
				ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
				bifrostErr := call(provider, ctx, schemas.Key{Value: *schemas.NewSecretVar("co-test")})
				require.NotNil(t, bifrostErr, "status %d must be an error", tc.status)
				require.NotNil(t, bifrostErr.StatusCode)
				require.Equal(t, tc.status, *bifrostErr.StatusCode, "Cohere's status is returned unchanged")
				require.Equal(t, tc.want, bifrostErr.ExtraFields.ErrorType, "status %d", tc.status)
				require.Equal(t, "cohere says no", bifrostErr.Error.Message)
			})
		}
		server.Close()
	}
}

func cohereErrorResponsesRequest() *schemas.BifrostResponsesRequest {
	return &schemas.BifrostResponsesRequest{
		Provider: schemas.Cohere,
		Model:    "command-a-03-2025",
		Input: []schemas.ResponsesMessage{{
			Type:    schemas.Ptr(schemas.ResponsesMessageTypeMessage),
			Role:    schemas.Ptr(schemas.ResponsesInputMessageRoleUser),
			Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr("hi")},
		}},
	}
}
