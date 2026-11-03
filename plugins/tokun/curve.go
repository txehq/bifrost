package tokun

import (
	"cmp"
	"math"
	"slices"

	routingv1 "github.com/capsohq/txe/tokun/proto/gen/go/tokun/routing/v1"
)

// The selection curve (Hot Path · 选择曲线) turns the planner's scores and uncertainties, the
// request rate, the upstreams' capacities and freshness.stale_after into one send's order. It has
// no coefficients of its own.

// candidate is one provider, upstream or key in a layer of the selection curve.
type candidate struct {
	// index is the candidate's position in the plan's list it comes from.
	index       int
	score       float64
	uncertainty float64
	// capacity is the requests per second the candidate can still take; 0 is no registered
	// limit.
	capacity float64
	// share is the candidate's share of the layer: the curve's s, then the final share p, the
	// probability that the candidate comes first in the layer. A key's is the planner's share
	// until rankKeys turns it into the key's part of the upstream's total.
	share float64
	// draw is the candidate's key in the weighted draw, ln(u)/p.
	draw float64
	// drawn is set for a candidate that takes part in the draw: one that scores at least the
	// layer's threshold, or a key whose share is above 0.
	drawn bool
	// cut is set for a candidate whose share clipToCapacity cut to its capacity.
	cut bool
}

// rank orders one provider or upstream layer by the selection curve (steps 2 to 7) and leaves
// each candidate's final share in share. rate is λ, the requests per second that enter the layer,
// and staleAfter is T, freshness.stale_after in seconds.
//
// The candidates that score at least threshold are drawn; the others follow them, ranked by
// score, with share 0. When none does, all are ranked by score and the first takes every request
// (share 1).
func rank(layer []candidate, threshold, rate, staleAfter float64, uniform func() float64) {
	if len(layer) == 0 {
		return
	}
	n := qualify(layer, threshold)
	if n == 0 {
		sortLayer(layer)
		layer[0].share = 1
		return
	}
	curveShares(layer)
	applyFloor(layer, n, rate, staleAfter)
	clipToCapacity(layer, rate)
	draw(layer, uniform)
	sortLayer(layer)
}

// rankKeys orders one upstream's keys (step 8). The keys whose share is above 0 are drawn, each
// by its part of the upstream's total share, as rank draws; the keys of share 0 (a key that failed
// authentication) follow in the plan's order with share 0. When no key has a share, all are in
// the plan's order and the first takes every request (share 1).
func rankKeys(layer []candidate, uniform func() float64) {
	if len(layer) == 0 {
		return
	}
	var total float64
	for i := range layer {
		c := &layer[i]
		c.drawn = c.share > 0
		if c.drawn {
			total += c.share
		} else {
			c.share = 0
		}
	}
	if total == 0 {
		sortLayer(layer)
		layer[0].share = 1
		return
	}
	for i := range layer {
		if layer[i].drawn {
			layer[i].share /= total
		}
	}
	draw(layer, uniform)
	sortLayer(layer)
}

// providerCapacity is a provider's capacity in one group: what its upstreams of the group that
// take its requests can still take. Those are the upstreams that score at least the provider's
// upstream threshold or, when none does, the top-scored one, which then takes them all. It is 0,
// no limit, when one of those has no registered limit. inGroup is false when the provider has no
// upstream in the group.
func providerCapacity(provider *routingv1.RouteProvider, sameFormat bool) (capacity float64, inGroup bool) {
	threshold := provider.GetUpstreamThreshold()
	var qualified float64
	anyQualified, unlimited := false, false
	var top *routingv1.RouteUpstream
	for _, upstream := range provider.GetUpstreams() {
		if upstream.GetSameFormat() != sameFormat {
			continue
		}
		inGroup = true
		switch {
		case upstream.GetScore() >= threshold:
			anyQualified = true
			if upstream.GetCapacity() > 0 {
				qualified += upstream.GetCapacity()
			} else {
				unlimited = true
			}
		case top == nil || upstream.GetScore() > top.GetScore():
			top = upstream
		}
	}
	switch {
	case !inGroup:
		return 0, false
	case !anyQualified:
		return max(top.GetCapacity(), 0), true
	case unlimited:
		return 0, true
	}
	return qualified, true
}

// qualify marks the candidates that score at least threshold as drawn (step 2) and returns how
// many are.
func qualify(layer []candidate, threshold float64) int {
	n := 0
	for i := range layer {
		c := &layer[i]
		c.drawn = c.score >= threshold
		c.share, c.cut = 0, false
		if c.drawn {
			n++
		}
	}
	return n
}

