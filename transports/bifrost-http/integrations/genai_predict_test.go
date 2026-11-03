package integrations

import (
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// A :predict call is marked for the predictions shape its caller expects: an embedding
// for the text embeddings API, an image generation for Imagen.
func TestPredictIsMarkedForThePredictionsShape(t *testing.T) {
	for _, tc := range []struct {
		body      string
		embedding bool
	}{
		{`{"instances":[{"content":"hello"}]}`, true},
		{`{"instances":[{"prompt":"a lighthouse"}]}`, false},
	} {
		ctx := &fasthttp.RequestCtx{}
		ctx.SetUserValue("model", "openai/fx-model:predict")
		ctx.Request.Header.SetMethod("POST")
		ctx.Request.SetBody([]byte(tc.body))
		_, got := extractModelAndRequestType(ctx)
		embedding := ctx.UserValue(isGeminiPredictEmbeddingRequestContextKey) != nil
		imagen := ctx.UserValue(isGeminiImagenPredictRequestContextKey) != nil
		if (got == schemas.EmbeddingRequest) != tc.embedding || embedding != tc.embedding || imagen == tc.embedding {
			t.Errorf("%s: %s, embedding %v, imagen %v", tc.body, got, embedding, imagen)
		}
	}
}

// :predict is an image generation when its instances carry a prompt and an embedding when
// they carry content, whatever the model is called; the name decides only without a body.
func TestPredictRequestsImageFollowsTheBody(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		model string
		want  bool
	}{
		{"imagen prompt on a prefixed model", `{"instances":[{"prompt":"a lighthouse"}],"parameters":{"sampleCount":2}}`, "openai/gpt-image-2", true},
		{"text embedding content on an imagen-named model", `{"instances":[{"content":"hello"}]}`, "vertex/imagen-embed-test", false},
		{"text embedding content", `{"instances":[{"content":"hello","task_type":"RETRIEVAL_DOCUMENT"}]}`, "openai/text-embedding-3-small", false},
		{"no body, imagen name", ``, "imagen-4.0-generate-001", true},
		{"no body, embedding name", ``, "text-embedding-005", false},
	} {
		if got := predictRequestsImage([]byte(tc.body), tc.model); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}
