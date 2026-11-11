package lib

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/valyala/fasthttp"
)

type closeRecorder struct {
	io.Reader
	events *[]string
}

func (c closeRecorder) Close() error {
	*c.events = append(*c.events, "vendor-released")
	return nil
}

// TestStreamedBodyEndsTheRequestWhenClosed pins who ends the request context of a streamed file
// (contract C9 / BF20): not the handler, which returns before the file is sent, but the body. A
// file read to its end releases the vendor response and then the request; one abandoned midway
// ends the request first, so the cancellation closes the vendor stream instead of a drain.
func TestStreamedBodyEndsTheRequestWhenClosed(t *testing.T) {
	var events []string
	var ctx fasthttp.RequestCtx
	body := ServeStreamedBody(&ctx, closeRecorder{strings.NewReader("MP4DATA"), &events}, "video/mp4", 7)
	body.OnClose(func() { events = append(events, "request-ended") })

	if StreamedBodyOf(&ctx) != body {
		t.Fatal("the served body must be reachable from the request")
	}
	if !ctx.Response.IsBodyStream() || string(ctx.Response.Header.ContentType()) != "video/mp4" || ctx.Response.Header.ContentLength() != 7 {
		t.Fatalf("response = stream %v type %q length %d", ctx.Response.IsBodyStream(), ctx.Response.Header.ContentType(), ctx.Response.Header.ContentLength())
	}
	if marked, _ := ctx.UserValue(FastHTTPUserValueLargeResponseMode).(bool); !marked {
		t.Fatal("a streamed body must be marked so the post-hook middleware does not copy it")
	}
	if len(events) != 0 {
		t.Fatalf("nothing ends before the body is read, got %v", events)
	}
	if got, err := io.ReadAll(body); err != nil || string(got) != "MP4DATA" {
		t.Fatalf("read %q, %v", got, err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(events, ",") != "vendor-released,request-ended" {
		t.Fatalf("a fully sent file releases the vendor, then the request: %v", events)
	}

	events = nil
	var abandoned fasthttp.RequestCtx
	partial := ServeStreamedBody(&abandoned, closeRecorder{io.MultiReader(strings.NewReader("MP4"), errReader{}), &events}, "", 0)
	partial.OnClose(func() { events = append(events, "request-ended") })
	if string(abandoned.Response.Header.ContentType()) != "application/octet-stream" {
		t.Fatalf("an untyped file is served as octet-stream, got %q", abandoned.Response.Header.ContentType())
	}
	buf := make([]byte, 3)
	_, _ = io.ReadFull(partial, buf)
	_ = partial.Close()
	_ = partial.Close()
	if strings.Join(events, ",") != "request-ended,vendor-released,vendor-released" {
		t.Fatalf("an abandoned file ends the request first, and only once: %v", events)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("client went away") }
