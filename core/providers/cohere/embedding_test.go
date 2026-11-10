package cohere

import (
	"context"
	"testing"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToCohereEmbeddingRequest(t *testing.T) {
	t.Run("returns error for missing input", func(t *testing.T) {
		_, err := ToCohereEmbeddingRequest(nil)
		assert.Error(t, err)
		_, err = ToCohereEmbeddingRequest(&schemas.BifrostEmbeddingRequest{})
		assert.Error(t, err)
		_, err = ToCohereEmbeddingRequest(&schemas.BifrostEmbeddingRequest{
			Input: nil,
		})
		assert.Error(t, err)
	})

	t.Run("single text content extracts typed params", func(t *testing.T) {
		text := "hello"
		truncate := "END"
		dimensions := 1024
		maxTokens := 256
		bifrostReq := &schemas.BifrostEmbeddingRequest{
			Model: "embed-v4.0",
			Input: []schemas.EmbeddingInputItem{
				{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &text}}},
			},
			Params: &schemas.EmbeddingParameters{
				Dimensions: &dimensions,
				ExtraParams: map[string]interface{}{
					"input_type":      "classification",
					"embedding_types": []string{"float", "int8"},
					"priority":        "high",
					"max_tokens":      maxTokens,
					"truncate":        truncate,
				},
			},
		}

		req, err := ToCohereEmbeddingRequest(bifrostReq)
		require.NoError(t, err)
		require.NotNil(t, req)
		assert.Equal(t, "embed-v4.0", req.Model)
		assert.Equal(t, "classification", req.InputType)
		assert.Equal(t, []string{"hello"}, req.Texts)
		assert.Equal(t, []string{"float", "int8"}, req.EmbeddingTypes)
		assert.Equal(t, &dimensions, req.OutputDimension)
		assert.Equal(t, &maxTokens, req.MaxTokens)
		require.NotNil(t, req.Truncate)
		assert.Equal(t, truncate, *req.Truncate)
		assert.Equal(t, map[string]interface{}{"priority": "high"}, req.ExtraParams)
	})

	t.Run("multiple text contents batch into texts array with task_type as input_type", func(t *testing.T) {
		hello := "hello"
		world := "world"
		req, err := ToCohereEmbeddingRequest(&schemas.BifrostEmbeddingRequest{
			Model:  "embed-english-v3.0",
			Params: &schemas.EmbeddingParameters{TaskType: schemas.Ptr("search_query")},
			Input: []schemas.EmbeddingInputItem{
				{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &hello}}},
				{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &world}}},
			},
		})

		require.NoError(t, err)
		require.NotNil(t, req)
		assert.Equal(t, "embed-english-v3.0", req.Model)
		assert.Equal(t, "search_query", req.InputType)
		assert.Equal(t, []string{"hello", "world"}, req.Texts)
		assert.Nil(t, req.ExtraParams)
	})

	t.Run("multimodal content uses inputs array", func(t *testing.T) {
		text := "describe this"
		imageURL := "data:image/jpeg;base64,abc123"
		req, err := ToCohereEmbeddingRequest(&schemas.BifrostEmbeddingRequest{
			Model:  "embed-v4.0",
			Params: &schemas.EmbeddingParameters{TaskType: schemas.Ptr("search_document")},
			Input: []schemas.EmbeddingInputItem{
				{Content: schemas.EmbeddingContent{
					{Type: schemas.EmbeddingContentPartTypeText, Text: &text},
					{Type: schemas.EmbeddingContentPartTypeImage, Image: &schemas.EmbeddingMediaPart{URL: &imageURL}},
				}},
			},
		})

		require.NoError(t, err)
		require.NotNil(t, req)
		require.Len(t, req.Inputs, 1)
		require.Len(t, req.Inputs[0].Content, 2)
		assert.Equal(t, CohereContentBlockTypeText, req.Inputs[0].Content[0].Type)
		assert.Equal(t, CohereContentBlockTypeImage, req.Inputs[0].Content[1].Type)
	})
}

