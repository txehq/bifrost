package gemini

import (
	"context"
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
	stream := func() []*schemas.BifrostStreamChunk {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		defer ctx.Cancel()
		ch, bifrostErr := provider.ImageGenerationStream(ctx, passThroughPostHook, func(context.Context) {}, schemas.Key{Value: *schemas.NewSecretVar("k")},
			&schemas.BifrostImageGenerationRequest{Provider: schemas.Gemini, Model: "gemini-3.1-flash-image", Input: &schemas.ImageGenerationInput{Prompt: "a lighthouse"}})
		require.Nil(t, bifrostErr)
		var out []*schemas.BifrostStreamChunk
		for chunk := range ch {
			out = append(out, chunk)
		}
		return out
	}

	got := stream()
	require.Equal(t, "/v1beta/models/gemini-3.1-flash-image:streamGenerateContent", path)
	require.Equal(t, "alt=sse", query)
	require.Len(t, got, 2)
	first, last := got[0].BifrostImageGenerationStreamResponse, got[1].BifrostImageGenerationStreamResponse
	require.Equal(t, "AAAA", first.B64JSON)
	require.Nil(t, first.Usage)
	require.Equal(t, "BBBB", last.B64JSON)
	require.Equal(t, 1, last.Index)
	require.Equal(t, schemas.ImageGenerationEventTypeCompleted, last.Type)
	require.NotNil(t, last.Usage)

	events = []string{`{"candidates":[{"content":{"role":"model","parts":[{"text":"I can't make that."}]},"finishReason":"IMAGE_SAFETY"}]}`}
	got = stream()
	require.Len(t, got, 1)
	require.NotNil(t, got[0].BifrostError)
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
