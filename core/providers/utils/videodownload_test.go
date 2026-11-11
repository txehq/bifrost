package utils

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/capsohq/bifrost/core/network/proxytest"
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

// TestFetchVideoContentStalledGzipHeaderTimesOut: a vendor that announces a gzip body and then
// stalls before sending the gzip header must not hold the request. Building the gzip decoder
// reads that header from the socket, so the idle timeout has to be on the stream first; the
// stall then surfaces as a read error on the body instead of a hang.
func TestFetchVideoContentStalledGzipHeaderTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	req.SetRequestURI(srv.URL + "/stalled-gzip")
	started := time.Now()
	resp, bifrostErr := FetchVideoContent(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), VideoContentFetch{
		Request: req, Client: &fasthttp.Client{}, StreamingClient: BuildStreamingClient(&fasthttp.Client{}),
		Stream: true, VideoID: "v", StreamIdleTimeoutSeconds: 1,
		ParseError: func(resp *fasthttp.Response) *schemas.BifrostError {
			return NewBifrostOperationError("unexpected", nil)
		},
	})
	if bifrostErr == nil {
		_, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err == nil {
			t.Fatal("a body whose gzip header never arrived must fail to read")
		}
	}
	if elapsed := time.Since(started); elapsed > 8*time.Second {
		t.Fatalf("a stalled gzip header held the request for %s", elapsed)
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
	// A chain: Brotli applied first, gzip on top of it.
	var chained bytes.Buffer
	chainw := gzip.NewWriter(&chained)
	_, _ = chainw.Write(br.Bytes())
	_ = chainw.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		switch r.URL.Path {
		case "/gzip":
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(gz.Bytes())
		case "/br":
			w.Header().Set("Content-Encoding", "br")
			_, _ = w.Write(br.Bytes())
		case "/chain":
			w.Header().Set("Content-Encoding", "br, gzip")
			_, _ = w.Write(chained.Bytes())
		}
	}))
	t.Cleanup(srv.Close)

	for path, wantLength := range map[string]int64{"/gzip": 0, "/br": int64(len(file)), "/chain": int64(len(file))} {
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

// baseURLVideoDownloads lists the FetchVideoContent calls that may omit VendorURL, by file: how
// many there are and why their address is not the vendor's to choose. Every other call must
// set VendorURL, which sends it through the SSRF-hardened media fetcher.
var baseURLVideoDownloads = map[string]struct {
	calls  int
	reason string
}{
	"openai/openai.go":         {1, "the address is the provider's base URL plus /videos/{id}/content"},
	"azure/azure.go":           {1, "the address is the deployment endpoint plus /videos/{id}/content"},
	"volcengine/volcengine.go": {1, "the address is the provider's base URL plus the videos path"},
	"vertex/vertex.go":         {1, "the gs:// object is read from the Cloud Storage API base, never from an address in the URI"},
}

// videoDownloadTransportCalls are the calls that send a request on a provider's own client. A
// function that downloads a video has no use for one: its fetch goes through FetchVideoContent.
var videoDownloadTransportCalls = map[string]bool{
	"MakeRequestWithContext": true, "MakeRequestWithContextFollowRedirects": true, "DoStreamingRequest": true,
	"Do": true, "DoRedirects": true, "DoTimeout": true, "DoDeadline": true,
	"FetchMediaURL": true, "FetchMediaURLWithHeader": true, "FetchMediaStream": true, "FetchAndEncodeURL": true,
}

// TestVideoDownloadsOfVendorNamedURLsUseTheHardenedFetcher is the check that keeps TXE-7036 a
// property of every provider instead of the seven it was written for. A vendor's task response
// names where its file is, and a provider client's dialer admits loopback and private
// addresses, so a download of such an address on that client lets the vendor (or whoever
// answers in its place) read the gateway's network. The check fails a provider source that
//   - calls FetchVideoContent without VendorURL and is not listed in baseURLVideoDownloads, or
//   - sends a request itself from a function that downloads a video, instead of handing the
//     fetch to FetchVideoContent.
func TestVideoDownloadsOfVendorNamedURLsUseTheHardenedFetcher(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "*", "*.go"))
	if err != nil || len(files) < 50 {
		t.Fatalf("provider sources not found: %d files, %v", len(files), err)
	}
	baseURLCalls := map[string]int{}
	vendorURLCalls := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") || filepath.Base(filepath.Dir(file)) == "utils" {
			continue
		}
		name := filepath.ToSlash(strings.TrimPrefix(file, ".."+string(filepath.Separator)))
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			lower := strings.ToLower(fn.Name.Name)
			downloadsVideo := strings.Contains(lower, "video") && strings.Contains(lower, "download")
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if downloadsVideo && videoDownloadTransportCalls[selector.Sel.Name] {
					t.Errorf("%s: %s calls %s itself; hand the fetch to providerUtils.FetchVideoContent (with VendorURL when the vendor names the address)", name, fn.Name.Name, selector.Sel.Name)
				}
				if selector.Sel.Name != "FetchVideoContent" || len(call.Args) != 2 {
					return true
				}
				literal, ok := call.Args[1].(*ast.CompositeLit)
				if !ok {
					t.Errorf("%s: %s passes FetchVideoContent something other than a VideoContentFetch literal, so this check cannot see whether VendorURL is set", name, fn.Name.Name)
					return true
				}
				for _, element := range literal.Elts {
					if field, ok := element.(*ast.KeyValueExpr); ok {
						if key, ok := field.Key.(*ast.Ident); ok && key.Name == "VendorURL" {
							vendorURLCalls++
							return true
						}
					}
				}
				baseURLCalls[name]++
				return true
			})
		}
	}
	for name, calls := range baseURLCalls {
		if allowed := baseURLVideoDownloads[name]; calls != allowed.calls {
			t.Errorf("%s has %d FetchVideoContent calls without VendorURL, %d listed in baseURLVideoDownloads. Set VendorURL when the address comes from the vendor's response; list the call with its reason when the address is built from the provider's base URL", name, calls, allowed.calls)
		}
	}
	for name, allowed := range baseURLVideoDownloads {
		if baseURLCalls[name] != allowed.calls {
			t.Errorf("baseURLVideoDownloads lists %d calls in %s (%s) and the source has %d; keep the list exact", allowed.calls, name, allowed.reason, baseURLCalls[name])
		}
	}
	if vendorURLCalls < 7 {
		t.Fatalf("found %d FetchVideoContent calls with VendorURL, want the 7 of TXE-7036 at least; the scan is not seeing the provider sources", vendorURLCalls)
	}
}