func TestToCohereEmbeddingRequestBodyIncludesModelForDirectCohere(t *testing.T) {
	text := "hello"
	bifrostReq := &schemas.BifrostEmbeddingRequest{
		Model:  "embed-v4.0",
		Params: &schemas.EmbeddingParameters{TaskType: schemas.Ptr("search_document")},
		Input: []schemas.EmbeddingInputItem{
			{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &text}}},
		},
	}

	wireBody, bifrostErr := providerUtils.CheckContextAndGetRequestBody(
		context.Background(),
		bifrostReq,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			return ToCohereEmbeddingRequest(bifrostReq)
		},
	)
	require.Nil(t, bifrostErr)
	assert.JSONEq(t, `{
		"model": "embed-v4.0",
		"input_type": "search_document",
		"texts": ["hello"]
	}`, string(wireBody))
}

func TestCohereEmbeddingResponseLabelsEachEncoding(t *testing.T) {
	base64Vec := "AAEC"
	resp := &CohereEmbeddingResponse{
		Embeddings: &CohereEmbeddingData{
			Float:   [][]float64{{0.1, 0.2}},
			Int8:    [][]int8{{1, -1}},
			Uint8:   [][]int32{{0, 255}},
			Binary:  [][]int8{{1, 0}},
			Ubinary: [][]int32{{7, 8}},
			Base64:  []string{base64Vec},
		},
	}

	bifrostResp := resp.ToBifrostEmbeddingResponse()
	require.Len(t, bifrostResp.Data, 6)

	byFormat := map[string]schemas.EmbeddingData{}
	for _, d := range bifrostResp.Data {
		assert.Equal(t, 0, d.Index, "every representation describes input 0")
		byFormat[d.EncodingFormat] = d
	}

	assert.Equal(t, []float64{0.1, 0.2}, byFormat[schemas.EmbeddingEncodingFloat].Embedding.EmbeddingArray)
	assert.Equal(t, []int8{1, -1}, byFormat[schemas.EmbeddingEncodingInt8].Embedding.EmbeddingInt8Array)
	assert.Equal(t, []int8{1, 0}, byFormat[schemas.EmbeddingEncodingBinary].Embedding.EmbeddingInt8Array)
	assert.Equal(t, []int32{0, 255}, byFormat[schemas.EmbeddingEncodingUint8].Embedding.EmbeddingInt32Array)
	assert.Equal(t, []int32{7, 8}, byFormat[schemas.EmbeddingEncodingUbinary].Embedding.EmbeddingInt32Array)
	require.NotNil(t, byFormat[schemas.EmbeddingEncodingBase64].Embedding.EmbeddingStr)
	assert.Equal(t, base64Vec, *byFormat[schemas.EmbeddingEncodingBase64].Embedding.EmbeddingStr)

	// The labels are what let the Cohere envelope be rebuilt without losing an encoding.
	roundTrip := ToCohereEmbeddingResponse(bifrostResp)
	require.NotNil(t, roundTrip.ResponseType)
	assert.Equal(t, "embeddings_by_type", *roundTrip.ResponseType)
	assert.Equal(t, resp.Embeddings.Float, roundTrip.Embeddings.Float)
	assert.Equal(t, resp.Embeddings.Int8, roundTrip.Embeddings.Int8)
	assert.Equal(t, resp.Embeddings.Uint8, roundTrip.Embeddings.Uint8)
	assert.Equal(t, resp.Embeddings.Binary, roundTrip.Embeddings.Binary)
	assert.Equal(t, resp.Embeddings.Ubinary, roundTrip.Embeddings.Ubinary)
	assert.Equal(t, resp.Embeddings.Base64, roundTrip.Embeddings.Base64)
}

