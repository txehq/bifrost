package governance

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/modelcatalog"
)

// A dollar budget refuses a price book row for a virtual key once an answer on it priced at $0
// (TXE-7957, unpricedbudget.go). That refusal is recorded when the answer returns, so while the
// first answer on a row is still in flight, every concurrent request on the same row was admitted
// and each could take another $0 answer (TXE-7977).
//
// Until one answer on a row has priced above $0 for a (budget, virtual key) scope, that scope runs
// at most one request on the row at a time: a concurrent one is refused with 429
// model_pricing_unconfirmed and a one-second retry hint. Once an answer on the row priced above
// $0, the row is confirmed for the scope and the limit lifts. A request with no dollar budget is
// never probed. The hold is per principal (unpricedHolder). The state is per replica, like the
// refusal it guards.
//
// Live and Realtime units are not probed: a session holds its unit open for its whole length, so
// one session would refuse every concurrent session of the key until its first unit closed.

// DecisionModelPricingUnconfirmed refuses a request under a dollar budget while another request of
// the same virtual key on the same price book row is still in flight and no answer on that row has
// yet priced above $0 for it (TXE-7977).
const DecisionModelPricingUnconfirmed Decision = "model_pricing_unconfirmed"

// unpricedProbeRetryAfterMs is the retry hint a probe refusal carries: the first answer usually
// returns within it.
const unpricedProbeRetryAfterMs = 1000

const (
	// maxConfirmedRowsPerScope bounds the rows a (budget, virtual key) scope remembers as
	// confirmed. A scope that fills stops probing new rows: the after-the-fact refusal still
	// applies to them, and only the concurrency window reopens.
	maxConfirmedRowsPerScope = 4096
	// unpricedProbeStaleAfter frees an in-flight probe whose request never reached its post-hook,
	// so a lost release cannot hold a row for good.
	unpricedProbeStaleAfter = 10 * time.Minute
)

const unpricedProbeContextKey schemas.BifrostContextKey = "bf-governance-unpriced-probe"

type unpricedProbes struct {
	mu        sync.Mutex
	confirmed map[string]*confirmedRows // scope -> rows confirmed priced
	inflight  map[string]inflightHold   // scope + "\x00" + row -> the probe holding it
	now       func() time.Time
	// afterAcquire runs once a request has taken its hold, outside the lock; tests use it to land
	// another request's settlement in the window before the gate reads the refusal record again.
	afterAcquire func()
}

// inflightHold is one probe's hold on a (scope, row): which probe holds it, and since when. A hold
// past unpricedProbeStaleAfter may be replaced, and its probe's later release must not free the
// replacement's.
type inflightHold struct {
	owner *unpricedProbe
	since time.Time
}

type confirmedRows struct {
	rows map[string]struct{}
	full bool
}

func newUnpricedProbes() *unpricedProbes {
	return &unpricedProbes{confirmed: make(map[string]*confirmedRows), inflight: make(map[string]inflightHold), now: time.Now}
}

// unpricedProbe is one request's hold on the rows it probes; release frees them, confirm marks
// them priced for the request's scopes.
type unpricedProbe struct {
	probes *unpricedProbes
	keys   []string
	scopes []string
	rows   []string
	once   sync.Once
	// stop cancels the release registered on the request's context.
	stop func() bool

	// What the gate priced the request as, so the row the serving key priced the answer on can be
	// read the same way once a key is selected.
	provider schemas.ModelProvider
	model    string
	mode     schemas.RequestType
}

