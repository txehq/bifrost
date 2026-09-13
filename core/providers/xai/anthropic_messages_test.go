package xai_test

import (
	"context"
	"net/http"
	"net/http/httptest"
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