func TestToCohereEmbeddingResponseOrdersByIndex(t *testing.T) {
	bifrostResp := &schemas.BifrostEmbeddingResponse{
		Data: []schemas.EmbeddingData{
			{Index: 1, EncodingFormat: schemas.EmbeddingEncodingInt8, Embedding: schemas.EmbeddingStruct{EmbeddingInt8Array: []int8{11}}},
			{Index: 1, EncodingFormat: schemas.EmbeddingEncodingFloat, Embedding: schemas.EmbeddingStruct{EmbeddingArray: []float64{1.1}}},
			{Index: 0, EncodingFormat: schemas.EmbeddingEncodingInt8, Embedding: schemas.EmbeddingStruct{EmbeddingInt8Array: []int8{10}}},
			{Index: 0, EncodingFormat: schemas.EmbeddingEncodingFloat, Embedding: schemas.EmbeddingStruct{EmbeddingArray: []float64{1.0}}},
		},
	}

	resp := ToCohereEmbeddingResponse(bifrostResp)
	assert.Equal(t, [][]float64{{1.0}, {1.1}}, resp.Embeddings.Float)
	assert.Equal(t, [][]int8{{10}, {11}}, resp.Embeddings.Int8)
	assert.Equal(t, 1, bifrostResp.Data[0].Index, "caller's data must not be reordered")

	plain := ToCohereEmbeddingResponse(&schemas.BifrostEmbeddingResponse{
		Data: []schemas.EmbeddingData{
			{Index: 2, Embedding: schemas.EmbeddingStruct{EmbeddingArray: []float64{2}}},
			{Index: 1, Embedding: schemas.EmbeddingStruct{EmbeddingArray: []float64{1}}},
			{Index: 0, Embedding: schemas.EmbeddingStruct{EmbeddingArray: []float64{0}}},
		},
	})
	// The object form is always built, and Cohere's v2 API labels it embeddings_by_type.
	assert.Equal(t, "embeddings_by_type", *plain.ResponseType)
	assert.Equal(t, [][]float64{{0}, {1}, {2}}, plain.Embeddings.Float)
}

func TestCohereEmbeddingUint8VectorsSerializeAsNumbers(t *testing.T) {
	// []uint8 is []byte, which marshals to a base64 string; int32 keeps the array form.
	body, err := schemas.Marshal(&CohereEmbeddingData{Uint8: [][]int32{{0, 1, 2, 255}}})
	require.NoError(t, err)
	assert.JSONEq(t, `{"uint8":[[0,1,2,255]]}`, string(body))
}

func TestCohereEmbeddingResponseCarriesImageTokens(t *testing.T) {
	// Live /v2/embed on an image: input_tokens 0, image_tokens 6, no tokens block.
	inputTokens, imageTokens := 0, 6
	bifrostResp := (&CohereEmbeddingResponse{
		Embeddings: &CohereEmbeddingData{Float: [][]float64{{1, 2}}},
		Meta: &CohereEmbeddingMeta{
			BilledUnits: &CohereBilledUnits{InputTokens: &inputTokens, ImageTokens: &imageTokens},
		},
	}).ToBifrostEmbeddingResponse()

	require.NotNil(t, bifrostResp.Usage)
	require.NotNil(t, bifrostResp.Usage.PromptTokensDetails)
	assert.Equal(t, 6, bifrostResp.Usage.PromptTokensDetails.ImageTokens)
	assert.Equal(t, 0, bifrostResp.Usage.PromptTokens, "image tokens bill separately, so they stay out of the text cost base")
	assert.Equal(t, 6, bifrostResp.Usage.TotalTokens)
}

func TestToCohereEmbeddingResponseMetaMatchesEmbedEndpoint(t *testing.T) {
	// /v2/embed returns billed_units only: no tokens block, and never output_tokens.
	resp := ToCohereEmbeddingResponse(&schemas.BifrostEmbeddingResponse{
		Data: []schemas.EmbeddingData{{Index: 0, Embedding: schemas.EmbeddingStruct{EmbeddingArray: []float64{1}}}},
		Usage: &schemas.BifrostLLMUsage{
			PromptTokens:        3,
			TotalTokens:         9,
			PromptTokensDetails: &schemas.ChatPromptTokensDetails{ImageTokens: 6},
		},
	})

	require.NotNil(t, resp.Meta)
	assert.Nil(t, resp.Meta.Tokens, "embed never returns a tokens block")
	require.NotNil(t, resp.Meta.BilledUnits)
	assert.Nil(t, resp.Meta.BilledUnits.OutputTokens, "embed never returns output_tokens")
	require.NotNil(t, resp.Meta.BilledUnits.InputTokens)
	assert.Equal(t, 3, *resp.Meta.BilledUnits.InputTokens)
	require.NotNil(t, resp.Meta.BilledUnits.ImageTokens)
	assert.Equal(t, 6, *resp.Meta.BilledUnits.ImageTokens)
	assert.Nil(t, resp.Meta.BilledUnits.Images, "no image count reported, none invented")

	withImages := ToCohereEmbeddingResponse(&schemas.BifrostEmbeddingResponse{
		Data:  []schemas.EmbeddingData{{Index: 0, Embedding: schemas.EmbeddingStruct{EmbeddingArray: []float64{1}}}},
		Usage: &schemas.BifrostLLMUsage{InputImages: schemas.Ptr(2)},
	})
	require.NotNil(t, withImages.Meta.BilledUnits.Images, "the native route returns the billed image count")
	assert.Equal(t, 2, *withImages.Meta.BilledUnits.Images)
}

