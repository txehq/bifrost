package gemini_test

import (
	"strings"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/capsohq/bifrost/core/providers/gemini"
	"github.com/capsohq/bifrost/core/providers/openai"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToGeminiVideoGenerationRequest_PutsReferenceImagesAndLastFrameOnInstance(t *testing.T) {
	seconds := "8"
	referenceImage := gemini.VideoReferenceImage{
		Image: gemini.VideoImageData{
			BytesBase64Encoded: schemas.Ptr("cmVmZXJlbmNl"),
			MimeType:           schemas.Ptr("image/png"),
		},
		ReferenceType: "asset",
	}
	lastFrame := &gemini.VideoImageData{
		BytesBase64Encoded: schemas.Ptr("bGFzdA=="),
		MimeType:           schemas.Ptr("image/jpeg"),
	}

	got, err := gemini.ToGeminiVideoGenerationRequest(&schemas.BifrostVideoGenerationRequest{
		Model: "veo-3.1-generate-001",
		Input: &schemas.VideoGenerationInput{
			Prompt: "make the reference subject run through a snowy garden",
		},
		Params: &schemas.VideoGenerationParameters{
			Seconds: &seconds,
			ExtraParams: map[string]any{
				"aspectRatio":     "16:9",
				"referenceImages": []gemini.VideoReferenceImage{referenceImage},
				"lastFrame":       lastFrame,
			},
		},
	})

	require.NoError(t, err)
	require.Len(t, got.Instances, 1)
	assert.Equal(t, []gemini.VideoReferenceImage{referenceImage}, got.Instances[0].ReferenceImages)
	assert.Equal(t, lastFrame, got.Instances[0].LastFrame)
	require.NotNil(t, got.Parameters)
	assert.Equal(t, schemas.Ptr(8), got.Parameters.DurationSeconds)
	assert.Equal(t, schemas.Ptr("16:9"), got.Parameters.AspectRatio)
	assert.Empty(t, got.Parameters.ReferenceImages)
	assert.Nil(t, got.Parameters.LastFrame)
}

func TestToGeminiVideoGenerationRequest_DecodesGenericReferenceImagesAndLastFrame(t *testing.T) {
	got, err := gemini.ToGeminiVideoGenerationRequest(&schemas.BifrostVideoGenerationRequest{
		Model: "veo-3.1-generate-001",
		Input: &schemas.VideoGenerationInput{
			Prompt: "use the provided reference image",
		},
		Params: &schemas.VideoGenerationParameters{
			ExtraParams: map[string]any{
				"referenceImages": []map[string]any{
					{
						"image": map[string]any{
							"bytesBase64Encoded": "cmVmZXJlbmNl",
							"mimeType":           "image/png",
						},
						"referenceType": "style",
					},
				},
				"lastFrame": map[string]any{
					"bytesBase64Encoded": "bGFzdA==",
					"mimeType":           "image/jpeg",
				},
			},
		},
	})

	require.NoError(t, err)
	require.Len(t, got.Instances, 1)
	require.Len(t, got.Instances[0].ReferenceImages, 1)
	assert.Equal(t, "style", got.Instances[0].ReferenceImages[0].ReferenceType)
	assert.Equal(t, schemas.Ptr("cmVmZXJlbmNl"), got.Instances[0].ReferenceImages[0].Image.BytesBase64Encoded)
	assert.Equal(t, schemas.Ptr("image/png"), got.Instances[0].ReferenceImages[0].Image.MimeType)
	require.NotNil(t, got.Instances[0].LastFrame)
	assert.Equal(t, schemas.Ptr("bGFzdA=="), got.Instances[0].LastFrame.BytesBase64Encoded)
	assert.Equal(t, schemas.Ptr("image/jpeg"), got.Instances[0].LastFrame.MimeType)
	require.NotNil(t, got.Parameters)
	assert.Empty(t, got.Parameters.ReferenceImages)
	assert.Nil(t, got.Parameters.LastFrame)
}

func TestToGeminiVideoGenerationRequest_IgnoresInvalidGenericReferenceImages(t *testing.T) {
	got, err := gemini.ToGeminiVideoGenerationRequest(&schemas.BifrostVideoGenerationRequest{
		Model: "veo-3.1-generate-001",
		Input: &schemas.VideoGenerationInput{
			Prompt: "use the provided reference image",
		},
		Params: &schemas.VideoGenerationParameters{
			ExtraParams: map[string]any{
				"referenceImages": []map[string]any{
					{"unexpected": "value"},
				},
			},
		},
	})

	require.NoError(t, err)
	require.Len(t, got.Instances, 1)
	assert.Nil(t, got.Instances[0].ReferenceImages)
	require.NotNil(t, got.Parameters)
	assert.Empty(t, got.Parameters.ReferenceImages)
}

func TestGeminiVideoGenerationRequest_RoundTripsInstanceReferenceFields(t *testing.T) {
	durationSeconds := 8
	in := &gemini.GeminiVideoGenerationRequest{
		Model: "gemini/veo-3.1-generate-001",
		Instances: []gemini.GeminiVideoGenerationInstance{
			{
				Prompt: "use the provided reference image",
				ReferenceImages: []gemini.VideoReferenceImage{
					{
						Image: gemini.VideoImageData{
							BytesBase64Encoded: schemas.Ptr("cmVmZXJlbmNl"),
							MimeType:           schemas.Ptr("image/png"),
						},
						ReferenceType: "asset",
					},
				},
				LastFrame: &gemini.VideoImageData{
					BytesBase64Encoded: schemas.Ptr("bGFzdA=="),
					MimeType:           schemas.Ptr("image/jpeg"),
				},
			},
		},
		Parameters: &gemini.VideoGenerationParameters{
			DurationSeconds: &durationSeconds,
		},
	}

	bifrostReq, err := in.ToBifrostVideoGenerationRequest(nil)
	require.NoError(t, err)

	got, err := gemini.ToGeminiVideoGenerationRequest(bifrostReq)
	require.NoError(t, err)

	require.Len(t, got.Instances, 1)
	assert.Equal(t, in.Instances[0].ReferenceImages, got.Instances[0].ReferenceImages)
	assert.Equal(t, in.Instances[0].LastFrame, got.Instances[0].LastFrame)
	require.NotNil(t, got.Parameters)
	assert.Equal(t, &durationSeconds, got.Parameters.DurationSeconds)
	assert.Empty(t, got.Parameters.ReferenceImages)
	assert.Nil(t, got.Parameters.LastFrame)
}

// TXE-7059: the genai route carries resolution as the unified field as well as the extra
// param the Gemini converter reads, so core's price-field rule sees it as sent and does
// not refuse the route's own requests. aspectRatio keeps Gemini's own name.
func TestGenAIVideoIngressSetsTheUnifiedResolution(t *testing.T) {
	in := &gemini.GeminiVideoGenerationRequest{
		Model:     "gemini/veo-3.1-generate-preview",
		Instances: []gemini.GeminiVideoGenerationInstance{{Prompt: "a paper boat"}},
		Parameters: &gemini.VideoGenerationParameters{
			Resolution:  schemas.Ptr("1080p"),
			AspectRatio: schemas.Ptr("16:9"),
		},
	}
	bifrostReq, err := in.ToBifrostVideoGenerationRequest(nil)
	require.NoError(t, err)
	require.NotNil(t, bifrostReq.Params)
	assert.Equal(t, schemas.TolerantString("1080p"), bifrostReq.Params.Resolution)
	assert.Equal(t, "1080p", bifrostReq.Params.ExtraParams["resolution"])
	assert.Nil(t, schemas.CheckExtraParamPriceFields(&schemas.BifrostRequest{
		RequestType: schemas.VideoGenerationRequest, VideoGenerationRequest: bifrostReq,
	}), "the genai route's own request must not be refused")

	got, err := gemini.ToGeminiVideoGenerationRequest(bifrostReq)
	require.NoError(t, err)
	require.NotNil(t, got.Parameters)
	assert.Equal(t, schemas.Ptr("1080p"), got.Parameters.Resolution)
	assert.Equal(t, schemas.Ptr("16:9"), got.Parameters.AspectRatio)
}

// TXE-7065: the unified aspect_ratio is delivered as Veo's aspectRatio, with or without
// extra params, and the genai route carries Veo's aspectRatio as the unified field too.
func TestGeminiVideoDeliversTheUnifiedAspectRatio(t *testing.T) {
	for _, extra := range []map[string]interface{}{nil, {"personGeneration": "allow_adult"}, {"aspectRatio": "9:16"}} {
		got, err := gemini.ToGeminiVideoGenerationRequest(&schemas.BifrostVideoGenerationRequest{
			Provider: schemas.Gemini, Model: "veo-3.1-generate-preview",
			Input:  &schemas.VideoGenerationInput{Prompt: "a paper boat"},
			Params: &schemas.VideoGenerationParameters{Size: "1280x720", AspectRatio: "9:16", ExtraParams: extra},
		})
		require.NoError(t, err)
		require.NotNil(t, got.Parameters)
		assert.Equal(t, schemas.Ptr("9:16"), got.Parameters.AspectRatio, "extra params %v", extra)
	}

	in := &gemini.GeminiVideoGenerationRequest{
		Model:      "gemini/veo-3.1-generate-preview",
		Instances:  []gemini.GeminiVideoGenerationInstance{{Prompt: "a paper boat"}},
		Parameters: &gemini.VideoGenerationParameters{AspectRatio: schemas.Ptr("9:16")},
	}
	bifrostReq, err := in.ToBifrostVideoGenerationRequest(nil)
	require.NoError(t, err)
	assert.Equal(t, schemas.TolerantString("9:16"), bifrostReq.Params.AspectRatio)
	assert.Nil(t, schemas.CheckExtraParamPriceFieldsFor(schemas.Gemini, &schemas.BifrostRequest{
		RequestType: schemas.VideoGenerationRequest, VideoGenerationRequest: bifrostReq,
	}), "the genai route's own request must not be refused")
}

// veoBody is the JSON a Veo create request puts on the wire with extra-param passthrough on,
// built by the encoding step CheckContextAndGetRequestBody runs.
func veoBody(t *testing.T, vertex bool, req *schemas.BifrostVideoGenerationRequest) []byte {
	t.Helper()
	convert := gemini.ToGeminiVideoGenerationRequest
	if vertex {
		convert = gemini.ToVertexVideoGenerationRequest
	}
	converted, err := convert(req)
	require.NoError(t, err)
	body, err := providerUtils.MarshalConvertedRequestBody(converted, true)
	require.NoError(t, err)
	return body
}

// requireUnsupportedVideoParam asserts the converter refused the request with 400
// caller_unsupported_parameter naming param, the error CheckContextAndGetRequestBody returns.
func requireUnsupportedVideoParam(t *testing.T, err error, param string) {
	t.Helper()
	require.Error(t, err)
	bifrostErr, ok := providerUtils.AsBifrostBadRequestError(err)
	require.True(t, ok, "converter error is not a caller refusal: %v", err)
	require.NotNil(t, bifrostErr.StatusCode)
	assert.Equal(t, 400, *bifrostErr.StatusCode)
	assert.Equal(t, schemas.ErrorTypeCallerUnsupportedParameter, bifrostErr.ExtraFields.ErrorType)
	require.NotNil(t, bifrostErr.Error)
	assert.Equal(t, param, bifrostErr.Error.Param)
}

const veoTestDataURL = "data:image/jpeg;base64,Zmlyc3Q="

// TXE-6537 (contract C4, C7): every unified video field and every Vertex parameter with no
// unified name reaches the vendor under the vendor's name, inside instances[0] or parameters,
// and nothing the converter sent is repeated at the top level of the body.
func TestToVertexVideoGenerationRequest_SendsEveryFieldUnderTheVendorName(t *testing.T) {
	// The HTTP handlers keep the four unified fields in the extra params under the same name.
	extra := map[string]any{
		"aspect_ratio":     "9:16",
		"resolution":       "1080p",
		"last_frame":       "gs://frames/last.png",
		"reference_images": []any{veoTestDataURL},
		"fps":              float64(24),
		"pubsubTopic":      "projects/p/topics/veo",
		"task":             "interpolation",
		"sampleCount":      float64(2),
		"watermark":        true,
	}
	body := veoBody(t, true, &schemas.BifrostVideoGenerationRequest{
		Model: "veo-3.1-generate-001",
		Input: &schemas.VideoGenerationInput{Prompt: "a fox", InputReference: schemas.Ptr("gs://frames/first.jpg")},
		Params: &schemas.VideoGenerationParameters{
			Seconds:         schemas.Ptr("8"),
			Audio:           schemas.Ptr(false),
			AspectRatio:     "9:16",
			Resolution:      "1080p",
			LastFrame:       "gs://frames/last.png",
			ReferenceImages: schemas.VideoReferenceImages{veoTestDataURL},
			TaskControlParameters: schemas.TaskControlParameters{
				OutputStorageURI: schemas.Ptr("gs://tokun-out/task_123/"),
			},
			ExtraParams: extra,
		},
	})

	want := map[string]any{
		"instances.0.prompt":                                     "a fox",
		"instances.0.image.gcsUri":                               "gs://frames/first.jpg",
		"instances.0.image.mimeType":                             "image/jpeg",
		"instances.0.lastFrame.gcsUri":                           "gs://frames/last.png",
		"instances.0.lastFrame.mimeType":                         "image/png",
		"instances.0.referenceImages.0.image.bytesBase64Encoded": "Zmlyc3Q=",
		"instances.0.referenceImages.0.image.mimeType":           "image/jpeg",
		"instances.0.referenceImages.0.referenceType":            "asset",
		"parameters.storageUri":                                  "gs://tokun-out/task_123/",
		"parameters.aspectRatio":                                 "9:16",
		"parameters.resolution":                                  "1080p",
		"parameters.durationSeconds":                             float64(8),
		"parameters.generateAudio":                               false,
		"parameters.fps":                                         float64(24),
		"parameters.pubsubTopic":                                 "projects/p/topics/veo",
		"parameters.task":                                        "interpolation",
		"parameters.sampleCount":                                 float64(2),
		// An extra param the converter does not know is merged at the top level, unchanged.
		"watermark": true,
	}
	for path, value := range want {
		got := providerUtils.GetJSONField(body, path)
		require.True(t, got.Exists(), "%s is missing from %s", path, body)
		assert.Equal(t, value, got.Value(), path)
	}
	assert.False(t, providerUtils.GetJSONField(body, "parameters.storageURI").Exists(), "the storageURI spelling is still sent: %s", body)

	var top map[string]any
	require.NoError(t, sonic.Unmarshal(body, &top))
	keys := make([]string, 0, len(top))
	for key := range top {
		keys = append(keys, key)
	}
	assert.ElementsMatch(t, []string{"instances", "parameters", "watermark"}, keys, "a converted field is repeated at the top level: %s", body)
	assert.Equal(t, "9:16", extra["aspect_ratio"], "the caller's extra params were changed")
}

// TXE-6537 (contract C7): output_storage_uri and the vendor's own storageUri, in either
// spelling, all become parameters.storageUri; two different locations are refused.
func TestToVertexVideoGenerationRequest_StorageURI(t *testing.T) {
	request := func(unified *string, extra map[string]any) *schemas.BifrostVideoGenerationRequest {
		return &schemas.BifrostVideoGenerationRequest{
			Model: "veo-3.1-lite-generate-001",
			Input: &schemas.VideoGenerationInput{Prompt: "a fox"},
			Params: &schemas.VideoGenerationParameters{
				TaskControlParameters: schemas.TaskControlParameters{OutputStorageURI: unified},
				ExtraParams:           extra,
			},
		}
	}
	for name, req := range map[string]*schemas.BifrostVideoGenerationRequest{
		"output_storage_uri": request(schemas.Ptr("gs://b/t/"), nil),
		"storageUri extra":   request(nil, map[string]any{"storageUri": "gs://b/t/"}),
		"storageURI extra":   request(nil, map[string]any{"storageURI": "gs://b/t/"}),
		"both, the same":     request(schemas.Ptr("gs://b/t/"), map[string]any{"storageUri": "gs://b/t/"}),
	} {
		t.Run(name, func(t *testing.T) {
			body := veoBody(t, true, req)
			assert.Equal(t, "gs://b/t/", providerUtils.GetJSONField(body, "parameters.storageUri").String(), "%s", body)
			assert.False(t, providerUtils.GetJSONField(body, "storageUri").Exists(), "%s", body)
			assert.False(t, providerUtils.GetJSONField(body, "storageURI").Exists(), "%s", body)
		})
	}
	_, err := gemini.ToVertexVideoGenerationRequest(request(schemas.Ptr("gs://b/t/"), map[string]any{"storageUri": "gs://other/"}))
	requireUnsupportedVideoParam(t, err, "output_storage_uri")
}

// TXE-6537 (contract C4): size names an aspect ratio and a resolution tier. A size Veo does not
// render is refused; before, it was sent as 16:9.
func TestToGeminiVideoGenerationRequest_Size(t *testing.T) {
	request := func(size string, params schemas.VideoGenerationParameters) *schemas.BifrostVideoGenerationRequest {
		params.Size = size
		return &schemas.BifrostVideoGenerationRequest{
			Model:  "veo-3.1-generate-001",
			Input:  &schemas.VideoGenerationInput{Prompt: "a fox"},
			Params: &params,
		}
	}
	sent := []struct {
		size, aspectRatio, resolution string
	}{
		{"1280x720", "16:9", ""},
		{"720x1280", "9:16", ""},
		{"1920x1080", "16:9", "1080p"},
		{"1080x1920", "9:16", "1080p"},
		{"3840x2160", "16:9", "4k"},
	}
	for _, tc := range sent {
		for _, vertex := range []bool{false, true} {
			body := veoBody(t, vertex, request(tc.size, schemas.VideoGenerationParameters{}))
			assert.Equal(t, tc.aspectRatio, providerUtils.GetJSONField(body, "parameters.aspectRatio").String(), "%s: %s", tc.size, body)
			assert.Equal(t, tc.resolution, providerUtils.GetJSONField(body, "parameters.resolution").String(), "%s: %s", tc.size, body)
		}
	}
	// A size that agrees with the explicit fields is sent; the explicit resolution is kept.
	body := veoBody(t, true, request("1280x720", schemas.VideoGenerationParameters{AspectRatio: "16:9", Resolution: "720p"}))
	assert.Equal(t, "720p", providerUtils.GetJSONField(body, "parameters.resolution").String(), "%s", body)

	refused := map[string]schemas.VideoGenerationParameters{
		"1024x1024": {},
		"1792x1024": {}, // an OpenAI size: 7:4, not 16:9
		"720p":      {},
		"1920x1080": {AspectRatio: "9:16"},
		"1280x720":  {Resolution: "1080p"},
	}
	for size, params := range refused {
		for _, convert := range []func(*schemas.BifrostVideoGenerationRequest) (*gemini.GeminiVideoGenerationRequest, error){
			gemini.ToGeminiVideoGenerationRequest, gemini.ToVertexVideoGenerationRequest,
		} {
			_, err := convert(request(size, params))
			requireUnsupportedVideoParam(t, err, "size")
		}
	}
}

// TXE-6537 (contract C4): a first frame, last frame or reference image the vendor cannot take
// is refused naming the field. Before, an http URL was sent as a bare mimeType with no image.
func TestToGeminiVideoGenerationRequest_FramesAreSentOrRefused(t *testing.T) {
	base := func() *schemas.BifrostVideoGenerationRequest {
		return &schemas.BifrostVideoGenerationRequest{
			Model:  "veo-3.1-generate-001",
			Input:  &schemas.VideoGenerationInput{Prompt: "a fox"},
			Params: &schemas.VideoGenerationParameters{},
		}
	}
	fields := map[string]func(req *schemas.BifrostVideoGenerationRequest, value string){
		"input_reference": func(req *schemas.BifrostVideoGenerationRequest, v string) { req.Input.InputReference = &v },
		"last_frame": func(req *schemas.BifrostVideoGenerationRequest, v string) {
			req.Params.LastFrame = schemas.TolerantString(v)
		},
		"reference_images": func(req *schemas.BifrostVideoGenerationRequest, v string) {
			req.Params.ReferenceImages = schemas.VideoReferenceImages{v}
		},
	}
	paths := map[string]string{
		"input_reference":  "instances.0.image",
		"last_frame":       "instances.0.lastFrame",
		"reference_images": "instances.0.referenceImages.0.image",
	}
	for field, set := range fields {
		for _, vertex := range []bool{false, true} {
			convert := gemini.ToGeminiVideoGenerationRequest
			if vertex {
				convert = gemini.ToVertexVideoGenerationRequest
			}
			// Bytes are sent on both.
			req := base()
			set(req, veoTestDataURL)
			body := veoBody(t, vertex, req)
			assert.Equal(t, "Zmlyc3Q=", providerUtils.GetJSONField(body, paths[field]+".bytesBase64Encoded").String(), "%s: %s", field, body)
			assert.Equal(t, "image/jpeg", providerUtils.GetJSONField(body, paths[field]+".mimeType").String(), "%s: %s", field, body)

			// Neither takes an http(s) URL.
			req = base()
			set(req, "https://example.com/frame.png")
			_, err := convert(req)
			requireUnsupportedVideoParam(t, err, field)

			// Only Vertex takes a Cloud Storage URI, and it is sent exactly as given.
			req = base()
			set(req, "gs://bucket/a b/frame.webp")
			if !vertex {
				_, err := convert(req)
				requireUnsupportedVideoParam(t, err, field)
				continue
			}
			body = veoBody(t, true, req)
			assert.Equal(t, "gs://bucket/a b/frame.webp", providerUtils.GetJSONField(body, paths[field]+".gcsUri").String(), "%s: %s", field, body)
			assert.Equal(t, "image/webp", providerUtils.GetJSONField(body, paths[field]+".mimeType").String(), "%s: %s", field, body)
			assert.False(t, providerUtils.GetJSONField(body, paths[field]+".bytesBase64Encoded").Exists(), "%s: %s", field, body)
		}
	}

	// A Cloud Storage object whose type the name does not tell is sent without a guessed mimeType.
	req := base()
	req.Input.InputReference = schemas.Ptr("gs://bucket/frame")
	body := veoBody(t, true, req)
	assert.Equal(t, "gs://bucket/frame", providerUtils.GetJSONField(body, "instances.0.image.gcsUri").String())
	assert.False(t, providerUtils.GetJSONField(body, "instances.0.image.mimeType").Exists(), "%s", body)

	// A unified field that arrived in a shape its type cannot hold is refused, not dropped; the
	// same frame given under both names is refused too.
	for field, extra := range map[string]map[string]any{
		"last_frame":       {"last_frame": float64(3)},
		"reference_images": {"reference_images": "https://example.com/a.png"},
		"aspect_ratio":     {"aspect_ratio": float64(16)},
		"resolution":       {"resolution": float64(1080)},
	} {
		req := base()
		req.Params.ExtraParams = extra
		_, err := gemini.ToVertexVideoGenerationRequest(req)
		requireUnsupportedVideoParam(t, err, field)
	}
	req = base()
	req.Params.LastFrame = veoTestDataURL
	req.Params.ExtraParams = map[string]any{"lastFrame": map[string]any{"gcsUri": "gs://b/o.png"}}
	_, err := gemini.ToVertexVideoGenerationRequest(req)
	requireUnsupportedVideoParam(t, err, "last_frame")
	req = base()
	req.Params.AspectRatio = "16:9"
	req.Params.ExtraParams = map[string]any{"aspectRatio": "9:16"}
	_, err = gemini.ToVertexVideoGenerationRequest(req)
	requireUnsupportedVideoParam(t, err, "aspect_ratio")

	// seconds that is not a whole number is refused, not a conversion failure.
	req = base()
	req.Params.Seconds = schemas.Ptr("7.5")
	_, err = gemini.ToVertexVideoGenerationRequest(req)
	requireUnsupportedVideoParam(t, err, "seconds")
}

// TXE-6537: the Gemini-shaped route carries a Cloud Storage first frame, storageUri and the
// Vertex parameters through to the Vertex body.
func TestGeminiVideoGenerationRequest_RoundTripsVertexFields(t *testing.T) {
	in := &gemini.GeminiVideoGenerationRequest{
		Model: "vertex/veo-3.1-generate-001",
		Instances: []gemini.GeminiVideoGenerationInstance{{
			Prompt: "a fox",
			Image:  &gemini.VideoImageData{GCSURI: schemas.Ptr("gs://frames/first.png"), MimeType: schemas.Ptr("image/png")},
		}},
		Parameters: &gemini.VideoGenerationParameters{
			StorageURI:  schemas.Ptr("gs://out/t/"),
			AspectRatio: schemas.Ptr("9:16"),
			FPS:         float64(24),
			PubsubTopic: "projects/p/topics/veo",
			Task:        "interpolation",
		},
	}
	bifrostReq, err := in.ToBifrostVideoGenerationRequest(nil)
	require.NoError(t, err)
	body := veoBody(t, true, bifrostReq)
	for path, value := range map[string]any{
		"instances.0.image.gcsUri": "gs://frames/first.png",
		"parameters.storageUri":    "gs://out/t/",
		"parameters.aspectRatio":   "9:16",
		"parameters.fps":           float64(24),
		"parameters.pubsubTopic":   "projects/p/topics/veo",
		"parameters.task":          "interpolation",
	} {
		assert.Equal(t, value, providerUtils.GetJSONField(body, path).Value(), "%s: %s", path, body)
	}
}

// veoOperation decodes a fetchPredictOperation response body.
func veoOperation(t *testing.T, body string) *gemini.GenerateVideosOperation {
	t.Helper()
	var operation gemini.GenerateVideosOperation
	require.NoError(t, sonic.Unmarshal([]byte(body), &operation))
	return &operation
}

const veoOperationName = "projects/p/locations/us-central1/publishers/google/models/veo-3.1-generate-001/operations/a1b07c8e"

// TXE-6537 (contract C6, C7): the Vertex operation shape (GoogleCloudAiplatformV1GenerateVideoResponse
// in the pinned vertex-ai/testdata/pinned/v1.json: videos[] of bytesBase64Encoded / gcsUri /
// mimeType, raiMediaFilteredCount, raiMediaFilteredReasons). Delivered videos are in videos[],
// filtered ones are counted in content_filter, and a task with every video filtered failed.
// Before, the count was not read: a fully filtered task reported completed with no video.
func TestToBifrostVideoGenerationResponse_VertexFilteredVideos(t *testing.T) {
	reasons := []string{"Veo could not generate videos because the input may violate the usage guidelines. Support codes: 15236754"}

	t.Run("two delivered, one filtered", func(t *testing.T) {
		resp, bifrostErr := gemini.ToBifrostVideoGenerationResponse(veoOperation(t, `{
			"name": "`+veoOperationName+`", "done": true,
			"response": {
				"@type": "type.googleapis.com/cloud.ai.large_models.vision.GenerateVideoResponse",
				"raiMediaFilteredCount": 1,
				"raiMediaFilteredReasons": ["`+reasons[0]+`"],
				"videos": [
					{"gcsUri": "gs://tokun-out/task_123/sample_0.mp4", "mimeType": "video/mp4"},
					{"bytesBase64Encoded": "dmlkZW8=", "mimeType": "video/mp4"}
				]
			}
		}`), "")
		require.Nil(t, bifrostErr)
		assert.Equal(t, schemas.VideoStatusCompleted, resp.Status)
		assert.Equal(t, "done=true", resp.ProviderStatus)
		assert.Nil(t, resp.Error)
		require.NotNil(t, resp.ContentFilter)
		assert.Equal(t, 1, resp.ContentFilter.FilteredCount)
		assert.Equal(t, reasons, resp.ContentFilter.Reasons)
		require.Len(t, resp.Videos, 2)
		// A Cloud Storage artifact needs the vendor's credentials: a content entry with no url.
		assert.Equal(t, schemas.VideoOutput{ID: "gs://tokun-out/task_123/sample_0.mp4", Type: schemas.VideoOutputTypeContent, ContentType: "video/mp4"}, resp.Videos[0])
		assert.Equal(t, schemas.VideoOutputTypeBase64, resp.Videos[1].Type)
		assert.Equal(t, schemas.Ptr("dmlkZW8="), resp.Videos[1].Base64Data)
		// The vendor reports no duration, so the task object carries none (contract C5).
		assert.Nil(t, resp.Seconds)
		assert.Nil(t, resp.Usage)

		wire, err := sonic.Marshal(resp)
		require.NoError(t, err)
		assert.False(t, providerUtils.GetJSONField(wire, "videos.0.url").Exists(), "%s", wire)
		assert.Equal(t, "content", providerUtils.GetJSONField(wire, "videos.0.type").String(), "%s", wire)
		assert.False(t, strings.Contains(string(wire), `"seconds"`), "%s", wire)
	})

	t.Run("every video filtered", func(t *testing.T) {
		resp, bifrostErr := gemini.ToBifrostVideoGenerationResponse(veoOperation(t, `{
			"name": "`+veoOperationName+`", "done": true,
			"response": {"raiMediaFilteredCount": 2, "raiMediaFilteredReasons": ["`+reasons[0]+`"]}
		}`), "")
		require.Nil(t, bifrostErr)
		assert.Equal(t, schemas.VideoStatusFailed, resp.Status)
		assert.Equal(t, "done=true", resp.ProviderStatus)
		assert.Empty(t, resp.Videos)
		require.NotNil(t, resp.ContentFilter)
		assert.Equal(t, 2, resp.ContentFilter.FilteredCount)
		assert.Equal(t, reasons, resp.ContentFilter.Reasons)
		require.NotNil(t, resp.Error)
		assert.Equal(t, "content_filtered", resp.Error.Code)
		assert.Equal(t, reasons[0], resp.Error.Message)
	})
}

// TXE-6537 (contract C7): every task object carries provider_status, the operation's done flag
// and its error code.
func TestToBifrostVideoGenerationResponse_ProviderStatus(t *testing.T) {
	cases := []struct {
		name, body, providerStatus string
		status                     schemas.VideoStatus
		errorCode                  string
	}{
		{name: "created", body: `{"name": "` + veoOperationName + `"}`, providerStatus: "done=false", status: schemas.VideoStatusInProgress},
		{name: "running", body: `{"name": "` + veoOperationName + `", "done": false, "metadata": {"progress": 40}}`, providerStatus: "done=false", status: schemas.VideoStatusInProgress},
		{name: "done", body: `{"name": "` + veoOperationName + `", "done": true, "response": {"videos": [{"bytesBase64Encoded": "dmlkZW8=", "mimeType": "video/mp4"}]}}`, providerStatus: "done=true", status: schemas.VideoStatusCompleted},
		{name: "failed", body: `{"name": "` + veoOperationName + `", "done": true, "error": {"code": 3, "message": "Unsupported aspect ratio"}}`, providerStatus: "done=true,error.code=3", status: schemas.VideoStatusFailed, errorCode: "3"},
		{name: "gemini shape", body: `{"name": "models/veo-3.1-generate-preview/operations/op1", "done": true, "response": {"generateVideoResponse": {"generatedSamples": [{"video": {"uri": "https://generativelanguage.googleapis.com/v1beta/files/f:download?alt=media"}}]}}}`, providerStatus: "done=true", status: schemas.VideoStatusCompleted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, bifrostErr := gemini.ToBifrostVideoGenerationResponse(veoOperation(t, tc.body), "")
			require.Nil(t, bifrostErr)
			assert.Equal(t, tc.providerStatus, resp.ProviderStatus)
			assert.Equal(t, tc.status, resp.Status)
			if tc.errorCode != "" {
				require.NotNil(t, resp.Error)
				assert.Equal(t, tc.errorCode, resp.Error.Code)
			}
			assert.Nil(t, resp.Seconds)
			assert.Nil(t, resp.Usage)
		})
	}
}

