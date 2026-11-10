package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

func TestServiceTierForModel(t *testing.T) {
	ultrafast := schemas.BifrostServiceTierUltrafast
	priority := schemas.BifrostServiceTierPriority
	fast := schemas.BifrostServiceTierFast
	defaultTier := schemas.BifrostServiceTierDefault

	schemas.SetCapabilityResolver(func(provider schemas.ModelProvider, model string) *schemas.ModelCapabilities {
		switch provider {
		case schemas.OpenAI:
			switch model {
			case "gpt-5.6-sol":
				return &schemas.ModelCapabilities{ServiceTiers: []string{"default", "flex", "priority", "ultrafast"}}
			case "gpt-6-astra":
				// Live datasheet shape: OpenAI lists the renamed Fast tier beside priority.
				return &schemas.ModelCapabilities{ServiceTiers: []string{"auto", "default", "flex", "priority", "fast", "ultrafast"}}
			case "standard-model":
				return &schemas.ModelCapabilities{ServiceTiers: []string{"default", "flex", "priority"}}
			}
		case schemas.BedrockMantle:
			if model == "xai.grok-4.6" {
				return &schemas.ModelCapabilities{ServiceTiers: []string{"priority", "flex"}}
			}
		}
		return nil
	})
	t.Cleanup(func() { schemas.SetCapabilityResolver(nil) })

	tests := []struct {
		name     string
		provider schemas.ModelProvider
		model    string
		tier     *schemas.BifrostServiceTier
		want     *schemas.BifrostServiceTier
	}{
		{name: "explicit ultrafast support", provider: schemas.OpenAI, model: "gpt-5.6-sol", tier: &ultrafast, want: &ultrafast},
		{name: "unsupported ultrafast", provider: schemas.OpenAI, model: "standard-model", tier: &ultrafast, want: nil},
		{name: "missing capabilities preserve tier", provider: schemas.OpenAI, model: "unknown-model", tier: &ultrafast, want: &ultrafast},
		{name: "missing capabilities preserve ordinary tier", provider: schemas.OpenAI, model: "unknown-model", tier: &priority, want: &priority},
		{name: "explicit fast support", provider: schemas.OpenAI, model: "gpt-6-astra", tier: &fast, want: &fast},
		{name: "explicit tier list without fast drops it", provider: schemas.OpenAI, model: "standard-model", tier: &fast, want: nil},
		{name: "missing capabilities preserve fast", provider: schemas.OpenAI, model: "unknown-model", tier: &fast, want: &fast},
		{name: "explicit tier list rejects omitted ordinary tier", provider: schemas.OpenAI, model: "standard-model", tier: schemas.Ptr(schemas.BifrostServiceTierProvisioned), want: nil},
		{name: "bedrock mantle keeps explicitly supported tier", provider: schemas.BedrockMantle, model: "xai.grok-4.6", tier: &priority, want: &priority},
		{name: "bedrock mantle drops unknown non-standard tier", provider: schemas.BedrockMantle, model: "openai.gpt-5.6-luna", tier: &priority, want: nil},
		{name: "bedrock mantle omits default", provider: schemas.BedrockMantle, model: "xai.grok-4.6", tier: &defaultTier, want: nil},
		{name: "nil tier", provider: schemas.OpenAI, model: "standard-model", tier: nil, want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			caps := schemas.ResolveModelCaps(tt.provider, tt.model)
			got := serviceTierForModel(caps, tt.tier)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("service tier = %q, want nil", *got)
				}
				return
			}
			if got == nil || *got != *tt.want {
				t.Fatalf("service tier = %v, want %q", got, *tt.want)
			}
		})
	}
}

