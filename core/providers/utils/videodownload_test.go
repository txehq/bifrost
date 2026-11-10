package utils

import (
	"bytes"
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

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// videoFetch is a FetchVideoContent call against a test server.
func videoFetch(t *testing.T, target string, stream bool) (*schemas.BifrostVideoDownloadResponse, *schemas.BifrostError) {
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
