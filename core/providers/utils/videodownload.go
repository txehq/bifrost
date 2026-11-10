package utils

import (
	"bytes"
	"io"
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
	defer wait()
	if bifrostErr != nil {
		releaseResp()
		return nil, bifrostErr
	}
	providerResponseHeaders := ExtractProviderResponseHeaders(resp)
	ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, providerResponseHeaders)

	if resp.StatusCode() != fasthttp.StatusOK {
		defer releaseResp()
		if !stream {
			return nil, SetErrorLatency(f.ParseError(resp), latency)
		}
		// Hand ParseError a buffered copy holding a bounded part of the refusal, so an
		// oversized error body is never read whole.
		errResp := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseResponse(errResp)
		resp.Header.CopyTo(&errResp.Header)
		if bodyStream := resp.BodyStream(); bodyStream != nil {
			errBody, _ := io.ReadAll(io.LimitReader(bodyStream, maxVideoErrorBodyBytes))
			errResp.SetBodyRaw(errBody)
		}
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
		defer releaseResp()
		body, err := CheckAndDecodeBody(resp)
		if err != nil {
			return nil, NewBifrostOperationError(schemas.ErrProviderResponseDecode, err)
		}
		response.Content = append([]byte(nil), body...)
		return response, nil
	}
	// Content-Length describes the bytes on the wire, so it is passed on only when they are
	// the file itself.
	if length := resp.Header.ContentLength(); length > 0 && len(resp.Header.Peek("Content-Encoding")) == 0 {
		response.ContentLength = int64(length)
	}
	response.Body = WrapStreamingResponseBody(ctx, resp)
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
