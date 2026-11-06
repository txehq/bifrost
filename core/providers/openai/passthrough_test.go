package openai

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
)

// TestBuildPassthroughURLWithUpstreamOverride verifies host-backed passthrough
// routes do not receive OpenAI's /v1 prefix.
func TestBuildPassthroughURLWithUpstreamOverride(t *testing.T) {
	provider := NewOpenAIProvider(&schemas.ProviderConfig{}, passthroughTestLogger{})

	req := &schemas.BifrostPassthroughRequest{
		Path:        "/backend-api/codex/responses",
		RawQuery:    "conversation=abc",
		UpstreamURL: "https://chatgpt.com",
	}

	got, err := provider.buildPassthroughURL(req)
	if err != nil {
		t.Fatalf("buildPassthroughURL unexpected error: %v", err)
	}
	want := "https://chatgpt.com/backend-api/codex/responses?conversation=abc"
	if got != want {
		t.Fatalf("buildPassthroughURL = %q, want %q", got, want)
	}
}

// TestBuildPassthroughURLRejectsForeignAuthority verifies a remainder that would move the
// resolved authority off the configured upstream is refused instead of being dialed. Only
// the upstream-override branch forwards the path verbatim; the default branch always
// prepends /v1, which anchors the path on the OpenAI host.
func TestBuildPassthroughURLRejectsForeignAuthority(t *testing.T) {
	provider := NewOpenAIProvider(&schemas.ProviderConfig{}, passthroughTestLogger{})

	req := &schemas.BifrostPassthroughRequest{Path: "//evil.example/x", UpstreamURL: "https://chatgpt.com"}
	if got, err := provider.buildPassthroughURL(req); err == nil {
		t.Fatalf("buildPassthroughURL(%q) = %q, want error", req.Path, got)
	}
}

// TestBuildPassthroughURLDefaultsToOpenAIV1 verifies normal OpenAI passthrough
// routes keep the existing /v1 URL construction.
func TestBuildPassthroughURLDefaultsToOpenAIV1(t *testing.T) {
	provider := NewOpenAIProvider(&schemas.ProviderConfig{}, passthroughTestLogger{})

	req := &schemas.BifrostPassthroughRequest{
		Path:     "/v1/responses",
		RawQuery: "stream=true",
	}

	got, err := provider.buildPassthroughURL(req)
	if err != nil {
		t.Fatalf("buildPassthroughURL unexpected error: %v", err)
	}
	want := "https://api.openai.com/v1/responses?stream=true"
	if got != want {
		t.Fatalf("buildPassthroughURL = %q, want %q", got, want)
	}
}

// passthroughTestLogger discards provider logs in passthrough URL tests.
type passthroughTestLogger struct{}

// Debug discards a debug log.
func (passthroughTestLogger) Debug(string, ...any) {}

// Info discards an info log.
func (passthroughTestLogger) Info(string, ...any) {}

// Warn discards a warning log.
func (passthroughTestLogger) Warn(string, ...any) {}

// Error discards an error log.
func (passthroughTestLogger) Error(string, ...any) {}

// Fatal discards a fatal log.
func (passthroughTestLogger) Fatal(string, ...any) {}

// SetLevel ignores log level changes.
func (passthroughTestLogger) SetLevel(schemas.LogLevel) {}

// SetOutputType ignores output type changes.
func (passthroughTestLogger) SetOutputType(schemas.LoggerOutputType) {}

// LogHTTPRequest returns a no-op structured log builder.
func (passthroughTestLogger) LogHTTPRequest(schemas.LogLevel, string) schemas.LogEventBuilder {
	return passthroughTestLogEvent{}
}

// passthroughTestLogEvent discards structured HTTP log fields.
type passthroughTestLogEvent struct{}

// Str discards a string field.
func (passthroughTestLogEvent) Str(string, string) schemas.LogEventBuilder {
	return passthroughTestLogEvent{}
}

