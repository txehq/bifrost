package anthropicmsg

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
