package gemini

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

func encodingFormatRequest(requestLevel, itemLevel *string) *schemas.BifrostEmbeddingRequest {
	item := schemas.EmbeddingInputItem{
		Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: schemas.Ptr("hi")}},
	}
	if itemLevel != nil {
		item.Params = &schemas.EmbeddingParameters{EncodingFormat: itemLevel}
	}
	req := &schemas.BifrostEmbeddingRequest{
		Provider: schemas.Gemini,
		Model:    "gemini-embedding-001",
		Input:    []schemas.EmbeddingInputItem{item},
	}
	if requestLevel != nil {
		req.Params = &schemas.EmbeddingParameters{EncodingFormat: requestLevel}
	}
	return req
}

// Google's embedding endpoints return float vectors only and take no output-format
// parameter, so encoding_format base64 (or any other non-float value), on the request or
// on one input item, is 400 caller_unsupported_parameter on encoding_format, and Gemini is
// never called (contract C4). Before, the field was dropped and float vectors came back.
func TestGeminiEmbeddingRefusesNonFloatEncodingFormat(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"embeddings":[{"values":[0.1,0.2]}]}`))
	}))
	defer ts.Close()

	provider := NewGeminiProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: ts.URL},
	}, testNoopLogger{})
	key := schemas.Key{Value: *schemas.NewSecretVar("dummy-key")}

	cases := map[string]*schemas.BifrostEmbeddingRequest{
		"request base64": encodingFormatRequest(schemas.Ptr(schemas.EmbeddingEncodingBase64), nil),
		"request int8":   encodingFormatRequest(schemas.Ptr(schemas.EmbeddingEncodingInt8), nil),
		"item base64":    encodingFormatRequest(nil, schemas.Ptr(schemas.EmbeddingEncodingBase64)),
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			_, bifrostErr := provider.Embedding(ctx, key, req)
			require.NotNil(t, bifrostErr)
			require.NotNil(t, bifrostErr.StatusCode)
			require.Equal(t, 400, *bifrostErr.StatusCode)
			require.Equal(t, schemas.ErrorTypeCallerUnsupportedParameter, bifrostErr.ExtraFields.ErrorType)
			require.NotNil(t, bifrostErr.Error)
			require.Equal(t, "encoding_format", bifrostErr.Error.Param)
		})
	}
	require.Zero(t, calls.Load(), "a refused request must not reach Gemini")

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, bifrostErr := provider.Embedding(ctx, key, encodingFormatRequest(schemas.Ptr(schemas.EmbeddingEncodingFloat), nil))
	require.Nil(t, bifrostErr, "float is what Google returns and is accepted")
	require.Equal(t, int32(1), calls.Load())
}

func TestRejectNonFloatEncodingFormatIsABadRequest(t *testing.T) {
	require.NoError(t, RejectNonFloatEncodingFormat(encodingFormatRequest(nil, nil)))
	require.NoError(t, RejectNonFloatEncodingFormat(encodingFormatRequest(schemas.Ptr(schemas.EmbeddingEncodingFloat), nil)))
	err := RejectNonFloatEncodingFormat(encodingFormatRequest(schemas.Ptr(schemas.EmbeddingEncodingBase64), nil))
	bifrostErr, ok := providerUtils.AsBifrostBadRequestError(err)
	require.True(t, ok)
	require.Equal(t, "encoding_format", bifrostErr.Error.Param)
}
