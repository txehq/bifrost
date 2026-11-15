package utils

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/capsohq/bifrost/core/network"
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
	// VendorURL is set when the file's address was named by the vendor (a URL in its task
	// response) instead of being built from the provider's base URL. It is the address to
	// fetch, byte for byte. Such an address is not trusted: the file is fetched through the
	// SSRF-hardened media fetcher (FetchMediaStream, or FetchMediaURL when not streamed), which
	// admits public addresses only, on every redirect hop, and honors the provider's proxy.
	// Request then supplies the headers only, which reach the host VendorURL names and no
	// other host a redirect leads to; Client, StreamingClient and MaxRedirects are not used.
	// The one exception is a VendorURL on the origin of BaseURL: the operator configured that
	// host, so it is fetched like any other request to it, by Client and StreamingClient
	// (fork, TXE-7036).
	VendorURL string
	// BaseURL is the provider's configured base URL, for the exception above. Empty means
	// VendorURL never goes to the provider's clients.
	BaseURL string
}

// sameOrigin reports whether two URLs share scheme, host and port, the port defaulted by
// scheme. A URL that does not parse, or has no host, shares an origin with nothing.
func sameOrigin(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil || ua.Host == "" || ub.Host == "" {
		return false
	}
	return sameParsedOrigin(ua, ub)
}

// sameParsedOrigin is sameOrigin for URLs already parsed, each with a host.
func sameParsedOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(dialAddr(a), dialAddr(b))
}

// fetchVendorVideoURL is FetchVideoContent for a file at an address the vendor named, off the
// provider's own origin: the download runs on the SSRF-hardened media fetcher.
func fetchVendorVideoURL(ctx *schemas.BifrostContext, f VideoContentFetch) (*schemas.BifrostVideoDownloadResponse, *schemas.BifrostError) {
	var header http.Header
	if f.Request != nil {
		for name, value := range f.Request.Header.All() {
			switch strings.ToLower(string(name)) {
			case "host", "user-agent", "content-length", "content-type":
				continue
			}
			if header == nil {
				header = make(http.Header)
			}
			header.Add(string(name), string(value))
		}
	}
	contentTypeOf := func(upstream string) string {
		if f.ContentType != nil {
			return f.ContentType(upstream)
		}
		if upstream == "" {
			return "video/mp4"
		}
		return upstream
	}
	failed := func(err error, latency time.Duration) *schemas.BifrostError {
		var statusErr *MediaFetchStatusError
		if errors.As(err, &statusErr) && f.ParseError != nil {
			// The refusal's body is not read from an untrusted host; ParseError gets the status.
			errResp := fasthttp.AcquireResponse()
			defer fasthttp.ReleaseResponse(errResp)
			errResp.SetStatusCode(statusErr.StatusCode)
			return SetErrorLatency(f.ParseError(errResp), latency)
		}
		return SetErrorLatency(NewBifrostOperationError("failed to download video", err), latency)
	}

	// The file host's response headers replace the task lookup's on the context and travel on
	// the response, as on the provider-client path.
	responseHeaders := func(h http.Header) map[string]string {
		headers := ExtractProviderResponseHeadersFromHTTP(&http.Response{Header: h})
		ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, headers)
		return headers
	}

	start := time.Now()
	if !f.Stream {
		contentType, body, respHeader, err := fetchMediaURLResponse(ctx, f.VendorURL, header)
		latency := time.Since(start)
		if err != nil {
			return nil, failed(err, latency)
		}
		// The held file is decoded as the streamed one is: gzip decoded whole under the held
		// cap, any other encoding refused.
		switch encoding := strings.ToLower(strings.TrimSpace(respHeader.Get("Content-Encoding"))); encoding {
		case "", "identity":
		case "gzip", "x-gzip":
			decoded, decodeErr := decodeHeldVideo(body, encoding)
			if decodeErr != nil {
				return nil, failed(decodeErr, latency)
			}
			body = decoded
		default:
			return nil, failed(fmt.Errorf("video has unsupported Content-Encoding %q", encoding), latency)
		}
		return &schemas.BifrostVideoDownloadResponse{
			VideoID:     f.VideoID,
			Content:     body,
			ContentType: contentTypeOf(contentType),
			ExtraFields: schemas.BifrostResponseExtraFields{Latency: latency.Milliseconds(), ProviderResponseHeaders: responseHeaders(respHeader)},
		}, nil
	}
	SetStreamIdleTimeoutIfEmpty(ctx, f.StreamIdleTimeoutSeconds)
	byteRange := videoDownloadRange(ctx)
	opts := MediaStreamOptions{Header: header, IdleTimeout: GetStreamIdleTimeout(ctx)}
	if byteRange != nil {
		opts.Range = videoRangeHeader(byteRange)
	}
	stream, err := FetchMediaStream(ctx, f.VendorURL, opts)
	latency := time.Since(start)
	if err != nil {
		return nil, failed(err, latency)
	}
	response := &schemas.BifrostVideoDownloadResponse{
		VideoID:     f.VideoID,
		ContentType: contentTypeOf(stream.ContentType),
		Body:        stream.Body,
		ExtraFields: schemas.BifrostResponseExtraFields{Latency: latency.Milliseconds(), ProviderResponseHeaders: responseHeaders(stream.Header)},
	}
	if byteRange != nil && stream.StatusCode == http.StatusPartialContent {
		contentRange, err := partialVideoContent(byteRange, stream.Header.Get("Content-Range"), stream.Header.Get("Content-Encoding"))
		if err != nil {
			_ = stream.Body.Close()
			return nil, failed(err, latency)
		}
		response.ContentRange = contentRange
		response.ContentLength = contentRange.Length
		return response, nil
	}
	if stream.ContentLength > 0 {
		response.ContentLength = stream.ContentLength
	}
	return response, nil
}

