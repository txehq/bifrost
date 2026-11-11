package gemini

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestToGeminiEmbeddingRequestBatchContentUsesBatchRequest(t *testing.T) {
	one := "one"
	two := "two"
	req, err := ToGeminiEmbeddingRequest(&schemas.BifrostEmbeddingRequest{
		Model: "gemini-embedding-001",
		Input: []schemas.EmbeddingInputItem{
			{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &one}}},
			{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &two}}},
		},
	})
	require.NoError(t, err)

	require.Len(t, req.Requests, 2)
	require.Equal(t, "one", req.Requests[0].Content.Parts[0].Text)
	require.Equal(t, "two", req.Requests[1].Content.Parts[0].Text)
}

func TestGeminiGenerationRequestToBifrostEmbeddingRequestPreservesMultimodalContent(t *testing.T) {
	request := &GeminiGenerationRequest{
		Model: "gemini/gemini-embedding-001",
		Requests: []GeminiEmbeddingRequest{
			{
				Content: &Content{
					Parts: []*Part{
						{Text: "hello"},
						{FileData: &FileData{FileURI: "https://example.com/img.png", MIMEType: "image/png"}},
					},
				},
			},
		},
	}

	bifrostReq := request.ToBifrostEmbeddingRequest(schemas.NewBifrostContext(nil, schemas.NoDeadline))
	require.NotNil(t, bifrostReq)
	require.NotNil(t, bifrostReq.Input)
	require.Len(t, bifrostReq.Input, 1)
	require.Len(t, bifrostReq.Input[0].Content, 2)
	require.Equal(t, schemas.EmbeddingContentPartTypeText, bifrostReq.Input[0].Content[0].Type)
	require.Equal(t, schemas.EmbeddingContentPartTypeImage, bifrostReq.Input[0].Content[1].Type)
}

// Issue #7812: a request-level `taskType` / `title` supplied as a Gemini-native
// extra param on /v1/embeddings must reach every entry of :batchEmbedContents,
// and must not leak to the top level of the batch body (Gemini rejects unknown
// top-level fields).
func TestToGeminiEmbeddingRequestRequestLevelTaskTypeExtraParamReachesEveryInput(t *testing.T) {
	texts := []string{
		"Biodegradable Hydraulic Ester Fluid 46",
		"stainless steel ball valves",
		"contract manufacturing of injection moulded parts",
	}
	input := make(schemas.EmbeddingInput, 0, len(texts))
	for i := range texts {
		input = append(input, schemas.EmbeddingInputItem{
			Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &texts[i]}},
		})
	}
	req, err := ToGeminiEmbeddingRequest(&schemas.BifrostEmbeddingRequest{
		Model: "gemini-embedding-001",
		Input: input,
		Params: &schemas.EmbeddingParameters{
			ExtraParams: map[string]interface{}{
				"taskType": "RETRIEVAL_DOCUMENT",
				"title":    "Industrial catalogue",
			},
		},
	})
	require.NoError(t, err)
	require.Len(t, req.Requests, len(texts))

	for i, r := range req.Requests {
		require.NotNil(t, r.TaskType, "requests[%d].taskType was dropped", i)
		require.Equal(t, "RETRIEVAL_DOCUMENT", *r.TaskType, "requests[%d].taskType", i)
		require.NotNil(t, r.Title, "requests[%d].title was dropped", i)
		require.Equal(t, "Industrial catalogue", *r.Title, "requests[%d].title", i)
	}
	require.NotContains(t, req.ExtraParams, "taskType", "taskType must not leak to the top level of :batchEmbedContents")
	require.NotContains(t, req.ExtraParams, "title", "title must not leak to the top level of :batchEmbedContents")

	// Same invariant on the wire bytes, with extra-param passthrough enabled as the
	// HTTP transport does for x-bf-passthrough-extra-params.
	body, err := providerUtils.MarshalProviderRequest(req)
	require.NoError(t, err)
	body, err = providerUtils.MergeExtraParamsIntoJSON(body, req.GetExtraParams())
	require.NoError(t, err)
	for i := range texts {
		require.Equal(t, "RETRIEVAL_DOCUMENT", gjson.GetBytes(body, fmt.Sprintf("requests.%d.taskType", i)).String(), "wire requests[%d].taskType", i)
		require.Equal(t, "Industrial catalogue", gjson.GetBytes(body, fmt.Sprintf("requests.%d.title", i)).String(), "wire requests[%d].title", i)
	}
	require.False(t, gjson.GetBytes(body, "taskType").Exists(), "top-level taskType on the wire: %s", body)
	require.False(t, gjson.GetBytes(body, "title").Exists(), "top-level title on the wire: %s", body)
}

