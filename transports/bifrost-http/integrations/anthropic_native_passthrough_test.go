package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
	"github.com/valyala/fasthttp"
)

type nativeMessagesTestAccount struct {
	configs map[schemas.ModelProvider]*schemas.ProviderConfig
	keys    map[schemas.ModelProvider][]schemas.Key
}

func (a *nativeMessagesTestAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	providers := make([]schemas.ModelProvider, 0, len(a.configs))
	for provider := range a.configs {
		providers = append(providers, provider)
	}
	return providers, nil
}

func (a *nativeMessagesTestAccount) GetKeysForProvider(_ context.Context, provider schemas.ModelProvider) ([]schemas.Key, error) {
	return a.keys[provider], nil
}

func TestAnthropicMessagesRouteNativeXAIEndToEnd(t *testing.T) {
	requestBody := []byte(`{"model":"xai/grok-4.5","max_tokens":64,"system":[{"type":"text","text":"cached","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"use a tool"}]}`)
	responseBody := []byte(`{"id":"msg_xai","type":"message","role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{"q":"native"}}],"usage":{"input_tokens":128,"cache_read_input_tokens":4736,"cache_creation_input_tokens":64,"output_tokens":12}}`)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "xai-secret" {
			t.Errorf("x-api-key = %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		if bytes.Contains(body, []byte(`xai/grok-4.5`)) || !bytes.Contains(body, []byte(`"model":"grok-4.5"`)) {
			t.Errorf("upstream model was not normalized: %s", body)
		}
		if !bytes.Contains(body, []byte(`"cache_control":{"type":"ephemeral"}`)) {
			t.Errorf("cache_control was lost: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(responseBody)
	}))
	defer upstream.Close()

	account := &nativeMessagesTestAccount{
		configs: map[schemas.ModelProvider]*schemas.ProviderConfig{
			schemas.XAI: {NetworkConfig: schemas.NetworkConfig{BaseURL: upstream.URL, AllowPrivateNetwork: true}},
		},
		keys: map[schemas.ModelProvider][]schemas.Key{
			schemas.XAI: {{ID: "xai-key", Value: schemas.SecretVar{Val: "xai-secret"}, Models: schemas.WhiteList{"*"}, Weight: 1}},
		},
	}
	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: account, Logger: bifrost.NewNoOpLogger(), InitialPoolSize: 1,
	})
	if err != nil {
		t.Fatalf("init Bifrost: %v", err)
	}
	defer client.Shutdown()

	route := createAnthropicMessagesRouteConfig("/anthropic", bifrost.NewNoOpLogger())[0]
	handler := NewGenericRouter(client, &mockHandlerStore{}, nil, nil, nil, bifrost.NewNoOpLogger()).createHandler(route)
	httpCtx := &fasthttp.RequestCtx{}
	httpCtx.Request.Header.SetMethod(fasthttp.MethodPost)
	httpCtx.Request.SetRequestURI("/anthropic/v1/messages")
	httpCtx.Request.Header.Set("anthropic-version", "2023-06-01")
	httpCtx.Request.SetBody(requestBody)
	handler(httpCtx)

	if httpCtx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("status = %d, body = %s", httpCtx.Response.StatusCode(), httpCtx.Response.Body())
	}
	if !bytes.Equal(httpCtx.Response.Body(), responseBody) {
		t.Fatalf("response bytes changed:\n got: %s\nwant: %s", httpCtx.Response.Body(), responseBody)
	}
}

// TXE-6547: /anthropic/v1/messages with moonshot/<id> is passed through to Kimi's
// Anthropic-compatible endpoint ({base}/anthropic/v1/messages, bearer auth). Kimi
// receives the caller's bytes with only the provider prefix removed from the model id,
// and the caller receives Kimi's response bytes.
func TestAnthropicMessagesRouteNativeMoonshotEndToEnd(t *testing.T) {
	requestBody := []byte(`{"model":"moonshot/kimi-k3.1-pro", "max_tokens":64,"system":[{"type":"text","text":"You are Kimi — helpful.","cache_control":{"type":"ephemeral"}}],"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"greet","signature":"c2ln"},{"type":"text","text":"hello"}]},{"role":"user","content":"again"}]}`)
	wantUpstream := bytes.Replace(requestBody, []byte(`"model":"moonshot/kimi-k3.1-pro"`), []byte(`"model":"kimi-k3.1-pro"`), 1)
	responseBody := []byte(`{"id":"msg_kimi","type":"message","role":"assistant","model":"kimi-k3.1-pro","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":128, "cache_read_input_tokens":4736,"cache_creation_input_tokens":64,"output_tokens":12}}`)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/anthropic/v1/messages" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer kimi-secret" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("x-api-key"); got != "" {
			t.Errorf("x-api-key = %q, want none", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		if !bytes.Equal(body, wantUpstream) {
			t.Errorf("body sent to Kimi:\n got: %s\nwant: %s", body, wantUpstream)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(responseBody)
	}))
	defer upstream.Close()

	account := &nativeMessagesTestAccount{
		configs: map[schemas.ModelProvider]*schemas.ProviderConfig{
			schemas.Moonshot: {NetworkConfig: schemas.NetworkConfig{BaseURL: upstream.URL, AllowPrivateNetwork: true}},
		},
		keys: map[schemas.ModelProvider][]schemas.Key{
			schemas.Moonshot: {{ID: "kimi-key", Value: schemas.SecretVar{Val: "kimi-secret"}, Models: schemas.WhiteList{"*"}, Weight: 1}},
		},
	}
	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: account, Logger: bifrost.NewNoOpLogger(), InitialPoolSize: 1,
	})
	if err != nil {
		t.Fatalf("init Bifrost: %v", err)
	}
	defer client.Shutdown()

	route := createAnthropicMessagesRouteConfig("/anthropic", bifrost.NewNoOpLogger())[0]
	handler := NewGenericRouter(client, &mockHandlerStore{}, nil, nil, nil, bifrost.NewNoOpLogger()).createHandler(route)
	httpCtx := &fasthttp.RequestCtx{}
	httpCtx.Request.Header.SetMethod(fasthttp.MethodPost)
	httpCtx.Request.SetRequestURI("/anthropic/v1/messages")
	httpCtx.Request.Header.Set("anthropic-version", "2023-06-01")
	httpCtx.Request.Header.Set("x-api-key", "caller-credential")
	httpCtx.Request.SetBody(requestBody)
	handler(httpCtx)

	if httpCtx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("status = %d, body = %s", httpCtx.Response.StatusCode(), httpCtx.Response.Body())
	}
	if !bytes.Equal(httpCtx.Response.Body(), responseBody) {
		t.Fatalf("response bytes changed:\n got: %s\nwant: %s", httpCtx.Response.Body(), responseBody)
	}
}

