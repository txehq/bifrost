package tokun

import (
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"testing"

	"gonum.org/v1/gonum/stat"
	"gonum.org/v1/gonum/stat/distuv"

	"github.com/capsohq/bifrost/core/schemas"

	plannerv1 "github.com/capsohq/txe/tokun/proto/gen/go/tokun/planner/v1"
	routingv1 "github.com/capsohq/txe/tokun/proto/gen/go/tokun/routing/v1"
)

// seeded is a uniform source that repeats for the same seed.
func seeded(seed uint64) func() float64 {
	return rand.New(rand.NewPCG(seed, seed)).Float64
}

// noDraw is a uniform source for a layer that must not draw.
func noDraw(t *testing.T) func() float64 {
	return func() float64 {
		t.Helper()
		t.Fatal("drew a random number")
		return 0
	}
}

// roundsTo reports whether got shows as want when rounded to unit, the last digit the design
// prints.
func roundsTo(got, want, unit float64) bool {
	return math.Abs(got-want) <= unit/2
}

// designPlan is the example plan of Hot Path · 路由计划, which Hot Path · 选择曲线 works through.
func designPlan() *routingv1.RoutePlan {
	return &routingv1.RoutePlan{
		RequestId:         "req_01JC9B2K7XQ4",
		RequestRate:       200,
		StaleAfterSeconds: 30,
		Threshold:         0.5,
		Providers: []*routingv1.RouteProvider{
			{ProviderId: "nimbus-cloud", Score: 0.82, Uncertainty: 0.02, UpstreamThreshold: 0.4, Upstreams: []*routingv1.RouteUpstream{{
				UpstreamId: 31, Interface: "48213:text.chat.anthropic-messages", BifrostProvider: "openrouter-us-2",
				ForwardModel: "anthropic/claude-opus-4.5", StripParams: []string{"temperature"}, Score: 0.77, Uncertainty: 0.03,
				Capacity: 150, Keys: []*plannerv1.KeyShare{{KeyId: "or-us-2-k1", Share: 0.33}, {KeyId: "or-us-2-k2", Share: 0.67}},
				InterfaceOutputTokens: 64000, SameFormat: true,
			}}},
			{ProviderId: "anthropic", Score: 0.74, Uncertainty: 0.03, UpstreamThreshold: 0.4, Upstreams: []*routingv1.RouteUpstream{{
				UpstreamId: 12, Interface: "51007:text.chat.anthropic-messages", BifrostProvider: "anthropic",
				ForwardModel: "claude-opus-4-5-20251101", StripParams: []string{}, Score: 0.9, Uncertainty: 0.01,
				Capacity: 66.7, Keys: []*plannerv1.KeyShare{{KeyId: "anth-k1", Share: 1}},
				InterfaceOutputTokens: 64000, SameFormat: true,
			}}},
		},
	}
}

// providerCandidates is a group's provider layer of plan, built as drawOrder builds it.
func providerCandidates(plan *routingv1.RoutePlan, sameFormat bool) []candidate {
	var layer []candidate
	for i, provider := range plan.GetProviders() {
		if capacity, inGroup := providerCapacity(provider, sameFormat); inGroup {
			layer = append(layer, candidate{index: i, score: provider.GetScore(), uncertainty: provider.GetUncertainty(), capacity: capacity})
		}
	}
	return layer
}

// keyCandidates is an upstream's key layer, built as drawOrder builds it.
func keyCandidates(shares ...float64) []candidate {
	layer := make([]candidate, len(shares))
	for i, share := range shares {
		layer[i] = candidate{index: i, share: share}
	}
	return layer
}

// orderOf lists the plan positions of a ranked layer.
func orderOf(layer []candidate) []int {
	order := make([]int, len(layer))
	for i, c := range layer {
		order[i] = c.index
	}
	return order
}

// sharesOf lists the shares of a layer, by plan position.
func sharesOf(layer []candidate) []float64 {
	shares := make([]float64, len(layer))
	for _, c := range layer {
		shares[c.index] = c.share
	}
	return shares
}

