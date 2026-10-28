package anthropic

import (
	"context"
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
// response.incomplete, error, response.failed) and, if the stream is abandoned
// without a terminal event, when its context is cancelled. Queues untouched for
// hostedToolSearchQueueTTL are swept whenever a new queue is created.
var (
	hostedToolSearchQueues    sync.Map // map[uint64]*hostedToolSearchQueue
	hostedToolSearchQueueNext atomic.Uint64
)

const (
	// maxPendingHostedToolSearches bounds the calls one stream may have waiting
	// for their outputs. An upstream that exceeds it ends the stream with an error
	// rather than losing a pairing.
	maxPendingHostedToolSearches = 256
	// hostedToolSearchQueueTTL is the fallback lifetime of a queue whose stream
	// neither ended nor was cancelled.
	hostedToolSearchQueueTTL = 30 * time.Minute
)

type hostedToolSearchQueue struct {
	mu      sync.Mutex
	pending []hostedToolSearchCall // oldest first
	touched atomic.Int64           // unix nanoseconds of the last push or pop
	stop    func() bool            // detaches the cancellation cleanup
}

// pushHostedToolSearch queues call for the stream that state belongs to. It
// reports false when the stream already has maxPendingHostedToolSearches calls
// waiting; nothing is queued then.
func pushHostedToolSearch(ctx *schemas.BifrostContext, state *anthropicToResponsesStreamState, call hostedToolSearchCall) bool {
	q := state.hostedToolSearchQueue()
	if q == nil {
		q = newHostedToolSearchQueue(ctx, state)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) >= maxPendingHostedToolSearches {
		return false
	}
	q.pending = append(q.pending, call)
	q.touched.Store(time.Now().UnixNano())
	return true
}

// popHostedToolSearch removes and returns the call an output pairs with: the one
// whose call_id the output names, else the oldest. ok is false when nothing waits.
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
	// Released (cancellation or the TTL sweep) while the stream was still writing:
	// start a fresh queue rather than reuse the stale handle.
	state.hostedToolSearches = 0
	return nil
}

func newHostedToolSearchQueue(ctx *schemas.BifrostContext, state *anthropicToResponsesStreamState) *hostedToolSearchQueue {
	sweepHostedToolSearchQueues(time.Now())
	id := hostedToolSearchQueueNext.Add(1)
	q := &hostedToolSearchQueue{}
	q.touched.Store(time.Now().UnixNano())
	hostedToolSearchQueues.Store(id, q)
	if ctx != nil {
		q.stop = context.AfterFunc(ctx, func() { releaseHostedToolSearchQueue(id) })
	}
	state.hostedToolSearches = id
	return q
}

func releaseHostedToolSearchQueue(id uint64) {
	v, ok := hostedToolSearchQueues.LoadAndDelete(id)
	if !ok {
		return
	}
	if q := v.(*hostedToolSearchQueue); q.stop != nil {
		q.stop()
	}
}

// sweepHostedToolSearchQueues releases queues untouched for hostedToolSearchQueueTTL.
func sweepHostedToolSearchQueues(now time.Time) {
	cutoff := now.Add(-hostedToolSearchQueueTTL).UnixNano()
	hostedToolSearchQueues.Range(func(key, value any) bool {
		if value.(*hostedToolSearchQueue).touched.Load() < cutoff {
			releaseHostedToolSearchQueue(key.(uint64))
		}
		return true
	})
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
