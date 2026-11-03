package bedrock

import (
	"context"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
)

// The invoke route classifies by the body, so a model routed by provider prefix whose name
// does not say "embed" is still an embedding when the body is embedding-shaped, and Titan
// text generation stays text generation.
func TestDetectInvokeRequestTypeFollowsTheBody(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		model string
		want  schemas.RequestType
	}{
		{"cohere texts on a prefixed model", `{"texts":["a","b"],"input_type":"search_document"}`, "openai/text-3-small", schemas.EmbeddingRequest},
		{"cohere images on a prefixed model", `{"images":["data:image/png;base64,AA=="],"input_type":"image"}`, "voyage/multimodal-3", schemas.EmbeddingRequest},
		{"cohere inputs", `{"inputs":[{"content":[{"type":"text","text":"a"}]}]}`, "cohere.embed-v4:0", schemas.EmbeddingRequest},
		{"titan embedding options on a prefixed model", `{"inputText":"a","dimensions":512,"normalize":true}`, "gemini/text-004", schemas.EmbeddingRequest},
		{"titan embedding by model id", `{"inputText":"a"}`, "amazon.titan-embed-text-v2:0", schemas.EmbeddingRequest},
		{"titan text generation", `{"inputText":"a","textGenerationConfig":{"maxTokenCount":64}}`, "amazon.titan-text-express-v1", schemas.TextCompletionRequest},
		{"titan text generation on an embed-named model id", `{"inputText":"a","textGenerationConfig":{"maxTokenCount":64}}`, "custom/embed-and-generate", schemas.TextCompletionRequest},
		{"titan image variation keeps its task", `{"taskType":"IMAGE_VARIATION","imageVariationParams":{"images":["AA=="],"text":"x"}}`, "amazon.titan-image-generator-v2:0", schemas.ImageVariationRequest},
	} {
		if got := DetectInvokeRequestType([]byte(tc.body), tc.model); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

// A Cohere-shaped request answered by another provider comes back in the Cohere envelope
// even for one input; a Titan-shaped one in the Titan envelope.
func TestEmbeddingInvokeResponseKeepsTheCallersEnvelope(t *testing.T) {
	served := &schemas.BifrostEmbeddingResponse{
		Model: "text-embedding-3-small",
		Data:  []schemas.EmbeddingData{{Index: 0, Embedding: schemas.EmbeddingStruct{EmbeddingArray: []float64{0.1, 0.2}}}},
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 3, TotalTokens: 3},
	}
	for _, tc := range []struct {
		name    string
		request BedrockInvokeRequest
		cohere  bool
	}{
		{"cohere texts", BedrockInvokeRequest{ModelID: "openai%2Ftext-embedding-3-small", Texts: []string{"a"}}, true},
		{"titan inputText", BedrockInvokeRequest{ModelID: "openai%2Ftext-embedding-3-small", InputText: "a"}, false},
	} {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		if _, err := tc.request.ToBifrostEmbeddingRequest(ctx); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		out, err := ToBedrockEmbeddingInvokeResponse(ctx, served)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		_, isTitan := out.(*BedrockInvokeEmbeddingResp)
		if tc.cohere == isTitan {
			t.Fatalf("%s: envelope %T", tc.name, out)
		}
	}
}
