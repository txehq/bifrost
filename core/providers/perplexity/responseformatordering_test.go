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

// benchResponsesReply is a native /v1/responses reply in the shape of the Agent API's ResponsesUsage.
const benchResponsesReply = `{"id":"resp_1","object":"response","created_at":1,"model":"openai/gpt-5.4","status":"completed",` +
	`"output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed",` +
	`"content":[{"type":"output_text","text":"Hello, this is the answer text of the reply.","annotations":[]}]}],` +
	`"usage":{"input_tokens":20,"output_tokens":60,"total_tokens":80,` +
	`"input_tokens_details":{"cache_creation_input_tokens":0,"cache_read_input_tokens":0},` +
	`"tool_calls_details":{"web_search":{"invocation":2,"cost_usd":0.01},"fetch_url":{"invocation":1,"cost_usd":0.0005}},` +
	`"cost":{"currency":"USD","input_cost":0.00004,"output_cost":0.0006,"tool_calls_cost":0.0105,` +
	`"tool_calls_cost_details":{"web_search":0.01,"fetch_url":0.0005},"total_cost":0.01114}}}`

// BenchmarkResponsesReplyDecode is the decode cost of a native /v1/responses reply (TXE-6987). "generic"
// is what the shared handler did before: one decode into the unified response. "perplexity" is the
// provider's decoder, which also reads the usage object in Perplexity's own shape.
func BenchmarkResponsesReplyDecode(b *testing.B) {
	body := []byte(benchResponsesReply)
	b.Run("generic", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var response schemas.BifrostResponsesResponse
			if _, _, err := providerUtils.HandleProviderResponse(body, &response, nil, false, false); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("perplexity", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var response schemas.BifrostResponsesResponse
			if _, _, err := parseResponsesResponse(body, &response, nil, false, false); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkResponsesStreamEventDecode is the per-event decode cost of the native /v1/responses stream
// (TXE-6987), for a text delta, which every stream has many of, and for response.completed, which
// carries the usage once per stream. "generic" is the shared stream's decode.
func BenchmarkResponsesStreamEventDecode(b *testing.B) {
	events := map[string]string{
		"delta":     `{"type":"response.output_text.delta","sequence_number":3,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hello"}`,
		"completed": `{"type":"response.completed","sequence_number":4,"response":` + benchResponsesReply + `}`,
	}
	provider := &PerplexityProvider{}
	for _, name := range []string{"delta", "completed"} {
		event := events[name]
		body := []byte(event)
		b.Run(name+"/generic", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var response schemas.BifrostResponsesStreamResponse
				if err := sonic.UnmarshalString(event, &response); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(name+"/perplexity", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var response schemas.BifrostResponsesStreamResponse
				if _, _, err := provider.parseResponsesStreamEvent(body, &response, nil, false, false); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