func (a *nativeMessagesTestAccount) GetConfigForProvider(provider schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	return a.configs[provider], nil
}

func TestAnthropicMessagesNativePassthroughSelection(t *testing.T) {
	account := &nativeMessagesTestAccount{configs: map[schemas.ModelProvider]*schemas.ProviderConfig{
		schemas.XAI: {
			NetworkConfig: schemas.NetworkConfig{BaseURL: "https://api.x.ai"},
		},
		schemas.OpenAI: {
			NetworkConfig: schemas.NetworkConfig{BaseURL: "https://api.openai.com"},
		},
	}}
	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: account, Logger: bifrost.NewNoOpLogger(), InitialPoolSize: 1,
	})
	if err != nil {
		t.Fatalf("init Bifrost: %v", err)
	}
	defer client.Shutdown()

	selector := createAnthropicMessagesNativePassthrough("/anthropic")
	rawBody := []byte(`{"model":"xai/grok-4.5","max_tokens":64,"system":[{"type":"text","text":"cached","cache_control":{"type":"ephemeral"}}],"messages":[]}`)
	httpCtx := &fasthttp.RequestCtx{}
	httpCtx.Request.Header.SetMethod(fasthttp.MethodPost)
	httpCtx.Request.SetRequestURI("/anthropic/v1/messages")

	passthroughReq, selected, err := selector(client, httpCtx, schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), &struct{}{}, rawBody)
	if err != nil || selected || passthroughReq != nil {
		t.Fatal("selector should reject a non-Anthropic request instance")
	}

	parsed := createAnthropicMessagesRouteConfig("/anthropic", bifrost.NewNoOpLogger())[0].GetRequestTypeInstance(context.Background())
	if err := parseJSONRequestBody(rawBody, parsed); err != nil {
		t.Fatalf("parse request: %v", err)
	}
	passthroughReq, selected, err = selector(client, httpCtx, schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), parsed, rawBody)
	if err != nil {
		t.Fatalf("select xAI passthrough: %v", err)
	}
	if !selected || passthroughReq == nil {
		t.Fatal("xAI Messages request did not select native passthrough")
	}
	if passthroughReq.Provider != schemas.XAI || passthroughReq.Model != "grok-4.5" || passthroughReq.Path != "/v1/messages" {
		t.Fatalf("passthrough routing = %+v", passthroughReq)
	}
	if bytes.Contains(passthroughReq.Body, []byte(`xai/grok-4.5`)) || !bytes.Contains(passthroughReq.Body, []byte(`"model":"grok-4.5"`)) {
		t.Fatalf("provider-prefixed model was not normalized: %s", passthroughReq.Body)
	}
	if !bytes.Contains(passthroughReq.Body, []byte(`"cache_control":{"type":"ephemeral"}`)) {
		t.Fatalf("cache_control was lost: %s", passthroughReq.Body)
	}

	openAIBody := []byte(`{"model":"openai/gpt-5","max_tokens":64,"messages":[]}`)
	openAIParsed := createAnthropicMessagesRouteConfig("/anthropic", bifrost.NewNoOpLogger())[0].GetRequestTypeInstance(context.Background())
	if err := parseJSONRequestBody(openAIBody, openAIParsed); err != nil {
		t.Fatalf("parse OpenAI request: %v", err)
	}
	passthroughReq, selected, err = selector(client, httpCtx, schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), openAIParsed, openAIBody)
	if err != nil || selected || passthroughReq != nil {
		t.Fatalf("OpenAI provider unexpectedly selected native Messages: req=%+v selected=%v err=%v", passthroughReq, selected, err)
	}
}

// TXE-5193: the xAI native Messages passthrough forwards the caller's body to xAI
// byte-for-byte, system included. The only rewrite allowed is the provider prefix on
// the top-level model value, which routing needs; an unprefixed body is forwarded
// untouched. The large-payload path is covered through a test hook: nothing in this
// build installs one, so the path is unreachable in production today.

const (
	xaiByteExactCallerModel   = "xai/grok-4.5"
	xaiByteExactUpstreamModel = "grok-4.5"
)

