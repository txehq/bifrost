package gemini

import (
	"context"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Gemini bills the prompt even when it returns no transcript (for example a
// speechless clip), so usage must survive an empty-text response.
func TestToBifrostTranscriptionResponseKeepsUsageWhenTextEmpty(t *testing.T) {
	resp := &GenerateContentResponse{
		Candidates: []*Candidate{{Content: &Content{Role: string(RoleModel), Parts: []*Part{{Text: ""}}}}},
		UsageMetadata: &GenerateContentResponseUsageMetadata{
			PromptTokenCount: 12,
			TotalTokenCount:  12,
		},
	}

	got := resp.ToBifrostTranscriptionResponse()
	if got.Text != "" {
		t.Fatalf("text = %q, want empty", got.Text)
	}
	if got.Usage == nil {
		t.Fatal("usage dropped for an empty transcript")
	}
	if got.Usage.InputTokens == nil || *got.Usage.InputTokens != 12 {
		t.Errorf("input tokens = %v, want 12", got.Usage.InputTokens)
	}

	genai := ToGeminiTranscriptionResponse(got)
	if genai.UsageMetadata == nil || genai.UsageMetadata.PromptTokenCount != 12 {
		t.Errorf("genai usageMetadata = %+v, want promptTokenCount 12", genai.UsageMetadata)
	}
}

// The transcription converter runs once per retry/fallback attempt on the same
// Bifrost request and removes safety_settings, cached_content and labels from the
// outbound ExtraParams. It used to alias the source map, so the second attempt was
// sent without any of them. Regression for issue #7826.
func TestToGeminiTranscriptionRequest_ExtraParamsSurviveRetries(t *testing.T) {
	bifrostReq := &schemas.BifrostTranscriptionRequest{
		Provider: schemas.Gemini,
		Model:    "gemini-2.5-flash",
		Input:    &schemas.TranscriptionInput{File: []byte("not-really-audio"), Filename: "sample.mp3"},
		Params: &schemas.TranscriptionParameters{
			ExtraParams: map[string]interface{}{
				"safety_settings": []interface{}{
					map[string]interface{}{
						"category":  "HARM_CATEGORY_HARASSMENT",
						"threshold": "BLOCK_NONE",
					},
				},
				"cached_content":     "cachedContents/abc123",
				"labels":             map[string]interface{}{"team": "platform"},
				"custom_passthrough": "keep-me",
			},
		},
	}

	for attempt := 1; attempt <= 3; attempt++ {
		geminiReq := ToGeminiTranscriptionRequest(bifrostReq)
		require.NotNil(t, geminiReq, "attempt %d", attempt)

		require.Len(t, geminiReq.SafetySettings, 1, "attempt %d: safetySettings", attempt)
		assert.Equal(t, "HARM_CATEGORY_HARASSMENT", geminiReq.SafetySettings[0].Category, "attempt %d", attempt)
		assert.Equal(t, "BLOCK_NONE", geminiReq.SafetySettings[0].Threshold, "attempt %d", attempt)
		assert.Equal(t, "cachedContents/abc123", geminiReq.CachedContent, "attempt %d: cachedContent", attempt)
		assert.Equal(t, map[string]string{"team": "platform"}, geminiReq.Labels, "attempt %d: labels", attempt)
		assert.Equal(t, map[string]interface{}{"custom_passthrough": "keep-me"}, geminiReq.GetExtraParams(), "attempt %d: wire extra params", attempt)
	}

	// The Bifrost request itself must be left intact for the next attempt.
	assert.Contains(t, bifrostReq.Params.ExtraParams, "safety_settings")
	assert.Contains(t, bifrostReq.Params.ExtraParams, "cached_content")
	assert.Contains(t, bifrostReq.Params.ExtraParams, "labels")
	assert.Len(t, bifrostReq.Params.ExtraParams, 4)
}

// Contract C4: generateContent has no output-format field and the transcript is returned as
// JSON, so any other response_format is refused naming the field, before anything is sent.
// It used to be dropped silently.
func TestGeminiTranscriptionRejectsUndeliverableResponseFormat(t *testing.T) {
	newReq := func(format *string) *schemas.BifrostTranscriptionRequest {
		return &schemas.BifrostTranscriptionRequest{
			Provider: schemas.Gemini,
			Model:    "gemini-3.5-transcribe",
			Input:    &schemas.TranscriptionInput{File: []byte("RIFF")},
			Params:   &schemas.TranscriptionParameters{ResponseFormat: format},
		}
	}
	for _, format := range []string{"text", "srt", "verbose_json", "vtt"} {
		t.Run(format, func(t *testing.T) {
			provider, calls, _ := newGeminiSpeechTestProvider(t, `{}`, false)
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

			_, bErr := provider.Transcription(ctx, geminiSpeechTestKey(), newReq(&format))
			assertUnsupportedParameter(t, bErr, "response_format")

			stream, bErr := provider.TranscriptionStream(ctx, nil, func(context.Context) {}, geminiSpeechTestKey(), newReq(&format))
			assert.Nil(t, stream)
			assertUnsupportedParameter(t, bErr, "response_format")

			assert.Zero(t, calls.Load(), "a refused request makes no upstream call")
		})
	}
	assert.Nil(t, ValidateTranscriptionRequest(newReq(nil)))
	assert.Nil(t, ValidateTranscriptionRequest(newReq(schemas.Ptr(""))))
	assert.Nil(t, ValidateTranscriptionRequest(newReq(schemas.Ptr("json"))))
	assert.Nil(t, ValidateTranscriptionRequest(nil))
	assert.Nil(t, ValidateTranscriptionRequest(&schemas.BifrostTranscriptionRequest{}))
}

// The fields the Gemini transcription declares it takes do reach the vendor: the prompt as the
// text part, the temperature as generationConfig.temperature (it used to be dropped).
func TestToGeminiTranscriptionRequestDeliversDeclaredFields(t *testing.T) {
	geminiReq := ToGeminiTranscriptionRequest(&schemas.BifrostTranscriptionRequest{
		Provider: schemas.Gemini,
		Model:    "gemini-3.5-transcribe",
		Input:    &schemas.TranscriptionInput{File: []byte("RIFF")},
		Params: &schemas.TranscriptionParameters{
			Prompt:         schemas.Ptr("Transcribe the call."),
			Temperature:    schemas.Ptr(0.2),
			ResponseFormat: schemas.Ptr("json"),
		},
	})
	require.NotNil(t, geminiReq)
	require.NotNil(t, geminiReq.GenerationConfig.Temperature)
	assert.Equal(t, 0.2, *geminiReq.GenerationConfig.Temperature)
	require.Len(t, geminiReq.Contents, 1)
	assert.Equal(t, "Transcribe the call.", geminiReq.Contents[0].Parts[0].Text)

	wire, err := sonic.Marshal(geminiReq)
	require.NoError(t, err)
	assert.Contains(t, string(wire), `"temperature":0.2`)
	assert.NotContains(t, string(wire), "response_format")

	// No temperature, no field.
	geminiReq = ToGeminiTranscriptionRequest(&schemas.BifrostTranscriptionRequest{
		Model: "gemini-3.5-transcribe", Input: &schemas.TranscriptionInput{File: []byte("RIFF")},
	})
	wire, err = sonic.Marshal(geminiReq)
	require.NoError(t, err)
	assert.NotContains(t, string(wire), "temperature")
}
