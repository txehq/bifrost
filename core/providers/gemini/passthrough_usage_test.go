package gemini_test

import (
	"testing"

	"github.com/capsohq/bifrost/core/providers/gemini"
	"github.com/capsohq/bifrost/core/schemas"
)

func TestExtractGeminiPassthroughUsage(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		reqBody string
		body    string
		check   func(t *testing.T, u *schemas.BifrostPassthroughUsage)
	}{
		{
			name: "generateContent text with thinking tokens",
			path: "/v1beta/models/gemini-2.5-flash:generateContent",
			body: `{"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":30,"totalTokenCount":199,"thoughtsTokenCount":159,"promptTokensDetails":[{"modality":"TEXT","tokenCount":10}]}}`,
			check: func(t *testing.T, u *schemas.BifrostPassthroughUsage) {
				// thinking tokens fold into completion: 30 + 159 = 189
				geminiMustLLM(t, u, 10, 189, 199)
				if u.LLMUsage.CompletionTokensDetails == nil || u.LLMUsage.CompletionTokensDetails.ReasoningTokens != 159 {
					t.Fatalf("reasoning tokens = %+v, want 159", u.LLMUsage.CompletionTokensDetails)
				}
			},
		},
		{
			name: "generateContent image output -> ImageUsage",
			path: "/v1beta/models/gemini-2.0-flash:generateContent",
			body: `{"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":200,"totalTokenCount":205,"candidatesTokensDetails":[{"modality":"IMAGE","tokenCount":200}]}}`,
			check: func(t *testing.T, u *schemas.BifrostPassthroughUsage) {
				if u == nil || u.ImageUsage == nil || u.ImageUsage.OutputTokensDetails == nil ||
					u.ImageUsage.OutputTokensDetails.ImageTokens != 200 {
					t.Fatalf("image usage = %+v", u)
				}
			},
		},
		{
			name: "embedContent",
			path: "/v1beta/models/text-embedding-004:embedContent",
			body: `{"usageMetadata":{"promptTokenCount":7,"totalTokenCount":7}}`,
			check: func(t *testing.T, u *schemas.BifrostPassthroughUsage) {
				if u == nil || u.LLMUsage == nil || u.LLMUsage.PromptTokens != 7 || u.LLMUsage.TotalTokens != 7 {
					t.Fatalf("embedding usage = %+v", u)
				}
			},
		},
		{
			name: "predict text embedding (token_count)",
			path: "/v1/projects/p/locations/l/publishers/google/models/text-embedding-005:predict",
			body: `{"predictions":[{"embeddings":{"statistics":{"token_count":6}}}]}`,
			check: func(t *testing.T, u *schemas.BifrostPassthroughUsage) {
				if u == nil || u.LLMUsage == nil || u.LLMUsage.PromptTokens != 6 || u.LLMUsage.TotalTokens != 6 {
					t.Fatalf("text embedding usage = %+v", u)
				}
				if u.ImageUsage != nil {
					t.Fatalf("text embedding must not be billed as image: %+v", u.ImageUsage)
				}
			},
		},
		{
			name: "predict multimodal embedding -> embedding (not image)",
			path: "/v1/projects/p/locations/l/publishers/google/models/multimodalembedding@001:predict",
			body: `{"predictions":[{"textEmbedding":[0.1,0.2],"imageEmbedding":[0.3]}]}`,
			check: func(t *testing.T, u *schemas.BifrostPassthroughUsage) {
				if u == nil || u.LLMUsage == nil {
					t.Fatalf("multimodal embedding usage = %+v", u)
				}
				if u.ImageUsage != nil {
					t.Fatalf("multimodal embedding must not be billed as image: %+v", u.ImageUsage)
				}
			},
		},
		{
			name: "predict imagen -> per-image count from predictions",
			path: "/v1/projects/p/locations/l/publishers/google/models/imagen-3.0-generate-002:predict",
			body: `{"predictions":[{"bytesBase64Encoded":"aaa"},{"bytesBase64Encoded":"bbb"}]}`,
			check: func(t *testing.T, u *schemas.BifrostPassthroughUsage) {
				if u == nil || u.ImageUsage == nil || u.ImageUsage.OutputTokensDetails == nil ||
					u.ImageUsage.OutputTokensDetails.NImages != 2 {
					t.Fatalf("imagen NImages = %+v, want 2", u)
				}
			},
		},
		{
			name:    "predict imagen -> sampleCount fallback when no predictions",
			path:    "/v1/projects/p/locations/l/publishers/google/models/imagen-3.0-generate-002:predict",
			reqBody: `{"parameters":{"sampleCount":3}}`,
			body:    `{}`,
			check: func(t *testing.T, u *schemas.BifrostPassthroughUsage) {
				if u == nil || u.ImageUsage == nil || u.ImageUsage.OutputTokensDetails == nil ||
					u.ImageUsage.OutputTokensDetails.NImages != 3 {
					t.Fatalf("imagen sampleCount fallback NImages = %+v, want 3", u)
				}
			},
		},
		{
			name:    "predictLongRunning veo -> seconds from request",
			path:    "/v1beta/models/veo-3.1-generate-preview:predictLongRunning",
			reqBody: `{"parameters":{"durationSeconds":6}}`,
			body:    `{"name":"models/veo/operations/abc"}`,
			check: func(t *testing.T, u *schemas.BifrostPassthroughUsage) {
				if u == nil || u.VideoSeconds == nil || *u.VideoSeconds != 6 {
					t.Fatalf("veo seconds = %+v, want 6", u)
				}
			},
		},
		{
			name:    "predictLongRunning veo -> default seconds",
			path:    "/v1beta/models/veo-3.1-generate-preview:predictLongRunning",
			reqBody: `{}`,
			check: func(t *testing.T, u *schemas.BifrostPassthroughUsage) {
				if u == nil || u.VideoSeconds == nil || *u.VideoSeconds != 8 {
					t.Fatalf("veo default seconds = %+v, want 8", u)
				}
			},
		},
		{
			name: "interactions top-level usage (standard tier dropped)",
			path: "/v1beta/interactions",
			body: `{"usage":{"total_tokens":104,"total_input_tokens":7,"total_output_tokens":27,"total_thought_tokens":70,"total_cached_tokens":0},"service_tier":"standard"}`,
			check: func(t *testing.T, u *schemas.BifrostPassthroughUsage) {
				// output + thought folded into completion: 27 + 70 = 97
				geminiMustLLM(t, u, 7, 97, 104)
				if u.LLMUsage.CompletionTokensDetails == nil || u.LLMUsage.CompletionTokensDetails.ReasoningTokens != 70 {
					t.Fatalf("reasoning tokens = %+v, want 70", u.LLMUsage.CompletionTokensDetails)
				}
				if u.ServiceTier != nil {
					t.Fatalf("standard tier should be dropped, got %v", *u.ServiceTier)
				}
			},
		},
		{
			name: "interactions nested (streaming completed) + non-standard tier",
			path: "/v1beta/interactions",
			body: `{"interaction":{"usage":{"total_tokens":50,"total_input_tokens":10,"total_output_tokens":40},"service_tier":"priority"}}`,
			check: func(t *testing.T, u *schemas.BifrostPassthroughUsage) {
				geminiMustLLM(t, u, 10, 40, 50)
				if u.ServiceTier == nil || *u.ServiceTier != "priority" {
					t.Fatalf("service tier = %v, want priority", u.ServiceTier)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := gemini.ExtractGeminiPassthroughUsage(tt.path, []byte(tt.reqBody), []byte(tt.body))
			tt.check(t, u)
		})
	}
}

