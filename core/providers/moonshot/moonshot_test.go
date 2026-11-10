package moonshot_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/internal/llmtests"
	"github.com/capsohq/bifrost/core/providers/moonshot"
	"github.com/capsohq/bifrost/core/providers/openai"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func TestMoonshot(t *testing.T) {
	t.Parallel()

	if strings.TrimSpace(os.Getenv("MOONSHOT_API_KEY")) == "" {
		t.Skip("Skipping Moonshot tests because MOONSHOT_API_KEY is not set")
	}

	client, ctx, cancel, err := llmtests.SetupTest()
	if err != nil {
		t.Fatalf("Error initializing test setup: %v", err)
	}
	defer cancel()

	testConfig := llmtests.ComprehensiveTestConfig{
		Provider:  schemas.Moonshot,
		ChatModel: envOrDefault("MOONSHOT_CHAT_MODEL", "kimi-k2.5"),
		TextModel: envOrDefault("MOONSHOT_TEXT_MODEL", "kimi-k2.5"),
		Scenarios: llmtests.TestScenarios{
			TextCompletion:        true,
			TextCompletionStream:  true,
			SimpleChat:            true,
			CompletionStream:      true,
			MultiTurnConversation: true,
			ToolCalls:             true,
			MultipleToolCalls:     true,
			End2EndToolCalling:    true,
			AutomaticFunctionCall: true,
			ListModels:            true,
		},
	}

	t.Run("MoonshotTests", func(t *testing.T) {
		llmtests.RunAllComprehensiveTests(t, client, ctx, testConfig)
	})
	client.Shutdown()
}

// kimiResponsesWebSearchRequest is the web search request from Kimi's Responses
// documentation (https://platform.kimi.ai/docs/api/responses.md, "Web search"), with
// the tool's documented domain filter and content types, the include value that
// returns sources, and Kimi's cache write options.
const kimiResponsesWebSearchRequest = `{
  "model": "moonshot/kimi-k3.1-pro",
  "input": "What are the top news stories today?",
  "tools": [
    {"type": "web_search", "filters": {"allowed_domains": ["example.com"]}, "search_content_types": ["text", "image"]}
  ],
  "include": ["web_search_call.action.sources"],
  "prompt_cache_options": {"mode": "implicit", "ttl": "1h"}
}`

// kimiResponsesWebSearchResponse carries the web_search_call item from the same page
// at the front of output, and usage in the shape of its ResponsesUsage schema.
const kimiResponsesWebSearchResponse = `{"id":"resp_kimi","object":"response","created_at":1,"status":"completed","model":"kimi-k3.1-pro",` +
	`"output":[{"type":"web_search_call","id":"ws_dackjbcdo9rs73fo3oig","status":"completed","action":{"type":"search","query":"top news September 3 2026","sources":[{"type":"url","url":"https://example.com/news/1","title":"News headline"}]}},` +
	`{"type":"message","id":"msg_kimi","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Here are today's top stories.","annotations":[]}]}],` +
	`"usage":{"input_tokens":2048,"input_tokens_details":{"cached_tokens":1024,"cache_write_tokens":512},"output_tokens":64,"output_tokens_details":{"reasoning_tokens":16},"total_tokens":2112}}`

type moonshotUpstreamRequest struct {
	path          string
	authorization string
	xAPIKey       string
	body          []byte
}

// newMoonshotTestProvider returns a Moonshot provider whose base URL is a local
// server that records each request and answers with respond.
func newMoonshotTestProvider(t *testing.T, respond func(w http.ResponseWriter, body []byte)) (*moonshot.MoonshotProvider, string, <-chan moonshotUpstreamRequest) {
	t.Helper()
	requests := make(chan moonshotUpstreamRequest, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- moonshotUpstreamRequest{
			path: r.URL.Path, authorization: r.Header.Get("Authorization"), xAPIKey: r.Header.Get("x-api-key"), body: body,
		}
		respond(w, body)
	}))
	t.Cleanup(server.Close)
	provider, err := moonshot.NewMoonshotProvider(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{
		BaseURL: server.URL, AllowPrivateNetwork: true,
	}}, bifrost.NewNoOpLogger())
	if err != nil {
		t.Fatalf("NewMoonshotProvider: %v", err)
	}
	return provider, server.URL, requests
}

func moonshotTestKey() schemas.Key {
	return schemas.Key{Value: schemas.SecretVar{Val: "kimi-secret"}}
}

func moonshotPassthroughPostHook(_ *schemas.BifrostContext, response *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
	return response, err
}