// The weight table of Hot Path · 选择曲线.
func TestWeightTable(t *testing.T) {
	for _, row := range []struct{ z, w, unit float64 }{
		{0, 1, 0},
		{0.5, 0.617, 0.001},
		{1, 0.317, 0.001},
		{2, 0.0455, 0.0001},
		{3, 0.0027, 0.0001},
		{4, 0.000063, 0.000001},
	} {
		if got := weight(row.z); !roundsTo(got, row.w, row.unit) {
			t.Errorf("w(z = %v) = %v, want %v", row.z, got, row.w)
		}
	}
	if got := trail(candidate{score: 0.8}, candidate{score: 0.8}); got != 0 {
		t.Errorf("a tie with no uncertainty trails by %v, want 0", got)
	}
	if got := weight(trail(candidate{score: 0.8}, candidate{score: 0.7})); got != 0 {
		t.Errorf("a certain gap weighs %v, want 0", got)
	}
}

// The four rows of the worked example in Hot Path · 选择曲线, on the provider layer of the example
// plan, and the shares drawOrder records for it.
func TestDesignExample(t *testing.T) {
	plan := designPlan()
	nimbus, anthropic := 0, 1

	t.Run("uncertainties 0.02 and 0.03", func(t *testing.T) {
		layer := providerCandidates(plan, true)
		if n := qualify(layer, plan.GetThreshold()); n != 2 {
			t.Fatalf("%d providers qualify, want 2", n)
		}
		z := trail(layer[nimbus], layer[anthropic])
		if !roundsTo(z, 2.22, 0.01) || !roundsTo(weight(z), 0.0265, 0.0001) {
			t.Errorf("z = %v, w = %v; want 2.22 and 0.0265", z, weight(z))
		}
		curveShares(layer)
		if s := sharesOf(layer); !roundsTo(s[nimbus], 0.974, 0.001) || !roundsTo(s[anthropic], 0.026, 0.001) {
			t.Errorf("shares = %v, want 97.4%% and 2.6%%", s)
		}
	})

	t.Run("both uncertainties 0.08", func(t *testing.T) {
		layer := providerCandidates(plan, true)
		layer[nimbus].uncertainty, layer[anthropic].uncertainty = 0.08, 0.08
		qualify(layer, plan.GetThreshold())
		z := trail(layer[nimbus], layer[anthropic])
		if !roundsTo(z, 0.71, 0.01) || !roundsTo(weight(z), 0.48, 0.01) {
			t.Errorf("z = %v, w = %v; want 0.71 and 0.48", z, weight(z))
		}
		curveShares(layer)
		if s := sharesOf(layer); !roundsTo(s[nimbus], 0.676, 0.001) || !roundsTo(s[anthropic], 0.324, 0.001) {
			t.Errorf("shares = %v, want 67.6%% and 32.4%%", s)
		}
	})

	t.Run("with the floor, then within capacity", func(t *testing.T) {
		layer := providerCandidates(plan, true)
		if layer[nimbus].capacity != 150 || layer[anthropic].capacity != 66.7 {
			t.Fatalf("capacities %v and %v, want 150 and 66.7", layer[nimbus].capacity, layer[anthropic].capacity)
		}
		n := qualify(layer, plan.GetThreshold())
		curveShares(layer)
		s := sharesOf(layer)
		applyFloor(layer, n, plan.GetRequestRate(), plan.GetStaleAfterSeconds())
		p := sharesOf(layer)
		const floor = 1.0 / 6000
		for i := range p {
			if want := floor + (1-2*floor)*s[i]; math.Abs(p[i]-want) > 1e-15 {
				t.Errorf("p[%d] = %v, want φ + (1 − 2φ)s = %v with φ = 1/6000", i, p[i], want)
			}
		}
		if !roundsTo(p[nimbus], 0.974, 0.001) || !roundsTo(p[anthropic], 0.026, 0.001) {
			t.Errorf("shares with the floor = %v, want 97.4%% and 2.6%%", p)
		}

		clipToCapacity(layer, plan.GetRequestRate())
		p = sharesOf(layer)
		if math.Abs(p[nimbus]-0.75) > 1e-12 || math.Abs(p[anthropic]-0.25) > 1e-12 {
			t.Errorf("shares within capacity = %v, want 75%% and 25%%", p)
		}
		if requests := p[anthropic] * plan.GetRequestRate(); math.Abs(requests-50) > 1e-9 || requests > 66.7 {
			t.Errorf("anthropic takes %v requests a second, want 50, within its 66.7", requests)
		}

		keys := keyCandidates(0.33, 0.67)
		rankKeys(keys, seeded(1))
		if k := sharesOf(keys); math.Abs(k[0]-0.33) > 1e-12 || math.Abs(k[1]-0.67) > 1e-12 {
			t.Errorf("key shares = %v, want 33%% and 67%%", k)
		}
	})

	t.Run("the shares drawOrder records", func(t *testing.T) {
		want := map[string][3]float64{
			"or-us-2-k1": {0.75, 1, 0.33},
			"or-us-2-k2": {0.75, 1, 0.67},
			"anth-k1":    {0.25, 1, 1},
		}
		order := drawOrder(plan, seeded(1))
		if len(order) != 3 {
			t.Fatalf("drew %d entries, want 3", len(order))
		}
		for _, e := range order {
			shares := want[e.key.GetKeyId()]
			got := [3]float64{e.providerShare, e.upstreamShare, e.keyShare}
			for layer := range got {
				if math.Abs(got[layer]-shares[layer]) > 1e-12 {
					t.Errorf("%s: shares %v, want %v", e.key.GetKeyId(), got, shares)
					break
				}
			}
			if e.belowThreshold {
				t.Errorf("%s is below a threshold", e.key.GetKeyId())
			}
		}
	})
}

