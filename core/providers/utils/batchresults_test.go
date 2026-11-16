package utils

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/internal/testtime"

	"github.com/capsohq/bifrost/core/network"
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
	// A limit near the integer maximum returns what remains; it does not overflow.
	mid := BatchResultsCursor{Offset: 3}.Encode()
	if start, end, next, err := PageBatchResultItems(7, &mid, int(^uint(0)>>1)); err != nil || start != 3 || end != 7 || next != nil {
		t.Fatalf("huge limit: [%d,%d) next=%v err=%v, want [3,7) and no cursor", start, end, next, err)
	}
	// A cursor past the end is an empty page, not a panic.
	past := BatchResultsCursor{Offset: 99}.Encode()
	if start, end, next, err := PageBatchResultItems(7, &past, 3); err != nil || start != 7 || end != 7 || next != nil {
		t.Fatalf("past the end: [%d,%d) next=%v err=%v, want an empty page", start, end, next, err)
	}
}

// readAllLines is readJSONLPage with no byte bound, for the tests of line splitting alone.
func readAllLines(r io.Reader, limit int, onLine func(line []byte)) (int64, bool, error) {
	consumed, _, more, err := readJSONLPage(r, limit, math.MaxInt64, math.MaxInt64, onLine, func(size int64, _ []byte) {
		panic(fmt.Sprintf("a %d-byte line was too large with no bound", size))
	})
	return consumed, more, err
}

// readPage is ReadBatchResultsPage for the tests in which every line fits the bound: a line
// reported too large to read fails the test (TXE-7526 has its own tests).
func readPage(sources []BatchResultsSource, request *schemas.BifrostBatchResultsRequest, onLine func(source int, line []byte)) (*BatchResultsPage, *schemas.BifrostError) {
	return ReadBatchResultsPage(sources, request, onLine, func(source int, big BatchOversizedLine) {
		panic(fmt.Sprintf("source %d: a %d-byte line was too large", source, big.Size))
	})
}

