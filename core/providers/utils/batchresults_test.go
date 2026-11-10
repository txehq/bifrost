package utils

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

func TestBatchResultsCursorRoundTrip(t *testing.T) {
	for _, cursor := range []BatchResultsCursor{{}, {Source: 0, Offset: 12345}, {Source: 1, Offset: 0}, {Source: 3, Offset: 1 << 40}} {
		encoded := cursor.Encode()
		got, err := DecodeBatchResultsCursor(&encoded)
		if err != nil || got != cursor {
			t.Fatalf("round trip of %+v: got %+v, err %v", cursor, got, err)
		}
	}
	if got, err := DecodeBatchResultsCursor(nil); err != nil || got != (BatchResultsCursor{}) {
		t.Fatalf("nil cursor: got %+v, err %v, want the start", got, err)
	}
	for _, bad := range []string{"not base64!", "djI6MDow" /* v2:0:0 */, "djE6eDow" /* v1:x:0 */, "djE6MDotMQ" /* v1:0:-1 */, "djE6MA" /* v1:0 */} {
		if _, err := DecodeBatchResultsCursor(&bad); err == nil {
			t.Fatalf("cursor %q decoded, want an error", bad)
		}
	}
}

func TestPageBatchResultItems(t *testing.T) {
	// Walk 7 items in pages of 3: [0,3) [3,6) [6,7).
	var after *string
	var pages [][2]int
	for {
		start, end, next, err := PageBatchResultItems(7, after, 3)
		if err != nil {
			t.Fatalf("PageBatchResultItems: %v", err)
		}
		pages = append(pages, [2]int{start, end})
		if next == nil {
			break
		}
		after = next
	}
	if fmt.Sprint(pages) != "[[0 3] [3 6] [6 7]]" {
		t.Fatalf("pages = %v, want [[0 3] [3 6] [6 7]]", pages)
	}
	// No limit is everything, with no cursor.
	if start, end, next, err := PageBatchResultItems(7, nil, 0); err != nil || start != 0 || end != 7 || next != nil {
		t.Fatalf("no limit: [%d,%d) next=%v err=%v, want [0,7) and no cursor", start, end, next, err)
	}
	// A page that ends exactly at the end has no next cursor.
	if _, end, next, _ := PageBatchResultItems(6, nil, 6); end != 6 || next != nil {
		t.Fatalf("exact fit: end=%d next=%v, want 6 and no cursor", end, next)
	}
	// A cursor past the end is an empty page, not a panic.
	past := BatchResultsCursor{Offset: 99}.Encode()
	if start, end, next, err := PageBatchResultItems(7, &past, 3); err != nil || start != 7 || end != 7 || next != nil {
		t.Fatalf("past the end: [%d,%d) next=%v err=%v, want an empty page", start, end, next, err)
	}
}

func TestReadJSONLPage(t *testing.T) {
	data := "{\"a\":1}\n\n{\"a\":2}\r\n{\"a\":3}\n   \n{\"a\":4}"
	collect := func(r io.Reader, limit int) ([]string, int64, bool) {
		var lines []string
		consumed, more, err := ReadJSONLPage(r, limit, func(line []byte) { lines = append(lines, string(line)) })
		if err != nil {
			t.Fatalf("ReadJSONLPage: %v", err)
		}
		return lines, consumed, more
	}

	lines, consumed, more := collect(strings.NewReader(data), 0)
	if strings.Join(lines, "|") != `{"a":1}|{"a":2}|{"a":3}|{"a":4}` || more || consumed != int64(len(data)) {
		t.Fatalf("whole file: lines=%v consumed=%d more=%v", lines, consumed, more)
	}

	// Paging by byte offset returns every line once, whatever the page size, and blank
	// lines never produce an empty trailing page.
	for limit := 1; limit <= 5; limit++ {
		var all []string
		offset := int64(0)
		for page := 0; ; page++ {
			if page > 10 {
				t.Fatalf("limit %d: paging did not terminate", limit)
			}
			lines, consumed, more := collect(strings.NewReader(data[offset:]), limit)
			if len(lines) == 0 {
				t.Fatalf("limit %d: empty page at offset %d", limit, offset)
			}
			all = append(all, lines...)
			offset += consumed
			if !more {
				break
			}
		}
		if strings.Join(all, "|") != `{"a":1}|{"a":2}|{"a":3}|{"a":4}` {
			t.Fatalf("limit %d: lines = %v", limit, all)
		}
	}
}

