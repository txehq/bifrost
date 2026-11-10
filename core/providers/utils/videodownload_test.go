package utils

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// videoFetch is a FetchVideoContent call against a test server.
func videoFetch(t testing.TB, target string, stream bool) (*schemas.BifrostVideoDownloadResponse, *schemas.BifrostError) {
	t.Helper()
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	req.SetRequestURI(target)
	req.Header.SetMethod(http.MethodGet)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	return FetchVideoContent(ctx, VideoContentFetch{
		Request:         req,
		Client:          &fasthttp.Client{},
		StreamingClient: BuildStreamingClient(&fasthttp.Client{}),
		Stream:          stream,
		VideoID:         "video_1:openai",
		ParseError: func(resp *fasthttp.Response) *schemas.BifrostError {
			return NewProviderAPIError(string(resp.Body()), nil, resp.StatusCode(), nil, nil)
		},
	})
}

// TestFetchVideoContentStreamsPastTheOldCap is contract C9 / BF20: a file larger than the 256 MiB
// the buffered download could hold is forwarded without being held. The allocation bound is the
// streaming assertion: a buffered read of this file allocates its whole size at least once.
func TestFetchVideoContentStreamsPastTheOldCap(t *testing.T) {
	fileBytes := int(mediaFetchLimits.maxBytes) + 8<<20
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", strconv.Itoa(fileBytes))
		chunk := make([]byte, 1<<20)
		for written := 0; written < fileBytes; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	resp, bifrostErr := videoFetch(t, srv.URL+"/v1/videos/video_1/content", true)
	if bifrostErr != nil {
		t.Fatalf("FetchVideoContent: %v", bifrostErr.Error)
	}
	if resp.Body == nil || resp.Content != nil {
		t.Fatalf("a streamed fetch must return Body and no Content, got body=%v content=%d bytes", resp.Body != nil, len(resp.Content))
	}
	drained, err := io.CopyBuffer(io.Discard, resp.Body, make([]byte, 64<<10))
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	runtime.ReadMemStats(&after)

	if drained != int64(fileBytes) || resp.ContentLength != int64(fileBytes) {
		t.Fatalf("drained %d bytes, content length %d, want %d", drained, resp.ContentLength, fileBytes)
	}
	if resp.ContentType != "video/mp4" || resp.VideoID != "video_1:openai" {
		t.Fatalf("got type %q id %q", resp.ContentType, resp.VideoID)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 32<<20 {
		t.Fatalf("forwarding a %d MiB file allocated %d MiB; the body is being buffered", fileBytes>>20, allocated>>20)
	}
}

// TestFetchVideoContentDeliversBytesBeforeTheVendorFinishes proves the body is a live stream: the
// vendor sends half the file and then waits for the caller to have read it before sending the
// rest. A buffered fetch would never return, because the vendor never finishes on its own.
func TestFetchVideoContentDeliversBytesBeforeTheVendorFinishes(t *testing.T) {
	firstHalfRead := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("FIRST-HALF|"))
		w.(http.Flusher).Flush()
		select {
		case <-firstHalfRead:
		case <-time.After(10 * time.Second):
			return
		}
		_, _ = w.Write([]byte("SECOND-HALF"))
	}))
	t.Cleanup(srv.Close)

	resp, bifrostErr := videoFetch(t, srv.URL+"/clip", true)
	if bifrostErr != nil {
		t.Fatalf("FetchVideoContent: %v", bifrostErr.Error)
	}
	defer resp.Body.Close()
	first := make([]byte, len("FIRST-HALF|"))
	if _, err := io.ReadFull(resp.Body, first); err != nil || string(first) != "FIRST-HALF|" {
		t.Fatalf("first half: %q, %v", first, err)
	}
	close(firstHalfRead)
	rest, err := io.ReadAll(resp.Body)
	if err != nil || string(rest) != "SECOND-HALF" {
		t.Fatalf("second half: %q, %v", rest, err)
	}
	// The vendor sent no Content-Length, so none is claimed.
	if resp.ContentLength != 0 {
		t.Fatalf("content length = %d, want 0 for a chunked body", resp.ContentLength)
	}
}

