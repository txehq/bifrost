package lib

import (
	"io"
	"strconv"

	"github.com/valyala/fasthttp"
)

// FastHTTPUserValueStreamedBody holds the *StreamedBody a handler is sending, so the code that
// owns the request context can hand its cancel func to the body (see StreamedBody.OnClose).
const FastHTTPUserValueStreamedBody = "__bifrost_streamed_body"

// StreamedBody is a provider's file being sent to the caller as it is read from the vendor
// (fork, contract C9 / BF20). fasthttp reads it after the handler has returned, so the request
// context must outlive the handler: whoever owns that context registers its cancel func with
// OnClose and does not cancel on return. Closing the body releases the vendor response, then
// the request context. A caller that hangs up midway ends the request first, so the vendor
// stream is abandoned the way a cancelled SSE stream is, instead of being read to its end for
// nobody.
type StreamedBody struct {
	body    io.ReadCloser
	release func()
	sent    bool // the whole file was read
}

// ServeStreamedBody sets body as the response: status 200, the given type, and the length when
// it is known (fasthttp reads to EOF and sends chunked otherwise). It marks the request as
// streamed so the post-hook middleware does not copy the body for plugins, which would read
// the whole file before it is sent.
func ServeStreamedBody(ctx *fasthttp.RequestCtx, body io.ReadCloser, contentType string, contentLength int64) *StreamedBody {
	streamed := &StreamedBody{body: body}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	ctx.SetStatusCode(fasthttp.StatusOK)
	ctx.Response.Header.Set("Content-Type", contentType)
	bodySize := -1
	if contentLength > 0 {
		bodySize = int(contentLength)
		ctx.Response.Header.Set("Content-Length", strconv.FormatInt(contentLength, 10))
	}
	ctx.Response.SetBodyStream(streamed, bodySize)
	ctx.SetUserValue(FastHTTPUserValueLargeResponseMode, true)
	ctx.SetUserValue(FastHTTPUserValueStreamedBody, streamed)
	return streamed
}

// StreamedBodyOf returns the body ServeStreamedBody set on this request, or nil.
func StreamedBodyOf(ctx *fasthttp.RequestCtx) *StreamedBody {
	streamed, _ := ctx.UserValue(FastHTTPUserValueStreamedBody).(*StreamedBody)
	return streamed
}

// OnClose registers the func that ends the request context; it runs once, when the body is
// closed. Call it before the handler returns.
func (b *StreamedBody) OnClose(release func()) {
	b.release = release
}

func (b *StreamedBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if err == io.EOF {
		b.sent = true
	}
	return n, err
}

// Close ends the request first when the file was not read to its end, so the vendor stream is
// closed by the cancellation instead of drained; otherwise after the vendor response is released.
func (b *StreamedBody) Close() error {
	if !b.sent {
		b.endRequest()
	}
	err := b.body.Close()
	b.endRequest()
	return err
}

func (b *StreamedBody) endRequest() {
	if b.release != nil {
		b.release()
		b.release = nil
	}
}
