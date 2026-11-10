package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The image converters run once per retry/fallback attempt on the same Bifrost
// request and remove the keys they map into typed fields from the outbound
// ExtraParams. They used to alias the source map, so from the second attempt on
// the typed fields were empty and the Bifrost request had lost the keys.
// Regression for issue #7826; the chat and Responses converters got the same
// fix in #7138.

func imageTestPNG(t *testing.T) []byte {
	t.Helper()
	pngPixel, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==",
	)
	require.NoError(t, err)
	return pngPixel
}

func geminiImageSafetySettings() []interface{} {
	return []interface{}{
		map[string]interface{}{
			"category":  "HARM_CATEGORY_DANGEROUS_CONTENT",
			"threshold": "BLOCK_ONLY_HIGH",
		},
	}
}

func assertGeminiImageRequestCarriesExtraParams(t *testing.T, attempt int, geminiReq *GeminiGenerationRequest) {
	t.Helper()
	require.NotNil(t, geminiReq, "attempt %d", attempt)
	require.Len(t, geminiReq.SafetySettings, 1, "attempt %d: safetySettings", attempt)
	assert.Equal(t, "HARM_CATEGORY_DANGEROUS_CONTENT", geminiReq.SafetySettings[0].Category, "attempt %d", attempt)
	assert.Equal(t, "BLOCK_ONLY_HIGH", geminiReq.SafetySettings[0].Threshold, "attempt %d", attempt)
	assert.Equal(t, "cachedContents/abc123", geminiReq.CachedContent, "attempt %d: cachedContent", attempt)
	assert.Equal(t, map[string]string{"team": "platform"}, geminiReq.Labels, "attempt %d: labels", attempt)
	// Consumed keys must not leak onto the wire; unrelated passthrough params still do.
	assert.Equal(t, map[string]interface{}{"custom_passthrough": "keep-me"}, geminiReq.GetExtraParams(), "attempt %d: wire extra params", attempt)
}

func TestToGeminiImageGenerationRequest_ExtraParamsSurviveRetries(t *testing.T) {
	cases := map[string]map[string]interface{}{
		"camelCase": {
			"safetySettings":     geminiImageSafetySettings(),
			"cachedContent":      "cachedContents/abc123",
			"labels":             map[string]interface{}{"team": "platform"},
			"custom_passthrough": "keep-me",
		},
		"snake_case": {
			"safety_settings":    geminiImageSafetySettings(),
			"cached_content":     "cachedContents/abc123",
			"labels":             map[string]interface{}{"team": "platform"},
			"custom_passthrough": "keep-me",
		},
	}
	for name, extraParams := range cases {
		t.Run(name, func(t *testing.T) {
			bifrostReq := &schemas.BifrostImageGenerationRequest{
				Provider: schemas.Vertex,
				Model:    "gemini-2.5-flash-image",
				Input:    &schemas.ImageGenerationInput{Prompt: "a red apple"},
				Params:   &schemas.ImageGenerationParameters{ExtraParams: extraParams},
			}

			for attempt := 1; attempt <= 3; attempt++ {
				assertGeminiImageRequestCarriesExtraParams(t, attempt, ToGeminiImageGenerationRequest(bifrostReq))
			}

			// The Bifrost request itself must be left intact for the next attempt.
			assert.Len(t, bifrostReq.Params.ExtraParams, 4)
			assert.Contains(t, bifrostReq.Params.ExtraParams, "labels")
		})
	}
}

func TestToGeminiImageEditRequest_ExtraParamsSurviveRetries(t *testing.T) {
	cases := map[string]map[string]interface{}{
		"camelCase": {
			"safetySettings":     geminiImageSafetySettings(),
			"cachedContent":      "cachedContents/abc123",
			"labels":             map[string]interface{}{"team": "platform"},
			"custom_passthrough": "keep-me",
		},
		"snake_case": {
			"safety_settings":    geminiImageSafetySettings(),
			"cached_content":     "cachedContents/abc123",
			"labels":             map[string]interface{}{"team": "platform"},
			"custom_passthrough": "keep-me",
		},
	}
	for name, extraParams := range cases {
		t.Run(name, func(t *testing.T) {
			bifrostReq := &schemas.BifrostImageEditRequest{
				Provider: schemas.Vertex,
				Model:    "gemini-2.5-flash-image",
				Input: &schemas.ImageEditInput{
					Prompt: "make it pop",
					Images: []schemas.ImageInput{{Image: imageTestPNG(t)}},
				},
				Params: &schemas.ImageEditParameters{ExtraParams: extraParams},
			}

			for attempt := 1; attempt <= 3; attempt++ {
				assertGeminiImageRequestCarriesExtraParams(t, attempt, ToGeminiImageEditRequest(bifrostReq))
			}

			assert.Len(t, bifrostReq.Params.ExtraParams, 4)
			assert.Contains(t, bifrostReq.Params.ExtraParams, "labels")
		})
	}
}

