package utils

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/internal/testtime"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// syncedPanicBody is a controlled io.ReadCloser used to deterministically
// reproduce the race in SetupStreamCancellation.
//
// Read blocks until Close() is called, then panics — simulating what fasthttp's
// streaming body does when the underlying TCP connection is force-closed.
//
// Close() triggers the panic (by closing panicTrigger) and then blocks on
// allowReturn until the test signals it. This keeps SetupStreamCancellation
// stuck inside Close() so BifrostContextKeyConnectionClosed is guaranteed to
// be unset when idleTimeoutReader.Read's recover block runs — the exact race
// window that exists in the current code.
type syncedPanicBody struct {
	panicTrigger chan struct{}
	allowReturn  chan struct{}
	closeOnce    sync.Once
	closes       atomic.Int32
}

// closeCount reports how many times the stream was closed, so a test can assert
// that nothing closed it rather than only that a read stayed blocked.
func (s *syncedPanicBody) closeCount() int { return int(s.closes.Load()) }

func newSyncedPanicBody() *syncedPanicBody {
	return &syncedPanicBody{
		panicTrigger: make(chan struct{}),
		allowReturn:  make(chan struct{}),
	}
}

func (s *syncedPanicBody) Read(_ []byte) (int, error) {
	<-s.panicTrigger
	panic("use of closed network connection")
}

func (s *syncedPanicBody) Close() error {
	s.closes.Add(1)
	s.closeOnce.Do(func() { close(s.panicTrigger) })
	<-s.allowReturn
	return nil
}

var errPanicReader = errors.New("panic reader called")

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) {
	panic(errPanicReader)
}

// timerPanicCloser mimics fasthttp's streaming body when the underlying
// connection has already been released to / reused from the pool: CloseWithError
// nil-derefs in (*HostClient).CloseConn and panics. It implements
// streamCloserWithError (not io.Closer) so closeBodyStream takes the
// CloseWithError branch — the path the idle timer hits.
//
// called is closed the instant CloseWithError is entered (just before the
// panic), so a test can deterministically wait for the guarded path to be
// exercised rather than rely on a fixed sleep — which a slow runner could
// outrun, letting cleanup stop the timer before it ever fired and passing the
// test without touching the recover.
type timerPanicCloser struct {
	called chan struct{}
}

func newTimerPanicCloser() *timerPanicCloser {
	return &timerPanicCloser{called: make(chan struct{})}
}

func (*timerPanicCloser) Read([]byte) (int, error) { return 0, io.EOF }

func (c *timerPanicCloser) CloseWithError(error) error {
	close(c.called)
	panic("simulated fasthttp CloseConn nil-deref")
}

// captureLogger records Debug messages so a test can assert that a recovered
// panic value is logged (not silently swallowed). It embeds noopLogger to
// satisfy the rest of the schemas.Logger interface.
type captureLogger struct {
	noopLogger
	mu   sync.Mutex
	msgs []string
}

func (c *captureLogger) Debug(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, fmt.Sprintf(format, args...))
}

