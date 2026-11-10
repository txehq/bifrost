package vertex

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/capsohq/bifrost/core/providers/gemini"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// parseVertexJSONLLines splits the JSONL output into one decoded map per line.
func parseVertexJSONLLines(t *testing.T, data []byte) []map[string]interface{} {
	t.Helper()
	var lines []map[string]interface{}
	for _, raw := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var m map[string]interface{}
		require.NoError(t, sonic.Unmarshal(raw, &m))
		lines = append(lines, m)
	}
	return lines
}

// TestVertexConvertRequestsToJSONL verifies inline batch requests are converted into Vertex's
// native {"request": {contents...}} shape (via the shared Gemini converter), and that each
// custom_id is carried in the request labels for round-trip correlation.
func TestVertexConvertRequestsToJSONL(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	t.Run("ConvertsOpenAIMessagesAndCarriesCustomIDLabel", func(t *testing.T) {
		requests := []schemas.BatchRequestItem{
			{
				CustomID: "req-1",
				Body: map[string]interface{}{
					"messages": []interface{}{
						map[string]interface{}{"role": "user", "content": "Hello"},
					},
				},
			},
		}

		data, err := vertexConvertRequestsToJSONL(ctx, schemas.Key{}, requests, "gemini-1.5-flash", "")
		require.NoError(t, err)

		lines := parseVertexJSONLLines(t, data)
		require.Len(t, lines, 1)

		request, ok := lines[0]["request"].(map[string]interface{})
		require.True(t, ok, "each line must be wrapped in a top-level \"request\" key")

		// OpenAI "messages" must have become Gemini "contents", not passed through verbatim.
		assert.NotContains(t, request, "messages")
		contents, ok := request["contents"].([]interface{})
		require.True(t, ok, "converted request must contain Gemini \"contents\"")
		require.Len(t, contents, 1)

		labels, ok := request["labels"].(map[string]interface{})
		require.True(t, ok, "custom_id must be carried in request labels")
		assert.Equal(t, "req-1", labels[vertexBatchCustomIDLabel])
	})

	t.Run("OmitsCustomIDLabelWhenNoCustomID", func(t *testing.T) {
		requests := []schemas.BatchRequestItem{
			{
				Body: map[string]interface{}{
					"messages": []interface{}{
						map[string]interface{}{"role": "user", "content": "Hi"},
					},
				},
			},
		}

		data, err := vertexConvertRequestsToJSONL(ctx, schemas.Key{}, requests, "gemini-1.5-flash", "")
		require.NoError(t, err)

		lines := parseVertexJSONLLines(t, data)
		require.Len(t, lines, 1)
		request := lines[0]["request"].(map[string]interface{})
		// A converted line still carries its url, so its result converts back; a line with
		// no custom_id carries no custom_id label.
		assert.Equal(t, map[string]interface{}{vertexBatchEndpointLabel: "chat_completions"}, request["labels"])
	})

	t.Run("PassesThroughNativeBodyVerbatim", func(t *testing.T) {
		requests := []schemas.BatchRequestItem{
			{
				CustomID: "req-native",
				Body: map[string]interface{}{
					"contents": []interface{}{
						map[string]interface{}{
							"role":  "user",
							"parts": []interface{}{map[string]interface{}{"text": "Native"}},
						},
					},
					"tools":         []interface{}{map[string]interface{}{"googleSearch": map[string]interface{}{}}},
					"cachedContent": "cachedContents/abc",
					"labels":        map[string]interface{}{"team": "research"},
				},
			},
		}

		data, err := vertexConvertRequestsToJSONL(ctx, schemas.Key{}, requests, "gemini-1.5-flash", "")
		require.NoError(t, err)

		lines := parseVertexJSONLLines(t, data)
		require.Len(t, lines, 1)
		request := lines[0]["request"].(map[string]interface{})

		// Native fields outside the Gemini batch struct must survive the conversion.
		assert.Contains(t, request, "tools")
		assert.Equal(t, "cachedContents/abc", request["cachedContent"])
		require.Contains(t, request, "contents")

		// Caller labels are preserved and the custom_id label is merged in.
		labels := request["labels"].(map[string]interface{})
		assert.Equal(t, "research", labels["team"])
		assert.Equal(t, "req-native", labels[vertexBatchCustomIDLabel])
	})

	t.Run("FallsBackToParamsWhenBodyNil", func(t *testing.T) {
		requests := []schemas.BatchRequestItem{
			{
				CustomID: "req-params",
				Params: map[string]interface{}{
					"contents": []interface{}{
						map[string]interface{}{
							"role":  "user",
							"parts": []interface{}{map[string]interface{}{"text": "Native"}},
						},
					},
				},
			},
		}

		data, err := vertexConvertRequestsToJSONL(ctx, schemas.Key{}, requests, "gemini-1.5-flash", "")
		require.NoError(t, err)

		lines := parseVertexJSONLLines(t, data)
		require.Len(t, lines, 1)
		request := lines[0]["request"].(map[string]interface{})
		contents, ok := request["contents"].([]interface{})
		require.True(t, ok)
		require.Len(t, contents, 1)
	})

	t.Run("EmitsOneLinePerRequest", func(t *testing.T) {
		requests := []schemas.BatchRequestItem{
			{CustomID: "a", Body: map[string]interface{}{"messages": []interface{}{map[string]interface{}{"role": "user", "content": "1"}}}},
			{CustomID: "b", Body: map[string]interface{}{"messages": []interface{}{map[string]interface{}{"role": "user", "content": "2"}}}},
		}

		data, err := vertexConvertRequestsToJSONL(ctx, schemas.Key{}, requests, "gemini-1.5-flash", "")
		require.NoError(t, err)

		lines := parseVertexJSONLLines(t, data)
		require.Len(t, lines, 2)
		assert.Equal(t, "a", lines[0]["request"].(map[string]interface{})["labels"].(map[string]interface{})[vertexBatchCustomIDLabel])
		assert.Equal(t, "b", lines[1]["request"].(map[string]interface{})["labels"].(map[string]interface{})[vertexBatchCustomIDLabel])
	})

	t.Run("ErrorsWhenItemHasNoBody", func(t *testing.T) {
		requests := []schemas.BatchRequestItem{{CustomID: "empty"}}
		_, err := vertexConvertRequestsToJSONL(ctx, schemas.Key{}, requests, "gemini-1.5-flash", "")
		require.Error(t, err)
	})

	t.Run("AnthropicConvertsOpenAIBodyAndUsesNativeCustomID", func(t *testing.T) {
		requests := []schemas.BatchRequestItem{
			{
				CustomID: "req-1",
				Body: map[string]interface{}{
					// A system-role message marks this as an OpenAI body needing conversion.
					"messages": []interface{}{
						map[string]interface{}{"role": "system", "content": "You are helpful"},
						map[string]interface{}{"role": "user", "content": "Hello"},
					},
					"max_tokens": 50,
				},
			},
		}

		data, err := vertexConvertRequestsToJSONL(ctx, schemas.Key{}, requests, "claude-3-5-haiku", "")
		require.NoError(t, err)

		lines := parseVertexJSONLLines(t, data)
		require.Len(t, lines, 1)

		// custom_id is a native top-level field for Claude batch, not Gemini labels.
		assert.Equal(t, "req-1", lines[0]["custom_id"])

		request, ok := lines[0]["request"].(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, "vertex-2023-10-16", request["anthropic_version"])
		assert.Contains(t, request, "messages")
		assert.NotContains(t, request, "labels")
		assert.NotContains(t, request, "model") // model lives at the job level
	})

	t.Run("AnthropicConvertsOpenAIOnlyFields", func(t *testing.T) {
		// Keys OpenAI has and Anthropic does not: passing them through verbatim makes
		// Claude reject every record, so each one must force conversion on its own.
		for field, value := range map[string]interface{}{
			"stop":             []interface{}{"END"},
			"seed":             42,
			"user":             "u1",
			"store":            true,
			"reasoning_effort": "low",
		} {
			requests := []schemas.BatchRequestItem{
				{
					CustomID: "req-" + field,
					Body: map[string]interface{}{
						"messages": []interface{}{map[string]interface{}{"role": "user", "content": "Hello"}},
						// Comfortably above the thinking floor so reasoning_effort converts cleanly.
						"max_tokens": 4096,
						field:        value,
					},
				},
			}

			data, err := vertexConvertRequestsToJSONL(ctx, schemas.Key{}, requests, "claude-3-5-haiku", "")
			require.NoError(t, err)

			lines := parseVertexJSONLLines(t, data)
			require.Len(t, lines, 1)
			request := lines[0]["request"].(map[string]interface{})
			assert.NotContains(t, request, field, "%s must not survive into the Anthropic body", field)
			assert.Equal(t, "vertex-2023-10-16", request["anthropic_version"])
		}

		// A bare string tool_choice is OpenAI-only; Anthropic's is always an object.
		requests := []schemas.BatchRequestItem{
			{
				CustomID: "req-tool-choice",
				Body: map[string]interface{}{
					"messages":    []interface{}{map[string]interface{}{"role": "user", "content": "Hello"}},
					"max_tokens":  50,
					"tool_choice": "auto",
				},
			},
		}
		data, err := vertexConvertRequestsToJSONL(ctx, schemas.Key{}, requests, "claude-3-5-haiku", "")
		require.NoError(t, err)
		lines := parseVertexJSONLLines(t, data)
		require.Len(t, lines, 1)
		request := lines[0]["request"].(map[string]interface{})
		assert.NotEqual(t, "auto", request["tool_choice"])
	})

	t.Run("AnthropicKeepsSharedFieldsAsNative", func(t *testing.T) {
		// stream/metadata/service_tier exist on both APIs, so they must not be read as
		// OpenAI markers — that would send a native body through the lossy converter.
		requests := []schemas.BatchRequestItem{
			{
				CustomID: "req-shared",
				Body: map[string]interface{}{
					"messages":       []interface{}{map[string]interface{}{"role": "user", "content": "Hello"}},
					"max_tokens":     50,
					"metadata":       map[string]interface{}{"user_id": "u1"},
					"service_tier":   "auto",
					"stop_sequences": []interface{}{"END"},
				},
			},
		}

		data, err := vertexConvertRequestsToJSONL(ctx, schemas.Key{}, requests, "claude-3-5-haiku", "")
		require.NoError(t, err)

		lines := parseVertexJSONLLines(t, data)
		require.Len(t, lines, 1)
		request := lines[0]["request"].(map[string]interface{})
		assert.Equal(t, "auto", request["service_tier"])
		assert.Equal(t, []interface{}{"END"}, request["stop_sequences"])
	})

	t.Run("AnthropicPassesThroughNativeBodyAndDropsModel", func(t *testing.T) {
		requests := []schemas.BatchRequestItem{
			{
				CustomID: "req-native",
				Body: map[string]interface{}{
					"messages":   []interface{}{map[string]interface{}{"role": "user", "content": "Hi"}},
					"system":     "be brief",
					"max_tokens": 64,
					"model":      "claude-3-5-haiku",
				},
			},
		}

		data, err := vertexConvertRequestsToJSONL(ctx, schemas.Key{}, requests, "claude-3-5-haiku", "")
		require.NoError(t, err)

		lines := parseVertexJSONLLines(t, data)
		require.Len(t, lines, 1)
		assert.Equal(t, "req-native", lines[0]["custom_id"])

		request := lines[0]["request"].(map[string]interface{})
		assert.Equal(t, "vertex-2023-10-16", request["anthropic_version"])
		assert.Equal(t, "be brief", request["system"])
		assert.EqualValues(t, 64, request["max_tokens"])
		assert.NotContains(t, request, "model")
	})

	t.Run("AnthropicPreservesExistingAnthropicVersion", func(t *testing.T) {
		requests := []schemas.BatchRequestItem{
			{
				CustomID: "req-ver",
				Body: map[string]interface{}{
					"messages":          []interface{}{map[string]interface{}{"role": "user", "content": "Hi"}},
					"max_tokens":        16,
					"anthropic_version": "vertex-2023-10-16",
				},
			},
		}

		data, err := vertexConvertRequestsToJSONL(ctx, schemas.Key{}, requests, "claude-3-5-haiku", "")
		require.NoError(t, err)

		lines := parseVertexJSONLLines(t, data)
		request := lines[0]["request"].(map[string]interface{})
		assert.Equal(t, "vertex-2023-10-16", request["anthropic_version"])
	})
}

