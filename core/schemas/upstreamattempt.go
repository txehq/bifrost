package schemas

import (
	"context"
	"sync"
	"sync/atomic"
)

// UpstreamAttempt records, for one provider call and its retries, whether the shared HTTP
// client wrote the request to the vendor (contract C6 upstream_sent, TXE-6746). Core puts one
// on the context when a call starts (BifrostContextKeyUpstreamAttempt) and holds it until the
// call returns, so the call's fallbacks and MCP agent turns add to it and the next call on the
// same context starts afresh; every exchange
// through providerUtils' shared client marks it; core applies it to the error the call returns.
type UpstreamAttempt struct {
	written       atomic.Bool
	connectFailed atomic.Bool
	holders       atomic.Int32
}

// Hold marks the record in use by a running call until the returned release is called. A call
// that starts while the record is held (an MCP agent turn inside the call that started it) adds
// to it; one that starts after every holder released gets a record of its own.
func (a *UpstreamAttempt) Hold() (release func()) {
	a.holders.Add(1)
	var once sync.Once
	return func() { once.Do(func() { a.holders.Add(-1) }) }
}

// Held reports whether a running call holds the record.
func (a *UpstreamAttempt) Held() bool {
	return a != nil && a.holders.Load() > 0
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