func TestToImagenImageGenerationRequest_ExtraParamsSurviveRetries(t *testing.T) {
	bifrostReq := &schemas.BifrostImageGenerationRequest{
		Provider: schemas.Vertex,
		Model:    "imagen-4.0-generate-001",
		Input:    &schemas.ImageGenerationInput{Prompt: "a red apple"},
		Params: &schemas.ImageGenerationParameters{
			ExtraParams: map[string]interface{}{
				"addWatermark":       true,
				"sampleImageSize":    "2K",
				"aspectRatio":        "16:9",
				"personGeneration":   "allow_adult",
				"language":           "en",
				"enhancePrompt":      false,
				"safetySettings":     geminiImageSafetySettings(),
				"custom_passthrough": "keep-me",
			},
		},
	}

	for attempt := 1; attempt <= 3; attempt++ {
		req := ToImagenImageGenerationRequest(bifrostReq)
		require.NotNil(t, req, "attempt %d", attempt)
		p := req.Parameters
		require.NotNil(t, p.AddWatermark, "attempt %d: addWatermark", attempt)
		assert.True(t, *p.AddWatermark, "attempt %d", attempt)
		require.NotNil(t, p.SampleImageSize, "attempt %d: sampleImageSize", attempt)
		assert.Equal(t, "2K", *p.SampleImageSize, "attempt %d", attempt)
		require.NotNil(t, p.AspectRatio, "attempt %d: aspectRatio", attempt)
		assert.Equal(t, "16:9", *p.AspectRatio, "attempt %d", attempt)
		require.NotNil(t, p.PersonGeneration, "attempt %d: personGeneration", attempt)
		assert.Equal(t, "allow_adult", *p.PersonGeneration, "attempt %d", attempt)
		require.NotNil(t, p.Language, "attempt %d: language", attempt)
		assert.Equal(t, "en", *p.Language, "attempt %d", attempt)
		require.NotNil(t, p.EnhancePrompt, "attempt %d: enhancePrompt", attempt)
		assert.False(t, *p.EnhancePrompt, "attempt %d", attempt)
		require.Len(t, p.SafetySettings, 1, "attempt %d: safetySettings", attempt)
		assert.Equal(t, "HARM_CATEGORY_DANGEROUS_CONTENT", p.SafetySettings[0].Category, "attempt %d", attempt)
		assert.Equal(t, map[string]interface{}{"custom_passthrough": "keep-me"}, req.GetExtraParams(), "attempt %d: wire extra params", attempt)
	}

	assert.Len(t, bifrostReq.Params.ExtraParams, 8)
	assert.Contains(t, bifrostReq.Params.ExtraParams, "personGeneration")
}

