package handlers

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/tidwall/gjson"
	"github.com/valyala/fasthttp"
)

// A "data: [DONE]" marker states that the stream finished normally. Emitting it
// after an error frame reproduces, one layer up, the same ambiguity that made
// truncated upstream streams look successful: a client keyed on the marker sees
// a clean end either way. See https://github.com/capsohq/bifrost/issues/5546.

// serveStreamingResponse runs handleStreamingResponse against an in-process
// connection and returns the raw SSE body the client would receive.
func serveStreamingResponse(t *testing.T, chunks []*schemas.BifrostStreamChunk) string {
	t.Helper()
	return serveStreamingResponseInPlacement(t, chunks, false)
}

// serveStreamingResponseInPlacement is serveStreamingResponse with
// client.extra_fields_placement "headers" when extraFieldsInHeaders is set.
func serveStreamingResponseInPlacement(t *testing.T, chunks []*schemas.BifrostStreamChunk, extraFieldsInHeaders bool) string {
	t.Helper()

	handler := func(reqCtx *fasthttp.RequestCtx) {
		if extraFieldsInHeaders {
			reqCtx.SetUserValue(lib.FastHTTPUserValueExtraFieldsInHeaders, true)
		}
		// A zero Config loads no transport plugins, so no chunk interceptor runs.
		h := &CompletionHandler{config: &lib.Config{}}

		base, cancel := context.WithCancel(context.Background())
		bifrostCtx := schemas.NewBifrostContext(base, schemas.NoDeadline)

		getStream := func() (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
			stream := make(chan *schemas.BifrostStreamChunk, len(chunks))
			for _, chunk := range chunks {
				stream <- chunk
			}
			close(stream)
			return stream, nil
		}

		h.handleStreamingResponse(reqCtx, bifrostCtx, schemas.ChatCompletionStreamRequest, getStream, cancel)
	}

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	go func() {
		_ = fasthttp.ServeConn(serverConn, handler)
	}()

	if err := clientConn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatalf("failed to set deadline: %v", err)
	}
	if _, err := clientConn.Write([]byte("POST /v1/chat/completions HTTP/1.1\r\nHost: test\r\n\r\n")); err != nil {
		t.Fatalf("failed to write request: %v", err)
	}

	br := bufio.NewReader(clientConn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("failed to read response header: %v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}

	var body strings.Builder
	for {
		sizeLine, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("failed to read chunk size: %v", err)
		}
		var chunkSize int
		if _, err := fmt.Sscanf(strings.TrimSpace(sizeLine), "%x", &chunkSize); err != nil {
			t.Fatalf("failed to parse chunk size %q: %v", sizeLine, err)
		}
		if chunkSize == 0 {
			break
		}
		data := make([]byte, chunkSize+2) // +2 for the trailing CRLF
		for n := 0; n < len(data); {
			nn, err := br.Read(data[n:])
			if err != nil {
				t.Fatalf("failed to read chunk data: %v", err)
			}
			n += nn
		}
		body.Write(data[:chunkSize])
	}
	return body.String()
}

func contentChunk(text string) *schemas.BifrostStreamChunk {
	return &schemas.BifrostStreamChunk{
		BifrostChatResponse: &schemas.BifrostChatResponse{
			ID:     "chatcmpl-repro",
			Object: "chat.completion.chunk",
			Model:  "repro-model",
			Choices: []schemas.BifrostResponseChoice{{
				Index: 0,
				ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
					Delta: &schemas.ChatStreamResponseChoiceDelta{Content: schemas.Ptr(text)},
				},
			}},
		},
	}
}

func truncationErrorChunk() *schemas.BifrostStreamChunk {
	statusCode := 502
	errorType := schemas.ProviderConnectionFailed
	return &schemas.BifrostStreamChunk{
		BifrostError: &schemas.BifrostError{
			IsBifrostError: false,
			StatusCode:     &statusCode,
			Error: &schemas.ErrorField{
				Message: schemas.ErrProviderStreamTruncated,
				Type:    &errorType,
			},
		},
	}
}

