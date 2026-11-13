package network

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/valyala/fasthttp"
)

// withResponseLimitMB sets client.max_response_body_size_mb for one test and restores the default.
func withResponseLimitMB(t *testing.T, mb int) {
	t.Helper()
	SetMaxResponseBodySizeMB(mb)
	t.Cleanup(func() { SetMaxResponseBodySizeMB(0) })
}

// TXE-7148: an unset or non-positive limit is the 512 MB default; a set one is read in MB.
func TestMaxResponseBodyBytesDefaultAndOverride(t *testing.T) {
	SetMaxResponseBodySizeMB(0)
	t.Cleanup(func() { SetMaxResponseBodySizeMB(0) })
	if got, want := MaxResponseBodyBytes(), int64(DefaultMaxResponseBodySizeMB)<<20; got != want {
		t.Fatalf("default limit = %d, want %d", got, want)
	}
	SetMaxResponseBodySizeMB(3)
	if got := MaxResponseBodyBytes(); got != 3<<20 {
		t.Fatalf("limit after setting 3 MB = %d, want %d", got, 3<<20)
	}
	SetMaxResponseBodySizeMB(-5)
	if got, want := MaxResponseBodyBytes(), int64(DefaultMaxResponseBodySizeMB)<<20; got != want {
		t.Fatalf("a negative setting must restore the default: got %d, want %d", got, want)
	}
}

// A fasthttp client's MaxResponseBodySize of 0 means unlimited to fasthttp; the effective limit
// is never 0, and a client's own tighter cap wins over the gateway's.
func TestEffectiveMaxResponseBodySize(t *testing.T) {
	withResponseLimitMB(t, 2)
	cases := []struct {
		clientMax int
		want      int
	}{
		{0, 2 << 20},       // unset client: the gateway's limit
		{1024, 1024},       // the media fetcher's or auth client's tighter cap
		{8 << 20, 2 << 20}, // a looser client cap does not lift the gateway's limit
		{-1, 2 << 20},      // fasthttp treats negative as unlimited too
		{2 << 20, 2 << 20}, // equal
		{2<<20 - 1, 2<<20 - 1},
	}
	for _, c := range cases {
		if got := EffectiveMaxResponseBodySize(c.clientMax); got != c.want {
			t.Errorf("EffectiveMaxResponseBodySize(%d) = %d, want %d", c.clientMax, got, c.want)
		}
	}
}

// The read fails past the limit and is never cut short: no prefix of an over-limit body is
// returned as if it were the whole body. Without the bound, io.ReadAll returns all 11 bytes.
func TestReadAllLimitFailsPastTheLimitWithoutTruncating(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reader  func(string) io.Reader
		body    string
		limit   int64
		wantErr bool
	}{
		{"under", func(s string) io.Reader { return strings.NewReader(s) }, "hello", 10, false},
		{"exactly at the limit", func(s string) io.Reader { return strings.NewReader(s) }, "0123456789", 10, false},
		{"one byte over", func(s string) io.Reader { return strings.NewReader(s) }, "0123456789x", 10, true},
		{"one byte at a time, over", func(s string) io.Reader { return iotest.OneByteReader(strings.NewReader(s)) }, "0123456789x", 10, true},
		{"one byte at a time, at", func(s string) io.Reader { return iotest.OneByteReader(strings.NewReader(s)) }, "0123456789", 10, false},
		{"empty", func(s string) io.Reader { return strings.NewReader(s) }, "", 10, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadAllLimit(tc.reader(tc.body), tc.limit)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("ReadAllLimit: %v", err)
				}
				if string(got) != tc.body {
					t.Fatalf("ReadAllLimit = %q, want %q", got, tc.body)
				}
				return
			}
			if got != nil {
				t.Fatalf("an over-limit read returned %d bytes; it must return none", len(got))
			}
			var tooLarge *ResponseBodyTooLargeError
			if !errors.As(err, &tooLarge) || tooLarge.Limit != tc.limit {
				t.Fatalf("error = %v, want *ResponseBodyTooLargeError with Limit %d", err, tc.limit)
			}
			if !errors.Is(err, ErrResponseBodyTooLarge) {
				t.Fatalf("errors.Is(err, ErrResponseBodyTooLarge) = false for %v", err)
			}
			// The media fetcher and the stale-connection retry test fasthttp's own error.
			if !errors.Is(err, fasthttp.ErrBodyTooLarge) {
				t.Fatalf("errors.Is(err, fasthttp.ErrBodyTooLarge) = false for %v", err)
			}
			if !strings.Contains(err.Error(), "client.max_response_body_size_mb") {
				t.Fatalf("error %q does not name the setting", err)
			}
		})
	}
}

// ReadAllBounded reads under the configured limit, so a saved change applies to the next read.
func TestReadAllBoundedFollowsTheConfiguredLimit(t *testing.T) {
	withResponseLimitMB(t, 1)
	if _, err := ReadAllBounded(bytes.NewReader(make([]byte, 1<<20))); err != nil {
		t.Fatalf("a 1 MiB body under a 1 MiB limit: %v", err)
	}
	if _, err := ReadAllBounded(bytes.NewReader(make([]byte, 1<<20+1))); !errors.Is(err, ErrResponseBodyTooLarge) {
		t.Fatalf("a body one byte over 1 MiB: err = %v, want ErrResponseBodyTooLarge", err)
	}
	SetMaxResponseBodySizeMB(2)
	if _, err := ReadAllBounded(bytes.NewReader(make([]byte, 1<<20+1))); err != nil {
		t.Fatalf("after raising the limit to 2 MiB: %v", err)
	}
}

// A decoder fed a bounded reader stops at the limit too (OAuth discovery decodes JSON this way).
func TestNewBoundedReaderStopsADecoder(t *testing.T) {
	r := NewBoundedReader(strings.NewReader(`{"a":"`+strings.Repeat("x", 64)+`"}`), 16)
	_, err := io.Copy(io.Discard, r)
	if !errors.Is(err, ErrResponseBodyTooLarge) {
		t.Fatalf("copy through a 16-byte bounded reader: err = %v, want ErrResponseBodyTooLarge", err)
	}
	// Further reads keep failing; the reader never resumes past the limit.
	if n, err := r.Read(make([]byte, 8)); n != 0 || !errors.Is(err, ErrResponseBodyTooLarge) {
		t.Fatalf("read after the limit = (%d, %v), want (0, ErrResponseBodyTooLarge)", n, err)
	}
}

// Request path: every buffered provider answer that goes through ReadAllBounded (Bedrock's
// net/http reads, the decoders in CheckAndDecodeBody) pays this.
func BenchmarkReadAllBounded(b *testing.B) {
	body := bytes.Repeat([]byte("x"), 64<<10)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		if _, err := ReadAllBounded(bytes.NewReader(body)); err != nil {
			b.Fatal(err)
		}
	}
}

// The unbounded read it replaced, for the before/after in the PR.
func BenchmarkReadAllUnbounded(b *testing.B) {
	body := bytes.Repeat([]byte("x"), 64<<10)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		if _, err := io.ReadAll(bytes.NewReader(body)); err != nil {
			b.Fatal(err)
		}
	}
}
