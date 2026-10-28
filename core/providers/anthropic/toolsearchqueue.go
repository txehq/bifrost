package anthropic

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
)

// Hosted tool searches an OpenAI upstream emits through the Anthropic streaming
// surface arrive as a tool_search_call, rendered as server_tool_use, and later a
// tool_search_output, rendered as the tool_search_tool_result that must reference
// the call's id. The calls still waiting for their output are queued here, in a
// manager keyed by a per-stream handle, and not on the BifrostContext: the queue
// grows with the stream (AGENTS.md: no stream-sized data in the context). The
// stream's conversion state carries only the handle.
//
// A queue is created on a stream's first hosted call, so a stream without one
// allocates nothing. It is released when the stream ends (response.completed,
// response.incomplete, error, response.failed), when its context is cancelled, or
// by its own timer once it has been idle for hostedToolSearchQueueTTL, so an
// abandoned stream's queue is freed without any later traffic. Release is
// idempotent. A queue holds at most maxPendingHostedToolSearches calls whose ids
// are at most maxHostedToolSearchIDBytes each, copied in, so it retains under
// 300 KB in the worst case and nothing of the upstream's buffers.
var (
	hostedToolSearchQueues    sync.Map // map[uint64]*hostedToolSearchQueue
	hostedToolSearchQueueNext atomic.Uint64
)

const (
	// maxPendingHostedToolSearches bounds the calls one stream may have waiting
	// for their outputs. An upstream that exceeds it ends the stream with an error
	// rather than losing a pairing.
	maxPendingHostedToolSearches = 256
	// maxHostedToolSearchIDBytes bounds each queued id; a longer one ends the stream
	// with an error rather than being shortened, which could pair the wrong result.
	maxHostedToolSearchIDBytes = 512
	// hostedToolSearchQueueTTL is how long a queue may sit idle before its timer
	// releases it, for a stream that neither ended nor was cancelled.
	hostedToolSearchQueueTTL = 30 * time.Minute
)

// hostedToolSearchQueue's fields are guarded by mu. Every lifecycle step (push,
// pop, the idle check and release) takes it, so the idle check sees the latest
// touch, a queue being used is never expired, and a released queue is never
// written to or re-armed.
type hostedToolSearchQueue struct {
	mu      sync.Mutex
	pending []hostedToolSearchCall // oldest first
	touched int64                  // unix nanoseconds of the last push or pop
	closed  bool                   // released; the stream must open a new queue
	timer   *time.Timer            // idle expiry; set once at creation
	stop    func() bool            // detaches the cancellation cleanup
}

// hostedToolSearchPushResult says why a call was not queued.
type hostedToolSearchPushResult int

const (
	hostedToolSearchQueued hostedToolSearchPushResult = iota
	hostedToolSearchQueueFull
	hostedToolSearchIDTooLong
	hostedToolSearchCancelled
)

// pushHostedToolSearch queues call for the stream that state belongs to. Nothing
// is queued when the stream already has maxPendingHostedToolSearches calls waiting
// or an id is longer than maxHostedToolSearchIDBytes.
func pushHostedToolSearch(ctx *schemas.BifrostContext, state *anthropicToResponsesStreamState, call hostedToolSearchCall) hostedToolSearchPushResult {
	if (call.toolUseID != nil && len(*call.toolUseID) > maxHostedToolSearchIDBytes) ||
		(call.callID != nil && len(*call.callID) > maxHostedToolSearchIDBytes) {
		return hostedToolSearchIDTooLong
	}
	for {
		q := state.hostedToolSearchQueue()
		if q == nil {
			if ctx != nil && ctx.Err() != nil {
				// A cancelled request's queue would be released as soon as it opened.
				return hostedToolSearchCancelled
			}
			q = newHostedToolSearchQueue(ctx, state)
		}
		q.mu.Lock()
		if q.closed {
			// Released between the lookup and the lock (cancellation or idle expiry):
			// drop the handle and open a new queue rather than write to a dead one.
			q.mu.Unlock()
			state.hostedToolSearches = 0
			continue
		}
		if len(q.pending) >= maxPendingHostedToolSearches {
			q.mu.Unlock()
			return hostedToolSearchQueueFull
		}
		q.pending = append(q.pending, hostedToolSearchCall{toolUseID: cloneStringPtr(call.toolUseID), callID: cloneStringPtr(call.callID)})
		q.touched = time.Now().UnixNano()
		q.mu.Unlock()
		return hostedToolSearchQueued
	}
}

