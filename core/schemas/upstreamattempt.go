package schemas

import (
	"context"
	"sync/atomic"
)

// UpstreamAttempt records, for one provider call and its retries, whether the shared HTTP
// client wrote the request to the vendor (contract C6 upstream_sent, TXE-6746). Core puts one
// on the context the first time the call reaches a provider (BifrostContextKeyUpstreamAttempt),
// and its fallbacks and MCP agent turns, which run on the same context, add to it; every exchange
// through providerUtils' shared client marks it; core applies it to the error the call returns.
type UpstreamAttempt struct {
	written       atomic.Bool
	connectFailed atomic.Bool
}

// MarkWritten records an exchange that wrote, or may have written, the request: it was answered,
// or it failed during or after the write.
func (a *UpstreamAttempt) MarkWritten() {
	if a != nil {
		a.written.Store(true)
	}
}

// MarkConnectFailed records an exchange that failed before any byte of the request was written:
// a dial, DNS or TLS failure, no free connection, or a refused egress target.
func (a *UpstreamAttempt) MarkConnectFailed() {
	if a != nil {
		a.connectFailed.Store(true)
	}
}

// Apply writes the record onto the error the call returns. Once any exchange wrote, upstream_sent
// is true, overriding a refusal's false (C6 sums every attempt). When an exchange failed before
// writing and none wrote, it is false. With nothing recorded, the error keeps what it declared:
// a refusal before dispatch says false itself (TXE-7651), and anything else stays untold, which
// C6 counts as sent, so a provider that left the shared client unrecorded never reads as unsent.
func (a *UpstreamAttempt) Apply(err *BifrostError) {
	if a == nil || err == nil {
		return
	}
	switch {
	case a.written.Load():
		err.ExtraFields.UpstreamSent = Ptr(true)
	case a.connectFailed.Load():
		err.ExtraFields.UpstreamSent = Ptr(false)
	}
}

// UpstreamAttemptFrom returns the record on ctx, or nil when there is none.
func UpstreamAttemptFrom(ctx context.Context) *UpstreamAttempt {
	if ctx == nil {
		return nil
	}
	a, _ := ctx.Value(BifrostContextKeyUpstreamAttempt).(*UpstreamAttempt)
	return a
}