func (c *captureLogger) contains(sub string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.msgs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

type blockingCloserSpy struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingCloserSpy() *blockingCloserSpy {
	return &blockingCloserSpy{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (c *blockingCloserSpy) Read([]byte) (int, error) { return 0, io.EOF }

func (c *blockingCloserSpy) Close() error {
	c.once.Do(func() { close(c.started) })
	<-c.release
	return nil
}

func (c *blockingCloserSpy) unblock() {
	close(c.release)
}

// readCloserSpy implements io.ReadCloser and records how many times Close() was called.
type readCloserSpy struct {
	mu     sync.Mutex
	closed int
}

func (c *readCloserSpy) Read([]byte) (int, error) { return 0, io.EOF }

func (c *readCloserSpy) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed++
	return nil
}

func (c *readCloserSpy) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// zeroThenBlockReader returns (0, nil) on the first read, then blocks forever.
type zeroThenBlockReader struct {
	first  atomic.Bool
	pipeRd *io.PipeReader
}

func (r *zeroThenBlockReader) Read(p []byte) (int, error) {
	if r.first.CompareAndSwap(false, true) {
		return 0, nil // zero-byte read
	}
	// block until pipe is closed
	return r.pipeRd.Read(p)
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

func TestIdleTimeoutReader_NormalRead(t *testing.T) {
	t.Parallel()
	pr, pw := io.Pipe()
	defer pr.Close()

	// Use pr as bodyStream — closing pr unblocks reads.
	wrapped, cleanup := NewIdleTimeoutReader(pr, pr, testtime.Scale(500*time.Millisecond), nil)
	defer cleanup()

	// Writer sends 5 chunks quickly.
	go func() {
		for i := 0; i < 5; i++ {
			time.Sleep(testtime.Scale(10 * time.Millisecond))
			pw.Write([]byte("chunk"))
		}
		pw.Close()
	}()

	buf := make([]byte, 64)
	var totalBytes int
	for {
		n, err := wrapped.Read(buf)
		totalBytes += n
		if err != nil {
			if err != io.EOF {
				t.Fatalf("unexpected error: %v", err)
			}
			break
		}
	}

	if totalBytes != 5*len("chunk") {
		t.Fatalf("expected %d bytes, got %d", 5*len("chunk"), totalBytes)
	}
}

func TestIdleTimeoutReader_TimeoutClosesStream(t *testing.T) {
	t.Parallel()
	pr, pw := io.Pipe()
	defer pw.Close()

	// 100ms timeout, write nothing — should timeout and close the pipe reader.
	wrapped, cleanup := NewIdleTimeoutReader(pr, pr, testtime.Scale(100*time.Millisecond), nil)
	defer cleanup()

	start := time.Now()
	buf := make([]byte, 64)
	_, err := wrapped.Read(buf)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error from timed-out read, got nil")
	}

	// Should complete within ~200ms (100ms timeout + margin), not hang.
	if elapsed > testtime.Scale(500*time.Millisecond) {
		t.Fatalf("read took %v, expected ~100ms timeout", elapsed)
	}
}

func TestIdleTimeoutReader_TimeoutAfterPartialData(t *testing.T) {
	t.Parallel()
	pr, pw := io.Pipe()

	// 200ms idle timeout.
	wrapped, cleanup := NewIdleTimeoutReader(pr, pr, testtime.Scale(200*time.Millisecond), nil)
	defer cleanup()

	// Writer sends 3 chunks then stops.
	go func() {
		for i := 0; i < 3; i++ {
			time.Sleep(testtime.Scale(20 * time.Millisecond))
			pw.Write([]byte("data"))
		}
		// stop writing — idle timeout should fire after 200ms and close pr
	}()

	buf := make([]byte, 64)
	chunksRead := 0
	for {
		n, err := wrapped.Read(buf)
		if n > 0 {
			chunksRead++
		}
		if err != nil {
			break
		}
	}

	if chunksRead != 3 {
		t.Fatalf("expected 3 chunks before timeout, got %d", chunksRead)
	}

	pw.Close()
}

func TestIdleTimeoutReader_ResetOnData(t *testing.T) {
	t.Parallel()
	pr, pw := io.Pipe()

	// 200ms timeout, but data arrives every 150ms — should never timeout.
	wrapped, cleanup := NewIdleTimeoutReader(pr, pr, testtime.Scale(200*time.Millisecond), nil)
	defer cleanup()

	go func() {
		for i := 0; i < 5; i++ {
			time.Sleep(testtime.Scale(150 * time.Millisecond))
			pw.Write([]byte("ok"))
		}
		pw.Close()
	}()

	buf := make([]byte, 64)
	chunksRead := 0
	for {
		n, err := wrapped.Read(buf)
		if n > 0 {
			chunksRead++
		}
		if err != nil {
			if err != io.EOF {
				t.Fatalf("expected EOF after all chunks, got: %v", err)
			}
			break
		}
	}

	if chunksRead != 5 {
		t.Fatalf("expected 5 chunks (timer should reset), got %d", chunksRead)
	}
}

func TestIdleTimeoutReader_CleanupStopsTimer(t *testing.T) {
	t.Parallel()
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()

	spy := &readCloserSpy{}

	_, cleanup := NewIdleTimeoutReader(pr, spy, testtime.Scale(100*time.Millisecond), nil)
	// Call cleanup immediately — timer should be stopped.
	cleanup()

	// Wait well past the timeout duration.
	time.Sleep(testtime.Scale(250 * time.Millisecond))

	if spy.closeCount() != 0 {
		t.Fatalf("expected closer to NOT be called after cleanup, but was called %d times", spy.closeCount())
	}
}

func TestIdleTimeoutReader_DoubleCloseIsSafe(t *testing.T) {
	t.Parallel()
	spy := &readCloserSpy{}

	br := &zeroThenBlockReader{first: atomic.Bool{}, pipeRd: nil}
	// Use spy as bodyStream — it implements both io.Reader and io.Closer.
	_, cleanup := NewIdleTimeoutReader(br, spy, testtime.Scale(50*time.Millisecond), nil)
	defer cleanup()

	// Let the timer fire (closes spy via sync.Once).
	time.Sleep(testtime.Scale(100 * time.Millisecond))

	// Manually close again — should not panic.
	spy.Close()

	// sync.Once ensures the idle timer's close ran exactly once.
	// The manual close above adds another, so total should be 2
	// (the once.Do protects the timer path, not external callers).
	// The key guarantee: no panic.
	if spy.closeCount() < 1 {
		t.Fatal("expected at least one close call")
	}
}

func TestIdleTimeoutReader_ZeroBytesDoNotResetTimer(t *testing.T) {
	t.Parallel()
	pr, pw := io.Pipe()
	defer pw.Close()

	// Use pr as bodyStream — when idle timeout fires, it closes pr,
	// which causes reads on pr to return io.ErrClosedPipe.
	zr := &zeroThenBlockReader{pipeRd: pr}
	wrapped, cleanup := NewIdleTimeoutReader(zr, pr, testtime.Scale(100*time.Millisecond), nil)
	defer cleanup()

	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 64)
		// First read returns (0, nil), second read blocks until pipe is closed.
		for {
			_, err := wrapped.Read(buf)
			if err != nil {
				done <- err
				return
			}
		}
	}()

	select {
	case <-done:
		// Timer fired and closed the pipe — Read() returned an error. Good.
	case <-time.After(testtime.Scale(500 * time.Millisecond)):
		t.Fatal("expected idle timeout to fire, but read is still blocking")
	}
}