// TestToVertexBatchCreateRequestPublisher verifies the batch job model is resolved to the
// correct publisher path per model family, and that already-qualified paths pass through.
func TestToVertexBatchCreateRequestPublisher(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	cases := []struct {
		name     string
		model    string
		expected string
	}{
		{"AnthropicBareName", "claude-3-5-haiku", "publishers/anthropic/models/claude-3-5-haiku"},
		{"GeminiBareName", "gemini-1.5-flash", "publishers/google/models/gemini-1.5-flash"},
		{"AlreadyQualifiedPassesThrough", "publishers/anthropic/models/claude-3-5-haiku", "publishers/anthropic/models/claude-3-5-haiku"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &schemas.BifrostBatchCreateRequest{Model: schemas.Ptr(tc.model)}
			out, err := ToVertexBatchCreateRequest(ctx, req, "job", "gs://in/input.jsonl", "gs://out/")
			require.NoError(t, err)
			assert.Equal(t, tc.expected, out.Model)
		})
	}
}

// TestToBifrostBatchCreateRequestModelLookup verifies the native batchPredictionJobs body's
// model is reduced to the bare id that key/catalog/pricing lookups match on, while the
// caller's exact publisher path is preserved for a rebuilt job body.
func TestToBifrostBatchCreateRequestModelLookup(t *testing.T) {
	cases := []struct {
		name      string
		model     string
		wantModel string
		wantExtra interface{} // nil when no publisher path was stripped
	}{
		{
			name:      "PublisherPathReducedToBareID",
			model:     "publishers/anthropic/models/claude-3-5-haiku",
			wantModel: "claude-3-5-haiku",
			wantExtra: "publishers/anthropic/models/claude-3-5-haiku",
		},
		{
			name:      "BareNamePassesThrough",
			model:     "claude-3-5-haiku",
			wantModel: "claude-3-5-haiku",
		},
		{
			// Fine-tuned models are addressed by resource path, not publisher path.
			name:      "FineTunedResourcePathPassesThrough",
			model:     "projects/p/locations/us-central1/models/1234",
			wantModel: "projects/p/locations/us-central1/models/1234",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := &VertexBatchPredictionJob{Model: tc.model}
			req := ToBifrostBatchCreateRequest(job)

			require.NotNil(t, req.Model)
			assert.Equal(t, tc.wantModel, *req.Model)
			if tc.wantExtra == nil {
				assert.NotContains(t, req.ExtraParams, "model")
			} else {
				assert.Equal(t, tc.wantExtra, req.ExtraParams["model"])
			}
		})
	}

	t.Run("RebuiltBodyReEmitsCallerPublisherPath", func(t *testing.T) {
		// A publisher Bifrost cannot re-derive from the model family: the reduced id must not
		// come back as publishers/google/... when the outbound body is rebuilt.
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		job := &VertexBatchPredictionJob{Model: "publishers/meta/models/llama-3.1-405b-instruct-maas"}
		req := ToBifrostBatchCreateRequest(job)
		assert.Equal(t, "llama-3.1-405b-instruct-maas", *req.Model)

		out, err := ToVertexBatchCreateRequest(ctx, req, "job", "gs://in/input.jsonl", "gs://out/")
		require.NoError(t, err)
		body, err := providerUtils.MergeExtraParamsIntoJSON(mustMarshal(t, out), out.GetExtraParams())
		require.NoError(t, err)

		var decoded map[string]interface{}
		require.NoError(t, sonic.Unmarshal(body, &decoded))
		assert.Equal(t, "publishers/meta/models/llama-3.1-405b-instruct-maas", decoded["model"])
	})
}