// Int discards an integer field.
func (passthroughTestLogEvent) Int(string, int) schemas.LogEventBuilder {
	return passthroughTestLogEvent{}
}

// Int64 discards an int64 field.
func (passthroughTestLogEvent) Int64(string, int64) schemas.LogEventBuilder {
	return passthroughTestLogEvent{}
}

// Send discards the built log event.
func (passthroughTestLogEvent) Send() {}

// passthroughStreamRun is what the post-hook chain saw for one OpenAI passthrough stream.
type passthroughStreamRun struct {
	raw    strings.Builder // every forwarded body chunk, in order
	finals []*schemas.BifrostPassthroughResponse
	errs   []*schemas.BifrostError
	// rawAfterFinal counts body chunks that reached the post hooks after the final chunk.
	rawAfterFinal int
	// rawRequestOnBody counts body chunks that carried RawRequest (only the final chunk may).
	rawRequestOnBody int
	closed           bool
	parkedFlag       bool
}

// runOpenAIPassthroughStream serves events from an offline httptest upstream through the real
// OpenAIProvider.PassthroughStream. Each event is flushed as its own write. With park set the
// upstream keeps the response open after the last event, like an upstream that parks after
// response.completed; with failAfter set it aborts the connection instead. cancelOn cancels the
// request as soon as a forwarded chunk contains it, the way a client hangs up.
func runOpenAIPassthroughStream(t *testing.T, path string, events []string, park, failAfter bool, cancelOn string) *passthroughStreamRun {
	t.Helper()
	hold := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, event := range events {
			_, _ = w.Write([]byte(event))
			flusher.Flush()
			time.Sleep(5 * time.Millisecond)
		}
		if failAfter {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
			return
		}
		if park {
			select {
			case <-hold:
			case <-r.Context().Done():
			}
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(hold) })

	provider := NewOpenAIProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, DefaultRequestTimeoutInSeconds: 10, AllowPrivateNetwork: true},
	}, passthroughTestLogger{})

	goCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := schemas.NewBifrostContext(goCtx, time.Time{})

	run := &passthroughStreamRun{}
	var mu sync.Mutex
	runner := func(_ *schemas.BifrostContext, r *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case err != nil:
			run.errs = append(run.errs, err)
		case r != nil && r.PassthroughResponse != nil && r.PassthroughResponse.Body == nil:
			run.finals = append(run.finals, r.PassthroughResponse)
		case r != nil && r.PassthroughResponse != nil:
			run.raw.Write(r.PassthroughResponse.Body)
			if len(run.finals) > 0 {
				run.rawAfterFinal++
			}
			if r.PassthroughResponse.ExtraFields.RawRequest != nil {
				run.rawRequestOnBody++
			}
		}
		return r, err
	}

	body := []byte(`{"model":"gpt-5","stream":true}`)
	ch, bErr := provider.PassthroughStream(ctx, runner, nil, schemas.Key{Value: *schemas.NewSecretVar("sk-test")}, &schemas.BifrostPassthroughRequest{
		Method: http.MethodPost,
		Path:   path,
		Body:   body,
	})
	if bErr != nil {
		t.Fatalf("PassthroughStream: %v", bErr.Error.Message)
	}
	done := make(chan struct{})
	go func() {
		for chunk := range ch {
			if cancelOn != "" && chunk.BifrostPassthroughResponse != nil && strings.Contains(string(chunk.BifrostPassthroughResponse.Body), cancelOn) {
				cancel()
			}
		}
		close(done)
	}()
	select {
	case <-done:
		run.closed = true
	case <-time.After(3 * time.Second):
	}
	run.parkedFlag, _ = ctx.Value(schemas.BifrostContextKeyStreamParkedAfterFinish).(bool)
	mu.Lock()
	defer mu.Unlock()
	return run
}

func sseEvent(data string) string { return "data: " + data + "\n\n" }

