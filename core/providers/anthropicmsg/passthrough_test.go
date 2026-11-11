package anthropicmsg

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

func TestPassthroughPreservesNativeMessagesBodyAndUsage(t *testing.T) {
	requestBody := []byte(`{"model":"grok-4.5","max_tokens":64,"system":[{"type":"text","text":"cached","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"use a tool"}]}`)
	responseBody := []byte(`{"id":"msg_xai","type":"message","role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{"q":"native"}}],"usage":{"input_tokens":128,"cache_read_input_tokens":4736,"cache_creation_input_tokens":64,"output_tokens":12}}`)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			t.Errorf("request = %s %s, want POST /v1/messages", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "xai-secret" {
			t.Errorf("x-api-key = %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got != DefaultAPIVersion {
			t.Errorf("anthropic-version = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		if !bytes.Equal(body, requestBody) {
			t.Errorf("request body changed:\n got: %s\nwant: %s", body, requestBody)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(responseBody)
	}))
	defer server.Close()

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	resp, bifrostErr := Passthrough(ctx, &fasthttp.Client{}, testKey("xai-secret"), &schemas.BifrostPassthroughRequest{
		Method: http.MethodPost, Path: "/v1/messages", Body: requestBody,
	}, Config{Provider: schemas.XAI, Endpoint: server.URL})
	if bifrostErr != nil {
		t.Fatalf("Passthrough error: %+v", bifrostErr)
	}
	if !bytes.Equal(resp.Body, responseBody) {
		t.Fatalf("response body changed:\n got: %s\nwant: %s", resp.Body, responseBody)
	}
	usage := resp.PassthroughUsage
	if usage == nil || usage.LLMUsage == nil {
		t.Fatalf("missing passthrough usage: %+v", usage)
	}
	if usage.LLMUsage.PromptTokens != 4928 || usage.LLMUsage.CompletionTokens != 12 || usage.LLMUsage.TotalTokens != 4940 {
		t.Fatalf("usage = %+v", usage.LLMUsage)
	}
	details := usage.LLMUsage.PromptTokensDetails
	if details == nil || details.CachedReadTokens != 4736 || details.CachedWriteTokens != 64 {
		t.Fatalf("cache usage = %+v", details)
	}
}

func TestPassthroughStreamRelaysAnthropicSSEVerbatimAndMergesUsage(t *testing.T) {
	frames := []byte("event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":128,\"cache_read_input_tokens\":4736,\"cache_creation_input_tokens\":64,\"output_tokens\":1}}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":\"lookup\",\"input\":{}}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":12}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(frames[:len(frames)/2])
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		_, _ = w.Write(frames[len(frames)/2:])
	}))
	defer server.Close()

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	postHook := func(_ *schemas.BifrostContext, response *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		return response, err
	}
	stream, bifrostErr := PassthroughStream(ctx, postHook, nil, &fasthttp.Client{}, testKey("xai-secret"), &schemas.BifrostPassthroughRequest{
		Method: http.MethodPost, Path: "/v1/messages", Body: []byte(`{"model":"grok-4.5","stream":true}`),
	}, Config{Provider: schemas.XAI, Endpoint: server.URL})
	if bifrostErr != nil {
		t.Fatalf("PassthroughStream error: %+v", bifrostErr)
	}

	var relayed []byte
	var usage *schemas.BifrostPassthroughUsage
	for chunk := range stream {
		if chunk == nil || chunk.BifrostPassthroughResponse == nil {
			continue
		}
		response := chunk.BifrostPassthroughResponse
		relayed = append(relayed, response.Body...)
		if response.PassthroughUsage != nil {
			usage = response.PassthroughUsage
		}
	}
	if !bytes.Equal(relayed, frames) {
		t.Fatalf("SSE bytes changed:\n got: %q\nwant: %q", relayed, frames)
	}
	if usage == nil || usage.LLMUsage == nil {
		t.Fatalf("missing stream usage: %+v", usage)
	}
	if usage.LLMUsage.PromptTokens != 4928 || usage.LLMUsage.CompletionTokens != 12 || usage.LLMUsage.TotalTokens != 4940 {
		t.Fatalf("stream usage = %+v", usage.LLMUsage)
	}
}

