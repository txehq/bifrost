package utils

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// Batch results are read as pages of JSONL lines from one or more ordered sources (OpenAI
// and Azure: the output file, then the error file). A page is read from a streamed
// download and the connection is dropped once the page is full, so a 200 MB result file is
// never held in memory, whatever its size. The cursor is the position of the next line:
// which source, and how many bytes into it.

// batchResultsCursorVersion prefixes every cursor, so a later change to its layout can
// refuse old cursors instead of misreading them.
const batchResultsCursorVersion = "v1"

// BatchResultsCursor is the position of the next result line to return.
type BatchResultsCursor struct {
	Source int   // Index into the provider's ordered result sources
	Offset int64 // Bytes into that source; for an in-memory source, the item index
}

// Encode returns the opaque cursor a caller sends back as `after`.
func (c BatchResultsCursor) Encode() string {
	return base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, "%s:%d:%d", batchResultsCursorVersion, c.Source, c.Offset))
}

// DecodeBatchResultsCursor parses an `after` cursor. A nil or empty cursor is the start.
func DecodeBatchResultsCursor(after *string) (BatchResultsCursor, error) {
	if after == nil || strings.TrimSpace(*after) == "" {
		return BatchResultsCursor{}, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(*after))
	if err != nil {
		return BatchResultsCursor{}, fmt.Errorf("invalid results cursor")
	}
	parts := strings.Split(string(decoded), ":")
	if len(parts) != 3 || parts[0] != batchResultsCursorVersion {
		return BatchResultsCursor{}, fmt.Errorf("invalid results cursor")
	}
	source, err := strconv.Atoi(parts[1])
	if err != nil || source < 0 {
		return BatchResultsCursor{}, fmt.Errorf("invalid results cursor")
	}
	offset, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || offset < 0 {
		return BatchResultsCursor{}, fmt.Errorf("invalid results cursor")
	}
	return BatchResultsCursor{Source: source, Offset: offset}, nil
}

// NewInvalidResultsCursorError is the 400 a provider returns for an `after` value it did
// not issue.
func NewInvalidResultsCursorError(err error) *schemas.BifrostError {
	bifrostErr := NewBifrostOperationError("invalid `after` cursor for batch results", err)
	bifrostErr.StatusCode = schemas.Ptr(fasthttp.StatusBadRequest)
	bifrostErr.Error.Param = "after"
	return bifrostErr
}

// PageBatchResultItems pages an in-memory list of total items (Gemini's inline responses,
// which arrive whole inside the batch object). It returns the half-open range of the page
// and the cursor of the next one. limit <= 0 is the whole list.
func PageBatchResultItems(total int, after *string, limit int) (start, end int, nextCursor *string, err error) {
	cursor, err := DecodeBatchResultsCursor(after)
	if err != nil {
		return 0, 0, nil, err
	}
	if cursor.Source != 0 {
		return 0, 0, nil, fmt.Errorf("invalid results cursor")
	}
	start = int(min(cursor.Offset, int64(total)))
	end = total
	// Compared against what remains, so a limit near the integer maximum cannot overflow.
	if limit > 0 && limit < total-start {
		end = start + limit
		nextCursor = schemas.Ptr(BatchResultsCursor{Offset: int64(end)}.Encode())
	}
	return start, end, nextCursor, nil
}

// ReadJSONLPage reads up to limit non-empty lines from r, calling onLine for each. It
// returns the bytes consumed, which is the offset of the next unread line relative to
// where r started, and whether any line remains after the page. limit <= 0 reads to the
// end. A line is passed without its line ending; the slice is only valid during the call.
func ReadJSONLPage(r io.Reader, limit int, onLine func(line []byte)) (consumed int64, more bool, err error) {
	br := bufio.NewReaderSize(r, 64*1024)
	count := 0
	for {
		if limit > 0 && count >= limit {
			// The page is full: more is true only if a non-blank byte follows.
			for {
				b, readErr := br.ReadByte()
				if readErr != nil {
					if errors.Is(readErr, io.EOF) {
						return consumed, false, nil
					}
					return consumed, false, readErr
				}
				if b == '\n' || b == '\r' || b == ' ' || b == '\t' {
					consumed++
					continue
				}
				return consumed, true, nil
			}
		}
		line, readErr := br.ReadBytes('\n')
		consumed += int64(len(line))
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			onLine(trimmed)
			count++
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return consumed, false, nil
			}
			return consumed, false, readErr
		}
	}
}

