package anthropic_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/internal/llmtests"
	"github.com/capsohq/bifrost/core/providers/anthropic"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

func TestAnthropic(t *testing.T) {
	t.Parallel()
	if strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) == "" {
		t.Skip("Skipping Anthropic tests because ANTHROPIC_API_KEY is not set")
	}

	client, ctx, cancel, err := llmtests.SetupTest()
	if err != nil {
		t.Fatalf("Error initializing test setup: %v", err)
	}
	defer cancel()
	defer client.Shutdown()

	testConfig := llmtests.ComprehensiveTestConfig{
		Provider:  schemas.Anthropic,
		ChatModel: "claude-sonnet-4-5",
		Fallbacks: []schemas.Fallback{
			{Provider: schemas.Anthropic, Model: "claude-3-7-sonnet-20250219"},
			{Provider: schemas.Anthropic, Model: "claude-sonnet-4-6"},
		},
		VisionModel:        "claude-sonnet-4-5", // Same model supports vision
		ReasoningModel:     "claude-opus-4-5",
		PromptCachingModel: "claude-sonnet-4-6",
		PassthroughModel:   "claude-sonnet-4-5",
		WebSearchChatModel: "claude-sonnet-4-5",
		Scenarios: llmtests.TestScenarios{
			TextCompletion:               false, // Not supported
			SimpleChat:                   true,
			CompletionStream:             true,
			MultiTurnConversation:        true,
			ToolCalls:                    true,
			ToolCallsStreaming:           true,
			MultipleToolCalls:            true,
			MultipleToolCallsStreaming:   true,
			End2EndToolCalling:           true,
			AutomaticFunctionCall:        true,
			WebSearchTool:                true,
			ImageURL:                     true,
			ImageBase64:                  true,
			MultipleImages:               true,
			FileBase64:                   true,
			FileURL:                      true,
			CompleteEnd2End:              true,
			Embedding:                    false,
			Reasoning:                    true,
			PromptCaching:                true,
			ListModels:                   true,
			BatchCreate:                  true,
			BatchList:                    true,
			BatchRetrieve:                true,
			BatchCancel:                  true,
			BatchResults:                 true,
			FileUpload:                   true,
			FileList:                     true,
			FileRetrieve:                 true,
			FileDelete:                   true,
			FileContent:                  false,
			FileBatchInput:               false, // Anthropic batch API only supports inline requests, not file-based input
			CountTokens:                  true,
			StructuredOutputs:            true, // Structured outputs with nullable enum support
			PassthroughAPI:               true,
			Compaction:                   true,
			ToolSearch:                   true,
			InterleavedThinking:          true,
			FastMode:                     false, // Enable when test API key has Opus 4.6 access
			EagerInputStreaming:          true,  // fine-grained-tool-streaming-2025-05-14 (GA on Anthropic)
			ServerToolsViaOpenAIEndpoint: true,  // web_search / web_fetch / code_execution via /v1/chat/completions
		},
	}

	t.Run("AnthropicTests", func(t *testing.T) {
		llmtests.RunAllComprehensiveTests(t, client, ctx, testConfig)
	})
}