// memorySource serves data as a result source and records the offsets it was opened at.
func memorySource(data string, opened *[]int64) BatchResultsSource {
	return func(offset int64) (*ResultStream, *schemas.BifrostError) {
		*opened = append(*opened, offset)
		return &ResultStream{Reader: strings.NewReader(data[min(offset, int64(len(data))):])}, nil
	}
}

func TestReadBatchResultsPageAcrossSources(t *testing.T) {
	output := "o1\no2\no3\n"
	errors := "e1\ne2\n"
	type line struct {
		source int
		text   string
	}
	walk := func(limit int, sources func() []BatchResultsSource) ([]line, int) {
		var got []line
		var after *string
		pages := 0
		for {
			pages++
			if pages > 20 {
				t.Fatalf("limit %d: paging did not terminate", limit)
			}
			page, bifrostErr := ReadBatchResultsPage(sources(), after, limit, func(source int, l []byte) {
				got = append(got, line{source, string(l)})
			})
			if bifrostErr != nil {
				t.Fatalf("ReadBatchResultsPage: %v", bifrostErr.Error.Message)
			}
			if page.HasMore != (page.NextCursor != nil) {
				t.Fatalf("has_more=%v but next_cursor=%v", page.HasMore, page.NextCursor)
			}
			if !page.HasMore {
				return got, pages
			}
			after = page.NextCursor
		}
	}
	want := "[{0 o1} {0 o2} {0 o3} {1 e1} {1 e2}]"

	for limit := 0; limit <= 6; limit++ {
		var opened []int64
		got, _ := walk(limit, func() []BatchResultsSource {
			return []BatchResultsSource{memorySource(output, &opened), memorySource(errors, &opened)}
		})
		if fmt.Sprint(got) != want {
			t.Fatalf("limit %d: lines = %v, want %s", limit, got, want)
		}
	}

	// A batch with no error file (nil stream) and one with no output file both page.
	absent := func(int64) (*ResultStream, *schemas.BifrostError) { return nil, nil }
	var opened []int64
	got, _ := walk(2, func() []BatchResultsSource { return []BatchResultsSource{memorySource(output, &opened), absent} })
	if fmt.Sprint(got) != "[{0 o1} {0 o2} {0 o3}]" {
		t.Fatalf("no error file: lines = %v", got)
	}
	got, _ = walk(1, func() []BatchResultsSource { return []BatchResultsSource{absent, memorySource(errors, &opened)} })
	if fmt.Sprint(got) != "[{1 e1} {1 e2}]" {
		t.Fatalf("no output file: lines = %v", got)
	}

	// A cursor this gateway did not issue is a 400 naming the parameter.
	bad := "garbage"
	_, bifrostErr := ReadBatchResultsPage([]BatchResultsSource{absent}, &bad, 2, func(int, []byte) {})
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 || bifrostErr.Error.Param != "after" {
		t.Fatalf("bad cursor error = %+v, want a 400 with param after", bifrostErr)
	}

	// A source that fails to open fails the page with the provider's error.
	failing := func(int64) (*ResultStream, *schemas.BifrostError) {
		return nil, NewProviderAPIError("file not found", nil, 404, nil, nil)
	}
	_, bifrostErr = ReadBatchResultsPage([]BatchResultsSource{failing}, nil, 2, func(int, []byte) {})
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 404 {
		t.Fatalf("failing source error = %+v, want the 404", bifrostErr)
	}
}