// ResultStream is one result source opened as a streamed download.
type ResultStream struct {
	Reader  io.Reader
	Latency time.Duration // Time to the response headers
	release func()
}

// Close ends the download. A stream that was not read to its end drops its connection
// instead of draining the rest of the file.
func (s *ResultStream) Close() {
	if s != nil && s.release != nil {
		s.release()
		s.release = nil
	}
}

// resultStreamErrorBodyLimit bounds how much of an error response is read for parsing.
const resultStreamErrorBodyLimit = 512 * 1024

// errResultStreamIdle closes a result download that stopped sending data.
var errResultStreamIdle = errors.New("batch result download stalled")

// OpenResultStream issues a GET for a result file and returns its body as a stream that
// starts at byte offset. client must be a streaming client (BuildStreamingClient): the body
// is read from the socket as the caller consumes it. prepare sets the URL, method and
// credentials; parseError turns a non-2xx response, whose body has been read into resp,
// into the provider's error.
//
// For offset > 0 the request asks for the byte range. A server that honours it answers 206
// and the stream starts at offset; one that ignores it answers 200 with the whole file,
// and the first offset bytes are read and discarded, which costs transfer but no memory.
// A 416, or a file shorter than offset, is an empty stream: the source is exhausted.
func OpenResultStream(
	ctx *schemas.BifrostContext,
	client *fasthttp.Client,
	offset int64,
	prepare func(req *fasthttp.Request),
	parseError func(resp *fasthttp.Response) *schemas.BifrostError,
) (*ResultStream, *schemas.BifrostError) {
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	resp.StreamBody = true
	prepare(req)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		// A byte range of a compressed representation cannot be decoded from the middle.
		req.Header.Set("Accept-Encoding", "identity")
	}

	startTime := time.Now()
	err := DoStreamingRequest(ctx, client, req, resp)
	latency := time.Since(startTime)
	fasthttp.ReleaseRequest(req)

	releaseResponse := func() {
		// Closing a body that was not read to EOF closes its connection (streamBody
		// CloseWithError), so the rest of the file is never downloaded.
		_ = resp.CloseBodyStream()
		fasthttp.ReleaseResponse(resp)
	}

	if err != nil {
		releaseResponse()
		switch {
		case errors.Is(err, context.Canceled):
			return nil, &schemas.BifrostError{
				IsBifrostError: false,
				Error: &schemas.ErrorField{
					Type:    schemas.Ptr(schemas.RequestCancelled),
					Message: schemas.ErrRequestCancelled,
					Error:   err,
				},
			}
		case errors.Is(err, fasthttp.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
			return nil, NewBifrostTimeoutError(schemas.ErrProviderRequestTimedOut, err)
		default:
			return nil, NewBifrostUpstreamConnectionError(schemas.ErrProviderDoRequest, err)
		}
	}

	statusCode := resp.StatusCode()
	if statusCode == fasthttp.StatusRequestedRangeNotSatisfiable && offset > 0 {
		releaseResponse()
		return &ResultStream{Reader: bytes.NewReader(nil), Latency: latency}, nil
	}

	// The transport stops watching the request context once the headers are in, so the
	// body is closed here when the context ends or the upstream stops sending. The watch
	// is in place before the first body byte is read, error bodies included.
	reader, releaseGzip := DecompressStreamBody(resp)
	bodyStream := resp.BodyStream()
	closeBody := func(cause error) {
		if closer, ok := bodyStream.(streamCloserWithError); ok {
			_ = closer.CloseWithError(cause)
		}
	}
	idleTimeout := GetStreamIdleTimeout(ctx)
	idleTimer := time.AfterFunc(idleTimeout, func() { closeBody(errResultStreamIdle) })
	done := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			closeBody(ctx.Err())
		case <-done:
		}
	}()
	var releaseOnce sync.Once
	stream := &ResultStream{
		Reader:  &resultBodyReader{ctx: ctx, reader: reader, timer: idleTimer, timeout: idleTimeout},
		Latency: latency,
		release: func() {
			releaseOnce.Do(func() {
				idleTimer.Stop()
				close(done)
				<-watcherDone
				releaseGzip()
				releaseResponse()
			})
		},
	}

	if statusCode != fasthttp.StatusOK && statusCode != fasthttp.StatusPartialContent {
		// Read the vendor's error for parsing, bounded in size and by the same watch.
		errorBody, _ := io.ReadAll(io.LimitReader(stream.Reader, resultStreamErrorBodyLimit))
		errorResp := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseResponse(errorResp)
		resp.Header.CopyTo(&errorResp.Header)
		errorResp.SetBody(errorBody)
		stream.Close()
		return nil, SetErrorLatency(parseError(errorResp), latency)
	}

	if statusCode == fasthttp.StatusOK && offset > 0 {
		// The server sent the whole file: skip to the offset without keeping the bytes.
		if _, skipErr := io.CopyN(io.Discard, stream.Reader, offset); skipErr != nil {
			stream.Close()
			if errors.Is(skipErr, io.EOF) {
				return &ResultStream{Reader: bytes.NewReader(nil), Latency: latency}, nil
			}
			return nil, NewBifrostUpstreamConnectionError(schemas.ErrProviderDoRequest, skipErr)
		}
	}
	return stream, nil
}

