package governance

import (
	"slices"
	"sync"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
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
		probe.probes.mu.Lock()
		defer probe.probes.mu.Unlock()
		for _, key := range probe.keys {
			if hold, held := probe.probes.inflight[key]; held && hold.owner == probe {
				delete(probe.probes.inflight, key)
			}
		}
	})
}

// confirm records the probe's rows as priced for its scopes: an answer on them priced above $0.
func (probe *unpricedProbe) confirm() {
	if probe == nil {
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
		for _, row := range probe.rows {
			if _, ok := confirmed.rows[row]; ok {
				continue
			}
			if len(confirmed.rows) >= maxConfirmedRowsPerScope {
				confirmed.full = true
				break
			}
			confirmed.rows[row] = struct{}{}
		}
	}
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

// probeUnconfirmed holds the request's rows for its (budget, virtual key) scopes until its answer
// returns, or refuses it when another request holds one of them. The hold is kept on the request,
// and a later pass over the same rows (a body model, then the envelope) reuses it.
//
// row is the price book row the request is priced as (priceIdentity in its pricing mode), which
// the gate has already read: concurrent requests on one model resolve to the same row.
//
// The hold is per principal (unpricedHolder): a virtual key, a user, or a presented credential each
// hold their own. Requests that presented nothing share one hold on the budget, which serializes
// them until the first priced answer rather than leaving them unprobed.
func (r *BudgetResolver) probeUnconfirmed(ctx *schemas.BifrostContext, requestType schemas.RequestType, row string, budgetIDs []string) *EvaluationResult {
	if r.probes == nil || ctx == nil || row == "" || !probesUnpriced(requestType) {
		return nil
	}
	holderScopes := unpricedScopes(budgetIDs, unpricedHolder(ctx))
	rows := []string{row}
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
	return probe
}