// mustMarshal serializes v to JSON bytes, failing the test if serialization fails.
func mustMarshal(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := sonic.Marshal(v)
	require.NoError(t, err)
	return b
}

// TestVertexNormalizeBatchUsage_ThinkingTokens verifies that vertexNormalizeBatchUsage
// accounts for thinking tokens by adding thoughtsTokenCount to completion_tokens
// and creating completion_tokens_details.reasoning_tokens.
func TestVertexNormalizeBatchUsage_ThinkingTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		body                 map[string]any
		wantPromptTokens     int
		wantCompletionTokens int
		wantTotalTokens      int
		wantReasoningTokens  int
		wantCachedTokens     int
	}{
		{
			name: "WithThinkingTokens",
			body: map[string]any{
				"usageMetadata": map[string]any{
					"promptTokenCount":     100,
					"candidatesTokenCount": 50,
					"thoughtsTokenCount":   400,
					"totalTokenCount":      550,
				},
			},
			wantPromptTokens:     100,
			wantCompletionTokens: 450, // 50 candidates + 400 thoughts
			wantTotalTokens:      550,
			wantReasoningTokens:  400,
		},
		{
			name: "WithThinkingTokensAndCachedTokens",
			body: map[string]any{
				"usageMetadata": map[string]any{
					"promptTokenCount":        150,
					"candidatesTokenCount":    75,
					"thoughtsTokenCount":      225,
					"totalTokenCount":         450,
					"cachedContentTokenCount": 50,
				},
			},
			wantPromptTokens:     150,
			wantCompletionTokens: 300, // 75 candidates + 225 thoughts
			wantTotalTokens:      450,
			wantReasoningTokens:  225,
			wantCachedTokens:     50,
		},
		{
			name: "ZeroThinkingTokensUnchanged",
			body: map[string]any{
				"usageMetadata": map[string]any{
					"promptTokenCount":     15,
					"candidatesTokenCount": 25,
					"thoughtsTokenCount":   0,
					"totalTokenCount":      40,
				},
			},
			wantPromptTokens:     15,
			wantCompletionTokens: 25,
			wantTotalTokens:      40,
			wantReasoningTokens:  0,
		},
		{
			name: "NoUsageMetadataPassesThrough",
			body: map[string]any{
				"response": "ok",
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			normalized := vertexNormalizeBatchUsage(tt.body)
			if tt.wantPromptTokens == 0 && tt.wantCompletionTokens == 0 {
				assert.NotContains(t, normalized, "usage")
				return
			}

			usage, ok := normalized["usage"].(map[string]any)
			require.True(t, ok)
			assert.EqualValues(t, tt.wantPromptTokens, usage["prompt_tokens"])
			assert.EqualValues(t, tt.wantCompletionTokens, usage["completion_tokens"])
			assert.EqualValues(t, tt.wantTotalTokens, usage["total_tokens"])

			if tt.wantReasoningTokens > 0 {
				details, ok := usage["completion_tokens_details"].(map[string]any)
				require.True(t, ok, "completion_tokens_details should be present")
				assert.EqualValues(t, tt.wantReasoningTokens, details["reasoning_tokens"])
			} else {
				assert.Nil(t, usage["completion_tokens_details"])
			}

			if tt.wantCachedTokens > 0 {
				details, ok := usage["prompt_tokens_details"].(map[string]any)
				require.True(t, ok, "prompt_tokens_details should be present")
				assert.EqualValues(t, tt.wantCachedTokens, details["cached_tokens"])
			}
		})
	}
}