// resultBodyReader reads a result download's body. It restarts the idle timer on every
// read, so a download is ended only when the upstream stops sending, not because the file
// is large, and it counts the time blocked on the socket as upstream latency: the request
// helper measures only up to the response headers.
type resultBodyReader struct {
	ctx     *schemas.BifrostContext
	reader  io.Reader
	timer   *time.Timer
	timeout time.Duration
}

func (r *resultBodyReader) Read(p []byte) (int, error) {
	start := time.Now()
	n, err := r.reader.Read(p)
	schemas.AddUpstreamLatency(r.ctx, time.Since(start))
	if n > 0 {
		r.timer.Reset(r.timeout)
	}
	return n, err
}

// BatchResultsSource opens one ordered source of result lines at a byte offset. A nil
// stream with a nil error means the batch has no such source.
type BatchResultsSource func(offset int64) (*ResultStream, *schemas.BifrostError)

// BatchResultsPage is what reading one page of result lines produced, besides the lines
// handed to the callback.
type BatchResultsPage struct {
	NextCursor *string
	HasMore    bool
	Latency    time.Duration
}

// ReadBatchResultsPage reads one page of result lines across sources, in order, starting
// at the `after` cursor, and calls onLine for each line with the index of its source.
// limit <= 0 reads every source to its end. Every non-empty line counts toward the limit,
// whether or not the caller can parse it, so the cursor always advances by whole lines.
//
// When the page ends exactly at the end of a source and a later source exists, HasMore is
// true and the next page starts at that source; that page is empty if the source is.
func ReadBatchResultsPage(sources []BatchResultsSource, after *string, limit int, onLine func(source int, line []byte)) (*BatchResultsPage, *schemas.BifrostError) {
	cursor, err := DecodeBatchResultsCursor(after)
	if err != nil {
		return nil, NewInvalidResultsCursorError(err)
	}
	page := &BatchResultsPage{}
	if cursor.Source >= len(sources) {
		return page, nil
	}

	remaining := limit
	offset := cursor.Offset
	for source := cursor.Source; source < len(sources); source++ {
		stream, bifrostErr := sources[source](offset)
		if bifrostErr != nil {
			return nil, bifrostErr
		}
		if stream == nil {
			offset = 0
			continue
		}
		page.Latency += stream.Latency
		read := 0
		consumed, more, readErr := ReadJSONLPage(stream.Reader, remaining, func(line []byte) {
			read++
			onLine(source, line)
		})
		stream.Close()
		if readErr != nil {
			return nil, NewBifrostUpstreamConnectionError("failed to read batch results", readErr)
		}
		if more {
			page.HasMore = true
			page.NextCursor = schemas.Ptr(BatchResultsCursor{Source: source, Offset: offset + consumed}.Encode())
			return page, nil
		}
		if limit > 0 {
			remaining -= read
			if remaining <= 0 {
				if source+1 < len(sources) {
					page.HasMore = true
					page.NextCursor = schemas.Ptr(BatchResultsCursor{Source: source + 1}.Encode())
				}
				return page, nil
			}
		}
		offset = 0
	}
	return page, nil
}
