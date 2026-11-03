package tokun

import (
	"math/rand/v2"
	"sync"

	"github.com/tidwall/gjson"

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
	// body is the caller's request body as Bifrost received it, which the integration keeps on
	// the request (BifrostContextKeyKeepRawRequestBody); nil when the route keeps none. Every
	// attempt is rewritten from it. It is the transport's own buffer, not a copy.
	body []byte
	// openAIStream is a streamed OpenAI chat or completions request: a same-format attempt asks
	// for usage in the body.
	openAIStream bool
	// dropUsage is an openAIStream whose caller did not ask for usage itself: the usage the
	// stream reports is kept out of the chunks the caller receives.
	dropUsage bool
	// stripped lists the strip_params the latest attempt removed from the body.
	stripped []string
}

// capture records what every attempt of the send is rewritten from: the caller's body, and
// whether it is a streamed OpenAI chat or completions request that asked for usage.
func (state *requestState) capture(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) {
	state.body = req.GetRawRequestBody()
	integration, _ := ctx.Value(schemas.BifrostContextKeyIntegrationType).(string)
	switch req.RequestType {
	case schemas.ChatCompletionStreamRequest, schemas.TextCompletionStreamRequest:
		state.openAIStream = integration == integrationOpenAI && len(state.body) > 0
	}
	state.dropUsage = state.openAIStream && !gjson.GetBytes(state.body, "stream_options.include_usage").Bool()
}

// entry is one {provider, upstream, key} of the drawn order with the share the selection
// curve gave it in each layer: the fields of a RouteAttempts.order item (DrawnCandidate), whose
// same_format is the upstream's. A share is the probability that the candidate comes first in
// its layer and group, so within the group that comes first (same_format, when the plan has any)
// the product of the three is the probability that the entry was drawn as the primary.
type entry struct {
	provider      *routingv1.RouteProvider
	upstream      *routingv1.RouteUpstream
	key           *plannerv1.KeyShare
	providerShare float64
	upstreamShare float64
	keyShare      float64
	// belowThreshold is set when the provider scored below the plan's threshold or the upstream
	// below its provider's upstream threshold, so the entry was ranked by score, not drawn.
	belowThreshold bool
}

// skippedEntry is an entry of the order that the plugin did not send, and why
// (RouteAttempt.skip_reason).
type skippedEntry struct {
	index  int
	reason string
}

// SeedDraws makes the selection curve draw from one generator seeded with seed, shared by all
// requests under a lock, in place of math/rand/v2's global source, so that a test sending its
// requests one after another can reproduce the orders they are drawn in. Call it before the
// plugin serves a request.
func (p *Plugin) SeedDraws(seed uint64) {
	var mu sync.Mutex
	generator := rand.New(rand.NewPCG(seed, seed))
	p.uniform = func() float64 {
		mu.Lock()
		defer mu.Unlock()
		return generator.Float64()
	}
}

// drawOrder draws the order of every {provider, upstream, key} in the plan by the selection
// curve (Hot Path · 选择曲线), with uniform as the source of the uniform numbers.
//
// The upstreams of the request's format (same_format) form the first group and the converted ones
// the second; each group is drawn on its own and the first comes first, so every same-format
// entry is tried before any converted one. In a group, the providers that have upstreams in it
// are one layer, drawn at the plan's request_rate; under each provider drawn, its upstreams in
// the group are the next layer, drawn at the rate the provider took (its share of request_rate);
// under each upstream, its keys are drawn by the planner's shares. A provider's capacity is that
// of its upstreams in the group that take its traffic (providerCapacity).
func drawOrder(plan *routingv1.RoutePlan, uniform func() float64) []entry {
	providers := plan.GetProviders()
	entries, widest := 0, 0
	for _, provider := range providers {
		widest = max(widest, len(provider.GetUpstreams()))
		for _, upstream := range provider.GetUpstreams() {
			entries += len(upstream.GetKeys())
			widest = max(widest, len(upstream.GetKeys()))
		}
	}
	order := make([]entry, 0, entries)
	// One scratch array for the three layers in progress: a group's providers, one provider's
	// upstreams and one upstream's keys.
	scratch := make([]candidate, len(providers)+2*widest)
	providerLayer := scratch[:len(providers):len(providers)]
	upstreamLayer := scratch[len(providers) : len(providers)+widest : len(providers)+widest]
	keyLayer := scratch[len(providers)+widest:]

	rate, staleAfter := plan.GetRequestRate(), plan.GetStaleAfterSeconds()
	for _, sameFormat := range [...]bool{true, false} {
		drawnProviders := providerLayer[:0]
		for i, provider := range providers {
			capacity, inGroup := providerCapacity(provider, sameFormat)
			if !inGroup {
				continue
			}
			drawnProviders = append(drawnProviders, candidate{
				index: i, score: provider.GetScore(), uncertainty: provider.GetUncertainty(), capacity: capacity,
			})
		}
		rank(drawnProviders, plan.GetThreshold(), rate, staleAfter, uniform)

		for _, p := range drawnProviders {
			provider := providers[p.index]
			upstreams := provider.GetUpstreams()
			drawnUpstreams := upstreamLayer[:0]
			for j, upstream := range upstreams {
				if upstream.GetSameFormat() != sameFormat {
					continue
				}
				drawnUpstreams = append(drawnUpstreams, candidate{
					index: j, score: upstream.GetScore(), uncertainty: upstream.GetUncertainty(), capacity: upstream.GetCapacity(),
				})
			}
			rank(drawnUpstreams, provider.GetUpstreamThreshold(), p.share*rate, staleAfter, uniform)

			for _, u := range drawnUpstreams {
				upstream := upstreams[u.index]
				keys := upstream.GetKeys()
				drawnKeys := keyLayer[:0]
				for k, key := range keys {
					drawnKeys = append(drawnKeys, candidate{index: k, share: key.GetShare()})
				}
				rankKeys(drawnKeys, uniform)

				for _, k := range drawnKeys {
					order = append(order, entry{
						provider:       provider,
						upstream:       upstream,
						key:            keys[k.index],
						providerShare:  p.share,
						upstreamShare:  u.share,
						keyShare:       k.share,
						belowThreshold: !p.drawn || !u.drawn,
					})
				}
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
	state.order = drawOrder(state.plan, p.uniform)
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