// A layer's draw: the frequency each candidate comes first, and each ordered pair comes first and
// second, match the fixed final shares (Pearson's chi-square test).
func TestDrawFrequenciesMatchTheShares(t *testing.T) {
	shares := []float64{0.5, 0.3, 0.15, 0.05}
	const draws = 200000
	uniform := seeded(20261104)
	first := make([]float64, len(shares))
	pairs := make([]float64, len(shares)*len(shares))
	layer := make([]candidate, len(shares))
	for range draws {
		for i, share := range shares {
			layer[i] = candidate{index: i, share: share, drawn: true}
		}
		draw(layer, uniform)
		sortLayer(layer)
		first[layer[0].index]++
		pairs[layer[0].index*len(shares)+layer[1].index]++
	}

	expectedFirst := make([]float64, len(shares))
	for i, share := range shares {
		expectedFirst[i] = draws * share
	}
	// Without replacement, j comes second after i with probability p_j / (1 − p_i).
	var observedPairs, expectedPairs []float64
	for i, pi := range shares {
		for j, pj := range shares {
			if i == j {
				if pairs[i*len(shares)+j] != 0 {
					t.Fatalf("candidate %d came both first and second", i)
				}
				continue
			}
			observedPairs = append(observedPairs, pairs[i*len(shares)+j])
			expectedPairs = append(expectedPairs, draws*pi*pj/(1-pi))
		}
	}
	for _, test := range []struct {
		name               string
		observed, expected []float64
	}{
		{"first", first, expectedFirst},
		{"first and second", observedPairs, expectedPairs},
	} {
		statistic := stat.ChiSquare(test.observed, test.expected)
		freedom := float64(len(test.observed) - 1)
		pValue := distuv.ChiSquared{K: freedom}.Survival(statistic)
		t.Logf("%s: %d draws, %d cells, χ² = %.2f, df = %v, p = %.3f", test.name, draws, len(test.observed), statistic, freedom, pValue)
		if pValue < 0.01 {
			t.Errorf("%s: the frequencies %v do not match %v (p = %.4f)", test.name, test.observed, test.expected, pValue)
		}
	}
}