// xaiByteExactBody builds a Claude Code shaped body with what a re-encode or a strip
// would change: Anthropic-only fields, a billing-header system block beside a cached
// one, thinking and tool blocks, unknown fields, odd whitespace and unicode escapes.
// padding adds that many bytes to a user message, for the large-payload path.
func xaiByteExactBody(model string, stream bool, padding int) []byte {
	streamField := ""
	if stream {
		streamField = "\"stream\" :true,\n"
	}
	return []byte("{\"model\":\"" + model + "\",\n" +
		"  \"max_tokens\":\t1024 ,\n" + streamField +
		"  \"safeguards\":{\"cyber\":{\"level\":\"strict\"},\"flags\":[1,2.50,true,null]},\n" +
		"  \"diagnostics\" : {\"session\":\"s-1\",\"trace\":\"\\u00e9\\u2028\\/x\"},\n" +
		"  \"system\":[{\"type\":\"text\",\"text\":\"x-anthropic-billing-header: cc_version=2.1.0.abc; cc_entrypoint=cli; cch=00000;\"},\n" +
		"    {\"type\":\"text\",\"text\":\"You are Claude Code.\\nCaf\\u00e9 \\ud83d\\ude00\",\"cache_control\":{\"type\":\"ephemeral\",\"ttl\":\"1h\"}}],\n" +
		"  \"thinking\":{\"type\":\"enabled\",\"budget_tokens\":512},\n" +
		"  \"tools\":[{\"name\":\"lookup\",\"description\":\"find\",\"input_schema\":{\"type\":\"object\",\"properties\":{\"q\":{\"type\":\"string\"}}},\"cache_control\":{\"type\":\"ephemeral\"}}],\n" +
		"  \"messages\":[\n" +
		"    {\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"look it up" + strings.Repeat("x", padding) + "\"}]},\n" +
		"    {\"role\":\"assistant\",\"content\":[{\"type\":\"thinking\",\"thinking\":\"plan\",\"signature\":\"c2lnbmF0dXJl\"},{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"lookup\",\"input\":{\"q\":\"native\"}}]},\n" +
		"    {\"role\":\"user\",\"content\":[{\"type\":\"tool_result\",\"tool_use_id\":\"toolu_1\",\"content\":\"ok\",\"cache_control\":{\"type\":\"ephemeral\",\"ttl\":\"5m\"}}]}\n" +
		"  ],\n" +
		"  \"metadata\":{\"user_id\":\"u-1\"},\"zz_unknown\":{\"b\":1,\"a\":[]}  }")
}

// xaiByteExactWant is the body xAI must receive: the caller's bytes with only the
// provider prefix taken off the model value.
func xaiByteExactWant(t *testing.T, body []byte) []byte {
	t.Helper()
	from := []byte(`"model":"` + xaiByteExactCallerModel + `"`)
	if bytes.Count(body, from) != 1 {
		t.Fatalf("fixture must carry the model value exactly once")
	}
	return bytes.Replace(body, from, []byte(`"model":"`+xaiByteExactUpstreamModel+`"`), 1)
}

const xaiByteExactResponse = `{"id":"msg_xai","type":"message","role":"assistant","model":"grok-4.5","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":128,"cache_read_input_tokens":4736,"cache_creation_input_tokens":64,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":64},"output_tokens":12}}`

const xaiByteExactSSE = "event: message_start\n" +
	"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_xai\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"grok-4.5\",\"content\":[],\"usage\":{\"input_tokens\":128,\"cache_read_input_tokens\":4736,\"cache_creation_input_tokens\":64,\"output_tokens\":1}}}\n\n" +
	"event: ping\n" +
	"data: {\"type\":\"ping\"}\n\n" +
	"event: content_block_start\n" +
	"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Caf\\u00e9\"}}\n\n" +
	"event: content_block_stop\n" +
	"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: message_delta\n" +
	"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":12}}\n\n" +
	"event: message_stop\n" +
	"data: {\"type\":\"message_stop\"}\n\n"

// passthroughUsagePlugin records the passthrough usage that reaches PostLLMHook.
type passthroughUsagePlugin struct {
	mu    sync.Mutex
	usage []*schemas.BifrostPassthroughUsage
}

func (p *passthroughUsagePlugin) GetName() string { return "txe-5193-usage" }
func (p *passthroughUsagePlugin) Cleanup() error  { return nil }
func (p *passthroughUsagePlugin) PreRequestHook(*schemas.BifrostContext, *schemas.BifrostRequest) error {
	return nil
}
func (p *passthroughUsagePlugin) PreLLMHook(_ *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	return req, nil, nil
}
func (p *passthroughUsagePlugin) PostLLMHook(_ *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	if resp != nil && resp.PassthroughResponse != nil && resp.PassthroughResponse.PassthroughUsage != nil {
		p.mu.Lock()
		p.usage = append(p.usage, resp.PassthroughResponse.PassthroughUsage)
		p.mu.Unlock()
	}
	return resp, bifrostErr, nil
}

func (p *passthroughUsagePlugin) assertCacheUsage(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.usage) == 0 || p.usage[len(p.usage)-1].LLMUsage == nil {
		t.Fatal("no passthrough usage reached PostLLMHook")
	}
	usage := p.usage[len(p.usage)-1].LLMUsage
	if usage.PromptTokensDetails == nil {
		t.Fatalf("cache usage lost: %+v", usage)
	}
	if read, write := usage.PromptTokensDetails.CachedReadTokens, usage.PromptTokensDetails.CachedWriteTokens; read != 4736 || write != 64 || usage.CompletionTokens != 12 {
		t.Fatalf("usage = cache_read %d, cache_creation %d, completion %d; want 4736, 64, 12", read, write, usage.CompletionTokens)
	}
}

