package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/bytedance/sonic"
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

// TXE-6526 (contract C4, C5, C6): Gemini API image generation and edit (Nano Banana, served by
// generateContent). The vendor bodies below are in the shape the Gemini API documents for
// generateContent on an image model.

func requireUnsupportedParameter(t *testing.T, bifrostErr *schemas.BifrostError, param string) {
	t.Helper()
	require.NotNil(t, bifrostErr, "expected 400 caller_unsupported_parameter for %q", param)
	require.NotNil(t, bifrostErr.StatusCode)
	assert.Equal(t, 400, *bifrostErr.StatusCode)
	assert.Equal(t, schemas.ErrorTypeCallerUnsupportedParameter, bifrostErr.ExtraFields.ErrorType)
	require.NotNil(t, bifrostErr.Error)
	assert.Equal(t, param, bifrostErr.Error.Param)
}

func TestCheckGeminiImageGenerationRequest(t *testing.T) {
	cases := []struct {
		name   string
		params *schemas.ImageGenerationParameters
		param  string // "" = accepted
	}{
		{"no params", nil, ""},
		{"n 1", &schemas.ImageGenerationParameters{N: schemas.Ptr(1)}, ""},
		{"n 2", &schemas.ImageGenerationParameters{N: schemas.Ptr(2)}, "n"},
		{"n 0", &schemas.ImageGenerationParameters{N: schemas.Ptr(0)}, "n"},
		{"size with a supported ratio", &schemas.ImageGenerationParameters{Size: schemas.Ptr("1024x768")}, ""},
		{"size auto", &schemas.ImageGenerationParameters{Size: schemas.Ptr("auto")}, ""},
		{"size with no supported ratio", &schemas.ImageGenerationParameters{Size: schemas.Ptr("1000x700")}, "size"},
		{"size above every tier", &schemas.ImageGenerationParameters{Size: schemas.Ptr("8192x8192")}, "size"},
		{"size not WxH", &schemas.ImageGenerationParameters{Size: schemas.Ptr("large")}, "size"},
		{"size with no ratio beside an explicit aspect_ratio", &schemas.ImageGenerationParameters{Size: schemas.Ptr("1000x700"), AspectRatio: schemas.Ptr("16:9")}, "size"},
		{"aspect_ratio and resolution", &schemas.ImageGenerationParameters{AspectRatio: schemas.Ptr("21:9"), Resolution: "2K"}, ""},
		{"quality", &schemas.ImageGenerationParameters{Quality: schemas.Ptr("high")}, "quality"},
		{"output_format", &schemas.ImageGenerationParameters{OutputFormat: schemas.Ptr("webp")}, "output_format"},
		{"background", &schemas.ImageGenerationParameters{Background: schemas.Ptr("transparent")}, "background"},
		{"input_images", &schemas.ImageGenerationParameters{InputImages: []string{"https://example.com/a.png"}}, "input_images"},
		{"response_format b64_json", &schemas.ImageGenerationParameters{ResponseFormat: schemas.Ptr("b64_json")}, ""},
		{"response_format url", &schemas.ImageGenerationParameters{ResponseFormat: schemas.Ptr("url")}, "response_format"},
		{"seed and negative_prompt are unpriced", &schemas.ImageGenerationParameters{Seed: schemas.Ptr(7), NegativePrompt: schemas.Ptr("blur")}, ""},
		{"extra params are not unified fields", &schemas.ImageGenerationParameters{ExtraParams: map[string]interface{}{"safetySettings": geminiImageSafetySettings()}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bifrostErr := CheckGeminiImageGenerationRequest(&schemas.BifrostImageGenerationRequest{
				Provider: schemas.Gemini,
				Model:    "gemini-3.1-flash-image",
				Input:    &schemas.ImageGenerationInput{Prompt: "a red bicycle"},
				Params:   tc.params,
			})
			if tc.param == "" {
				require.Nil(t, bifrostErr)
				return
			}
			requireUnsupportedParameter(t, bifrostErr, tc.param)
		})
	}
}