// acquire holds every (scope, row) not yet confirmed for the request. It reports busy when another
// request already holds one of them; a request whose rows are all confirmed holds nothing.
func (p *unpricedProbes) acquire(scopes, rows []string) (probe *unpricedProbe, busy bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var keys []string
	for _, scope := range scopes {
		confirmed := p.confirmed[scope]
		if confirmed != nil && confirmed.full {
			continue
		}
		for _, row := range rows {
			if confirmed != nil {
				if _, ok := confirmed.rows[row]; ok {
					continue
				}
			}
			key := scope + "\x00" + row
			if hold, held := p.inflight[key]; held && now.Sub(hold.since) < unpricedProbeStaleAfter {
				return nil, true
			}
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return nil, false
	}
	probe = &unpricedProbe{probes: p, keys: keys, scopes: scopes, rows: rows}
	for _, key := range keys {
		p.inflight[key] = inflightHold{owner: probe, since: now}
	}
	return probe, false
}

// release frees the probe's rows that it still holds; a row whose stale hold another probe has
// replaced stays held by that probe. Later calls do nothing.
func (probe *unpricedProbe) release() {
	if probe == nil {
		return
	}
	probe.once.Do(func() {
		if probe.stop != nil {
			probe.stop()
		}
		probe.probes.mu.Lock()
		defer probe.probes.mu.Unlock()
		for _, key := range probe.keys {
			if hold, held := probe.probes.inflight[key]; held && hold.owner == probe {
				delete(probe.probes.inflight, key)
			}
		}
	})
}

// confirm records row as priced for the probe's scopes: an answer on it priced above $0. A row the
// probe did not hold is not confirmed.
func (probe *unpricedProbe) confirm(row string) {
	if probe == nil || row == "" || !slices.Contains(probe.rows, row) {
		return
	}
	p := probe.probes
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, scope := range probe.scopes {
		confirmed := p.confirmed[scope]
		if confirmed == nil {
			confirmed = &confirmedRows{rows: make(map[string]struct{})}
			p.confirmed[scope] = confirmed
		}
		if _, ok := confirmed.rows[row]; ok {
			continue
		}
		if len(confirmed.rows) >= maxConfirmedRowsPerScope {
			confirmed.full = true
			continue
		}
		confirmed.rows[row] = struct{}{}
	}
}

// confirmProbe confirms the row the answer was priced on: read as the gate read it, under the
// scopes of the key that served the answer. A row another key prices, which may still answer $0,
// stays unconfirmed.
func (r *BudgetResolver) confirmProbe(probe *unpricedProbe, servedScopes *modelcatalog.PricingLookupScopes) {
	if r == nil || probe == nil || r.modelCatalog == nil {
		return
	}
	row, _ := r.modelCatalog.PriceIdentity(probe.provider, probe.model, probe.mode, servedScopes)
	probe.confirm(row)
}

// probesUnpriced reports whether a request type is probed: an answer the after-the-fact refusal
// judges, other than a Live or Realtime unit.
func probesUnpriced(requestType schemas.RequestType) bool {
	switch requestType {
	case schemas.LiveRequest, schemas.RealtimeRequest:
		return false
	}
	return budgetPricingOf(requestType) == budgetPricingAnswer
}

// probeUnconfirmed holds the request's rows for its (budget, principal) scopes until its answer
// returns, or refuses it when another request holds one of them. The hold is kept on the request,
// and a later pass over the same rows (a body model, then the envelope) reuses it. It is released
// by the request's last post-hook call, or when the request's context ends, whichever is first: a
// request core ends without post-hooks (a full queue, a cancel while queued) frees it too.
//
// The rows are every row the request could be priced as before its key is chosen (priceIdentities:
// the scope given and each configured key's): each stays held until an answer the serving key
// priced on it confirms it, so confirming one key's row leaves another key's row probed.
//
// The hold is per principal (unpricedHolder): a virtual key, a user, or a presented credential each
// hold their own. Requests that presented nothing share one hold on the budget, which serializes
// them until the first priced answer rather than leaving them unprobed.
func (r *BudgetResolver) probeUnconfirmed(ctx *schemas.BifrostContext, requestType schemas.RequestType, provider schemas.ModelProvider, model string, mode schemas.RequestType, scopes *modelcatalog.PricingLookupScopes, budgetIDs []string) *EvaluationResult {
	if r.probes == nil || ctx == nil || model == "" || !probesUnpriced(requestType) {
		return nil
	}
	_, rows := r.priceIdentities(provider, model, mode, scopes)
	if len(rows) == 0 {
		return nil
	}
	holderScopes := unpricedScopes(budgetIDs, unpricedHolder(ctx))
	if held, ok := ctx.Value(unpricedProbeContextKey).(*unpricedProbe); ok && held != nil {
		if slices.Equal(held.scopes, holderScopes) && slices.Equal(held.rows, rows) {
			return nil
		}
		held.release()
		ctx.ClearValue(unpricedProbeContextKey)
	}
	probe, busy := r.probes.acquire(holderScopes, rows)
	if busy {
		return &EvaluationResult{
			Decision: DecisionModelPricingUnconfirmed,
			Reason:   "another request of this virtual key on this model is in flight, and no answer on it has yet been priced against this request's dollar budget; retry when it completes",
		}
	}
	if probe != nil {
		probe.provider, probe.model, probe.mode = provider, model, mode
		probe.stop = context.AfterFunc(ctx, probe.release)
		ctx.SetValue(unpricedProbeContextKey, probe)
		if r.probes.afterAcquire != nil {
			r.probes.afterAcquire()
		}
	}
	return nil
}

// takeUnpricedProbe removes the request's probe from its context when this post-hook call is the
// request's last (an error, a unary answer, or a stream's final chunk), for the caller to confirm
// and release; nil otherwise.
func takeUnpricedProbe(ctx *schemas.BifrostContext, err *schemas.BifrostError, isStream, isFinalChunk bool) *unpricedProbe {
	if ctx == nil {
		return nil
	}
	probe, ok := ctx.Value(unpricedProbeContextKey).(*unpricedProbe)
	if !ok || probe == nil {
		return nil
	}
	if err == nil && isStream && !isFinalChunk {
		return nil
	}
	ctx.ClearValue(unpricedProbeContextKey)
	// The taker owns the hold from here: the request's context ending (an HTTP handler returning
	// before the usage worker settles) no longer frees it, so it lasts until the answer is recorded.
	if probe.stop != nil {
		probe.stop()
	}
	return probe
}