func TestAnthropicMessagesRouteXAIForwardsBodyByteExact(t *testing.T) {
	cases := []struct {
		name         string
		unprefixed   bool
		stream       bool
		largePayload bool
	}{
		{name: "unary"},
		{name: "stream", stream: true},
		{name: "large_payload_unary", largePayload: true},
		{name: "large_payload_stream", stream: true, largePayload: true},
		{name: "unprefixed_unary", unprefixed: true},
		{name: "unprefixed_stream", unprefixed: true, stream: true},
		{name: "unprefixed_large_payload_unary", unprefixed: true, largePayload: true},
		{name: "unprefixed_large_payload_stream", unprefixed: true, stream: true, largePayload: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			padding := 0
			if tc.largePayload {
				// Past the 256KB prefix the large-payload model rewrite scans.
				padding = 300 * 1024
			}
			callerModel := xaiByteExactCallerModel
			if tc.unprefixed {
				callerModel = xaiByteExactUpstreamModel
			}
			requestBody := xaiByteExactBody(callerModel, tc.stream, padding)
			want := requestBody
			if !tc.unprefixed {
				want = xaiByteExactWant(t, requestBody)
			}
			contentType, responseBody := "application/json", []byte(xaiByteExactResponse)
			if tc.stream {
				contentType, responseBody = "text/event-stream", []byte(xaiByteExactSSE)
			}

			var mu sync.Mutex
			var upstreamBody []byte
			upstreamCalls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read upstream body: %v", err)
				}
				mu.Lock()
				upstreamBody, upstreamCalls = body, upstreamCalls+1
				mu.Unlock()
				if r.URL.Path != "/v1/messages" {
					t.Errorf("upstream path = %q", r.URL.Path)
				}
				if got := r.Header.Get("x-api-key"); got != "xai-secret" {
					t.Errorf("x-api-key = %q", got)
				}
				w.Header().Set("Content-Type", contentType)
				_, _ = w.Write(responseBody)
			}))
			defer upstream.Close()

			plugin := &passthroughUsagePlugin{}
			account := &nativeMessagesTestAccount{
				configs: map[schemas.ModelProvider]*schemas.ProviderConfig{
					schemas.XAI: {NetworkConfig: schemas.NetworkConfig{BaseURL: upstream.URL, AllowPrivateNetwork: true}},
				},
				keys: map[schemas.ModelProvider][]schemas.Key{
					schemas.XAI: {{ID: "xai-shared", Value: schemas.SecretVar{Val: "xai-secret"}, Models: schemas.WhiteList{"*"}, Weight: 1}},
				},
			}
			client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
				Account: account, Logger: bifrost.NewNoOpLogger(), InitialPoolSize: 1,
				LLMPlugins: []schemas.LLMPlugin{plugin},
			})
			if err != nil {
				t.Fatalf("init Bifrost: %v", err)
			}
			defer client.Shutdown()

			router := NewGenericRouter(client, &mockHandlerStore{}, nil, nil, nil, bifrost.NewNoOpLogger())
			if tc.largePayload {
				router.SetLargePayloadHook(func(_ *fasthttp.RequestCtx, bifrostCtx *schemas.BifrostContext, _ RouteConfigType) (bool, error) {
					bifrostCtx.SetValue(schemas.BifrostContextKeyLargePayloadMode, true)
					bifrostCtx.SetValue(schemas.BifrostContextKeyLargePayloadReader, io.Reader(bytes.NewReader(requestBody)))
					bifrostCtx.SetValue(schemas.BifrostContextKeyLargePayloadContentLength, len(requestBody))
					bifrostCtx.SetValue(schemas.BifrostContextKeyLargePayloadContentType, "application/json")
					bifrostCtx.SetValue(schemas.BifrostContextKeyLargePayloadMetadata, &schemas.LargePayloadMetadata{
						Model: callerModel, StreamRequested: schemas.Ptr(tc.stream),
					})
					return true, nil
				})
			}
			handler := router.createHandler(createAnthropicMessagesRouteConfig("/anthropic", bifrost.NewNoOpLogger())[0])
			httpCtx := &fasthttp.RequestCtx{}
			httpCtx.Request.Header.SetMethod(fasthttp.MethodPost)
			httpCtx.Request.SetRequestURI("/anthropic/v1/messages")
			httpCtx.Request.Header.SetContentType("application/json")
			httpCtx.Request.Header.Set("anthropic-version", "2023-06-01")
			if tc.unprefixed {
				// An unprefixed model names its provider in the header instead.
				httpCtx.Request.Header.Set("x-model-provider", string(schemas.XAI))
			}
			httpCtx.Request.SetBody(requestBody)
			handler(httpCtx)

			if httpCtx.Response.StatusCode() != fasthttp.StatusOK {
				t.Fatalf("status = %d, body = %s", httpCtx.Response.StatusCode(), httpCtx.Response.Body())
			}
			clientBody := httpCtx.Response.Body()
			if tc.stream && httpCtx.Response.IsBodyStream() {
				clientBody, err = io.ReadAll(httpCtx.Response.BodyStream())
				if err != nil {
					t.Fatalf("read client stream: %v", err)
				}
			}
			if !bytes.Equal(clientBody, responseBody) {
				t.Fatalf("client bytes changed:\n got: %q\nwant: %q", clientBody, responseBody)
			}

			mu.Lock()
			got, calls := upstreamBody, upstreamCalls
			mu.Unlock()
			if calls != 1 {
				t.Fatalf("upstream calls = %d, want 1", calls)
			}
			if model := gjson.GetBytes(got, "model").Str; model != xaiByteExactUpstreamModel {
				t.Fatalf("upstream model = %q, want %q", model, xaiByteExactUpstreamModel)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("upstream body is not byte-exact (got %d bytes, want %d):\n got: %s\nwant: %s", len(got), len(want), truncateForLog(got), truncateForLog(want))
			}
			plugin.assertCacheUsage(t)
		})
	}
}

