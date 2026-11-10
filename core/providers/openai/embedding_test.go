package openai_test

import (
	"context"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/providers/openai"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

// The OpenAI converter is shared by OpenAI-compatible providers, so its errors must not name openai.
func TestToOpenAIEmbeddingRequestErrorsDoNotNameOpenAI(t *testing.T) {
	text := "hello"
	dims := 8
	cases := map[string][]schemas.EmbeddingInputItem{
		"image input": {
			{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeImage, Image: &schemas.EmbeddingMediaPart{URL: schemas.Ptr("https://example.com/a.png")}}}},
		},
		"mixed text and tokens": {
			{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &text}, {Type: schemas.EmbeddingContentPartTypeTokens, Tokens: []int{1, 2}}}},
		},
		"per-item params": {
			{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &text}}, Params: &schemas.EmbeddingParameters{Dimensions: &dims}},
		},
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := openai.ToOpenAIEmbeddingRequest(&schemas.BifrostEmbeddingRequest{
				Provider: schemas.Azure,
				Model:    "text-embedding-3-small",
				Input:    input,
			}, schemas.Azure)
			require.Error(t, err)
			require.Contains(t, err.Error(), "this provider")
			require.NotContains(t, err.Error(), "openai")
		})
	}
}

// task_type/title/auto_truncate are not OpenAI wire fields; strict OpenAI-compatible
// providers (Mistral) reject them with 422, so only encoding_format and dimensions go out.
func TestToOpenAIEmbeddingRequestSendsOnlyOpenAIParams(t *testing.T) {
	text := "hello"
	dims := 8
	format := "float"
	req, err := openai.ToOpenAIEmbeddingRequest(&schemas.BifrostEmbeddingRequest{
		Provider: schemas.Mistral,
		Model:    "mistral-embed",
		Input:    []schemas.EmbeddingInputItem{{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &text}}}},
		Params: &schemas.EmbeddingParameters{
			EncodingFormat: &format,
			Dimensions:     &dims,
			TaskType:       schemas.Ptr("RETRIEVAL_QUERY"),
			Title:          schemas.Ptr("t"),
			AutoTruncate:   schemas.Ptr(true),
		},
	}, schemas.Mistral)
	require.NoError(t, err)

	body, err := schemas.MarshalSorted(req)
	require.NoError(t, err)
	require.JSONEq(t, `{"model":"mistral-embed","input":"hello","encoding_format":"float","dimensions":8}`, string(body))
}

// BenchmarkEmbeddingRequestBody measures the OpenAI embedding body build that
// /v1/embeddings now always runs with the passthrough flag set (TXE-6635, contract
// C4): "flag off" is the pre-change path for a caller without
// x-bf-passthrough-extra-params, "flag on" the path every caller now takes. With
// no extra fields the merge is skipped, so on and off must cost the same; with
// extra fields "on" adds the JSON merge that delivers them.
func BenchmarkEmbeddingRequestBody(b *testing.B) {
	text := "The quick brown fox jumps over the lazy dog"
	dims := 256
	extras := map[string]interface{}{
		"user":       "tokun-user-1",
		"input_type": "query",
		"provider":   map[string]interface{}{"only": []interface{}{"vendor-a"}},
	}
	for _, extra := range []struct {
		name   string
		params map[string]interface{}
	}{{"no extras", nil}, {"3 extras", extras}} {
		for _, flag := range []bool{false, true} {
			name := extra.name + "/flag off"
			if flag {
				name = extra.name + "/flag on"
			}
			b.Run(name, func(b *testing.B) {
				req := &schemas.BifrostEmbeddingRequest{
					Provider: schemas.OpenAI,
					Model:    "text-embedding-3-small",
					Input:    []schemas.EmbeddingInputItem{{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &text}}}},
					Params:   &schemas.EmbeddingParameters{Dimensions: &dims, ExtraParams: extra.params},
				}
				ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
				if flag {
					ctx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					body, err := providerUtils.CheckContextAndGetRequestBody(ctx, req, func() (providerUtils.RequestBodyWithExtraParams, error) {
						return openai.ToOpenAIEmbeddingRequest(req, schemas.OpenAI)
					})
					if err != nil || len(body) == 0 {
						b.Fatalf("body build failed: %v", err)
					}
				}
			})
		}
	}
}