func TestCheckGeminiImageEditRequest(t *testing.T) {
	png := imageTestPNG(t)
	cases := []struct {
		name   string
		images []schemas.ImageInput
		params *schemas.ImageEditParameters
		param  string
	}{
		{"bytes, no params", []schemas.ImageInput{{Image: png}}, nil, ""},
		{"URL and data URL inputs", []schemas.ImageInput{{URL: "https://example.com/a.png"}, {URL: "data:image/png;base64,iVBORw0KGgo="}}, &schemas.ImageEditParameters{Resolution: "2K"}, ""},
		{"mask", []schemas.ImageInput{{Image: png}}, &schemas.ImageEditParameters{Mask: png}, "mask"},
		{"n 2", []schemas.ImageInput{{Image: png}}, &schemas.ImageEditParameters{N: schemas.Ptr(2)}, "n"},
		{"quality", []schemas.ImageInput{{Image: png}}, &schemas.ImageEditParameters{Quality: schemas.Ptr("high")}, "quality"},
		{"output_format", []schemas.ImageInput{{Image: png}}, &schemas.ImageEditParameters{OutputFormat: schemas.Ptr("png")}, "output_format"},
		{"background", []schemas.ImageInput{{Image: png}}, &schemas.ImageEditParameters{Background: schemas.Ptr("opaque")}, "background"},
		{"input_fidelity", []schemas.ImageInput{{Image: png}}, &schemas.ImageEditParameters{InputFidelity: schemas.Ptr("high")}, "input_fidelity"},
		{"size with no supported ratio", []schemas.ImageInput{{Image: png}}, &schemas.ImageEditParameters{Size: schemas.Ptr("1000x700")}, "size"},
		{"data URL that is not base64", []schemas.ImageInput{{URL: "data:image/svg+xml,%3Csvg/%3E"}}, nil, "image"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bifrostErr := CheckGeminiImageEditRequest(&schemas.BifrostImageEditRequest{
				Provider: schemas.Gemini,
				Model:    "gemini-3.1-flash-image",
				Input:    &schemas.ImageEditInput{Images: tc.images, Prompt: "make it blue"},
				Params:   tc.params,
			})
			if tc.param == "" {
				require.Nil(t, bifrostErr)
				return
			}
			requireUnsupportedParameter(t, bifrostErr, tc.param)
		})
	}
}

// The unified resolution tier reaches the vendor as generationConfig.imageConfig.imageSize, as
// sent, and its copy in ExtraParams does not also go out as a top-level "resolution".
func TestToGeminiImageRequests_ResolutionToImageSize(t *testing.T) {
	marshal := func(t *testing.T, geminiReq *GeminiGenerationRequest) string {
		t.Helper()
		require.NotNil(t, geminiReq)
		body, err := sonic.Marshal(geminiReq)
		require.NoError(t, err)
		return string(body)
	}

	t.Run("generation, resolution only", func(t *testing.T) {
		geminiReq := ToGeminiImageGenerationRequest(&schemas.BifrostImageGenerationRequest{
			Model: "gemini-3.1-flash-image",
			Input: &schemas.ImageGenerationInput{Prompt: "a red bicycle"},
			Params: &schemas.ImageGenerationParameters{
				Resolution:  "2K",
				ExtraParams: map[string]interface{}{"resolution": "2K", "custom_passthrough": "keep-me"},
			},
		})
		body := marshal(t, geminiReq)
		assert.Contains(t, body, `"imageConfig":{"imageSize":"2K"}`)
		assert.Equal(t, map[string]interface{}{"custom_passthrough": "keep-me"}, geminiReq.GetExtraParams())
	})

	t.Run("generation, resolution wins over the tier derived from size", func(t *testing.T) {
		geminiReq := ToGeminiImageGenerationRequest(&schemas.BifrostImageGenerationRequest{
			Model:  "gemini-3.1-flash-image",
			Input:  &schemas.ImageGenerationInput{Prompt: "a red bicycle"},
			Params: &schemas.ImageGenerationParameters{Size: schemas.Ptr("1024x1024"), Resolution: "4K"},
		})
		require.NotNil(t, geminiReq.GenerationConfig.ImageConfig)
		assert.Equal(t, "1:1", geminiReq.GenerationConfig.ImageConfig.AspectRatio)
		assert.Equal(t, "4K", geminiReq.GenerationConfig.ImageConfig.ImageSize)
	})

	t.Run("generation, a non-string resolution extra is left alone", func(t *testing.T) {
		geminiReq := ToGeminiImageGenerationRequest(&schemas.BifrostImageGenerationRequest{
			Model:  "gemini-3.1-flash-image",
			Input:  &schemas.ImageGenerationInput{Prompt: "a red bicycle"},
			Params: &schemas.ImageGenerationParameters{ExtraParams: map[string]interface{}{"resolution": 1024}},
		})
		assert.Nil(t, geminiReq.GenerationConfig.ImageConfig)
		assert.Equal(t, map[string]interface{}{"resolution": 1024}, geminiReq.GetExtraParams())
	})

	t.Run("edit", func(t *testing.T) {
		geminiReq := ToGeminiImageEditRequest(&schemas.BifrostImageEditRequest{
			Model: "gemini-3.1-flash-image",
			Input: &schemas.ImageEditInput{Images: []schemas.ImageInput{{Image: imageTestPNG(t)}}, Prompt: "make it blue"},
			Params: &schemas.ImageEditParameters{
				AspectRatio: schemas.Ptr("16:9"),
				Resolution:  "2K",
				ExtraParams: map[string]interface{}{"resolution": "2K"},
			},
		})
		body := marshal(t, geminiReq)
		assert.Contains(t, body, `"imageConfig":{"aspectRatio":"16:9","imageSize":"2K"}`)
		assert.Empty(t, geminiReq.GetExtraParams())
	})
}