func TestReadJSONLPage(t *testing.T) {
	data := "{\"a\":1}\n\n{\"a\":2}\r\n{\"a\":3}\n   \n{\"a\":4}"
	collect := func(r io.Reader, limit int) ([]string, int64, bool) {
		var lines []string
		consumed, more, err := readAllLines(r, limit, func(line []byte) { lines = append(lines, string(line)) })
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
		return &ResultStream{reader: strings.NewReader(data[min(offset, int64(len(data))):])}, nil
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
			page, bifrostErr := readPage(sources(), &schemas.BifrostBatchResultsRequest{After: after, Limit: limit}, func(source int, l []byte) {
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
	_, bifrostErr := readPage([]BatchResultsSource{absent}, &schemas.BifrostBatchResultsRequest{After: &bad, Limit: 2}, func(int, []byte) {})
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 || bifrostErr.Error.Param != "after" {
		t.Fatalf("bad cursor error = %+v, want a 400 with param after", bifrostErr)
	}

	// A source that fails to open fails the page with the provider's error.
	failing := func(int64) (*ResultStream, *schemas.BifrostError) {
		return nil, NewProviderAPIError("file not found", nil, 404, nil, nil)
	}
	_, bifrostErr = readPage([]BatchResultsSource{failing}, &schemas.BifrostBatchResultsRequest{Limit: 2}, func(int, []byte) {})
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
				consumed, more, err := readAllLines(stream.reader, pageLines, func(line []byte) {
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
			rest, _ := io.ReadAll(stream.reader)
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
		_, more, err := readAllLines(stream.reader, 0, func(line []byte) { ids = append(ids, string(line[:22])) })
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
		_, _, err := readAllLines(stream.reader, 0, func([]byte) { lines++; cancel() })
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
	case <-time.After(testtime.Scale(200 * time.Millisecond)):
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
	case <-time.After(testtime.Scale(5 * time.Second)):
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
	if _, _, err := readAllLines(stream.reader, 0, func([]byte) { lines++ }); err != nil || lines != 1 {
		t.Fatalf("read %d lines, err %v", lines, err)
	}
	stream.Close()
	total, ok := schemas.GetUpstreamLatency(ctx)
	if waited := total - afterHeaders; !ok || waited < bodyDelay/2 {
		t.Fatalf("upstream latency grew by %v while the body took %v to arrive", waited, bodyDelay)
	}
}

// The tests below (TXE-7494) set the process-wide response limit, so none of them runs in
// parallel: Go runs every sequential test before it releases the parallel ones, and Cleanup
// restores the default.

// countingSource serves data as a result source and counts the bytes read from it, to show
// a bounded read stops pulling from the stream.
func countingSource(data string, pulled *int64) BatchResultsSource {
	return func(offset int64) (*ResultStream, *schemas.BifrostError) {
		return NewResultStream(&countingReader{r: strings.NewReader(data[min(offset, int64(len(data))):]), n: pulled}, 0), nil
	}
}

type countingReader struct {
	r io.Reader
	n *int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	*c.n += int64(n)
	return n, err
}

// jsonlOf returns count lines of exactly size bytes each, newline included.
func jsonlOf(count, size int, tag string) string {
	var b strings.Builder
	for i := range count {
		line := fmt.Sprintf(`{"custom_id":"%s-%d","pad":"`, tag, i)
		b.WriteString(line)
		b.WriteString(strings.Repeat("x", size-len(line)-3))
		b.WriteString("\"}\n")
	}
	return b.String()
}

func setResultsLimitMB(t *testing.T, mb int) int64 {
	t.Helper()
	network.SetMaxResponseBodySizeMB(mb)
	t.Cleanup(func() { network.SetMaxResponseBodySizeMB(0) })
	return network.MaxResponseBodyBytes()
}

func TestReadBatchResultsPageRefusesAnUnpagedReadPastTheLimit(t *testing.T) {
	limit := setResultsLimitMB(t, 1)
	// 1.5 MiB of output and 0.5 MiB of errors: neither source alone is past the limit, so
	// the budget must count across sources.
	output := jsonlOf(1536, 1024, "o")
	errorFile := jsonlOf(512, 1024, "e")
	var pulled int64
	sources := []BatchResultsSource{countingSource(output, &pulled), countingSource(errorFile, &pulled)}

	_, bifrostErr := readPage(sources, &schemas.BifrostBatchResultsRequest{}, func(int, []byte) {})
	if bifrostErr == nil {
		t.Fatal("an unpaged read of 2 MiB under a 1 MiB limit succeeded")
	}
	if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 || bifrostErr.Error.Param != "limit" {
		t.Fatalf("error = %+v, want a 400 naming limit", bifrostErr)
	}
	if !strings.Contains(bifrostErr.Error.Message, "`limit` and `after`") {
		t.Fatalf("message %q does not tell the caller to page", bifrostErr.Error.Message)
	}
	if bifrostErr.AllowFallbacks == nil || *bifrostErr.AllowFallbacks {
		t.Fatal("a too-large results read must not fall back to another provider")
	}
	// The read stops at the limit; the reader may have read ahead one buffer past it.
	if pulled > limit+64*1024 {
		t.Fatalf("read pulled %d bytes from the stream, more than the %d-byte limit and one buffer", pulled, limit)
	}

	// Split the other way, the output alone is past the limit.
	pulled = 0
	_, bifrostErr = readPage([]BatchResultsSource{countingSource(jsonlOf(2048, 1024, "o"), &pulled)}, nil, func(int, []byte) {})
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 {
		t.Fatalf("one 2 MiB source: error = %+v, want a 400", bifrostErr)
	}
	if pulled > limit+64*1024 {
		t.Fatalf("read pulled %d bytes from one source, more than the %d-byte limit and one buffer", pulled, limit)
	}
}

func TestReadBatchResultsPageReturnsAResultSetExactlyAtTheLimit(t *testing.T) {
	setResultsLimitMB(t, 1)
	// Exactly 1 MiB across two sources, then nothing: the last byte is allowed.
	output, errorFile := jsonlOf(768, 1024, "o"), jsonlOf(256, 1024, "e")
	var pulled int64
	lines := 0
	page, bifrostErr := readPage([]BatchResultsSource{countingSource(output, &pulled), countingSource(errorFile, &pulled)}, nil, func(int, []byte) { lines++ })
	if bifrostErr != nil {
		t.Fatalf("a result set of exactly the limit was refused: %s", bifrostErr.Error.Message)
	}
	if lines != 1024 || page.HasMore {
		t.Fatalf("lines = %d, has_more = %v; want all 1024 and no more", lines, page.HasMore)
	}
}

func TestReadBatchResultsPageEndsAPageWithAHugeLimitAtTheLimit(t *testing.T) {
	limit := setResultsLimitMB(t, 1)
	// A caller cannot read the whole set in one response by sending a huge limit: the page
	// ends at its last whole line within the limit, and the pages together are every line
	// exactly once.
	output, errorFile := jsonlOf(1500, 1000, "o"), jsonlOf(700, 1000, "e")
	seen := map[string]int{}
	var after *string
	pages := 0
	for {
		pages++
		if pages > 10 {
			t.Fatal("paging did not terminate")
		}
		var pulled int64
		pageBytes := int64(0)
		page, bifrostErr := readPage(
			[]BatchResultsSource{countingSource(output, &pulled), countingSource(errorFile, &pulled)},
			&schemas.BifrostBatchResultsRequest{Limit: 1 << 30, After: after},
			func(_ int, line []byte) {
				pageBytes += int64(len(line)) + 1
				seen[string(line[:strings.IndexByte(string(line), ',')])]++
			})
		if bifrostErr != nil {
			t.Fatalf("page %d: %s", pages, bifrostErr.Error.Message)
		}
		if pageBytes > limit {
			t.Fatalf("page %d returned %d bytes of lines, more than the %d-byte limit", pages, pageBytes, limit)
		}
		// Each source may pull one byte past its share, and the first may have read ahead up
		// to one buffer past the last whole line it returned.
		if pulled > limit+64*1024+2 {
			t.Fatalf("page %d pulled %d bytes from the streams under a %d-byte limit", pages, pulled, limit)
		}
		if !page.HasMore {
			break
		}
		after = page.NextCursor
	}
	if pages < 2 {
		t.Fatalf("2.2 MB of lines came back in %d page under a 1 MiB limit", pages)
	}
	if len(seen) != 2200 {
		t.Fatalf("pages returned %d distinct lines, want 2200", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("line %s returned %d times", id, n)
		}
	}
}

// TXE-7526 review: the slim copies a page hands on count against its byte budget like any line.
// 24 oversized lines whose slim copies are about 50 KB each (1.2 MB together) cannot come back
// in one page under a 1 MiB limit: pages end before a slim copy that would pass it, every line
// still comes back exactly once, and an unpaged read is the 400.
func TestReadBatchResultsPageCountsSlimCopiesAgainstTheLimit(t *testing.T) {
	limit := setResultsLimitMB(t, 1)
	var file strings.Builder
	for i := range 24 {
		// 1,000 short fields make a ~50 KB slim copy; the padding makes the line past the limit.
		fmt.Fprintf(&file, `{"custom_id":"big-%02d"`, i)
		for f := range 1000 {
			fmt.Fprintf(&file, `,"f%03d":"%036d"`, f, f)
		}
		fmt.Fprintf(&file, `,"pad":"%s"}`+"\n", strings.Repeat("x", int(limit)))
	}
	data := file.String()

	seen := map[string]int{}
	var after *string
	pages := 0
	for {
		pages++
		if pages > 50 {
			t.Fatal("paging did not terminate")
		}
		var pageSlim int64
		page, bifrostErr := ReadBatchResultsPage([]BatchResultsSource{countingSource(data, new(int64))}, &schemas.BifrostBatchResultsRequest{Limit: 1 << 30, After: after},
			func(int, []byte) { t.Fatal("no line here fits a page whole") },
			func(_ int, big BatchOversizedLine) {
				pageSlim += int64(len(big.Slim))
				seen[string(big.Slim[:len(`{"custom_id":"big-00"`)])]++
			})
		if bifrostErr != nil {
			t.Fatalf("page %d: %s", pages, bifrostErr.Error.Message)
		}
		if pageSlim > limit {
			t.Fatalf("page %d handed on %d bytes of slim copies under a %d-byte limit", pages, pageSlim, limit)
		}
		if !page.HasMore {
			break
		}
		after = page.NextCursor
	}
	if len(seen) != 24 || pages < 2 {
		t.Fatalf("%d distinct lines in %d pages; want all 24 in more than one page", len(seen), pages)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("%s came back %d times", id, n)
		}
	}
	_, bifrostErr := ReadBatchResultsPage([]BatchResultsSource{countingSource(data, new(int64))}, nil, func(int, []byte) {}, func(int, BatchOversizedLine) {})
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 {
		t.Fatalf("unpaged read of 1.2 MB of slim copies: error %+v, want the 400", bifrostErr)
	}
}

// TXE-7526: a line larger than the limit on its own fits no page. It is skipped without being
// held, reported once as line_too_large, and the pages walk past it: every other line comes
// back exactly once, in both sources, paged or not. Before, every page that reached it was a
// 400, so no caller, and not the accounting sweeper, could read the rest of the batch.
func TestReadBatchResultsPageSkipsALineLargerThanTheLimit(t *testing.T) {
	limit := setResultsLimitMB(t, 1)
	huge := jsonlOf(1, int(limit)+10, "big")
	output := jsonlOf(400, 1000, "o") + huge + jsonlOf(400, 1000, "p")
	errorFile := huge + jsonlOf(300, 1000, "e")
	for _, pageLimit := range []int{0, 1 << 30, 500, 1} {
		seen := map[string]int{}
		var skipped []string
		var after *string
		for pages := 1; ; pages++ {
			if pages > 2000 {
				t.Fatalf("limit %d: paging did not terminate", pageLimit)
			}
			var pulled, pageBytes int64
			page, bifrostErr := ReadBatchResultsPage(
				[]BatchResultsSource{countingSource(output, &pulled), countingSource(errorFile, &pulled)},
				&schemas.BifrostBatchResultsRequest{Limit: pageLimit, After: after},
				func(_ int, line []byte) {
					pageBytes += int64(len(line)) + 1
					seen[string(line[:strings.IndexByte(string(line), ',')])]++
				},
				func(source int, big BatchOversizedLine) {
					if big.Size != int64(len(huge)) || string(big.Slim) != `{"custom_id":"big-0","pad":""}` {
						t.Fatalf("limit %d: oversized line reported as size %d, slim %q", pageLimit, big.Size, big.Slim)
					}
					if lineErr := big.Error(); lineErr.Code != BatchLineTooLargeCode || !strings.Contains(lineErr.Message, fmt.Sprintf("%d bytes", len(huge))) {
						t.Fatalf("limit %d: its parse error is %+v", pageLimit, lineErr)
					}
					skipped = append(skipped, fmt.Sprint(source))
				})
			if pageLimit == 0 {
				// 1.1 MB of readable lines is still past the limit for one unpaged response.
				if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 {
					t.Fatalf("unpaged: error = %+v, want the 400 telling the caller to page", bifrostErr)
				}
				break
			}
			if bifrostErr != nil {
				t.Fatalf("limit %d, page %d: %s", pageLimit, pages, bifrostErr.Error.Message)
			}
			// A skipped line is read through, so the stream gives more than the page holds; what
			// the page hands on stays within the limit.
			if pageBytes > limit {
				t.Fatalf("limit %d, page %d returned %d bytes of lines under a %d-byte limit", pageLimit, pages, pageBytes, limit)
			}
			if !page.HasMore {
				break
			}
			after = page.NextCursor
		}
		if pageLimit == 0 {
			continue
		}
		if fmt.Sprint(skipped) != "[0 1]" {
			t.Fatalf("limit %d: skipped lines reported from sources %v, want one in each [0 1]", pageLimit, skipped)
		}
		if len(seen) != 1100 {
			t.Fatalf("limit %d: %d distinct lines came back, want all 1100 others", pageLimit, len(seen))
		}
		for id, n := range seen {
			if n != 1 {
				t.Fatalf("limit %d: line %s came back %d times", pageLimit, id, n)
			}
		}
	}

	// A set of small lines with one huge line, under the limit once it is skipped, reads whole.
	var lines, skips int
	page, bifrostErr := ReadBatchResultsPage([]BatchResultsSource{countingSource(jsonlOf(10, 1000, "a")+huge+jsonlOf(10, 1000, "b"), new(int64))}, nil,
		func(int, []byte) { lines++ }, func(int, BatchOversizedLine) { skips++ })
	if bifrostErr != nil || lines != 20 || skips != 1 || page.HasMore {
		t.Fatalf("unpaged read with one huge line: %d lines, %d skipped, has_more %v, err %v; want 20, 1, false, nil", lines, skips, page != nil && page.HasMore, bifrostErr)
	}
}

// No request reads past the bound (TXE-7513). Each field of the request is set in turn to a
// value that is not its zero value, so a field added later to exempt a read (as Settlement did
// until the sweeper paged) fails here instead of reading a batch whole.
func TestReadBatchResultsPageBoundsEveryRequest(t *testing.T) {
	limit := setResultsLimitMB(t, 1)
	output := jsonlOf(2048, 1024, "o")
	start := BatchResultsCursor{}.Encode()
	requestType := reflect.TypeOf(schemas.BifrostBatchResultsRequest{})
	for i := range requestType.NumField() {
		field := requestType.Field(i)
		request := &schemas.BifrostBatchResultsRequest{}
		value := reflect.ValueOf(request).Elem().Field(i)
		switch {
		case field.Name == "After":
			value.Set(reflect.ValueOf(&start))
		case value.Kind() == reflect.Bool:
			value.SetBool(true)
		case value.Kind() == reflect.Int:
			value.SetInt(1 << 30)
		case value.Kind() == reflect.String:
			value.SetString("openai")
		case value.Kind() == reflect.Pointer && value.Type().Elem().Kind() == reflect.String:
			s := "x"
			value.Set(reflect.ValueOf(&s))
		case value.Kind() == reflect.Slice && value.Type().Elem().Kind() == reflect.Uint8:
			value.SetBytes([]byte("{}"))
		case value.Kind() == reflect.Map && value.Type().Key().Kind() == reflect.String:
			m := reflect.MakeMap(value.Type())
			m.SetMapIndex(reflect.ValueOf("x").Convert(value.Type().Key()), reflect.Zero(value.Type().Elem()))
			value.Set(m)
		default:
			t.Fatalf("field %s (%s): add a non-zero value for it to this test", field.Name, value.Type())
		}
		var pulled, returned int64
		page, bifrostErr := readPage([]BatchResultsSource{countingSource(output, &pulled)}, request, func(_ int, line []byte) {
			returned += int64(len(line)) + 1
		})
		if bifrostErr == nil && (returned > limit || !page.HasMore) {
			t.Errorf("with %s set, the read returned %d bytes of lines (has_more %v) under a %d-byte limit", field.Name, returned, page.HasMore, limit)
		}
		if bifrostErr != nil && (bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400) {
			t.Errorf("with %s set: error %+v, want the bounded 400", field.Name, bifrostErr)
		}
		if pulled > limit+64*1024+1 {
			t.Errorf("with %s set, the read pulled %d bytes from the stream under a %d-byte limit", field.Name, pulled, limit)
		}
	}
}

func TestReadJSONLPageDoesNotHandOnALineAFailedReadCutShort(t *testing.T) {
	r := io.MultiReader(strings.NewReader("{\"a\":1}\n{\"b\":"), iotestErrReader{})
	var lines []string
	consumed, _, err := readAllLines(r, 0, func(line []byte) { lines = append(lines, string(line)) })
	if err == nil {
		t.Fatal("read error was dropped")
	}
	if fmt.Sprint(lines) != `[{"a":1}]` || consumed != 8 {
		t.Fatalf("lines = %v, consumed = %d; want only the whole line and its 8 bytes", lines, consumed)
	}
}

type iotestErrReader struct{}

func (iotestErrReader) Read([]byte) (int, error) { return 0, fmt.Errorf("connection reset") }

// BenchmarkReadBatchResultsPage measures one results read of in-memory sources, the request
// path every streamed provider's GET /v1/batches/{id}/results takes after its downloads are
// open (TXE-7494 added the byte budget to it). small_page is an ordinary paged read, 100 of
// 1,000 lines of 300 bytes; unpaged reads all 1,000 lines, output then errors.
func BenchmarkReadBatchResultsPage(b *testing.B) {
	output, errorFile := jsonlOf(800, 300, "o"), jsonlOf(200, 300, "e")
	sources := func() []BatchResultsSource {
		open := func(data string) BatchResultsSource {
			return func(offset int64) (*ResultStream, *schemas.BifrostError) {
				return NewResultStream(strings.NewReader(data[min(offset, int64(len(data))):]), 0), nil
			}
		}
		return []BatchResultsSource{open(output), open(errorFile)}
	}
	for _, bc := range []struct {
		name  string
		limit int
		want  int
	}{{"small_page", 100, 100}, {"unpaged", 0, 1000}} {
		b.Run(bc.name, func(b *testing.B) {
			request := &schemas.BifrostBatchResultsRequest{Limit: bc.limit}
			b.ReportAllocs()
			for b.Loop() {
				lines := 0
				if _, bifrostErr := readPage(sources(), request, func(int, []byte) { lines++ }); bifrostErr != nil || lines != bc.want {
					b.Fatalf("lines = %d, err %v", lines, bifrostErr)
				}
			}
		})
	}
}