// An OpenAI-shape inline line reaches Vertex as the request the Vertex sync route sends for
// the same body (contract C8, BF17): the line is built by the sync route's own builder, so
// the two are compared here. Before this, the line kept only messages and dropped
// max_tokens, temperature, response_format, OpenAI-shape tools and the rest.
func TestVertexBatchLinesAreTheSyncRequest(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	parse := func(line string) schemas.BatchRequestItem {
		var item schemas.BatchRequestItem
		require.NoError(t, sonic.UnmarshalString(line, &item))
		return item
	}
	lineRequest := func(data []byte) map[string]interface{} {
		lines := parseVertexJSONLLines(t, data)
		require.Len(t, lines, 1)
		request, ok := lines[0]["request"].(map[string]interface{})
		require.True(t, ok)
		return request
	}
	asMap := func(encoded []byte) map[string]interface{} {
		out := map[string]interface{}{}
		require.NoError(t, sonic.Unmarshal(encoded, &out))
		return out
	}

	t.Run("chat line", func(t *testing.T) {
		item := parse(`{"custom_id":"line-1","method":"POST","url":"/v1/chat/completions","body":{"model":"gemini-2.5-flash","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"What is the capital of France?"}],"max_tokens":256,"temperature":0.2,"top_p":0.9,"stop":["END"],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}],"tool_choice":"auto"}}`)
		data, err := vertexConvertRequestsToJSONL(ctx, schemas.Key{}, []schemas.BatchRequestItem{item}, "gemini-2.5-flash", schemas.BatchEndpointChatCompletions)
		require.NoError(t, err)
		got := lineRequest(data)

		chatRequest, err := item.ToBifrostChatRequest(schemas.Vertex, "gemini-2.5-flash")
		require.NoError(t, err)
		syncBody, err := vertexGeminiChatRequestBody(ctx, schemas.Key{}, chatRequest)
		require.NoError(t, err)
		want := asMap(syncBody.RawBody)

		assert.Equal(t, map[string]interface{}{"bifrost_custom_id": "line-1", "bifrost_endpoint": "chat_completions"}, got["labels"])
		delete(got, "labels")
		assert.Equal(t, want, got, "the batch line, labels aside, must be the sync request")

		generationConfig := got["generationConfig"].(map[string]interface{})
		assert.EqualValues(t, 256, generationConfig["maxOutputTokens"])
		assert.EqualValues(t, 0.2, generationConfig["temperature"])
		assert.EqualValues(t, 0.9, generationConfig["topP"])
		assert.Equal(t, []interface{}{"END"}, generationConfig["stopSequences"])
		declarations := got["tools"].([]interface{})[0].(map[string]interface{})["functionDeclarations"].([]interface{})
		assert.Equal(t, "lookup", declarations[0].(map[string]interface{})["name"])
		assert.NotNil(t, got["systemInstruction"])
	})

	t.Run("structured output line", func(t *testing.T) {
		item := parse(`{"custom_id":"line-2","url":"/v1/chat/completions","body":{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"Name a city."}],"max_completion_tokens":64,"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}}}`)
		data, err := vertexConvertRequestsToJSONL(ctx, schemas.Key{}, []schemas.BatchRequestItem{item}, "gemini-2.5-flash", schemas.BatchEndpointChatCompletions)
		require.NoError(t, err)
		generationConfig := lineRequest(data)["generationConfig"].(map[string]interface{})
		assert.EqualValues(t, 64, generationConfig["maxOutputTokens"])
		assert.Equal(t, "application/json", generationConfig["responseMimeType"])
		assert.True(t, generationConfig["responseSchema"] != nil || generationConfig["responseJsonSchema"] != nil, "the response schema is missing: %v", generationConfig)
	})

	t.Run("image generation line", func(t *testing.T) {
		item := parse(`{"custom_id":"img-1","url":"/v1/images/generations","body":{"model":"gemini-2.5-flash-image","prompt":"A lighthouse at dusk","aspect_ratio":"16:9"}}`)
		data, err := vertexConvertRequestsToJSONL(ctx, schemas.Key{}, []schemas.BatchRequestItem{item}, "gemini-2.5-flash-image", schemas.BatchEndpointImageGeneration)
		require.NoError(t, err)
		got := lineRequest(data)

		imageRequest, err := item.ToBifrostImageGenerationRequest(schemas.Vertex, "gemini-2.5-flash-image")
		require.NoError(t, err)
		syncBody, err := vertexGeminiImageGenerationRequestBody(imageRequest)
		require.NoError(t, err)

		assert.Equal(t, map[string]interface{}{"bifrost_custom_id": "img-1", "bifrost_endpoint": "images_generations"}, got["labels"])
		delete(got, "labels")
		assert.Equal(t, asMap(syncBody.RawBody), got)
	})

	t.Run("a line the batch cannot run is an error naming the line", func(t *testing.T) {
		for name, tc := range map[string]struct{ line, param, text string }{
			"embeddings url": {`{"custom_id":"e-1","url":"/v1/embeddings","body":{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}}`, "url", "/v1/embeddings"},
			"another model":  {`{"custom_id":"l-1","url":"/v1/chat/completions","body":{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hi"}]}}`, "body.model", "gemini-2.5-pro"},
			"no body":        {`{"custom_id":"l-1","url":"/v1/chat/completions"}`, "body", "body is required"},
			"another url":    {`{"custom_id":"i-1","url":"/v1/images/generations","body":{"model":"gemini-2.5-flash","prompt":"a cat"}}`, "url", "a batch runs one url"},
		} {
			ok := parse(`{"custom_id":"ok","url":"/v1/chat/completions","body":{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}}`)
			_, err := vertexConvertRequestsToJSONL(ctx, schemas.Key{}, []schemas.BatchRequestItem{ok, parse(tc.line)}, "gemini-2.5-flash", schemas.BatchEndpointChatCompletions)
			var lineErr *schemas.BatchLineError
			require.ErrorAs(t, err, &lineErr, name)
			assert.Equal(t, 1, lineErr.Index, name)
			assert.Equal(t, tc.param, lineErr.Param, name)
			assert.Contains(t, lineErr.Message, tc.text, name)
		}
	})
}

