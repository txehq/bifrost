package anthropicmsg

import (
	"bytes"
	"context"
	"io"
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

// A passthrough path must stay on the configured provider host; "//host" would make
// the joined URL leave it with the provider credentials attached.
func TestPassthroughRejectsPathThatLeavesProviderHost(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, bifrostErr := Passthrough(ctx, &fasthttp.Client{}, testKey("xai-secret"), &schemas.BifrostPassthroughRequest{
		Method: http.MethodPost, Path: "//attacker.example/v1/messages", Body: []byte(`{}`),
	}, Config{Provider: schemas.XAI, Endpoint: server.URL})
	if bifrostErr == nil {
		t.Fatal("expected a bad-request error for a host-escaping path")
	}
	_, bifrostErr = PassthroughStream(ctx, nil, nil, &fasthttp.Client{}, testKey("xai-secret"), &schemas.BifrostPassthroughRequest{
		Method: http.MethodPost, Path: "//attacker.example/v1/messages", Body: []byte(`{}`),
	}, Config{Provider: schemas.XAI, Endpoint: server.URL})
	if bifrostErr == nil {
		t.Fatal("expected a bad-request error for a host-escaping streaming path")
	}
	if hits != 0 {
		t.Fatalf("provider received %d requests, want 0", hits)
	}
}

// A caller's Authorization header is only forwarded over TLS or to loopback.
func TestPassthroughStripsCallerAuthorizationForInsecureEndpoint(t *testing.T) {
	headers := map[string]string{"Authorization": "Bearer caller", "anthropic-beta": "x"}
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	Config{}.setHeaders(ctx, req, "http://provider.example/v1/messages", testKey("k"), headers)
	if got := req.Header.Peek("Authorization"); len(got) != 0 {
		t.Fatalf("Authorization forwarded over plain http: %q", got)
	}
	if got := string(req.Header.Peek("anthropic-beta")); got != "x" {
		t.Fatalf("anthropic-beta = %q, want x", got)
	}

	headers = map[string]string{"Authorization": "Bearer caller"}
	req.Reset()
	Config{}.setHeaders(ctx, req, "https://provider.example/v1/messages", testKey("k"), headers)
	if got := string(req.Header.Peek("Authorization")); got != "Bearer caller" {
		t.Fatalf("Authorization over https = %q, want Bearer caller", got)
	}
}

func TestClassifyPath(t *testing.T) {
	for path, want := range map[string]schemas.RequestType{
		"/v1/messages":              schemas.ResponsesRequest,
		"/v1/messages?beta=true":    schemas.ResponsesRequest,
		"/v1/messages/count_tokens": schemas.CountTokensRequest,
		"/v1/complete":              schemas.TextCompletionRequest,
		"/v1/messages/batches":      "",
		"/v1/models":                "",
	} {
		if got := ClassifyPath(path); got != want {
			t.Errorf("ClassifyPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestExtractUsageReportsWebSearchToolUsage(t *testing.T) {
	usage := ExtractUsage("/v1/messages", nil, []byte(`{"usage":{"input_tokens":5,"output_tokens":2,"server_tool_use":{"web_search_requests":3}}}`))
	if usage == nil || usage.LLMUsage == nil || usage.LLMUsage.ToolUsage == nil || usage.LLMUsage.ToolUsage.WebSearch == nil {
		t.Fatalf("missing web search tool usage: %+v", usage)
	}
	if got := usage.LLMUsage.ToolUsage.WebSearch.NumRequests; got != 3 {
		t.Fatalf("web search requests = %d, want 3", got)
	}
	if usage.LLMUsage.CompletionTokensDetails == nil || usage.LLMUsage.CompletionTokensDetails.NumSearchQueries == nil || *usage.LLMUsage.CompletionTokensDetails.NumSearchQueries != 3 {
		t.Fatalf("deprecated NumSearchQueries must still be set: %+v", usage.LLMUsage.CompletionTokensDetails)
	}
}
