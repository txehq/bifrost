package utils

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// VideoContentFetch is one GET of a video file from a vendor, for FetchVideoContent.
type VideoContentFetch struct {
	// Request is the prepared GET: URL, auth and extra headers. The caller keeps ownership and
	// releases it.
	Request *fasthttp.Request
	// Client performs a buffered fetch. StreamingClient performs a streamed one: it must be a
	// client built by BuildStreamingClient, whose read timeout bounds the wait for the headers
	// only. A fetch asked to stream without a StreamingClient is buffered.
	Client          *fasthttp.Client
	StreamingClient *fasthttp.Client
	// Stream is BifrostVideoDownloadRequest.Stream.
	Stream bool
	// VideoID is the id the response carries.
	VideoID string
	// MaxRedirects, when positive, follows that many redirects.
	MaxRedirects int
	// StreamIdleTimeoutSeconds is the provider's configured stream idle timeout.
	StreamIdleTimeoutSeconds int
	// ParseError turns a non-200 response into the error returned. On a streamed fetch it is
	// given a buffered copy holding at most the first 512 KiB of the body.
	ParseError func(resp *fasthttp.Response) *schemas.BifrostError
	// ContentType maps the vendor's Content-Type to the one returned; nil keeps it, and an
	// absent one becomes video/mp4.
	ContentType func(upstream string) string
}

// maxVideoErrorBodyBytes caps how much of a refused download's body is read for its error.
const maxVideoErrorBodyBytes = 512 * 1024

// FetchVideoContent downloads a video file and returns it as the VideoDownload response: the
// bytes in Content, or, when f.Stream is set and the provider has a streaming client, the open
// vendor response in Body, which is read as the caller forwards it and so is never held whole
// (fork, contract C9 / BF20). Closing Body releases the vendor response. Every provider's
// VideoDownload builds its response through this function or NewVideoContentResponse; a test
// keeps it that way.
func FetchVideoContent(ctx *schemas.BifrostContext, f VideoContentFetch) (*schemas.BifrostVideoDownloadResponse, *schemas.BifrostError) {
	stream := f.Stream && f.StreamingClient != nil
	client := f.Client
	resp := fasthttp.AcquireResponse()
	if stream {
		client = f.StreamingClient
		resp.StreamBody = true
		SetStreamIdleTimeoutIfEmpty(ctx, f.StreamIdleTimeoutSeconds)
	}
	// A streamed response is released by Body.Close, or by releaseResp on a failure here.
	releaseResp := func() {
		if stream {
			ReleaseStreamingResponse(ctx, resp)
			return
		}
		fasthttp.ReleaseResponse(resp)
	}

	var latency time.Duration
	var bifrostErr *schemas.BifrostError
	var wait func()
	if f.MaxRedirects > 0 {
		latency, bifrostErr, wait = MakeRequestWithContextFollowRedirects(ctx, client, f.Request, resp, f.MaxRedirects)
	} else {
		latency, bifrostErr, wait = MakeRequestWithContext(ctx, client, f.Request, resp)
	}
	// On a cancelled request the call above returns while its goroutine may still be filling
	// resp. wait blocks until that goroutine is done (a no-op otherwise), so it runs before
	// resp is read or released, never after.
	wait()
	if bifrostErr != nil {
		releaseResp()
		return nil, bifrostErr
	}
	providerResponseHeaders := ExtractProviderResponseHeaders(resp)
	ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, providerResponseHeaders)
	// A response with no body stream (a status that carries none) has nothing to forward.
	if stream && resp.BodyStream() == nil {
		stream = false
	}

	if resp.StatusCode() != fasthttp.StatusOK {
		if !stream {
			defer fasthttp.ReleaseResponse(resp)
			return nil, SetErrorLatency(f.ParseError(resp), latency)
		}
		// The refusal's body is a stream too. It is read through the same guard as a file: a
		// vendor that stalls after its headers trips the idle timeout, and a caller that hangs
		// up closes the socket. Only a bounded part is kept for ParseError; the rest is
		// abandoned with the connection, not drained.
		// The header is copied after the wrap, which drops Content-Encoding once it decodes
		// gzip, so ParseError is not told to decode the body a second time.
		guarded := WrapStreamingResponseBody(ctx, resp)
		errResp := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseResponse(errResp)
		resp.Header.CopyTo(&errResp.Header)
		errBody, _ := io.ReadAll(io.LimitReader(guarded, maxVideoErrorBodyBytes))
		_ = guarded.Close()
		errResp.SetBodyRaw(errBody)
		return nil, SetErrorLatency(f.ParseError(errResp), latency)
	}

	contentType := string(resp.Header.ContentType())
	if f.ContentType != nil {
		contentType = f.ContentType(contentType)
	} else if contentType == "" {
		contentType = "video/mp4"
	}
	response := &schemas.BifrostVideoDownloadResponse{
		VideoID:     f.VideoID,
		ContentType: contentType,
		ExtraFields: schemas.BifrostResponseExtraFields{
			Latency:                 latency.Milliseconds(),
			ProviderResponseHeaders: providerResponseHeaders,
		},
	}
	if !stream {
		defer fasthttp.ReleaseResponse(resp)
		body, err := CheckAndDecodeBody(resp)
		if err != nil {
			return nil, NewBifrostOperationError(schemas.ErrProviderResponseDecode, err)
		}
		response.Content = append([]byte(nil), body...)
		return response, nil
	}
	encoding := strings.ToLower(strings.TrimSpace(string(resp.Header.Peek("Content-Encoding"))))
	switch encoding {
	case "", "identity":
		// Content-Length describes the bytes on the wire, so it is passed on only when they
		// are the file itself.
		if length := resp.Header.ContentLength(); length > 0 {
			response.ContentLength = int64(length)
		}
	case "gzip", "x-gzip":
		// Decoded on the way through by WrapStreamingResponseBody.
	default:
		// br, deflate, zstd or a chain: the stream decoder handles gzip only, so the file is
		// read whole through the guarded stream and decoded as the buffered path decodes it.
		// A video is already compressed, so a vendor encoding it again is not expected; the
		// read is capped at the media fetch limit.
		return decodeEncodedVideoStream(ctx, resp, response)
	}
	response.Body = WrapStreamingResponseBody(ctx, resp)
	return response, nil
}

