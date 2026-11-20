package utils

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/network"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// TXE-8031: a retrieve is read under a quarter of the response limit, on every provider, because
// every provider's buffered read goes through this transport. Converting a retrieve holds several
// copies of its body, so a retrieve read under the whole limit could hold several times it. With
// the limit at 4 MiB a retrieve answer of 1 MiB + 1 is refused, naming the retrieve limit, while
// the same answer to a request that is not a retrieve is read. A retrieve asks for the body
// uncompressed, and a vendor that compresses it anyway is decoded under the retrieve limit, so a
// small gzip body cannot expand to the whole response limit.
func TestContextTransport_RetrieveIsReadUnderAQuarterOfTheResponseLimit(t *testing.T) {
	network.SetMaxResponseBodySizeMB(4)
	t.Cleanup(func() { network.SetMaxResponseBodySizeMB(0) })
	require.Equal(t, int64(1<<20), network.MaxRetrieveResponseBodyBytes())

	over := strings.Repeat("x", 1<<20+1)
	at := strings.Repeat("y", 1<<20)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(over))
	require.NoError(t, zw.Close())
	require.Less(t, gz.Len(), 1<<20, "the compressed body is small on the wire")

	plain := func(body string) string {
		return fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	}
	gzipped := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\nContent-Length: %d\r\n\r\n%s", gz.Len(), gz.String())

	cases := []struct {
		name     string
		response string
		retrieve bool
		wantLen  int // 0 when the read must fail
	}{
		{"a retrieve past the retrieve limit", plain(over), true, 0},
		{"a retrieve at the retrieve limit", plain(at), true, len(at)},
		{"the same body when the request is not a retrieve", plain(over), false, len(over)},
		{"a compressed retrieve that expands past the retrieve limit", gzipped, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var acceptEncoding []string
			srv := newScriptedServer(t, func(conn net.Conn, br *bufio.Reader) {
				for {
					req := fasthttp.AcquireRequest()
					if req.Read(br) != nil {
						fasthttp.ReleaseRequest(req)
						return
					}
					mu.Lock()
					acceptEncoding = append(acceptEncoding, string(req.Header.Peek(fasthttp.HeaderAcceptEncoding)))
					mu.Unlock()
					fasthttp.ReleaseRequest(req)
					writeIgnoringClose(conn, tc.response)
				}
			})
			client := srv.client(5 * time.Second)
			req := fasthttp.AcquireRequest()
			resp := fasthttp.AcquireResponse()
			defer fasthttp.ReleaseRequest(req)
			defer fasthttp.ReleaseResponse(resp)
			req.SetRequestURI("http://scripted/v1beta/batches/b1")
			req.Header.SetMethod(http.MethodGet)
			// What a caller's Accept-Encoding passthrough would add.
			req.Header.Set(fasthttp.HeaderAcceptEncoding, "gzip")

			ctx, cancel := schemas.NewBifrostContextWithCancel(context.Background())
			defer cancel()
			ctx.SetValue(schemas.BifrostContextKeyRetrieveRequest, tc.retrieve)

			_, bifrostErr, wait := MakeRequestWithContext(ctx, client, req, resp)
			wait()
			mu.Lock()
			require.Len(t, acceptEncoding, 1, "one request, whatever the answer")
			if tc.retrieve {
				assert.Equal(t, "identity", acceptEncoding[0], "a retrieve asks for the body uncompressed")
			} else {
				assert.Equal(t, "gzip", acceptEncoding[0], "any other request keeps the caller's Accept-Encoding")
			}
			mu.Unlock()

			if tc.wantLen > 0 {
				require.Nil(t, bifrostErr)
				body, err := CheckAndDecodeBody(resp)
				require.NoError(t, err)
				assert.Len(t, body, tc.wantLen)
				return
			}
			require.NotNil(t, bifrostErr)
			assert.True(t, bifrostErr.IsBifrostError)
			assert.Equal(t, schemas.ErrorTypeBifrostInternal, bifrostErr.ExtraFields.ErrorType)
			assert.Contains(t, bifrostErr.Error.Message, "limit 1 MiB, for a retrieve, client.max_response_body_size_mb / 4")
			var tooLarge *network.ResponseBodyTooLargeError
			require.True(t, errors.As(bifrostErr.Error.Error, &tooLarge), "%v", bifrostErr.Error.Error)
			assert.True(t, tooLarge.Retrieve)
			assert.True(t, StopKeyWalk(bifrostErr), "a refused retrieve ends the walk over keys")
		})
	}
}

// The large-response path reads a body it must convert under the same retrieve limit.
func TestFinalizeResponseWithLargeDetection_ConvertedRetrieveUsesTheRetrieveLimit(t *testing.T) {
	network.SetMaxResponseBodySizeMB(4)
	t.Cleanup(func() { network.SetMaxResponseBodySizeMB(0) })

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyLargeResponseThreshold, int64(8<<20))
	ctx.SetValue(schemas.BifrostContextKeyRetrieveRequest, true)

	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	resp.SetBodyStream(strings.NewReader(strings.Repeat("x", 1<<20+1)), -1)

	_, isLarge, bifrostErr := FinalizeResponseWithLargeDetection(ctx, resp, nil)
	require.False(t, isLarge)
	require.NotNil(t, bifrostErr)
	assert.Contains(t, bifrostErr.Error.Message, "limit 1 MiB, for a retrieve")
}