// The converter runs again on a retry or fallback with the same request, so lifting the
// native fields out of extra params must not consume the caller's map.
func TestToCohereEmbeddingRequest_LeavesCallerExtraParamsIntact(t *testing.T) {
	text := "hello"
	req := &schemas.BifrostEmbeddingRequest{
		Model: "embed-v4.0",
		Input: []schemas.EmbeddingInputItem{{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &text}}}},
		Params: &schemas.EmbeddingParameters{ExtraParams: map[string]interface{}{
			"input_type": "search_query", "truncate": "END", "other": "kept",
		}},
	}
	for attempt := 0; attempt < 2; attempt++ {
		out, err := ToCohereEmbeddingRequest(req)
		require.NoError(t, err)
		assert.Equal(t, "search_query", out.InputType, "attempt %d", attempt)
		require.NotNil(t, out.Truncate, "attempt %d", attempt)
		assert.NotContains(t, out.ExtraParams, "input_type", "attempt %d", attempt)
		assert.Equal(t, "kept", out.ExtraParams["other"], "attempt %d", attempt)
	}
	assert.Len(t, req.Params.ExtraParams, 3, "the caller's extra params were modified")
}

func TestToCohereEmbeddingRequestTextOnlyUsesTexts(t *testing.T) {
	text := "hello"
	req, err := ToCohereEmbeddingRequest(&schemas.BifrostEmbeddingRequest{
		Model:  "embed-v4.0",
		Params: &schemas.EmbeddingParameters{TaskType: schemas.Ptr("search_document")},
		Input:  []schemas.EmbeddingInputItem{{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &text}}}},
	})

	require.NoError(t, err)
	require.NotNil(t, req)
	require.Equal(t, []string{"hello"}, req.Texts)
	require.Empty(t, req.Inputs)
}

func TestToCohereEmbeddingRequestMultimodalUsesInputs(t *testing.T) {
	caption := "caption"
	req, err := ToCohereEmbeddingRequest(&schemas.BifrostEmbeddingRequest{
		Model:  "embed-v4.0",
		Params: &schemas.EmbeddingParameters{TaskType: schemas.Ptr("search_document")},
		Input: []schemas.EmbeddingInputItem{{Content: schemas.EmbeddingContent{
			{Type: schemas.EmbeddingContentPartTypeText, Text: &caption},
			{Type: schemas.EmbeddingContentPartTypeImage, Image: &schemas.EmbeddingMediaPart{URL: schemas.Ptr("https://example.com/cat.png")}},
		}},
		},
	})

	require.NoError(t, err)
	require.NotNil(t, req)
	require.Len(t, req.Inputs, 1)
	require.Len(t, req.Inputs[0].Content, 2)
	require.Equal(t, CohereContentBlockTypeText, req.Inputs[0].Content[0].Type)
	require.Equal(t, CohereContentBlockTypeImage, req.Inputs[0].Content[1].Type)
}

func TestToCohereEmbeddingRequestRejectsUnsupportedModalities(t *testing.T) {
	_, err := ToCohereEmbeddingRequest(&schemas.BifrostEmbeddingRequest{
		Model:  "embed-v4.0",
		Params: &schemas.EmbeddingParameters{TaskType: schemas.Ptr("search_document")},
		Input: []schemas.EmbeddingInputItem{{Content: schemas.EmbeddingContent{
			{Type: schemas.EmbeddingContentPartTypeAudio, Audio: &schemas.EmbeddingMediaPart{URL: schemas.Ptr("https://example.com/audio.mp3")}},
		}},
		},
	})

	require.Error(t, err)
}

