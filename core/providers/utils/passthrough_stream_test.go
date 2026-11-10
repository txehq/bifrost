package utils

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

const responsesStreamBody = "event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
	"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":3}}}\n\n"

func endsResponsesStream(event []byte) bool {
	return strings.Contains(string(event), `"type":"response.completed"`)
}

// hangupBody makes a read that the cancellation close interrupts fail the way a live connection
// does. fasthttp's body reports io.EOF there, which the stream loop would take as a clean end.
type hangupBody struct {
	io.Reader
	closed atomic.Bool
}

func (b *hangupBody) CloseWithError(err error) error {
	b.closed.Store(true)
	closeBodyStream(b.Reader, err)
	return nil
}

func (b *hangupBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF && b.closed.Load() {
		err = net.ErrClosed
	}
	return n, err
}

// runParkedPassthroughStream serves responsesStreamBody as a chunked SSE response and then keeps
// the connection open, like an upstream that parks after response.completed. With hangUp set the
// request is cancelled as soon as the client reads response.completed, the way Codex hangs up. It
// returns every post-hook call and whether the stream channel closed within the deadline.
func runParkedPassthroughStream(t *testing.T, endsStream func([]byte) bool, hangUp bool) (finals []*schemas.BifrostPassthroughResponse, errs []*schemas.BifrostError, closed bool) {
	t.Helper()
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	srv := newScriptedServer(t, func(conn net.Conn, br *bufio.Reader) {
		if !readRequest(br) {
			return
		}
		writeAll(t, conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
		writeAll(t, conn, fmt.Sprintf("%x\r\n%s\r\n", len(responsesStreamBody), responsesStreamBody))
		<-hold
	})

	goCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := schemas.NewBifrostContext(goCtx, time.Time{})
	req, resp := streamGet(t, BuildStreamingClient(srv.client(5*time.Second)), "POST")
	defer fasthttp.ReleaseRequest(req)

	runner := func(_ *schemas.BifrostContext, r *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		if err != nil {
			errs = append(errs, err)
		} else if r != nil && r.PassthroughResponse != nil && r.PassthroughResponse.Body == nil {
			finals = append(finals, r.PassthroughResponse)
		}
		return r, err
	}

	ch := StreamPassthrough(ctx, runner, nil, resp, &hangupBody{Reader: resp.BodyStream()}, PassthroughStreamParams{
		StatusCode: 200,
		EndsStream: endsStream,
		Observe: func(event []byte) *schemas.BifrostPassthroughUsage {
			if strings.Contains(string(event), `"usage"`) {
				return &schemas.BifrostPassthroughUsage{}
			}
			return nil
		},
	})

	done := make(chan struct{})
	go func() {
		for chunk := range ch {
			if hangUp && chunk.BifrostPassthroughResponse != nil && strings.Contains(string(chunk.BifrostPassthroughResponse.Body), "response.completed") {
				cancel()
			}
		}
		close(done)
	}()
	select {
	case <-done:
		return finals, errs, true
	case <-time.After(2 * time.Second):
		return finals, errs, false
	}
}

func TestStreamPassthrough_EndsStreamAbandonsParkedUpstream(t *testing.T) {
	// No client hangup: release must not drain a body the upstream never closes.
	finals, errs, closed := runParkedPassthroughStream(t, endsResponsesStream, false)
	if !closed {
		t.Fatal("stream channel did not close: release is draining the parked upstream body")
	}
	if len(errs) != 0 {
		t.Fatalf("expected no error, got %q", errs[0].Error.Message)
	}
	if len(finals) != 1 || finals[0].PassthroughUsage == nil {
		t.Fatalf("expected one final chunk with usage from response.completed, got %d", len(finals))
	}
}

func TestStreamPassthrough_EndsStreamFinalizesBeforeClientHangup(t *testing.T) {
	finals, errs, closed := runParkedPassthroughStream(t, endsResponsesStream, true)
	if !closed {
		t.Fatal("stream channel did not close")
	}
	if len(errs) != 0 {
		t.Fatalf("expected no cancellation error, got %q", errs[0].Error.Message)
	}
	if len(finals) != 1 || finals[0].PassthroughUsage == nil {
		t.Fatalf("expected one final chunk with usage from response.completed, got %d", len(finals))
	}
}

// A passthrough stream interrupted after the vendor reported usage bills that usage: the
// observed usage is published as the accumulated-usage handle, which the cancellation
// handler copies into the error's BilledUsage, as the typed streams do (TXE-6548; the
// Responses bridge that native Messages replaced did this). The forwarded bytes are
// unchanged, and a stream that reported no usage registers nothing.
func TestStreamPassthrough_InterruptedStreamBillsObservedUsage(t *testing.T) {
	const started = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":20,\"cache_read_input_tokens\":4736,\"output_tokens\":1}}}\n\n"
	for _, tc := range []struct {
		name      string
		withUsage bool
	}{{name: "usage_reported", withUsage: true}, {name: "no_usage"}} {
		t.Run(tc.name, func(t *testing.T) {
			hold := make(chan struct{})
			t.Cleanup(func() { close(hold) })
			srv := newScriptedServer(t, func(conn net.Conn, br *bufio.Reader) {
				if !readRequest(br) {
					return
				}
				writeAll(t, conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
				writeAll(t, conn, fmt.Sprintf("%x\r\n%s\r\n", len(started), started))
				<-hold
			})

			goCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := schemas.NewBifrostContext(goCtx, time.Time{})
			req, resp := streamGet(t, BuildStreamingClient(srv.client(5*time.Second)), "POST")
			defer fasthttp.ReleaseRequest(req)

			var errs []*schemas.BifrostError
			runner := func(_ *schemas.BifrostContext, r *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
				if err != nil {
					errs = append(errs, err)
				}
				return r, err
			}
			ch := StreamPassthrough(ctx, runner, nil, resp, &hangupBody{Reader: resp.BodyStream()}, PassthroughStreamParams{
				StatusCode: 200,
				Observe: func(event []byte) *schemas.BifrostPassthroughUsage {
					if !tc.withUsage || !strings.Contains(string(event), `"usage"`) {
						return nil
					}
					return &schemas.BifrostPassthroughUsage{LLMUsage: &schemas.BifrostLLMUsage{
						PromptTokens: 4756, CompletionTokens: 1, TotalTokens: 4757,
						PromptTokensDetails: &schemas.ChatPromptTokensDetails{CachedReadTokens: 4736},
					}}
				},
			})

			var relayed strings.Builder
			done := make(chan struct{})
			go func() {
				for chunk := range ch {
					if chunk.BifrostPassthroughResponse != nil {
						relayed.Write(chunk.BifrostPassthroughResponse.Body)
						if strings.Contains(relayed.String(), "message_start") {
							cancel() // the client hangs up mid-stream
						}
					}
				}
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("stream channel did not close after the cancel")
			}
			if relayed.String() != started {
				t.Fatalf("relayed bytes changed:\n got: %q\nwant: %q", relayed.String(), started)
			}
			if len(errs) != 1 {
				t.Fatalf("post-hook errors = %d, want 1 cancellation error", len(errs))
			}
			billed := errs[0].ExtraFields.BilledUsage
			if !tc.withUsage {
				if billed != nil {
					t.Fatalf("BilledUsage = %+v, want nil when no usage was reported", billed)
				}
				return
			}
			if billed == nil || billed.PromptTokens != 4756 || billed.CompletionTokens != 1 ||
				billed.PromptTokensDetails == nil || billed.PromptTokensDetails.CachedReadTokens != 4736 {
				t.Fatalf("BilledUsage = %+v, want the usage observed before the cancel", billed)
			}
		})
	}
}

// BenchmarkStreamPassthrough measures the shared passthrough stream loop over an
// in-memory Anthropic SSE body (no sockets), with and without token usage reported.
// with_usage registers the accumulated-usage handle on the first usage event and copies
// each later usage into it (TXE-6548); no_usage must cost what it did before. Observe
// returns prebuilt usage so only the loop's own allocations are counted.
func BenchmarkStreamPassthrough(b *testing.B) {
	var sse strings.Builder
	sse.WriteString("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":20,\"output_tokens\":1}}}\n\n")
	for range 64 {
		sse.WriteString("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello world \"}}\n\n")
	}
	sse.WriteString("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":65}}\n\n")
	sse.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	body := sse.String()
	usage := &schemas.BifrostPassthroughUsage{LLMUsage: &schemas.BifrostLLMUsage{PromptTokens: 20, CompletionTokens: 65, TotalTokens: 85}}
	runner := func(_ *schemas.BifrostContext, r *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		return r, err
	}
	for _, bc := range []struct {
		name      string
		withUsage bool
	}{{"with_usage", true}, {"no_usage", false}} {
		b.Run(bc.name, func(b *testing.B) {
			params := PassthroughStreamParams{
				StatusCode: 200,
				HasUsage:   func(event []byte) bool { return strings.Contains(string(event), `"usage"`) },
				Observe: func([]byte) *schemas.BifrostPassthroughUsage {
					if bc.withUsage {
						return usage
					}
					return nil
				},
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
				for range StreamPassthroughHTTP(ctx, runner, nil, io.NopCloser(strings.NewReader(body)), params) {
				}
			}
		})
	}
}