// A stream that ends on an error frame is already terminated for the client;
// appending [DONE] would report it as a clean finish.
func TestStreamingResponseSkipsDoneAfterErrorChunk(t *testing.T) {
	body := serveStreamingResponse(t, []*schemas.BifrostStreamChunk{
		contentChunk("partial answer"),
		truncationErrorChunk(),
	})

	if !strings.Contains(body, schemas.ErrProviderStreamTruncated) {
		t.Errorf("expected the error frame to reach the client, got:\n%s", body)
	}
	if strings.Contains(body, "data: [DONE]") {
		t.Errorf("[DONE] must not follow an error frame, got:\n%s", body)
	}
}

// Guard against over-suppression: a clean stream still gets its marker.
func TestStreamingResponseSendsDoneOnCleanStream(t *testing.T) {
	body := serveStreamingResponse(t, []*schemas.BifrostStreamChunk{
		contentChunk("hello"),
		contentChunk(" world"),
	})

	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("expected [DONE] to terminate a clean stream, got:\n%s", body)
	}
}

// sseDataFrames returns the data payload of every SSE event in body.
func sseDataFrames(body string) []string {
	var frames []string
	for _, event := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(event, "\n") {
			if value, ok := strings.CutPrefix(line, "data: "); ok {
				frames = append(frames, value)
			}
		}
	}
	return frames
}

// With extra_fields_placement "headers" the stream's headers go out before a provider
// fails, so x-bifrost-error-type cannot carry the error frame's classification. The frame
// keeps extra_fields.error_type and nothing else from extra_fields (TXE-6985); content
// chunks still carry no extra_fields, and body placement keeps the full extra_fields.
func TestStreamingErrorFrameKeepsErrorTypeInHeadersPlacement(t *testing.T) {
	for _, tc := range []struct {
		name     string
		inHeader bool
	}{{"headers placement", true}, {"body placement", false}} {
		t.Run(tc.name, func(t *testing.T) {
			content := contentChunk("partial answer")
			content.BifrostChatResponse.ExtraFields.Provider = schemas.OpenAI
			failure := truncationErrorChunk()
			failure.BifrostError.ExtraFields.Provider = schemas.OpenAI
			failure.BifrostError.ExtraFields.RequestType = schemas.ChatCompletionStreamRequest
			failure.BifrostError.ExtraFields.Latency = 12
			body := serveStreamingResponseInPlacement(t, []*schemas.BifrostStreamChunk{content, failure}, tc.inHeader)

			frames := sseDataFrames(body)
			if len(frames) != 2 {
				t.Fatalf("want a content frame and an error frame, got %d:\n%s", len(frames), body)
			}
			if gjson.Get(frames[0], "extra_fields").Exists() == tc.inHeader {
				t.Errorf("content frame extra_fields present=%v in %s", !tc.inHeader, frames[0])
			}
			errorFrame := frames[1]
			if got := gjson.Get(errorFrame, "extra_fields.error_type").String(); got != string(schemas.ErrorTypeProviderConnectionFailed) {
				t.Errorf("error frame error_type = %q, want %q: %s", got, schemas.ErrorTypeProviderConnectionFailed, errorFrame)
			}
			if !strings.Contains(errorFrame, schemas.ErrProviderStreamTruncated) {
				t.Errorf("error frame lost its message: %s", errorFrame)
			}
			if tc.inHeader {
				if got := gjson.Get(errorFrame, "extra_fields").Raw; got != `{"error_type":"provider_connection_failed"}` {
					t.Errorf("headers placement keeps only error_type in extra_fields, got %s", got)
				}
			} else if gjson.Get(errorFrame, "extra_fields.provider").String() != "openai" || gjson.Get(errorFrame, "extra_fields.latency").Int() != 12 {
				t.Errorf("body placement keeps the full extra_fields, got %s", errorFrame)
			}
		})
	}
}
