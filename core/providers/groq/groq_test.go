package groq_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/internal/llmtests"
	"github.com/capsohq/bifrost/core/livetest"
	"github.com/capsohq/bifrost/core/providers/groq"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

func TestGroq(t *testing.T) {
	t.Parallel()
	livetest.Require(t, "GROQ_API_KEY")

	client, ctx, cancel, err := llmtests.SetupTest()
	if err != nil {
		t.Fatalf("Error initializing test setup: %v", err)
	}
	defer cancel()
	defer client.Shutdown()

	testConfig := llmtests.ComprehensiveTestConfig{
		Provider:  schemas.Groq,
		ChatModel: "qwen/qwen3.8-27b",
		Fallbacks: []schemas.Fallback{
			{Provider: schemas.Groq, Model: "openai/gpt-oss-120b"},
		},
		TextModel: "qwen/qwen3.8-27b",
		TextCompletionFallbacks: []schemas.Fallback{
			{Provider: schemas.Groq, Model: "openai/gpt-oss-20b"},
		},
		EmbeddingModel:       "", // Groq doesn't support embedding
		ReasoningModel:       "openai/gpt-oss-120b",
		TranscriptionModel:   "whisper-large-v3",
		SpeechSynthesisModel: "canopylabs/orpheus-v1-english",
		Scenarios: llmtests.TestScenarios{
			TextCompletion:             false,
			TextCompletionStream:       false,
			SimpleChat:                 true,
			CompletionStream:           true,
			MultiTurnConversation:      true,
			ToolCalls:                  true,
			ToolCallsStreaming:         true,
			MultipleToolCalls:          true,
			MultipleToolCallsStreaming: true,
			End2EndToolCalling:         true,
			AutomaticFunctionCall:      true,
			ImageURL:                   false,
			ImageBase64:                false,
			MultipleImages:             false,
			FileBase64:                 false, // Not supported
			FileURL:                    false, // Not supported
			CompleteEnd2End:            true,
			Embedding:                  false,
			ListModels:                 true,
			Reasoning:                  true,
			Transcription:              true,
			SpeechSynthesis:            true,
		},
	}
	t.Run("GroqTests", func(t *testing.T) {
		llmtests.RunAllComprehensiveTests(t, client, ctx, testConfig)
	})
}

// A retrieve must land on /v1/models/{model} with the key's bearer token, map the
// provider's model object onto the Bifrost shape, and refuse a path-shaping model id
// before anything is dispatched.
func TestGroqModelRetrieve(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var gotPath, gotMethod, gotAuth string
	dispatched := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath, gotMethod, gotAuth = r.URL.Path, r.Method, r.Header.Get("Authorization")
		dispatched++
		mu.Unlock()
		id := strings.TrimPrefix(r.URL.Path, "/v1/models/")
		_, _ = w.Write([]byte(`{"id":"` + id + `","object":"model","created":1693721698,"owned_by":"Meta","context_window":131072}`))
	}))
	defer server.Close()

	provider, err := groq.NewGroqProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, DefaultRequestTimeoutInSeconds: 30},
	}, bifrost.NewNoOpLogger())
	require.NoError(t, err)

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	key := schemas.Key{Value: *schemas.NewSecretVar("gsk-test")}

	response, bifrostErr := provider.ModelRetrieve(ctx, key, &schemas.BifrostModelRetrieveRequest{Model: "llama-3.3-70b-versatile"})
	require.Nil(t, bifrostErr)

	mu.Lock()
	path, method, auth := gotPath, gotMethod, gotAuth
	mu.Unlock()
	require.Equal(t, "/v1/models/llama-3.3-70b-versatile", path)
	require.Equal(t, http.MethodGet, method)
	require.Equal(t, "Bearer gsk-test", auth)

	require.Equal(t, "groq/llama-3.3-70b-versatile", response.ID)
	require.Equal(t, schemas.Ptr("Meta"), response.OwnedBy)
	require.Equal(t, schemas.Ptr(131072), response.ContextLength)

	// Most current Groq models are namespaced ("openai/gpt-oss-120b"); each segment is a path segment.
	response, bifrostErr = provider.ModelRetrieve(ctx, key, &schemas.BifrostModelRetrieveRequest{Model: "openai/gpt-oss-120b"})
	require.Nil(t, bifrostErr)
	mu.Lock()
	path = gotPath
	mu.Unlock()
	require.Equal(t, "/v1/models/openai/gpt-oss-120b", path)
	require.Equal(t, "groq/openai/gpt-oss-120b", response.ID)

	mu.Lock()
	before := dispatched
	mu.Unlock()
	for _, invalid := range []string{"../models", "openai/../models", "openai/./gpt", "openai//gpt", "/openai", "openai/", "openai/%2e%2e", "openai/gpt?x=1", "openai\\gpt"} {
		_, bifrostErr = provider.ModelRetrieve(ctx, key, &schemas.BifrostModelRetrieveRequest{Model: invalid})
		require.NotNil(t, bifrostErr, invalid)
		require.Equal(t, schemas.Ptr(http.StatusBadRequest), bifrostErr.StatusCode, invalid)
	}
	mu.Lock()
	require.Equal(t, before, dispatched, "a path-shaping model id must never be dispatched upstream")
	mu.Unlock()
}