// The first-class snake_case parameters must fan out to every entry too.
func TestToGeminiEmbeddingRequestRequestLevelTaskTypeParamReachesEveryInput(t *testing.T) {
	one, two, three := "one", "two", "three"
	taskType, title := "RETRIEVAL_DOCUMENT", "Catalogue"
	req, err := ToGeminiEmbeddingRequest(&schemas.BifrostEmbeddingRequest{
		Model: "gemini-embedding-001",
		Input: schemas.EmbeddingInput{
			{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &one}}},
			{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &two}}},
			{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &three}}},
		},
		Params: &schemas.EmbeddingParameters{TaskType: &taskType, Title: &title},
	})
	require.NoError(t, err)
	require.Len(t, req.Requests, 3)
	for i, r := range req.Requests {
		require.NotNil(t, r.TaskType, "requests[%d].taskType was dropped", i)
		require.Equal(t, taskType, *r.TaskType, "requests[%d].taskType", i)
		require.NotNil(t, r.Title, "requests[%d].title was dropped", i)
		require.Equal(t, title, *r.Title, "requests[%d].title", i)
	}
}

// GenAI SDKs repeat taskType/outputDimensionality on every batchEmbedContents entry.
// Identical params must land at request level so providers that reject per-item
// params (openai, vertex, bedrock, cohere) still accept the request.
func TestGeminiEmbeddingRequestsHoistUniformParamsToRequestLevel(t *testing.T) {
	dims := 8
	taskType := "RETRIEVAL_DOCUMENT"
	entry := func(text string) GeminiEmbeddingRequest {
		return GeminiEmbeddingRequest{
			Content:              &Content{Parts: []*Part{{Text: text}}},
			TaskType:             &taskType,
			OutputDimensionality: &dims,
		}
	}
	ctx := schemas.NewBifrostContext(nil, schemas.NoDeadline)

	batch, err := (&GeminiBatchEmbeddingRequest{
		Model:    "openai/text-embedding-3-small",
		Requests: []GeminiEmbeddingRequest{entry("one"), entry("two")},
	}).ToBifrostEmbeddingRequest(ctx)
	require.NoError(t, err)

	embedContent := entry("one")
	embedContent.Model = "vertex/text-embedding-005"
	single, err := embedContent.ToBifrostEmbeddingRequest(ctx)
	require.NoError(t, err)

	for _, req := range []*schemas.BifrostEmbeddingRequest{batch, single} {
		require.NoError(t, schemas.EmbeddingInput(req.Input).RejectPerItemParams())
		require.NotNil(t, req.Params)
		require.Equal(t, dims, *req.Params.Dimensions)
		require.Equal(t, taskType, *req.Params.TaskType)
	}
}

// Entries with genuinely different params stay per-item whatever the model string says: the
// converter runs before routing, so a bare or non-Gemini model may still be served by Gemini.
func TestGeminiBatchEmbeddingRequestKeepsDifferingParamsPerItem(t *testing.T) {
	query := "RETRIEVAL_QUERY"
	doc := "RETRIEVAL_DOCUMENT"
	for _, model := range []string{"gemini/gemini-embedding-001", "gemini-embedding-001", "openai/text-embedding-3-small"} {
		t.Run(model, func(t *testing.T) {
			req, err := (&GeminiBatchEmbeddingRequest{
				Model: model,
				Requests: []GeminiEmbeddingRequest{
					{Content: &Content{Parts: []*Part{{Text: "q"}}}, TaskType: &query},
					{Content: &Content{Parts: []*Part{{Text: "d"}}}, TaskType: &doc},
				},
			}).ToBifrostEmbeddingRequest(schemas.NewBifrostContext(nil, schemas.NoDeadline))
			require.NoError(t, err)

			require.Nil(t, req.Params)
			require.Equal(t, query, *req.Input[0].Params.TaskType)
			require.Equal(t, doc, *req.Input[1].Params.TaskType)
		})
	}
}