func truncateForLog(body []byte) []byte {
	if len(body) <= 4096 {
		return body
	}
	return append(append([]byte(nil), body[:4096]...), "…"...)
}

// Native Anthropic Messages calls take the byte-preserving passthrough, not the typed bridge:
// the caller's body and anthropic-beta reach Anthropic as sent (only a provider-prefixed model is
// normalized), Anthropic's bytes come back unchanged, and the routed-identity headers name the
// path. Upstream harness rows that assert typed rewrites or extra_fields.raw_request on this
// route are tagged [NATIVE-PASSTHROUGH]; this is the offline coverage that replaces them.
func TestAnthropicMessagesRouteNativeAnthropicEndToEnd(t *testing.T) {
	// Fields the typed bridge would rewrite or derive a beta for: disabled thinking, a forced
	// tool choice, a dated computer tool, an effort-only system message and string fallbacks.
	const clientBeta = "mid-conversation-output-config-2026-07-01"
	requestBody := []byte(`{"model":"anthropic/claude-opus-5-5","max_tokens":64,"thinking":{"type":"disabled"},"output_config":{"effort":"high"},"fallbacks":["anthropic/claude-sonnet-4-6"],"tool_choice":{"type":"any","disable_parallel_tool_use":true},"tools":[{"type":"computer_20251124","name":"computer","display_width_px":1024,"display_height_px":768},{"name":"lookup","description":"Harness probe tool","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}],"messages":[{"role":"system","content":[],"output_config":{"effort":"low"}},{"role":"user","content":"use a tool"}]}`)
	wantUpstream := bytes.Replace(requestBody, []byte(`"anthropic/claude-opus-5-5"`), []byte(`"claude-opus-5-5"`), 1)
	responseBody := []byte(`{"id":"msg_native","type":"message","role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"sig"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"native"}}],"stop_reason":"tool_use","usage":{"input_tokens":128,"cache_read_input_tokens":4736,"cache_creation_input_tokens":64,"output_tokens":12}}`)
	streamBody := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_native\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":128,\"cache_read_input_tokens\":4736,\"output_tokens\":1}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"plan\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"lookup\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\":\\\"native\\\"}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":12}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "anthropic-secret" {
			t.Errorf("x-api-key = %q", got)
		}
		if got := r.Header.Values("anthropic-beta"); len(got) != 1 || got[0] != clientBeta {
			t.Errorf("anthropic-beta = %q, want the caller's %q with nothing derived", got, clientBeta)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		streaming := bytes.Contains(body, []byte(`"stream":true`))
		want := wantUpstream
		if streaming {
			want = bytes.Replace(wantUpstream, []byte(`"max_tokens":64,`), []byte(`"max_tokens":64,"stream":true,`), 1)
		}
		if !bytes.Equal(body, want) {
			t.Errorf("upstream body changed:\n got: %s\nwant: %s", body, want)
		}
		if streaming {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write(streamBody)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(responseBody)
	}))
	defer upstream.Close()

	account := &nativeMessagesTestAccount{
		configs: map[schemas.ModelProvider]*schemas.ProviderConfig{
			schemas.Anthropic: {NetworkConfig: schemas.NetworkConfig{BaseURL: upstream.URL, AllowPrivateNetwork: true}},
		},
		keys: map[schemas.ModelProvider][]schemas.Key{
			schemas.Anthropic: {{ID: "anthropic-key", Value: schemas.SecretVar{Val: "anthropic-secret"}, Models: schemas.WhiteList{"*"}, Weight: 1}},
		},
	}
	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: account, Logger: bifrost.NewNoOpLogger(), InitialPoolSize: 1,
	})
	if err != nil {
		t.Fatalf("init Bifrost: %v", err)
	}
	defer client.Shutdown()

	route := createAnthropicMessagesRouteConfig("/anthropic", bifrost.NewNoOpLogger())[0]
	handler := NewGenericRouter(client, &mockHandlerStore{}, nil, nil, nil, bifrost.NewNoOpLogger()).createHandler(route)
	call := func(body []byte) *fasthttp.RequestCtx {
		httpCtx := &fasthttp.RequestCtx{}
		httpCtx.Request.Header.SetMethod(fasthttp.MethodPost)
		httpCtx.Request.SetRequestURI("/anthropic/v1/messages")
		httpCtx.Request.Header.Set("anthropic-version", "2023-06-01")
		httpCtx.Request.Header.Set("anthropic-beta", clientBeta)
		httpCtx.Request.Header.Set("x-bf-send-back-raw-request", "true")
		httpCtx.Request.SetBody(body)
		handler(httpCtx)
		return httpCtx
	}

	t.Run("unary", func(t *testing.T) {
		httpCtx := call(requestBody)
		if httpCtx.Response.StatusCode() != fasthttp.StatusOK {
			t.Fatalf("status = %d, body = %s", httpCtx.Response.StatusCode(), httpCtx.Response.Body())
		}
		if !bytes.Equal(httpCtx.Response.Body(), responseBody) {
			t.Fatalf("response bytes changed:\n got: %s\nwant: %s", httpCtx.Response.Body(), responseBody)
		}
		if got := string(httpCtx.Response.Header.Peek("x-bifrost-request-type")); got != string(schemas.PassthroughRequest) {
			t.Errorf("x-bifrost-request-type = %q, want passthrough", got)
		}
		// x-bifrost-passthrough-path is written only under headers placement, so these two
		// always-present headers are what identify the native path to a client.
		if got := string(httpCtx.Response.Header.Peek("x-bifrost-provider")); got != string(schemas.Anthropic) {
			t.Errorf("x-bifrost-provider = %q, want anthropic", got)
		}
	})

	t.Run("stream", func(t *testing.T) {
		httpCtx := call(bytes.Replace(requestBody, []byte(`"max_tokens":64,`), []byte(`"max_tokens":64,"stream":true,`), 1))
		if httpCtx.Response.StatusCode() != fasthttp.StatusOK {
			t.Fatalf("status = %d, body = %s", httpCtx.Response.StatusCode(), httpCtx.Response.Body())
		}
		var got []byte
		if stream := httpCtx.Response.BodyStream(); stream != nil {
			if got, err = io.ReadAll(stream); err != nil {
				t.Fatalf("read stream: %v", err)
			}
		} else {
			got = httpCtx.Response.Body()
		}
		// Heartbeat comments may interleave; every upstream frame must arrive verbatim and in order.
		var frames []byte
		for _, block := range bytes.Split(got, []byte("\n\n")) {
			if len(block) > 0 && !bytes.HasPrefix(bytes.TrimLeft(block, "\n"), []byte(":")) {
				frames = append(frames, bytes.TrimLeft(block, "\n")...)
				frames = append(frames, '\n', '\n')
			}
		}
		if !bytes.Equal(frames, streamBody) {
			t.Fatalf("SSE frames changed:\n got: %q\nwant: %q", frames, streamBody)
		}
	})
}

