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
}

// An attempt the routing plugin did not route fails, and is not sent as the caller's own type:
// an index outside the routed types or not an int with 500 tokun_attempt_index_invalid, a target
// core cannot convert the request to with 501 tokun_route_plan_not_applied, neither falling back,
// and no conversion is left on the context.
func TestRoutedRequestTypesFailAnAttemptNotRouted(t *testing.T) {
	p, err := Init(Config{}, bifrost.NewNoOpLogger(), nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	responses := &schemas.BifrostRequest{RequestType: schemas.ResponsesRequest, ResponsesRequest: &schemas.BifrostResponsesRequest{Provider: "up-a", Model: "m"}}
	for _, tc := range []struct {
		name   string
		routed []schemas.RequestType
		index  any
		status int
		code   string
	}{
		{"an index past the routed types", []schemas.RequestType{schemas.ChatCompletionRequest}, 1, 500, "tokun_attempt_index_invalid"},
		{"an index below 0", []schemas.RequestType{schemas.ChatCompletionRequest}, -1, 500, "tokun_attempt_index_invalid"},
		{"no routed types at all", []schemas.RequestType{}, nil, 500, "tokun_attempt_index_invalid"},
		{"an index that is not an int", []schemas.RequestType{schemas.ChatCompletionRequest}, "0", 500, "tokun_attempt_index_invalid"},
		{"a target of another operation", []schemas.RequestType{schemas.CountTokensRequest}, 0, 501, "tokun_route_plan_not_applied"},
		{"a streamed target for a request that does not stream", []schemas.RequestType{schemas.ChatCompletionStreamRequest}, 0, 501, "tokun_route_plan_not_applied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
			ctx.SetValue(schemas.BifrostContextKeyRoutedRequestTypes, tc.routed)
			if tc.index != nil {
				ctx.SetValue(schemas.BifrostContextKeyFallbackIndex, tc.index)
			}
			got, shortCircuit, err := p.PreLLMHook(ctx, responses)
			if err != nil || shortCircuit == nil || shortCircuit.Response != nil || shortCircuit.Error == nil {
				t.Fatalf("PreLLMHook = %+v, %v; want the attempt failed", shortCircuit, err)
			}
			fault := shortCircuit.Error
			if fault.StatusCode == nil || *fault.StatusCode != tc.status || fault.Type == nil || *fault.Type != tc.code {
				t.Fatalf("the attempt failed with %+v, want %d %s", fault, tc.status, tc.code)
			}
			if fault.AllowFallbacks == nil || *fault.AllowFallbacks {
				t.Fatalf("AllowFallbacks = %v, want false", fault.AllowFallbacks)
			}
			if got != responses || got.RequestType != schemas.ResponsesRequest {
				t.Fatalf("PreLLMHook returned the request %+v", got)
			}
			if change := ctx.Value(schemas.BifrostContextKeyChangeRequestType); change != nil {
				t.Fatalf("the failed attempt carries the conversion %v", change)
			}
		})
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