// ErrVideoRangeUnusable is a 206 answer to a ranged download whose bytes are not the ones asked
// for: no Content-Range, a different start, more bytes than asked, or an encoded body. The caller
// measures the file from a whole download instead (fork, TXE-7391).
var ErrVideoRangeUnusable = errors.New("the host's partial answer does not match the range asked for")

// videoDownloadRange is the range a streamed download was asked for, nil when none or invalid.
func videoDownloadRange(ctx *schemas.BifrostContext) *schemas.VideoByteRange {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(schemas.BifrostContextKeyVideoDownloadRange).(*schemas.VideoByteRange)
	if r == nil || r.Offset < 0 || r.Length <= 0 || r.Offset > math.MaxInt64-r.Length {
		return nil
	}
	return r
}

// videoRangeHeader is r as an HTTP Range header value.
func videoRangeHeader(r *schemas.VideoByteRange) string {
	return fmt.Sprintf("bytes=%d-%d", r.Offset, r.Offset+r.Length-1)
}

// partialVideoContent checks a 206 answer against the range asked for: "bytes first-last/total"
// (total may be "*"), starting where asked, no longer than asked, and not encoded.
func partialVideoContent(r *schemas.VideoByteRange, contentRange, contentEncoding string) (*schemas.VideoContentRange, error) {
	if enc := strings.ToLower(strings.TrimSpace(contentEncoding)); enc != "" && enc != "identity" {
		return nil, fmt.Errorf("%w: the partial body is %s-encoded", ErrVideoRangeUnusable, enc)
	}
	spec, ok := strings.CutPrefix(strings.TrimSpace(contentRange), "bytes ")
	if !ok {
		return nil, fmt.Errorf("%w: Content-Range %q", ErrVideoRangeUnusable, contentRange)
	}
	span, totalSpec, ok := strings.Cut(spec, "/")
	firstSpec, lastSpec, ok2 := strings.Cut(span, "-")
	if !ok || !ok2 {
		return nil, fmt.Errorf("%w: Content-Range %q", ErrVideoRangeUnusable, contentRange)
	}
	first, err1 := strconv.ParseInt(strings.TrimSpace(firstSpec), 10, 64)
	last, err2 := strconv.ParseInt(strings.TrimSpace(lastSpec), 10, 64)
	if err1 != nil || err2 != nil || first != r.Offset || last < first || last-first+1 > r.Length {
		return nil, fmt.Errorf("%w: Content-Range %q for %s", ErrVideoRangeUnusable, contentRange, videoRangeHeader(r))
	}
	total := int64(-1)
	if t := strings.TrimSpace(totalSpec); t != "*" {
		parsed, err := strconv.ParseInt(t, 10, 64)
		if err != nil || parsed <= last {
			return nil, fmt.Errorf("%w: Content-Range %q", ErrVideoRangeUnusable, contentRange)
		}
		total = parsed
	}
	return &schemas.VideoContentRange{Offset: first, Length: last - first + 1, Total: total}, nil
}

