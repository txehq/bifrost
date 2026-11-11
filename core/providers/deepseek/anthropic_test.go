package deepseek_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/internal/llmtests"
	deepseek "github.com/capsohq/bifrost/core/providers/deepseek"
	schemas "github.com/capsohq/bifrost/core/schemas"
)

type testLogger struct{}

func (l testLogger) Debug(string, ...any)                   {}
func (l testLogger) Info(string, ...any)                    {}
func (l testLogger) Warn(string, ...any)                    {}
func (l testLogger) Error(string, ...any)                   {}
func (l testLogger) Fatal(string, ...any)                   {}
func (l testLogger) SetLevel(schemas.LogLevel)              {}
func (l testLogger) SetOutputType(schemas.LoggerOutputType) {}
func (l testLogger) LogHTTPRequest(schemas.LogLevel, string) schemas.LogEventBuilder {
	return schemas.NoopLogEvent
}

func newTestDeepSeekProvider(baseURL string) (*deepseek.DeepSeekProvider, error) {
	return deepseek.NewDeepSeekProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{
			BaseURL:                        baseURL,
			DefaultRequestTimeoutInSeconds: 5,
			StreamIdleTimeoutInSeconds:     5,
			MaxConnsPerHost:                1,
		},
		ConcurrencyAndBufferSize: schemas.ConcurrencyAndBufferSize{
			Concurrency: 1,
			BufferSize:  1,
		},
	}, testLogger{})
}