// TestOpenAIPassthroughStreamEndsOnResponsesTerminalEvent pins the fork guarantees on the
// upstream end-event rule (upstream 43e55b2a6): each Responses terminal event finishes the
// stream exactly once while the upstream stays open, the usage it carries reaches the final
// chunk, every byte up to it is forwarded in order before the final chunk, and RawRequest
// rides only on the final chunk.
func TestOpenAIPassthroughStreamEndsOnResponsesTerminalEvent(t *testing.T) {
	for _, terminal := range []string{"response.completed", "response.failed", "response.incomplete"} {
		t.Run(terminal, func(t *testing.T) {
			events := []string{
				"event: response.created\n" + sseEvent(`{"type":"response.created","response":{"id":"resp_1"}}`),
				"event: response.output_text.delta\n" + sseEvent(`{"type":"response.output_text.delta","delta":"hi"}`),
				"event: " + terminal + "\n" + sseEvent(`{"type":"`+terminal+`","response":{"id":"resp_1","usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`),
			}
			run := runOpenAIPassthroughStream(t, "/backend-api/codex/responses", events, true, false, "")
			if !run.closed {
				t.Fatal("stream channel did not close: the parked upstream body was drained, not abandoned")
			}
			if len(run.errs) != 0 {
				t.Fatalf("expected no error, got %q", run.errs[0].Error.Message)
			}
			if len(run.finals) != 1 {
				t.Fatalf("expected exactly one final chunk, got %d", len(run.finals))
			}
			if got, want := run.raw.String(), strings.Join(events, ""); got != want {
				t.Fatalf("forwarded bytes differ:\n got %q\nwant %q", got, want)
			}
			if run.rawAfterFinal != 0 || run.rawRequestOnBody != 0 {
				t.Fatalf("ordering broken: %d body chunks after the final, %d body chunks with RawRequest", run.rawAfterFinal, run.rawRequestOnBody)
			}
			final := run.finals[0]
			if final.ExtraFields.RawRequest == nil {
				t.Fatal("final chunk lost RawRequest")
			}
			if final.PassthroughUsage == nil || final.PassthroughUsage.LLMUsage == nil || final.PassthroughUsage.LLMUsage.TotalTokens != 8 {
				t.Fatalf("final chunk lost the terminal usage: %+v", final.PassthroughUsage)
			}
			if !run.parkedFlag {
				t.Fatal("end event did not mark the stream parked, so release would drain the open body")
			}
		})
	}
}