// chiPlan has three qualifying same-format providers, one below the threshold, one converted
// upstream, an upstream over its capacity and keys of unequal shares.
func chiPlan() *routingv1.RoutePlan {
	keys := func(shares ...float64) []*plannerv1.KeyShare {
		var list []*plannerv1.KeyShare
		for _, share := range shares {
			list = append(list, &plannerv1.KeyShare{KeyId: fmt.Sprintf("k%d", len(list)), Share: share})
		}
		return list
	}
	return &routingv1.RoutePlan{
		RequestRate: 2, StaleAfterSeconds: 30, Threshold: 0.5,
		Providers: []*routingv1.RouteProvider{
			{ProviderId: "p1", Score: 0.80, Uncertainty: 0.05, UpstreamThreshold: 0.4, Upstreams: []*routingv1.RouteUpstream{
				{UpstreamId: 11, Score: 0.8, Uncertainty: 0.05, Capacity: 0.5, SameFormat: true, Keys: keys(0.5, 0.5)},
				{UpstreamId: 12, Score: 0.75, Uncertainty: 0.05, SameFormat: true, Keys: keys(1)},
				{UpstreamId: 13, Score: 0.9, Uncertainty: 0.01, Keys: keys(1)},
			}},
			{ProviderId: "p2", Score: 0.78, Uncertainty: 0.05, UpstreamThreshold: 0.4, Upstreams: []*routingv1.RouteUpstream{
				{UpstreamId: 21, Score: 0.7, Uncertainty: 0.1, SameFormat: true, Keys: keys(1, 3)},
			}},
			{ProviderId: "p3", Score: 0.76, Uncertainty: 0.05, UpstreamThreshold: 0.4, Upstreams: []*routingv1.RouteUpstream{
				{UpstreamId: 31, Score: 0.6, Uncertainty: 0.05, SameFormat: true, Keys: keys(1)},
			}},
			{ProviderId: "p4", Score: 0.2, Uncertainty: 0.05, UpstreamThreshold: 0.4, Upstreams: []*routingv1.RouteUpstream{
				{UpstreamId: 41, Score: 0.6, Uncertainty: 0.05, SameFormat: true, Keys: keys(1)},
			}},
		},
	}
}

// The whole order: the frequency each entry is drawn as the primary matches the product of the
// three shares drawOrder records for it, and an entry whose product is 0 (below a threshold, or
// in the converted group) is never the primary.
func TestPrimaryFrequenciesMatchTheRecordedShares(t *testing.T) {
	plan := chiPlan()
	name := func(e entry) string { return fmt.Sprintf("%d/%s", e.upstream.GetUpstreamId(), e.key.GetKeyId()) }
	propensity := map[string]float64{}
	var cells []string
	for _, e := range drawOrder(plan, seeded(1)) {
		propensity[name(e)] = e.providerShare * e.upstreamShare * e.keyShare
		if !e.upstream.GetSameFormat() || e.belowThreshold {
			propensity[name(e)] = 0
			continue
		}
		cells = append(cells, name(e))
	}
	if len(cells) != 6 {
		t.Fatalf("%d entries can be the primary (%v), want 6", len(cells), cells)
	}

	const draws = 100000
	uniform := seeded(42)
	primaries := map[string]float64{}
	for range draws {
		primaries[name(drawOrder(plan, uniform)[0])]++
	}
	observed := make([]float64, len(cells))
	expected := make([]float64, len(cells))
	var total float64
	for i, cell := range cells {
		observed[i], expected[i] = primaries[cell], draws*propensity[cell]
		total += propensity[cell]
	}
	if math.Abs(total-1) > 1e-12 {
		t.Fatalf("the primaries' probabilities add up to %v, want 1", total)
	}
	for cell, count := range primaries {
		if propensity[cell] == 0 {
			t.Errorf("%s, whose shares multiply to 0, was the primary %v times", cell, count)
		}
	}
	statistic := stat.ChiSquare(observed, expected)
	freedom := float64(len(cells) - 1)
	pValue := distuv.ChiSquared{K: freedom}.Survival(statistic)
	t.Logf("%d draws of the order, %d cells, χ² = %.2f, df = %v, p = %.3f", draws, len(cells), statistic, freedom, pValue)
	if pValue < 0.01 {
		t.Errorf("primary frequencies %v do not match %v (p = %.4f)", observed, expected, pValue)
	}
}