// nativeMessagesProviders is every provider that implements
// schemas.AnthropicMessagesCapable, with the model a caller names and the path its
// Anthropic-compatible endpoint serves Messages on. A provider that starts implementing
// the interface must get a row: TestAnthropicNativeMessagesRedactionCoversEveryCapableProvider
// fails until it has one, and the redaction tests below then run for it.
var nativeMessagesProviders = []struct {
	provider     schemas.ModelProvider
	model        string
	upstreamPath string
}{
	{schemas.Anthropic, "claude-sonnet-4-5", "/v1/messages"},
	{schemas.XAI, "grok-4.5", "/v1/messages"},
	{schemas.Moonshot, "kimi-k3.1-pro", "/anthropic/v1/messages"},
}

const (
	redactionSecret      = "4111-1111-1111-1111"
	redactionPlaceholder = "[CARD]"
)

// redactionGuardrailPlugin stands in for a runtime-redaction guardrail. That plugin is
// not in this repository; what the gateway owes it is the Anthropic raw-body adapters on
// the request context, which are the only way to change a body that is forwarded as bytes.
// This double uses each adapter the way its contract in core/schemas/redaction.go
// describes: the request rewriter on the body before it is sent, the response
// transformer on a buffered body, the stream codec on each SSE event.
type redactionGuardrailPlugin struct {
	mu      sync.Mutex
	missing []string
}

func (p *redactionGuardrailPlugin) GetName() string { return "txe-6965-redaction-guardrail" }
func (p *redactionGuardrailPlugin) Cleanup() error  { return nil }
func (p *redactionGuardrailPlugin) PreRequestHook(*schemas.BifrostContext, *schemas.BifrostRequest) error {
	return nil
}

func (p *redactionGuardrailPlugin) miss(what string) {
	p.mu.Lock()
	p.missing = append(p.missing, what)
	p.mu.Unlock()
}

func (p *redactionGuardrailPlugin) PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	if req == nil || req.PassthroughRequest == nil {
		return req, nil, nil
	}
	if _, ok := ctx.Value(schemas.BifrostContextKeyRawRequestBodyTextTransformer).(schemas.RawRequestBodyTextTransformer); !ok {
		p.miss("request transformer at PreLLMHook")
	}
	rewriter, ok := ctx.Value(schemas.BifrostContextKeyRawRequestBodyTextRewriter).(schemas.RawRequestBodyTextRewriter)
	if !ok {
		p.miss("request rewriter at PreLLMHook")
		return req, nil, nil
	}
	body, err := rewriter(append([]byte(nil), req.PassthroughRequest.Body...), map[string]string{redactionSecret: redactionPlaceholder})
	if err != nil {
		return req, nil, err
	}
	req.PassthroughRequest.Body = body
	return req, nil, nil
}

func (p *redactionGuardrailPlugin) PostLLMHook(ctx *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	if resp == nil || resp.PassthroughResponse == nil || len(resp.PassthroughResponse.Body) == 0 {
		return resp, bifrostErr, nil
	}
	body := resp.PassthroughResponse.Body
	if !bytes.Contains(body, []byte("data: ")) {
		transformer, ok := ctx.Value(schemas.BifrostContextKeyRawResponseTextTransformer).(schemas.RawResponseTextTransformer)
		if !ok {
			p.miss("response transformer at PostLLMHook")
			return resp, bifrostErr, nil
		}
		text := gjson.GetBytes(body, "content.0.text").String()
		rewritten, err := transformer(append([]byte(nil), body...), []schemas.TextRewrite{{
			TargetID: schemas.TextTargetIDForIndex(0), Original: text,
			Replacement: strings.ReplaceAll(text, redactionSecret, redactionPlaceholder),
		}})
		if err != nil {
			return resp, bifrostErr, err
		}
		resp.PassthroughResponse.Body = rewritten.([]byte)
		return resp, bifrostErr, nil
	}
	codec, ok := ctx.Value(schemas.BifrostContextKeyRawStreamTextCodec).(schemas.RawStreamTextCodec)
	if !ok {
		p.miss("stream codec at PostLLMHook")
		return resp, bifrostErr, nil
	}
	lines := strings.Split(string(body), "\n")
	for i, line := range lines {
		event, found := strings.CutPrefix(line, "data: ")
		if !found {
			continue
		}
		inspected, eligible, err := codec.Inspect(event)
		if err != nil {
			return resp, bifrostErr, err
		}
		if !eligible || !strings.Contains(inspected.Text, redactionSecret) {
			continue
		}
		rewritten, err := codec.Rewrite(event, strings.ReplaceAll(inspected.Text, redactionSecret, redactionPlaceholder))
		if err != nil {
			return resp, bifrostErr, err
		}
		lines[i] = "data: " + rewritten
	}
	resp.PassthroughResponse.Body = []byte(strings.Join(lines, "\n"))
	return resp, bifrostErr, nil
}