// TestOpenAIPassthroughStreamKeepsChatTerminalUsageChunk pins that the end-event rule never
// fires on Chat Completions: the usage chunk that follows finish_reason (stream_options
// include_usage) still reaches the final chunk, and the stream ends on the upstream's EOF.
func TestOpenAIPassthroughStreamKeepsChatTerminalUsageChunk(t *testing.T) {
	events := []string{
		sseEvent(`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"}}]}`),
		sseEvent(`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		sseEvent(`{"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`),
		sseEvent(`[DONE]`),
	}
	run := runOpenAIPassthroughStream(t, "/v1/chat/completions", events, false, false, "")
	if !run.closed || len(run.errs) != 0 || len(run.finals) != 1 {
		t.Fatalf("closed=%v errs=%d finals=%d, want a clean single completion", run.closed, len(run.errs), len(run.finals))
	}
	if got, want := run.raw.String(), strings.Join(events, ""); got != want {
		t.Fatalf("forwarded bytes differ:\n got %q\nwant %q", got, want)
	}
	usage := run.finals[0].PassthroughUsage
	if usage == nil || usage.LLMUsage == nil || usage.LLMUsage.TotalTokens != 9 {
		t.Fatalf("trailing usage chunk lost: %+v", usage)
	}
	if run.parkedFlag {
		t.Fatal("a Chat Completions stream was marked parked")
	}
}

// TestOpenAIPassthroughStreamCancelledBeforeEndEvent pins that a client hangup before the end
// event is still reported once as a cancellation, never as a completion.
func TestOpenAIPassthroughStreamCancelledBeforeEndEvent(t *testing.T) {
	events := []string{
		sseEvent(`{"type":"response.created","response":{"id":"resp_1"}}`),
		sseEvent(`{"type":"response.output_text.delta","delta":"hi"}`),
	}
	run := runOpenAIPassthroughStream(t, "/v1/responses", events, true, false, "output_text.delta")
	if !run.closed {
		t.Fatal("stream channel did not close after the client hung up")
	}
	if len(run.finals) != 0 {
		t.Fatalf("a cancelled stream reported %d completions", len(run.finals))
	}
	if len(run.errs) != 1 || run.errs[0].StatusCode == nil || *run.errs[0].StatusCode != 499 {
		t.Fatalf("expected exactly one 499 cancellation, got %d errors", len(run.errs))
	}
}

// TestOpenAIPassthroughStreamUpstreamFailureBeforeEndEvent pins that an upstream that drops the
// connection before any end event is reported once as an error, never as a completion.
func TestOpenAIPassthroughStreamUpstreamFailureBeforeEndEvent(t *testing.T) {
	events := []string{
		sseEvent(`{"type":"response.created","response":{"id":"resp_1"}}`),
		sseEvent(`{"type":"response.output_text.delta","delta":"hi"}`),
	}
	run := runOpenAIPassthroughStream(t, "/v1/responses", events, false, true, "")
	if !run.closed {
		t.Fatal("stream channel did not close")
	}
	if len(run.finals)+len(run.errs) != 1 {
		t.Fatalf("expected exactly one terminal post-hook call, got %d finals and %d errors", len(run.finals), len(run.errs))
	}
	if len(run.finals) == 1 && run.finals[0].PassthroughUsage != nil {
		t.Fatalf("an aborted stream reported usage: %+v", run.finals[0].PassthroughUsage)
	}
	if got := run.raw.String(); !strings.Contains(got, "output_text.delta") {
		t.Fatalf("bytes before the failure were not forwarded: %q", got)
	}
}

// openAIResponsesStreamBody is a Responses stream of deltas events ending in response.completed.
func openAIResponsesStreamBody(deltas int) []byte {
	var b bytes.Buffer
	b.WriteString("event: response.created\n" + sseEvent(`{"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`))
	for range deltas {
		b.WriteString("event: response.output_text.delta\n" + sseEvent(`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hello"}`))
	}
	b.WriteString("event: response.completed\n" + sseEvent(`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`))
	return b.Bytes()
}

// BenchmarkOpenAIPassthroughStreamLoop measures the shared passthrough loop with OpenAI's
// passthrough params over a 50-delta Responses stream, with and without the end-event rule
// (upstream 43e55b2a6 added EndsStream, called once per framed event).
func BenchmarkOpenAIPassthroughStreamLoop(b *testing.B) {
	body := openAIResponsesStreamBody(50)
	reqBody := []byte(`{"model":"gpt-5","stream":true}`)
	runner := func(_ *schemas.BifrostContext, r *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		return r, err
	}
	for _, tc := range []struct {
		name string
		ends func([]byte) bool
	}{{"without_end_rule", nil}, {"with_end_rule", EndsOpenAIPassthroughStream}} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
				ch := providerUtils.StreamPassthroughHTTP(ctx, runner, nil, io.NopCloser(bytes.NewReader(body)), providerUtils.PassthroughStreamParams{
					StatusCode: http.StatusOK,
					Path:       "/v1/responses",
					RawRequest: reqBody,
					Logger:     passthroughTestLogger{},
					HasUsage:   HasOpenAIPassthroughUsage,
					EndsStream: tc.ends,
					Observe: func(event []byte) *schemas.BifrostPassthroughUsage {
						return ExtractOpenAIPassthroughUsage(http.MethodPost, "/v1/responses", reqBody, event)
					},
				})
				for range ch {
				}
				ctx.Cancel()
			}
		})
	}
}