// vendorURLFetch is a FetchVideoContent call for an address the vendor named, with a provider
// proxy that tunnels every address to handler.
func vendorURLFetch(t testing.TB, handler http.HandlerFunc, f VideoContentFetch) (*schemas.BifrostVideoDownloadResponse, *schemas.BifrostError, *proxytest.OriginTunnel) {
	t.Helper()
	proxy := proxytest.NewOriginTunnel(t, handler)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyProviderProxyConfig, &schemas.ProxyConfig{Type: schemas.HTTPProxy, URL: schemas.NewSecretVar(proxy.URL)})
	f.VideoID = "task_1"
	if f.ParseError == nil {
		f.ParseError = func(resp *fasthttp.Response) *schemas.BifrostError {
			return NewBifrostOperationError(fmt.Sprintf("failed to download video: HTTP %d", resp.StatusCode()), nil)
		}
	}
	resp, bifrostErr := FetchVideoContent(ctx, f)
	return resp, bifrostErr, proxy
}

// TestFetchVideoContentVendorURL covers the route TXE-7036 adds to FetchVideoContent: an address
// the vendor named is fetched by the SSRF-hardened media fetcher, streamed or held as the
// request asks, with the request's headers; a private address is refused whichever is asked;
// and the provider's own origin stays on the provider's client.
func TestFetchVideoContentVendorURL(t *testing.T) {
	file := bytes.Repeat([]byte("v"), 300<<10)
	var gotKey atomic.Value
	handler := func(w http.ResponseWriter, r *http.Request) {
		gotKey.Store(r.Header.Get("X-Goog-Api-Key"))
		if r.URL.Path == "/gone.mp4" {
			http.Error(w, "AccessDenied", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/gzip.mp4" || r.URL.Path == "/br.mp4" {
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Content-Encoding", map[string]string{"/gzip.mp4": "gzip", "/br.mp4": "br"}[r.URL.Path])
			gz := gzip.NewWriter(w)
			_, _ = gz.Write(file)
			_ = gz.Close()
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(file)))
		w.Header().Set("X-Request-Id", "file-host-request-1")
		_, _ = w.Write(file)
	}
	request := func() *fasthttp.Request {
		req := fasthttp.AcquireRequest()
		t.Cleanup(func() { fasthttp.ReleaseRequest(req) })
		req.Header.SetMethod(http.MethodGet)
		req.Header.Set("x-goog-api-key", "vendor-key")
		return req
	}
	pinned := func(upstream string) string { return VideoContentTypeOr(upstream, "video/mp4") }

	t.Run("streamed", func(t *testing.T) {
		resp, bifrostErr, _ := vendorURLFetch(t, handler, VideoContentFetch{VendorURL: "http://203.0.113.10/clip.mp4?sig=abc", Request: request(), Stream: true, ContentType: pinned})
		if bifrostErr != nil {
			t.Fatalf("FetchVideoContent: %v", bifrostErr.Error)
		}
		defer resp.Body.Close()
		got, err := io.ReadAll(resp.Body)
		if err != nil || !bytes.Equal(got, file) || resp.Content != nil {
			t.Fatalf("read %d bytes, %v, content %d bytes", len(got), err, len(resp.Content))
		}
		if resp.ContentLength != int64(len(file)) || resp.ContentType != "video/mp4" || resp.VideoID != "task_1" {
			t.Fatalf("length %d type %q id %q", resp.ContentLength, resp.ContentType, resp.VideoID)
		}
		if gotKey.Load() != "vendor-key" {
			t.Fatalf("the vendor's host got key %q, want the request's header", gotKey.Load())
		}
		if got := resp.ExtraFields.ProviderResponseHeaders["X-Request-Id"]; got != "file-host-request-1" {
			t.Fatalf("the file host's response headers were dropped: %v", resp.ExtraFields.ProviderResponseHeaders)
		}
	})

	t.Run("held", func(t *testing.T) {
		resp, bifrostErr, _ := vendorURLFetch(t, handler, VideoContentFetch{VendorURL: "http://203.0.113.10/clip.mp4", Request: request()})
		if bifrostErr != nil {
			t.Fatalf("FetchVideoContent: %v", bifrostErr.Error)
		}
		if resp.Body != nil || !bytes.Equal(resp.Content, file) || resp.ContentType != "application/octet-stream" {
			t.Fatalf("body %v, %d bytes, type %q", resp.Body != nil, len(resp.Content), resp.ContentType)
		}
		if got := resp.ExtraFields.ProviderResponseHeaders["X-Request-Id"]; got != "file-host-request-1" {
			t.Fatalf("the file host's response headers were dropped: %v", resp.ExtraFields.ProviderResponseHeaders)
		}
		if gotKey.Load() != "vendor-key" {
			t.Fatalf("the vendor's host got key %q, want the request's header", gotKey.Load())
		}
	})

	for _, stream := range []bool{true, false} {
		// An encoded file is decoded whichever way it is asked for, as the provider-client path
		// decodes it; an encoding the fetcher cannot decode is refused, not passed on encoded.
		t.Run(fmt.Sprintf("gzip decoded stream=%v", stream), func(t *testing.T) {
			resp, bifrostErr, _ := vendorURLFetch(t, handler, VideoContentFetch{VendorURL: "http://203.0.113.10/gzip.mp4", Stream: stream})
			if bifrostErr != nil {
				t.Fatalf("FetchVideoContent: %v", bifrostErr.Error)
			}
			got := resp.Content
			if resp.Body != nil {
				got, _ = io.ReadAll(resp.Body)
				_ = resp.Body.Close()
			}
			if !bytes.Equal(got, file) {
				t.Fatalf("got %d bytes, want the %d decoded ones", len(got), len(file))
			}
		})
		t.Run(fmt.Sprintf("other encoding refused stream=%v", stream), func(t *testing.T) {
			_, bifrostErr, _ := vendorURLFetch(t, handler, VideoContentFetch{VendorURL: "http://203.0.113.10/br.mp4", Stream: stream})
			if bifrostErr == nil || bifrostErr.Error.Error == nil || !strings.Contains(bifrostErr.Error.Error.Error(), "Content-Encoding") {
				t.Fatalf("got %+v, want an unsupported Content-Encoding error", bifrostErr)
			}
		})
		t.Run(fmt.Sprintf("refusal stream=%v", stream), func(t *testing.T) {
			_, bifrostErr, _ := vendorURLFetch(t, handler, VideoContentFetch{VendorURL: "http://203.0.113.10/gone.mp4?sig=deadbeefcafe", Stream: stream})
			if bifrostErr == nil || bifrostErr.Error.Message != "failed to download video: HTTP 403" {
				t.Fatalf("got %+v, want the provider's ParseError for 403", bifrostErr)
			}
		})
		for name, target := range map[string]string{
			"loopback": "http://127.0.0.1:1/clip.mp4?sig=deadbeefcafe",
			"private":  "http://10.0.0.5/clip.mp4?sig=deadbeefcafe",
			"metadata": "http://169.254.169.254/latest/meta-data/?sig=deadbeefcafe",
			"scheme":   "file:///etc/passwd?sig=deadbeefcafe",
		} {
			t.Run(fmt.Sprintf("%s refused stream=%v", name, stream), func(t *testing.T) {
				// The provider's clients are given and its base URL is another host: they must
				// not be used for an address that is not on it.
				_, bifrostErr, proxy := vendorURLFetch(t, handler, VideoContentFetch{
					VendorURL: target, Stream: stream, BaseURL: "https://api.vendor.example/v1",
					Client: &fasthttp.Client{}, StreamingClient: BuildStreamingClient(&fasthttp.Client{}),
				})
				if bifrostErr == nil {
					t.Fatal("the download was not refused")
				}
				message := bifrostErr.Error.Message
				if bifrostErr.Error.Error != nil {
					message += " " + bifrostErr.Error.Error.Error()
				}
				if strings.Contains(message, "deadbeefcafe") {
					t.Fatalf("the error leaks the signed query: %s", message)
				}
				if n := proxy.Connects(); n != 0 {
					t.Fatalf("the refused address reached the proxy %d times", n)
				}
			})
		}
	}

	t.Run("the provider's own origin stays on its client", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(handler))
		t.Cleanup(srv.Close)
		for _, stream := range []bool{true, false} {
			req := request()
			req.SetRequestURI(srv.URL + "/files/clip.mp4")
			resp, bifrostErr, proxy := vendorURLFetch(t, handler, VideoContentFetch{
				VendorURL: srv.URL + "/files/clip.mp4", BaseURL: srv.URL + "/v1beta", Request: req, Stream: stream,
				Client: &fasthttp.Client{}, StreamingClient: BuildStreamingClient(&fasthttp.Client{}),
			})
			if bifrostErr != nil {
				t.Fatalf("stream=%v: a loopback address on the provider's own origin was refused: %v", stream, bifrostErr.Error)
			}
			got := resp.Content
			if resp.Body != nil {
				got, _ = io.ReadAll(resp.Body)
				_ = resp.Body.Close()
			}
			if !bytes.Equal(got, file) {
				t.Fatalf("stream=%v: read %d bytes", stream, len(got))
			}
			if n := proxy.Connects(); n != 0 {
				t.Fatalf("stream=%v: the provider-origin download went through the media fetcher's proxy path", stream)
			}
		}
		// Another port on the same host is another origin.
		if !sameOrigin("https://api.vendor.example/v1", "https://API.vendor.example:443/files/x") || sameOrigin(srv.URL, "http://127.0.0.1:1/x") || sameOrigin("http://a.example/x", "https://a.example/x") || sameOrigin("not a url", "not a url") {
			t.Fatal("sameOrigin compares scheme, host and port")
		}
	})
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