// A caller OAuth bearer forwarded in SafeHeaders (Claude Code's sk-ant-oat token) reaches a
// loopback or TLS upstream but never a cleartext remote one, on both the buffered and the
// streaming path (upstream #7325, ported because the fork's Anthropic and xAI passthroughs
// share this client).
func TestPassthroughForwardsCallerAuthOnlyToSecureUpstreams(t *testing.T) {
	const callerAuth = "Bearer sk-ant-oat01-caller-token"
	for name, tc := range map[string]struct {
		endpoint string
		stream   bool
		wantAuth string
	}{
		"loopback kept":               {endpoint: "", wantAuth: callerAuth},
		"remote http stripped":        {endpoint: "http://anthropic.internal.example", wantAuth: ""},
		"remote http stream stripped": {endpoint: "http://anthropic.internal.example", stream: true, wantAuth: ""},
	} {
		t.Run(name, func(t *testing.T) {
			gotAuth := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth <- r.Header.Get("Authorization")
				if got := r.Header.Get("x-api-key"); got != "" {
					t.Errorf("x-api-key = %q, want none when key selection was skipped", got)
				}
				if got := r.Header.Get("anthropic-beta"); got != "context-1m" {
					t.Errorf("anthropic-beta = %q, non-auth safe headers must survive", got)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"type":"message","usage":{"input_tokens":1,"output_tokens":1}}`))
			}))
			defer server.Close()
			endpoint := tc.endpoint
			if endpoint == "" {
				endpoint = server.URL
			}
			// Route every dial to the test server so a non-loopback URL can be exercised.
			client := &fasthttp.Client{Dial: func(string) (net.Conn, error) {
				return net.Dial("tcp", server.Listener.Addr().String())
			}}
			req := &schemas.BifrostPassthroughRequest{
				Method: http.MethodPost, Path: "/v1/messages", Body: []byte(`{"model":"claude"}`),
				SafeHeaders: map[string]string{"authorization": callerAuth, "anthropic-beta": "context-1m"},
			}
			cfg := Config{Provider: schemas.Anthropic, Endpoint: endpoint}
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			if tc.stream {
				postHook := func(_ *schemas.BifrostContext, response *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
					return response, err
				}
				stream, bifrostErr := PassthroughStream(ctx, postHook, nil, client, testKey(""), req, cfg)
				if bifrostErr != nil {
					t.Fatalf("PassthroughStream error: %+v", bifrostErr)
				}
				for range stream {
				}
			} else if _, bifrostErr := Passthrough(ctx, client, testKey(""), req, cfg); bifrostErr != nil {
				t.Fatalf("Passthrough error: %+v", bifrostErr)
			}
			if got := <-gotAuth; got != tc.wantAuth {
				t.Fatalf("upstream Authorization = %q, want %q", got, tc.wantAuth)
			}
		})
	}
}

func TestExtractUsagePreservesThinkingTokensAndInferenceGeo(t *testing.T) {
	usage := ExtractUsage("/v1/messages", nil, []byte(`{
		"usage": {
			"input_tokens": 23,
			"output_tokens": 87,
			"output_tokens_details": {"thinking_tokens": 39},
			"inference_geo": "us"
		}
	}`))
	if usage == nil || usage.LLMUsage == nil || usage.LLMUsage.CompletionTokensDetails == nil {
		t.Fatalf("missing passthrough usage details: %+v", usage)
	}
	if got := usage.LLMUsage.CompletionTokensDetails.ReasoningTokens; got != 39 {
		t.Fatalf("reasoning tokens = %d, want 39", got)
	}
	if usage.InferenceGeo == nil || *usage.InferenceGeo != "us" {
		t.Fatalf("inference geo = %v, want us", usage.InferenceGeo)
	}
}

func testKey(value string) schemas.Key {
	return schemas.Key{Value: schemas.SecretVar{Val: value}}
}

func TestStripAnthropicOnlyFields(t *testing.T) {
	const billing = "x-anthropic-billing-header: cc_version=2.1.80.a1b; cc_entrypoint=cli; cch=0f3c2;"
	cases := []struct {
		name, body, want string
	}{
		{
			name: "string system that is only the billing line",
			body: `{"model":"m","system":"` + billing + `","messages":[]}`,
			want: `{"model":"m","messages":[]}`,
		},
		{
			name: "billing block removed, others kept in order with their cache markers",
			body: `{"system":[{"type":"text","text":"a"},{"type":"text","text":"` + billing + `"},{"type":"text","text":"b","cache_control":{"type":"ephemeral"}}]}`,
			want: `{"system":[{"type":"text","text":"a"},{"type":"text","text":"b","cache_control":{"type":"ephemeral"}}]}`,
		},
		{
			name: "system of only billing blocks is dropped",
			body: `{"system":[{"type":"text","text":"` + billing + `"}],"messages":[]}`,
			want: `{"messages":[]}`,
		},
		{
			name: "billing line mixed with instructions is real system content",
			body: `{"system":"` + billing + ` Always answer in French."}`,
			want: `{"system":"` + billing + ` Always answer in French."}`,
		},
		{
			name: "safeguards and diagnostics removed",
			body: `{"model":"m","safeguards":{"a":1},"diagnostics":{"previous_message_id":"x"},"max_tokens":8}`,
			want: `{"model":"m","max_tokens":8}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := StripAnthropicOnlyFields([]byte(tc.body))
			if err != nil {
				t.Fatalf("StripAnthropicOnlyFields: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
		})
	}

	clean := []byte(`{"model":"m","system":[{"type":"text","text":"keep"}],"messages":[]}`)
	got, err := StripAnthropicOnlyFields(clean)
	if err != nil || &got[0] != &clean[0] {
		t.Fatalf("a body with nothing to strip must be returned as is (err %v)", err)
	}
}

// Anthropic itself must still receive Claude Code's fields: the strip is opt-in.
func TestPassthroughKeepsAnthropicOnlyFieldsUnlessAsked(t *testing.T) {
	body := []byte(`{"model":"claude","safeguards":{"a":1},"system":"x-anthropic-billing-header: cc_version=1;","messages":[]}`)
	received := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		received <- got
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","content":[]}`))
	}))
	defer server.Close()

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	if _, bifrostErr := Passthrough(ctx, &fasthttp.Client{}, testKey("k"), &schemas.BifrostPassthroughRequest{
		Method: http.MethodPost, Path: "/v1/messages", Body: body,
	}, Config{Provider: schemas.Anthropic, Endpoint: server.URL}); bifrostErr != nil {
		t.Fatalf("Passthrough: %+v", bifrostErr)
	}
	if got := <-received; !bytes.Equal(got, body) {
		t.Fatalf("Anthropic body changed:\n got: %s\nwant: %s", got, body)
	}
}