// TestFetchVideoContentBufferedAndErrors pins the paths around the stream: a caller that did not
// ask for a stream gets Content as before; a provider with no streaming client buffers; and a
// refusal reaches ParseError with its body on both paths, read to a bound when streamed.
func TestFetchVideoContentBufferedAndErrors(t *testing.T) {
	oversized := bytes.Repeat([]byte("e"), maxVideoErrorBodyBytes+4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/clip":
			_, _ = w.Write([]byte("MP4DATA"))
		case "/gone":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"video expired"}}`))
		case "/huge-error":
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write(oversized)
		}
	}))
	t.Cleanup(srv.Close)

	resp, bifrostErr := videoFetch(t, srv.URL+"/clip", false)
	if bifrostErr != nil {
		t.Fatalf("buffered fetch: %v", bifrostErr.Error)
	}
	// An absent vendor type becomes video/mp4; Go's server sniffs one here, so only presence is pinned.
	if string(resp.Content) != "MP4DATA" || resp.Body != nil || resp.ContentType == "" {
		t.Fatalf("buffered fetch = content %q body %v type %q", resp.Content, resp.Body != nil, resp.ContentType)
	}

	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	req.SetRequestURI(srv.URL + "/clip")
	noStreamingClient, bifrostErr := FetchVideoContent(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), VideoContentFetch{
		Request: req, Client: &fasthttp.Client{}, Stream: true, VideoID: "v",
	})
	if bifrostErr != nil || string(noStreamingClient.Content) != "MP4DATA" || noStreamingClient.Body != nil {
		t.Fatalf("a provider without a streaming client must buffer, got %+v %v", noStreamingClient, bifrostErr)
	}

	for _, stream := range []bool{false, true} {
		_, bifrostErr := videoFetch(t, srv.URL+"/gone", stream)
		if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusNotFound || !strings.Contains(bifrostErr.Error.Message, "video expired") {
			t.Fatalf("stream=%v: want the vendor's 404 and message, got %+v", stream, bifrostErr)
		}
	}
	_, bifrostErr = videoFetch(t, srv.URL+"/huge-error", true)
	if bifrostErr == nil || *bifrostErr.StatusCode != http.StatusBadGateway || len(bifrostErr.Error.Message) != maxVideoErrorBodyBytes {
		t.Fatalf("a streamed refusal is read to %d bytes, got %d", maxVideoErrorBodyBytes, len(bifrostErr.Error.Message))
	}
}

// TestFetchVideoContentFollowsRedirectsWhenStreaming covers Gemini's file download, which answers
// with a redirect to the bytes: the redirect is followed and the final body is the stream.
func TestFetchVideoContentFollowsRedirectsWhenStreaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/download" {
			http.Redirect(w, r, "/bytes", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("REDIRECTED-MP4"))
	}))
	t.Cleanup(srv.Close)

	for _, stream := range []bool{true, false} {
		req := fasthttp.AcquireRequest()
		req.SetRequestURI(srv.URL + "/download")
		req.Header.SetMethod(http.MethodGet)
		resp, bifrostErr := FetchVideoContent(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), VideoContentFetch{
			Request: req, Client: &fasthttp.Client{}, StreamingClient: BuildStreamingClient(&fasthttp.Client{}),
			Stream: stream, VideoID: "v:gemini", MaxRedirects: 5,
			ParseError: func(resp *fasthttp.Response) *schemas.BifrostError {
				return NewProviderAPIError(string(resp.Body()), nil, resp.StatusCode(), nil, nil)
			},
		})
		fasthttp.ReleaseRequest(req)
		if bifrostErr != nil {
			t.Fatalf("stream=%v: %+v", stream, bifrostErr.Error)
		}
		got := resp.Content
		if stream {
			if resp.Body == nil {
				t.Fatal("a streamed fetch through a redirect must still return Body")
			}
			var err error
			if got, err = io.ReadAll(resp.Body); err != nil || resp.Body.Close() != nil {
				t.Fatalf("read: %v", err)
			}
		}
		if string(got) != "REDIRECTED-MP4" || resp.ContentType != "video/mp4" {
			t.Fatalf("stream=%v: got %q type %q", stream, got, resp.ContentType)
		}
	}
}

// TestFetchVideoContentStalledRefusalTimesOut: a vendor that answers a streamed download with an
// error status and then stalls its body must not hold the request. The refusal's body is read
// under the stream idle timeout, like a file, and what arrived before the stall is still parsed.
func TestFetchVideoContentStalledRefusalTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream stalled"}}`))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	req.SetRequestURI(srv.URL + "/stalled")
	started := time.Now()
	_, bifrostErr := FetchVideoContent(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), VideoContentFetch{
		Request: req, Client: &fasthttp.Client{}, StreamingClient: BuildStreamingClient(&fasthttp.Client{}),
		Stream: true, VideoID: "v", StreamIdleTimeoutSeconds: 1,
		ParseError: func(resp *fasthttp.Response) *schemas.BifrostError {
			return NewProviderAPIError(string(resp.Body()), nil, resp.StatusCode(), nil, nil)
		},
	})
	if elapsed := time.Since(started); elapsed > 8*time.Second {
		t.Fatalf("a stalled refusal held the request for %s", elapsed)
	}
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadGateway || !strings.Contains(bifrostErr.Error.Message, "upstream stalled") {
		t.Fatalf("want the vendor's 502 with the part of its body that arrived, got %+v", bifrostErr)
	}
}

