package vertex

import (
	"testing"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

// Vertex resolves gs:// through fileData.fileUri, the only way to embed media too large to
// inline. The embedding path used to run image URLs through the http/https-only allowlist,
// so a GCS URI failed inside the converter - and as a conversion fault, at HTTP 500.
func TestToVertexGeminiEmbeddingRequestAllowsGCSImages(t *testing.T) {
	t.Run("gs image url survives as fileData", func(t *testing.T) {
		req, err := ToVertexGeminiEmbeddingRequest(&schemas.BifrostEmbeddingRequest{
			Model: "gemini-embedding-2-preview",
			Input: []schemas.EmbeddingInputItem{{Content: schemas.EmbeddingContent{
				{Type: schemas.EmbeddingContentPartTypeImage, Image: &schemas.EmbeddingMediaPart{URL: schemas.Ptr("gs://my-bucket/xxx.png")}},
			}}},
		})
		require.NoError(t, err)
		require.Len(t, req.Content.Parts, 1)
		require.NotNil(t, req.Content.Parts[0].FileData)
		require.Equal(t, "gs://my-bucket/xxx.png", req.Content.Parts[0].FileData.FileURI)
	})

	t.Run("a scheme no one allows is a caller error, not a conversion fault", func(t *testing.T) {
		_, err := ToVertexGeminiEmbeddingRequest(&schemas.BifrostEmbeddingRequest{
			Model: "gemini-embedding-2-preview",
			Input: []schemas.EmbeddingInputItem{{Content: schemas.EmbeddingContent{
				{Type: schemas.EmbeddingContentPartTypeImage, Image: &schemas.EmbeddingMediaPart{URL: schemas.Ptr("ftp://my-host/xxx.png")}},
			}}},
		})
		require.Error(t, err)
		require.True(t, providerUtils.IsInvalidRequestError(err), "expected a 400-class error, got %v", err)
	})

	// The other modalities never went through the image allowlist, so a gs:// URI already
	// reached Vertex for them. Pinned so the allowlist cannot be widened to cover them.
	t.Run("audio and video gs urls still pass through", func(t *testing.T) {
		for _, part := range []schemas.EmbeddingContentPart{
			{Type: schemas.EmbeddingContentPartTypeAudio, Audio: &schemas.EmbeddingMediaPart{URL: schemas.Ptr("gs://my-bucket/clip.mp3")}},
			{Type: schemas.EmbeddingContentPartTypeVideo, Video: &schemas.EmbeddingMediaPart{URL: schemas.Ptr("gs://my-bucket/clip.mp4")}},
		} {
			req, err := ToVertexGeminiEmbeddingRequest(&schemas.BifrostEmbeddingRequest{
				Model: "gemini-embedding-2-preview",
				Input: []schemas.EmbeddingInputItem{{Content: schemas.EmbeddingContent{part}}},
			})
			require.NoError(t, err, "part type %s", part.Type)
			require.NotNil(t, req.Content.Parts[0].FileData, "part type %s", part.Type)
		}
	})
}

func TestToVertexGeminiEmbeddingRequest(t *testing.T) {
	text := "hello"
	req, err := ToVertexGeminiEmbeddingRequest(&schemas.BifrostEmbeddingRequest{
		Input: []schemas.EmbeddingInputItem{{Content: schemas.EmbeddingContent{
			{Type: schemas.EmbeddingContentPartTypeText, Text: &text},
			{Type: schemas.EmbeddingContentPartTypeImage, Image: &schemas.EmbeddingMediaPart{URL: schemas.Ptr("https://example.com/img.png")}},
		}},
		},
		Params: &schemas.EmbeddingParameters{
			TaskType:   schemas.Ptr("RETRIEVAL_DOCUMENT"),
			Dimensions: schemas.Ptr(128),
		},
	})
	require.NoError(t, err)
	require.NotNil(t, req.Content)
	require.Len(t, req.Content.Parts, 2)
	require.Equal(t, "hello", req.Content.Parts[0].Text)
	require.NotNil(t, req.Content.Parts[1].FileData)
	require.Equal(t, 128, *req.OutputDimensionality)
}

func TestToVertexGeminiEmbeddingRequestRejectsBatch(t *testing.T) {
	t1 := "first"
	t2 := "second"
	_, err := ToVertexGeminiEmbeddingRequest(&schemas.BifrostEmbeddingRequest{
		Input: []schemas.EmbeddingInputItem{
			{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &t1}}},
			{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &t2}}},
		},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "batch")
}