// documentOcr / audioTrackExtraction are only accepted inside embedContentConfig on each
// requests[] entry; a nested config must land there, never at the batch top level.
func TestToGeminiEmbeddingRequestPutsEmbedContentConfigOnEachEntry(t *testing.T) {
	text := "alpha"
	req, err := ToGeminiEmbeddingRequest(&schemas.BifrostEmbeddingRequest{
		Model: "gemini-embedding-2",
		Input: []schemas.EmbeddingInputItem{{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &text}}}},
		Params: &schemas.EmbeddingParameters{ExtraParams: map[string]interface{}{
			"embedContentConfig": map[string]interface{}{"documentOcr": true, "audioTrackExtraction": true},
		}},
	})
	require.NoError(t, err)
	body, err := providerUtils.MarshalSorted(req)
	require.NoError(t, err)
	merged, err := providerUtils.MergeExtraParamsIntoJSON(body, req.ExtraParams)
	require.NoError(t, err)

	// Without passthrough only the struct is sent; with it the extras are merged on top.
	for _, wire := range [][]byte{body, merged} {
		require.True(t, gjson.GetBytes(wire, "requests.0.embedContentConfig.documentOcr").Bool(), string(wire))
		require.True(t, gjson.GetBytes(wire, "requests.0.embedContentConfig.audioTrackExtraction").Bool(), string(wire))
		require.False(t, gjson.GetBytes(wire, "embedContentConfig").Exists(), string(wire))
	}
}

// The GenAI integration accepts embedContentConfig on each entry and forwards it in place.
func TestGeminiBatchEmbeddingRequestReadsEmbedContentConfig(t *testing.T) {
	var batch GeminiBatchEmbeddingRequest
	require.NoError(t, schemas.Unmarshal([]byte(`{"requests":[{"content":{"parts":[{"text":"a"}]},"embedContentConfig":{"documentOcr":true}}]}`), &batch))
	batch.Model = "gemini/gemini-embedding-2"
	bifrostReq, err := batch.ToBifrostEmbeddingRequest(schemas.NewBifrostContext(nil, schemas.NoDeadline))
	require.NoError(t, err)

	out, err := ToGeminiEmbeddingRequest(bifrostReq)
	require.NoError(t, err)
	body, err := providerUtils.MarshalSorted(out)
	require.NoError(t, err)
	require.True(t, gjson.GetBytes(body, "requests.0.embedContentConfig.documentOcr").Bool(), string(body))
}

func benchGeminiEmbeddingRequest() *schemas.BifrostEmbeddingRequest {
	return &schemas.BifrostEmbeddingRequest{
		Provider: schemas.Gemini,
		Model:    "gemini-embedding-001",
		Input: []schemas.EmbeddingInputItem{
			{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: schemas.Ptr("alpha kestrel harbor")}}},
		},
		Params: &schemas.EmbeddingParameters{TaskType: schemas.Ptr("RETRIEVAL_QUERY")},
	}
}

// Request path cost of the Gemini embedding conversion (TXE-6636).
func BenchmarkToGeminiEmbeddingRequest(b *testing.B) {
	req := benchGeminiEmbeddingRequest()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ToGeminiEmbeddingRequest(req); err != nil {
			b.Fatal(err)
		}
	}
}

// Response path cost of reading usage from what Google reports (TXE-6636): Vertex
// :embedContent's shape, with per-embedding statistics and a billable character count.
func BenchmarkGeminiEmbeddingResponseToBifrost(b *testing.B) {
	var resp GeminiEmbeddingResponse
	if err := schemas.Unmarshal([]byte(`{"embeddings":[{"values":[0.1,0.2,0.3],"statistics":{"tokenCount":5}}],"metadata":{"billableCharacterCount":18}}`), &resp); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if out := ToBifrostEmbeddingResponse(&resp, "gemini-embedding-001"); out == nil {
			b.Fatal("nil response")
		}
	}
}

