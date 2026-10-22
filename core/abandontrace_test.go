package bifrost

import (
	"context"
	"sync"
	"testing"
	"time"

	schemas "github.com/capsohq/bifrost/core/schemas"
)

// abandonOrderTracer records the order of trace-deferral registration and trace
// completion. Its DeferTraceCompletion stalls, standing in for a caller that is
// descheduled right after publishing abandonment.
type abandonOrderTracer struct {
	schemas.NoOpTracer
	mu      sync.Mutex
	events  []string
	flushed chan struct{}
}

func (r *abandonOrderTracer) record(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *abandonOrderTracer) DeferTraceCompletion(string) {
	select {
	case <-r.flushed:
	case <-time.After(200 * time.Millisecond):
	}
	r.record("defer")
}

func (r *abandonOrderTracer) CompleteAndFlushTrace(string) {
	r.record("flush")
	close(r.flushed)
}

// TestAbandonDeliveryRegistersTraceDeferralBeforeWorkerCompletes verifies the
// worker cannot complete an abandoned trace before the caller has registered
// its deferral. Otherwise the marker lands after the flush: the transport then
// skips its own completion and the marker is never cleared.
func TestAbandonDeliveryRegistersTraceDeferralBeforeWorkerCompletes(t *testing.T) {
	tracer := &abandonOrderTracer{flushed: make(chan struct{})}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyTracer, schemas.Tracer(tracer))
	ctx.SetValue(schemas.BifrostContextKeyTraceID, "trace-1")
	msg := &ChannelMessage{Context: ctx}
	client := &Bifrost{}

	abandoned := make(chan bool, 1)
	go func() { abandoned <- msg.abandonDelivery() }()
	deadline := time.Now().Add(5 * time.Second)
	for msg.handoff.Load() == handoffOpen {
		if time.Now().After(deadline) {
			t.Fatal("caller never published abandonment")
		}
		time.Sleep(50 * time.Microsecond)
	}
	// The worker side, as in sendWorkerError/billAbandonedTerminal.
	if msg.claimDelivery() {
		t.Fatal("worker claimed a message the caller abandoned")
	}
	client.completeAbandonedTrace(ctx)
	if !<-abandoned {
		t.Fatal("abandonDelivery reported failure after its CAS landed")
	}

	tracer.mu.Lock()
	defer tracer.mu.Unlock()
	if len(tracer.events) != 2 || tracer.events[0] != "defer" || tracer.events[1] != "flush" {
		t.Fatalf("trace deferral must be registered before the worker completes the trace, got %v", tracer.events)
	}
}