// resultFileServer serves a JSONL file of lineCount lines, each lineSize bytes, generated as
// it is written so the test holds no copy of it. It records how many body bytes it wrote and
// the Range headers it saw. honourRange selects a server that answers byte ranges with 206.
type resultFileServer struct {
	*httptest.Server
	bytesWritten atomic.Int64
	rangeHeaders atomic.Value // []string
}

func resultLine(i, lineSize int) []byte {
	prefix := fmt.Sprintf(`{"custom_id":"line-%d","pad":"`, i)
	suffix := "\"}\n"
	return []byte(prefix + strings.Repeat("x", lineSize-len(prefix)-len(suffix)) + suffix)
}

func newResultFileServer(t *testing.T, lineCount, lineSize int, honourRange bool) *resultFileServer {
	t.Helper()
	s := &resultFileServer{}
	s.rangeHeaders.Store([]string{})
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"message":"No such file"}}`)
			return
		}
		rangeHeader := r.Header.Get("Range")
		s.rangeHeaders.Store(append(s.rangeHeaders.Load().([]string), rangeHeader))
		total := int64(lineCount) * int64(lineSize)
		start := int64(0)
		if honourRange && rangeHeader != "" {
			parsed, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(rangeHeader, "bytes="), "-"), 10, 64)
			if err != nil {
				t.Errorf("unparseable Range %q", rangeHeader)
			}
			if parsed >= total {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			start = parsed
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, total-1, total))
			w.Header().Set("Content-Length", strconv.FormatInt(total-start, 10))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
		}
		for i := int(start / int64(lineSize)); i < lineCount; i++ {
			line := resultLine(i, lineSize)
			if lineStart := int64(i) * int64(lineSize); lineStart < start {
				line = line[start-lineStart:]
			}
			n, err := w.Write(line)
			s.bytesWritten.Add(int64(n))
			if err != nil {
				return // the client dropped the connection: the page was full
			}
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func openTestResultStream(t *testing.T, client *fasthttp.Client, url string, offset int64) (*ResultStream, *schemas.BifrostError) {
	t.Helper()
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	return OpenResultStream(ctx, client, offset, func(req *fasthttp.Request) {
		req.SetRequestURI(url)
		req.Header.SetMethod(http.MethodGet)
	}, func(resp *fasthttp.Response) *schemas.BifrostError {
		return NewProviderAPIError(string(resp.Body()), nil, resp.StatusCode(), nil, nil)
	})
}

// A page of a large result file is read from a stream and the download is dropped once the
// page is full: the server writes a small fraction of the file, with or without byte-range
// support, and every line still comes back exactly once across the pages.
func TestOpenResultStreamReadsOnePageOfALargeFile(t *testing.T) {
	const lineCount, lineSize = 200_000, 1024 // a 200 MB file
	const pageLines = 100
	client := BuildStreamingClient(&fasthttp.Client{})

	for _, honourRange := range []bool{true, false} {
		t.Run(fmt.Sprintf("server honours Range=%v", honourRange), func(t *testing.T) {
			server := newResultFileServer(t, lineCount, lineSize, honourRange)
			offset := int64(0)
			next := 0
			// Three pages from the start, then one from deep in the file.
			for _, startLine := range []int{0, pageLines, 2 * pageLines, 150_000} {
				if startLine == 150_000 {
					offset, next = int64(startLine)*lineSize, startLine
				}
				before := server.bytesWritten.Load()
				stream, bifrostErr := openTestResultStream(t, client, server.URL+"/file", offset)
				if bifrostErr != nil {
					t.Fatalf("OpenResultStream: %v", bifrostErr.Error.Message)
				}
				consumed, more, err := ReadJSONLPage(stream.Reader, pageLines, func(line []byte) {
					if want := bytes.TrimSpace(resultLine(next, lineSize)); !bytes.Equal(line, want) {
						t.Fatalf("line %d differs: got %.60q", next, line)
					}
					next++
				})
				stream.Close()
				if err != nil || !more || consumed != pageLines*lineSize {
					t.Fatalf("page at line %d: consumed=%d more=%v err=%v", startLine, consumed, more, err)
				}
				offset += consumed

				// Without Range support the bytes before the offset are transferred and
				// discarded; they are never held. With it, only about one page is sent.
				written := server.bytesWritten.Load() - before
				budget := int64(pageLines*lineSize) + 8<<20 // the page plus socket and reader buffers
				if !honourRange {
					budget += int64(startLine) * lineSize
				}
				if written > budget {
					t.Fatalf("page at line %d: server wrote %d bytes, want at most %d of the %d-byte file", startLine, written, budget, lineCount*lineSize)
				}
			}
			ranges := server.rangeHeaders.Load().([]string)
			if ranges[0] != "" || ranges[1] != fmt.Sprintf("bytes=%d-", pageLines*lineSize) {
				t.Fatalf("Range headers = %v, want none for the first page and bytes=<offset>- after", ranges)
			}
		})
	}
}

func TestOpenResultStreamEdges(t *testing.T) {
	const lineCount, lineSize = 10, 64
	client := BuildStreamingClient(&fasthttp.Client{})

	for _, honourRange := range []bool{true, false} {
		server := newResultFileServer(t, lineCount, lineSize, honourRange)

		// An offset at or past the end of the file is an exhausted source, not an error.
		for _, offset := range []int64{lineCount * lineSize, lineCount*lineSize + 500} {
			stream, bifrostErr := openTestResultStream(t, client, server.URL+"/file", offset)
			if bifrostErr != nil {
				t.Fatalf("honourRange=%v offset=%d: %v", honourRange, offset, bifrostErr.Error.Message)
			}
			rest, _ := io.ReadAll(stream.Reader)
			stream.Close()
			if len(rest) != 0 {
				t.Fatalf("honourRange=%v offset=%d: read %d bytes past the end", honourRange, offset, len(rest))
			}
		}

		// A stream read to its end returns the last lines and closes cleanly.
		stream, bifrostErr := openTestResultStream(t, client, server.URL+"/file", 8*lineSize)
		if bifrostErr != nil {
			t.Fatalf("honourRange=%v: %v", honourRange, bifrostErr.Error.Message)
		}
		var ids []string
		_, more, err := ReadJSONLPage(stream.Reader, 0, func(line []byte) { ids = append(ids, string(line[:22])) })
		stream.Close()
		stream.Close() // idempotent
		if err != nil || more || len(ids) != 2 || !strings.Contains(ids[0], "line-8") || !strings.Contains(ids[1], "line-9") {
			t.Fatalf("honourRange=%v: tail lines = %v more=%v err=%v", honourRange, ids, more, err)
		}

		// A vendor error keeps its status and body.
		_, bifrostErr = openTestResultStream(t, client, server.URL+"/missing", 0)
		if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 404 || !strings.Contains(bifrostErr.Error.Message, "No such file") {
			t.Fatalf("honourRange=%v: error = %+v, want the vendor's 404 and body", honourRange, bifrostErr)
		}
	}
}

// A cancelled request ends a download that is blocked on the upstream.
func TestOpenResultStreamStopsOnContextCancel(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		fmt.Fprint(w, "{\"custom_id\":\"line-0\"}\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)

	parent, cancel := context.WithCancel(context.Background())
	ctx := schemas.NewBifrostContext(parent, schemas.NoDeadline)
	stream, bifrostErr := OpenResultStream(ctx, BuildStreamingClient(&fasthttp.Client{}), 0, func(req *fasthttp.Request) {
		req.SetRequestURI(server.URL)
		req.Header.SetMethod(http.MethodGet)
	}, func(resp *fasthttp.Response) *schemas.BifrostError {
		return NewProviderAPIError("unexpected", nil, resp.StatusCode(), nil, nil)
	})
	if bifrostErr != nil {
		t.Fatalf("OpenResultStream: %v", bifrostErr.Error.Message)
	}
	defer stream.Close()

	lines := 0
	done := make(chan error, 1)
	go func() {
		_, _, err := ReadJSONLPage(stream.Reader, 0, func([]byte) { lines++; cancel() })
		done <- err
	}()
	if err := <-done; err == nil {
		t.Fatalf("read returned no error after the context was cancelled (lines=%d)", lines)
	}
}

// A vendor that answers with an error status and then stalls its body cannot hold the
// request: the watch that ends a download on cancellation covers the error body too.
func TestOpenResultStreamErrorBodyStallEndsOnContextCancel(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"message":"partial`)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)

	parent, cancel := context.WithCancel(context.Background())
	ctx := schemas.NewBifrostContext(parent, schemas.NoDeadline)
	type outcome struct {
		stream *ResultStream
		err    *schemas.BifrostError
	}
	done := make(chan outcome, 1)
	go func() {
		stream, bifrostErr := OpenResultStream(ctx, BuildStreamingClient(&fasthttp.Client{}), 0, func(req *fasthttp.Request) {
			req.SetRequestURI(server.URL)
			req.Header.SetMethod(http.MethodGet)
		}, func(resp *fasthttp.Response) *schemas.BifrostError {
			return NewProviderAPIError("vendor error: "+string(resp.Body()), nil, resp.StatusCode(), nil, nil)
		})
		done <- outcome{stream, bifrostErr}
	}()

	select {
	case got := <-done:
		t.Fatalf("returned before the context was cancelled, with the body still stalled: %+v", got.err)
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	select {
	case got := <-done:
		if got.err == nil || got.err.StatusCode == nil || *got.err.StatusCode != http.StatusInternalServerError {
			t.Fatalf("error = %+v, want the vendor's 500", got.err)
		}
		if !strings.Contains(got.err.Error.Message, "partial") {
			t.Fatalf("error message %q lost the part of the body that did arrive", got.err.Error.Message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OpenResultStream did not return after the context was cancelled")
	}
}

// Time spent waiting for a result file's body is upstream latency, like the wait for its
// headers: the body is where a large download spends its time.
func TestOpenResultStreamCountsBodyReadsAsUpstreamLatency(t *testing.T) {
	const bodyDelay = 150 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "23")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(bodyDelay)
		fmt.Fprint(w, "{\"custom_id\":\"line-0\"}\n")
	}))
	defer server.Close()

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.ResetUpstreamLatency()
	stream, bifrostErr := OpenResultStream(ctx, BuildStreamingClient(&fasthttp.Client{}), 0, func(req *fasthttp.Request) {
		req.SetRequestURI(server.URL)
		req.Header.SetMethod(http.MethodGet)
	}, func(resp *fasthttp.Response) *schemas.BifrostError {
		return NewProviderAPIError("unexpected", nil, resp.StatusCode(), nil, nil)
	})
	if bifrostErr != nil {
		t.Fatalf("OpenResultStream: %v", bifrostErr.Error.Message)
	}
	afterHeaders, _ := schemas.GetUpstreamLatency(ctx)
	lines := 0
	if _, _, err := ReadJSONLPage(stream.Reader, 0, func([]byte) { lines++ }); err != nil || lines != 1 {
		t.Fatalf("read %d lines, err %v", lines, err)
	}
	stream.Close()
	total, ok := schemas.GetUpstreamLatency(ctx)
	if waited := total - afterHeaders; !ok || waited < bodyDelay/2 {
		t.Fatalf("upstream latency grew by %v while the body took %v to arrive", waited, bodyDelay)
	}
}