// A retrieve must send Anthropic's own auth headers, return the dated model an alias
// resolves to, carry created_at as a Unix timestamp, surface an upstream 404, and refuse
// a path-shaping id before anything is dispatched.
func TestAnthropicModelRetrieve(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var gotPath, gotAPIKey, gotVersion, gotAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath, gotAPIKey = r.URL.Path, r.Header.Get("x-api-key")
		gotVersion, gotAuthorization = r.Header.Get("anthropic-version"), r.Header.Get("Authorization")
		mu.Unlock()
		if r.URL.Path == "/v1/models/nope" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"not_found_error","message":"model: nope"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"type":"model","id":"claude-sonnet-4-5-20250929","display_name":"Claude Sonnet 4.5","created_at":"2025-09-29T00:00:00Z","max_input_tokens":200000,"max_tokens":64000,"capabilities":{"batch":{"supported":true}}}`))
	}))
	defer server.Close()

	provider := anthropic.NewAnthropicProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, DefaultRequestTimeoutInSeconds: 30},
	}, bifrost.NewNoOpLogger())

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	key := schemas.Key{Value: *schemas.NewSecretVar("sk-ant-test")}

	response, bifrostErr := provider.ModelRetrieve(ctx, key, &schemas.BifrostModelRetrieveRequest{Model: "claude-sonnet-4-5"})
	require.Nil(t, bifrostErr)

	mu.Lock()
	path, apiKey, version, authorization := gotPath, gotAPIKey, gotVersion, gotAuthorization
	mu.Unlock()
	require.Equal(t, "/v1/models/claude-sonnet-4-5", path)
	require.Equal(t, "sk-ant-test", apiKey)
	require.Equal(t, "2023-06-01", version)
	require.Empty(t, authorization, "Anthropic authenticates with x-api-key, not a bearer token")

	require.Equal(t, "anthropic/claude-sonnet-4-5-20250929", response.ID)
	require.Equal(t, schemas.Ptr("Claude Sonnet 4.5"), response.Name)
	require.Equal(t, schemas.Ptr(time.Date(2025, 9, 29, 0, 0, 0, 0, time.UTC).Unix()), response.Created)
	require.Equal(t, schemas.Ptr(200000), response.MaxInputTokens)
	require.Equal(t, schemas.Ptr(64000), response.MaxOutputTokens)

	_, bifrostErr = provider.ModelRetrieve(ctx, key, &schemas.BifrostModelRetrieveRequest{Model: "nope"})
	require.NotNil(t, bifrostErr)
	require.Equal(t, schemas.Ptr(http.StatusNotFound), bifrostErr.StatusCode)

	_, bifrostErr = provider.ModelRetrieve(ctx, key, &schemas.BifrostModelRetrieveRequest{Model: "../models"})
	require.NotNil(t, bifrostErr)
	require.Equal(t, schemas.Ptr(http.StatusBadRequest), bifrostErr.StatusCode)
}

// anthropicBatchVendor is a stand-in Anthropic API for the batch tests: it records every
// call and answers the sync Messages route, batch create, retrieve and the results file.
type anthropicBatchVendor struct {
	t       *testing.T
	server  *httptest.Server
	mu      sync.Mutex
	calls   []string
	bodies  map[string][]byte
	betas   map[string]string
	results string
}

func newAnthropicBatchVendor(t *testing.T, results string) *anthropicBatchVendor {
	v := &anthropicBatchVendor{t: t, bodies: map[string][]byte{}, betas: map[string]string{}, results: results}
	v.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		v.mu.Lock()
		v.calls = append(v.calls, r.Method+" "+r.URL.Path)
		v.bodies[r.URL.Path] = body
		v.betas[r.URL.Path] = r.Header.Get("anthropic-beta")
		v.mu.Unlock()
		switch {
		case r.URL.Path == "/v1/messages":
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`))
		case r.URL.Path == "/v1/messages/batches" && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"id":"msgbatch_1","type":"message_batch","processing_status":"in_progress","request_counts":{"processing":2},"created_at":"2026-11-10T00:00:00Z","expires_at":"2026-11-11T00:00:00Z"}`))
		case r.URL.Path == "/v1/messages/batches/msgbatch_1":
			_, _ = w.Write([]byte(`{"id":"msgbatch_1","type":"message_batch","processing_status":"ended","request_counts":{"succeeded":1,"errored":1,"expired":1,"canceled":1},"created_at":"2026-11-10T00:00:00Z","ended_at":"2026-11-10T01:00:00Z","expires_at":"2026-11-11T00:00:00Z"}`))
		case r.URL.Path == "/v1/messages/batches/msgbatch_1/results":
			_, _ = w.Write([]byte(v.results))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"not_found_error","message":"not found"}}`))
		}
	}))
	t.Cleanup(v.server.Close)
	return v
}