// TestFetchVideoContentCancelledRequestReturns: a request whose context ends while the vendor has
// not answered returns an error, having waited for the request goroutine before the response is
// released (the race detector is the assertion on the ordering).
func TestFetchVideoContentCancelledRequestReturns(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	for _, stream := range []bool{true, false} {
		base, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		req := fasthttp.AcquireRequest()
		req.SetRequestURI(srv.URL + "/never")
		// The buffered client carries a read timeout, as every provider client does: the wait
		// for the request goroutine ends when that goroutine's read does.
		_, bifrostErr := FetchVideoContent(schemas.NewBifrostContext(base, schemas.NoDeadline), VideoContentFetch{
			Request: req, Client: &fasthttp.Client{ReadTimeout: time.Second}, StreamingClient: BuildStreamingClient(&fasthttp.Client{ReadTimeout: time.Second}),
			Stream: stream, VideoID: "v",
			ParseError: func(resp *fasthttp.Response) *schemas.BifrostError {
				return NewBifrostOperationError("unexpected", nil)
			},
		})
		fasthttp.ReleaseRequest(req)
		cancel()
		if bifrostErr == nil {
			t.Fatalf("stream=%v: a cancelled download must fail", stream)
		}
	}
}

// TestFetchVideoContentDecodesContentEncodings: the buffered path decoded gzip, deflate, br and
// zstd, so the streamed path must not hand a caller compressed bytes as a video. gzip is decoded
// on the way through (no length is claimed, since Content-Length was the compressed size); any
// other encoding is read whole and decoded, and its decoded length is reported.
func TestFetchVideoContentDecodesContentEncodings(t *testing.T) {
	file := bytes.Repeat([]byte("MP4-FRAME-"), 4096)
	var gz, br bytes.Buffer
	gzw := gzip.NewWriter(&gz)
	_, _ = gzw.Write(file)
	_ = gzw.Close()
	brw := brotli.NewWriter(&br)
	_, _ = brw.Write(file)
	_ = brw.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		switch r.URL.Path {
		case "/gzip":
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(gz.Bytes())
		case "/br":
			w.Header().Set("Content-Encoding", "br")
			_, _ = w.Write(br.Bytes())
		}
	}))
	t.Cleanup(srv.Close)

	for path, wantLength := range map[string]int64{"/gzip": 0, "/br": int64(len(file))} {
		for _, stream := range []bool{true, false} {
			resp, bifrostErr := videoFetch(t, srv.URL+path, stream)
			if bifrostErr != nil {
				t.Fatalf("%s stream=%v: %+v", path, stream, bifrostErr.Error)
			}
			got := resp.Content
			if stream {
				if resp.Body == nil {
					t.Fatalf("%s: a streamed fetch returns Body", path)
				}
				var err error
				if got, err = io.ReadAll(resp.Body); err != nil || resp.Body.Close() != nil {
					t.Fatalf("%s: read: %v", path, err)
				}
				if resp.ContentLength != wantLength {
					t.Fatalf("%s: content length = %d, want %d", path, resp.ContentLength, wantLength)
				}
			}
			if !bytes.Equal(got, file) {
				t.Fatalf("%s stream=%v: got %d bytes that are not the decoded file (%d bytes)", path, stream, len(got), len(file))
			}
		}
	}
}