// An edit sends a URL input image unchanged as fileData{fileUri}; bytes and a base64 data URL go
// as inlineData. URL inputs used to be skipped, which turned an all-URL edit into a generation.
func TestToGeminiImageEditRequest_InputImages(t *testing.T) {
	png := imageTestPNG(t)
	pngB64 := base64.StdEncoding.EncodeToString(png)
	geminiReq := ToGeminiImageEditRequest(&schemas.BifrostImageEditRequest{
		Model: "gemini-3.1-flash-image",
		Input: &schemas.ImageEditInput{
			Prompt: "combine them",
			Images: []schemas.ImageInput{
				{URL: "https://example.com/cat.jpg?sig=abc"},
				{Image: png},
				{URL: "data:image/webp;base64," + pngB64},
				{URL: "https://example.com/asset/42"},
				{},
			},
		},
	})
	require.NotNil(t, geminiReq)
	require.Len(t, geminiReq.Contents, 1)
	parts := geminiReq.Contents[0].Parts
	require.Len(t, parts, 5, "prompt plus four images; the empty input sends nothing")
	assert.Equal(t, "combine them", parts[0].Text)
	require.NotNil(t, parts[1].FileData)
	assert.Equal(t, "https://example.com/cat.jpg?sig=abc", parts[1].FileData.FileURI)
	assert.Equal(t, "image/jpeg", parts[1].FileData.MIMEType)
	assert.Nil(t, parts[1].InlineData)
	require.NotNil(t, parts[2].InlineData)
	assert.Equal(t, "image/png", parts[2].InlineData.MIMEType)
	assert.Equal(t, pngB64, parts[2].InlineData.Data)
	require.NotNil(t, parts[3].InlineData)
	assert.Equal(t, "image/webp", parts[3].InlineData.MIMEType)
	assert.Equal(t, pngB64, parts[3].InlineData.Data)
	require.NotNil(t, parts[4].FileData)
	assert.Equal(t, "https://example.com/asset/42", parts[4].FileData.FileURI)
	assert.Empty(t, parts[4].FileData.MIMEType, "no extension, no guessed MIME type")

	body, err := sonic.Marshal(geminiReq)
	require.NoError(t, err)
	assert.Contains(t, string(body), `"fileData":{"fileUri":"https://example.com/cat.jpg?sig=abc","mimeType":"image/jpeg"}`)
	assert.Contains(t, string(body), `"fileData":{"fileUri":"https://example.com/asset/42"}`)
}