func TestToImagenImageEditRequest_ExtraParamsSurviveRetries(t *testing.T) {
	bifrostReq := &schemas.BifrostImageEditRequest{
		Provider: schemas.Vertex,
		Model:    "imagen-3.0-capability-001",
		Input: &schemas.ImageEditInput{
			Prompt: "replace the background with a beach",
			Images: []schemas.ImageInput{{Image: imageTestPNG(t)}},
		},
		Params: &schemas.ImageEditParameters{
			ExtraParams: map[string]interface{}{
				"maskMode":                "MASK_MODE_BACKGROUND",
				"dilation":                0.05,
				"maskClasses":             []interface{}{float64(1), float64(2)},
				"editMode":                "EDIT_MODE_BGSWAP",
				"guidanceScale":           60,
				"baseSteps":               35,
				"addWatermark":            false,
				"includeRaiReason":        true,
				"includeSafetyAttributes": true,
				"personGeneration":        "allow_adult",
				"language":                "en",
				"storageUri":              "gs://bucket/out/",
				"safetySettings":          geminiImageSafetySettings(),
				"custom_passthrough":      "keep-me",
			},
		},
	}

	for attempt := 1; attempt <= 3; attempt++ {
		req := ToImagenImageEditRequest(bifrostReq)
		require.NotNil(t, req, "attempt %d", attempt)

		require.Len(t, req.Instances, 1, "attempt %d", attempt)
		var mask *ImagenReferenceImage
		for i := range req.Instances[0].ReferenceImages {
			if req.Instances[0].ReferenceImages[i].ReferenceType == "REFERENCE_TYPE_MASK" {
				mask = &req.Instances[0].ReferenceImages[i]
			}
		}
		require.NotNil(t, mask, "attempt %d: mask reference from maskMode", attempt)
		require.NotNil(t, mask.MaskImageConfig, "attempt %d", attempt)
		assert.Equal(t, "MASK_MODE_BACKGROUND", mask.MaskImageConfig.MaskMode, "attempt %d: maskMode", attempt)
		require.NotNil(t, mask.MaskImageConfig.Dilation, "attempt %d: dilation", attempt)
		assert.InDelta(t, 0.05, *mask.MaskImageConfig.Dilation, 1e-9, "attempt %d", attempt)
		assert.Equal(t, []int{1, 2}, mask.MaskImageConfig.MaskClasses, "attempt %d: maskClasses", attempt)

		p := req.Parameters
		require.NotNil(t, p.EditMode, "attempt %d: editMode", attempt)
		assert.Equal(t, "EDIT_MODE_BGSWAP", *p.EditMode, "attempt %d", attempt)
		require.NotNil(t, p.GuidanceScale, "attempt %d: guidanceScale", attempt)
		assert.Equal(t, 60, *p.GuidanceScale, "attempt %d", attempt)
		require.NotNil(t, p.BaseSteps, "attempt %d: baseSteps", attempt)
		assert.Equal(t, 35, *p.BaseSteps, "attempt %d", attempt)
		require.NotNil(t, p.AddWatermark, "attempt %d: addWatermark", attempt)
		assert.False(t, *p.AddWatermark, "attempt %d", attempt)
		require.NotNil(t, p.IncludeRaiReason, "attempt %d: includeRaiReason", attempt)
		assert.True(t, *p.IncludeRaiReason, "attempt %d", attempt)
		require.NotNil(t, p.IncludeSafetyAttributes, "attempt %d: includeSafetyAttributes", attempt)
		assert.True(t, *p.IncludeSafetyAttributes, "attempt %d", attempt)
		require.NotNil(t, p.PersonGeneration, "attempt %d: personGeneration", attempt)
		assert.Equal(t, "allow_adult", *p.PersonGeneration, "attempt %d", attempt)
		require.NotNil(t, p.Language, "attempt %d: language", attempt)
		assert.Equal(t, "en", *p.Language, "attempt %d", attempt)
		require.NotNil(t, p.StorageUri, "attempt %d: storageUri", attempt)
		assert.Equal(t, "gs://bucket/out/", *p.StorageUri, "attempt %d", attempt)
		require.Len(t, p.SafetySettings, 1, "attempt %d: safetySettings", attempt)
		assert.Equal(t, "HARM_CATEGORY_DANGEROUS_CONTENT", p.SafetySettings[0].Category, "attempt %d", attempt)
		assert.Equal(t, map[string]interface{}{"custom_passthrough": "keep-me"}, req.GetExtraParams(), "attempt %d: wire extra params", attempt)
	}

	assert.Len(t, bifrostReq.Params.ExtraParams, 14)
	assert.Contains(t, bifrostReq.Params.ExtraParams, "maskMode")
}

// imagenPartiallyFilteredResponse has the shape Imagen returns with includeRaiReason on: the blocked
// image's prediction holds only the reason, and the middle position shows the blocked image takes no
// slot in data.
const imagenPartiallyFilteredResponse = `{"predictions":[
	{"bytesBase64Encoded":"aW1hZ2Ux","mimeType":"image/png"},
	{"raiFilteredReason":"Unable to show generated images. Support codes: 58061214"},
	{"bytesBase64Encoded":"aW1hZ2Uy","mimeType":"image/png"}
]}`