// :predict takes many instances, so the OpenAI batched form maps onto it directly.
func TestToVertexEmbeddingRequestBatch(t *testing.T) {
	t.Run("each input becomes its own instance", func(t *testing.T) {
		t1 := "Hello!"
		t2 := "World"
		req, err := ToVertexEmbeddingRequest(&schemas.BifrostEmbeddingRequest{
			Input: []schemas.EmbeddingInputItem{
				{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &t1}}},
				{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &t2}}},
			},
			Params: &schemas.EmbeddingParameters{TaskType: schemas.Ptr("RETRIEVAL_DOCUMENT")},
		})
		require.NoError(t, err)
		require.Len(t, req.Instances, 2)
		require.Equal(t, "Hello!", req.Instances[0].Content)
		require.Equal(t, "World", req.Instances[1].Content)
		require.Equal(t, "RETRIEVAL_DOCUMENT", *req.Instances[1].TaskType)
	})

	t.Run("parts within one input still join into one instance", func(t *testing.T) {
		t1 := "first"
		t2 := "second"
		req, err := ToVertexEmbeddingRequest(&schemas.BifrostEmbeddingRequest{
			Input: []schemas.EmbeddingInputItem{{Content: schemas.EmbeddingContent{
				{Type: schemas.EmbeddingContentPartTypeText, Text: &t1},
				{Type: schemas.EmbeddingContentPartTypeText, Text: &t2},
			}}},
		})
		require.NoError(t, err)
		require.Len(t, req.Instances, 1)
		require.Equal(t, "first\nsecond", req.Instances[0].Content)
	})

	t.Run("a non-text part in any input is rejected", func(t *testing.T) {
		t1 := "fine"
		_, err := ToVertexEmbeddingRequest(&schemas.BifrostEmbeddingRequest{
			Input: []schemas.EmbeddingInputItem{
				{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &t1}}},
				{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeImage, Image: &schemas.EmbeddingMediaPart{URL: schemas.Ptr("https://example.com/img.png")}}}},
			},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "only supports text parts")
	})
}

// Go SDK callers still pass task_type, title and autoTruncate through ExtraParams (the
// documented aliases); the first-class fields win when both are set, and the consumed
// aliases are not forwarded again as extra params.
func TestToVertexEmbeddingRequest_HonoursExtraParamAliases(t *testing.T) {
	text := "hello"
	input := []schemas.EmbeddingInputItem{{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &text}}}}

	req := &schemas.BifrostEmbeddingRequest{Model: "text-embedding-005", Input: input, Params: &schemas.EmbeddingParameters{
		ExtraParams: map[string]interface{}{"task_type": "RETRIEVAL_QUERY", "title": "doc", "autoTruncate": false, "other": 1},
	}}
	out, err := ToVertexEmbeddingRequest(req)
	require.NoError(t, err)
	require.Equal(t, "RETRIEVAL_QUERY", *out.Instances[0].TaskType)
	require.Equal(t, "doc", *out.Instances[0].Title)
	require.False(t, *out.Parameters.AutoTruncate)
	require.Equal(t, map[string]interface{}{"other": 1}, out.ExtraParams)
	require.Len(t, req.Params.ExtraParams, 4, "the caller's extra params were modified")

	first := "SEMANTIC_SIMILARITY"
	req.Params.TaskType = &first
	req.Params.AutoTruncate = schemas.Ptr(true)
	out, err = ToVertexEmbeddingRequest(req)
	require.NoError(t, err)
	require.Equal(t, first, *out.Instances[0].TaskType, "the first-class field wins over the alias")
	require.True(t, *out.Parameters.AutoTruncate)
}