// The batch's metadata reaches the job as labels, a field of the job body in its own right:
// present whether or not extra params are passed through, and what the list filters on
// (contract C8 agreement two, BF19).
func TestVertexBatchMetadataBecomesJobLabels(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	t.Run("metadata and a labels field merge into labels", func(t *testing.T) {
		extraParams := map[string]interface{}{
			"labels":          map[string]interface{}{"team": "tokun", "tokun_batch_id": "from-labels"},
			"modelParameters": map[string]interface{}{"temperature": 0.1},
		}
		request := &schemas.BifrostBatchCreateRequest{
			Model:       schemas.Ptr("gemini-2.5-flash"),
			Metadata:    map[string]string{"tokun_batch_id": "batch_tk_1", "chunk": "3", "job_name": "my job"},
			ExtraParams: extraParams,
		}
		job, err := ToVertexBatchCreateRequest(ctx, request, "my job", "gs://in/input.jsonl", "gs://out/")
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"tokun_batch_id": "from-labels", "chunk": "3", "team": "tokun"}, job.Labels, "job_name is the display name, not a label; a labels field wins over metadata")
		assert.NotContains(t, job.ExtraParams, "labels", "labels must not be merged a second time as an extra param")
		assert.Contains(t, job.ExtraParams, "modelParameters")
		assert.Contains(t, extraParams, "labels", "the caller's map is not modified")

		// The encoded job body carries labels with no extra-params passthrough at all.
		encoded, err := providerUtils.MarshalConvertedRequestBody(job, false)
		require.NoError(t, err)
		var body map[string]interface{}
		require.NoError(t, sonic.Unmarshal(encoded, &body))
		assert.Equal(t, map[string]interface{}{"tokun_batch_id": "from-labels", "chunk": "3", "team": "tokun"}, body["labels"])
		assert.NotContains(t, body, "modelParameters", "other extra params still need the passthrough switch")
	})

	t.Run("labels supplied as a typed string map are accepted", func(t *testing.T) {
		// The Go SDK and the genai integration's own converter supply map[string]string.
		job := &VertexBatchPredictionJob{
			DisplayName: "round trip",
			Model:       "publishers/google/models/gemini-2.5-flash",
			InputConfig: VertexBatchInputConfig{InstancesFormat: "jsonl", GcsSource: &VertexGcsSource{Uris: []string{"gs://in/input.jsonl"}}},
			OutputConfig: VertexBatchOutputConfig{
				PredictionsFormat: "jsonl",
				GcsDestination:    &VertexGcsDestination{OutputUriPrefix: "gs://out/"},
			},
			Labels: map[string]string{"team": "tokun", "tokun_batch_id": "batch_tk_1"},
		}
		inbound := ToBifrostBatchCreateRequest(job)
		require.NotNil(t, inbound)
		outbound, err := ToVertexBatchCreateRequest(ctx, inbound, "round trip", "gs://in/input.jsonl", "gs://out/")
		require.NoError(t, err, "the labels the inbound converter produced must convert back")
		assert.Equal(t, map[string]string{"team": "tokun", "tokun_batch_id": "batch_tk_1"}, outbound.Labels)

		_, err = ToVertexBatchCreateRequest(ctx, &schemas.BifrostBatchCreateRequest{
			Model:       schemas.Ptr("gemini-2.5-flash"),
			ExtraParams: map[string]interface{}{"labels": map[string]string{"team": "Not A Label"}},
		}, "job", "gs://in/input.jsonl", "gs://out/")
		var labelErr *vertexLabelError
		require.ErrorAs(t, err, &labelErr)
		assert.Equal(t, "labels.team", labelErr.Param)
	})

	t.Run("no metadata is no labels field", func(t *testing.T) {
		job, err := ToVertexBatchCreateRequest(ctx, &schemas.BifrostBatchCreateRequest{Model: schemas.Ptr("gemini-2.5-flash")}, "job", "gs://in/input.jsonl", "gs://out/")
		require.NoError(t, err)
		assert.Nil(t, job.Labels)
	})

	t.Run("metadata Vertex cannot hold as a label is an error naming the entry", func(t *testing.T) {
		for name, tc := range map[string]struct {
			metadata    map[string]string
			extraParams map[string]interface{}
			wantParam   string
		}{
			"uppercase value":    {map[string]string{"tokun_batch_id": "Batch_TK_1"}, nil, "metadata.tokun_batch_id"},
			"value with a slash": {map[string]string{"tokun_batch_id": "batch/1"}, nil, "metadata.tokun_batch_id"},
			"value too long":     {map[string]string{"tokun_batch_id": strings.Repeat("a", 64)}, nil, "metadata.tokun_batch_id"},
			"key not a label":    {map[string]string{"Tokun-Batch": "b1"}, nil, "metadata.Tokun-Batch"},
			"labels not strings": {nil, map[string]interface{}{"labels": map[string]interface{}{"team": 7}}, "labels.team"},
			"labels not a map":   {nil, map[string]interface{}{"labels": "team=tokun"}, "labels"},
		} {
			_, err := ToVertexBatchCreateRequest(ctx, &schemas.BifrostBatchCreateRequest{Model: schemas.Ptr("gemini-2.5-flash"), Metadata: tc.metadata, ExtraParams: tc.extraParams}, "job", "gs://in/input.jsonl", "gs://out/")
			var labelErr *vertexLabelError
			require.ErrorAs(t, err, &labelErr, name)
			assert.Equal(t, tc.wantParam, labelErr.Param, name)
		}
	})

	t.Run("the job's labels come back as the batch's metadata", func(t *testing.T) {
		batch := vertexBatchJobToBifrost(&VertexBatchPredictionJob{
			Name:        "projects/p/locations/us-central1/batchPredictionJobs/123",
			DisplayName: "my job",
			State:       "JOB_STATE_RUNNING",
			Labels:      map[string]string{"tokun_batch_id": "batch_tk_1"},
		})
		assert.Equal(t, "projects/p/locations/us-central1/batchPredictionJobs/123", batch.ID)
		assert.Equal(t, map[string]string{"tokun_batch_id": "batch_tk_1"}, batch.Metadata)
		assert.Equal(t, schemas.BatchStatusInProgress, batch.Status)
	})
}