func (v *anthropicBatchVendor) provider() *anthropic.AnthropicProvider {
	return anthropic.NewAnthropicProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: v.server.URL, DefaultRequestTimeoutInSeconds: 30},
	}, bifrost.NewNoOpLogger())
}

func (v *anthropicBatchVendor) callCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.calls)
}

func (v *anthropicBatchVendor) body(path string) []byte {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.bodies[path]
}

func batchTestContext() *schemas.BifrostContext {
	return schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
}

var batchTestKey = schemas.Key{Value: *schemas.NewSecretVar("sk-ant-test")}

// An inline OpenAI-shape chat line reaches Anthropic as the same Messages body the sync chat
// route sends for the same request (contract C8, TXE-6597); a native Messages line runs
// unchanged beside it.
func TestAnthropicBatchCreateConvertsOpenAIChatLinesLikeTheSyncRoute(t *testing.T) {
	t.Parallel()
	const lineBody = `{"model":"claude-haiku-4-5","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"What is the weather in Paris?"}],"max_tokens":64,"temperature":0.2,"tools":[{"type":"function","function":{"name":"get_weather","description":"Weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],"tool_choice":"auto"}`
	vendor := newAnthropicBatchVendor(t, "")
	provider := vendor.provider()

	// The sync route: the same OpenAI body, read as the chat route reads it.
	var syncBody struct {
		Messages []schemas.ChatMessage `json:"messages"`
	}
	require.NoError(t, json.Unmarshal([]byte(lineBody), &syncBody))
	params := &schemas.ChatParameters{}
	require.NoError(t, json.Unmarshal([]byte(lineBody), params))
	params.MaxCompletionTokens = schemas.Ptr(64)
	_, bifrostErr := provider.ChatCompletion(batchTestContext(), batchTestKey, &schemas.BifrostChatRequest{
		Provider: schemas.Anthropic, Model: "claude-haiku-4-5", Input: syncBody.Messages, Params: params,
	})
	require.Nil(t, bifrostErr)
	syncSent := vendor.body("/v1/messages")

	var openAILine map[string]any
	require.NoError(t, json.Unmarshal([]byte(lineBody), &openAILine))
	native := map[string]any{"model": "claude-haiku-4-5", "max_tokens": float64(16), "messages": []any{map[string]any{"role": "user", "content": "native"}}}
	resp, bifrostErr := provider.BatchCreate(batchTestContext(), batchTestKey, &schemas.BifrostBatchCreateRequest{
		Provider: schemas.Anthropic,
		Model:    schemas.Ptr("claude-haiku-4-5"),
		Endpoint: schemas.BatchEndpointChatCompletions,
		Requests: []schemas.BatchRequestItem{
			{CustomID: "line-1", Method: "POST", URL: "/v1/chat/completions", Body: openAILine},
			{CustomID: "line-2", URL: "/v1/messages", Params: native},
		},
	})
	require.Nil(t, bifrostErr, "%+v", bifrostErr)
	require.Equal(t, "msgbatch_1", resp.ID)

	var batchSent struct {
		Requests []struct {
			CustomID string          `json:"custom_id"`
			Params   json.RawMessage `json:"params"`
		} `json:"requests"`
	}
	require.NoError(t, json.Unmarshal(vendor.body("/v1/messages/batches"), &batchSent))
	require.Len(t, batchSent.Requests, 2)
	require.Equal(t, "line-1", batchSent.Requests[0].CustomID)
	require.JSONEq(t, string(syncSent), string(batchSent.Requests[0].Params), "the batch line must reach Anthropic as the sync route's Messages body")
	require.Contains(t, string(batchSent.Requests[0].Params), `"system"`, "the system message moved to Messages' top-level system")
	nativeSent, _ := json.Marshal(native)
	require.JSONEq(t, string(nativeSent), string(batchSent.Requests[1].Params), "a native Messages line runs unchanged")
}