func TestOpenAIConvertersFilterServiceTierByModelCapability(t *testing.T) {
	ultrafast := schemas.BifrostServiceTierUltrafast
	schemas.SetCapabilityResolver(func(provider schemas.ModelProvider, model string) *schemas.ModelCapabilities {
		if provider == schemas.OpenAI && model == "gpt-5.6-sol" {
			return &schemas.ModelCapabilities{ServiceTiers: []string{"ultrafast"}}
		}
		if provider == schemas.OpenRouter && model == "compatible-fast-model" {
			return &schemas.ModelCapabilities{ServiceTiers: []string{"ultrafast"}}
		}
		if provider == schemas.OpenRouter && model == "compatible-standard-model" {
			return &schemas.ModelCapabilities{ServiceTiers: []string{"default", "priority"}}
		}
		if provider == schemas.BedrockMantle && model == "xai.grok-4.6" {
			return &schemas.ModelCapabilities{ServiceTiers: []string{"priority", "flex"}}
		}
		return nil
	})
	t.Cleanup(func() { schemas.SetCapabilityResolver(nil) })

	tests := []struct {
		name     string
		provider schemas.ModelProvider
		model    string
		tier     schemas.BifrostServiceTier
		wantTier bool
	}{
		{name: "OpenAI supported model", provider: schemas.OpenAI, model: "gpt-5.6-sol", tier: ultrafast, wantTier: true},
		{name: "OpenAI model without tier metadata", provider: schemas.OpenAI, model: "other-model", tier: ultrafast, wantTier: true},
		{name: "compatible provider without tier metadata", provider: schemas.OpenRouter, model: "other-model", tier: ultrafast, wantTier: true},
		{name: "compatible provider explicitly unsupported tier", provider: schemas.OpenRouter, model: "compatible-standard-model", tier: ultrafast, wantTier: false},
		{name: "compatible provider supported model", provider: schemas.OpenRouter, model: "compatible-fast-model", tier: ultrafast, wantTier: true},
		{name: "bedrock mantle keeps explicitly supported tier", provider: schemas.BedrockMantle, model: "xai.grok-4.6", tier: schemas.BifrostServiceTierPriority, wantTier: true},
		{name: "bedrock mantle drops tier without metadata", provider: schemas.BedrockMantle, model: "openai.gpt-5.6-terra", tier: ultrafast, wantTier: false},
		{name: "bedrock mantle drops tier for bare name", provider: schemas.BedrockMantle, model: "gpt-5.6-terra", tier: ultrafast, wantTier: false},
		{name: "legacy in-bedrock mantle drops tier", provider: schemas.Bedrock, model: "openai.gpt-5.4", tier: ultrafast, wantTier: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tier := tt.tier
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			chat := ToOpenAIChatRequest(ctx, &schemas.BifrostChatRequest{
				Provider: tt.provider,
				Model:    tt.model,
				Input:    []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser}},
				Params:   &schemas.ChatParameters{ServiceTier: &tier},
			})
			if chat == nil {
				t.Fatal("chat conversion returned nil")
			}
			// Chat refuses a tier the model cannot take instead of dropping it (contract
			// C4); the converter itself leaves such a tier as sent, never a different one.
			chatReq := &schemas.BifrostChatRequest{
				Provider: tt.provider,
				Model:    tt.model,
				Input:    []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser}},
				Params:   &schemas.ChatParameters{ServiceTier: &tier},
			}
			tierErr := ChatServiceTierError(ctx, chatReq)
			if (tierErr == nil) != tt.wantTier {
				t.Fatalf("ChatServiceTierError = %+v, want refused=%v", tierErr, !tt.wantTier)
			}
			if chat.ServiceTier == nil || *chat.ServiceTier != tt.tier {
				t.Fatalf("chat service tier = %v, want %q as sent", chat.ServiceTier, tt.tier)
			}

			responses := ToOpenAIResponsesRequest(ctx, &schemas.BifrostResponsesRequest{
				Provider: tt.provider,
				Model:    tt.model,
				Input: []schemas.ResponsesMessage{{
					Role:    schemas.Ptr(schemas.ResponsesInputMessageRoleUser),
					Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr("hello")},
				}},
				Params: &schemas.ResponsesParameters{ServiceTier: &tier},
			})
			if responses == nil {
				t.Fatal("responses conversion returned nil")
			}
			if (responses.ServiceTier != nil) != tt.wantTier {
				t.Fatalf("responses service tier = %v, want present=%v", responses.ServiceTier, tt.wantTier)
			}
		})
	}
}

// stubChatTierCatalog publishes a tier list for one model per OpenAI-shape provider under
// test and none for any other model.
func stubChatTierCatalog(t testing.TB) {
	t.Helper()
	schemas.SetCapabilityResolver(func(provider schemas.ModelProvider, model string) *schemas.ModelCapabilities {
		if model == "tiered-model" {
			return &schemas.ModelCapabilities{ServiceTiers: []string{"default", "priority"}}
		}
		return nil
	})
	t.Cleanup(func() { schemas.SetCapabilityResolver(nil) })
}