func TestIdleTimeoutReader_ErrorFromClosedPipe(t *testing.T) {
	t.Parallel()
	pr, pw := io.Pipe()
	defer pw.Close()

	// Use pr as bodyStream — when idle timeout fires, it closes pr,
	// which makes Read return io.ErrClosedPipe.
	wrapped, cleanup := NewIdleTimeoutReader(pr, pr, testtime.Scale(50*time.Millisecond), nil)
	defer cleanup()

	buf := make([]byte, 64)
	_, err := wrapped.Read(buf)

	if err == nil {
		t.Fatal("expected error from closed pipe")
	}
	// The error should indicate the pipe was closed.
	if !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, io.EOF) {
		// Some implementations return io.ErrClosedPipe, others EOF.
		t.Logf("got error: %v (acceptable)", err)
	}
}

func TestIdleTimeoutReader_NilContextDoesNotPanic(t *testing.T) {
	t.Parallel()
	wrapped, cleanup := NewIdleTimeoutReader(&readCloserSpy{}, &readCloserSpy{}, testtime.Scale(time.Second), nil)
	defer cleanup()

	_, err := wrapped.Read(make([]byte, 1))
	if err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestIdleTimeoutReader_ClosedContextReturnsError(t *testing.T) {
	t.Parallel()
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	ctx.SetValue(schemas.BifrostContextKeyConnectionClosed, true)
	wrapped, cleanup := NewIdleTimeoutReader(panicReader{}, &readCloserSpy{}, testtime.Scale(time.Second), ctx)
	defer cleanup()

	_, err := wrapped.Read(make([]byte, 1))
	if !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("expected ErrStreamClosed, got %v", err)
	}
}

func TestIdleTimeoutReader_RecoversReadPanicAfterTimeout(t *testing.T) {
	t.Parallel()
	wrapped, cleanup := NewIdleTimeoutReader(panicReader{}, &readCloserSpy{}, testtime.Scale(10*time.Millisecond), nil)
	defer cleanup()

	time.Sleep(testtime.Scale(50 * time.Millisecond))

	_, err := wrapped.Read(make([]byte, 1))
	if !errors.Is(err, ErrStreamIdleTimeout) {
		t.Fatalf("expected ErrStreamIdleTimeout, got %v", err)
	}
}

// TestIdleTimeoutReader_RecoversCloseStreamPanicOnTimerFire verifies that a
// panic raised by closeBodyStream WHEN THE IDLE TIMER FIRES — e.g. fasthttp's
// CloseWithError nil-dereffing in (*HostClient).CloseConn because the stream's
// connection was already released to / reused from the pool (an orphaned timer
// on a completed stream) — is recovered inside the timer goroutine and does not
// crash the process.
//
// This is the timer-callback counterpart to RecoversReadPanicAfterTimeout
// (#3677), which guarded the Read() path but not the AfterFunc's own
// closeBodyStream call. Without the recover in the AfterFunc, the panic runs in
// the timer goroutine, is unrecoverable by callers, and takes the whole process
// down (observed crashing a router under sustained streaming load).
func TestIdleTimeoutReader_RecoversCloseStreamPanicOnTimerFire(t *testing.T) {
	t.Parallel()
	body := newTimerPanicCloser()
	_, cleanup := NewIdleTimeoutReader(&readCloserSpy{}, body, testtime.Scale(10*time.Millisecond), nil)

	// Wait until the timer goroutine has actually entered CloseWithError (about to
	// panic). This proves the guarded path was exercised; a fixed sleep could be
	// outrun by a slow runner, letting cleanup stop the timer before it fired and
	// passing the test without ever touching the recover.
	select {
	case <-body.called:
	case <-time.After(testtime.Scale(2 * time.Second)):
		t.Fatal("idle timer never fired CloseWithError within 2s")
	}

	// cleanup() blocks on timerDone, closed only after the (now-recovered) timer
	// callback returns — so reaching the end proves the panic was recovered in the
	// timer goroutine. Without the recover, the process would already have crashed.
	cleanup()
}

