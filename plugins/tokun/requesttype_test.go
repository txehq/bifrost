package tokun

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"

	plannerv1 "github.com/capsohq/txe/tokun/proto/gen/go/tokun/planner/v1"
	routingv1 "github.com/capsohq/txe/tokun/proto/gen/go/tokun/routing/v1"
)

// orderedPlan lists each upstream under a provider of its own, each provider scored far below
// the one before it with almost no uncertainty, so the selection curve draws the upstreams of
// each group in the order given, the same_format ones first.
func orderedPlan(upstreams ...*routingv1.RouteUpstream) *routingv1.RoutePlan {
	plan := &routingv1.RoutePlan{RequestId: testRequestID, RequestRate: 1e9, StaleAfterSeconds: 30}
	for i, upstream := range upstreams {
		score := 0.9 - 0.1*float64(i)
		upstream.UpstreamId = int64(i + 1)
		upstream.Score, upstream.Uncertainty = score, 0.001
		upstream.Keys = []*plannerv1.KeyShare{{KeyId: fmt.Sprintf("key-%d", i+1), Share: 1}}
		plan.Providers = append(plan.Providers, &routingv1.RouteProvider{
			ProviderId: fmt.Sprintf("provider-%d", i+1), Score: score, Uncertainty: 0.001,
			Upstreams: []*routingv1.RouteUpstream{upstream},
		})
	}
	return plan
}

// typedUpstream is an upstream on the Bifrost entry provider that takes requestType.
func typedUpstream(provider string, sameFormat bool, requestType schemas.RequestType) *routingv1.RouteUpstream {
	return &routingv1.RouteUpstream{BifrostProvider: provider, ForwardModel: "model-" + provider, SameFormat: sameFormat, RequestType: string(requestType)}
}

// routeRequest runs PreRequestHook for a request of requestType carrying plan, and returns its
// context and state.
func routeRequest(t testing.TB, plugin *Plugin, plan *routingv1.RoutePlan, requestType schemas.RequestType, route string) (*schemas.BifrostContext, *requestState) {
	t.Helper()
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	state := &requestState{plan: plan, route: route}
	ctx.SetValue(requestStateKey, state)
	req := &schemas.BifrostRequest{RequestType: requestType}
	switch requestType {
	case schemas.CountTokensRequest:
		req.CountTokensRequest = &schemas.BifrostResponsesRequest{}
	case schemas.ChatCompletionRequest, schemas.ChatCompletionStreamRequest:
		req.ChatRequest = &schemas.BifrostChatRequest{}
	default:
		req.ResponsesRequest = &schemas.BifrostResponsesRequest{}
	}
	if err := plugin.PreRequestHook(ctx, req); err != nil {
		t.Fatalf("PreRequestHook: %v", err)
	}
	return ctx, state
}

