package utils

import (
	"sync"
	"sync/atomic"
	"time"
)

// idleClockTimer is the part of *time.Timer an idleTimer drives. Tests replace it
// with a timer they fire by hand, so a test never races a real timeout.
type idleClockTimer interface {
	Reset(d time.Duration) bool
	Stop() bool
}

// idleAfterFunc starts an idleClockTimer that calls f after d, like time.AfterFunc.
type idleAfterFunc func(d time.Duration, f func()) idleClockTimer

// systemAfterFunc is the production idleAfterFunc.
func systemAfterFunc(d time.Duration, f func()) idleClockTimer {
	return time.AfterFunc(d, f)
}

// idleTimer is the one idle-timeout state machine behind every timer-wrapped
// stream reader in this package: idleTimeoutReader (every provider stream and
// passthrough), mediaStreamBody (media fetches) and resultBodyReader (batch
// result downloads). Each reader decides when the timer runs; the timer decides
// what a read reports once it has fired.
//
// The invariant: once the timer has fired, Settle never lets a read end cleanly.
// Closing the body to unblock a stalled read can make that read return io.EOF,
// a short read, or nothing at all, depending on the transport and the OS. A
// reader that passed io.EOF through reported a stalled upstream as a complete,
// successful response (TXE-7264). Settle turns every result after the fire into
// the timer's error instead.
type idleTimer struct {
	timeout     time.Duration
	err         error       // what every read reports once the timer has fired
	claim       func() bool // optional; see newIdleTimer
	closeStream func()      // unblocks a read waiting on the stalled stream
	timer       idleClockTimer
	mu          sync.Mutex     // orders a fire's claim and fired mark; see FiredOnceClaimed
	fired       atomic.Bool    // written under mu, read lock-free on every read
	stopped     bool           // guarded by mu
	closing     sync.WaitGroup // a fire's closeStream still running
}

// newIdleTimer creates an idle timer of timeout that reports err after it fires.
// It starts armed when armed is true; otherwise Arm starts it. When the timer
// fires it runs claim first, when one is given: claim returns false when another
// owner (a cancellation or a release) already closed the stream, and the timer
// then closes nothing and does not count as fired, so that owner's error stands.
// Otherwise the timer marks itself fired and calls closeStream to unblock a read
// that is waiting on the stalled stream.
func newIdleTimer(timeout time.Duration, err error, afterFunc idleAfterFunc, armed bool, claim func() bool, closeStream func()) *idleTimer {
	if timeout <= 0 {
		timeout = DefaultStreamIdleTimeout
	}
	if afterFunc == nil {
		afterFunc = systemAfterFunc
	}
	t := &idleTimer{timeout: timeout, err: err, claim: claim, closeStream: closeStream}
	t.timer = afterFunc(timeout, t.fire)
	if !armed {
		t.timer.Stop()
	}
	return t
}

func (t *idleTimer) fire() {
	t.mu.Lock()
	if t.fired.Load() || t.stopped {
		t.mu.Unlock()
		return
	}
	if t.claim != nil && !t.claim() {
		// Another owner closed the stream first; its error stands.
		t.stopped = true
		t.mu.Unlock()
		return
	}
	t.fired.Store(true)
	t.closing.Add(1)
	t.mu.Unlock()
	defer t.closing.Done()
	// Outside mu: a close can block on the transport, and a read that it
	// unblocks must be able to ask Fired without waiting for it.
	t.closeStream()
}

// Arm restarts the full timeout. It does nothing once the timer has fired or
// been stopped, so a read that raced the fire cannot re-arm it.
func (t *idleTimer) Arm() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.fired.Load() || t.stopped {
		return
	}
	t.timer.Reset(t.timeout)
}

// Disarm pauses the timer until the next Arm.
func (t *idleTimer) Disarm() {
	t.timer.Stop()
}

// Fired reports whether the timer fired and closed the stream. It takes no lock:
// a read that a fire's close unblocked always sees the mark, because the mark is
// set before the close starts.
func (t *idleTimer) Fired() bool {
	return t.fired.Load()
}

// FiredOnceClaimed is Fired for a reader that learned the stream was claimed as
// closed some other way (the BifrostContextKeyConnectionClosed flag a fire's claim
// sets): it waits out a fire between its claim and its mark, so that reader
// reports the idle timeout rather than a plain close.
func (t *idleTimer) FiredOnceClaimed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.fired.Load()
}

// Settle is the result a read reports, given what the wrapped read returned.
// Before the fire it is the read's own result. After it, the bytes are kept and
// the error is always the timer's: never nil, never io.EOF.
func (t *idleTimer) Settle(n int, err error) (int, error) {
	if t.Fired() {
		return n, t.err
	}
	return n, err
}

// Stop ends the timer for good and waits for a fire that is still closing the
// stream, so the caller can release the stream after it returns. A fire that
// has not claimed the stream by then never will. Safe to call more than once.
func (t *idleTimer) Stop() {
	t.timer.Stop()
	t.mu.Lock()
	t.stopped = true
	t.mu.Unlock()
	t.closing.Wait()
}
