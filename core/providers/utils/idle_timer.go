package utils

import (
	"sync"
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
	timeout time.Duration
	err     error // what every read reports once the timer has fired
	// claim, when set, runs first when the timer fires. It returns false when
	// another owner already closed the stream; the timer then closes nothing and
	// does not count as fired.
	claim func() bool
	// closeStream unblocks a read that is waiting on the stalled stream.
	closeStream func()
	timer       idleClockTimer

	// mu orders a fire's claim and its fired mark against readers of fired, so a
	// read that sees the stream claimed as closed also sees why it was closed.
	mu      sync.Mutex
	fired   bool
	stopped bool
	closing sync.WaitGroup // a fire's closeStream still running
}

// newIdleTimer creates an idle timer of timeout that reports err after it fires.
// It starts armed when armed is true; otherwise Arm starts it.
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
	if t.fired || t.stopped {
		t.mu.Unlock()
		return
	}
	if t.claim != nil && !t.claim() {
		// Another owner closed the stream first; its error stands.
		t.stopped = true
		t.mu.Unlock()
		return
	}
	t.fired = true
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
	if t.fired || t.stopped {
		return
	}
	t.timer.Reset(t.timeout)
}

// Disarm pauses the timer until the next Arm.
func (t *idleTimer) Disarm() {
	t.timer.Stop()
}

// Fired reports whether the timer fired and closed the stream.
func (t *idleTimer) Fired() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.fired
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