const (
	geminiImageRespDelivered     = `{"candidates":[{"content":{"role":"model","parts":[{"text":"Here is your image."},{"inlineData":{"mimeType":"image/png","data":"aW1hZ2U="}}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":1300,"totalTokenCount":1512,"thoughtsTokenCount":200,"promptTokensDetails":[{"modality":"TEXT","tokenCount":12}],"candidatesTokensDetails":[{"modality":"IMAGE","tokenCount":1290},{"modality":"TEXT","tokenCount":10}]},"modelVersion":"gemini-3.1-flash-image","responseId":"resp-1"}`
	geminiImageRespSafetyFinish  = `{"candidates":[{"finishReason":"IMAGE_SAFETY","finishMessage":"Unable to show the generated image because it may violate our policies.","index":0}],"usageMetadata":{"promptTokenCount":9,"totalTokenCount":9,"promptTokensDetails":[{"modality":"TEXT","tokenCount":9}]},"modelVersion":"gemini-3.1-flash-image","responseId":"resp-2"}`
	geminiImageRespPromptBlocked = `{"promptFeedback":{"blockReason":"PROHIBITED_CONTENT"},"usageMetadata":{"promptTokenCount":7,"totalTokenCount":7},"modelVersion":"gemini-3.1-flash-image","responseId":"resp-3"}`
	geminiImageRespTextOnly      = `{"candidates":[{"content":{"role":"model","parts":[{"text":"I can't create that image."}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":7,"totalTokenCount":15,"candidatesTokensDetails":[{"modality":"TEXT","tokenCount":7}]},"modelVersion":"gemini-3.1-flash-image","responseId":"resp-4"}`
	geminiImageRespNoCandidates  = `{"usageMetadata":{"promptTokenCount":7,"totalTokenCount":7},"modelVersion":"gemini-3.1-flash-image","responseId":"resp-5"}`
	geminiImageRespNoImage       = `{"candidates":[{"finishReason":"NO_IMAGE","index":0}],"usageMetadata":{"promptTokenCount":7,"totalTokenCount":7},"modelVersion":"gemini-3.1-flash-image","responseId":"resp-6"}`
)

func decodeGeminiImageResponse(t *testing.T, body string) *GenerateContentResponse {
	t.Helper()
	var resp GenerateContentResponse
	require.NoError(t, sonic.Unmarshal([]byte(body), &resp))
	return &resp
}

// thoughtsTokenCount becomes usage.output_tokens_details.reasoning_tokens (it used to be dropped),
// and usage.output_images is the delivered count.
func TestGeminiImageResponse_UsageReasoningTokensAndOutputImages(t *testing.T) {
	resp, bifrostErr := decodeGeminiImageResponse(t, geminiImageRespDelivered).ToBifrostImageGenerationResponse()
	require.Nil(t, bifrostErr)
	require.Len(t, resp.Data, 1)
	assert.Equal(t, "aW1hZ2U=", resp.Data[0].B64JSON)
	assert.Nil(t, resp.ContentFilter)
	require.NotNil(t, resp.ImageGenerationResponseParameters)
	assert.Equal(t, "png", resp.OutputFormat)

	require.NotNil(t, resp.Usage)
	assert.Equal(t, 12, resp.Usage.InputTokens)
	assert.Equal(t, 1300, resp.Usage.OutputTokens, "candidatesTokenCount, unchanged: thinking tokens are reported beside it")
	assert.Equal(t, 1512, resp.Usage.TotalTokens)
	require.NotNil(t, resp.Usage.OutputTokensDetails)
	assert.Equal(t, 1290, resp.Usage.OutputTokensDetails.ImageTokens)
	assert.Equal(t, 10, resp.Usage.OutputTokensDetails.TextTokens)
	require.NotNil(t, resp.Usage.OutputTokensDetails.ReasoningTokens)
	assert.Equal(t, 200, *resp.Usage.OutputTokensDetails.ReasoningTokens)
	require.NotNil(t, resp.Usage.OutputImages)
	assert.Equal(t, 1, *resp.Usage.OutputImages)
	assert.Nil(t, resp.Usage.InputImages, "set only on an edit")

	wire, err := sonic.Marshal(resp)
	require.NoError(t, err)
	assert.Contains(t, string(wire), `"reasoning_tokens":200`)
	assert.Contains(t, string(wire), `"output_images":1`)

	// No thinking reported: the field stays absent.
	noThoughts, bifrostErr := decodeGeminiImageResponse(t, geminiImageRespTextOnly).ToBifrostImageGenerationResponse()
	require.Nil(t, bifrostErr)
	require.NotNil(t, noThoughts.Usage.OutputTokensDetails)
	assert.Nil(t, noThoughts.Usage.OutputTokensDetails.ReasoningTokens)

	// Thinking reported with no modality breakdown still lands.
	onlyThoughts := convertGeminiUsageMetadataToImageUsage(&GenerateContentResponseUsageMetadata{ThoughtsTokenCount: 5})
	require.NotNil(t, onlyThoughts.OutputTokensDetails)
	require.NotNil(t, onlyThoughts.OutputTokensDetails.ReasoningTokens)
	assert.Equal(t, 5, *onlyThoughts.OutputTokensDetails.ReasoningTokens)

	// The native route gets it back as thoughtsTokenCount.
	back := convertBifrostImageUsageToGeminiUsageMetadata(resp.Usage)
	assert.Equal(t, int32(200), back.ThoughtsTokenCount)
	assert.Equal(t, int32(1300), back.CandidatesTokenCount)
}