// TXE-6537 (contract C6): the Gemini API shape follows the same rule. Before, one filtered
// video failed the task and withheld the videos that were delivered.
func TestToBifrostVideoGenerationResponse_GeminiPartlyFiltered(t *testing.T) {
	resp, bifrostErr := gemini.ToBifrostVideoGenerationResponse(veoOperation(t, `{
		"name": "models/veo-3.1-generate-preview/operations/op1", "done": true,
		"response": {"generateVideoResponse": {
			"generatedSamples": [{"video": {"uri": "https://generativelanguage.googleapis.com/v1beta/files/f:download?alt=media"}}],
			"raiMediaFilteredCount": 1, "raiMediaFilteredReasons": ["blocked"]
		}}
	}`), "")
	require.Nil(t, bifrostErr)
	assert.Equal(t, schemas.VideoStatusCompleted, resp.Status)
	require.Len(t, resp.Videos, 1)
	assert.Equal(t, schemas.VideoOutputTypeURL, resp.Videos[0].Type)
	require.NotNil(t, resp.ContentFilter)
	assert.Equal(t, 1, resp.ContentFilter.FilteredCount)
	assert.Equal(t, []string{"blocked"}, resp.ContentFilter.Reasons)

	// The Gemini-shaped route gives the same task back: the videos and the count together, and
	// a Cloud Storage artifact by its gs:// URI.
	resp.Videos = append(resp.Videos, schemas.VideoOutput{ID: "gs://out/t/sample_1.mp4", Type: schemas.VideoOutputTypeContent, ContentType: "video/mp4"})
	resp.Model = "veo-3.1-generate-preview"
	operation := gemini.ToGeminiVideoGenerationResponse(resp)
	require.NotNil(t, operation.Response)
	require.NotNil(t, operation.Response.GenerateVideoResponse)
	require.Len(t, operation.Response.GenerateVideoResponse.GeneratedSamples, 2)
	assert.Equal(t, "gs://out/t/sample_1.mp4", operation.Response.GenerateVideoResponse.GeneratedSamples[1].Video.URI)
	assert.Equal(t, int32(1), operation.Response.GenerateVideoResponse.RAIMediaFilteredCount)
	assert.Equal(t, []string{"blocked"}, operation.Response.GenerateVideoResponse.RAIMediaFilteredReasons)
}