// A caller-supplied path that is not rooted on the endpoint would turn the configured host
// into userinfo or a scheme-relative authority and send the key to another host. Both the
// buffered and the streaming path refuse it with 400 before dialing anything.
func TestPassthroughRefusesPathsThatLeaveTheEndpointHost(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	for _, path := range []string{"@evil.example/v1/messages", "//evil.example/v1/messages", "v1/messages"} {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		req := &schemas.BifrostPassthroughRequest{Method: http.MethodPost, Path: path, Body: []byte(`{}`)}
		cfg := Config{Provider: schemas.XAI, Endpoint: server.URL}
		if _, bifrostErr := Passthrough(ctx, &fasthttp.Client{}, testKey("xai-secret"), req, cfg); bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
			t.Fatalf("Passthrough(%q) = %+v, want a 400", path, bifrostErr)
		}
		if _, bifrostErr := PassthroughStream(ctx, nil, nil, &fasthttp.Client{}, testKey("xai-secret"), req, cfg); bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
			t.Fatalf("PassthroughStream(%q) = %+v, want a 400", path, bifrostErr)
		}
	}
	if hits != 0 {
		t.Fatalf("the endpoint was dialed %d times for refused paths", hits)
	}
}

// Core resolves a key alias per attempt and stashes it on the context, but
// BifrostRequest.SetModel has no passthrough branch. The native Messages body must
// therefore swap the alias key for the alias's model id itself, on both transports,
// and change nothing else (TXE-6548). A body whose model is not the alias key, and a
// non-Messages path, are forwarded as sent.
func TestPassthroughAppliesResolvedKeyAliasToModel(t *testing.T) {
	const sent = "{\"model\":\"flash-anthropic\",\n \"max_tokens\":\t64,\"stream\":%s,\"messages\":[{\"role\":\"user\",\"content\":\"Caf\\u00e9\"}],\"zz\":{\"b\":1}}"
	const want = "{\"model\":\"deepseek-v4-flash\",\n \"max_tokens\":\t64,\"stream\":%s,\"messages\":[{\"role\":\"user\",\"content\":\"Caf\\u00e9\"}],\"zz\":{\"b\":1}}"
	received := make(chan []byte, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		received <- got
		if bytes.Contains(got, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","content":[]}`))
	}))
	defer server.Close()

	aliasCtx := func() *schemas.BifrostContext {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		ctx.SetValue(schemas.BifrostContextKeyResolvedAlias, &schemas.ResolvedAlias{
			Key: "flash-anthropic", Config: &schemas.AliasConfig{ModelID: "deepseek-v4-flash"},
		})
		return ctx
	}
	cfg := Config{Provider: schemas.DeepSeek, Endpoint: server.URL}
	postHook := func(_ *schemas.BifrostContext, response *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		return response, err
	}

	if _, bifrostErr := Passthrough(aliasCtx(), &fasthttp.Client{}, testKey("k"), &schemas.BifrostPassthroughRequest{
		Method: http.MethodPost, Path: "/v1/messages", Body: []byte(fmt.Sprintf(sent, "false")),
	}, cfg); bifrostErr != nil {
		t.Fatalf("Passthrough: %+v", bifrostErr)
	}
	if got := string(<-received); got != fmt.Sprintf(want, "false") {
		t.Fatalf("buffered body:\n got: %s\nwant: %s", got, fmt.Sprintf(want, "false"))
	}

	stream, bifrostErr := PassthroughStream(aliasCtx(), postHook, nil, &fasthttp.Client{}, testKey("k"), &schemas.BifrostPassthroughRequest{
		Method: http.MethodPost, Path: "/v1/messages", Body: []byte(fmt.Sprintf(sent, "true")),
	}, cfg)
	if bifrostErr != nil {
		t.Fatalf("PassthroughStream: %+v", bifrostErr)
	}
	for range stream {
	}
	if got := string(<-received); got != fmt.Sprintf(want, "true") {
		t.Fatalf("streamed body:\n got: %s\nwant: %s", got, fmt.Sprintf(want, "true"))
	}

	other := `{"model":"deepseek-v4-pro","messages":[]}`
	if _, bifrostErr := Passthrough(aliasCtx(), &fasthttp.Client{}, testKey("k"), &schemas.BifrostPassthroughRequest{
		Method: http.MethodPost, Path: "/v1/messages", Body: []byte(other),
	}, cfg); bifrostErr != nil {
		t.Fatalf("Passthrough: %+v", bifrostErr)
	}
	if got := string(<-received); got != other {
		t.Fatalf("a model other than the alias key changed:\n got: %s\nwant: %s", got, other)
	}

	countTokens := `{"model":"flash-anthropic","messages":[]}`
	if _, bifrostErr := Passthrough(aliasCtx(), &fasthttp.Client{}, testKey("k"), &schemas.BifrostPassthroughRequest{
		Method: http.MethodPost, Path: "/v1/messages/count_tokens", Body: []byte(countTokens),
	}, cfg); bifrostErr != nil {
		t.Fatalf("Passthrough: %+v", bifrostErr)
	}
	if got := string(<-received); got != countTokens {
		t.Fatalf("a non-Messages path changed:\n got: %s\nwant: %s", got, countTokens)
	}
}

