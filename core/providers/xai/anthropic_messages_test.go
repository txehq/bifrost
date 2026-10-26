package xai_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/providers/xai"
	"github.com/capsohq/bifrost/core/schemas"
)

func TestXAIAnthropicMessagesAttachmentDoesNotChangeOpenAIEndpoints(t *testing.T) {
	paths := make(chan string, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/messages" {
			_, _ = w.Write([]byte(`{"type":"message","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"path captured"}}`))
	}))
	defer server.Close()

	config := &schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{
		BaseURL: server.URL, AllowPrivateNetwork: true,
	}}
	provider, err := xai.NewXAIProvider(config, bifrost.NewNoOpLogger())
	if err != nil {
		t.Fatalf("NewXAIProvider: %v", err)
	}
	capable, ok := interface{}(provider).(schemas.AnthropicMessagesCapable)
	if !ok {
		t.Fatal("xAI provider does not advertise Anthropic Messages capability")
	}
	if endpoint, enabled := capable.AnthropicMessagesEndpoint(); !enabled || endpoint != server.URL {
		t.Fatalf("endpoint = %q, enabled = %v", endpoint, enabled)
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	key := schemas.Key{Value: schemas.SecretVar{Val: "test-key"}}
	_, _ = provider.ChatCompletion(ctx, key, &schemas.BifrostChatRequest{
		Provider: schemas.XAI, Model: "grok-4.5",
		Input: []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("Hello")}}},
	})
	_, _ = provider.Responses(ctx, key, &schemas.BifrostResponsesRequest{
		Provider: schemas.XAI, Model: "grok-4.5",
		Input: []schemas.ResponsesMessage{{Role: schemas.Ptr(schemas.ResponsesInputMessageRoleUser), Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr("Hello")}}},
	})
	_, passthroughErr := provider.Passthrough(ctx, key, &schemas.BifrostPassthroughRequest{
		Method: http.MethodPost, Path: "/v1/messages", Body: []byte(`{"model":"grok-4.5","messages":[]}`),
	})
	if passthroughErr != nil {
		t.Fatalf("Messages passthrough: %+v", passthroughErr)
	}

	want := []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"}
	for i, expected := range want {
		if got := <-paths; got != expected {
			t.Fatalf("request %d path = %q, want %q", i, got, expected)
		}
	}
}

// Claude Code sends Anthropic-only fields and a per-request billing line on every
// request. Upstream strips them before xAI's native Messages passthrough; this fork
// forwards the body byte-for-byte on both transports, system included (TXE-5193).
func TestXAIMessagesPassthroughForwardsAnthropicOnlyFields(t *testing.T) {
	const billing = "x-anthropic-billing-header: cc_version=2.1.80.a1b; cc_entrypoint=cli; cch=0f3c2;"
	body := `{"model":"grok-4.5","safeguards":{"mode":"auto"},"diagnostics":{"previous_message_id":"msg_1"},` +
		`"system":[{"type":"text","text":"` + billing + `"},{"type":"text","text":"You are helpful.","cache_control":{"type":"ephemeral"}}],` +
		`"messages":[{"role":"user","content":"hi"}]}`
	want := body

	bodies := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		bodies <- string(got)
		if strings.Contains(string(got), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	provider, err := xai.NewXAIProvider(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{
		BaseURL: server.URL, AllowPrivateNetwork: true,
	}}, bifrost.NewNoOpLogger())
	if err != nil {
		t.Fatalf("NewXAIProvider: %v", err)
	}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	key := schemas.Key{Value: schemas.SecretVar{Val: "test-key"}}

	if _, bifrostErr := provider.Passthrough(ctx, key, &schemas.BifrostPassthroughRequest{
		Method: http.MethodPost, Path: "/v1/messages", Body: []byte(body),
	}); bifrostErr != nil {
		t.Fatalf("Passthrough: %+v", bifrostErr)
	}
	if got := <-bodies; got != want {
		t.Fatalf("buffered body sent to xAI:\n got: %s\nwant: %s", got, want)
	}

	streamBody := strings.Replace(body, `"model":"grok-4.5",`, `"model":"grok-4.5","stream":true,`, 1)
	streamWant := strings.Replace(want, `"model":"grok-4.5",`, `"model":"grok-4.5","stream":true,`, 1)
	postHook := func(_ *schemas.BifrostContext, response *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		return response, err
	}
	stream, bifrostErr := provider.PassthroughStream(ctx, postHook, nil, key, &schemas.BifrostPassthroughRequest{
		Method: http.MethodPost, Path: "/v1/messages", Body: []byte(streamBody),
	})
	if bifrostErr != nil {
		t.Fatalf("PassthroughStream: %+v", bifrostErr)
	}
	for range stream {
	}
	if got := <-bodies; got != streamWant {
		t.Fatalf("streamed body sent to xAI:\n got: %s\nwant: %s", got, streamWant)
	}
}
