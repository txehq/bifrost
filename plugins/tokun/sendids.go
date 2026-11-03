package tokun

import (
	"container/heap"
	"sync"
	"time"
)

// sendIDLedger remembers every send_id this pod has accepted until the send expires, so a
// signed plan is used at most once per pod. Memory: one map entry and one heap entry per
// accepted send (the send_id and its expiry, about a hundred bytes), freed by the first claim
// after the send's expires_at.
type sendIDLedger struct {
	mu       sync.Mutex
	seen     map[string]time.Time
	byExpiry expiryHeap
}

func newSendIDLedger() *sendIDLedger {
	return &sendIDLedger{seen: make(map[string]time.Time)}
}

// claim records sendID until expiresAt and reports true, or reports false when the pod has
// already accepted sendID and that send has not expired at now.
func (l *sendIDLedger) claim(sendID string, expiresAt, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for len(l.byExpiry) > 0 && !l.byExpiry[0].expiresAt.After(now) {
		expired := heap.Pop(&l.byExpiry).(expiry)
		delete(l.seen, expired.sendID)
	}
	if _, seen := l.seen[sendID]; seen {
		return false
	}
	l.seen[sendID] = expiresAt
	heap.Push(&l.byExpiry, expiry{sendID: sendID, expiresAt: expiresAt})
	return true
}

// size is the number of sends the ledger holds.
func (l *sendIDLedger) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.seen)
}

type expiry struct {
	sendID    string
	expiresAt time.Time
}

// expiryHeap orders the remembered sends by expiry, earliest first.
type expiryHeap []expiry

func (h expiryHeap) Len() int           { return len(h) }
func (h expiryHeap) Less(i, j int) bool { return h[i].expiresAt.Before(h[j].expiresAt) }
func (h expiryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *expiryHeap) Push(x any)        { *h = append(*h, x.(expiry)) }
func (h *expiryHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}