// decodeEncodedVideoStream finishes a streamed download whose Content-Encoding the stream
// decoder cannot handle. It takes ownership of resp.
func decodeEncodedVideoStream(ctx *schemas.BifrostContext, resp *fasthttp.Response, response *schemas.BifrostVideoDownloadResponse) (*schemas.BifrostVideoDownloadResponse, *schemas.BifrostError) {
	decodeResp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(decodeResp)
	resp.Header.CopyTo(&decodeResp.Header)
	// The raw bytes, with every layer of the encoding still on them: CheckAndDecodeBody below
	// removes the layers the copied header names, a gzip layer of a chain included.
	guarded := WrapRawStreamingResponseBody(ctx, resp)
	encoded, err := io.ReadAll(io.LimitReader(guarded, mediaFetchLimits.maxBytes+1))
	_ = guarded.Close()
	if err != nil {
		return nil, largeResponseReadError(err)
	}
	if int64(len(encoded)) > mediaFetchLimits.maxBytes {
		return nil, NewBifrostOperationError(fmt.Sprintf("encoded video exceeds %d-byte limit", mediaFetchLimits.maxBytes), nil)
	}
	decodeResp.SetBodyRaw(encoded)
	decoded, err := CheckAndDecodeBody(decodeResp)
	if err != nil {
		return nil, NewBifrostOperationError(schemas.ErrProviderResponseDecode, err)
	}
	response.Body = io.NopCloser(bytes.NewReader(decoded))
	response.ContentLength = int64(len(decoded))
	return response, nil
}

// NewVideoContentResponse is the VideoDownload response for bytes the provider already holds
// (a file that arrived inline in the task, or one fetched by a client that cannot stream). A
// request that asked for a stream gets them as Body, so its caller has one path to serve.
func NewVideoContentResponse(request *schemas.BifrostVideoDownloadRequest, videoID string, content []byte, contentType string, latency time.Duration) *schemas.BifrostVideoDownloadResponse {
	response := &schemas.BifrostVideoDownloadResponse{
		VideoID:     videoID,
		ContentType: contentType,
		ExtraFields: schemas.BifrostResponseExtraFields{Latency: latency.Milliseconds()},
	}
	if request != nil && request.Stream {
		response.Body = io.NopCloser(bytes.NewReader(content))
		response.ContentLength = int64(len(content))
		return response
	}
	response.Content = content
	return response
}