// The upstreams of the request's format come first: a provider with upstreams in both groups is
// drawn in each, and the converted group, drawn on its own, follows every same-format entry.
func TestDrawOrderPutsTheSameFormatGroupFirst(t *testing.T) {
	plan := &routingv1.RoutePlan{
		RequestRate: 200, StaleAfterSeconds: 30, Threshold: 0.5,
		Providers: []*routingv1.RouteProvider{
			// The top score, but only converted upstreams.
			{ProviderId: "converted", Score: 0.95, Uncertainty: 0.01, UpstreamThreshold: 0.4, Upstreams: []*routingv1.RouteUpstream{
				{UpstreamId: 1, Score: 0.9, Uncertainty: 0.01, Keys: []*plannerv1.KeyShare{{KeyId: "c1", Share: 1}}},
			}},
			{ProviderId: "both", Score: 0.85, Uncertainty: 0.05, UpstreamThreshold: 0.4, Upstreams: []*routingv1.RouteUpstream{
				{UpstreamId: 2, Score: 0.8, Uncertainty: 0.05, Keys: []*plannerv1.KeyShare{{KeyId: "b-converted", Share: 1}}},
				{UpstreamId: 3, Score: 0.8, Uncertainty: 0.05, SameFormat: true, Keys: []*plannerv1.KeyShare{{KeyId: "b-same", Share: 1}}},
			}},
			{ProviderId: "same", Score: 0.9, Uncertainty: 0.05, UpstreamThreshold: 0.4, Upstreams: []*routingv1.RouteUpstream{
				{UpstreamId: 4, Score: 0.8, Uncertainty: 0.05, SameFormat: true, Keys: []*plannerv1.KeyShare{{KeyId: "s1", Share: 1}}},
			}},
		},
	}
	for seed := uint64(1); seed <= 50; seed++ {
		order := drawOrder(plan, seeded(seed))
		var keys []string
		for _, e := range order {
			keys = append(keys, e.key.GetKeyId())
		}
		if len(order) != 4 {
			t.Fatalf("seed %d: order %v, want 4 entries", seed, keys)
		}
		for i, e := range order {
			if e.upstream.GetSameFormat() != (i < 2) {
				t.Fatalf("seed %d: order %v, want the two same-format entries first", seed, keys)
			}
		}
		// The converted group has "converted" and "both", drawn at the plan's rate on their own:
		// "both" trails by z = 1.96, w = 0.05, so the shares are 95.2% and 4.8%. Drawn with
		// "same" as well, they would be about 73% and 4%.
		for _, e := range order[2:] {
			want := map[string]float64{"c1": 0.952, "b-converted": 0.048}[e.key.GetKeyId()]
			if !roundsTo(e.providerShare, want, 0.001) || e.upstreamShare != 1 {
				t.Fatalf("seed %d: %s has shares %v and %v in the converted group, want %v and 1", seed, e.key.GetKeyId(), e.providerShare, e.upstreamShare, want)
			}
		}
	}
}

// Candidates below the threshold are not drawn: they follow the drawn ones, by score, with share
// 0, and the floor counts only the drawn ones.
func TestRankPutsTheCandidatesBelowTheThresholdLast(t *testing.T) {
	layer := []candidate{
		{index: 0, score: 0.3, uncertainty: 0.01},
		{index: 1, score: 0.8, uncertainty: 0.05},
		{index: 2, score: 0.4, uncertainty: 0.01},
		{index: 3, score: 0.78, uncertainty: 0.05},
	}
	rank(layer, 0.5, 0, 30, seeded(3))
	order := orderOf(layer)
	if order[2] != 2 || order[3] != 0 {
		t.Fatalf("order %v, want the two below the threshold last, the higher score first", order)
	}
	shares := sharesOf(layer)
	// λ = 0: the floor is 1/n with the two drawn candidates.
	if shares[1] != 0.5 || shares[3] != 0.5 || shares[0] != 0 || shares[2] != 0 {
		t.Fatalf("shares %v, want 1/2 for the two drawn and 0 for the others", shares)
	}

	// The entries of a provider, or of an upstream, below its threshold are marked.
	plan := designPlan()
	plan.Providers[1].Score = 0.45
	plan.Providers[0].Upstreams = append(plan.Providers[0].Upstreams, &routingv1.RouteUpstream{
		UpstreamId: 32, Score: 0.3, Uncertainty: 0.03, SameFormat: true, Keys: []*plannerv1.KeyShare{{KeyId: "low", Share: 1}},
	})
	var got []string
	for _, e := range drawOrder(plan, seeded(5)) {
		got = append(got, fmt.Sprintf("%s:%v", e.key.GetKeyId(), e.belowThreshold))
	}
	if got[2] != "low:true" || got[3] != "anth-k1:true" || got[0] == "low:true" || got[1] == "low:true" {
		t.Fatalf("order %v, want nimbus-cloud's two keys, its upstream below the threshold, then anthropic, both marked", got)
	}
}