// documentOcr / audioTrackExtraction are only accepted inside embedContentConfig. Top-level
// keys are not rewritten (Vertex rejects them there), and the caller's params stay intact
// because retries and fallbacks reuse them.
func TestToVertexGeminiEmbeddingRequestForwardsEmbedContentConfig(t *testing.T) {
	text := "alpha"
	convert := func(extra map[string]interface{}) (*VertexGeminiEmbeddingRequest, []byte, []byte) {
		t.Helper()
		req, err := ToVertexGeminiEmbeddingRequest(&schemas.BifrostEmbeddingRequest{
			Model:  "gemini-embedding-2",
			Input:  []schemas.EmbeddingInputItem{{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &text}}}},
			Params: &schemas.EmbeddingParameters{ExtraParams: extra},
		})
		require.NoError(t, err)
		body, err := providerUtils.MarshalSorted(req)
		require.NoError(t, err)
		merged, err := providerUtils.MergeExtraParamsIntoJSON(body, req.ExtraParams)
		require.NoError(t, err)
		return req, body, merged
	}

	t.Run("nested config reaches the wire", func(t *testing.T) {
		extra := map[string]interface{}{"embedContentConfig": map[string]interface{}{"documentOcr": true, "audioTrackExtraction": true}}
		_, body, merged := convert(extra)
		// Without passthrough only the struct is sent; with it the extras are merged on top.
		for _, wire := range [][]byte{body, merged} {
			var out map[string]interface{}
			require.NoError(t, schemas.Unmarshal(wire, &out))
			cfg, _ := out["embedContentConfig"].(map[string]interface{})
			require.Equal(t, true, cfg["documentOcr"], string(wire))
			require.Equal(t, true, cfg["audioTrackExtraction"], string(wire))
		}
		_, again, _ := convert(extra)
		require.Equal(t, string(body), string(again), "a second attempt (retry/fallback) must send the same body")
	})

	t.Run("top-level keys are not rewritten", func(t *testing.T) {
		extra := map[string]interface{}{"documentOcr": true}
		req, body, _ := convert(extra)
		require.Nil(t, req.EmbedContentConfig)
		require.NotContains(t, string(body), "documentOcr")
		require.Contains(t, extra, "documentOcr", "caller's ExtraParams must not be mutated")
	})
}

// Contract C4: Bifrost adds no value the caller did not send. autoTruncate reaches Vertex
// only when the caller set it; otherwise Vertex applies its own default.
func TestToVertexEmbeddingRequestSendsNoAutoTruncateUnlessGiven(t *testing.T) {
	text := "hello"
	input := []schemas.EmbeddingInputItem{{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &text}}}}

	for name, params := range map[string]*schemas.EmbeddingParameters{
		"empty params":    {},
		"task_type only":  {TaskType: schemas.Ptr("RETRIEVAL_QUERY")},
		"unrelated extra": {ExtraParams: map[string]interface{}{"other": 1}},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := ToVertexEmbeddingRequest(&schemas.BifrostEmbeddingRequest{Model: "text-embedding-005", Input: input, Params: params})
			require.NoError(t, err)
			body, err := providerUtils.MarshalSorted(out)
			require.NoError(t, err)
			require.NotContains(t, string(body), "autoTruncate", "Bifrost wrote a value the caller did not send: %s", body)
		})
	}

	out, err := ToVertexEmbeddingRequest(&schemas.BifrostEmbeddingRequest{Model: "text-embedding-005", Input: input,
		Params: &schemas.EmbeddingParameters{Dimensions: schemas.Ptr(256)}})
	require.NoError(t, err)
	require.Nil(t, out.Parameters.AutoTruncate)
	require.Equal(t, 256, *out.Parameters.OutputDimensionality)

	out, err = ToVertexEmbeddingRequest(&schemas.BifrostEmbeddingRequest{Model: "text-embedding-005", Input: input,
		Params: &schemas.EmbeddingParameters{AutoTruncate: schemas.Ptr(false)}})
	require.NoError(t, err)
	require.False(t, *out.Parameters.AutoTruncate, "a caller's false is sent as false")
}