// The list sends the caller's filter as Vertex's own filter expression, so a batch whose
// create response was lost is found by its label.
func TestVertexBatchListQueryCarriesTheFilter(t *testing.T) {
	query := vertexBatchListQuery(&schemas.BifrostBatchListRequest{Limit: 5, Filter: schemas.Ptr(" labels.tokun_batch_id=batch_tk_1 "), PageToken: schemas.Ptr("next")})
	assert.Equal(t, "labels.tokun_batch_id=batch_tk_1", query.Get("filter"))
	assert.Equal(t, "5", query.Get("pageSize"))
	assert.Equal(t, "next", query.Get("pageToken"))
	assert.Contains(t, query.Encode(), "filter=labels.tokun_batch_id%3Dbatch_tk_1")

	plain := vertexBatchListQuery(&schemas.BifrostBatchListRequest{})
	assert.False(t, plain.Has("filter"), "no filter is no filter parameter")
	assert.Equal(t, "20", plain.Get("pageSize"))
}

// Lines of a Vertex predictions file, as batch prediction writes them: the request echoed
// with its labels, the model's native response, and for a failed record the error in status.
const (
	vertexOutputLineChat    = `{"status":"","processed_time":"2026-11-10T09:13:16.826+00:00","request":{"contents":[{"role":"user","parts":[{"text":"What is the capital of France?"}]}],"generationConfig":{"maxOutputTokens":256},"labels":{"bifrost_custom_id":"line-1","bifrost_endpoint":"chat_completions"}},"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"Paris."}]},"finishReason":"STOP","avgLogprobs":-0.1,"safetyRatings":[{"category":"HARM_CATEGORY_HARASSMENT","probability":"NEGLIGIBLE"}]}],"modelVersion":"gemini-2.5-flash","responseId":"resp-1","usageMetadata":{"promptTokenCount":120,"candidatesTokenCount":60,"totalTokenCount":480,"cachedContentTokenCount":40,"thoughtsTokenCount":300,"promptTokensDetails":[{"modality":"TEXT","tokenCount":120}],"candidatesTokensDetails":[{"modality":"TEXT","tokenCount":60}]}}}`
	vertexOutputLineImage   = `{"status":"","processed_time":"2026-11-10T09:13:17.100+00:00","request":{"contents":[{"role":"user","parts":[{"text":"A lighthouse at dusk"}]}],"labels":{"bifrost_custom_id":"img-1","bifrost_endpoint":"images_generations"}},"response":{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgoAAAANSUhEUg=="}}]},"finishReason":"STOP"}],"modelVersion":"gemini-2.5-flash-image","usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":1290,"totalTokenCount":1298,"promptTokensDetails":[{"modality":"TEXT","tokenCount":8}],"candidatesTokensDetails":[{"modality":"IMAGE","tokenCount":1290}]}}}`
	vertexOutputLineFailed  = `{"status":"Bad Request: {\"error\": {\"code\": 400, \"message\": \"Unable to submit request because it has a maxOutputTokens value of 99999 but the supported range is from 1 to 65536.\", \"status\": \"INVALID_ARGUMENT\"}}","processed_time":"2026-11-10T09:13:18.000+00:00","request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"labels":{"bifrost_custom_id":"line-3","bifrost_endpoint":"chat_completions"}},"response":{}}`
	vertexOutputLineClaude  = `{"custom_id":"claude-1","request":{"anthropic_version":"vertex-2023-10-16","messages":[{"role":"user","content":"hi"}],"max_tokens":32},"response":{"id":"msg_1","type":"message","role":"assistant","model":"claude-3-5-haiku","content":[{"type":"text","text":"Hello."}],"stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":4}}}`
	vertexOutputLineNoLabel = `{"status":"","request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]},"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello."}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2,"totalTokenCount":3}}}`
)