// When every candidate is below the threshold, all are ranked by score without a draw and the
// first takes every request.
func TestRankRanksByScoreWhenNoneQualifies(t *testing.T) {
	layer := []candidate{
		{index: 0, score: 0.2},
		{index: 1, score: 0.4},
		{index: 2, score: 0.3},
		{index: 3, score: 0.4},
	}
	rank(layer, 0.5, 200, 30, noDraw(t))
	if order := orderOf(layer); !reflect.DeepEqual(order, []int{1, 3, 2, 0}) {
		t.Fatalf("order %v, want by score, ties in plan order", order)
	}
	if shares := sharesOf(layer); !reflect.DeepEqual(shares, []float64{0, 1, 0, 0}) {
		t.Fatalf("shares %v, want 1 for the first", shares)
	}
	for _, c := range layer {
		if c.drawn {
			t.Fatalf("candidate %d is drawn", c.index)
		}
	}
}

// A plan shaped as txe's builtin/cost-health@1 writes it (tokun/internal/planner/costhealth.go):
// every score is the candidate's place counted from the end, uncertainty 0, both thresholds n + 1,
// above every score, and every key share 1. Both layers rank by score without a draw, so the floor
// never applies; only the keys of an upstream are drawn, at equal shares.
func TestDrawOrderRanksTheBuiltinPlannersPlanByScore(t *testing.T) {
	// Five candidates in the builtin's order: provider-x's upstream 2 (score 5), provider-y's 7
	// (4), provider-x's 3 (3), provider-z's 9 (2), provider-y's 8 (1).
	keys := func(ids ...string) []*plannerv1.KeyShare {
		var list []*plannerv1.KeyShare
		for _, id := range ids {
			list = append(list, &plannerv1.KeyShare{KeyId: id, Share: 1})
		}
		return list
	}
	const threshold = 6
	plan := &routingv1.RoutePlan{
		RequestRate: 200, StaleAfterSeconds: 30, Threshold: threshold,
		Providers: []*routingv1.RouteProvider{
			{ProviderId: "provider-x", Score: 5, UpstreamThreshold: threshold, Upstreams: []*routingv1.RouteUpstream{
				{UpstreamId: 2, Score: 5, SameFormat: true, Keys: keys("x2-a", "x2-b")},
				{UpstreamId: 3, Score: 3, SameFormat: true, Keys: keys("x3-a")},
			}},
			{ProviderId: "provider-y", Score: 4, UpstreamThreshold: threshold, Upstreams: []*routingv1.RouteUpstream{
				{UpstreamId: 7, Score: 4, SameFormat: true, Keys: keys("y7-a")},
				{UpstreamId: 8, Score: 1, SameFormat: true, Keys: keys("y8-a")},
			}},
			{ProviderId: "provider-z", Score: 2, UpstreamThreshold: threshold, Upstreams: []*routingv1.RouteUpstream{
				{UpstreamId: 9, Score: 2, SameFormat: true, Keys: keys("z9-a")},
			}},
		},
	}
	wantUpstreams := []int64{2, 2, 3, 7, 8, 9}
	wantShares := map[int64][2]float64{2: {1, 1}, 3: {1, 0}, 7: {0, 1}, 8: {0, 0}, 9: {0, 1}}
	firstKeys := map[string]bool{}
	for seed := uint64(1); seed <= 20; seed++ {
		order := drawOrder(plan, seeded(seed))
		for i, e := range order {
			id := e.upstream.GetUpstreamId()
			if id != wantUpstreams[i] {
				t.Fatalf("seed %d: entry %d is upstream %d, want the upstreams in %v", seed, i, id, wantUpstreams)
			}
			if shares := wantShares[id]; e.providerShare != shares[0] || e.upstreamShare != shares[1] || !e.belowThreshold {
				t.Fatalf("seed %d: upstream %d has shares %v and %v, below threshold %v; want %v, below", seed, id, e.providerShare, e.upstreamShare, e.belowThreshold, shares)
			}
		}
		if order[0].keyShare != 0.5 || order[1].keyShare != 0.5 {
			t.Fatalf("seed %d: upstream 2's keys have shares %v and %v, want 1/2 each", seed, order[0].keyShare, order[1].keyShare)
		}
		firstKeys[order[0].key.GetKeyId()] = true
	}
	if len(firstKeys) != 2 {
		t.Fatalf("20 seeds drew %v as the primary key, want both of upstream 2's keys", firstKeys)
	}
}