func TestToCohereEmbeddingRequestBuildsDataURIFromDataAndMIMEType(t *testing.T) {
	caption := "caption"
	image := schemas.EmbeddingMediaPart{Data: schemas.Ptr("iVBORw0KGgo="), MIMEType: schemas.Ptr("image/png")}

	lone, err := ToCohereEmbeddingRequest(&schemas.BifrostEmbeddingRequest{
		Model:  "embed-v4.0",
		Params: &schemas.EmbeddingParameters{TaskType: schemas.Ptr("search_document")},
		Input:  []schemas.EmbeddingInputItem{{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeImage, Image: &image}}}},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"data:image/png;base64,iVBORw0KGgo="}, lone.Images)

	mixed, err := ToCohereEmbeddingRequest(&schemas.BifrostEmbeddingRequest{
		Model:  "embed-v4.0",
		Params: &schemas.EmbeddingParameters{TaskType: schemas.Ptr("search_document")},
		Input: []schemas.EmbeddingInputItem{{Content: schemas.EmbeddingContent{
			{Type: schemas.EmbeddingContentPartTypeText, Text: &caption},
			{Type: schemas.EmbeddingContentPartTypeImage, Image: &image},
		}}},
	})
	require.NoError(t, err)
	require.Equal(t, "data:image/png;base64,iVBORw0KGgo=", mixed.Inputs[0].Content[1].ImageURL.URL)
}

func TestToCohereEmbeddingRequestAllLoneImagesUseImages(t *testing.T) {
	a, b := "data:image/png;base64,AAAA", "data:image/jpeg;base64,BBBB"
	req, err := ToCohereEmbeddingRequest(&schemas.BifrostEmbeddingRequest{
		Model:  "embed-v4.0",
		Params: &schemas.EmbeddingParameters{TaskType: schemas.Ptr("search_document")},
		Input: []schemas.EmbeddingInputItem{
			{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeImage, Image: &schemas.EmbeddingMediaPart{URL: &a}}}},
			{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeImage, Image: &schemas.EmbeddingMediaPart{URL: &b}}}},
		},
	})

	require.NoError(t, err)
	require.Equal(t, []string{a, b}, req.Images)
	require.Empty(t, req.Inputs)
}

func TestToCohereEmbeddingRequestMapsEncodingFormat(t *testing.T) {
	text := "hello"
	input := []schemas.EmbeddingInputItem{{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &text}}}}

	req, err := ToCohereEmbeddingRequest(&schemas.BifrostEmbeddingRequest{
		Model:  "embed-v4.0",
		Input:  input,
		Params: &schemas.EmbeddingParameters{EncodingFormat: schemas.Ptr("base64"), TaskType: schemas.Ptr("search_document")},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"base64"}, req.EmbeddingTypes)

	explicit, err := ToCohereEmbeddingRequest(&schemas.BifrostEmbeddingRequest{
		Model: "embed-v4.0",
		Input: input,
		Params: &schemas.EmbeddingParameters{
			EncodingFormat: schemas.Ptr("base64"),
			TaskType:       schemas.Ptr("search_document"),
			ExtraParams:    map[string]interface{}{"embedding_types": []string{"int8"}},
		},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"int8"}, explicit.EmbeddingTypes)
}

func TestCohereEmbeddingRequestRoundTripKeepsMaxTokensAndPriority(t *testing.T) {
	bifrostReq, err := (&CohereEmbeddingRequest{
		Model:     "embed-v4.0",
		InputType: "search_document",
		Texts:     []string{"hello"},
		MaxTokens: schemas.Ptr(64),
		Priority:  schemas.Ptr(5),
	}).ToBifrostEmbeddingRequest(nil)
	require.NoError(t, err)

	req, err := ToCohereEmbeddingRequest(bifrostReq)
	require.NoError(t, err)
	require.Equal(t, 64, *req.MaxTokens)
	require.Equal(t, 5, *req.Priority)
}

func TestCohereEmbeddingRequestUnsupportedBlockIsInvalidRequest(t *testing.T) {
	_, err := (&CohereEmbeddingRequest{
		Model:  "embed-v4.0",
		Inputs: []CohereEmbeddingInput{{Content: []CohereContentBlock{{Type: "audio"}}}},
	}).ToBifrostEmbeddingRequest(nil)

	badRequest, ok := providerUtils.AsBifrostBadRequestError(err)
	require.True(t, ok)
	require.Equal(t, `unsupported cohere embedding block type "audio"`, badRequest.Error.Message)
}