// TXE-6965: the table above names every provider that serves native Messages. The set is
// read from the providers themselves, so a provider that starts implementing
// AnthropicMessagesEndpoint fails here until it has a row, and with the row it is run
// through the redaction cases below.
func TestAnthropicNativeMessagesRedactionCoversEveryCapableProvider(t *testing.T) {
	configs := map[schemas.ModelProvider]*schemas.ProviderConfig{}
	for _, provider := range schemas.StandardProviders {
		configs[provider] = &schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: "https://native-messages.example"}}
	}
	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: &nativeMessagesTestAccount{configs: configs}, Logger: bifrost.NewNoOpLogger(), InitialPoolSize: 1,
	})
	if err != nil {
		t.Fatalf("init Bifrost: %v", err)
	}
	defer client.Shutdown()

	covered := map[schemas.ModelProvider]bool{}
	for _, row := range nativeMessagesProviders {
		covered[row.provider] = true
	}
	var built int
	for _, provider := range schemas.StandardProviders {
		instance := client.GetProviderByKey(provider)
		if instance == nil {
			continue
		}
		built++
		capable, ok := instance.(schemas.AnthropicMessagesCapable)
		if !ok {
			if covered[provider] {
				t.Errorf("%s has a row in nativeMessagesProviders but does not implement AnthropicMessagesEndpoint", provider)
			}
			continue
		}
		if _, enabled := capable.AnthropicMessagesEndpoint(); enabled && !covered[provider] {
			t.Errorf("%s implements AnthropicMessagesEndpoint but has no row in nativeMessagesProviders: add one so its native Messages redaction is tested", provider)
		}
		delete(covered, provider)
	}
	for provider := range covered {
		t.Errorf("%s has a row in nativeMessagesProviders but was not built", provider)
	}
	// A provider that cannot be built from a bare base URL would be skipped silently.
	if built != len(schemas.StandardProviders) {
		t.Errorf("built %d of %d standard providers; the ones not built were not checked", built, len(schemas.StandardProviders))
	}
}