// When the candidates' capacities together cannot take the rate, the shares are their
// capacities' proportions.
func TestClipToCapacityWhenNoCapacitySuffices(t *testing.T) {
	layer := []candidate{
		{index: 0, score: 0.8, uncertainty: 0.02, capacity: 30},
		{index: 1, score: 0.7, uncertainty: 0.02, capacity: 10},
	}
	rank(layer, 0.5, 200, 30, seeded(1))
	if shares := sharesOf(layer); shares[0] != 0.75 || shares[1] != 0.25 {
		t.Fatalf("shares %v, want 3/4 and 1/4, the capacities' proportions", shares)
	}

	// One candidate without a limit takes what the others cannot.
	layer = []candidate{
		{index: 0, score: 0.8, uncertainty: 0.02, capacity: 30},
		{index: 1, score: 0.7, uncertainty: 0.02},
	}
	rank(layer, 0.5, 200, 30, seeded(1))
	if shares := sharesOf(layer); math.Abs(shares[0]-0.15) > 1e-12 || math.Abs(shares[1]-0.85) > 1e-12 {
		t.Fatalf("shares %v, want 0.15 and 0.85", shares)
	}
}

// A cut can put another candidate over its capacity; the cuts repeat until none is.
func TestClipToCapacityRepeatsTheCut(t *testing.T) {
	layer := []candidate{
		{index: 0, share: 0.6, capacity: 40, drawn: true},
		{index: 1, share: 0.3, capacity: 35, drawn: true},
		{index: 2, share: 0.1, drawn: true},
	}
	clipToCapacity(layer, 100)
	want := []float64{0.4, 0.35, 0.25}
	for i, share := range sharesOf(layer) {
		if math.Abs(share-want[i]) > 1e-12 {
			t.Fatalf("shares %v, want %v", sharesOf(layer), want)
		}
	}
}

// The upstream layer is drawn at the rate the provider took: nimbus-cloud takes 150 of 200
// requests a second, and its upstreams split those 150 within their capacities.
func TestDrawOrderDrawsUpstreamsAtTheProvidersRate(t *testing.T) {
	plan := designPlan()
	nimbus := plan.Providers[0]
	nimbus.Upstreams[0].Score, nimbus.Upstreams[0].Uncertainty, nimbus.Upstreams[0].Capacity = 0.9, 0.01, 100
	nimbus.Upstreams = append(nimbus.Upstreams, &routingv1.RouteUpstream{
		UpstreamId: 32, Score: 0.5, Uncertainty: 0.01, Capacity: 50, SameFormat: true, Keys: []*plannerv1.KeyShare{{KeyId: "or-us-3-k1", Share: 1}},
	})
	shares := map[int64]float64{}
	for _, e := range drawOrder(plan, seeded(1)) {
		if e.provider == nimbus {
			if math.Abs(e.providerShare-0.75) > 1e-12 {
				t.Fatalf("nimbus-cloud's share %v, want 0.75 of its capacity 150", e.providerShare)
			}
			shares[e.upstream.GetUpstreamId()] = e.upstreamShare
		}
	}
	// At 150 requests a second, upstream 31 would take nearly all and is cut to 100 / 150.
	if math.Abs(shares[31]-2.0/3) > 1e-12 || math.Abs(shares[32]-1.0/3) > 1e-12 {
		t.Fatalf("upstream shares %v, want 2/3 and 1/3", shares)
	}
}

// A layer of one candidate gives it every request, also over its capacity.
func TestRankOneCandidate(t *testing.T) {
	for _, capacity := range []float64{0, 50, 500} {
		layer := []candidate{{index: 0, score: 0.6, uncertainty: 0.1, capacity: capacity}}
		rank(layer, 0.5, 200, 30, seeded(1))
		if layer[0].share != 1 || !layer[0].drawn {
			t.Fatalf("capacity %v: share %v, drawn %v; want 1, drawn", capacity, layer[0].share, layer[0].drawn)
		}
	}
	keys := keyCandidates(0.4)
	rankKeys(keys, seeded(1))
	if keys[0].share != 1 {
		t.Fatalf("one key's share %v, want 1", keys[0].share)
	}
}

