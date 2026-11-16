package utils

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/network"
	"github.com/capsohq/bifrost/core/schemas"
)

// These tests set the process-wide response limit, so none of them runs in parallel: Go runs
// every sequential test before it releases the parallel ones, and Cleanup restores the default.

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

	_, bifrostErr := ReadBatchResultsPage(sources, &schemas.BifrostBatchResultsRequest{}, func(int, []byte) {})
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
	if pulled > limit+1 {
		t.Fatalf("read pulled %d bytes from the stream, more than the %d-byte limit", pulled, limit)
	}

	// Split the other way, the output alone is past the limit.
	pulled = 0
	_, bifrostErr = ReadBatchResultsPage([]BatchResultsSource{countingSource(jsonlOf(2048, 1024, "o"), &pulled)}, nil, func(int, []byte) {})
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 {
		t.Fatalf("one 2 MiB source: error = %+v, want a 400", bifrostErr)
	}
	if pulled > limit+1 {
		t.Fatalf("read pulled %d bytes from one source, more than the %d-byte limit", pulled, limit)
	}
}

func TestReadBatchResultsPageReturnsAResultSetExactlyAtTheLimit(t *testing.T) {
	setResultsLimitMB(t, 1)
	// Exactly 1 MiB across two sources, then nothing: the last byte is allowed.
	output, errorFile := jsonlOf(768, 1024, "o"), jsonlOf(256, 1024, "e")
	var pulled int64
	lines := 0
	page, bifrostErr := ReadBatchResultsPage([]BatchResultsSource{countingSource(output, &pulled), countingSource(errorFile, &pulled)}, nil, func(int, []byte) { lines++ })
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
		page, bifrostErr := ReadBatchResultsPage(
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

func TestReadBatchResultsPageRefusesALineLargerThanTheLimit(t *testing.T) {
	setResultsLimitMB(t, 1)
	// Paging cannot help a single line past the limit, so a page with nothing to return is
	// a 400, not an empty page with a cursor that never advances.
	huge := jsonlOf(1, (1<<20)+10, "big")
	var pulled int64
	_, bifrostErr := ReadBatchResultsPage([]BatchResultsSource{countingSource(huge, &pulled)}, &schemas.BifrostBatchResultsRequest{Limit: 10}, func(int, []byte) {
		t.Fatal("a line past the limit was handed on")
	})
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 {
		t.Fatalf("error = %+v, want a 400", bifrostErr)
	}
}

func TestReadBatchResultsPageLetsTheSettlementReadSeeTheWholeSet(t *testing.T) {
	setResultsLimitMB(t, 1)
	// The accounting sweeper prices the whole set, so its read alone is not bounded.
	output := jsonlOf(2048, 1024, "o")
	var pulled int64
	lines := 0
	page, bifrostErr := ReadBatchResultsPage([]BatchResultsSource{countingSource(output, &pulled)}, &schemas.BifrostBatchResultsRequest{Settlement: true}, func(int, []byte) { lines++ })
	if bifrostErr != nil {
		t.Fatalf("the settlement read was refused: %s", bifrostErr.Error.Message)
	}
	if lines != 2048 || page.HasMore {
		t.Fatalf("lines = %d, has_more = %v; want all 2048", lines, page.HasMore)
	}
}

func TestReadJSONLPageDoesNotHandOnALineAFailedReadCutShort(t *testing.T) {
	r := io.MultiReader(strings.NewReader("{\"a\":1}\n{\"b\":"), iotestErrReader{})
	var lines []string
	consumed, _, err := readJSONLPage(r, 0, func(line []byte) { lines = append(lines, string(line)) })
	if err == nil {
		t.Fatal("read error was dropped")
	}
	if fmt.Sprint(lines) != `[{"a":1}]` || consumed != 8 {
		t.Fatalf("lines = %v, consumed = %d; want only the whole line and its 8 bytes", lines, consumed)
	}
}

type iotestErrReader struct{}

func (iotestErrReader) Read([]byte) (int, error) { return 0, fmt.Errorf("connection reset") }