// An /openai/v1/videos request's unified resolution reaches Veo (fork, TXE-7126). The Gemini
// converter reads resolution only from ExtraParams, so it depends on the compatibility copy the
// OpenAI route request keeps there; before that copy, the route dropped it.
func TestOpenAIRouteResolutionReachesVeo(t *testing.T) {
	req := openai.OpenAIVideoGenerationRequest{Model: "gemini/veo-3.1-generate-preview", Prompt: "a paper boat"}
	req.Resolution = "1080p"
	geminiReq, err := gemini.ToGeminiVideoGenerationRequest(req.ToBifrostVideoGenerationRequest(nil))
	require.NoError(t, err)
	require.NotNil(t, geminiReq.Parameters)
	require.NotNil(t, geminiReq.Parameters.Resolution)
	assert.Equal(t, "1080p", *geminiReq.Parameters.Resolution)
}

// BenchmarkVeoVideoGenerationRequest is the conversion every Veo create runs on the request path:
// a request with no extra params, the same with the four extra params a caller typically adds,
// and a Vertex request using every unified field.
func BenchmarkVeoVideoGenerationRequest(b *testing.B) {
	base := func() *schemas.BifrostVideoGenerationRequest {
		return &schemas.BifrostVideoGenerationRequest{
			Model:  "veo-3.1-generate-001",
			Input:  &schemas.VideoGenerationInput{Prompt: "a fox running through snow", InputReference: schemas.Ptr(veoTestDataURL)},
			Params: &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("8"), Size: "1280x720", Audio: schemas.Ptr(true)},
		}
	}
	withExtras := base()
	withExtras.Params.ExtraParams = map[string]any{"sampleCount": float64(2), "personGeneration": "allow_adult", "enhancePrompt": true, "compressionQuality": "optimized"}
	unified := base()
	unified.Input.InputReference = schemas.Ptr("gs://frames/first.png")
	unified.Params.Size = ""
	unified.Params.AspectRatio = "9:16"
	unified.Params.Resolution = "1080p"
	unified.Params.LastFrame = "gs://frames/last.png"
	unified.Params.ReferenceImages = schemas.VideoReferenceImages{"gs://frames/ref.png"}
	unified.Params.OutputStorageURI = schemas.Ptr("gs://out/task_1/")
	unified.Params.ExtraParams = map[string]any{"aspect_ratio": "9:16", "resolution": "1080p", "last_frame": "gs://frames/last.png", "reference_images": []any{"gs://frames/ref.png"}, "fps": float64(24)}

	for _, tc := range []struct {
		name    string
		req     *schemas.BifrostVideoGenerationRequest
		convert func(*schemas.BifrostVideoGenerationRequest) (*gemini.GeminiVideoGenerationRequest, error)
	}{
		{"no_extra_params", base(), gemini.ToGeminiVideoGenerationRequest},
		{"four_extra_params", withExtras, gemini.ToGeminiVideoGenerationRequest},
		{"vertex_unified_fields", unified, gemini.ToVertexVideoGenerationRequest},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := tc.convert(tc.req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkVeoOperationResponse is the conversion every Veo query runs: a finished Vertex
// operation with one inline video, and one with a bucket video and a filtered one.
func BenchmarkVeoOperationResponse(b *testing.B) {
	inline := "dmlkZW8="
	gcs := "gs://out/task_1/sample_0.mp4"
	mime := "video/mp4"
	for _, tc := range []struct {
		name string
		resp *gemini.GenerateVideosOperationResponse
	}{
		{"one_inline_video", &gemini.GenerateVideosOperationResponse{Videos: []gemini.VideoGenerationResponse{{BytesBase64Encoded: &inline, MIMEType: &mime}}}},
		{"bucket_video_one_filtered", &gemini.GenerateVideosOperationResponse{
			Videos:                  []gemini.VideoGenerationResponse{{GCSURI: &gcs, MIMEType: &mime}},
			RAIMediaFilteredCount:   1,
			RAIMediaFilteredReasons: []string{"blocked"},
		}},
	} {
		operation := &gemini.GenerateVideosOperation{Name: veoOperationName, Done: true, Response: tc.resp}
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if resp, bifrostErr := gemini.ToBifrostVideoGenerationResponse(operation, ""); bifrostErr != nil || len(resp.Videos) != 1 {
					b.Fatal("conversion failed")
				}
			}
		})
	}
}