// Keys of share 0 follow the drawn keys in the plan's order; with no key of a share above 0, the
// plan's order stands and the first takes every request.
func TestRankKeysPutsTheKeysOfShare0Last(t *testing.T) {
	for seed := uint64(1); seed <= 20; seed++ {
		keys := keyCandidates(0, 2, 0, 6)
		rankKeys(keys, seeded(seed))
		if order := orderOf(keys); order[2] != 0 || order[3] != 2 {
			t.Fatalf("seed %d: order %v, want keys 0 and 2 last, in the plan's order", seed, order)
		}
		if shares := sharesOf(keys); !reflect.DeepEqual(shares, []float64{0, 0.25, 0, 0.75}) {
			t.Fatalf("seed %d: shares %v, want each key's part of the total", seed, shares)
		}
	}
	keys := keyCandidates(0, 0, 0)
	rankKeys(keys, noDraw(t))
	if order := orderOf(keys); !reflect.DeepEqual(order, []int{0, 1, 2}) {
		t.Fatalf("order %v, want the plan's order", order)
	}
	if shares := sharesOf(keys); !reflect.DeepEqual(shares, []float64{1, 0, 0}) {
		t.Fatalf("shares %v, want 1 for the first", shares)
	}
}

// A provider's capacity is that of its upstreams in the group that take its requests.
func TestProviderCapacity(t *testing.T) {
	upstream := func(score, capacity float64, sameFormat bool) *routingv1.RouteUpstream {
		return &routingv1.RouteUpstream{Score: score, Capacity: capacity, SameFormat: sameFormat}
	}
	for _, tc := range []struct {
		name       string
		upstreams  []*routingv1.RouteUpstream
		sameFormat bool
		capacity   float64
		inGroup    bool
	}{
		{"the qualifying upstreams", []*routingv1.RouteUpstream{upstream(0.8, 100, true), upstream(0.6, 50, true), upstream(0.3, 1000, true)}, true, 150, true},
		{"a qualifying upstream without a limit", []*routingv1.RouteUpstream{upstream(0.8, 100, true), upstream(0.6, 0, true)}, true, 0, true},
		{"none qualifies: the top-scored", []*routingv1.RouteUpstream{upstream(0.2, 100, true), upstream(0.3, 40, true)}, true, 40, true},
		{"only the group's upstreams", []*routingv1.RouteUpstream{upstream(0.8, 100, true), upstream(0.8, 0, false)}, true, 100, true},
		{"no upstream in the group", []*routingv1.RouteUpstream{upstream(0.8, 100, true)}, false, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &routingv1.RouteProvider{UpstreamThreshold: 0.5, Upstreams: tc.upstreams}
			capacity, inGroup := providerCapacity(provider, tc.sameFormat)
			if capacity != tc.capacity || inGroup != tc.inGroup {
				t.Fatalf("capacity %v, in group %v; want %v, %v", capacity, inGroup, tc.capacity, tc.inGroup)
			}
		})
	}
}

// The same seed draws the same order; other seeds draw others.
func TestDrawOrderRepeatsForASeed(t *testing.T) {
	plan := chiPlan()
	keysOf := func(order []entry) string {
		var keys string
		for _, e := range order {
			keys += fmt.Sprintf("%d/%s ", e.upstream.GetUpstreamId(), e.key.GetKeyId())
		}
		return keys
	}
	orders := map[string]bool{}
	for seed := uint64(1); seed <= 20; seed++ {
		order := keysOf(drawOrder(plan, seeded(seed)))
		if again := keysOf(drawOrder(plan, seeded(seed))); again != order {
			t.Fatalf("seed %d drew %q, then %q", seed, order, again)
		}
		orders[order] = true
	}
	if len(orders) < 5 {
		t.Fatalf("20 seeds drew %d orders, want them to differ", len(orders))
	}
}

// In production every request draws from the global source, so the same plan is drawn anew each
// time.
func TestPluginDrawsEachRequestAnew(t *testing.T) {
	plugin := testPlugin(t, map[string]string{"gw-2026-11": "gateway-secret"})
	plan := testPlan()
	plan.RequestRate = 0 // λ = 0: the floor makes the two providers' shares equal
	primaries := map[string]bool{}
	for range 200 {
		state := &requestState{plan: plan}
		ctx := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
		plugin.route(ctx, &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{}}, state)
		primaries[state.order[0].key.GetKeyId()] = true
	}
	if len(primaries) != 3 {
		t.Fatalf("200 requests drew %v as primaries, want every key", primaries)
	}
}
