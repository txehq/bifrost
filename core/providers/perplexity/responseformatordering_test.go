package perplexity

import (
	"testing"

	"github.com/bytedance/sonic"
	"github.com/capsohq/bifrost/core/internal/schemaorder"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

// TestResponseFormatOrderSurvivesPerplexityChat covers the pass-through
// providers: response_format is forwarded untouched, so the only thing that can
// corrupt it is the neutral decode/encode.
func TestResponseFormatOrderSurvivesPerplexityChat(t *testing.T) {
	var params schemas.ChatParameters
	require.NoError(t, schemas.Unmarshal([]byte(schemaorder.ChatBody), &params))

	out := ToPerplexityChatCompletionRequest(&schemas.BifrostChatRequest{
		Provider: schemas.Perplexity,
		Model:    "sonar",
		Input: []schemas.ChatMessage{{
			Role:    schemas.ChatMessageRoleUser,
			Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("assign it")},
		}},
		Params: &params,
	})
	require.NotNil(t, out)
	require.NotNil(t, out.ResponseFormat)

	raw, err := providerUtils.MarshalSorted(out)
	require.NoError(t, err)
	schemaorder.AssertPropertyOrder(t, string(raw))
	schemaorder.AssertSchemaKeyOrder(t, string(raw), "schema")
}

// TestResponseFormatSchemaBytesReachPerplexityVerbatim covers the pass-through
// providers: nothing may be re-encoded on a route that only forwards.
func TestResponseFormatSchemaBytesReachPerplexityVerbatim(t *testing.T) {
	var params schemas.ChatParameters
	require.NoError(t, schemas.Unmarshal([]byte(schemaorder.ByteExactChatBody), &params))

	out := ToPerplexityChatCompletionRequest(&schemas.BifrostChatRequest{
		Provider: schemas.Perplexity,
		Model:    "sonar",
		Input: []schemas.ChatMessage{{
			Role:    schemas.ChatMessageRoleUser,
			Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("assign it")},
		}},
		Params: &params,
	})
	require.NotNil(t, out)

	raw, err := providerUtils.MarshalSorted(out)
	require.NoError(t, err)
	schemaorder.AssertSchemaBytes(t, string(raw), "schema")
}

// TestResponseFormatOrderSurvivesPerplexityResponses covers the Responses method.
func TestResponseFormatOrderSurvivesPerplexityResponses(t *testing.T) {
	var params schemas.ResponsesParameters
	require.NoError(t, schemas.Unmarshal([]byte(schemaorder.ResponsesBody), &params))

	out := ToPerplexityResponsesRequest(&schemas.BifrostResponsesRequest{
		Provider: schemas.Perplexity,
		Model:    "sonar",
		Input: []schemas.ResponsesMessage{{
			Role:    schemas.Ptr(schemas.ResponsesInputMessageRoleUser),
			Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr("assign it")},
		}},
		Params: &params,
	})
	require.NotNil(t, out)

	raw, err := providerUtils.MarshalSorted(out)
	require.NoError(t, err)
	schemaorder.AssertPropertyOrder(t, string(raw))
}

// benchStreamFrame is a Perplexity content frame carrying its full usage, as every frame of a
// Sonar stream does.
const benchStreamFrame = `{"id":"pplx-1","object":"chat.completion.chunk","created":1,"model":"sonar-pro",` +
	`"choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}],` +
	`"usage":{"prompt_tokens":12,"completion_tokens":40,"total_tokens":52,"search_context_size":"high",` +
	`"citation_tokens":300,"num_search_queries":3,"reasoning_tokens":8,` +
	`"cost":{"input_tokens_cost":0.000036,"output_tokens_cost":0.0006,"reasoning_tokens_cost":0.000024,` +
	`"citation_tokens_cost":0.0006,"search_queries_cost":0.015,"request_cost":0.014,"total_cost":0.03026},` +
	`"tool_calls_details":{"web_search":{"invocation":3},"fetch_url":{"invocation":1}}}}`

// BenchmarkChatStreamFrameDecode is the per-frame cost of the Perplexity chat stream (TXE-6549).
// "generic" is what the shared OpenAI-compatible stream did for each frame before: one decode
// into the unified response. "perplexity" is the provider's own decode, which also converts
// usage.
func BenchmarkChatStreamFrameDecode(b *testing.B) {
	body := []byte(benchStreamFrame)
	b.Run("generic", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var response schemas.BifrostChatResponse
			if err := sonic.UnmarshalString(benchStreamFrame, &response); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("perplexity", func(b *testing.B) {
		provider := &PerplexityProvider{}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var response schemas.BifrostChatResponse
			if _, _, err := provider.parseChatStreamFrame(body, &response, nil, false, false); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkToBifrostChatResponseUsage is the non-streamed conversion of a response carrying the
// same usage; it shares toBifrostUsage with the stream.
func BenchmarkToBifrostChatResponseUsage(b *testing.B) {
	var frame PerplexityChatResponse
	if err := sonic.UnmarshalString(benchStreamFrame, &frame); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = frame.ToBifrostChatResponse("sonar-pro")
	}
}