// TestIdleTimeoutReader_LogsRecoveredTimerPanic verifies that the recovered
// panic value is logged (not silently swallowed), so an unexpected future
// panic on this path leaves a forensic trace. Not parallel: it swaps the
// package-global logger and restores it via t.Cleanup before parallel tests
// resume.
func TestIdleTimeoutReader_LogsRecoveredTimerPanic(t *testing.T) {
	capLog := &captureLogger{}
	prev := getLogger()
	SetLogger(capLog)
	t.Cleanup(func() { SetLogger(prev) })

	body := newTimerPanicCloser()
	_, cleanup := NewIdleTimeoutReader(&readCloserSpy{}, body, testtime.Scale(10*time.Millisecond), nil)
	select {
	case <-body.called:
	case <-time.After(testtime.Scale(2 * time.Second)):
		t.Fatal("idle timer never fired CloseWithError within 2s")
	}
	cleanup() // blocks until the timer callback (recover + log) has returned

	if !capLog.contains("idle-timeout timer") || !capLog.contains("nil-deref") {
		t.Fatalf("expected recovered panic to be logged; got %v", capLog.msgs)
	}
}

func TestIdleTimeoutReader_CleanupWaitsForRunningTimerCallback(t *testing.T) {
	t.Parallel()
	body := newBlockingCloserSpy()
	_, cleanup := NewIdleTimeoutReader(&readCloserSpy{}, body, testtime.Scale(10*time.Millisecond), nil)

	select {
	case <-body.started:
	case <-time.After(testtime.Scale(500 * time.Millisecond)):
		t.Fatal("expected idle timeout callback to start closing the body")
	}

	cleanupDone := make(chan struct{})
	go func() {
		cleanup()
		close(cleanupDone)
	}()

	select {
	case <-cleanupDone:
		t.Fatal("cleanup returned before the running timer callback finished")
	case <-time.After(testtime.Scale(50 * time.Millisecond)):
	}

	body.unblock()

	select {
	case <-cleanupDone:
	case <-time.After(testtime.Scale(500 * time.Millisecond)):
		t.Fatal("cleanup did not return after timer callback finished")
	}
}