// TXE-6965: with a runtime-redaction guardrail in the pipeline, a native Messages
// request to any provider that serves them is redacted before it leaves and its response
// is redacted before the caller sees it, buffered and streamed. With no guardrail the
// bytes are untouched in both directions (contract C4, C11): installing the adapters
// changes nothing by itself.
func TestAnthropicNativeMessagesRedaction(t *testing.T) {
	for _, row := range nativeMessagesProviders {
		for _, stream := range []bool{false, true} {
			for _, guardrail := range []bool{true, false} {
				name := fmt.Sprintf("%s/stream=%v/guardrail=%v", row.provider, stream, guardrail)
				t.Run(name, func(t *testing.T) {
					callerModel := string(row.provider) + "/" + row.model
					requestBody := `{"model":"` + callerModel + `", "max_tokens":64,"metadata":{"user_id":"` + redactionSecret + `"},"system":"Card on file — ` + redactionSecret + `.","messages":[{"role":"user","content":"charge ` + redactionSecret + ` please"},{"role":"assistant","content":[{"type":"text","text":"which card?"}]},{"role":"user","content":[{"type":"text","text":"` + redactionSecret + `"}]}]}`
					if stream {
						requestBody = strings.Replace(requestBody, `"max_tokens":64,`, `"max_tokens":64,"stream":true,`, 1)
					}
					responseBody := `{"id":"msg_1","type":"message","role":"assistant","model":"` + row.model + `","content":[{"type":"text","text":"Charging ` + redactionSecret + ` now."}],"stop_reason":"end_turn","usage":{"input_tokens":12, "output_tokens":6}}`
					contentType := "application/json"
					if stream {
						contentType = "text/event-stream"
						responseBody = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"" + row.model + "\",\"content\":[],\"usage\":{\"input_tokens\":12,\"output_tokens\":1}}}\n\n" +
							"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
							"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Charging " + redactionSecret + " now.\"}}\n\n" +
							"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
							"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":6}}\n\n" +
							"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
					}

					var mu sync.Mutex
					var upstreamBody []byte
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						body, _ := io.ReadAll(r.Body)
						mu.Lock()
						upstreamBody = body
						mu.Unlock()
						if r.URL.Path != row.upstreamPath {
							t.Errorf("upstream path = %q, want %q", r.URL.Path, row.upstreamPath)
						}
						w.Header().Set("Content-Type", contentType)
						_, _ = io.WriteString(w, responseBody)
					}))
					defer upstream.Close()

					plugin := &redactionGuardrailPlugin{}
					var plugins []schemas.LLMPlugin
					if guardrail {
						plugins = append(plugins, plugin)
					}
					client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
						Account: &nativeMessagesTestAccount{
							configs: map[schemas.ModelProvider]*schemas.ProviderConfig{
								row.provider: {NetworkConfig: schemas.NetworkConfig{BaseURL: upstream.URL, AllowPrivateNetwork: true}},
							},
							keys: map[schemas.ModelProvider][]schemas.Key{
								row.provider: {{ID: "key", Value: schemas.SecretVar{Val: "secret"}, Models: schemas.WhiteList{"*"}, Weight: 1}},
							},
						},
						Logger: bifrost.NewNoOpLogger(), InitialPoolSize: 1, LLMPlugins: plugins,
					})
					if err != nil {
						t.Fatalf("init Bifrost: %v", err)
					}
					defer client.Shutdown()

					handler := NewGenericRouter(client, &mockHandlerStore{}, nil, nil, nil, bifrost.NewNoOpLogger()).
						createHandler(createAnthropicMessagesRouteConfig("/anthropic", bifrost.NewNoOpLogger())[0])
					httpCtx := &fasthttp.RequestCtx{}
					httpCtx.Request.Header.SetMethod(fasthttp.MethodPost)
					httpCtx.Request.SetRequestURI("/anthropic/v1/messages")
					httpCtx.Request.Header.SetContentType("application/json")
					httpCtx.Request.Header.Set("anthropic-version", "2023-06-01")
					httpCtx.Request.SetBody([]byte(requestBody))
					handler(httpCtx)

					if httpCtx.Response.StatusCode() != fasthttp.StatusOK {
						t.Fatalf("status = %d, body = %s", httpCtx.Response.StatusCode(), httpCtx.Response.Body())
					}
					clientBody := httpCtx.Response.Body()
					if stream && httpCtx.Response.IsBodyStream() {
						if clientBody, err = io.ReadAll(httpCtx.Response.BodyStream()); err != nil {
							t.Fatalf("read client stream: %v", err)
						}
					}
					mu.Lock()
					sent := string(upstreamBody)
					mu.Unlock()
					wantSent := strings.Replace(requestBody, `"model":"`+callerModel+`"`, `"model":"`+row.model+`"`, 1)

					if !guardrail {
						if sent != wantSent {
							t.Errorf("no guardrail: upstream body is not byte-exact:\n got: %s\nwant: %s", sent, wantSent)
						}
						if string(clientBody) != responseBody {
							t.Errorf("no guardrail: client bytes changed:\n got: %q\nwant: %q", clientBody, responseBody)
						}
						return
					}

					if missing := plugin.missing; len(missing) > 0 {
						t.Errorf("adapters not on the request context: %v", missing)
					}
					// Text the guardrail sees is redacted; metadata, which it does not, is the caller's.
					for _, path := range []string{"system", "messages.0.content", "messages.2.content.0.text"} {
						if got := gjson.Get(sent, path).String(); strings.Contains(got, redactionSecret) || !strings.Contains(got, redactionPlaceholder) {
							t.Errorf("upstream %s was not redacted: %q", path, got)
						}
					}
					if got := gjson.Get(sent, "metadata.user_id").String(); got != redactionSecret {
						t.Errorf("upstream metadata.user_id = %q, want it untouched", got)
					}
					if got := gjson.Get(sent, "model").String(); got != row.model {
						t.Errorf("upstream model = %q", got)
					}
					if bytes.Contains(clientBody, []byte(redactionSecret)) || !bytes.Contains(clientBody, []byte("Charging "+redactionPlaceholder+" now.")) {
						t.Errorf("client response was not redacted: %q", clientBody)
					}
					if want := strings.ReplaceAll(responseBody, redactionSecret, redactionPlaceholder); !stream && !sameJSONBytes(clientBody, []byte(want)) {
						t.Errorf("redaction changed more than the text:\n got: %s\nwant: %s", clientBody, want)
					} else if stream && string(clientBody) != want {
						t.Errorf("redaction changed more than the text delta:\n got: %q\nwant: %q", clientBody, want)
					}
				})
			}
		}
	}
}

func sameJSONBytes(a, b []byte) bool {
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// BenchmarkAnthropicNativeMessagesSelection is what the route does per native Messages
// request before the provider call: select the passthrough and, since TXE-6965, put the
// four raw-body adapters on the request context. "install" is that added step alone.
func BenchmarkAnthropicNativeMessagesSelection(b *testing.B) {
	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: &nativeMessagesTestAccount{configs: map[schemas.ModelProvider]*schemas.ProviderConfig{
			schemas.Moonshot: {NetworkConfig: schemas.NetworkConfig{BaseURL: "https://native-messages.example"}},
		}},
		Logger: bifrost.NewNoOpLogger(), InitialPoolSize: 1,
	})
	if err != nil {
		b.Fatalf("init Bifrost: %v", err)
	}
	defer client.Shutdown()

	rawBody := []byte(`{"model":"moonshot/kimi-k3.1-pro","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	parsed := createAnthropicMessagesRouteConfig("/anthropic", bifrost.NewNoOpLogger())[0].GetRequestTypeInstance(context.Background())
	if err := parseJSONRequestBody(rawBody, parsed); err != nil {
		b.Fatalf("parse request: %v", err)
	}
	selector := createAnthropicMessagesNativePassthrough("/anthropic")
	httpCtx := &fasthttp.RequestCtx{}
	httpCtx.Request.Header.SetMethod(fasthttp.MethodPost)
	httpCtx.Request.SetRequestURI("/anthropic/v1/messages")

	b.Run("selection", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			bifrostCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			if _, selected, err := selector(client, httpCtx, bifrostCtx, parsed, rawBody); err != nil || !selected {
				b.Fatalf("selected = %v, err = %v", selected, err)
			}
			bifrostCtx.Cancel()
		}
	})
	b.Run("install", func(b *testing.B) {
		bifrostCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		defer bifrostCtx.Cancel()
		b.ReportAllocs()
		for b.Loop() {
			installAnthropicNativeMessagesAdapters(bifrostCtx)
		}
	})
}