func newAnthropicResponse() string {
	return `{"id":"msg_1","type":"message","role":"assistant","model":"deepseek-chat","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
}

func TestChatCompletion_UsesAnthropicEndpoint(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	// Handlers run on their own goroutine, so they assert with t.Errorf and
	// return an error status rather than t.Fatalf: FailNow is only legal from
	// the goroutine running the test, and from a handler it aborts the response
	// mid-write, surfacing an opaque transport error instead of this message.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/anthropic/v1/messages" {
			t.Errorf("path = %q, want /anthropic/v1/messages", r.URL.Path)
			http.Error(w, "unexpected path", http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("x-api-key"); got != "test-api-key" {
			t.Errorf("x-api-key = %q, want test-api-key", got)
			http.Error(w, "unexpected api key", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			http.Error(w, "read body", http.StatusInternalServerError)
			return
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Errorf("decode body: %v", err)
			http.Error(w, "decode body", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, newAnthropicResponse())
	}))
	defer server.Close()

	provider, err := newTestDeepSeekProvider(server.URL)
	if err != nil {
		t.Fatalf("NewDeepSeekProvider: %v", err)
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	msg := "hello"
	resp, bifrostErr := provider.ChatCompletion(ctx, schemas.Key{Value: schemas.SecretVar{Val: "test-api-key"}, UseAnthropicEndpoints: new(true)}, &schemas.BifrostChatRequest{
		Provider: schemas.DeepSeek,
		Model:    "deepseek-v4-flash",
		Input: []schemas.ChatMessage{{
			Role:    schemas.ChatMessageRoleUser,
			Content: &schemas.ChatMessageContent{ContentStr: &msg},
		}},
	})
	if bifrostErr != nil {
		t.Fatalf("ChatCompletion: %v", bifrostErr.Error.Message)
	}
	if resp == nil || len(resp.Choices) == 0 {
		t.Fatalf("expected chat response, got %#v", resp)
	}
	if _, ok := captured["messages"]; !ok {
		t.Fatalf("outbound body missing messages: %#v", captured)
	}
}

func TestResponses_UsesAnthropicEndpointAndKeepsWebSearch(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/anthropic/v1/messages" {
			t.Errorf("path = %q, want /anthropic/v1/messages", r.URL.Path)
			http.Error(w, "unexpected path", http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("x-api-key"); got != "test-api-key" {
			t.Errorf("x-api-key = %q, want test-api-key", got)
			http.Error(w, "unexpected api key", http.StatusBadRequest)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			http.Error(w, "read body", http.StatusInternalServerError)
			return
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Errorf("decode body: %v", err)
			http.Error(w, "decode body", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, newAnthropicResponse())
	}))
	defer server.Close()

	provider, err := newTestDeepSeekProvider(server.URL)
	if err != nil {
		t.Fatalf("NewDeepSeekProvider: %v", err)
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	resp, bifrostErr := provider.Responses(ctx, schemas.Key{Value: schemas.SecretVar{Val: "test-api-key"}, UseAnthropicEndpoints: new(true)}, &schemas.BifrostResponsesRequest{
		Provider: schemas.DeepSeek,
		Model:    "deepseek-v4-pro",
		Input: []schemas.ResponsesMessage{{
			Type: new(schemas.ResponsesMessageTypeMessage),
			Role: new(schemas.ResponsesInputMessageRoleUser),
			Content: &schemas.ResponsesMessageContent{
				ContentStr: new("hello"),
			},
		}},
		Params: &schemas.ResponsesParameters{
			Tools: []schemas.ResponsesTool{{Type: schemas.ResponsesToolTypeWebSearch}},
		},
	})
	if bifrostErr != nil {
		t.Fatalf("Responses: %v", bifrostErr.Error.Message)
	}
	if resp == nil || len(resp.Output) == 0 {
		t.Fatalf("expected responses payload, got %#v", resp)
	}
	tools, ok := captured["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("outbound body missing tools: %#v", captured)
	}
	toolJSON, _ := json.Marshal(tools[0])
	if !json.Valid(toolJSON) || !strings.Contains(string(toolJSON), "web_search") {
		t.Fatalf("outbound tool body did not preserve web search: %s", toolJSON)
	}
}

func TestChatCompletion_DisablesThinkingForForcedToolChoice(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			http.Error(w, "read body", http.StatusInternalServerError)
			return
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Errorf("decode body: %v", err)
			http.Error(w, "decode body", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, newAnthropicResponse())
	}))
	defer server.Close()

	provider, err := newTestDeepSeekProvider(server.URL)
	if err != nil {
		t.Fatalf("NewDeepSeekProvider: %v", err)
	}

	chatTool := llmtests.GetSampleChatTool(llmtests.SampleToolTypeTime)
	if chatTool == nil {
		t.Fatal("GetSampleChatTool returned nil")
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	msg := "get the current time in UTC"
	_, bifrostErr := provider.ChatCompletion(ctx, schemas.Key{Value: schemas.SecretVar{Val: "test-api-key"}, UseAnthropicEndpoints: new(true)}, &schemas.BifrostChatRequest{
		Provider: schemas.DeepSeek,
		Model:    "deepseek-v4-flash",
		Input: []schemas.ChatMessage{{
			Role:    schemas.ChatMessageRoleUser,
			Content: &schemas.ChatMessageContent{ContentStr: &msg},
		}},
		Params: &schemas.ChatParameters{
			Tools: []schemas.ChatTool{*chatTool},
			ToolChoice: &schemas.ChatToolChoice{
				ChatToolChoiceStruct: &schemas.ChatToolChoiceStruct{
					Type: schemas.ChatToolChoiceTypeFunction,
					Function: &schemas.ChatToolChoiceFunction{
						Name: string(llmtests.SampleToolTypeTime),
					},
				},
			},
		},
	})
	if bifrostErr != nil {
		t.Fatalf("ChatCompletion: %v", bifrostErr.Error.Message)
	}

	thinking, ok := captured["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("expected thinking block in outbound body, got %#v", captured)
	}
	if got := thinking["type"]; got != "disabled" {
		t.Fatalf("thinking.type = %v, want disabled", got)
	}
}

func TestResponses_DisablesThinkingForForcedToolChoice(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			http.Error(w, "read body", http.StatusInternalServerError)
			return
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Errorf("decode body: %v", err)
			http.Error(w, "decode body", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, newAnthropicResponse())
	}))
	defer server.Close()

	provider, err := newTestDeepSeekProvider(server.URL)
	if err != nil {
		t.Fatalf("NewDeepSeekProvider: %v", err)
	}

	responsesTool := llmtests.GetSampleResponsesTool(llmtests.SampleToolTypeTime)
	if responsesTool == nil {
		t.Fatal("GetSampleResponsesTool returned nil")
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, bifrostErr := provider.Responses(ctx, schemas.Key{Value: schemas.SecretVar{Val: "test-api-key"}, UseAnthropicEndpoints: new(true)}, &schemas.BifrostResponsesRequest{
		Provider: schemas.DeepSeek,
		Model:    "deepseek-v4-pro",
		Input: []schemas.ResponsesMessage{{
			Type: new(schemas.ResponsesMessageTypeMessage),
			Role: new(schemas.ResponsesInputMessageRoleUser),
			Content: &schemas.ResponsesMessageContent{
				ContentStr: new("get the current time in UTC"),
			},
		}},
		Params: &schemas.ResponsesParameters{
			Tools: []schemas.ResponsesTool{*responsesTool},
			ToolChoice: &schemas.ResponsesToolChoice{
				ResponsesToolChoiceStruct: &schemas.ResponsesToolChoiceStruct{
					Type: schemas.ResponsesToolChoiceTypeFunction,
					Name: new(string(llmtests.SampleToolTypeTime)),
				},
			},
		},
	})
	if bifrostErr != nil {
		t.Fatalf("Responses: %v", bifrostErr.Error.Message)
	}

	thinking, ok := captured["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("expected thinking block in outbound body, got %#v", captured)
	}
	if got := thinking["type"]; got != "disabled" {
		t.Fatalf("thinking.type = %v, want disabled", got)
	}
}

func TestChatCompletion_DefaultsToOpenAIEndpoint(t *testing.T) {
	t.Parallel()

	var capturedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		if got := r.Header.Get("Authorization"); got != "Bearer test-api-key" {
			t.Errorf("Authorization = %q, want Bearer test-api-key", got)
			http.Error(w, "unexpected authorization", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl_1","object":"chat.completion","model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer server.Close()

	provider, err := newTestDeepSeekProvider(server.URL)
	if err != nil {
		t.Fatalf("NewDeepSeekProvider: %v", err)
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	msg := "hello"
	resp, bifrostErr := provider.ChatCompletion(ctx, schemas.Key{Value: schemas.SecretVar{Val: "test-api-key"}}, &schemas.BifrostChatRequest{
		Provider: schemas.DeepSeek,
		Model:    "deepseek-v4-flash",
		Input: []schemas.ChatMessage{{
			Role:    schemas.ChatMessageRoleUser,
			Content: &schemas.ChatMessageContent{ContentStr: &msg},
		}},
	})
	if bifrostErr != nil {
		t.Fatalf("ChatCompletion: %v", bifrostErr.Error.Message)
	}
	if resp == nil || len(resp.Choices) == 0 {
		t.Fatalf("expected chat response, got %#v", resp)
	}
	if capturedPath != "/chat/completions" {
		t.Fatalf("path = %q, want /chat/completions", capturedPath)
	}
}

func TestChatCompletion_OpenAIEndpointDisablesThinkingForRequiredToolChoice(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			http.Error(w, "read body", http.StatusInternalServerError)
			return
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Errorf("decode body: %v", err)
			http.Error(w, "decode body", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl_1","object":"chat.completion","model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer server.Close()

	provider, err := newTestDeepSeekProvider(server.URL)
	if err != nil {
		t.Fatalf("NewDeepSeekProvider: %v", err)
	}

	chatTool := llmtests.GetSampleChatTool(llmtests.SampleToolTypeTime)
	if chatTool == nil {
		t.Fatal("GetSampleChatTool returned nil")
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	msg := "get the current time in UTC"
	requiredChoice := string(schemas.ChatToolChoiceTypeRequired)
	_, bifrostErr := provider.ChatCompletion(ctx, schemas.Key{Value: schemas.SecretVar{Val: "test-api-key"}}, &schemas.BifrostChatRequest{
		Provider: schemas.DeepSeek,
		Model:    "deepseek-v4-flash",
		Input: []schemas.ChatMessage{{
			Role:    schemas.ChatMessageRoleUser,
			Content: &schemas.ChatMessageContent{ContentStr: &msg},
		}},
		Params: &schemas.ChatParameters{
			Tools: []schemas.ChatTool{*chatTool},
			ToolChoice: &schemas.ChatToolChoice{
				ChatToolChoiceStr: &requiredChoice,
			},
		},
	})
	if bifrostErr != nil {
		t.Fatalf("ChatCompletion: %v", bifrostErr.Error.Message)
	}

	thinking, ok := captured["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("expected thinking block in outbound body, got %#v", captured)
	}
	if got := thinking["type"]; got != "disabled" {
		t.Fatalf("thinking.type = %v, want disabled", got)
	}
}

// TestChatCompletion_AliasOverrideUsesAnthropicEndpoint pins the alias branch of
// ResolveUseAnthropicEndpoints: a key with no use_anthropic_endpoints of its own
// still routes through /anthropic/v1/messages when the resolved alias carries the
// flag. This is the shape the provider harness relies on (#7610): the shared
// DeepSeek key stays on the OpenAI-compatible wire and only the *-anthropic
// aliases opt into the Anthropic-compatible endpoint.
func TestChatCompletion_AliasOverrideUsesAnthropicEndpoint(t *testing.T) {
	t.Parallel()

	var gotPath, gotAPIKey, gotAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, newAnthropicResponse())
	}))
	defer server.Close()

	provider, err := newTestDeepSeekProvider(server.URL)
	if err != nil {
		t.Fatalf("NewDeepSeekProvider: %v", err)
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyResolvedAlias, &schemas.ResolvedAlias{
		Key:    "deepseek-v4-flash-anthropic",
		Config: &schemas.AliasConfig{ModelID: "deepseek-v4-flash", UseAnthropicEndpoints: new(true)},
	})
	msg := "hello"
	resp, bifrostErr := provider.ChatCompletion(ctx, schemas.Key{Value: schemas.SecretVar{Val: "test-api-key"}}, &schemas.BifrostChatRequest{
		Provider: schemas.DeepSeek,
		Model:    "deepseek-v4-flash",
		Input: []schemas.ChatMessage{{
			Role:    schemas.ChatMessageRoleUser,
			Content: &schemas.ChatMessageContent{ContentStr: &msg},
		}},
	})
	if bifrostErr != nil {
		t.Fatalf("ChatCompletion: %v", bifrostErr.Error.Message)
	}
	if resp == nil || len(resp.Choices) == 0 {
		t.Fatalf("expected chat response, got %#v", resp)
	}
	if gotPath != "/anthropic/v1/messages" {
		t.Fatalf("path = %q, want /anthropic/v1/messages (alias-level use_anthropic_endpoints was ignored)", gotPath)
	}
	if gotAPIKey != "test-api-key" {
		t.Fatalf("x-api-key = %q, want test-api-key", gotAPIKey)
	}
	if gotAuthorization != "" {
		t.Fatalf("Authorization = %q, want empty (bearer auth belongs to the OpenAI-compatible wire)", gotAuthorization)
	}
}

// TXE-6548: /anthropic/v1/messages for DeepSeek passes through to DeepSeek's
// Anthropic-compatible endpoint instead of round-tripping through Bifrost Responses.
// The fixtures follow DeepSeek's Anthropic API guide
// (api-docs.deepseek.com/guides/anthropic_api): x-api-key auth, input_tokens that
// exclude cache hits, cache_read_input_tokens for the hits, and streaming usage that
// the final message_delta restates in full.
const (
	deepSeekMessagesRequest = "{\"model\":\"deepseek-v4-flash\",\n  \"max_tokens\":\t1024,\n" +
		"  \"system\":[{\"type\":\"text\",\"text\":\"Caf\\u00e9 \\ud83d\\ude00\",\"cache_control\":{\"type\":\"ephemeral\"}}],\n" +
		"  \"thinking\":{\"type\":\"enabled\",\"budget_tokens\":512},\n" +
		"  \"tools\":[{\"name\":\"lookup\",\"input_schema\":{\"type\":\"object\"}}],\"tool_choice\":{\"type\":\"auto\",\"disable_parallel_tool_use\":true},\n" +
		"  \"messages\":[{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"hi\"}]}],\n" +
		"  \"metadata\":{\"user_id\":\"u-1\"},\"top_k\":5,\"zz_unknown\":{\"b\":1,\"a\":[]} }"
	deepSeekMessagesResponse = `{"id":"msg_ds","type":"message","role":"assistant","model":"deepseek-v4-flash","content":[{"type":"thinking","thinking":"plan","signature":"sig"},{"type":"text","text":"Hello"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":20,"cache_read_input_tokens":4736,"cache_creation_input_tokens":0,"output_tokens":52}}`
	deepSeekMessagesSSE      = "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_ds\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"deepseek-v4-flash\",\"content\":[],\"usage\":{\"input_tokens\":20,\"cache_read_input_tokens\":4736,\"cache_creation_input_tokens\":0,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: ping\n" +
		"data: {\"type\":\"ping\"}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Caf\\u00e9\"}}\n\n" +
		"event: content_block_stop\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"input_tokens\":20,\"cache_read_input_tokens\":4736,\"cache_creation_input_tokens\":0,\"output_tokens\":52}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
)

type capturedMessagesCall struct {
	path, apiKey, authorization string
	body                        []byte
}

func newDeepSeekMessagesServer(t *testing.T, status int, contentType, response string) (*httptest.Server, chan capturedMessagesCall) {
	t.Helper()
	calls := make(chan capturedMessagesCall, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		calls <- capturedMessagesCall{path: r.URL.Path, apiKey: r.Header.Get("x-api-key"), authorization: r.Header.Get("Authorization"), body: body}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("x-ds-trace-id", "trace-1")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(server.Close)
	return server, calls
}

func assertDeepSeekMessagesCall(t *testing.T, call capturedMessagesCall, wantBody string) {
	t.Helper()
	if call.path != "/anthropic/v1/messages" {
		t.Fatalf("upstream path = %q, want /anthropic/v1/messages", call.path)
	}
	if call.apiKey != "ds-secret" || call.authorization != "" {
		t.Fatalf("auth headers = x-api-key %q, Authorization %q; want only x-api-key", call.apiKey, call.authorization)
	}
	if string(call.body) != wantBody {
		t.Fatalf("upstream body is not byte-exact:\n got: %s\nwant: %s", call.body, wantBody)
	}
}

func TestDeepSeekAdvertisesAnthropicMessagesEndpoint(t *testing.T) {
	t.Parallel()

	provider, err := newTestDeepSeekProvider("https://api.deepseek.com/")
	if err != nil {
		t.Fatalf("NewDeepSeekProvider: %v", err)
	}
	capable, ok := any(provider).(schemas.AnthropicMessagesCapable)
	if !ok {
		t.Fatal("DeepSeek provider does not advertise Anthropic Messages capability")
	}
	if endpoint, enabled := capable.AnthropicMessagesEndpoint(); !enabled || endpoint != "https://api.deepseek.com/anthropic" {
		t.Fatalf("endpoint = %q, enabled = %v; want https://api.deepseek.com/anthropic, true", endpoint, enabled)
	}
}

func TestPassthrough_ForwardsMessagesToAnthropicEndpointByteExact(t *testing.T) {
	t.Parallel()

	server, calls := newDeepSeekMessagesServer(t, http.StatusOK, "application/json", deepSeekMessagesResponse)
	provider, err := newTestDeepSeekProvider(server.URL)
	if err != nil {
		t.Fatalf("NewDeepSeekProvider: %v", err)
	}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	resp, bifrostErr := provider.Passthrough(ctx, schemas.Key{Value: schemas.SecretVar{Val: "ds-secret"}}, &schemas.BifrostPassthroughRequest{
		Method: http.MethodPost, Path: "/v1/messages", Body: []byte(deepSeekMessagesRequest),
		SafeHeaders: map[string]string{"anthropic-version": "2023-06-01", "anthropic-beta": "interleaved-thinking-2025-05-14"},
	})
	if bifrostErr != nil {
		t.Fatalf("Passthrough: %+v", bifrostErr)
	}
	assertDeepSeekMessagesCall(t, <-calls, deepSeekMessagesRequest)

	if resp.StatusCode != http.StatusOK || string(resp.Body) != deepSeekMessagesResponse {
		t.Fatalf("response = %d %s; want DeepSeek's bytes unchanged", resp.StatusCode, resp.Body)
	}
	if resp.Headers["X-Ds-Trace-Id"] != "trace-1" {
		t.Fatalf("vendor response headers lost: %v", resp.Headers)
	}
	usage := resp.PassthroughUsage
	if usage == nil || usage.LLMUsage == nil || usage.LLMUsage.PromptTokensDetails == nil {
		t.Fatalf("missing passthrough usage: %+v", usage)
	}
	if got := usage.LLMUsage; got.PromptTokens != 4756 || got.CompletionTokens != 52 || got.PromptTokensDetails.CachedReadTokens != 4736 {
		t.Fatalf("usage = prompt %d, completion %d, cache_read %d; want 4756, 52, 4736", got.PromptTokens, got.CompletionTokens, got.PromptTokensDetails.CachedReadTokens)
	}
}

// DeepSeek's 400 for a named tool_choice in thinking mode is the documented case where
// the converted path used to switch thinking off. Passthrough forwards the body as sent,
// so the vendor's status and error body come back as they are (contract C11).
func TestPassthrough_ReturnsDeepSeekErrorStatusAndBodyAsIs(t *testing.T) {
	t.Parallel()

	const errorBody = `{"type":"error","error":{"type":"invalid_request_error","message":"tool_choice of type tool is not supported in thinking mode"}}`
	server, calls := newDeepSeekMessagesServer(t, http.StatusBadRequest, "application/json", errorBody)
	provider, err := newTestDeepSeekProvider(server.URL)
	if err != nil {
		t.Fatalf("NewDeepSeekProvider: %v", err)
	}
	request := `{"model":"deepseek-v4-flash","max_tokens":64,"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"lookup"},"messages":[{"role":"user","content":"hi"}]}`
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	resp, bifrostErr := provider.Passthrough(ctx, schemas.Key{Value: schemas.SecretVar{Val: "ds-secret"}}, &schemas.BifrostPassthroughRequest{
		Method: http.MethodPost, Path: "/v1/messages", Body: []byte(request),
	})
	if bifrostErr != nil {
		t.Fatalf("Passthrough: %+v", bifrostErr)
	}
	assertDeepSeekMessagesCall(t, <-calls, request)
	if resp.StatusCode != http.StatusBadRequest || string(resp.Body) != errorBody {
		t.Fatalf("response = %d %s; want DeepSeek's 400 unchanged", resp.StatusCode, resp.Body)
	}
	if resp.PassthroughUsage != nil {
		t.Fatalf("an error response carries no usage, got %+v", resp.PassthroughUsage)
	}
}

func TestPassthroughStream_RelaysDeepSeekSSEVerbatimWithUsage(t *testing.T) {
	t.Parallel()

	server, calls := newDeepSeekMessagesServer(t, http.StatusOK, "text/event-stream", deepSeekMessagesSSE)
	provider, err := newTestDeepSeekProvider(server.URL)
	if err != nil {
		t.Fatalf("NewDeepSeekProvider: %v", err)
	}
	request := strings.Replace(deepSeekMessagesRequest, `"max_tokens":`, `"stream":true,"max_tokens":`, 1)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	postHook := func(_ *schemas.BifrostContext, response *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		return response, err
	}
	stream, bifrostErr := provider.PassthroughStream(ctx, postHook, nil, schemas.Key{Value: schemas.SecretVar{Val: "ds-secret"}}, &schemas.BifrostPassthroughRequest{
		Method: http.MethodPost, Path: "/v1/messages", Body: []byte(request),
	})
	if bifrostErr != nil {
		t.Fatalf("PassthroughStream: %+v", bifrostErr)
	}
	var relayed []byte
	var usage *schemas.BifrostPassthroughUsage
	for chunk := range stream {
		if chunk == nil || chunk.BifrostPassthroughResponse == nil {
			continue
		}
		relayed = append(relayed, chunk.BifrostPassthroughResponse.Body...)
		if chunk.BifrostPassthroughResponse.PassthroughUsage != nil {
			usage = chunk.BifrostPassthroughResponse.PassthroughUsage
		}
	}
	assertDeepSeekMessagesCall(t, <-calls, request)
	if string(relayed) != deepSeekMessagesSSE {
		t.Fatalf("SSE bytes changed:\n got: %q\nwant: %q", relayed, deepSeekMessagesSSE)
	}
	// DeepSeek restates the full usage in the final message_delta; it must not be
	// added to message_start's.
	if usage == nil || usage.LLMUsage == nil || usage.LLMUsage.PromptTokens != 4756 || usage.LLMUsage.CompletionTokens != 52 {
		t.Fatalf("stream usage = %+v; want prompt 4756, completion 52", usage)
	}
}

func TestPassthrough_RefusesNonMessagesPaths(t *testing.T) {
	t.Parallel()

	provider, err := newTestDeepSeekProvider("https://api.deepseek.com")
	if err != nil {
		t.Fatalf("NewDeepSeekProvider: %v", err)
	}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	key := schemas.Key{Value: schemas.SecretVar{Val: "ds-secret"}}
	req := &schemas.BifrostPassthroughRequest{Method: http.MethodPost, Path: "/v1/messages/count_tokens", Body: []byte(`{}`)}
	if _, bifrostErr := provider.Passthrough(ctx, key, req); bifrostErr == nil {
		t.Fatal("Passthrough accepted a non-Messages path")
	}
	if _, bifrostErr := provider.PassthroughStream(ctx, nil, nil, key, req); bifrostErr == nil {
		t.Fatal("PassthroughStream accepted a non-Messages path")
	}
}

// Advertising the Messages endpoint must not move chat or Responses: without
// use_anthropic_endpoints both stay on the OpenAI-compatible /chat/completions.
func TestAnthropicMessagesEndpointLeavesChatAndResponsesOnChatCompletions(t *testing.T) {
	t.Parallel()

	paths := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	provider, err := newTestDeepSeekProvider(server.URL)
	if err != nil {
		t.Fatalf("NewDeepSeekProvider: %v", err)
	}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	key := schemas.Key{Value: schemas.SecretVar{Val: "ds-secret"}}
	if _, bifrostErr := provider.ChatCompletion(ctx, key, &schemas.BifrostChatRequest{
		Provider: schemas.DeepSeek, Model: "deepseek-v4-flash",
		Input: []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("hi")}}},
	}); bifrostErr != nil {
		t.Fatalf("ChatCompletion: %+v", bifrostErr)
	}
	if _, bifrostErr := provider.Responses(ctx, key, &schemas.BifrostResponsesRequest{
		Provider: schemas.DeepSeek, Model: "deepseek-v4-flash",
		Input: []schemas.ResponsesMessage{{Role: schemas.Ptr(schemas.ResponsesInputMessageRoleUser), Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr("hi")}}},
	}); bifrostErr != nil {
		t.Fatalf("Responses: %+v", bifrostErr)
	}
	for i := 0; i < 2; i++ {
		if got := <-paths; got != "/chat/completions" {
			t.Fatalf("request %d path = %q, want /chat/completions", i, got)
		}
	}
}

// BenchmarkDeepSeekMessagesPassthrough is one buffered native Messages request, from
// the provider call to the decoded response, against an in-process DeepSeek stand-in
// over a kept-alive loopback connection (TXE-6548).
func BenchmarkDeepSeekMessagesPassthrough(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(deepSeekMessagesResponse))
	}))
	defer server.Close()
	provider, err := newTestDeepSeekProvider(server.URL)
	if err != nil {
		b.Fatalf("NewDeepSeekProvider: %v", err)
	}
	key := schemas.Key{Value: schemas.SecretVar{Val: "ds-secret"}}
	request := &schemas.BifrostPassthroughRequest{Method: http.MethodPost, Path: "/v1/messages", Body: []byte(deepSeekMessagesRequest)}
	b.ReportAllocs()
	for b.Loop() {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		if _, bifrostErr := provider.Passthrough(ctx, key, request); bifrostErr != nil {
			b.Fatalf("Passthrough: %+v", bifrostErr)
		}
		ctx.Cancel()
	}
}