// TestSetupStreamCancellation_NoPanicOnCancelledContext verifies that
// cancelling a context unblocks a read that is already in flight, and that the
// reader surfaces ErrStreamClosed rather than letting a panic escape.
//
// The body's Close() blocks on allowReturn, so the flag is guaranteed to be
// unset when idleTimeoutReader.Read's recover block runs - the worst-case
// ordering.
//
// Closing under an active reader is only safe on a fasthttp carrying upstream
// commit fb3b29e (#2353), where clientStreamBody waits on its readLock before
// pooling the *requestStream and the connection's *bufio.Reader. See
// TestStreamCloseUnderActiveReaderIsSafe, which fails under -race on any
// fasthttp without that fix, and maximhq/bifrost#6143 for what it cost.
func TestSetupStreamCancellation_NoPanicOnCancelledContext(t *testing.T) {
	t.Parallel()

	body := newSyncedPanicBody()
	goCtx, cancel := context.WithCancel(context.Background())
	bifrostCtx := schemas.NewBifrostContext(goCtx, time.Time{})

	reader, cleanupReader := NewIdleTimeoutReader(body, body, time.Minute, bifrostCtx)
	// Defers run LIFO: allowReturn first, then stopCancel, then cleanupReader.
	// This order lets Close() return before we wait for the goroutine.
	defer cleanupReader()
	stopCancel := SetupStreamCancellation(bifrostCtx, body, getLogger())
	defer stopCancel()
	defer close(body.allowReturn)

	type readResult struct {
		err      error
		panicked any
	}
	resultCh := make(chan readResult, 1)
	go func() {
		var res readResult
		defer func() {
			if r := recover(); r != nil {
				res.panicked = r
			}
			resultCh <- res
		}()
		buf := make([]byte, 1)
		_, res.err = reader.Read(buf)
	}()

	cancel()

	select {
	case res := <-resultCh:
		if res.panicked != nil {
			t.Errorf("Read re-panicked instead of reporting a closed stream: %v", res.panicked)
			return
		}
		if !errors.Is(res.err, ErrStreamClosed) {
			t.Errorf("expected ErrStreamClosed, got: %v", res.err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Read goroutine did not unblock after context cancellation")
	}
}

// TestSetupStreamCancellation_RecoversCloseStreamPanic verifies that a panic
// raised by closeBodyStream inside the cancellation goroutine (fasthttp's
// CloseWithError nil-dereffing in (*HostClient).CloseConn once the connection is
// gone) is recovered and logged rather than taking the process down. This is the
// cancellation-goroutine counterpart to
// TestIdleTimeoutReader_RecoversCloseStreamPanicOnTimerFire. Not parallel: it
// swaps the package-global logger.
func TestSetupStreamCancellation_RecoversCloseStreamPanic(t *testing.T) {
	capLog := &captureLogger{}
	prev := getLogger()
	SetLogger(capLog)
	t.Cleanup(func() { SetLogger(prev) })

	body := newTimerPanicCloser()
	goCtx, cancel := context.WithCancel(context.Background())
	bifrostCtx := schemas.NewBifrostContext(goCtx, time.Time{})

	stopCancel := SetupStreamCancellation(bifrostCtx, body, getLogger())
	cancel()
	select {
	case <-body.called:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation goroutine never called CloseWithError within 2s")
	}
	stopCancel() // blocks until the goroutine (recover + log) has returned

	if !capLog.contains("stream cancellation") || !capLog.contains("nil-deref") {
		t.Fatalf("expected recovered panic to be logged; got %v", capLog.msgs)
	}
}

// TestSetupStreamCancellation_ClosesExactlyOnce guards the ownership claim: the
// cancellation goroutine, the idle-timeout timer and ReleaseStreamingResponse
// all close the same stream, and only one of them may drive it.
func TestSetupStreamCancellation_ClosesExactlyOnce(t *testing.T) {
	t.Parallel()

	body := newSyncedPanicBody()
	close(body.allowReturn) // let Close() return immediately
	goCtx, cancel := context.WithCancel(context.Background())
	bifrostCtx := schemas.NewBifrostContext(goCtx, time.Time{})

	stopCancel := SetupStreamCancellation(bifrostCtx, body, getLogger())
	cancel()
	// BifrostContext propagates its parent's cancellation through a watcher
	// goroutine, so cancel() alone does not guarantee ctx.Err() is set yet.
	// Waiting here keeps the assertion about close ownership rather than about
	// how fast that propagation happens.
	<-bifrostCtx.Done()
	stopCancel()

	if got := body.closeCount(); got != 1 {
		t.Errorf("body stream closed %d times, want exactly 1", got)
	}
}

// countingStreamCloser models fasthttp's non-idempotent streaming body handle:
// it implements streamCloserWithError (CloseWithError) and io.Reader, counting
// closes and reads. The whole point of the atomic-claim fix is that the
// underlying close runs at most once across the cancellation goroutine, the
// idle-timeout timer, and ReleaseStreamingResponse — a second close would
// re-run fasthttp's releaseRequestStream and double-Put the pooled struct.
type countingStreamCloser struct {
	closeCount atomic.Int32
	readCount  atomic.Int32
}

func (c *countingStreamCloser) Read([]byte) (int, error) {
	c.readCount.Add(1)
	return 0, io.EOF
}

func (c *countingStreamCloser) CloseWithError(error) error {
	c.closeCount.Add(1)
	return nil
}

// TestStreamClose_ExactlyOnceAcrossOwners drives the idle-timeout timer and the
// cancellation goroutine concurrently against one shared stream + context and
// asserts the underlying CloseWithError fires exactly once. Before the fix the
// two owners used a racy Value-then-SetValue guard and could both close, which
// double-released the pooled fasthttp requestStream and poisoned another
// request (the `slice bounds out of range [:-680]` crash). Run with -race.
func TestStreamClose_ExactlyOnceAcrossOwners(t *testing.T) {
	t.Parallel()

	for i := 0; i < 100; i++ {
		body := &countingStreamCloser{}
		goCtx, cancel := context.WithCancel(context.Background())
		ctx := schemas.NewBifrostContext(goCtx, time.Time{})

		// Tiny idle timeout so the timer races the cancellation.
		_, cleanup := NewIdleTimeoutReader(body, body, testtime.Scale(time.Millisecond), ctx)
		stop := SetupStreamCancellation(ctx, body, getLogger())

		go cancel() // fire cancellation concurrently with the idle timer

		// Wait (bounded) until at least one owner has performed the close, rather
		// than relying on a fixed sleep that can miss both close paths on slow CI.
		deadline := time.Now().Add(testtime.Scale(2 * time.Second))
		for body.closeCount.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(testtime.Scale(50 * time.Microsecond))
		}
		stop()
		cleanup()

		if got := body.closeCount.Load(); got != 1 {
			t.Fatalf("iteration %d: CloseWithError called %d times, want exactly 1", i, got)
		}
	}
}

// TestConnectionClosedClaim_SerializesOwners verifies the primitive the fix
// relies on: GetAndSetValue on BifrostContextKeyConnectionClosed is an atomic
// compare-and-claim, so exactly one of many concurrent owners observes the
// prior `false` and is permitted to perform the underlying close.
func TestConnectionClosedClaim_SerializesOwners(t *testing.T) {
	t.Parallel()

	const owners = 64
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})

	var winners atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < owners; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if prev, _ := ctx.GetAndSetValue(schemas.BifrostContextKeyConnectionClosed, true).(bool); !prev {
				winners.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := winners.Load(); got != 1 {
		t.Fatalf("expected exactly 1 owner to win the close claim, got %d", got)
	}
}

// TestReleaseStreamingResponse_SkipsWhenAlreadyClaimed asserts that once a
// cancel/timeout owner has claimed the close, ReleaseStreamingResponse neither
// drains nor releases the body stream — preventing fasthttp.ReleaseResponse
// from triggering a second releaseRequestStream on an already-relinquished
// pooled struct.
func TestReleaseStreamingResponse_SkipsWhenAlreadyClaimed(t *testing.T) {
	t.Parallel()

	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	// A prior owner already claimed + closed the stream.
	ctx.GetAndSetValue(schemas.BifrostContextKeyConnectionClosed, true)

	body := &countingStreamCloser{}
	resp := fasthttp.AcquireResponse()
	resp.SetBodyStream(body, -1)

	ReleaseStreamingResponse(ctx, resp)

	if got := body.readCount.Load(); got != 0 {
		t.Fatalf("ReleaseStreamingResponse drained the body despite a prior close claim (read %d times)", got)
	}
	if got := body.closeCount.Load(); got != 0 {
		t.Fatalf("ReleaseStreamingResponse closed an already-claimed stream (%d times)", got)
	}

	// The claim was lost, so resp was intentionally not released (the
	// production GC-leak trade-off). Release it here to keep the test clean.
	fasthttp.ReleaseResponse(resp)
}