// A vendor 200 with no image and a block marker is a 200 with "data": [] and content_filter
// carrying the vendor's values. These used to be an error with no status code (safety
// finishReason), an error that lost blockReason (no candidates), and a success with no marker
// (text only).
func TestGeminiImageResponse_BlockIsContentFilter(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		reasons []string
	}{
		{"safety finishReason", geminiImageRespSafetyFinish, []string{"IMAGE_SAFETY", "Unable to show the generated image because it may violate our policies."}},
		{"blockReason and no candidates", geminiImageRespPromptBlocked, []string{"PROHIBITED_CONTENT"}},
		{"text only", geminiImageRespTextOnly, []string{"STOP"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, bifrostErr := decodeGeminiImageResponse(t, tc.body).ToBifrostImageGenerationResponse()
			require.Nil(t, bifrostErr)
			require.NotNil(t, resp)
			assert.Empty(t, resp.Data)
			require.NotNil(t, resp.ContentFilter)
			assert.Equal(t, 1, resp.ContentFilter.FilteredCount)
			assert.Equal(t, tc.reasons, resp.ContentFilter.Reasons)
			require.NotNil(t, resp.Usage, "the vendor's usage is kept on a block")
			assert.Positive(t, resp.Usage.InputTokens)
			require.NotNil(t, resp.Usage.OutputImages)
			assert.Equal(t, 0, *resp.Usage.OutputImages)

			wire, err := sonic.Marshal(resp)
			require.NoError(t, err)
			assert.Contains(t, string(wire), `"data":[]`)
			assert.Contains(t, string(wire), `"content_filter":{"filtered_count":1,"reasons":["`+tc.reasons[0]+`"`)

			// The native route reports the block as a candidate with no content.
			native, err := ToGeminiImageGenerationResponse(context.Background(), resp)
			require.NoError(t, err)
			require.Len(t, native.Candidates, 1)
			assert.Nil(t, native.Candidates[0].Content)
			assert.Equal(t, FinishReason(tc.reasons[0]), native.Candidates[0].FinishReason)
			if len(tc.reasons) > 1 {
				assert.Equal(t, tc.reasons[1], native.Candidates[0].FinishMessage)
			}
		})
	}
}

// A vendor 200 with no image and nothing that marks a block is 502 provider_empty_result.
func TestGeminiImageResponse_EmptyResult(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		code    string
		message string
	}{
		{"no candidates, no blockReason", geminiImageRespNoCandidates, "", "No candidates found in response"},
		{"candidate with a finishReason that is not a block", geminiImageRespNoImage, "NO_IMAGE", "the provider returned a successful response with no image"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, bifrostErr := decodeGeminiImageResponse(t, tc.body).ToBifrostImageGenerationResponse()
			require.Nil(t, resp)
			require.NotNil(t, bifrostErr)
			require.NotNil(t, bifrostErr.StatusCode)
			assert.Equal(t, 502, *bifrostErr.StatusCode)
			assert.Equal(t, schemas.ErrorTypeProviderEmptyResult, bifrostErr.ExtraFields.ErrorType)
			assert.False(t, bifrostErr.IsBifrostError)
			assert.Equal(t, tc.message, bifrostErr.Error.Message)
			if tc.code == "" {
				assert.Nil(t, bifrostErr.Error.Code)
			} else {
				require.NotNil(t, bifrostErr.Error.Code)
				assert.Equal(t, tc.code, *bifrostErr.Error.Code)
			}
		})
	}
}