// moonshotResponsesRequest parses a Responses body the way the /openai/v1/responses
// route does, so the test covers the fields a caller can actually send.
func moonshotResponsesRequest(t *testing.T, ctx *schemas.BifrostContext, body string) *schemas.BifrostResponsesRequest {
	t.Helper()
	var parsed openai.OpenAIResponsesRequest
	if err := schemas.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("parse Responses request: %v", err)
	}
	request := parsed.ToBifrostResponsesRequest(ctx)
	if request == nil || request.Provider != schemas.Moonshot {
		t.Fatalf("parsed request = %+v", request)
	}
	return request
}

// TXE-6547: a Moonshot Responses request reaches Kimi's own /v1/responses with the
// server-side web_search tool intact, and the web_search_call item and usage Kimi
// reports come back. Before, the request was converted to chat and the tool was lost.
func TestMoonshotResponsesCallsKimiResponsesAPIWithWebSearch(t *testing.T) {
	provider, _, requests := newMoonshotTestProvider(t, func(w http.ResponseWriter, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(kimiResponsesWebSearchResponse))
	})
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	response, bifrostErr := provider.Responses(ctx, moonshotTestKey(), moonshotResponsesRequest(t, ctx, kimiResponsesWebSearchRequest))
	if bifrostErr != nil {
		t.Fatalf("Responses: %+v", bifrostErr)
	}

	sent := <-requests
	if sent.path != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses", sent.path)
	}
	if sent.authorization != "Bearer kimi-secret" {
		t.Fatalf("Authorization = %q", sent.authorization)
	}
	if got := gjson.GetBytes(sent.body, "model").String(); got != "kimi-k3.1-pro" {
		t.Fatalf("upstream model = %q", got)
	}
	for path, want := range map[string]string{
		"tools":                gjson.Get(kimiResponsesWebSearchRequest, "tools").Raw,
		"include":              gjson.Get(kimiResponsesWebSearchRequest, "include").Raw,
		"prompt_cache_options": gjson.Get(kimiResponsesWebSearchRequest, "prompt_cache_options").Raw,
	} {
		assertSameJSON(t, "upstream "+path, gjson.GetBytes(sent.body, path).Raw, want)
	}

	encoded, err := schemas.Marshal(response)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	assertWebSearchCall(t, gjson.GetBytes(encoded, "output.0"))
	if got := gjson.GetBytes(encoded, "output.1.content.0.text").String(); got != "Here are today's top stories." {
		t.Fatalf("message text = %q", got)
	}
	usage := response.Usage
	if usage == nil || usage.InputTokens != 2048 || usage.OutputTokens != 64 || usage.TotalTokens != 2112 {
		t.Fatalf("usage = %+v", usage)
	}
	if usage.InputTokensDetails == nil || usage.InputTokensDetails.CachedReadTokens != 1024 || usage.InputTokensDetails.CachedWriteTokens != 512 {
		t.Fatalf("input token details = %+v", usage.InputTokensDetails)
	}
	if usage.OutputTokensDetails == nil || usage.OutputTokensDetails.ReasoningTokens != 16 {
		t.Fatalf("output token details = %+v", usage.OutputTokensDetails)
	}
}