// TestReleaseStreamingResponse_NoBodyDoesNotClaimConnection covers the
// pre-response error path: fasthttp.Do can fail before installing a body stream.
// Releasing that empty response must not mark the request context as
// connection-closed, because retries/fallbacks reuse the same BifrostContext.
func TestReleaseStreamingResponse_NoBodyDoesNotClaimConnection(t *testing.T) {
	t.Parallel()

	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	resp := fasthttp.AcquireResponse()

	ReleaseStreamingResponse(ctx, resp)

	if closed, _ := ctx.Value(schemas.BifrostContextKeyConnectionClosed).(bool); closed {
		t.Fatal("ReleaseStreamingResponse claimed connection ownership without a body stream")
	}
}

type closeRecorder struct {
	closed chan struct{}
	once   sync.Once
}

func (c *closeRecorder) Read([]byte) (int, error) { <-c.closed; return 0, io.EOF }
func (c *closeRecorder) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

// A missed first-token deadline closes the attempt's body and claims the
// connection, while the request context stays live for the fallback.
func TestSetupStreamCancellation_FirstTokenDeadlineClosesBody(t *testing.T) {
	bifrostCtx := schemas.NewBifrostContext(context.Background(), time.Time{})
	abort := NewAttemptAbort(50 * time.Millisecond)
	bifrostCtx.SetValue(schemas.BifrostContextKeyStreamAttemptAbort, abort)
	body := &closeRecorder{closed: make(chan struct{})}

	stop := SetupStreamCancellation(bifrostCtx, body, getLogger())
	defer stop()
	select {
	case <-body.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("first-token deadline never closed the body")
	}
	if claimed, _ := bifrostCtx.Value(schemas.BifrostContextKeyConnectionClosed).(bool); !claimed {
		t.Fatal("the abort must claim connection_closed so ReleaseStreamingResponse does not drain")
	}
	if bifrostCtx.Err() != nil {
		t.Fatal("a TTFT miss must not cancel the request context")
	}
}

// After the first token (Disarm) the body stays open, and ctx cancellation
// still closes it.
func TestSetupStreamCancellation_DisarmedDeadlineStillHonoursCtx(t *testing.T) {
	goCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bifrostCtx := schemas.NewBifrostContext(goCtx, time.Time{})
	abort := NewAttemptAbort(testtime.Scale(50 * time.Millisecond))
	bifrostCtx.SetValue(schemas.BifrostContextKeyStreamAttemptAbort, abort)
	body := &closeRecorder{closed: make(chan struct{})}

	stop := SetupStreamCancellation(bifrostCtx, body, getLogger())
	defer stop()
	if !abort.Disarm() {
		t.Fatal("disarm lost to a 50ms deadline")
	}
	select {
	case <-body.closed:
		t.Fatal("a disarmed deadline closed the body")
	case <-time.After(testtime.Scale(150 * time.Millisecond)):
	}
	cancel()
	select {
	case <-body.closed:
	case <-time.After(testtime.Scale(2 * time.Second)):
		t.Fatal("ctx cancellation no longer closes the body after disarm")
	}
}

// ---------------------------------------------------------------------------
// TXE-7264: once the idle timer fires, no read ends cleanly
// ---------------------------------------------------------------------------

// manualIdleClock is an idleAfterFunc whose timer fires only when the test calls
// expire, so a test of what happens after the idle timeout never races one.
type manualIdleClock struct {
	mu     sync.Mutex
	f      func()
	armed  bool
	resets int
}

func (c *manualIdleClock) afterFunc(_ time.Duration, f func()) idleClockTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.f = f
	c.armed = true
	return c
}

func (c *manualIdleClock) Reset(time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	was := c.armed
	c.armed = true
	c.resets++
	return was
}

func (c *manualIdleClock) Stop() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	was := c.armed
	c.armed = false
	return was
}

func (c *manualIdleClock) isArmed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.armed
}

// expire runs the timer's callback as an elapsed timeout would, on the calling
// goroutine. It reports false, and runs nothing, when the timer is not armed.
func (c *manualIdleClock) expire() bool {
	c.mu.Lock()
	if !c.armed {
		c.mu.Unlock()
		return false
	}
	c.armed = false
	f := c.f
	c.mu.Unlock()
	f()
	return true
}