// TestChatServiceTierError pins the refusal itself: 400, caller_unsupported_parameter,
// error.param "service_tier", for every OpenAI-shape provider the vendor sheets name. A
// supported tier, a model with no tier list, and a request with no tier are not refused. A
// Bedrock default tier is never refused: it is what Bedrock uses when the field is omitted.
func TestChatServiceTierError(t *testing.T) {
	stubChatTierCatalog(t)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	request := func(provider schemas.ModelProvider, model string, tier *schemas.BifrostServiceTier) *schemas.BifrostChatRequest {
		return &schemas.BifrostChatRequest{
			Provider: provider,
			Model:    model,
			Input:    []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser}},
			Params:   &schemas.ChatParameters{ServiceTier: tier},
		}
	}
	flex := schemas.Ptr(schemas.BifrostServiceTierFlex)
	for _, provider := range []schemas.ModelProvider{schemas.OpenAI, schemas.Azure, schemas.DeepSeek, schemas.GLM, schemas.Moonshot} {
		t.Run(string(provider), func(t *testing.T) {
			got := ChatServiceTierError(ctx, request(provider, "tiered-model", flex))
			if got == nil {
				t.Fatal("a tier outside the model's list must be refused")
			}
			if got.StatusCode == nil || *got.StatusCode != 400 {
				t.Fatalf("status = %v, want 400", got.StatusCode)
			}
			if got.ExtraFields.ErrorType != schemas.ErrorTypeCallerUnsupportedParameter {
				t.Fatalf("extra_fields.error_type = %q, want caller_unsupported_parameter", got.ExtraFields.ErrorType)
			}
			if got.Error == nil || got.Error.Param != "service_tier" {
				t.Fatalf("error.param = %v, want service_tier", got.Error)
			}
			if got.IsBifrostError {
				t.Fatal("a refused parameter is the caller's fault, not a Bifrost failure")
			}
			if err := ChatServiceTierError(ctx, request(provider, "tiered-model", schemas.Ptr(schemas.BifrostServiceTierPriority))); err != nil {
				t.Fatalf("a listed tier was refused: %+v", err)
			}
			if err := ChatServiceTierError(ctx, request(provider, "unlisted-model", flex)); err != nil {
				t.Fatalf("a model with no tier list takes any tier, got %+v", err)
			}
			if err := ChatServiceTierError(ctx, request(provider, "tiered-model", nil)); err != nil {
				t.Fatalf("a request with no tier was refused: %+v", err)
			}
		})
	}
	if err := ChatServiceTierError(ctx, &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "tiered-model"}); err != nil {
		t.Fatalf("a request with no params was refused: %+v", err)
	}
	if err := ChatServiceTierError(ctx, nil); err != nil {
		t.Fatalf("a nil request was refused: %+v", err)
	}
	// "default" is never refused, even where the model's list leaves it out: the datasheet
	// omits it for models that take it. It reaches the vendor as sent.
	schemas.SetCapabilityResolver(func(schemas.ModelProvider, string) *schemas.ModelCapabilities {
		return &schemas.ModelCapabilities{ServiceTiers: []string{"auto", "priority"}}
	})
	defaultTier := schemas.Ptr(schemas.BifrostServiceTierDefault)
	if err := ChatServiceTierError(ctx, request(schemas.OpenAI, "gpt-4", defaultTier)); err != nil {
		t.Fatalf("the default tier was refused for a model whose list omits it: %+v", err)
	}
	if out := ToOpenAIChatRequest(ctx, request(schemas.OpenAI, "gpt-4", defaultTier)); out.ServiceTier == nil || *out.ServiceTier != schemas.BifrostServiceTierDefault {
		t.Fatalf("the default tier must reach the vendor as sent, got %v", out.ServiceTier)
	}
	if err := ChatServiceTierError(ctx, request(schemas.OpenAI, "gpt-4", flex)); err == nil {
		t.Fatal("a non-default tier outside the list must still be refused")
	}
	stubChatTierCatalog(t)

	if err := ChatServiceTierError(ctx, request(schemas.BedrockMantle, "unlisted-model", schemas.Ptr(schemas.BifrostServiceTierDefault))); err != nil {
		t.Fatalf("a Bedrock default tier was refused: %+v", err)
	}
	if err := ChatServiceTierError(ctx, request(schemas.BedrockMantle, "unlisted-model", flex)); err == nil {
		t.Fatal("an unlisted non-default Bedrock tier must be refused")
	}
}

