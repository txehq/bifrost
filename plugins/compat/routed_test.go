package compat

import (
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
)

// changeOf runs the plugin's PreLLMHook for the attempt at index (nil for none set) and returns
// the conversion it left on the context.
func changeOf(t testing.TB, p *CompatPlugin, ctx *schemas.BifrostContext, req *schemas.BifrostRequest, index any) any {
	t.Helper()
	if index != nil {
		ctx.SetValue(schemas.BifrostContextKeyFallbackIndex, index)
	}
	if _, shortCircuit, err := p.PreLLMHook(ctx, req); shortCircuit != nil || err != nil {
		t.Fatalf("PreLLMHook = %+v, %v", shortCircuit, err)
	}
	return ctx.Value(schemas.BifrostContextKeyChangeRequestType)
}

// With the request types a routing plugin routed (BifrostContextKeyRoutedRequestTypes), each
// attempt is sent as its own target's type: a Responses request goes to a chat completions
// target as a chat completion and to a Responses target as it is, in either order of the
// attempts, and a request without an attempt index is the primary.
func TestRoutedRequestTypesDecideEachAttempt(t *testing.T) {
	p, err := Init(Config{}, bifrost.NewNoOpLogger(), nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	responses := func(requestType schemas.RequestType) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: requestType, ResponsesRequest: &schemas.BifrostResponsesRequest{Provider: "up-a", Model: "m"}}
	}
	ctx := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRoutedRequestTypes, []schemas.RequestType{schemas.ChatCompletionRequest, schemas.ResponsesRequest, schemas.ChatCompletionRequest})
	if got := changeOf(t, p, ctx, responses(schemas.ResponsesRequest), nil); got != schemas.ChatCompletionRequest {
		t.Fatalf("the primary without an index: change %v, want chat_completion", got)
	}
	if got := changeOf(t, p, ctx, responses(schemas.ResponsesRequest), 1); got != nil {
		t.Fatalf("fallback 0, a Responses target: change %v, want none", got)
	}
	if got := changeOf(t, p, ctx, responses(schemas.ResponsesRequest), 2); got != schemas.ChatCompletionRequest {
		t.Fatalf("fallback 1, a chat completions target: change %v, want chat_completion", got)
	}

	stream := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
	stream.SetValue(schemas.BifrostContextKeyRoutedRequestTypes, []schemas.RequestType{schemas.ResponsesStreamRequest, schemas.ChatCompletionStreamRequest})
	if got := changeOf(t, p, stream, responses(schemas.ResponsesStreamRequest), 0); got != nil {
		t.Fatalf("a streamed Responses target: change %v, want none", got)
	}
	if got := changeOf(t, p, stream, responses(schemas.ResponsesStreamRequest), 1); got != schemas.ChatCompletionRequest {
		t.Fatalf("a streamed chat completions target: change %v, want chat_completion", got)
	}

	// An index outside the routed types, or a target core cannot convert to, changes nothing;
	// the routing plugin sends no such attempt.
	other := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
	other.SetValue(schemas.BifrostContextKeyRoutedRequestTypes, []schemas.RequestType{schemas.CountTokensRequest})
	if got := changeOf(t, p, other, responses(schemas.ResponsesRequest), 0); got != nil {
		t.Fatalf("an unconvertible target: change %v, want none", got)
	}
	if got := changeOf(t, p, other, responses(schemas.ResponsesRequest), 3); got != nil {
		t.Fatalf("an index outside the routed types: change %v, want none", got)
	}
}

// With routed request types, none of the plugin's own conversions applies: a decision the
// plugin would emulate through Responses (convert_decision_to_responses) goes to its target as a
// decision.
func TestRoutedRequestTypesReplaceTheCatalogConversions(t *testing.T) {
	p, err := Init(Config{ConvertDecisionToResponses: true}, bifrost.NewNoOpLogger(), nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	decision := func() *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: schemas.DecisionRequest, DecisionRequest: &schemas.BifrostDecisionRequest{Provider: schemas.Anthropic, Model: "claude-x"}}
	}
	unrouted := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
	if got := changeOf(t, p, unrouted, decision(), 0); got != schemas.ResponsesRequest {
		t.Fatalf("without routed types the decision is emulated: change %v, want responses", got)
	}
	routed := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
	routed.SetValue(schemas.BifrostContextKeyRoutedRequestTypes, []schemas.RequestType{schemas.DecisionRequest})
	if got := changeOf(t, p, routed, decision(), 0); got != nil {
		t.Fatalf("with routed types: change %v, want none", got)
	}
}

// BenchmarkPreLLMHook measures the hook with the server's default settings on a chat completion,
// without routed request types (a request no routing plugin routed) and with them.
func BenchmarkPreLLMHook(b *testing.B) {
	p, err := Init(Config{ForceReasoningOnlyModelsToResponses: true}, bifrost.NewNoOpLogger(), nil)
	if err != nil {
		b.Fatalf("Init: %v", err)
	}
	req := &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "gpt-x"}}
	run := func(b *testing.B, ctx *schemas.BifrostContext) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, _, err := p.PreLLMHook(ctx, req); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.Run("without routed request types", func(b *testing.B) {
		run(b, schemas.NewBifrostContext(b.Context(), schemas.NoDeadline))
	})
	b.Run("with routed request types", func(b *testing.B) {
		ctx := schemas.NewBifrostContext(b.Context(), schemas.NoDeadline)
		ctx.SetValue(schemas.BifrostContextKeyRoutedRequestTypes, []schemas.RequestType{schemas.ResponsesRequest})
		ctx.SetValue(schemas.BifrostContextKeyFallbackIndex, 0)
		run(b, ctx)
	})
}