// Contract C4: Cohere's input_type is required from v3 on, and Bifrost adds no value the
// caller did not send. A request with neither task_type nor input_type is refused before
// any upstream call, as 400 caller_unsupported_parameter naming task_type.
func TestToCohereEmbeddingRequestWithoutTaskTypeIsUnsupportedParameter(t *testing.T) {
	text := "hello"
	input := []schemas.EmbeddingInputItem{{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &text}}}}

	for name, params := range map[string]*schemas.EmbeddingParameters{
		"no params":           nil,
		"params without it":   {Dimensions: schemas.Ptr(256)},
		"empty task_type":     {TaskType: schemas.Ptr("")},
		"non-string extra it": {ExtraParams: map[string]interface{}{"input_type": 3}},
	} {
		t.Run(name, func(t *testing.T) {
			req := &schemas.BifrostEmbeddingRequest{Model: "embed-v4.0", Input: input, Params: params}
			wireBody, bifrostErr := providerUtils.CheckContextAndGetRequestBody(context.Background(), req,
				func() (providerUtils.RequestBodyWithExtraParams, error) { return ToCohereEmbeddingRequest(req) })

			require.Nil(t, wireBody, "nothing may reach Cohere")
			require.NotNil(t, bifrostErr)
			require.NotNil(t, bifrostErr.StatusCode)
			assert.Equal(t, 400, *bifrostErr.StatusCode)
			assert.Equal(t, schemas.ErrorTypeCallerUnsupportedParameter, bifrostErr.ExtraFields.ErrorType)
			require.NotNil(t, bifrostErr.Error)
			assert.Equal(t, "task_type", bifrostErr.Error.Param)
			assert.False(t, bifrostErr.IsBifrostError)
		})
	}
}

// task_type is the unified field (contract C4) and is converted to input_type: Cohere's own
// values as given, the Gemini dialect's onto their equivalent, anything else refused. A
// caller speaking Cohere's own name through extra params still works, and the two may not
// disagree.
func TestToCohereEmbeddingRequestDeliversTaskTypeAsInputType(t *testing.T) {
	text := "hello"
	input := []schemas.EmbeddingInputItem{{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &text}}}}

	req := &schemas.BifrostEmbeddingRequest{Model: "embed-v4.0", Input: input, Params: &schemas.EmbeddingParameters{TaskType: schemas.Ptr("classification")}}
	wireBody, bifrostErr := providerUtils.CheckContextAndGetRequestBody(context.Background(), req,
		func() (providerUtils.RequestBodyWithExtraParams, error) { return ToCohereEmbeddingRequest(req) })
	require.Nil(t, bifrostErr)
	assert.JSONEq(t, `{"model":"embed-v4.0","input_type":"classification","texts":["hello"]}`, string(wireBody))

	extra, err := ToCohereEmbeddingRequest(&schemas.BifrostEmbeddingRequest{Model: "embed-v4.0", Input: input,
		Params: &schemas.EmbeddingParameters{ExtraParams: map[string]interface{}{"input_type": "image"}}})
	require.NoError(t, err)
	assert.Equal(t, "image", extra.InputType)
	assert.NotContains(t, extra.ExtraParams, "input_type")

	same, err := ToCohereEmbeddingRequest(&schemas.BifrostEmbeddingRequest{Model: "embed-v4.0", Input: input,
		Params: &schemas.EmbeddingParameters{TaskType: schemas.Ptr("clustering"), ExtraParams: map[string]interface{}{"input_type": "clustering"}}})
	require.NoError(t, err)
	assert.Equal(t, "clustering", same.InputType)

	for taskType, want := range map[string]string{
		"RETRIEVAL_QUERY":    "search_query",
		"RETRIEVAL_DOCUMENT": "search_document",
		"CLASSIFICATION":     "classification",
		"Search_Query":       "search_query",
	} {
		mapped, err := ToCohereEmbeddingRequest(&schemas.BifrostEmbeddingRequest{Model: "embed-v4.0", Input: input,
			Params: &schemas.EmbeddingParameters{TaskType: schemas.Ptr(taskType)}})
		require.NoError(t, err, taskType)
		assert.Equal(t, want, mapped.InputType, taskType)
	}

	// No Cohere equivalent: refused with the field named, never sent as something else.
	unmappable := &schemas.BifrostEmbeddingRequest{Model: "embed-v4.0", Input: input,
		Params: &schemas.EmbeddingParameters{TaskType: schemas.Ptr("SEMANTIC_SIMILARITY")}}
	body, unmappableErr := providerUtils.CheckContextAndGetRequestBody(context.Background(), unmappable,
		func() (providerUtils.RequestBodyWithExtraParams, error) { return ToCohereEmbeddingRequest(unmappable) })
	require.Nil(t, body)
	require.NotNil(t, unmappableErr)
	assert.Equal(t, 400, *unmappableErr.StatusCode)
	assert.Equal(t, schemas.ErrorTypeCallerUnsupportedParameter, unmappableErr.ExtraFields.ErrorType)
	assert.Equal(t, "task_type", unmappableErr.Error.Param)

	_, err = ToCohereEmbeddingRequest(&schemas.BifrostEmbeddingRequest{Model: "embed-v4.0", Input: input,
		Params: &schemas.EmbeddingParameters{TaskType: schemas.Ptr("search_query"), ExtraParams: map[string]interface{}{"input_type": "search_document"}}})
	badRequest, ok := providerUtils.AsBifrostBadRequestError(err)
	require.True(t, ok, "disagreeing task_type and input_type is the caller's error")
	assert.Equal(t, 400, *badRequest.StatusCode)
}