// TXE-7063 (contract C4, agreement three): Groq takes neither prompt_cache_retention nor
// prompt_cache_options, so a chat request carrying one is refused with 400
// caller_unsupported_parameter naming the field, unary and streamed, before any upstream call.
// prompt_cache_key alone is the documented exemption: Groq has no cache key, so it does not set
// the price there, and the request goes out without it.
func TestGroqChatRefusesPriceTierCacheFieldsWithNoUpstreamCall(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-groq","object":"chat.completion","created":1,"model":"llama-3.3-70b-versatile","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	provider, err := groq.NewGroqProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, DefaultRequestTimeoutInSeconds: 30},
	}, bifrost.NewNoOpLogger())
	require.NoError(t, err)
	key := schemas.Key{Value: *schemas.NewSecretVar("gsk-test")}
	request := func(params *schemas.ChatParameters) *schemas.BifrostChatRequest {
		return &schemas.BifrostChatRequest{
			Provider: schemas.Groq,
			Model:    "llama-3.3-70b-versatile",
			Input:    []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("hello")}}},
			Params:   params,
		}
	}
	requireRefused := func(t *testing.T, bifrostErr *schemas.BifrostError, field string) {
		t.Helper()
		require.NotNil(t, bifrostErr, "the request was sent")
		require.NotNil(t, bifrostErr.StatusCode)
		require.Equal(t, http.StatusBadRequest, *bifrostErr.StatusCode)
		require.Equal(t, schemas.ErrorTypeCallerUnsupportedParameter, bifrostErr.ExtraFields.ErrorType)
		require.Equal(t, field, bifrostErr.Error.Param)
	}

	for field, params := range map[string]func() *schemas.ChatParameters{
		schemas.FieldPromptCacheRetention: func() *schemas.ChatParameters {
			return &schemas.ChatParameters{PromptCacheKey: schemas.Ptr("tenant-42"), PromptCacheRetention: schemas.Ptr("24h")}
		},
		schemas.FieldPromptCacheOptions: func() *schemas.ChatParameters {
			return &schemas.ChatParameters{PromptCacheOptions: &schemas.PromptCacheOptions{TTL: schemas.Ptr("1h")}}
		},
	} {
		_, bifrostErr := provider.ChatCompletion(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), key, request(params()))
		requireRefused(t, bifrostErr, field)
		_, bifrostErr = provider.ChatCompletionStream(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline),
			func(_ *schemas.BifrostContext, r *schemas.BifrostResponse, e *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
				return r, e
			}, nil, key, request(params()))
		requireRefused(t, bifrostErr, field)
	}
	mu.Lock()
	require.Empty(t, bodies, "a refused request reached Groq")
	mu.Unlock()

	_, bifrostErr := provider.ChatCompletion(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), key,
		request(&schemas.ChatParameters{PromptCacheKey: schemas.Ptr("tenant-42")}))
	require.Nil(t, bifrostErr)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, bodies, 1)
	require.NotContains(t, bodies[0], "prompt_cache_key", "the exempt key goes out removed")
	require.Contains(t, bodies[0], `"hello"`)
}
