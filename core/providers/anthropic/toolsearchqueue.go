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

type hostedToolSearchQueue struct {
	mu      sync.Mutex
	pending []hostedToolSearchCall // oldest first
	touched atomic.Int64           // unix nanoseconds of the last push or pop
	timer   *time.Timer            // idle expiry; set once at creation
	stop    func() bool            // detaches the cancellation cleanup
}

// hostedToolSearchPushResult says why a call was not queued.
type hostedToolSearchPushResult int

const (
	hostedToolSearchQueued hostedToolSearchPushResult = iota
	hostedToolSearchQueueFull
	hostedToolSearchIDTooLong
)

// pushHostedToolSearch queues call for the stream that state belongs to. Nothing
// is queued when the stream already has maxPendingHostedToolSearches calls waiting
// or an id is longer than maxHostedToolSearchIDBytes.
func pushHostedToolSearch(ctx *schemas.BifrostContext, state *anthropicToResponsesStreamState, call hostedToolSearchCall) hostedToolSearchPushResult {
	if (call.toolUseID != nil && len(*call.toolUseID) > maxHostedToolSearchIDBytes) ||
		(call.callID != nil && len(*call.callID) > maxHostedToolSearchIDBytes) {
		return hostedToolSearchIDTooLong
	}
	q := state.hostedToolSearchQueue()
	if q == nil {
		q = newHostedToolSearchQueue(ctx, state)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) >= maxPendingHostedToolSearches {
		return hostedToolSearchQueueFull
	}
	q.pending = append(q.pending, hostedToolSearchCall{toolUseID: cloneStringPtr(call.toolUseID), callID: cloneStringPtr(call.callID)})
	q.touched.Store(time.Now().UnixNano())
	return hostedToolSearchQueued
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
	if len(q.pending) == 0 {
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
	q.touched.Store(time.Now().UnixNano())
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
	q := &hostedToolSearchQueue{}
	q.touched.Store(time.Now().UnixNano())
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
func expireHostedToolSearchQueue(id uint64, now time.Time) {
	v, ok := hostedToolSearchQueues.Load(id)
	if !ok {
		return
	}
	q := v.(*hostedToolSearchQueue)
	idle := now.Sub(time.Unix(0, q.touched.Load()))
	if idle >= hostedToolSearchQueueTTL {
		releaseHostedToolSearchQueue(id)
		return
	}
	q.mu.Lock()
	q.timer.Reset(hostedToolSearchQueueTTL - idle)
	q.mu.Unlock()
}

func releaseHostedToolSearchQueue(id uint64) {
	v, ok := hostedToolSearchQueues.LoadAndDelete(id)
	if !ok {
		return
	}
	q := v.(*hostedToolSearchQueue)
	q.mu.Lock()
	defer q.mu.Unlock()
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