// Each entry is sent as its request_type: an entry in the request's format of the request's own
// type and a converted entry of a type core converts the request to are sent, and their types
// are on the context for the compat plugin, in the order of the attempts; an entry without the
// operation, one of a type core does not convert to, and one in the request's format of another
// type are left out with their skip_reasons.
func TestRouteSendsEachEntryAsItsRequestType(t *testing.T) {
	plugin := testPlugin(t, map[string]string{"gw-2026-11": "gateway-secret"})
	plan := orderedPlan(
		typedUpstream("up-responses", true, schemas.ResponsesRequest),
		typedUpstream("up-same-chat", true, schemas.ChatCompletionRequest),
		typedUpstream("up-chat", false, schemas.ChatCompletionRequest),
		typedUpstream("up-none", false, ""),
		typedUpstream("up-count", false, schemas.CountTokensRequest),
		typedUpstream("up-messages", false, schemas.ResponsesRequest),
	)
	ctx, state := routeRequest(t, plugin, plan, schemas.ResponsesRequest, "/openai/v1/responses")
	var sent []string
	for _, i := range state.chain {
		sent = append(sent, state.order[i].upstream.GetBifrostProvider())
	}
	if want := []string{"up-responses", "up-chat", "up-messages"}; !reflect.DeepEqual(sent, want) {
		t.Fatalf("sent %v, want %v", sent, want)
	}
	want := []schemas.RequestType{schemas.ResponsesRequest, schemas.ChatCompletionRequest, schemas.ResponsesRequest}
	if !reflect.DeepEqual(state.routed, want) {
		t.Fatalf("routed %v, want %v", state.routed, want)
	}
	if got, _ := ctx.Value(schemas.BifrostContextKeyRoutedRequestTypes).([]schemas.RequestType); !reflect.DeepEqual(got, want) {
		t.Fatalf("the context's routed request types are %v, want %v", got, want)
	}
	skipped := map[string]string{}
	for _, s := range state.skipped {
		skipped[state.order[s.index].upstream.GetBifrostProvider()] = s.reason
	}
	wantSkipped := map[string]string{
		"up-same-chat": skipRequestTypeUnconvertible,
		"up-none":      skipOperationNotOffered,
		"up-count":     skipRequestTypeUnconvertible,
	}
	if !reflect.DeepEqual(skipped, wantSkipped) {
		t.Fatalf("skipped %v, want %v", skipped, wantSkipped)
	}
	if state.estimate {
		t.Fatal("a Responses request is not answered with the local count")
	}

	// A streamed request is sent only to the streamed types.
	streamPlan := orderedPlan(
		typedUpstream("up-chat-stream", false, schemas.ChatCompletionStreamRequest),
		typedUpstream("up-chat", false, schemas.ChatCompletionRequest),
	)
	_, streamState := routeRequest(t, plugin, streamPlan, schemas.ResponsesStreamRequest, "/openai/v1/responses")
	if len(streamState.chain) != 1 || streamState.order[streamState.chain[0]].upstream.GetBifrostProvider() != "up-chat-stream" ||
		!reflect.DeepEqual(streamState.routed, []schemas.RequestType{schemas.ChatCompletionStreamRequest}) {
		t.Fatalf("a streamed Responses request sent to %v as %v, want up-chat-stream as chat_completion_stream", streamState.chain, streamState.routed)
	}
}

// A count_tokens request is sent to the entries whose interface has the operation; when none
// has it the plugin answers with the local count and sends nothing; when one has it but cannot
// be sent, the request fails.
func TestRouteCountsTokensWhereAnUpstreamCounts(t *testing.T) {
	plugin := testPlugin(t, map[string]string{"gw-2026-11": "gateway-secret"})
	req := &schemas.BifrostRequest{RequestType: schemas.CountTokensRequest, CountTokensRequest: &schemas.BifrostResponsesRequest{}}

	counting := orderedPlan(typedUpstream("up-none", true, ""), typedUpstream("up-count", false, schemas.CountTokensRequest))
	_, state := routeRequest(t, plugin, counting, schemas.CountTokensRequest, "/anthropic/v1/messages/count_tokens")
	if state.estimate || len(state.chain) != 1 || state.order[state.chain[0]].upstream.GetBifrostProvider() != "up-count" {
		t.Fatalf("estimate %v, chain %v: want the request sent to up-count", state.estimate, state.chain)
	}

	noneCounts := orderedPlan(typedUpstream("up-a", true, ""), typedUpstream("up-b", false, ""))
	noneCounts.TokenEstimate = testTokenEstimate()
	ctx, state := routeRequest(t, plugin, noneCounts, schemas.CountTokensRequest, "/anthropic/v1/messages/count_tokens")
	state.body = []byte(`{"model":"tk/claude","messages":[{"role":"user","content":"abcdefghi"}]}`)
	if !state.estimate || len(state.chain) != 0 || len(state.skipped) != 2 {
		t.Fatalf("estimate %v, chain %v, skipped %v: want the local count and both entries skipped", state.estimate, state.chain, state.skipped)
	}
	_, shortCircuit, err := plugin.PreLLMHook(ctx, req)
	if err != nil || shortCircuit == nil || shortCircuit.Response == nil || shortCircuit.Response.CountTokensResponse == nil {
		t.Fatalf("PreLLMHook = %+v, %v; want the local count", shortCircuit, err)
	}
	// 9 bytes of text at 3 per token, and the overhead.
	if count := shortCircuit.Response.CountTokensResponse; count.InputTokens != 3+512 || count.Model != "tk/claude" {
		t.Fatalf("count %+v, want 515 input tokens for tk/claude", count)
	}

	// The only entry with a type other than count_tokens has the operation in a type the request
	// cannot be sent as: the plan offers a count, so the request fails rather than being counted.
	wrongType := orderedPlan(typedUpstream("up-a", true, ""), typedUpstream("up-b", false, schemas.ResponsesRequest))
	wrongType.TokenEstimate = testTokenEstimate()
	ctx, state = routeRequest(t, plugin, wrongType, schemas.CountTokensRequest, "/anthropic/v1/messages/count_tokens")
	if state.estimate {
		t.Fatal("a plan with an upstream that has the operation is not answered with the local count")
	}
	if _, shortCircuit, _ := plugin.PreLLMHook(ctx, req); shortCircuit == nil || shortCircuit.Error == nil ||
		shortCircuit.Error.StatusCode == nil || *shortCircuit.Error.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("PreLLMHook = %+v, want 503 tokun_route_plan_unroutable", shortCircuit)
	}
}

