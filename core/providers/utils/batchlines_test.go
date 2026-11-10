package utils

import (
	"errors"
	"net/http"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckBatchLineModel(t *testing.T) {
	t.Run("a line may name the batch's model in another spelling", func(t *testing.T) {
		for _, tc := range []struct {
			provider    schemas.ModelProvider
			line, batch string
		}{
			{schemas.Gemini, "gemini-2.5-flash", "publishers/google/models/gemini-2.5-flash"},
			{schemas.Gemini, "models/gemini-2.5-flash", "gemini-2.5-flash"},
			{schemas.Gemini, "gemini/gemini-2.5-flash", "gemini-2.5-flash"},
			{schemas.Gemini, "publishers/google/models/gemini-2.5-flash", "gemini-2.5-flash"},
			{schemas.Anthropic, "anthropic/claude-haiku-4-5", "claude-haiku-4-5"},
		} {
			item := schemas.BatchRequestItem{CustomID: "l", Body: map[string]interface{}{"model": tc.line}}
			assert.NoError(t, CheckBatchLineModel(item, tc.provider, tc.batch, nil), "%s vs %s", tc.line, tc.batch)
		}
	})
	t.Run("an alias the key serves as the batch's model is the batch's model", func(t *testing.T) {
		aliased := schemas.BatchRequestItem{CustomID: "l", Body: map[string]interface{}{"model": "fast"}}
		resolve := func(model string) string {
			if model == "fast" {
				return "gemini-2.5-flash"
			}
			return model
		}
		assert.NoError(t, CheckBatchLineModel(aliased, schemas.Gemini, "publishers/google/models/gemini-2.5-flash", resolve))
		assert.Error(t, CheckBatchLineModel(aliased, schemas.Gemini, "gemini-2.5-pro", resolve))
	})
	t.Run("another model is a line error naming body.model", func(t *testing.T) {
		item := schemas.BatchRequestItem{CustomID: "l", Body: map[string]interface{}{"model": "claude-opus-4-5"}}
		err := CheckBatchLineModel(item, schemas.Anthropic, "claude-haiku-4-5", nil)
		var lineErr *schemas.BatchLineError
		require.True(t, errors.As(err, &lineErr))
		assert.Equal(t, "body.model", lineErr.Param)
	})
}

func TestNewBatchLineBadRequestError(t *testing.T) {
	bifrostErr := NewBatchLineBadRequestError(3, &schemas.BatchLineError{CustomID: "c", Param: "url", Message: "bad url"})
	require.NotNil(t, bifrostErr.StatusCode)
	assert.Equal(t, http.StatusBadRequest, *bifrostErr.StatusCode)
	assert.Equal(t, "requests[3].url", bifrostErr.Error.Param)
	assert.Contains(t, bifrostErr.Error.Message, "batch request item 3")

	other := NewBatchLineBadRequestError(1, errors.New("boom"))
	assert.Nil(t, other.StatusCode, "an error that is not the caller's is not a 400")
}

// BenchmarkNewBatchLineBadRequestError measures the 400 built for a refused batch line, for
// both classifications: a field the batch cannot take and a malformed line.
func BenchmarkNewBatchLineBadRequestError(b *testing.B) {
	for name, unsupported := range map[string]bool{"unsupported_parameter": true, "invalid_request": false} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				lineErr := &schemas.BatchLineError{CustomID: "line-1", Param: "url", Unsupported: unsupported, Message: "url /v1/embeddings cannot run in a generateContent batch"}
				if bifrostErr := NewBatchLineBadRequestError(1, lineErr); bifrostErr.StatusCode == nil {
					b.Fatal("no status code")
				}
			}
		})
	}
}