// popHostedToolSearch removes and returns the call an output pairs with: the one
// whose call_id the output names, else the oldest. ok is false when nothing waits,
// including after the queue was released.
func popHostedToolSearch(state *anthropicToResponsesStreamState, callID *string) (toolUseID *string, ok bool) {
	q := state.hostedToolSearchQueue()
	if q == nil {
		return nil, false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || len(q.pending) == 0 {
		return nil, false
	}
	pick := 0
	if callID != nil {
		for i, pending := range q.pending {
			if pending.callID != nil && *pending.callID == *callID {
				pick = i
				break
			}
		}
	}
	toolUseID = q.pending[pick].toolUseID
	q.pending = append(q.pending[:pick], q.pending[pick+1:]...)
	q.touched = time.Now().UnixNano()
	return toolUseID, true
}

// releaseHostedToolSearches drops the stream's queue, if it has one.
func releaseHostedToolSearches(state *anthropicToResponsesStreamState) {
	if state == nil || state.hostedToolSearches == 0 {
		return
	}
	releaseHostedToolSearchQueue(state.hostedToolSearches)
	state.hostedToolSearches = 0
}

func (state *anthropicToResponsesStreamState) hostedToolSearchQueue() *hostedToolSearchQueue {
	if state.hostedToolSearches == 0 {
		return nil
	}
	if v, ok := hostedToolSearchQueues.Load(state.hostedToolSearches); ok {
		return v.(*hostedToolSearchQueue)
	}
	// Released (cancellation or idle expiry) while the stream was still writing:
	// start a fresh queue rather than reuse the stale handle.
	state.hostedToolSearches = 0
	return nil
}

func newHostedToolSearchQueue(ctx *schemas.BifrostContext, state *anthropicToResponsesStreamState) *hostedToolSearchQueue {
	id := hostedToolSearchQueueNext.Add(1)
	q := &hostedToolSearchQueue{touched: time.Now().UnixNano()}
	q.mu.Lock() // the timer and cancellation callbacks may fire before the fields are set
	hostedToolSearchQueues.Store(id, q)
	q.timer = time.AfterFunc(hostedToolSearchQueueTTL, func() { expireHostedToolSearchQueue(id, time.Now()) })
	if ctx != nil {
		q.stop = context.AfterFunc(ctx, func() { releaseHostedToolSearchQueue(id) })
	}
	q.mu.Unlock()
	state.hostedToolSearches = id
	return q
}

// expireHostedToolSearchQueue releases the queue if it has been idle for
// hostedToolSearchQueueTTL at now, and otherwise re-arms its timer for the rest.
// The decision is made under the queue's lock, against the latest touch.
func expireHostedToolSearchQueue(id uint64, now time.Time) {
	v, ok := hostedToolSearchQueues.Load(id)
	if !ok {
		return
	}
	q := v.(*hostedToolSearchQueue)
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	idle := now.Sub(time.Unix(0, q.touched))
	if idle < hostedToolSearchQueueTTL {
		q.timer.Reset(hostedToolSearchQueueTTL - idle)
		return
	}
	q.closeLocked(id)
}

func releaseHostedToolSearchQueue(id uint64) {
	v, ok := hostedToolSearchQueues.Load(id)
	if !ok {
		return
	}
	q := v.(*hostedToolSearchQueue)
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closeLocked(id)
}

// closeLocked releases q, registered under id. q.mu must be held. Idempotent.
func (q *hostedToolSearchQueue) closeLocked(id uint64) {
	if q.closed {
		return
	}
	q.closed = true
	hostedToolSearchQueues.CompareAndDelete(id, q)
	if q.timer != nil {
		q.timer.Stop()
	}
	if q.stop != nil {
		q.stop()
	}
	q.pending = nil
}

func cloneStringPtr(s *string) *string {
	if s == nil {
		return nil
	}
	c := strings.Clone(*s)
	return &c
}

// isTerminalResponsesStreamEvent reports whether the event ends the stream.
func isTerminalResponsesStreamEvent(t schemas.ResponsesStreamResponseType) bool {
	switch t {
	case schemas.ResponsesStreamResponseTypeCompleted,
		schemas.ResponsesStreamResponseTypeIncomplete,
		schemas.ResponsesStreamResponseTypeError,
		schemas.ResponsesStreamResponseTypeFailed:
		return true
	}
	return false
}
