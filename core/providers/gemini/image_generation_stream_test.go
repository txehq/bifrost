package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

func passThroughPostHook(_ *schemas.BifrostContext, r *schemas.BifrostResponse, e *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
	return r, e
}

// A Gemini image stream emits each image of :streamGenerateContent as a completed event, the
// last one carrying the usage; a stream with no image is an error.
func TestGeminiProviderImageGenerationStream(t *testing.T) {
	events := []string{
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"Here it is"}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"AAAA"}}]}}],"responseId":"r1"}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"BBBB"}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2580,"totalTokenCount":2585,"candidatesTokensDetails":[{"modality":"IMAGE","tokenCount":2580}]}}`,
	}
	var path, query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, query = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			fmt.Fprintf(w, "data: %s\n\n", e)
		}
	}))
	defer srv.Close()
	provider := NewGeminiProvider(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: srv.URL + "/v1beta", DefaultRequestTimeoutInSeconds: 10}}, testNoopLogger{})
	stream := func(raw bool) []*schemas.BifrostStreamChunk { return imageStream(t, provider, raw) }

	got := stream(false)
	require.Equal(t, "/v1beta/models/gemini-3.1-flash-image:streamGenerateContent", path)
	require.Equal(t, "alt=sse", query)
	require.Len(t, got, 2)
	first, last := got[0].BifrostImageGenerationStreamResponse, got[1].BifrostImageGenerationStreamResponse
	require.Equal(t, "AAAA", first.B64JSON)
	require.Nil(t, first.Usage)
	require.Nil(t, first.ExtraFields.RawResponse)
	require.Equal(t, "BBBB", last.B64JSON)
	require.Equal(t, 1, last.Index)
	require.Equal(t, schemas.ImageGenerationEventTypeCompleted, last.Type)
	require.NotNil(t, last.Usage)

	events = []string{`{"candidates":[{"content":{"role":"model","parts":[{"text":"I can't make that."}]},"finishReason":"IMAGE_SAFETY"}]}`}
	got = stream(false)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].BifrostError)
}

// Asked for raw responses, every upstream event reaches the caller exactly once, with the
// chunk it stands for; the closing event, which carries the request's usage and no image,
// goes with the last image.
func TestGeminiProviderImageGenerationStreamRawEvents(t *testing.T) {
	events := []string{
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"Here they are"}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"AAAA"}},{"inlineData":{"mimeType":"image/png","data":"BBBB"}}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"and one more"}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"CCCC"}}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3870}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			fmt.Fprintf(w, "data: %s\n\n", e)
		}
	}))
	defer srv.Close()
	provider := NewGeminiProvider(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: srv.URL + "/v1beta", DefaultRequestTimeoutInSeconds: 10}}, testNoopLogger{})
	got := imageStream(t, provider, true)
	require.Len(t, got, 3)
	raws := make([]string, 0, 3)
	for _, c := range got {
		r := c.BifrostImageGenerationStreamResponse.ExtraFields.RawResponse
		switch v := r.(type) {
		case nil:
			raws = append(raws, "")
		case string:
			raws = append(raws, v)
		case json.RawMessage:
			raws = append(raws, string(v))
		default:
			t.Fatalf("raw response of type %T", r)
		}
	}
	require.Equal(t, "["+events[0]+","+events[1]+"]", raws[0])
	require.Equal(t, events[2], raws[1])
	require.Equal(t, "["+events[3]+","+events[4]+"]", raws[2])
	require.Equal(t, "CCCC", got[2].BifrostImageGenerationStreamResponse.B64JSON)
	require.NotNil(t, got[2].BifrostImageGenerationStreamResponse.Usage)
}

func imageStream(t *testing.T, provider *GeminiProvider, raw bool) []*schemas.BifrostStreamChunk {
	t.Helper()
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	defer ctx.Cancel()
	ctx.SetValue(schemas.BifrostContextKeyCaptureRawResponse, raw)
	ch, bifrostErr := provider.ImageGenerationStream(ctx, passThroughPostHook, func(context.Context) {}, schemas.Key{Value: *schemas.NewSecretVar("k")},
		&schemas.BifrostImageGenerationRequest{Provider: schemas.Gemini, Model: "gemini-3.1-flash-image", Input: &schemas.ImageGenerationInput{Prompt: "a lighthouse"}})
	require.Nil(t, bifrostErr)
	var out []*schemas.BifrostStreamChunk
	for chunk := range ch {
		out = append(out, chunk)
	}
	return out
}

// An Imagen :predict caller gets predictions with each image's bytes and MIME type.
func TestToImagenPredictResponse(t *testing.T) {
	out, err := ToImagenPredictResponse(context.Background(), &schemas.BifrostImageGenerationResponse{
		Data: []schemas.ImageData{{B64JSON: "AAAA"}, {B64JSON: "BBBB", Index: 1}},
	})
	require.NoError(t, err)
	require.Len(t, out.Predictions, 2)
	require.Equal(t, "BBBB", out.Predictions[1].BytesBase64Encoded)
	require.Equal(t, "image/png", out.Predictions[0].MimeType)
}