// geminiImageUpstream is a stand-in for the Gemini API that counts generateContent calls and
// keeps the last request body.
type geminiImageUpstream struct {
	*httptest.Server
	calls    atomic.Int32
	lastBody atomic.Value // string
}

func newGeminiImageUpstream(t *testing.T, responseBody string) *geminiImageUpstream {
	t.Helper()
	upstream := &geminiImageUpstream{}
	upstream.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		upstream.lastBody.Store(string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseBody))
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func (u *geminiImageUpstream) provider() *GeminiProvider {
	return NewGeminiProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: u.URL + "/v1beta"},
	}, testNoopLogger{})
}

// Through the provider: a deliverable request makes exactly one upstream call, a refused one
// makes none, and an edit reports usage.input_images.
func TestGeminiProviderImage_UpstreamCalls(t *testing.T) {
	key := schemas.Key{Value: *schemas.NewSecretVar("dummy-key")}
	newCtx := func() *schemas.BifrostContext {
		return schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	}

	t.Run("generation: one call, resolution on the wire", func(t *testing.T) {
		upstream := newGeminiImageUpstream(t, geminiImageRespDelivered)
		resp, bifrostErr := upstream.provider().ImageGeneration(newCtx(), key, &schemas.BifrostImageGenerationRequest{
			Provider: schemas.Gemini,
			Model:    "gemini-3.1-flash-image",
			Input:    &schemas.ImageGenerationInput{Prompt: "a red bicycle"},
			Params:   &schemas.ImageGenerationParameters{N: schemas.Ptr(1), Resolution: "2K"},
		})
		require.Nil(t, bifrostErr)
		require.Len(t, resp.Data, 1)
		assert.Equal(t, int32(1), upstream.calls.Load())
		assert.Contains(t, upstream.lastBody.Load().(string), `"imageSize":"2K"`)
		assert.Nil(t, resp.Usage.InputImages)
	})

	t.Run("generation: n 2 and an underivable size make no call", func(t *testing.T) {
		upstream := newGeminiImageUpstream(t, geminiImageRespDelivered)
		for param, params := range map[string]*schemas.ImageGenerationParameters{
			"n":    {N: schemas.Ptr(2)},
			"size": {Size: schemas.Ptr("1000x700")},
		} {
			resp, bifrostErr := upstream.provider().ImageGeneration(newCtx(), key, &schemas.BifrostImageGenerationRequest{
				Provider: schemas.Gemini,
				Model:    "gemini-3.1-flash-image",
				Input:    &schemas.ImageGenerationInput{Prompt: "a red bicycle"},
				Params:   params,
			})
			require.Nil(t, resp)
			requireUnsupportedParameter(t, bifrostErr, param)
		}
		assert.Equal(t, int32(0), upstream.calls.Load())
	})

	t.Run("generation: a block is a 200 with content_filter", func(t *testing.T) {
		upstream := newGeminiImageUpstream(t, geminiImageRespPromptBlocked)
		resp, bifrostErr := upstream.provider().ImageGeneration(newCtx(), key, &schemas.BifrostImageGenerationRequest{
			Provider: schemas.Gemini,
			Model:    "gemini-3.1-flash-image",
			Input:    &schemas.ImageGenerationInput{Prompt: "blocked"},
		})
		require.Nil(t, bifrostErr)
		assert.Empty(t, resp.Data)
		require.NotNil(t, resp.ContentFilter)
		assert.Equal(t, []string{"PROHIBITED_CONTENT"}, resp.ContentFilter.Reasons)
		assert.Equal(t, int32(1), upstream.calls.Load())
	})

	t.Run("generation: an empty 200 is 502 and keeps the vendor body", func(t *testing.T) {
		upstream := newGeminiImageUpstream(t, geminiImageRespNoCandidates)
		ctx := newCtx()
		ctx.SetValue(schemas.BifrostContextKeyCaptureRawResponse, true)
		resp, bifrostErr := upstream.provider().ImageGeneration(ctx, key, &schemas.BifrostImageGenerationRequest{
			Provider: schemas.Gemini,
			Model:    "gemini-3.1-flash-image",
			Input:    &schemas.ImageGenerationInput{Prompt: "nothing"},
		})
		require.Nil(t, resp)
		require.NotNil(t, bifrostErr)
		assert.Equal(t, schemas.ErrorTypeProviderEmptyResult, bifrostErr.ExtraFields.ErrorType)
		assert.NotNil(t, bifrostErr.ExtraFields.RawResponse)
		assert.Equal(t, int32(1), upstream.calls.Load())
	})

	t.Run("edit: one call, URL input sent as fileData, input_images filled", func(t *testing.T) {
		upstream := newGeminiImageUpstream(t, geminiImageRespDelivered)
		resp, bifrostErr := upstream.provider().ImageEdit(newCtx(), key, &schemas.BifrostImageEditRequest{
			Provider: schemas.Gemini,
			Model:    "gemini-3.1-flash-image",
			Input: &schemas.ImageEditInput{
				Prompt: "make it blue",
				Images: []schemas.ImageInput{{URL: "https://example.com/cat.png"}, {Image: imageTestPNG(t)}},
			},
		})
		require.Nil(t, bifrostErr)
		assert.Equal(t, int32(1), upstream.calls.Load())
		assert.Contains(t, upstream.lastBody.Load().(string), `"fileData":{"fileUri":"https://example.com/cat.png","mimeType":"image/png"}`)
		assert.Contains(t, upstream.lastBody.Load().(string), `"inlineData"`)
		require.NotNil(t, resp.Usage)
		require.NotNil(t, resp.Usage.InputImages)
		assert.Equal(t, 2, *resp.Usage.InputImages)
		require.NotNil(t, resp.Usage.OutputImages)
		assert.Equal(t, 1, *resp.Usage.OutputImages)
	})

	t.Run("edit: mask makes no call", func(t *testing.T) {
		upstream := newGeminiImageUpstream(t, geminiImageRespDelivered)
		resp, bifrostErr := upstream.provider().ImageEdit(newCtx(), key, &schemas.BifrostImageEditRequest{
			Provider: schemas.Gemini,
			Model:    "gemini-3.1-flash-image",
			Input:    &schemas.ImageEditInput{Prompt: "make it blue", Images: []schemas.ImageInput{{Image: imageTestPNG(t)}}},
			Params:   &schemas.ImageEditParameters{Mask: imageTestPNG(t)},
		})
		require.Nil(t, resp)
		requireUnsupportedParameter(t, bifrostErr, "mask")
		assert.Equal(t, int32(0), upstream.calls.Load())
	})
}