// Contract C5: usage is what Cohere bills. billed_units wins over the processed counts in
// meta.tokens, and the billed image count reaches usage.input_images.
func TestCohereEmbeddingUsageIsBilledUnits(t *testing.T) {
	t.Run("billed_units first, images counted", func(t *testing.T) {
		resp := (&CohereEmbeddingResponse{
			Embeddings: &CohereEmbeddingData{Float: [][]float64{{1}, {2}}},
			Meta: &CohereEmbeddingMeta{
				Tokens:      &CohereTokenUsage{InputTokens: schemas.Ptr(40)},
				BilledUnits: &CohereBilledUnits{InputTokens: schemas.Ptr(12), ImageTokens: schemas.Ptr(1000), Images: schemas.Ptr(2)},
			},
		}).ToBifrostEmbeddingResponse()

		require.NotNil(t, resp.Usage)
		assert.Equal(t, 12, resp.Usage.PromptTokens, "billed, not processed, input tokens")
		require.NotNil(t, resp.Usage.PromptTokensDetails)
		assert.Equal(t, 1000, resp.Usage.PromptTokensDetails.ImageTokens)
		require.NotNil(t, resp.Usage.InputImages)
		assert.Equal(t, 2, *resp.Usage.InputImages)
		assert.Equal(t, 1012, resp.Usage.TotalTokens)

		body, err := schemas.Marshal(resp.Usage)
		require.NoError(t, err)
		var wire map[string]any
		require.NoError(t, schemas.Unmarshal(body, &wire))
		assert.EqualValues(t, 2, wire["input_images"], "the image count is on the wire")
		assert.EqualValues(t, 12, wire["prompt_tokens"])
	})

	t.Run("text only reports no image count", func(t *testing.T) {
		resp := (&CohereEmbeddingResponse{
			Meta: &CohereEmbeddingMeta{BilledUnits: &CohereBilledUnits{InputTokens: schemas.Ptr(7)}},
		}).ToBifrostEmbeddingResponse()
		require.NotNil(t, resp.Usage)
		assert.Equal(t, 7, resp.Usage.PromptTokens)
		assert.Nil(t, resp.Usage.InputImages, "never filled by Bifrost")
		assert.Nil(t, resp.Usage.PromptTokensDetails)
	})

	t.Run("tokens only when nothing is billed", func(t *testing.T) {
		resp := (&CohereEmbeddingResponse{
			Meta: &CohereEmbeddingMeta{Tokens: &CohereTokenUsage{InputTokens: schemas.Ptr(5)}},
		}).ToBifrostEmbeddingResponse()
		require.NotNil(t, resp.Usage)
		assert.Equal(t, 5, resp.Usage.PromptTokens)
		assert.Equal(t, 5, resp.Usage.TotalTokens)
	})

	t.Run("no counters, no usage", func(t *testing.T) {
		resp := (&CohereEmbeddingResponse{Meta: &CohereEmbeddingMeta{}}).ToBifrostEmbeddingResponse()
		assert.Nil(t, resp.Usage)
	})
}