// curveShares sets every drawn candidate's share to s = w / Σw (steps 3 and 4), w being the
// weight of how far it trails the top-scored drawn candidate. The top's weight is 1, so its share
// is the largest.
func curveShares(layer []candidate) {
	top := -1
	for i := range layer {
		if layer[i].drawn && (top < 0 || layer[i].score > layer[top].score) {
			top = i
		}
	}
	var total float64
	for i := range layer {
		if layer[i].drawn {
			layer[i].share = weight(trail(layer[top], layer[i]))
			total += layer[i].share
		}
	}
	for i := range layer {
		if layer[i].drawn {
			layer[i].share /= total
		}
	}
}

// trail is z, how far c trails top measured in their uncertainty:
// (top's score − c's score) / √(top's uncertainty² + c's uncertainty²). A candidate that scores
// as top does trails by 0, even when neither score is uncertain.
func trail(top, c candidate) float64 {
	gap := top.score - c.score
	if gap == 0 {
		return 0
	}
	return gap / math.Hypot(top.uncertainty, c.uncertainty)
}

// weight is w = erfc(z / √2), the two-sided p-value of the hypothesis that a candidate trailing
// by z is as good as the top: 1 at z = 0, falling fast as z grows.
func weight(z float64) float64 {
	return math.Erfc(z / math.Sqrt2)
}

// applyFloor turns every drawn candidate's share s into p = φ + (1 − n × φ) × s, with the floor
// φ = min(1 / (λ × T), 1 / n) (step 5), so that each of the n drawn candidates takes on average
// at least one request every T and its signals do not go stale. When λ × T is 0, φ is 1 / n and
// the shares are equal.
func applyFloor(layer []candidate, n int, rate, staleAfter float64) {
	floor := math.Min(1/(rate*staleAfter), 1/float64(n))
	for i := range layer {
		if layer[i].drawn {
			layer[i].share = floor + (1-float64(n)*floor)*layer[i].share
		}
	}
}

// clipToCapacity keeps every drawn candidate's requests, p × λ, within its capacity (step 6). A
// candidate over its capacity is cut to capacity / λ, and what it loses goes to the candidates not
// cut, in proportion to their shares, until none is over. When every drawn candidate has a
// capacity and together they cannot take λ, the shares are the proportions of their capacities
// instead: the requests over capacity are sent all the same, and the fallbacks take the 429s.
func clipToCapacity(layer []candidate, rate float64) {
	if !(rate > 0) {
		return
	}
	var total float64
	unlimited := false
	for i := range layer {
		if !layer[i].drawn {
			continue
		}
		if layer[i].capacity > 0 {
			total += layer[i].capacity
		} else {
			unlimited = true
		}
	}
	if !unlimited && total < rate {
		for i := range layer {
			if layer[i].drawn {
				layer[i].share = layer[i].capacity / total
			}
		}
		return
	}
	for {
		var excess, free float64
		for i := range layer {
			c := &layer[i]
			if !c.drawn || c.cut {
				continue
			}
			if c.capacity > 0 && c.share*rate > c.capacity {
				limit := c.capacity / rate
				excess += c.share - limit
				c.share, c.cut = limit, true
			} else {
				free += c.share
			}
		}
		if excess == 0 || free == 0 {
			return
		}
		for i := range layer {
			c := &layer[i]
			if c.drawn && !c.cut {
				c.share += excess * c.share / free
			}
		}
	}
}

// draw gives every drawn candidate its key in a weighted draw without replacement (step 7): each
// takes u^(1/p) for a uniform u, and the largest comes first. The key kept is ln(u)/p, which ranks
// the same and does not underflow for a small p; u is taken in (0, 1], so ln(u) is finite.
func draw(layer []candidate, uniform func() float64) {
	for i := range layer {
		if layer[i].drawn {
			layer[i].draw = math.Log(1-uniform()) / layer[i].share
		}
	}
}

// sortLayer puts the drawn candidates first, by their draw keys from the largest, and then the
// others by score from the highest; ties keep the plan's order.
func sortLayer(layer []candidate) {
	slices.SortFunc(layer, func(a, b candidate) int {
		if a.drawn != b.drawn {
			if a.drawn {
				return -1
			}
			return 1
		}
		var order int
		if a.drawn {
			order = cmp.Compare(b.draw, a.draw)
		} else {
			order = cmp.Compare(b.score, a.score)
		}
		if order != 0 {
			return order
		}
		return cmp.Compare(a.index, b.index)
	})
}