// closeEndsBody is a stream whose Read blocks until Close, then returns tail and
// tailErr: what a transport hands back to a read that the idle timer's close
// unblocked. net/http on Linux runners returned io.EOF there (TXE-7264).
type closeEndsBody struct {
	tail      string
	tailErr   error
	entered   chan struct{} // receives once per Read, as the read starts waiting
	closed    chan struct{}
	closeOnce sync.Once
	closes    atomic.Int32
}

func newCloseEndsBody(tail string, tailErr error) *closeEndsBody {
	return &closeEndsBody{tail: tail, tailErr: tailErr, entered: make(chan struct{}, 8), closed: make(chan struct{})}
}

func (b *closeEndsBody) Read(p []byte) (int, error) {
	b.entered <- struct{}{}
	<-b.closed
	return copy(p, b.tail), b.tailErr
}

func (b *closeEndsBody) Close() error {
	b.closes.Add(1)
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

type readResult struct {
	n   int
	err error
}

// readAfterIdleFire starts one read on r, waits until the read is blocked on body,
// fires the idle timer, and returns what the read reported. The read is already
// past every pre-read check when the timer fires, so the result comes from the
// wrapped read's own return values.
func readAfterIdleFire(t *testing.T, r io.Reader, body *closeEndsBody, clock *manualIdleClock) readResult {
	t.Helper()
	got := make(chan readResult, 1)
	go func() {
		n, err := r.Read(make([]byte, 64))
		got <- readResult{n, err}
	}()
	select {
	case <-body.entered:
	case <-time.After(testtime.Scale(5 * time.Second)):
		t.Fatal("the read never reached the body")
	}
	if !clock.expire() {
		t.Fatal("the idle timer was not armed while the read waited")
	}
	select {
	case res := <-got:
		return res
	case <-time.After(testtime.Scale(5 * time.Second)):
		t.Fatal("the read did not return after the idle timer closed the body")
		return readResult{}
	}
}

// The idle timer closes the body and the blocked read comes back io.EOF, a short
// read, or a short read with io.EOF. Every one must report ErrStreamIdleTimeout:
// io.EOF there turned a stalled upstream into a clean, complete response, and a
// passthrough stream ended without its error chunk (TXE-7264).
func TestIdleTimeoutReader_FiredReadNeverEndsCleanly(t *testing.T) {
	cases := []struct {
		name    string
		tail    string
		tailErr error
		ctx     bool
	}{
		{name: "EOF", tailErr: io.EOF},
		{name: "EOF with a request context", tailErr: io.EOF, ctx: true},
		{name: "short read", tail: "late"},
		{name: "short read with EOF", tail: "late", tailErr: io.EOF},
		{name: "unexpected EOF", tailErr: io.ErrUnexpectedEOF},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ctx *schemas.BifrostContext
			if tc.ctx {
				ctx = schemas.NewBifrostContext(context.Background(), time.Time{})
			}
			clock := &manualIdleClock{}
			body := newCloseEndsBody(tc.tail, tc.tailErr)
			r, cleanup := newIdleTimeoutReader(body, body, time.Minute, ctx, clock.afterFunc)
			defer cleanup()

			res := readAfterIdleFire(t, r, body, clock)
			if !errors.Is(res.err, ErrStreamIdleTimeout) {
				t.Fatalf("read after the idle timer fired returned (%d, %v); want ErrStreamIdleTimeout", res.n, res.err)
			}
			if res.n != len(tc.tail) {
				t.Fatalf("read returned %d bytes; want the %d bytes the body produced", res.n, len(tc.tail))
			}
			if body.closes.Load() != 1 {
				t.Fatalf("the idle timer closed the body %d times; want 1", body.closes.Load())
			}
			// Every later read reports the timeout too, without touching the body.
			if n, err := r.Read(make([]byte, 8)); n != 0 || !errors.Is(err, ErrStreamIdleTimeout) {
				t.Fatalf("next read returned (%d, %v); want (0, ErrStreamIdleTimeout)", n, err)
			}
		})
	}
}

// A read that delivers data restarts the timer; a read that delivers data after
// the fire must not re-arm it, and a stop waits for nothing and closes nothing.
func TestIdleTimeoutReader_ArmAfterFireAndStop(t *testing.T) {
	t.Run("data restarts the timer", func(t *testing.T) {
		clock := &manualIdleClock{}
		r, cleanup := newIdleTimeoutReader(strings.NewReader("data"), io.NopCloser(nil), time.Minute, nil, clock.afterFunc)
		defer cleanup()
		if n, err := r.Read(make([]byte, 8)); n != 4 || err != nil {
			t.Fatalf("read returned (%d, %v)", n, err)
		}
		if clock.resets != 1 {
			t.Fatalf("timer reset %d times; want 1", clock.resets)
		}
		if n, err := r.Read(make([]byte, 8)); n != 0 || err != io.EOF {
			t.Fatalf("a clean end before any fire must stay io.EOF; got (%d, %v)", n, err)
		}
	})
	t.Run("a late chunk does not re-arm a fired timer", func(t *testing.T) {
		clock := &manualIdleClock{}
		body := newCloseEndsBody("late", nil)
		r, cleanup := newIdleTimeoutReader(body, body, time.Minute, nil, clock.afterFunc)
		defer cleanup()
		readAfterIdleFire(t, r, body, clock)
		if clock.isArmed() {
			t.Fatal("a chunk that raced the fire re-armed the idle timer")
		}
	})
	t.Run("cleanup before the fire closes nothing", func(t *testing.T) {
		clock := &manualIdleClock{}
		body := newCloseEndsBody("", io.EOF)
		r, cleanup := newIdleTimeoutReader(body, body, time.Minute, nil, clock.afterFunc)
		cleanup()
		cleanup()
		// A timer callback already dispatched when cleanup ran finds it stopped.
		r.idle.fire()
		if body.closes.Load() != 0 || r.idle.Fired() {
			t.Fatalf("a fire after cleanup closed the body %d times (fired=%v)", body.closes.Load(), r.idle.Fired())
		}
	})
}

