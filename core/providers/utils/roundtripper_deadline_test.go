package utils

import (
	"bufio"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

// TestContextTransport_HonorsDoDeadline verifies a per-request DoDeadline /
// DoTimeout bounds the exchange even when the client's ReadTimeout is longer.
// Credential exchanges (githubcopilot.doRequest) rely on this to keep a stalled
// upstream from holding the token cache's per-credential lock past its deadline.
func TestContextTransport_HonorsDoDeadline(t *testing.T) {
	srv := newScriptedServer(t, func(conn net.Conn, br *bufio.Reader) {
		if !readRequest(br) {
			return
		}
		// Never answer: hold the connection until the client hangs up.
		_, _ = br.ReadByte()
	})
	client := srv.client(10 * time.Second)

	for name, do := range map[string]func(req *fasthttp.Request, resp *fasthttp.Response) error{
		"DoDeadline": func(req *fasthttp.Request, resp *fasthttp.Response) error {
			return client.DoDeadline(req, resp, time.Now().Add(200*time.Millisecond))
		},
		"DoTimeout": func(req *fasthttp.Request, resp *fasthttp.Response) error {
			return client.DoTimeout(req, resp, 200*time.Millisecond)
		},
	} {
		t.Run(name, func(t *testing.T) {
			req := fasthttp.AcquireRequest()
			resp := fasthttp.AcquireResponse()
			req.SetRequestURI("http://scripted/exchange")
			req.Header.SetMethod(fasthttp.MethodPost)

			done := make(chan error, 1)
			start := time.Now()
			go func() { done <- do(req, resp) }()
			select {
			case err := <-done:
				if !errors.Is(err, fasthttp.ErrTimeout) {
					t.Fatalf("err = %v, want fasthttp.ErrTimeout", err)
				}
				if elapsed := time.Since(start); elapsed > 2*time.Second {
					t.Fatalf("returned after %v, want close to the 200ms deadline", elapsed)
				}
				fasthttp.ReleaseRequest(req)
				fasthttp.ReleaseResponse(resp)
			case <-time.After(3 * time.Second):
				t.Fatal("per-request deadline ignored: request still blocked 3s past a 200ms deadline, bounded only by the client's 10s ReadTimeout")
			}
		})
	}
}