// TXE-6547: a streamed Moonshot Responses request reads Kimi's own event stream; the
// web search events are relayed and the stream ends with response.completed carrying usage.
func TestMoonshotResponsesStreamEndsWithCompletedUsage(t *testing.T) {
	searchItem := gjson.Get(kimiResponsesWebSearchResponse, "output.0").Raw
	events := []string{
		`{"type":"response.created","sequence_number":0,"response":{"id":"resp_kimi","object":"response","created_at":1,"status":"in_progress","model":"kimi-k3.1-pro","output":[]}}`,
		`{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"web_search_call","id":"ws_dackjbcdo9rs73fo3oig","status":"in_progress"}}`,
		`{"type":"response.web_search_call.in_progress","sequence_number":2,"output_index":0,"item_id":"ws_dackjbcdo9rs73fo3oig"}`,
		`{"type":"response.web_search_call.searching","sequence_number":3,"output_index":0,"item_id":"ws_dackjbcdo9rs73fo3oig"}`,
		`{"type":"response.web_search_call.completed","sequence_number":4,"output_index":0,"item_id":"ws_dackjbcdo9rs73fo3oig"}`,
		`{"type":"response.output_item.done","sequence_number":5,"output_index":0,"item":` + searchItem + `}`,
		`{"type":"response.output_text.delta","sequence_number":6,"output_index":1,"content_index":0,"item_id":"msg_kimi","delta":"Here are today's top stories."}`,
		`{"type":"response.completed","sequence_number":7,"response":` + kimiResponsesWebSearchResponse + `}`,
	}
	provider, _, requests := newMoonshotTestProvider(t, func(w http.ResponseWriter, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", gjson.Get(event, "type").String(), event)
		}
	})
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	stream, bifrostErr := provider.ResponsesStream(ctx, moonshotPassthroughPostHook, nil, moonshotTestKey(), moonshotResponsesRequest(t, ctx, kimiResponsesWebSearchRequest))
	if bifrostErr != nil {
		t.Fatalf("ResponsesStream: %+v", bifrostErr)
	}
	var types []string
	var last *schemas.BifrostResponsesStreamResponse
	for chunk := range stream {
		if chunk.BifrostError != nil {
			t.Fatalf("stream returned error: %+v", chunk.BifrostError)
		}
		if chunk.BifrostResponsesStreamResponse != nil {
			last = chunk.BifrostResponsesStreamResponse
			types = append(types, string(last.Type))
		}
	}

	sent := <-requests
	if sent.path != "/v1/responses" || sent.authorization != "Bearer kimi-secret" {
		t.Fatalf("upstream path = %q, Authorization = %q", sent.path, sent.authorization)
	}
	if !gjson.GetBytes(sent.body, "stream").Bool() {
		t.Fatalf("upstream body is not a stream request: %s", sent.body)
	}
	assertSameJSON(t, "upstream tools", gjson.GetBytes(sent.body, "tools").Raw, gjson.Get(kimiResponsesWebSearchRequest, "tools").Raw)

	wantTypes := make([]string, 0, len(events))
	for _, event := range events {
		wantTypes = append(wantTypes, gjson.Get(event, "type").String())
	}
	if strings.Join(types, ",") != strings.Join(wantTypes, ",") {
		t.Fatalf("stream event types:\n got: %v\nwant: %v", types, wantTypes)
	}
	if last == nil || last.Type != schemas.ResponsesStreamResponseTypeCompleted || last.Response == nil || last.Response.Usage == nil {
		t.Fatalf("stream did not end with response.completed carrying usage: %+v", last)
	}
	usage := last.Response.Usage
	if usage.InputTokens != 2048 || usage.OutputTokens != 64 || usage.TotalTokens != 2112 ||
		usage.InputTokensDetails == nil || usage.InputTokensDetails.CachedReadTokens != 1024 || usage.InputTokensDetails.CachedWriteTokens != 512 {
		t.Fatalf("completed usage = %+v (details %+v)", usage, usage.InputTokensDetails)
	}
	encoded, err := schemas.Marshal(last.Response)
	if err != nil {
		t.Fatalf("marshal completed response: %v", err)
	}
	assertWebSearchCall(t, gjson.GetBytes(encoded, "output.0"))
}

// moonshotMessagesBody is a native Messages body with fields a conversion to chat
// would lose or re-encode: cache_control, a thinking block with a signature, key
// order, spacing and a non-ASCII character.
func moonshotMessagesBody(stream bool) string {
	body := `{"model":"kimi-k3.1-pro", "max_tokens":64,"system":[{"type":"text","text":"You are Kimi — helpful.","cache_control":{"type":"ephemeral"}}],` +
		`"thinking":{"type":"enabled","budget_tokens":1024},"metadata":{"user_id":"u_1"},` +
		`"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"greet","signature":"c2ln"},{"type":"text","text":"hello"}]},{"role":"user","content":"again"}]}`
	if stream {
		body = strings.Replace(body, `"max_tokens":64,`, `"max_tokens":64,"stream":true,`, 1)
	}
	return body
}

