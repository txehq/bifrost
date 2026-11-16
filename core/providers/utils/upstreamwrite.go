package utils

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"

	"github.com/capsohq/bifrost/core/network"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// IsConnectStageError reports whether a transport error happened before any byte of the
// request was written (contract C6 attempt stage "connect", TXE-6746): a dial, DNS or TLS
// failure, no free connection within the wait, or an egress target the dial guard refused.
// A pooled connection the pre-write check finds closed never surfaces here: it is replaced
// before the write (pooledConnIsOpen). Every other error may have followed a write, so it
// is not one: fasthttp.ErrTimeout and ErrConnectionClosed included.
func IsConnectStageError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, fasthttp.ErrNoFreeConns) || errors.Is(err, fasthttp.ErrDialTimeout) || errors.Is(err, fasthttp.ErrTLSHandshakeTimeout) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	var dnsErr *net.DNSError
	// Only certificate verification, which happens in the handshake: a TLS alert or a malformed
	// record can also arrive after the write, and a wrong "not sent" is the one error here
	// that costs a customer, while a missed one only leaves the attempt counted as sent.
	var certErr *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var invalidCert x509.CertificateInvalidError
	var privateTarget *network.PrivateTargetNotAllowedError
	var offline *network.OfflineEgressError
	return errors.As(err, &dnsErr) || errors.As(err, &certErr) ||
		errors.As(err, &unknownAuthority) || errors.As(err, &hostnameErr) ||
		errors.As(err, &invalidCert) || errors.As(err, &privateTarget) || errors.As(err, &offline)
}

// RecordUpstreamExchange marks ctx's upstream attempt (schemas.BifrostContextKeyUpstreamAttempt)
// with the outcome of one exchange through the shared client: a connect-stage error wrote
// nothing; nil or any other error wrote, or may have written, the request. Every exchange a
// provider makes goes through MakeRequestWithContext, DoStreamingRequest, DoHTTPRequest or the
// fasthttp RoundTripper, which call it; TestProvidersSendThroughTheSharedClient fails on a
// provider that bypasses them.
func RecordUpstreamExchange(ctx context.Context, err error) {
	attempt := schemas.UpstreamAttemptFrom(ctx)
	if attempt == nil {
		return
	}
	if IsConnectStageError(err) {
		attempt.MarkConnectFailed()
		return
	}
	attempt.MarkWritten()
}

// traceUpstreamWrite returns req with a client trace that notes whether net/http obtained a
// connection (TLS included), and a function that records the exchange once client.Do returned:
// an error before any connection wrote nothing; anything after one may have written.
func traceUpstreamWrite(req *http.Request) (*http.Request, func(error)) {
	if schemas.UpstreamAttemptFrom(req.Context()) == nil {
		return req, func(error) {}
	}
	var gotConn atomic.Bool
	trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { gotConn.Store(true) }}
	traced := req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	return traced, func(err error) {
		attempt := schemas.UpstreamAttemptFrom(traced.Context())
		if err != nil && !gotConn.Load() {
			attempt.MarkConnectFailed()
			return
		}
		attempt.MarkWritten()
	}
}