func TestImagenResponse_FilteredImagesAndOutputCount(t *testing.T) {
	const reasonA = "Unable to show generated images. Support codes: 58061214"
	const reasonB = "Your current safety filter settings do not allow generation. Support codes: 17301594"

	tests := []struct {
		name         string
		body         string
		wantData     []string
		wantReasons  []string
		wantFormat   string
		wantDataJSON string
	}{
		{
			name:        "one of three blocked",
			body:        imagenPartiallyFilteredResponse,
			wantData:    []string{"aW1hZ2Ux", "aW1hZ2Uy"},
			wantReasons: []string{reasonA},
			wantFormat:  "png",
		},
		{
			name:        "first blocked still reports the delivered format",
			body:        `{"predictions":[{"raiFilteredReason":"` + reasonA + `"},{"bytesBase64Encoded":"aW1hZ2Ux","mimeType":"image/jpeg"}]}`,
			wantData:    []string{"aW1hZ2Ux"},
			wantReasons: []string{reasonA},
			wantFormat:  "jpeg",
		},
		{
			name:         "all blocked keeps one reason per image and an empty array",
			body:         `{"predictions":[{"raiFilteredReason":"` + reasonA + `"},{"raiFilteredReason":"` + reasonB + `"}]}`,
			wantData:     []string{},
			wantReasons:  []string{reasonA, reasonB},
			wantDataJSON: `"data":[]`,
		},
		{
			name:       "nothing blocked has no content_filter",
			body:       `{"predictions":[{"bytesBase64Encoded":"aW1hZ2Ux","mimeType":"image/webp"}]}`,
			wantData:   []string{"aW1hZ2Ux"},
			wantFormat: "webp",
		},
		{
			name:       "a prediction with neither bytes nor reason is skipped and not counted",
			body:       `{"predictions":[{"bytesBase64Encoded":"aW1hZ2Ux","mimeType":"image/png"},{"mimeType":"image/png"}]}`,
			wantData:   []string{"aW1hZ2Ux"},
			wantFormat: "png",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var response GeminiImagenResponse
			require.NoError(t, json.Unmarshal([]byte(tt.body), &response))

			got := response.ToBifrostImageGenerationResponse()

			require.Len(t, got.Data, len(tt.wantData))
			for i, want := range tt.wantData {
				assert.Equal(t, want, got.Data[i].B64JSON)
				assert.Equal(t, i, got.Data[i].Index, "blocked images take no slot")
			}

			require.NotNil(t, got.Usage)
			require.NotNil(t, got.Usage.OutputImages)
			assert.Equal(t, len(tt.wantData), *got.Usage.OutputImages)

			if tt.wantReasons == nil {
				assert.Nil(t, got.ContentFilter)
			} else {
				require.NotNil(t, got.ContentFilter)
				assert.Equal(t, len(tt.wantReasons), got.ContentFilter.FilteredCount)
				assert.Equal(t, tt.wantReasons, got.ContentFilter.Reasons)
			}

			if tt.wantFormat == "" {
				assert.Nil(t, got.ImageGenerationResponseParameters)
			} else {
				require.NotNil(t, got.ImageGenerationResponseParameters)
				assert.Equal(t, tt.wantFormat, got.ImageGenerationResponseParameters.OutputFormat)
			}

			wire, err := json.Marshal(got)
			require.NoError(t, err)
			if tt.wantDataJSON != "" {
				assert.Contains(t, string(wire), tt.wantDataJSON)
				assert.Contains(t, string(wire), `"output_images":0`, "zero delivered is reported, not omitted")
			}
		})
	}
}

func TestToImagenImageGenerationRequest_IncludeRaiReasonOnlyWhenSent(t *testing.T) {
	request := func(extra map[string]interface{}) *schemas.BifrostImageGenerationRequest {
		return &schemas.BifrostImageGenerationRequest{
			Provider: schemas.Vertex,
			Model:    "imagen-4.0-generate-001",
			Input:    &schemas.ImageGenerationInput{Prompt: "a red apple"},
			Params:   &schemas.ImageGenerationParameters{ExtraParams: extra},
		}
	}

	sent := ToImagenImageGenerationRequest(request(map[string]interface{}{"includeRaiReason": true}))
	require.NotNil(t, sent.Parameters.IncludeRaiReason)
	assert.True(t, *sent.Parameters.IncludeRaiReason)
	assert.Empty(t, sent.GetExtraParams(), "the converted key must not also go out as an extra")

	notSent := ToImagenImageGenerationRequest(request(map[string]interface{}{}))
	assert.Nil(t, notSent.Parameters.IncludeRaiReason, "Bifrost does not add a value the caller did not send")
}

func assertImagenUnsupportedParameter(t *testing.T, bifrostErr *schemas.BifrostError, param string) {
	t.Helper()
	require.NotNil(t, bifrostErr)
	require.NotNil(t, bifrostErr.StatusCode)
	assert.Equal(t, http.StatusBadRequest, *bifrostErr.StatusCode)
	require.NotNil(t, bifrostErr.Error)
	assert.Equal(t, param, bifrostErr.Error.Param)
	assert.Equal(t, schemas.ErrorTypeCallerUnsupportedParameter, bifrostErr.ExtraFields.ErrorType)
}