// TestChatHandlersRefuseUnsupportedServiceTierBeforeAnyUpstreamCall drives both shared chat
// handlers against a counting upstream: an unsupported tier comes back as the 400 with no
// request made, and a supported tier reaches the vendor in the body as sent.
func TestChatHandlersRefuseUnsupportedServiceTierBeforeAnyUpstreamCall(t *testing.T) {
	stubChatTierCatalog(t)
	var calls atomic.Int64
	var lastBody atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		lastBody.Store(body)
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"tiered-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"tiered-model","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)
	client := &fasthttp.Client{}
	url := upstream.URL + "/v1/chat/completions"
	request := func(tier schemas.BifrostServiceTier) *schemas.BifrostChatRequest {
		return &schemas.BifrostChatRequest{
			Provider: schemas.Moonshot,
			Model:    "tiered-model",
			Input:    []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("hello")}}},
			Params:   &schemas.ChatParameters{ServiceTier: &tier},
		}
	}
	newCtx := func() *schemas.BifrostContext {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		ctx.SetValue(schemas.BifrostContextKeyTracer, &schemas.NoOpTracer{})
		return ctx
	}
	send := func(stream bool, tier schemas.BifrostServiceTier) *schemas.BifrostError {
		if !stream {
			_, bifrostErr := HandleOpenAIChatCompletionRequest(newCtx(), client, url, request(tier), nil, nil, false, false,
				schemas.Moonshot, nil, nil, nil, nil)
			return bifrostErr
		}
		postHook := func(_ *schemas.BifrostContext, result *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
			return result, err
		}
		chunks, bifrostErr := HandleOpenAIChatCompletionStreaming(newCtx(), client, url, request(tier), nil, nil, 5, false, false,
			schemas.Moonshot, postHook, nil, nil, nil, nil, nil, nil, nil, nil)
		if bifrostErr != nil {
			return bifrostErr
		}
		for range chunks {
		}
		return nil
	}
	for _, stream := range []bool{false, true} {
		name := "chat"
		if stream {
			name = "chat stream"
		}
		t.Run(name, func(t *testing.T) {
			before := calls.Load()
			refused := send(stream, schemas.BifrostServiceTierFlex)
			if refused == nil {
				t.Fatal("an unsupported tier must be refused")
			}
			if got := calls.Load() - before; got != 0 {
				t.Fatalf("%d upstream calls for a refused tier, want 0", got)
			}
			if refused.StatusCode == nil || *refused.StatusCode != 400 ||
				refused.ExtraFields.ErrorType != schemas.ErrorTypeCallerUnsupportedParameter ||
				refused.Error == nil || refused.Error.Param != "service_tier" {
				t.Fatalf("refusal = %+v (error %+v), want 400 caller_unsupported_parameter with param service_tier", refused, refused.Error)
			}

			if bifrostErr := send(stream, schemas.BifrostServiceTierPriority); bifrostErr != nil {
				t.Fatalf("a supported tier failed: %+v", bifrostErr.Error)
			}
			if got := calls.Load() - before; got != 1 {
				t.Fatalf("%d upstream calls for a supported tier, want 1", got)
			}
			body, _ := lastBody.Load().(map[string]any)
			if body["service_tier"] != "priority" {
				t.Fatalf("upstream service_tier = %v, want priority as sent", body["service_tier"])
			}
		})
	}
}

// BenchmarkChatServiceTierError is the check both chat handlers run before converting. The
// common request carries no tier and pays one nil comparison; a request with a tier pays the
// capability lookup the converter already made.
func BenchmarkChatServiceTierError(b *testing.B) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	input := []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser}}
	b.Run("no tier", func(b *testing.B) {
		req := &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "gpt-4o", Input: input, Params: &schemas.ChatParameters{}}
		b.ReportAllocs()
		for b.Loop() {
			if err := ChatServiceTierError(ctx, req); err != nil {
				b.Fatal(err.Error.Message)
			}
		}
	})
	b.Run("tier sent", func(b *testing.B) {
		tier := schemas.BifrostServiceTierPriority
		req := &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "gpt-4o", Input: input, Params: &schemas.ChatParameters{ServiceTier: &tier}}
		b.ReportAllocs()
		for b.Loop() {
			if err := ChatServiceTierError(ctx, req); err != nil {
				b.Fatal(err.Error.Message)
			}
		}
	})
}