// A Responses stream sent as a chat completion stream whose provider streams chat chunks, not
// the Responses events Bifrost makes of them, fails the attempt and falls back; Responses events
// pass, and so do chat chunks of a chat completion stream.
func TestPostLLMHookFailsAStreamNotConvertedToResponses(t *testing.T) {
	plugin := testPlugin(t, map[string]string{"gw-2026-11": "gateway-secret"})
	plan := orderedPlan(
		typedUpstream("up-chat-stream", false, schemas.ChatCompletionStreamRequest),
		typedUpstream("up-responses", false, schemas.ResponsesStreamRequest),
	)
	ctx, _ := routeRequest(t, plugin, plan, schemas.ResponsesStreamRequest, "/openai/v1/responses")
	ctx.SetValue(schemas.BifrostContextKeyFallbackIndex, 0)
	chatChunk := &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{ExtraFields: schemas.BifrostResponseExtraFields{Provider: "up-chat-stream"}}}
	resp, bifrostErr, err := plugin.PostLLMHook(ctx, chatChunk, nil)
	if err != nil || resp != nil || bifrostErr == nil || bifrostErr.Type == nil || *bifrostErr.Type != "tokun_attempt_stream_unconverted" {
		t.Fatalf("PostLLMHook = %v, %+v, %v; want tokun_attempt_stream_unconverted", resp, bifrostErr, err)
	}
	if bifrostErr.AllowFallbacks != nil {
		t.Fatalf("AllowFallbacks = %v, want the send to fall back", *bifrostErr.AllowFallbacks)
	}

	ctx.SetValue(schemas.BifrostContextKeyFallbackIndex, 1)
	event := &schemas.BifrostResponse{ResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{}}
	if resp, bifrostErr, err := plugin.PostLLMHook(ctx, event, nil); resp != event || bifrostErr != nil || err != nil {
		t.Fatalf("a Responses event: PostLLMHook = %v, %+v, %v", resp, bifrostErr, err)
	}

	chatCtx, _ := routeRequest(t, plugin, orderedPlan(typedUpstream("up-chat", true, schemas.ChatCompletionStreamRequest)), schemas.ChatCompletionStreamRequest, "/openai/v1/chat/completions")
	if resp, bifrostErr, err := plugin.PostLLMHook(chatCtx, chatChunk, nil); resp != chatChunk || bifrostErr != nil || err != nil {
		t.Fatalf("a chat chunk of a chat stream: PostLLMHook = %v, %+v, %v", resp, bifrostErr, err)
	}
}