// Cost on the request path (AGENTS.md "Every change states its cost"): the field check runs once
// per Gemini API image request, before the upstream call.
func BenchmarkCheckGeminiImageGenerationRequest(b *testing.B) {
	request := &schemas.BifrostImageGenerationRequest{
		Provider: schemas.Gemini,
		Model:    "gemini-3.1-flash-image",
		Input:    &schemas.ImageGenerationInput{Prompt: "a red bicycle"},
		Params:   &schemas.ImageGenerationParameters{N: schemas.Ptr(1), Size: schemas.Ptr("1024x1024"), Resolution: "2K"},
	}
	b.ReportAllocs()
	for b.Loop() {
		if bifrostErr := CheckGeminiImageGenerationRequest(request); bifrostErr != nil {
			b.Fatal(bifrostErr.Error.Message)
		}
	}
}

func BenchmarkToGeminiImageGenerationRequest(b *testing.B) {
	request := &schemas.BifrostImageGenerationRequest{
		Provider: schemas.Gemini,
		Model:    "gemini-3.1-flash-image",
		Input:    &schemas.ImageGenerationInput{Prompt: "a red bicycle"},
		Params:   &schemas.ImageGenerationParameters{N: schemas.Ptr(1), Size: schemas.Ptr("1024x1024")},
	}
	b.ReportAllocs()
	for b.Loop() {
		if ToGeminiImageGenerationRequest(request) == nil {
			b.Fatal("nil request")
		}
	}
}

func BenchmarkGeminiImageToBifrostResponse(b *testing.B) {
	var response GenerateContentResponse
	if err := sonic.Unmarshal([]byte(geminiImageRespDelivered), &response); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, bifrostErr := response.ToBifrostImageGenerationResponse(); bifrostErr != nil {
			b.Fatal(bifrostErr.Error.Message)
		}
	}
}