// Results convert each predictions line back to the OpenAI shape of the line's url with
// status and the sync operation's unified usage; a failed record carries Vertex's error; a
// line that cannot be parsed is an error for the caller to count (contract C8, BF18).
func TestVertexBatchOutputLineToResultItem(t *testing.T) {
	t.Run("chat line", func(t *testing.T) {
		item, endpoint, err := vertexBatchOutputLineToResultItem([]byte(vertexOutputLineChat), false, true)
		require.NoError(t, err)
		assert.Equal(t, schemas.BatchEndpointChatCompletions, endpoint)
		assert.Equal(t, "line-1", item.CustomID)
		assert.Equal(t, schemas.BatchResultStatusSucceeded, item.Status)
		assert.Equal(t, 200, item.Response.StatusCode)
		body := item.Response.Body
		assert.Equal(t, "chat.completion", body["object"])
		assert.Equal(t, "gemini-2.5-flash", body["model"])
		choice := body["choices"].([]interface{})[0].(map[string]interface{})
		assert.Equal(t, "Paris.", choice["message"].(map[string]interface{})["content"])
		assert.Equal(t, "stop", choice["finish_reason"])
		usage := body["usage"].(map[string]interface{})
		assert.EqualValues(t, 120, usage["prompt_tokens"])
		assert.EqualValues(t, 360, usage["completion_tokens"], "candidates plus thoughts, as on the sync route")
		assert.EqualValues(t, 480, usage["total_tokens"])
		assert.EqualValues(t, 300, usage["completion_tokens_details"].(map[string]interface{})["reasoning_tokens"])
		assert.EqualValues(t, 40, usage["prompt_tokens_details"].(map[string]interface{})["cached_read_tokens"])
		assert.EqualValues(t, 40, usage["prompt_tokens_details"].(map[string]interface{})["cached_tokens"])
		raw := body["extra_fields"].(map[string]interface{})["raw_response"].(map[string]interface{})
		assert.NotNil(t, raw["candidates"].([]interface{})[0].(map[string]interface{})["safetyRatings"])

		// The body is the sync chat response of the same native response.
		var native gemini.GenerateContentResponse
		require.NoError(t, sonic.UnmarshalString(gjson.Get(vertexOutputLineChat, "response").Raw, &native))
		want, err := sonic.Marshal(native.ToBifrostChatResponse())
		require.NoError(t, err)
		var wantBody map[string]interface{}
		require.NoError(t, sonic.Unmarshal(want, &wantBody))
		delete(wantBody, "extra_fields")
		delete(body, "extra_fields")
		assert.Equal(t, wantBody, body)
	})

	t.Run("image line keeps its artifact", func(t *testing.T) {
		item, endpoint, err := vertexBatchOutputLineToResultItem([]byte(vertexOutputLineImage), false, false)
		require.NoError(t, err)
		assert.Equal(t, schemas.BatchEndpointImageGeneration, endpoint)
		assert.Equal(t, "img-1", item.CustomID)
		assert.Equal(t, schemas.BatchResultStatusSucceeded, item.Status)
		data := item.Response.Body["data"].([]interface{})
		require.Len(t, data, 1)
		assert.Equal(t, "iVBORw0KGgoAAAANSUhEUg==", data[0].(map[string]interface{})["b64_json"])
		usage := item.Response.Body["usage"].(map[string]interface{})
		assert.EqualValues(t, 1290, usage["output_tokens"])
		assert.EqualValues(t, 1290, usage["output_tokens_details"].(map[string]interface{})["image_tokens"])
		_, hasExtraFields := item.Response.Body["extra_fields"]
		assert.False(t, hasExtraFields)
	})

	t.Run("failed record carries Vertex's error, not an empty success", func(t *testing.T) {
		item, endpoint, err := vertexBatchOutputLineToResultItem([]byte(vertexOutputLineFailed), false, false)
		require.NoError(t, err)
		assert.Equal(t, schemas.BatchEndpointChatCompletions, endpoint)
		assert.Equal(t, "line-3", item.CustomID)
		assert.Equal(t, schemas.BatchResultStatusFailed, item.Status)
		assert.True(t, item.Failed())
		assert.Nil(t, item.Response)
		assert.Contains(t, item.Error.Message, "maxOutputTokens value of 99999")
	})

	t.Run("a line with no endpoint label is a chat line", func(t *testing.T) {
		item, endpoint, err := vertexBatchOutputLineToResultItem([]byte(vertexOutputLineNoLabel), false, false)
		require.NoError(t, err)
		assert.Equal(t, schemas.BatchEndpoint(""), endpoint)
		assert.Equal(t, "chat.completion", item.Response.Body["object"])
	})

	t.Run("Claude line keeps its native response with status", func(t *testing.T) {
		item, _, err := vertexBatchOutputLineToResultItem([]byte(vertexOutputLineClaude), true, false)
		require.NoError(t, err)
		assert.Equal(t, "claude-1", item.CustomID)
		assert.Equal(t, schemas.BatchResultStatusSucceeded, item.Status)
		assert.Equal(t, "message", item.Response.Body["type"])
		assert.EqualValues(t, 9, item.Response.Body["usage"].(map[string]interface{})["input_tokens"])
	})

	t.Run("a malformed line is an error to count", func(t *testing.T) {
		for _, bad := range []string{`{"status":`, `{"request":{"labels":{}},"response":{"candidates":"nope"}}`} {
			_, _, err := vertexBatchOutputLineToResultItem([]byte(bad), false, false)
			require.Error(t, err, bad)
		}
	})
}

// A line that names the batch's model by its bare id is accepted when the batch names it by
// its publisher path, which ToVertexBatchCreateRequest supports.
func TestVertexBatchLineModelMatchesPublisherPath(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	var item schemas.BatchRequestItem
	require.NoError(t, sonic.UnmarshalString(`{"custom_id":"l-1","url":"/v1/chat/completions","body":{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}}`, &item))
	_, err := vertexConvertRequestsToJSONL(ctx, schemas.Key{}, []schemas.BatchRequestItem{item}, "publishers/google/models/gemini-2.5-flash", schemas.BatchEndpointChatCompletions)
	require.NoError(t, err)
}