// TXE-6547: a native Messages request is posted to Kimi's Anthropic-compatible endpoint,
// {base}/anthropic/v1/messages (https://platform.kimi.ai/docs/api/messages.md), with
// Kimi's bearer auth; the request and response bodies are passed through byte for byte.
func TestMoonshotMessagesPassthroughToKimiAnthropicEndpoint(t *testing.T) {
	const responseBody = `{"id":"msg_kimi","type":"message","role":"assistant","model":"kimi-k3.1-pro","content":[{"type":"thinking","thinking":"greet","signature":"c2ln"},{"type":"text","text":"hello"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":128, "cache_read_input_tokens":4736,"cache_creation_input_tokens":64,"output_tokens":12}}`
	const streamBody = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_kimi\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"kimi-k3.1-pro\",\"content\":[],\"usage\":{\"input_tokens\":128,\"cache_read_input_tokens\":4736,\"output_tokens\":1}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":12}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	provider, baseURL, requests := newMoonshotTestProvider(t, func(w http.ResponseWriter, body []byte) {
		if gjson.GetBytes(body, "stream").Bool() {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(streamBody))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseBody))
	})

	capable, ok := interface{}(provider).(schemas.AnthropicMessagesCapable)
	if !ok {
		t.Fatal("Moonshot provider does not advertise Anthropic Messages capability")
	}
	if endpoint, enabled := capable.AnthropicMessagesEndpoint(); !enabled || endpoint != baseURL+"/anthropic" {
		t.Fatalf("endpoint = %q, enabled = %v", endpoint, enabled)
	}

	assertUpstream := func(t *testing.T, want string) {
		t.Helper()
		sent := <-requests
		if sent.path != "/anthropic/v1/messages" {
			t.Fatalf("upstream path = %q, want /anthropic/v1/messages", sent.path)
		}
		if sent.authorization != "Bearer kimi-secret" || sent.xAPIKey != "" {
			t.Fatalf("Authorization = %q, x-api-key = %q", sent.authorization, sent.xAPIKey)
		}
		if string(sent.body) != want {
			t.Fatalf("body sent to Kimi:\n got: %s\nwant: %s", sent.body, want)
		}
	}

	t.Run("buffered", func(t *testing.T) {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		body := moonshotMessagesBody(false)
		response, bifrostErr := provider.Passthrough(ctx, moonshotTestKey(), &schemas.BifrostPassthroughRequest{
			Method: http.MethodPost, Path: "/v1/messages", Body: []byte(body),
		})
		if bifrostErr != nil {
			t.Fatalf("Passthrough: %+v", bifrostErr)
		}
		assertUpstream(t, body)
		if response.StatusCode != http.StatusOK || string(response.Body) != responseBody {
			t.Fatalf("status = %d, response body changed:\n got: %s\nwant: %s", response.StatusCode, response.Body, responseBody)
		}
		usage := response.PassthroughUsage
		if usage == nil || usage.LLMUsage == nil || usage.LLMUsage.CompletionTokens != 12 {
			t.Fatalf("usage read from Kimi's response = %+v", usage)
		}
	})

	t.Run("stream", func(t *testing.T) {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		body := moonshotMessagesBody(true)
		stream, bifrostErr := provider.PassthroughStream(ctx, moonshotPassthroughPostHook, nil, moonshotTestKey(), &schemas.BifrostPassthroughRequest{
			Method: http.MethodPost, Path: "/v1/messages", Body: []byte(body),
		})
		if bifrostErr != nil {
			t.Fatalf("PassthroughStream: %+v", bifrostErr)
		}
		var relayed []byte
		for chunk := range stream {
			if chunk.BifrostError != nil {
				t.Fatalf("stream returned error: %+v", chunk.BifrostError)
			}
			if chunk.BifrostPassthroughResponse != nil {
				relayed = append(relayed, chunk.BifrostPassthroughResponse.Body...)
			}
		}
		assertUpstream(t, body)
		if string(relayed) != streamBody {
			t.Fatalf("stream bytes changed:\n got: %q\nwant: %q", relayed, streamBody)
		}
	})

	// Only the Messages create path is served; Kimi's Anthropic endpoint is not a
	// general passthrough door.
	t.Run("other paths stay unsupported", func(t *testing.T) {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		for _, path := range []string{"/v1/messages/count_tokens", "/v1/chat/completions", "/v1/files"} {
			req := &schemas.BifrostPassthroughRequest{Method: http.MethodPost, Path: path, Body: []byte(`{}`)}
			if _, bifrostErr := provider.Passthrough(ctx, moonshotTestKey(), req); bifrostErr == nil {
				t.Fatalf("Passthrough(%s) was not refused", path)
			}
			if _, bifrostErr := provider.PassthroughStream(ctx, moonshotPassthroughPostHook, nil, moonshotTestKey(), req); bifrostErr == nil {
				t.Fatalf("PassthroughStream(%s) was not refused", path)
			}
		}
		select {
		case sent := <-requests:
			t.Fatalf("an unsupported path reached Kimi: %s", sent.path)
		default:
		}
	})
}

// assertWebSearchCall checks that item is the web_search_call Kimi returned: the item
// Tokun counts as one web search, with the query and sources Kimi reported.
func assertWebSearchCall(t *testing.T, item gjson.Result) {
	t.Helper()
	want := gjson.Get(kimiResponsesWebSearchResponse, "output.0")
	for _, field := range []string{"type", "id", "status"} {
		if got := item.Get(field).String(); got != want.Get(field).String() {
			t.Errorf("web_search_call %s = %q, want %q", field, got, want.Get(field).String())
		}
	}
	assertSameJSON(t, "web_search_call action", item.Get("action").Raw, want.Get("action").Raw)
}

// assertSameJSON compares two JSON documents by value, ignoring key order and spacing.
func assertSameJSON(t *testing.T, what, got, want string) {
	t.Helper()
	var gotValue, wantValue interface{}
	if err := schemas.Unmarshal([]byte(got), &gotValue); err != nil {
		t.Fatalf("%s is not JSON (%v): %q", what, err, got)
	}
	if err := schemas.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("%s: want is not JSON (%v): %q", what, err, want)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Errorf("%s:\n got: %s\nwant: %s", what, got, want)
	}
}