// maxVideoErrorBodyBytes caps how much of a refused download's body is read for its error.
const maxVideoErrorBodyBytes = 512 * 1024

// FetchVideoContent downloads a video file and returns it as the VideoDownload response: the
// bytes in Content, or, when f.Stream is set and the provider has a streaming client, the open
// vendor response in Body, which is read as the caller forwards it and so is never held whole
// (fork, contract C9 / BF20). Closing Body releases the vendor response. Every provider's
// VideoDownload builds its response through this function or NewVideoContentResponse; a test
// keeps it that way.
//
// A streamed download may ask for part of the file only (BifrostVideoDownloadRequest.Range, fork,
// TXE-7391): the video settler reads an MP4's header boxes this way and never its media. Core puts
// the range on the context (BifrostContextKeyVideoDownloadRange), so no provider passes it on, and
// it is sent as an HTTP Range request on the provider's client and on the media fetcher alike. A
// host that honors it answers exactly those bytes, with the response's ContentRange set; a host
// that ignores it, a file the provider holds inline, and a buffered download answer with the whole
// file and ContentRange nil, so the caller always checks ContentRange. A partial answer for other
// bytes, or an encoded one, is ErrVideoRangeUnusable.
func FetchVideoContent(ctx *schemas.BifrostContext, f VideoContentFetch) (*schemas.BifrostVideoDownloadResponse, *schemas.BifrostError) {
	if f.VendorURL != "" && !(f.BaseURL != "" && f.Client != nil && sameOrigin(f.VendorURL, f.BaseURL)) {
		return fetchVendorVideoURL(ctx, f)
	}
	stream := f.Stream && f.StreamingClient != nil
	client := f.Client
	resp := fasthttp.AcquireResponse()
	var byteRange *schemas.VideoByteRange
	if stream {
		client = f.StreamingClient
		resp.StreamBody = true
		SetStreamIdleTimeoutIfEmpty(ctx, f.StreamIdleTimeoutSeconds)
		if byteRange = videoDownloadRange(ctx); byteRange != nil {
			f.Request.Header.Set("Range", videoRangeHeader(byteRange))
			f.Request.Header.Set("Accept-Encoding", "identity")
		}
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

	partial := stream && byteRange != nil && resp.StatusCode() == fasthttp.StatusPartialContent
	if resp.StatusCode() != fasthttp.StatusOK && !partial {
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
	encoding := strings.ToLower(strings.TrimSpace(string(resp.Header.Peek("Content-Encoding"))))
	if !stream {
		defer fasthttp.ReleaseResponse(resp)
		if encoding != "" && encoding != "identity" {
			decoded, err := decodeHeldVideo(resp.Body(), encoding)
			if err != nil {
				return nil, heldVideoError(err)
			}
			response.Content = decoded
			return response, nil
		}
		body, err := CheckAndDecodeBody(resp)
		if err != nil {
			return nil, NewBifrostOperationError(schemas.ErrProviderResponseDecode, err)
		}
		response.Content = append([]byte(nil), body...)
		return response, nil
	}
	if partial {
		contentRange, err := partialVideoContent(byteRange, string(resp.Header.Peek("Content-Range")), string(resp.Header.Peek("Content-Encoding")))
		if err != nil {
			ReleaseStreamingResponse(ctx, resp)
			return nil, NewBifrostOperationError("failed to download video range", err)
		}
		response.ContentRange = contentRange
		response.ContentLength = contentRange.Length
		response.Body = WrapStreamingResponseBody(ctx, resp)
		return response, nil
	}
	if encoding != "" && encoding != "identity" {
		// gzip, br, deflate, zstd or a chain: an encoded file is never streamed. It is read whole
		// through the guarded stream and decoded under the held cap (TXE-7089), since a stream
		// decoded on the way through has no cap, and a few KiB of gzip can unfold into
		// gigabytes. A video is already compressed, so a vendor encoding it again is not
		// expected, and holding it costs nothing in practice.
		return decodeEncodedVideoStream(ctx, resp, response)
	}
	// Content-Length describes the bytes on the wire, so it is passed on only when they are the
	// file itself.
	if length := resp.Header.ContentLength(); length > 0 {
		response.ContentLength = int64(length)
	}
	response.Body = WrapStreamingResponseBody(ctx, resp)
	return response, nil
}

// heldVideoLimit is the most a held video download may be, both as the bytes on the wire and
// decoded (fork, TXE-7089): the media fetch cap or client.max_response_body_size_mb, whichever is
// tighter. It is read on every download, so a saved client config applies to the next one.
func heldVideoLimit() int64 {
	return min(mediaFetchLimits.maxBytes, network.MaxResponseBodyBytes())
}

// decodeHeldVideo removes the content codings an encoded video carries, decoding it whole.
// Every video download path decodes through it (fork, TXE-7089): the cap applies to the bytes the
// caller is handed, not to the bytes on the wire, so a small compressed body that would unfold
// past heldVideoLimit fails with an error matching ErrMediaTooLarge, and no part of it is
// delivered. Decoding stops at the limit; the expansion is never completed.
func decodeHeldVideo(encoded []byte, encoding string) ([]byte, error) {
	limit := heldVideoLimit()
	tooLarge := func(stage string) error {
		return &mediaTooLargeError{message: fmt.Sprintf("%s video exceeds %d-byte limit", stage, limit)}
	}
	if int64(len(encoded)) > limit {
		return nil, tooLarge("encoded")
	}
	decodeResp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(decodeResp)
	decodeResp.Header.Set("Content-Encoding", encoding)
	decodeResp.SetBodyRaw(encoded)
	decoded, err := checkAndDecodeBodyLimit(decodeResp, limit)
	if errors.Is(err, network.ErrResponseBodyTooLarge) {
		return nil, tooLarge("decoded")
	}
	if err != nil {
		return nil, err
	}
	return decoded, nil
}

// heldVideoError is the BifrostError for a held video that failed to decode: a download past the
// cap keeps its ErrMediaTooLarge cause, which the video settler reads as final.
func heldVideoError(err error) *schemas.BifrostError {
	if errors.Is(err, ErrMediaTooLarge) {
		return NewBifrostOperationError("failed to download video", err)
	}
	return NewBifrostOperationError(schemas.ErrProviderResponseDecode, err)
}

// decodeEncodedVideoStream finishes a streamed download whose Content-Encoding the stream
// decoder cannot handle. It takes ownership of resp.
func decodeEncodedVideoStream(ctx *schemas.BifrostContext, resp *fasthttp.Response, response *schemas.BifrostVideoDownloadResponse) (*schemas.BifrostVideoDownloadResponse, *schemas.BifrostError) {
	encoding := strings.ToLower(strings.TrimSpace(string(resp.Header.Peek("Content-Encoding"))))
	// The raw bytes, with every layer of the encoding still on them: decodeHeldVideo removes the
	// layers the header names, a gzip layer of a chain included.
	guarded := WrapRawStreamingResponseBody(ctx, resp)
	encoded, err := io.ReadAll(io.LimitReader(guarded, heldVideoLimit()+1))
	_ = guarded.Close()
	if err != nil {
		return nil, largeResponseReadError(err)
	}
	decoded, err := decodeHeldVideo(encoded, encoding)
	if err != nil {
		return nil, heldVideoError(err)
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
