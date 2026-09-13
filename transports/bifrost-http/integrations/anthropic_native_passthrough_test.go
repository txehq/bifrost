package integrations

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
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