func TestHasGeminiPassthroughUsage(t *testing.T) {
	tests := []struct {
		name  string
		event string
		want  bool
	}{
		{"usageMetadata", `{"usageMetadata":{"totalTokenCount":5}}`, true},
		{"interactions top-level usage", `{"usage":{"total_tokens":5}}`, true},
		{"interactions nested", `{"interaction":{"usage":{"total_tokens":5}}}`, true},
		{"content chunk (no usage)", `{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := gemini.HasGeminiPassthroughUsage([]byte(tt.event)); got != tt.want {
				t.Fatalf("HasGeminiPassthroughUsage = %v, want %v", got, tt.want)
			}
		})
	}
}

func geminiMustLLM(t *testing.T, u *schemas.BifrostPassthroughUsage, prompt, completion, total int) {
	t.Helper()
	if u == nil || u.LLMUsage == nil {
		t.Fatalf("expected LLMUsage, got %+v", u)
	}
	if u.LLMUsage.PromptTokens != prompt || u.LLMUsage.CompletionTokens != completion || u.LLMUsage.TotalTokens != total {
		t.Fatalf("LLMUsage = {prompt:%d completion:%d total:%d}, want {%d %d %d}",
			u.LLMUsage.PromptTokens, u.LLMUsage.CompletionTokens, u.LLMUsage.TotalTokens, prompt, completion, total)
	}
}

func TestClassifyPassthroughPath(t *testing.T) {
	tests := []struct {
		path string
		want schemas.RequestType
	}{
		{"/models/gemini-2.5-flash:generateContent", schemas.ResponsesRequest},
		{"/models/gemini-2.5-flash:streamGenerateContent?alt=sse", schemas.ResponsesRequest},
		{"/projects/p/locations/us-central1/publishers/google/models/gemini-2.5-pro:generateContent", schemas.ResponsesRequest},
		{"/models/gemini-2.5-flash:countTokens", schemas.CountTokensRequest},
		{"/models/text-embedding-004:embedContent", schemas.EmbeddingRequest},
		{"/models/text-embedding-004:batchEmbedContents", schemas.EmbeddingRequest},
		{"/models/imagen-4.0-generate-001:predict", schemas.ImageGenerationRequest},
		// Vertex serves embedding models on :predict too; they are embeddings, not images.
		{"/projects/p/locations/us-central1/publishers/google/models/multimodalembedding@001:predict", schemas.EmbeddingRequest},
		{"/projects/p/locations/us-central1/publishers/google/models/text-embedding-005:predict", schemas.EmbeddingRequest},
		{"/projects/p/locations/us-central1/publishers/google/models/gemini-embedding-001:predict?alt=json", schemas.EmbeddingRequest},
		{"/models/veo-3.0-generate-001:predictLongRunning", schemas.VideoGenerationRequest},
		{"/interactions", ""},
		{"/interactions/abc:cancel", ""},
		{"/models", ""},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := gemini.ClassifyPassthroughPath(tt.path); got != tt.want {
				t.Fatalf("ClassifyPassthroughPath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestIsInteractionsPassthroughPath(t *testing.T) {
	if !gemini.IsInteractionsPassthroughPath("/v1beta/interactions?alt=sse") {
		t.Fatal("expected /interactions to be an Interactions path")
	}
	if gemini.IsInteractionsPassthroughPath("/models/gemini-2.5-flash:generateContent") {
		t.Fatal("generateContent is not an Interactions path")
	}
}

// TestExtractGeminiPassthroughUsage_EmbeddingPredictBilling pins that a Vertex embedding model on
// :predict is billed from its predict response (token statistics) whatever the path classifies as,
// and that an Imagen :predict is still billed per image.
func TestExtractGeminiPassthroughUsage_EmbeddingPredictBilling(t *testing.T) {
	embedding := gemini.ExtractGeminiPassthroughUsage(
		"/projects/p/locations/us-central1/publishers/google/models/text-embedding-005:predict",
		[]byte(`{"instances":[{"content":"hi"}]}`),
		[]byte(`{"predictions":[{"embeddings":{"values":[0.1,0.2],"statistics":{"token_count":7,"truncated":false}}}]}`),
	)
	if embedding == nil || embedding.LLMUsage == nil || embedding.LLMUsage.PromptTokens != 7 || embedding.LLMUsage.TotalTokens != 7 {
		t.Fatalf("embedding :predict usage = %+v, want 7 prompt/total tokens", embedding)
	}

	imagen := gemini.ExtractGeminiPassthroughUsage(
		"/models/imagen-4.0-generate-001:predict",
		[]byte(`{"instances":[{"prompt":"a fox"}],"parameters":{"sampleCount":1}}`),
		[]byte(`{"predictions":[{"bytesBase64Encoded":"aGk=","mimeType":"image/png"}]}`),
	)
	if imagen == nil || imagen.ImageUsage == nil {
		t.Fatalf("imagen :predict usage = %+v, want per-image usage", imagen)
	}
}

// TestExtractGeminiPassthroughUsage_VideoEmbeddingInputs pins TXE-7408: a passthrough embedding
// keeps what its video is priced by. Gemini Embedding 2's :embedContent keeps the per-modality
// prompt split (video tokens take input_cost_per_video_token); Vertex multimodalembedding's
// :predict keeps the embedded segments and the interval the request asked for.
func TestExtractGeminiPassthroughUsage_VideoEmbeddingInputs(t *testing.T) {
	gemini2 := gemini.ExtractGeminiPassthroughUsage(
		"/projects/p/locations/us-central1/publishers/google/models/gemini-embedding-2:embedContent",
		[]byte(`{"content":{"parts":[{"fileData":{"fileUri":"gs://b/clip.mp4","mimeType":"video/mp4"}}]}}`),
		[]byte(`{"embedding":{"values":[0.1]},"usageMetadata":{"promptTokenCount":1300,"totalTokenCount":1300,
			"promptTokensDetails":[{"modality":"TEXT","tokenCount":42},{"modality":"VIDEO","tokenCount":1258}]}}`),
	)
	if gemini2 == nil || gemini2.LLMUsage == nil || gemini2.LLMUsage.PromptTokensDetails == nil {
		t.Fatalf(":embedContent usage = %+v, want the modality split", gemini2)
	}
	if d := gemini2.LLMUsage.PromptTokensDetails; d.VideoTokens != 1258 || d.TextTokens != 42 {
		t.Fatalf("prompt split = %+v, want 1258 video and 42 text", d)
	}

	multimodal := gemini.ExtractGeminiPassthroughUsage(
		"/projects/p/locations/us-central1/publishers/google/models/multimodalembedding@001:predict",
		[]byte(`{"instances":[{"video":{"gcsUri":"gs://b/clip.mp4","videoSegmentConfig":{"intervalSec":10}}}]}`),
		[]byte(`{"predictions":[{"videoEmbeddings":[{"startOffsetSec":0,"endOffsetSec":10,"embedding":[0.1]},{"startOffsetSec":10,"endOffsetSec":20,"embedding":[0.2]}]}]}`),
	)
	want := []schemas.EmbeddingVideoSegment{{StartOffsetSec: 0, EndOffsetSec: 10}, {StartOffsetSec: 10, EndOffsetSec: 20}}
	if multimodal == nil || len(multimodal.EmbeddingVideoSegments) != 2 || multimodal.EmbeddingVideoSegments[0] != want[0] || multimodal.EmbeddingVideoSegments[1] != want[1] {
		t.Fatalf(":predict usage = %+v, want the two 10 s segments", multimodal)
	}
	if multimodal.EmbeddingVideoIntervalSec != 10 {
		t.Fatalf("interval = %d, want the requested 10", multimodal.EmbeddingVideoIntervalSec)
	}
	// A request naming no interval leaves it to pricing's default.
	noInterval := gemini.ExtractGeminiPassthroughUsage(
		"/projects/p/locations/us-central1/publishers/google/models/multimodalembedding@001:predict",
		[]byte(`{"instances":[{"video":{"gcsUri":"gs://b/clip.mp4"}}]}`),
		[]byte(`{"predictions":[{"videoEmbeddings":[{"startOffsetSec":0,"endOffsetSec":6,"embedding":[0.1]}]}]}`),
	)
	if noInterval == nil || len(noInterval.EmbeddingVideoSegments) != 1 || noInterval.EmbeddingVideoIntervalSec != 0 {
		t.Fatalf(":predict usage without an interval = %+v", noInterval)
	}
	// Vertex's proto JSON reads the snake_case names too: every spelling is read and the largest
	// interval taken, so a body naming it twice is never priced at the shorter one.
	for name, body := range map[string]string{
		"snake case":          `{"instances":[{"video":{"gcsUri":"gs://b/c.mp4","video_segment_config":{"interval_sec":16}}}]}`,
		"both spellings":      `{"instances":[{"video":{"gcsUri":"gs://b/c.mp4","videoSegmentConfig":{"intervalSec":4,"interval_sec":16}}}]}`,
		"case variant":        `{"instances":[{"video":{"gcsUri":"gs://b/c.mp4","VideoSegmentConfig":{"IntervalSec":16},"videoSegmentConfig":{"intervalSec":4}}}]}`,
		"two video instances": `{"instances":[{"video":{"gcsUri":"gs://b/a.mp4","videoSegmentConfig":{"intervalSec":4}}},{"video":{"gcsUri":"gs://b/b.mp4","videoSegmentConfig":{"intervalSec":16}}}]}`,
	} {
		u := gemini.ExtractGeminiPassthroughUsage(
			"/projects/p/locations/us-central1/publishers/google/models/multimodalembedding@001:predict",
			[]byte(body),
			[]byte(`{"predictions":[{"videoEmbeddings":[{"startOffsetSec":0,"endOffsetSec":6,"embedding":[0.1]}]}]}`),
		)
		if u == nil || u.EmbeddingVideoIntervalSec != 16 {
			t.Fatalf("%s: interval = %+v, want 16, the largest the body names", name, u)
		}
	}
}