// Contract C5: Vertex text embeddings bill per character and report the count in the
// response metadata; it reaches usage.billed_characters, under either JSON spelling.
func TestVertexEmbeddingResponseReportsBillableCharacters(t *testing.T) {
	for name, body := range map[string]string{
		"REST camelCase": `{"predictions":[{"embeddings":{"values":[0.1],"statistics":{"token_count":3,"truncated":false}}}],"metadata":{"billableCharacterCount":11}}`,
		"proto snake":    `{"predictions":[{"embeddings":{"values":[0.1]}}],"metadata":{"billable_character_count":11}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var resp VertexEmbeddingResponse
			require.NoError(t, schemas.Unmarshal([]byte(body), &resp))
			out := resp.ToBifrostEmbeddingResponse()
			require.NotNil(t, out.Usage)
			require.NotNil(t, out.Usage.BilledCharacters)
			require.Equal(t, 11, *out.Usage.BilledCharacters)
		})
	}

	var none VertexEmbeddingResponse
	require.NoError(t, schemas.Unmarshal([]byte(`{"predictions":[{"embeddings":{"values":[0.1]}}]}`), &none))
	out := none.ToBifrostEmbeddingResponse()
	if out.Usage != nil {
		require.Nil(t, out.Usage.BilledCharacters, "never filled by Bifrost")
	}
}

// Every Vertex embedding endpoint returns float vectors only, so a non-float
// encoding_format is refused with caller_unsupported_parameter whichever endpoint the
// model reaches (:embedContent, multimodal :predict, text :predict), and float is accepted.
func TestVertexEmbeddingRequestBodyRefusesNonFloatEncodingFormat(t *testing.T) {
	for _, model := range []string{"gemini-embedding-2-preview", "multimodalembedding@001", "text-embedding-005"} {
		t.Run(model, func(t *testing.T) {
			req := func(format string) *schemas.BifrostEmbeddingRequest {
				return &schemas.BifrostEmbeddingRequest{
					Provider: schemas.Vertex,
					Model:    model,
					Input: []schemas.EmbeddingInputItem{
						{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: schemas.Ptr("hi")}}},
					},
					Params: &schemas.EmbeddingParameters{EncodingFormat: schemas.Ptr(format)},
				}
			}
			_, err := toVertexEmbeddingRequestBody(req(schemas.EmbeddingEncodingBase64))
			bifrostErr, ok := providerUtils.AsBifrostBadRequestError(err)
			require.True(t, ok, "want a bad request, got %v", err)
			require.Equal(t, 400, *bifrostErr.StatusCode)
			require.Equal(t, schemas.ErrorTypeCallerUnsupportedParameter, bifrostErr.ExtraFields.ErrorType)
			require.Equal(t, "encoding_format", bifrostErr.Error.Param)

			body, err := toVertexEmbeddingRequestBody(req(schemas.EmbeddingEncodingFloat))
			require.NoError(t, err)
			require.NotNil(t, body)
		})
	}
}

// Request path cost of the Vertex text embedding conversion (TXE-6636).
func BenchmarkToVertexEmbeddingRequest(b *testing.B) {
	req := &schemas.BifrostEmbeddingRequest{
		Provider: schemas.Vertex,
		Model:    "text-embedding-005",
		Input: []schemas.EmbeddingInputItem{
			{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: schemas.Ptr("alpha kestrel harbor")}}},
		},
		Params: &schemas.EmbeddingParameters{TaskType: schemas.Ptr("RETRIEVAL_QUERY")},
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ToVertexEmbeddingRequest(req); err != nil {
			b.Fatal(err)
		}
	}
}

// Response path cost of reading Vertex usage, billable characters included (TXE-6636).
func BenchmarkVertexEmbeddingResponseToBifrost(b *testing.B) {
	var resp VertexEmbeddingResponse
	if err := schemas.Unmarshal([]byte(`{"predictions":[{"embeddings":{"values":[0.1,0.2,0.3],"statistics":{"token_count":5,"truncated":false}}}],"metadata":{"billableCharacterCount":18}}`), &resp); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if out := resp.ToBifrostEmbeddingResponse(); out == nil {
			b.Fatal("nil response")
		}
	}
}