// TestNewVideoContentResponse: bytes a provider already holds are Content, or Body with their
// length when the request asked for a stream.
func TestNewVideoContentResponse(t *testing.T) {
	buffered := NewVideoContentResponse(&schemas.BifrostVideoDownloadRequest{}, "v:vertex", []byte("abc"), "video/mp4", 5*time.Millisecond)
	if string(buffered.Content) != "abc" || buffered.Body != nil || buffered.ExtraFields.Latency != 5 {
		t.Fatalf("buffered = %+v", buffered)
	}
	streamed := NewVideoContentResponse(&schemas.BifrostVideoDownloadRequest{Stream: true}, "v:vertex", []byte("abc"), "video/mp4", 0)
	if streamed.Content != nil || streamed.Body == nil || streamed.ContentLength != 3 {
		t.Fatalf("streamed = %+v", streamed)
	}
	if got, _ := io.ReadAll(streamed.Body); string(got) != "abc" {
		t.Fatalf("streamed body = %q", got)
	}
}

// TestVideoDownloadResponsesAreBuiltByTheSharedHelpers is the check that keeps BF20 a property
// of every provider: a VideoDownload that builds its response by hand ignores the request's
// Stream flag and holds the whole file. A new provider fails here until it returns through
// FetchVideoContent or NewVideoContentResponse.
func TestVideoDownloadResponsesAreBuiltByTheSharedHelpers(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "*", "*.go"))
	if err != nil || len(files) < 50 {
		t.Fatalf("provider sources not found: %d files, %v", len(files), err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") || filepath.Base(filepath.Dir(file)) == "utils" {
			continue
		}
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(source, []byte("schemas.BifrostVideoDownloadResponse{")) {
			t.Errorf("%s builds a BifrostVideoDownloadResponse by hand; return providerUtils.FetchVideoContent or providerUtils.NewVideoContentResponse so a streamed request is honored", file)
		}
	}
}

// TestAddVideoListFilters is contract C7 / BF15 at the query: vendor filters are added by their
// own names with every value, and never replace a parameter the provider set from a typed field.
func TestAddVideoListFilters(t *testing.T) {
	values := url.Values{}
	values.Set("limit", "2")
	AddVideoListFilters(values, map[string][]string{
		"start_time": {"2026-11-09T00:00:00Z"},
		"status":     {"SUCCEEDED", "FAILED"},
		"limit":      {"100"},
		"":           {"x"},
	})
	want := url.Values{"limit": {"2"}, "start_time": {"2026-11-09T00:00:00Z"}, "status": {"SUCCEEDED", "FAILED"}}
	if fmt.Sprint(values) != fmt.Sprint(want) {
		t.Fatalf("query = %v, want %v", values, want)
	}
}

// BenchmarkFetchVideoContent is the cost of one download of a 64 KiB file from a local vendor:
// "buffered" is the path every download took before BF20 (and the Go SDK's still), "streamed" the
// path /content takes now, read to its end and closed as the transport does.
func BenchmarkFetchVideoContent(b *testing.B) {
	file := bytes.Repeat([]byte("v"), 64<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(file)
	}))
	b.Cleanup(srv.Close)
	client := &fasthttp.Client{}
	streamingClient := BuildStreamingClient(&fasthttp.Client{})
	parse := func(resp *fasthttp.Response) *schemas.BifrostError { return NewBifrostOperationError("refused", nil) }
	sink := make([]byte, 32<<10)

	for _, stream := range []bool{false, true} {
		name := "buffered"
		if stream {
			name = "streamed"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(file)))
			for b.Loop() {
				req := fasthttp.AcquireRequest()
				req.SetRequestURI(srv.URL + "/clip")
				resp, bifrostErr := FetchVideoContent(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), VideoContentFetch{
					Request: req, Client: client, StreamingClient: streamingClient, Stream: stream, VideoID: "v:openai", ParseError: parse,
				})
				fasthttp.ReleaseRequest(req)
				if bifrostErr != nil {
					b.Fatal(bifrostErr.Error.Message)
				}
				if resp.Body != nil {
					if _, err := io.CopyBuffer(io.Discard, resp.Body, sink); err != nil {
						b.Fatal(err)
					}
					_ = resp.Body.Close()
				}
			}
		})
	}
}

// BenchmarkAddVideoListFilters is the cost the filter pass-through adds to building a video list
// query: nothing for a request with no filters, one map walk otherwise.
func BenchmarkAddVideoListFilters(b *testing.B) {
	filters := map[string][]string{"status": {"completed", "failed"}, "created_after": {"1762700000"}, "model": {"sora-2"}}
	for name, f := range map[string]map[string][]string{"none": nil, "three": filters} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				values := url.Values{"limit": {"2"}}
				AddVideoListFilters(values, f)
			}
		})
	}
}