func TestGeminiImagen_UnsupportedParamsRejectedWithoutUpstreamCall(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(imagenPartiallyFilteredResponse))
	}))
	defer server.Close()

	provider := NewGeminiProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL},
	}, testNoopLogger{})
	key := schemas.Key{Value: *schemas.NewSecretVar("gemini-test")}
	newCtx := func() *schemas.BifrostContext {
		return schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	}
	generation := func(params *schemas.ImageGenerationParameters) *schemas.BifrostImageGenerationRequest {
		return &schemas.BifrostImageGenerationRequest{Provider: schemas.Gemini, Model: "imagen-4.0-generate-001", Input: &schemas.ImageGenerationInput{Prompt: "a red apple"}, Params: params}
	}
	edit := func(params *schemas.ImageEditParameters) *schemas.BifrostImageEditRequest {
		return &schemas.BifrostImageEditRequest{Provider: schemas.Gemini, Model: "imagen-3.0-capability-001", Input: &schemas.ImageEditInput{Prompt: "make it green", Images: []schemas.ImageInput{{Image: imageTestPNG(t)}}}, Params: params}
	}

	_, bifrostErr := provider.ImageGeneration(newCtx(), key, generation(&schemas.ImageGenerationParameters{Quality: schemas.Ptr("high")}))
	assertImagenUnsupportedParameter(t, bifrostErr, "quality")
	_, bifrostErr = provider.ImageGeneration(newCtx(), key, generation(&schemas.ImageGenerationParameters{Background: schemas.Ptr("transparent")}))
	assertImagenUnsupportedParameter(t, bifrostErr, "background")
	_, bifrostErr = provider.ImageEdit(newCtx(), key, edit(&schemas.ImageEditParameters{Quality: schemas.Ptr("high")}))
	assertImagenUnsupportedParameter(t, bifrostErr, "quality")
	_, bifrostErr = provider.ImageEdit(newCtx(), key, edit(&schemas.ImageEditParameters{Background: schemas.Ptr("opaque")}))
	assertImagenUnsupportedParameter(t, bifrostErr, "background")
	assert.Zero(t, calls.Load(), "a rejected request makes no upstream call")

	// Without those fields the same paths reach the vendor once and report the filtered image.
	response, bifrostErr := provider.ImageGeneration(newCtx(), key, generation(&schemas.ImageGenerationParameters{N: schemas.Ptr(3)}))
	require.Nil(t, bifrostErr)
	assert.Equal(t, int32(1), calls.Load())
	assert.Len(t, response.Data, 2)
	require.NotNil(t, response.ContentFilter)
	assert.Equal(t, 1, response.ContentFilter.FilteredCount)

	response, bifrostErr = provider.ImageEdit(newCtx(), key, edit(&schemas.ImageEditParameters{}))
	require.Nil(t, bifrostErr)
	assert.Equal(t, int32(2), calls.Load())
	require.NotNil(t, response.Usage)
	assert.Equal(t, schemas.Ptr(2), response.Usage.OutputImages)
}

func TestValidateImagenParams_NilAndAbsent(t *testing.T) {
	assert.Nil(t, ValidateImagenImageGenerationParams(nil))
	assert.Nil(t, ValidateImagenImageEditParams(nil))
	assert.Nil(t, ValidateImagenImageGenerationParams(&schemas.ImageGenerationParameters{N: schemas.Ptr(1)}))
	assert.Nil(t, ValidateImagenImageEditParams(&schemas.ImageEditParameters{N: schemas.Ptr(1)}))
}

// The common case: neither unsupported field is sent, so the check is two nil comparisons.
func BenchmarkValidateImagenImageGenerationParams(b *testing.B) {
	params := &schemas.ImageGenerationParameters{}
	b.ReportAllocs()
	for b.Loop() {
		if err := ValidateImagenImageGenerationParams(params); err != nil {
			b.Fatal(err)
		}
	}
}

// benchImagenResponse is a response of n images, with blocked of them filtered.
func benchImagenResponse(n, blocked int) *GeminiImagenResponse {
	resp := &GeminiImagenResponse{Predictions: make([]GeminiImagenPrediction, n)}
	for i := range resp.Predictions {
		if i < blocked {
			resp.Predictions[i].RaiFilteredReason = "Unable to show generated images. Support codes: 58061214"
			continue
		}
		resp.Predictions[i].BytesBase64Encoded = "aW1hZ2Ux"
		resp.Predictions[i].MimeType = "image/png"
	}
	return resp
}

func BenchmarkImagenResponseAllDelivered(b *testing.B) {
	resp := benchImagenResponse(4, 0)
	b.ReportAllocs()
	for b.Loop() {
		resp.ToBifrostImageGenerationResponse()
	}
}

func BenchmarkImagenResponseOneBlocked(b *testing.B) {
	resp := benchImagenResponse(4, 1)
	b.ReportAllocs()
	for b.Loop() {
		resp.ToBifrostImageGenerationResponse()
	}
}