// When a cancellation or release owner closed the stream first, the timer's fire
// claims nothing: it closes nothing and reads report ErrStreamClosed, not a timeout.
func TestIdleTimeoutReader_FireLosesToAnEarlierClose(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	clock := &manualIdleClock{}
	body := newCloseEndsBody("", io.EOF)
	r, cleanup := newIdleTimeoutReader(body, body, time.Minute, ctx, clock.afterFunc)
	defer cleanup()
	ctx.SetValue(schemas.BifrostContextKeyConnectionClosed, true)
	if !clock.expire() {
		t.Fatal("timer not armed")
	}
	if body.closes.Load() != 0 {
		t.Fatalf("the timer closed a stream another owner had claimed (%d closes)", body.closes.Load())
	}
	if _, err := r.Read(make([]byte, 8)); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("read returned %v; want ErrStreamClosed", err)
	}
}

// The media fetch body shares the rule: a fired idle timer is never a clean end.
func TestMediaStreamBody_FiredReadNeverEndsCleanly(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tail    string
		tailErr error
	}{
		{name: "EOF", tailErr: io.EOF},
		{name: "short read", tail: "late"},
		{name: "short read with EOF", tail: "late", tailErr: io.EOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &manualIdleClock{}
			body := newCloseEndsBody(tc.tail, tc.tailErr)
			b := newMediaStreamBodyWithTimer(context.Background(), body, time.Minute, "http://example.invalid/x", clock.afterFunc)
			defer b.Close()
			if clock.isArmed() {
				t.Fatal("the media idle timer runs only while a read waits")
			}
			res := readAfterIdleFire(t, b, body, clock)
			if !errors.Is(res.err, ErrStreamIdleTimeout) || res.n != len(tc.tail) {
				t.Fatalf("read returned (%d, %v); want (%d, ErrStreamIdleTimeout)", res.n, res.err, len(tc.tail))
			}
			if n, err := b.Read(make([]byte, 8)); n != 0 || !errors.Is(err, ErrStreamIdleTimeout) {
				t.Fatalf("next read returned (%d, %v); want (0, ErrStreamIdleTimeout)", n, err)
			}
		})
	}
}

// The batch result download shares the rule: an io.EOF from the connection the
// idle timer closed would end a truncated result file as complete.
func TestResultBodyReader_FiredReadNeverEndsCleanly(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tail    string
		tailErr error
	}{
		{name: "EOF", tailErr: io.EOF},
		{name: "short read with EOF", tail: "{\"id\":", tailErr: io.EOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &manualIdleClock{}
			body := newCloseEndsBody(tc.tail, tc.tailErr)
			idle := newIdleTimer(time.Minute, errResultStreamIdle, clock.afterFunc, true, nil, func() { _ = body.Close() })
			defer idle.Stop()
			r := &resultBodyReader{ctx: schemas.NewBifrostContext(context.Background(), time.Time{}), reader: body, idle: idle}
			res := readAfterIdleFire(t, r, body, clock)
			if !errors.Is(res.err, errResultStreamIdle) || res.n != len(tc.tail) {
				t.Fatalf("read returned (%d, %v); want (%d, errResultStreamIdle)", res.n, res.err, len(tc.tail))
			}
			if _, _, err := ReadJSONLPage(r, 10, func([]byte) {}); !errors.Is(err, errResultStreamIdle) {
				t.Fatalf("a page read after the stall returned %v; want errResultStreamIdle", err)
			}
		})
	}
}

// endlessBody answers every read in full without allocating, so the benchmark
// below measures only the idle-timeout wrapper's per-read cost.
type endlessBody struct{}

func (endlessBody) Read(p []byte) (int, error) { return len(p), nil }

// BenchmarkIdleTimeoutReader_Read is the per-read cost every provider stream
// pays on the common path: the timer never fires, and each read that brings data
// restarts it.
func BenchmarkIdleTimeoutReader_Read(b *testing.B) {
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	r, cleanup := NewIdleTimeoutReader(endlessBody{}, endlessBody{}, DefaultStreamIdleTimeout, ctx)
	defer cleanup()
	buf := make([]byte, 4096)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.Read(buf); err != nil {
			b.Fatal(err)
		}
	}
}