// Lines and inputs the Messages batch cannot run are refused before any upstream call.
func TestAnthropicBatchCreateRefusesBeforeAnyUpstreamCall(t *testing.T) {
	t.Parallel()
	line := func(url string, body map[string]any) schemas.BatchRequestItem {
		return schemas.BatchRequestItem{CustomID: "line-1", URL: url, Body: body}
	}
	chat := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	cases := []struct {
		name      string
		request   *schemas.BifrostBatchCreateRequest
		param     string
		errorType schemas.ErrorType
	}{
		{
			name:      "input_file_id is caller_unsupported_parameter",
			request:   &schemas.BifrostBatchCreateRequest{Provider: schemas.Anthropic, Model: schemas.Ptr("claude-haiku-4-5"), InputFileID: "file-abc"},
			param:     "input_file_id",
			errorType: schemas.ErrorTypeCallerUnsupportedParameter,
		},
		{
			name:      "input_blob is caller_unsupported_parameter",
			request:   &schemas.BifrostBatchCreateRequest{Provider: schemas.Anthropic, Model: schemas.Ptr("claude-haiku-4-5"), InputBlob: schemas.Ptr("{}")},
			param:     "input_blob",
			errorType: schemas.ErrorTypeCallerUnsupportedParameter,
		},
		{
			name:      "a url a Messages batch cannot run names the line",
			request:   &schemas.BifrostBatchCreateRequest{Provider: schemas.Anthropic, Model: schemas.Ptr("claude-haiku-4-5"), Requests: []schemas.BatchRequestItem{line("/v1/embeddings", map[string]any{"input": "x"})}},
			param:     "requests[0].url",
			errorType: schemas.ErrorTypeCallerUnsupportedParameter,
		},
		{
			name:      "a line naming another model names body.model",
			request:   &schemas.BifrostBatchCreateRequest{Provider: schemas.Anthropic, Model: schemas.Ptr("claude-haiku-4-5"), Requests: []schemas.BatchRequestItem{line("/v1/chat/completions", map[string]any{"model": "claude-opus-4-5", "messages": chat["messages"]})}},
			param:     "requests[0].body.model",
			errorType: schemas.ErrorTypeCallerUnsupportedParameter,
		},
		{
			name:      "a chat line without messages names the body",
			request:   &schemas.BifrostBatchCreateRequest{Provider: schemas.Anthropic, Model: schemas.Ptr("claude-haiku-4-5"), Requests: []schemas.BatchRequestItem{line("/v1/chat/completions", map[string]any{"max_tokens": 5})}},
			param:     "requests[0].body",
			errorType: schemas.ErrorTypeCallerInvalidRequest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vendor := newAnthropicBatchVendor(t, "")
			_, bifrostErr := vendor.provider().BatchCreate(batchTestContext(), batchTestKey, tc.request)
			require.NotNil(t, bifrostErr)
			require.Equal(t, schemas.Ptr(http.StatusBadRequest), bifrostErr.StatusCode, "%+v", bifrostErr.Error)
			require.Equal(t, tc.param, bifrostErr.Error.Param)
			if tc.errorType != "" {
				require.Equal(t, tc.errorType, bifrostErr.ExtraFields.ErrorType)
			}
			require.Zero(t, vendor.callCount(), "nothing may reach the vendor")
		})
	}
}

// A recorded Messages results file comes back as paged OpenAI-shape lines: a succeeded
// Message as the sync route's chat.completion with unified usage and the original Message in
// raw_response, every other result with its status and the vendor's error.
const anthropicBatchResultsFile = `{"custom_id":"line-1","result":{"type":"succeeded","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5-20251001","content":[{"type":"text","text":"Sunny."}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":6,"cache_creation_input_tokens":2}}}}
{"custom_id":"line-2","result":{"type":"errored","error":{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: Field required"}}}}
{"custom_id":"line-3","result":{"type":"expired"}}
{"custom_id":"line-4","result":{"type":"canceled"}}
`

