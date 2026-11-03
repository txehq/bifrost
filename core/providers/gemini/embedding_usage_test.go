package gemini

import (
	"testing"

	"github.com/bytedance/sonic"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func embeddingResponse(n, prompt int) *schemas.BifrostEmbeddingResponse {
	resp := &schemas.BifrostEmbeddingResponse{Usage: &schemas.BifrostLLMUsage{PromptTokens: prompt, TotalTokens: prompt}}
	for i := 0; i < n; i++ {
		resp.Data = append(resp.Data, schemas.EmbeddingData{Index: i, Embedding: schemas.EmbeddingStruct{EmbeddingArray: []float64{float64(i), 0.5}}})
	}
	return resp
}

// A converted embedding response reports its tokens in usageMetadata, as Gemini does,
// and never as a character count.
func TestGeminiEmbeddingResponsesReportTokensInUsageMetadata(t *testing.T) {
	batch, err := sonic.Marshal(ToGeminiEmbeddingResponse(embeddingResponse(2, 9)))
	require.NoError(t, err)
	require.Equal(t, int64(9), gjson.GetBytes(batch, "usageMetadata.promptTokenCount").Int())
	require.False(t, gjson.GetBytes(batch, "metadata.billableCharacterCount").Exists())
	require.Len(t, gjson.GetBytes(batch, "embeddings").Array(), 2)

	single, err := sonic.Marshal(ToGeminiEmbedContentResponse(embeddingResponse(1, 4)))
	require.NoError(t, err)
	require.Equal(t, int64(4), gjson.GetBytes(single, "usageMetadata.promptTokenCount").Int())
	require.Equal(t, int64(4), gjson.GetBytes(single, "embedding.statistics.tokenCount").Int())

	noUsage := embeddingResponse(1, 0)
	noUsage.Usage = nil
	none, err := sonic.Marshal(ToGeminiEmbedContentResponse(noUsage))
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(none, "usageMetadata").Exists())
}

// A :predict caller gets the text embeddings API's predictions back, with the request's
// tokens in usageMetadata and a prediction's own only when it is the only one.
func TestToGeminiPredictEmbeddingResponse(t *testing.T) {
	many, err := sonic.Marshal(ToGeminiPredictEmbeddingResponse(embeddingResponse(2, 11)))
	require.NoError(t, err)
	require.Len(t, gjson.GetBytes(many, "predictions").Array(), 2)
	require.Equal(t, 0.5, gjson.GetBytes(many, "predictions.1.embeddings.values.1").Float())
	require.Equal(t, int64(11), gjson.GetBytes(many, "usageMetadata.promptTokenCount").Int())
	require.False(t, gjson.GetBytes(many, "predictions.0.embeddings.statistics").Exists())

	one, err := sonic.Marshal(ToGeminiPredictEmbeddingResponse(embeddingResponse(1, 3)))
	require.NoError(t, err)
	require.Equal(t, int64(3), gjson.GetBytes(one, "predictions.0.embeddings.statistics.token_count").Int())
}

// The text embeddings API's instances become the embedding's input, with its parameters.
func TestPredictInstancesBecomeEmbeddingInput(t *testing.T) {
	req := &GeminiGenerationRequest{}
	require.NoError(t, sonic.Unmarshal([]byte(`{"instances":[{"content":"one","task_type":"RETRIEVAL_DOCUMENT","title":"t"},{"content":"two"}],
		"parameters":{"outputDimensionality":256,"autoTruncate":false}}`), req))
	req.Model = "openai/text-embedding-3-small"
	out := req.ToBifrostEmbeddingRequest(schemas.NewBifrostContext(t.Context(), schemas.NoDeadline))
	require.NotNil(t, out)
	require.Len(t, out.Input, 2)
	require.Equal(t, "one", *out.Input[0].Content[0].Text)
	require.Equal(t, "RETRIEVAL_DOCUMENT", *out.Input[0].Params.TaskType)
	require.Nil(t, out.Input[1].Params)
	require.Equal(t, 256, *out.Params.Dimensions)
	require.False(t, *out.Params.AutoTruncate)
}