// TestPassthroughLargePayloadAppliesResolvedKeyAlias covers a body large payload mode
// streams upstream unbuffered: the resolved key alias reaches the vendor as its model id,
// as it does on the buffered path, so a caller cannot pass the alias name through by
// sending a large body.
func TestPassthroughLargePayloadAppliesResolvedKeyAlias(t *testing.T) {
	received := make(chan []byte, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		received <- got
		if bytes.Contains(got, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","content":[]}`))
	}))
	defer server.Close()

	largeCtx := func(body, model string, alias bool) *schemas.BifrostContext {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		ctx.SetValue(schemas.BifrostContextKeyLargePayloadMode, true)
		// The mechanism under test runs only for an attempt the eligibility rule lets skip conversion.
		ctx.SetValue(schemas.BifrostContextKeyLargePayloadEligibility, schemas.LargePayloadEligibility{RawRequest: true, RawResponse: true})
		ctx.SetValue(schemas.BifrostContextKeyLargePayloadReader, strings.NewReader(body))
		ctx.SetValue(schemas.BifrostContextKeyLargePayloadContentLength, len(body))
		ctx.SetValue(schemas.BifrostContextKeyLargePayloadContentType, "application/json")
		ctx.SetValue(schemas.BifrostContextKeyLargePayloadMetadata, &schemas.LargePayloadMetadata{Model: model})
		if alias {
			ctx.SetValue(schemas.BifrostContextKeyResolvedAlias, &schemas.ResolvedAlias{
				Key: "flash-anthropic", Config: &schemas.AliasConfig{ModelID: "deepseek-v4-flash"},
			})
		}
		return ctx
	}
	cfg := Config{Provider: schemas.DeepSeek, Endpoint: server.URL}
	postHook := func(_ *schemas.BifrostContext, response *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		return response, err
	}
	req := &schemas.BifrostPassthroughRequest{Method: http.MethodPost, Path: "/v1/messages"}

	cases := []struct {
		name, model, sent, want string
		alias                   bool
	}{
		{"alias", "deepseek/flash-anthropic", `{"model":"deepseek/flash-anthropic","stream":%s,"messages":[]}`, `{"model":"deepseek-v4-flash","stream":%s,"messages":[]}`, true},
		{"alias_without_prefix", "flash-anthropic", `{"model":"flash-anthropic","stream":%s,"messages":[]}`, `{"model":"deepseek-v4-flash","stream":%s,"messages":[]}`, true},
		{"prefix_only", "deepseek/deepseek-v4-pro", `{"model":"deepseek/deepseek-v4-pro","stream":%s,"messages":[]}`, `{"model":"deepseek-v4-pro","stream":%s,"messages":[]}`, true},
		{"no_alias", "deepseek/flash-anthropic", `{"model":"deepseek/flash-anthropic","stream":%s,"messages":[]}`, `{"model":"flash-anthropic","stream":%s,"messages":[]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, bifrostErr := Passthrough(largeCtx(fmt.Sprintf(tc.sent, "false"), tc.model, tc.alias), &fasthttp.Client{}, testKey("k"), req, cfg); bifrostErr != nil {
				t.Fatalf("Passthrough: %+v", bifrostErr)
			}
			if got, want := string(<-received), fmt.Sprintf(tc.want, "false"); got != want {
				t.Fatalf("buffered call, large body:\n got: %s\nwant: %s", got, want)
			}
			stream, bifrostErr := PassthroughStream(largeCtx(fmt.Sprintf(tc.sent, "true"), tc.model, tc.alias), postHook, nil, &fasthttp.Client{}, testKey("k"), req, cfg)
			if bifrostErr != nil {
				t.Fatalf("PassthroughStream: %+v", bifrostErr)
			}
			for range stream {
			}
			if got, want := string(<-received), fmt.Sprintf(tc.want, "true"); got != want {
				t.Fatalf("streamed call, large body:\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

// BenchmarkRequestBody measures the native Messages body builder every buffered and
// streamed passthrough runs (TXE-6548). no_alias is the common case: the only added work
// is a path check and one context lookup, with no allocation. alias_rewrite is a key
// alias resolved for this attempt; alias_other_model is an alias on the context whose key
// the body does not spell, which must not rewrite.
func BenchmarkRequestBody(b *testing.B) {
	body := []byte(`{"model":"flash-anthropic","max_tokens":1024,"system":[{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"look it up"}]}],"metadata":{"user_id":"u-1"}}`)
	other := []byte(`{"model":"deepseek-v4-pro","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)
	alias := &schemas.ResolvedAlias{Key: "flash-anthropic", Config: &schemas.AliasConfig{ModelID: "deepseek-v4-flash"}}
	cfg := Config{Provider: schemas.DeepSeek, Endpoint: "https://api.deepseek.com/anthropic"}
	cases := []struct {
		name  string
		body  []byte
		alias *schemas.ResolvedAlias
	}{
		{name: "no_alias", body: body},
		{name: "alias_rewrite", body: body, alias: alias},
		{name: "alias_other_model", body: other, alias: alias},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			defer ctx.Cancel()
			if tc.alias != nil {
				ctx.SetValue(schemas.BifrostContextKeyResolvedAlias, tc.alias)
			}
			req := &schemas.BifrostPassthroughRequest{Method: http.MethodPost, Path: "/v1/messages", Body: tc.body}
			b.ReportAllocs()
			for b.Loop() {
				if _, bifrostErr := cfg.requestBody(ctx, req); bifrostErr != nil {
					b.Fatalf("requestBody: %+v", bifrostErr)
				}
			}
		})
	}
}