// Native /genai response cost: Gemini's shape rebuilt from unified usage (TXE-6636).
func BenchmarkToGeminiEmbeddingResponse(b *testing.B) {
	chars := 18
	resp := &schemas.BifrostEmbeddingResponse{
		Data:  []schemas.EmbeddingData{{Index: 0, Object: "embedding", Embedding: schemas.EmbeddingStruct{EmbeddingArray: []float64{0.1, 0.2, 0.3}}}},
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 5, TotalTokens: 5, BilledCharacters: &chars},
	}
	b.ReportAllocs()
	for b.Loop() {
		if out := ToGeminiEmbeddingResponse(resp); out == nil {
			b.Fatal("nil response")
		}
	}
}

// Request path cost of the encoding_format check (TXE-6636), run before every Gemini and
// Vertex embedding conversion; there is no "before", the check is new.
func BenchmarkRejectNonFloatEncodingFormat(b *testing.B) {
	req := benchGeminiEmbeddingRequest()
	b.ReportAllocs()
	for b.Loop() {
		if err := RejectNonFloatEncodingFormat(req); err != nil {
			b.Fatal(err)
		}
	}
}

func encodingFormatRequest(requestLevel, itemLevel *string) *schemas.BifrostEmbeddingRequest {
	item := schemas.EmbeddingInputItem{
		Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: schemas.Ptr("hi")}},
	}
	if itemLevel != nil {
		item.Params = &schemas.EmbeddingParameters{EncodingFormat: itemLevel}
	}
	req := &schemas.BifrostEmbeddingRequest{
		Provider: schemas.Gemini,
		Model:    "gemini-embedding-001",
		Input:    []schemas.EmbeddingInputItem{item},
	}
	if requestLevel != nil {
		req.Params = &schemas.EmbeddingParameters{EncodingFormat: requestLevel}
	}
	return req
}

// Google's embedding endpoints return float vectors only and take no output-format
// parameter, so encoding_format base64 (or any other non-float value), on the request or
// on one input item, is 400 caller_unsupported_parameter on encoding_format, and Gemini is
// never called (contract C4). Before, the field was dropped and float vectors came back.
func TestGeminiEmbeddingRefusesNonFloatEncodingFormat(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"embeddings":[{"values":[0.1,0.2]}]}`))
	}))
	defer ts.Close()

	provider := NewGeminiProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: ts.URL},
	}, testNoopLogger{})
	key := schemas.Key{Value: *schemas.NewSecretVar("dummy-key")}

	cases := map[string]*schemas.BifrostEmbeddingRequest{
		"request base64": encodingFormatRequest(schemas.Ptr(schemas.EmbeddingEncodingBase64), nil),
		"request int8":   encodingFormatRequest(schemas.Ptr(schemas.EmbeddingEncodingInt8), nil),
		"item base64":    encodingFormatRequest(nil, schemas.Ptr(schemas.EmbeddingEncodingBase64)),
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			_, bifrostErr := provider.Embedding(ctx, key, req)
			require.NotNil(t, bifrostErr)
			require.NotNil(t, bifrostErr.StatusCode)
			require.Equal(t, 400, *bifrostErr.StatusCode)
			require.Equal(t, schemas.ErrorTypeCallerUnsupportedParameter, bifrostErr.ExtraFields.ErrorType)
			require.NotNil(t, bifrostErr.Error)
			require.Equal(t, "encoding_format", bifrostErr.Error.Param)
		})
	}
	require.Zero(t, calls.Load(), "a refused request must not reach Gemini")

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, bifrostErr := provider.Embedding(ctx, key, encodingFormatRequest(schemas.Ptr(schemas.EmbeddingEncodingFloat), nil))
	require.Nil(t, bifrostErr, "float is what Google returns and is accepted")
	require.Equal(t, int32(1), calls.Load())
}

func TestRejectNonFloatEncodingFormatIsABadRequest(t *testing.T) {
	require.NoError(t, RejectNonFloatEncodingFormat(encodingFormatRequest(nil, nil)))
	require.NoError(t, RejectNonFloatEncodingFormat(encodingFormatRequest(schemas.Ptr(schemas.EmbeddingEncodingFloat), nil)))
	err := RejectNonFloatEncodingFormat(encodingFormatRequest(schemas.Ptr(schemas.EmbeddingEncodingBase64), nil))
	bifrostErr, ok := providerUtils.AsBifrostBadRequestError(err)
	require.True(t, ok)
	require.Equal(t, "encoding_format", bifrostErr.Error.Param)
}