func TestAnthropicBatchResultsArePagedOpenAIShapeLines(t *testing.T) {
	t.Parallel()
	vendor := newAnthropicBatchVendor(t, anthropicBatchResultsFile)
	provider := vendor.provider()
	ctx := batchTestContext()
	ctx.SetValue(schemas.BifrostContextKeyCaptureRawResponse, true)

	first, bifrostErr := provider.BatchResults(ctx, []schemas.Key{batchTestKey}, &schemas.BifrostBatchResultsRequest{Provider: schemas.Anthropic, BatchID: "msgbatch_1", Limit: 2})
	require.Nil(t, bifrostErr, "%+v", bifrostErr)
	require.Equal(t, schemas.BatchEndpointChatCompletions, first.Endpoint)
	require.Len(t, first.Results, 2)
	require.True(t, first.HasMore)
	require.NotNil(t, first.NextCursor)

	succeeded := first.Results[0]
	require.Equal(t, "line-1", succeeded.CustomID)
	require.Equal(t, schemas.BatchResultStatusSucceeded, succeeded.Status)
	require.Nil(t, succeeded.Result, "the unified routes carry no Messages-shape result")
	require.NotNil(t, succeeded.Response)
	require.Equal(t, http.StatusOK, succeeded.Response.StatusCode)
	body := succeeded.Response.Body
	require.Equal(t, "chat.completion", body["object"])
	require.Equal(t, "claude-haiku-4-5-20251001", body["model"])
	choices := body["choices"].([]any)
	message := choices[0].(map[string]any)["message"].(map[string]any)
	require.Equal(t, "Sunny.", message["content"])
	usage := body["usage"].(map[string]any)
	require.EqualValues(t, 18, usage["prompt_tokens"], "prompt_tokens counts cached and cache-write input, as on the sync route: %v", usage)
	require.EqualValues(t, 4, usage["completion_tokens"])
	details, _ := usage["prompt_tokens_details"].(map[string]any)
	require.EqualValues(t, 6, details["cached_read_tokens"], "%v", usage)
	require.EqualValues(t, 2, details["cached_write_tokens"], "%v", usage)
	raw := body["extra_fields"].(map[string]any)["raw_response"].(map[string]any)
	require.Equal(t, "msg_1", raw["id"], "the original Message is kept for reconciliation")

	errored := first.Results[1]
	require.Equal(t, schemas.BatchResultStatusFailed, errored.Status)
	require.Equal(t, &schemas.BatchResultError{Code: "invalid_request_error", Message: "max_tokens: Field required"}, errored.Error)
	require.Nil(t, errored.Response)

	second, bifrostErr := provider.BatchResults(ctx, []schemas.Key{batchTestKey}, &schemas.BifrostBatchResultsRequest{Provider: schemas.Anthropic, BatchID: "msgbatch_1", Limit: 2, After: first.NextCursor})
	require.Nil(t, bifrostErr, "%+v", bifrostErr)
	require.Len(t, second.Results, 2)
	require.False(t, second.HasMore)
	require.Equal(t, "line-3", second.Results[0].CustomID)
	require.Equal(t, schemas.BatchResultStatusExpired, second.Results[0].Status)
	require.Equal(t, "expired", second.Results[0].Error.Code)
	require.Equal(t, "line-4", second.Results[1].CustomID)
	require.Equal(t, schemas.BatchResultStatusCancelled, second.Results[1].Status)
	require.Equal(t, "canceled", second.Results[1].Error.Code)

	// Without the raw-response switch the line carries no extra_fields.
	plain, bifrostErr := provider.BatchResults(batchTestContext(), []schemas.Key{batchTestKey}, &schemas.BifrostBatchResultsRequest{Provider: schemas.Anthropic, BatchID: "msgbatch_1"})
	require.Nil(t, bifrostErr)
	require.Len(t, plain.Results, 4)
	require.False(t, plain.HasMore)
	require.NotContains(t, plain.Results[0].Response.Body, "extra_fields")
}

