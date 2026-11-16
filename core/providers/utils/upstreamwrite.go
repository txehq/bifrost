package utils

import (
	"context"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"

	"github.com/capsohq/bifrost/core/schemas"
)

// How the vendor call learns whether its request was written (contract C6 upstream_sent,
// TXE-6746). Core puts a schemas.UpstreamAttempt on the call's context. Every fasthttp
// exchange of a request bound to that context (MakeRequestWithContext, DoStreamingRequest,
// the fasthttp RoundTripper behind NewProviderHTTPClient) passes through contextTransport,
// which records it: a failed connection acquire (a dial, DNS or TLS failure, a refused
// egress target, no free connection) wrote nothing; reaching the write may have written.
// DoHTTPRequest on a net/http transport records from its client trace.
// TestProvidersSendThroughTheSharedClient fails on a provider that bypasses all of these.

// withoutUpstreamAttempt returns ctx with the call's upstream attempt hidden, for an
// exchange that is not the vendor call (a resource fetch).
func withoutUpstreamAttempt(ctx context.Context) context.Context {
	if schemas.UpstreamAttemptFrom(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, schemas.BifrostContextKeyUpstreamAttempt, (*schemas.UpstreamAttempt)(nil))
}

// traceUpstreamWrite returns req with a client trace that notes whether net/http obtained a
// connection (TLS included), and a function that records the exchange once client.Do returned:
// an error before any connection wrote nothing; anything after one may have written. A client
// on the fasthttp RoundTripper records through contextTransport instead, which sees each
// exchange; net/http emits no trace for it.
func traceUpstreamWrite(client *http.Client, req *http.Request) (*http.Request, func(error)) {
	attempt := schemas.UpstreamAttemptFrom(req.Context())
	if attempt == nil || client == nil {
		return req, func(error) {}
	}
	if _, ok := client.Transport.(*fasthttpRoundTripper); ok {
		return req, func(error) {}
	}
	var gotConn atomic.Bool
	trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { gotConn.Store(true) }}
	traced := req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	return traced, func(err error) {
		if err != nil && !gotConn.Load() {
			attempt.MarkConnectFailed()
			return
		}
		attempt.MarkWritten()
	}
}
