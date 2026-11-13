package network

import (
	"errors"
	"fmt"
	"io"
	"math"
	"sync/atomic"

	"github.com/valyala/fasthttp"
)

// DefaultMaxResponseBodySizeMB is the largest upstream response body the gateway holds in memory
// when client.max_response_body_size_mb is unset (fork, TXE-7148). It matches OpenAI's 512 MB file
// limit, so a file or batch-output download of the largest file a vendor accepts still fits.
const DefaultMaxResponseBodySizeMB = 512

// maxResponseBodyBytes is the configured limit in bytes; 0 means DefaultMaxResponseBodySizeMB.
var maxResponseBodyBytes atomic.Int64

// SetMaxResponseBodySizeMB sets the limit every buffered upstream response body is read under,
// from client.max_response_body_size_mb. A value of 0 or less restores the default. It is read
// on every buffered read, so a saved change applies to the next response without a restart.
func SetMaxResponseBodySizeMB(mb int) {
	if mb <= 0 {
		maxResponseBodyBytes.Store(0)
		return
	}
	// A size beyond what int64 bytes can hold is the same as no practical limit; keep it finite.
	if int64(mb) > math.MaxInt64>>20 {
		maxResponseBodyBytes.Store(math.MaxInt64)
		return
	}
	maxResponseBodyBytes.Store(int64(mb) << 20)
}

// MaxResponseBodyBytes returns the limit in bytes that a buffered upstream response body is read
// under: client.max_response_body_size_mb, or DefaultMaxResponseBodySizeMB when it is unset.
func MaxResponseBodyBytes() int64 {
	if v := maxResponseBodyBytes.Load(); v > 0 {
		return v
	}
	return DefaultMaxResponseBodySizeMB << 20
}

// EffectiveMaxResponseBodySize returns the limit a fasthttp client's buffered read applies: the
// client's own MaxResponseBodySize when it sets a tighter one on purpose (the media fetcher, the
// auth client), otherwise the configured limit. It never returns 0, which fasthttp reads as
// unlimited.
func EffectiveMaxResponseBodySize(clientMax int) int {
	limit := MaxResponseBodyBytes()
	if clientMax > 0 && int64(clientMax) < limit {
		return clientMax
	}
	if limit > math.MaxInt {
		return math.MaxInt
	}
	return int(limit)
}

// ErrResponseBodyTooLarge is what every over-limit upstream body read wraps; test with errors.Is.
var ErrResponseBodyTooLarge = errors.New("upstream response body exceeds the gateway's limit")

// ResponseBodyTooLargeError reports an upstream response body larger than the limit it was read
// under. The read fails; the body is never cut short and handed on as if it were whole.
type ResponseBodyTooLargeError struct {
	Limit int64 // bytes
}

func (e *ResponseBodyTooLargeError) Error() string {
	return fmt.Sprintf("upstream response body exceeds the gateway's limit of %d bytes (client.max_response_body_size_mb)", e.Limit)
}

// Is makes errors.Is(err, ErrResponseBodyTooLarge) true for every limit, and keeps
// errors.Is(err, fasthttp.ErrBodyTooLarge) true for the callers that tested fasthttp's own error
// before the transport began reporting this one (the media fetcher, the stale-connection retry).
func (e *ResponseBodyTooLargeError) Is(target error) bool {
	return target == ErrResponseBodyTooLarge || target == fasthttp.ErrBodyTooLarge
}

// boundedReader returns the error in place of the first byte past limit.
type boundedReader struct {
	r         io.Reader
	limit     int64
	remaining int64 // bytes still allowed, plus the one byte that proves the body is over
}

// NewBoundedReader wraps r so that reading more than limit bytes from it fails with
// *ResponseBodyTooLargeError. Up to limit bytes read exactly as from r. A limit of 0 or less uses
// MaxResponseBodyBytes. Use it for a body handed to a decoder (json.NewDecoder, a decompressor)
// rather than to ReadAllBounded.
func NewBoundedReader(r io.Reader, limit int64) io.Reader {
	if limit <= 0 {
		limit = MaxResponseBodyBytes()
	}
	remaining := limit
	if remaining < math.MaxInt64 {
		remaining++
	}
	return &boundedReader{r: r, limit: limit, remaining: remaining}
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, &ResponseBodyTooLargeError{Limit: b.limit}
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.r.Read(p)
	b.remaining -= int64(n)
	if b.remaining <= 0 {
		// The extra byte arrived: the body is longer than the limit. Hand back only what fits.
		return n - 1, &ResponseBodyTooLargeError{Limit: b.limit}
	}
	return n, err
}

// ReadAllBounded reads r to EOF under MaxResponseBodyBytes and fails with
// *ResponseBodyTooLargeError when r holds more. It is the one way the fork reads a whole upstream
// response body into memory; TestNoUnboundedUpstreamBodyReads fails on provider code that reads
// one any other way.
func ReadAllBounded(r io.Reader) ([]byte, error) {
	return ReadAllLimit(r, MaxResponseBodyBytes())
}

// ReadAllLimit is ReadAllBounded with an explicit limit, for a body whose own cap is tighter than
// the gateway's (an error snippet, a token response). A limit of 0 or less uses
// MaxResponseBodyBytes.
//
// It keeps io.ReadAll for the buffering, whose growth holds about the body's own size, and adds
// one small allocation for the bounded reader. A hand-written doubling loop measured twice the
// bytes per read.
func ReadAllLimit(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(NewBoundedReader(r, limit))
	if err != nil {
		return nil, err
	}
	return data, nil
}