// The /anthropic/v1/messages/batches route keeps returning the Messages results unchanged.
func TestAnthropicBatchResultsNativeRouteKeepsMessagesShape(t *testing.T) {
	t.Parallel()
	vendor := newAnthropicBatchVendor(t, anthropicBatchResultsFile)
	ctx := batchTestContext()
	ctx.SetValue(schemas.BifrostContextKeyIntegrationType, "anthropic")
	resp, bifrostErr := vendor.provider().BatchResults(ctx, []schemas.Key{batchTestKey}, &schemas.BifrostBatchResultsRequest{Provider: schemas.Anthropic, BatchID: "msgbatch_1"})
	require.Nil(t, bifrostErr)
	require.Equal(t, schemas.BatchEndpointMessages, resp.Endpoint)
	require.Len(t, resp.Results, 4)
	require.Nil(t, resp.Results[0].Response)
	require.Equal(t, "succeeded", resp.Results[0].Result.Type)
	require.Equal(t, "msg_1", resp.Results[0].Result.Message["id"])
	require.Equal(t, "errored", resp.Results[1].Result.Type)
	require.Equal(t, "invalid_request_error", resp.Results[1].Error.Code)
}

// Anthropic's ended maps to completed on the batch object (contract C8); the vendor's own
// word stays in processing_status.
func TestAnthropicBatchRetrieveReportsEndedAsCompleted(t *testing.T) {
	t.Parallel()
	vendor := newAnthropicBatchVendor(t, "")
	resp, bifrostErr := vendor.provider().BatchRetrieve(batchTestContext(), []schemas.Key{batchTestKey}, &schemas.BifrostBatchRetrieveRequest{Provider: schemas.Anthropic, BatchID: "msgbatch_1"})
	require.Nil(t, bifrostErr)
	require.Equal(t, schemas.BatchStatusCompleted, resp.Status)
	require.Equal(t, schemas.Ptr("ended"), resp.ProcessingStatus)
	require.NotNil(t, resp.CompletedAt)
	require.Equal(t, 4, resp.RequestCounts.Total)
}

// Batch line conversion cost (TXE-6597). "native" is the path every line took before:
// the body sent unchanged on create, the Messages result kept on results. "openai" is the
// new conversion an OpenAI-shape line takes. Run with -benchmem.
func BenchmarkAnthropicBatchLineConversion(b *testing.B) {
	var chatBody map[string]any
	if err := json.Unmarshal([]byte(`{"model":"claude-haiku-4-5","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"What is the weather in Paris?"}],"max_tokens":64,"temperature":0.2,"tools":[{"type":"function","function":{"name":"get_weather","description":"Weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],"tool_choice":"auto"}`), &chatBody); err != nil {
		b.Fatal(err)
	}
	item := schemas.BatchRequestItem{CustomID: "line-1", URL: "/v1/chat/completions", Body: chatBody}
	resultLine := []byte(strings.SplitN(anthropicBatchResultsFile, "\n", 2)[0])
	cfg := anthropic.AnthropicRequestBuildConfig{Provider: schemas.Anthropic}

	b.Run("create/native", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = anthropic.AnthropicBatchRequestItem{CustomID: item.CustomID, Params: item.RequestBody()}
		}
	})
	b.Run("create/openai", func(b *testing.B) {
		ctx := batchTestContext()
		b.ReportAllocs()
		for b.Loop() {
			if _, err := anthropic.BatchChatLineParams(ctx, item, schemas.Anthropic, "claude-haiku-4-5", cfg); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("results/native", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var line anthropic.AnthropicBatchResultItem
			if err := json.Unmarshal(resultLine, &line); err != nil {
				b.Fatal(err)
			}
			_ = anthropic.BatchResultLineToNative(line)
		}
	})
	b.Run("results/openai", func(b *testing.B) {
		ctx := batchTestContext()
		b.ReportAllocs()
		for b.Loop() {
			var line anthropic.AnthropicBatchResultItem
			if err := json.Unmarshal(resultLine, &line); err != nil {
				b.Fatal(err)
			}
			_ = anthropic.BatchResultLineToOpenAI(ctx, line, false)
		}
	})
}
