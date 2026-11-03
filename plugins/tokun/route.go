package tokun

import (
	"github.com/capsohq/bifrost/core/schemas"

	plannerv1 "github.com/capsohq/txe/tokun/proto/gen/go/tokun/planner/v1"
	routingv1 "github.com/capsohq/txe/tokun/proto/gen/go/tokun/routing/v1"
)

// requestStateKey carries a send's *requestState on the request context. The state is
// bounded by the plan, which fits in one request header, and is released with the request.
const requestStateKey schemas.BifrostContextKey = "tokun-route-request"

// requestState is what the plugin keeps for one send.
type requestState struct {
	plan   *routingv1.RoutePlan
	claims *routingv1.RoutePlanClaims
	// order is the drawn order of every {provider, upstream, key} in the plan
	// (RouteAttempts.order).
	order []entry
	// chain is Bifrost's attempt chain as indexes into order: chain[0] is the primary and
	// chain[i] the entry of fallback i, so BifrostContextKeyFallbackIndex indexes chain.
	chain []int
	// skipped lists the entries of order that were not sent, with why.
	skipped []skippedEntry
}

// entry is one {provider, upstream, key} of the drawn order with the share the selection
// curve gave it in each layer.
type entry struct {
	provider       *routingv1.RouteProvider
	upstream       *routingv1.RouteUpstream
	key            *plannerv1.KeyShare
	providerShare  float64
	upstreamShare  float64
	keyShare       float64
	belowThreshold bool
}

// skippedEntry is an entry of the order that the plugin did not send, and why
// (RouteAttempt.skip_reason).
type skippedEntry struct {
	index  int
	reason string
}

// TODO(tokun-B1): 按选择曲线分组（同格式在前）、按资格和 erfc 权重算出供应商和上游两层的份额，加上地板值、按余量截取，再不放回抽取供应商、上游和 key 的顺序，记下每一层的份额，低于阈值的候选按分数排在这一层的最后
// 阶段：B1 放入；B1-D5「选择曲线」完成时删除这两行和占位
func drawOrder(plan *routingv1.RoutePlan) []entry {
	var order []entry
	for _, provider := range plan.GetProviders() {
		for _, upstream := range provider.GetUpstreams() {
			for _, key := range upstream.GetKeys() {
				order = append(order, entry{provider: provider, upstream: upstream, key: key})
			}
		}
	}
	return order
}

// TODO(tokun-B1): 按 key 的名称（tokun 的 key_id）找到 Bifrost 里这把 key 的 id；Bifrost 还没有这个 provider 条目或这把 key 时返回 provider_not_synced 或 key_not_synced，这一项不发出
// 阶段：B1 放入；B1-D8「上游和 key 在 Bifrost 里的对应」完成时删除这两行和占位
func (p *Plugin) bifrostTarget(e entry) (schemas.Fallback, string) {
	return schemas.Fallback{
		Provider: schemas.ModelProvider(e.upstream.GetBifrostProvider()),
		Model:    e.upstream.GetForwardModel(),
		KeyID:    e.key.GetKeyId(),
	}, ""
}

// route sets req to dispatch inside the plan only: the primary and the fallbacks are the
// drawn order's entries, each with its own key pinned, and whatever provider, model,
// fallbacks or key the caller named is replaced. Entries Bifrost does not have are left
// out, and at most max_attempts entries are sent.
func (p *Plugin) route(ctx *schemas.BifrostContext, req *schemas.BifrostRequest, state *requestState) {
	state.order = drawOrder(state.plan)
	maxAttempts := int(state.plan.GetMaxAttempts())
	targets := make([]schemas.Fallback, 0, len(state.order))
	state.chain = make([]int, 0, len(state.order))
	for i, e := range state.order {
		if maxAttempts > 0 && len(targets) == maxAttempts {
			break
		}
		target, skipReason := p.bifrostTarget(e)
		if skipReason != "" {
			state.skipped = append(state.skipped, skippedEntry{index: i, reason: skipReason})
			continue
		}
		targets = append(targets, target)
		state.chain = append(state.chain, i)
	}
	if len(targets) == 0 {
		// Nothing in the plan can be sent from this Bifrost. The request keeps the first
		// entry's route, so no later routing step picks another, and PreLLMHook fails it
		// before any upstream is called.
		first := state.order[0].upstream
		req.SetProvider(schemas.ModelProvider(first.GetBifrostProvider()))
		req.SetModel(first.GetForwardModel())
		req.SetFallbacks(nil)
		return
	}
	req.SetProvider(targets[0].Provider)
	req.SetModel(targets[0].Model)
	req.SetFallbacks(targets[1:])
	// Core commits this pin as the primary's key after the pre-request hooks, over any key
	// the caller named; each fallback re-pins its own KeyID.
	ctx.SetValue(schemas.BifrostContextKeyRoutingPinnedAPIKeyID, targets[0].KeyID)
}
