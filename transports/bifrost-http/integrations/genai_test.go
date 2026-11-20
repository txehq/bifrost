package integrations

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/providers/gemini"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/providers/vertex"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/valyala/fasthttp"
)

// TestRewriteGenAIRawRequestBodyRedactsOnlyContentFields verifies Gemini redaction covers native text and function-result values without touching metadata or function-call arguments.
func TestRewriteGenAIRawRequestBodyRedactsOnlyContentFields(t *testing.T) {
	rawBody := []byte(`{
		"systemInstruction":{"parts":[{"text":"system alice@example.com"}]},
		"contents":[
			{"role":"user","parts":[{"text":"first alice@example.com"}]},
			{"role":"model","parts":[
				{"thought":true,"text":"reason alice@example.com","thoughtSignature":"alice@example.com"},
				{"functionCall":{"name":"lookup","args":{"email":"alice@example.com"}}}
			]},
			{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"output":"tool alice@example.com","nested.value":{"contact:key":"alice@example.com"}}}}]}
		],
		"instances":[{"prompt":"image alice@example.com"}],
		"labels":{"owner":"alice@example.com"},
		"tools":[{"functionDeclarations":[{"name":"lookup","description":"alice@example.com"}]}]
	}`)

	got, err := rewriteGenAIRawRequestBody(rawBody, map[string]string{"alice@example.com": "[EMAIL]"})
	require.NoError(t, err)

	redactedPaths := []string{
		"systemInstruction.parts.0.text",
		"contents.0.parts.0.text",
		"contents.1.parts.0.text",
		"contents.2.parts.0.functionResponse.response.output",
		`contents.2.parts.0.functionResponse.response.nested\.value.contact:key`,
		"instances.0.prompt",
	}
	for _, path := range redactedPaths {
		value := gjson.GetBytes(got, path).String()
		assert.NotContains(t, value, "alice@example.com", path)
		assert.Contains(t, value, "[EMAIL]", path)
	}

	untouchedPaths := []string{
		"contents.1.parts.0.thoughtSignature",
		"contents.1.parts.1.functionCall.args.email",
		"labels.owner",
		"tools.0.functionDeclarations.0.description",
	}
	for _, path := range untouchedPaths {
		assert.Equal(t, "alice@example.com", gjson.GetBytes(got, path).String(), path)
	}
	assert.True(t, strings.Contains(string(got), `"labels":{"owner":"alice@example.com"}`))
}

// TestRewriteGenAIRawRequestBodySupportsCountTokensEnvelope verifies both documented envelope spelling and snake-case system instructions use the same content allowlist.
func TestRewriteGenAIRawRequestBodySupportsCountTokensEnvelope(t *testing.T) {
	rawBody := []byte(`{"generate_content_request":{"system_instruction":{"parts":[{"text":"system alice@example.com"}]},"contents":[{"parts":[{"text":"user alice@example.com"}]}]}}`)

	got, err := rewriteGenAIRawRequestBody(rawBody, map[string]string{"alice@example.com": "[EMAIL]"})
	require.NoError(t, err)
	assert.Equal(t, "system [EMAIL]", gjson.GetBytes(got, "generate_content_request.system_instruction.parts.0.text").String())
	assert.Equal(t, "user [EMAIL]", gjson.GetBytes(got, "generate_content_request.contents.0.parts.0.text").String())
}

// TestRewriteGenAIRawRequestBodyRejectsUnmappedLiteral verifies a normalized runtime mutation cannot silently leave Gemini content unredacted.
func TestRewriteGenAIRawRequestBodyRejectsUnmappedLiteral(t *testing.T) {
	_, err := rewriteGenAIRawRequestBody(
		[]byte(`{"contents":[{"parts":[{"text":"hello"}]}]}`),
		map[string]string{"alice@example.com": "[EMAIL]"},
	)
	require.Error(t, err)
}

// TestRewriteGenAIRawRequestBodyRejectsSensitiveObjectKeys verifies unsupported key mutation fails closed inside a function-result subtree.
func TestRewriteGenAIRawRequestBodyRejectsSensitiveObjectKeys(t *testing.T) {
	_, err := rewriteGenAIRawRequestBody(
		[]byte(`{"contents":[{"parts":[{"functionResponse":{"response":{"alice@example.com":"value"}}}]}]}`),
		map[string]string{"alice@example.com": "[EMAIL]"},
	)
	require.ErrorContains(t, err, "unsupported JSON object key")
}

func TestCreateGenAIRerankRouteConfig(t *testing.T) {
	route := createGenAIRerankRouteConfig("/genai")

	assert.Equal(t, "/genai/v1/rank", route.Path)
	assert.Equal(t, "POST", route.Method)
	assert.Equal(t, RouteConfigTypeGenAI, route.Type)
	assert.NotNil(t, route.GetHTTPRequestType)
	assert.Equal(t, schemas.RerankRequest, route.GetHTTPRequestType(nil))
	assert.NotNil(t, route.GetRequestTypeInstance)
	assert.NotNil(t, route.RequestConverter)
	assert.NotNil(t, route.RerankResponseConverter)
	assert.NotNil(t, route.ErrorConverter)
	// The route resolves x-model-provider so it can be served cross-provider.
	assert.NotNil(t, route.PreCallback)

	// Verify request instance type
	reqInstance := route.GetRequestTypeInstance(context.Background())
	_, ok := reqInstance.(*vertex.VertexRankRequest)
	assert.True(t, ok, "GetRequestTypeInstance should return *vertex.VertexRankRequest")
}

func TestCreateGenAIRouteConfigsIncludesRerank(t *testing.T) {
	routes := CreateGenAIRouteConfigs("/genai")

	found := false
	for _, route := range routes {
		if route.Path == "/genai/v1/rank" && route.Method == "POST" {
			found = true
			break
		}
	}
	assert.True(t, found, "expected rerank route in genai route configs")
}

func findGenAIRouteForTest(t *testing.T, routes []RouteConfig, path, method string) RouteConfig {
	t.Helper()
	for _, route := range routes {
		if route.Path == path && route.Method == method {
			return route
		}
	}
	t.Fatalf("route %s %s not found", method, path)
	return RouteConfig{}
}

func TestGenAISpeechStreamResponseConverter(t *testing.T) {
	route := findGenAIRouteForTest(t, CreateGenAIRouteConfigs("/genai"), "/genai/v1beta/models/{model:*}", "POST")
	require.NotNil(t, route.StreamConfig)
	require.NotNil(t, route.StreamConfig.SpeechStreamResponseConverter)

	audio := []byte{0x01, 0x02, 0x03, 0x04}
	_, converted, err := route.StreamConfig.SpeechStreamResponseConverter(nil, &schemas.BifrostSpeechStreamResponse{
		Type:  schemas.SpeechStreamResponseTypeDelta,
		Audio: audio,
		ExtraFields: schemas.BifrostResponseExtraFields{
			RoutingInfo: schemas.RoutingInfo{Provider: schemas.Gemini},
		},
	})
	require.NoError(t, err)

	response, ok := converted.(*gemini.GenerateContentResponse)
	require.True(t, ok)
	require.Len(t, response.Candidates, 1)
	require.NotNil(t, response.Candidates[0].Content)
	require.Len(t, response.Candidates[0].Content.Parts, 1)
	require.NotNil(t, response.Candidates[0].Content.Parts[0].InlineData)
	assert.Equal(t, base64.StdEncoding.EncodeToString(audio), response.Candidates[0].Content.Parts[0].InlineData.Data)
	assert.Equal(t, "audio/L16;codec=pcm;rate=24000", response.Candidates[0].Content.Parts[0].InlineData.MIMEType)
}

func TestGenAISpeechStreamDoneResponseIncludesUsageAndFinishReason(t *testing.T) {
	route := findGenAIRouteForTest(t, CreateGenAIRouteConfigs("/genai"), "/genai/v1beta/models/{model:*}", "POST")
	_, converted, err := route.StreamConfig.SpeechStreamResponseConverter(nil, &schemas.BifrostSpeechStreamResponse{
		Type: schemas.SpeechStreamResponseTypeDone,
		Usage: &schemas.SpeechUsage{
			InputTokens:  3,
			OutputTokens: 5,
			TotalTokens:  8,
		},
	})
	require.NoError(t, err)

	response := converted.(*gemini.GenerateContentResponse)
	require.Len(t, response.Candidates, 1)
	assert.Equal(t, gemini.FinishReasonStop, response.Candidates[0].FinishReason)
	require.NotNil(t, response.UsageMetadata)
	assert.Equal(t, int32(3), response.UsageMetadata.PromptTokenCount)
	assert.Equal(t, int32(5), response.UsageMetadata.CandidatesTokenCount)
	assert.Equal(t, int32(8), response.UsageMetadata.TotalTokenCount)
}

func TestGenAICamelCaseSpeechVoiceSurvivesConversion(t *testing.T) {
	rawBody := []byte(`{
		"contents":[{"role":"user","parts":[{"text":"hello"}]}],
		"generationConfig":{
			"responseModalities":["AUDIO"],
			"speechConfig":{"voiceConfig":{"prebuiltVoiceConfig":{"voiceName":"alloy"}}}
		}
	}`)
	requestCtx := &fasthttp.RequestCtx{}
	requestCtx.SetUserValue("model", "gpt-4o-mini-tts:streamGenerateContent")
	requestCtx.Request.Header.Set("x-model-provider", "openai")
	requestCtx.Request.SetBody(rawBody)
	bifrostCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	geminiReq := &gemini.GeminiGenerationRequest{}
	require.NoError(t, sonic.Unmarshal(rawBody, geminiReq))
	require.NoError(t, extractAndSetModelAndRequestType(requestCtx, bifrostCtx, geminiReq))

	route := findGenAIRouteForTest(t, CreateGenAIRouteConfigs("/genai"), "/genai/v1beta/models/{model:*}", "POST")
	converted, err := route.RequestConverter(bifrostCtx, geminiReq)
	require.NoError(t, err)
	require.NotNil(t, converted.SpeechRequest)
	require.NotNil(t, converted.SpeechRequest.Params)
	require.NotNil(t, converted.SpeechRequest.Params.VoiceConfig)
	require.NotNil(t, converted.SpeechRequest.Params.VoiceConfig.Voice)
	assert.Equal(t, "alloy", *converted.SpeechRequest.Params.VoiceConfig.Voice)
}

// TXE-6539: on the native route the speech output-token bound is the request's own
// generationConfig.maxOutputTokens. It becomes the unified max_output_tokens and is sent back
// to Gemini as generationConfig.maxOutputTokens; it used to be dropped on the way in.
func TestGenAISpeechMaxOutputTokensSurvivesConversion(t *testing.T) {
	rawBody := []byte(`{
		"contents":[{"role":"user","parts":[{"text":"hello"}]}],
		"generationConfig":{
			"responseModalities":["AUDIO"],
			"maxOutputTokens":2048,
			"speechConfig":{"voiceConfig":{"prebuiltVoiceConfig":{"voiceName":"Kore"}}}
		}
	}`)
	requestCtx := &fasthttp.RequestCtx{}
	requestCtx.SetUserValue("model", "gemini-3.8-flash-tts:generateContent")
	requestCtx.Request.Header.Set("x-model-provider", "gemini")
	requestCtx.Request.SetBody(rawBody)
	bifrostCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	geminiReq := &gemini.GeminiGenerationRequest{}
	require.NoError(t, sonic.Unmarshal(rawBody, geminiReq))
	require.NoError(t, extractAndSetModelAndRequestType(requestCtx, bifrostCtx, geminiReq))

	route := findGenAIRouteForTest(t, CreateGenAIRouteConfigs("/genai"), "/genai/v1beta/models/{model:*}", "POST")
	converted, err := route.RequestConverter(bifrostCtx, geminiReq)
	require.NoError(t, err)
	require.NotNil(t, converted.SpeechRequest)
	require.NotNil(t, converted.SpeechRequest.Params)
	require.NotNil(t, converted.SpeechRequest.Params.MaxOutputTokens)
	assert.Equal(t, 2048, *converted.SpeechRequest.Params.MaxOutputTokens)

	// The native request sets only fields the Gemini speech conversion delivers.
	assert.Subset(t, gemini.SpeechUnifiedFields, (&schemas.BifrostRequest{SpeechRequest: converted.SpeechRequest}).UnifiedFieldsPresent())

	upstream, err := gemini.ToGeminiSpeechRequest(converted.SpeechRequest)
	require.NoError(t, err)
	assert.EqualValues(t, 2048, upstream.GenerationConfig.MaxOutputTokens)
}

// TXE-6539: the unified speech route answers WAV when no format is named, but a native GenAI
// client reads the inlineData as the bare PCM Gemini answers. A native speech request that goes
// to Gemini or Vertex therefore names pcm, and its response is labelled as that PCM; a request
// routed to another provider keeps that provider's default.
func TestGenAISpeechNativeRouteKeepsPCM(t *testing.T) {
	rawBody := []byte(`{
		"contents":[{"role":"user","parts":[{"text":"hello"}]}],
		"generationConfig":{"responseModalities":["AUDIO"],"speechConfig":{"voiceConfig":{"prebuiltVoiceConfig":{"voiceName":"Kore"}}}}
	}`)
	route := findGenAIRouteForTest(t, CreateGenAIRouteConfigs("/genai"), "/genai/v1beta/models/{model:*}", "POST")
	convert := func(model, providerHeader string) *schemas.BifrostSpeechRequest {
		requestCtx := &fasthttp.RequestCtx{}
		requestCtx.SetUserValue("model", model)
		if providerHeader != "" {
			requestCtx.Request.Header.Set("x-model-provider", providerHeader)
		}
		requestCtx.Request.SetBody(rawBody)
		bifrostCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		geminiReq := &gemini.GeminiGenerationRequest{}
		require.NoError(t, sonic.Unmarshal(rawBody, geminiReq))
		require.NoError(t, extractAndSetModelAndRequestType(requestCtx, bifrostCtx, geminiReq))
		converted, err := route.RequestConverter(bifrostCtx, geminiReq)
		require.NoError(t, err)
		require.NotNil(t, converted.SpeechRequest)
		require.NotNil(t, converted.SpeechRequest.Params)
		return converted.SpeechRequest
	}

	assert.Equal(t, "pcm", convert("gemini-3.8-flash-tts:generateContent", "").Params.ResponseFormat, "a bare model defaults to Gemini")
	assert.Equal(t, "pcm", convert("gemini-3.8-flash-tts:generateContent", "gemini").Params.ResponseFormat)
	assert.Equal(t, "pcm", convert("vertex/gemini-3.8-flash-tts:generateContent", "").Params.ResponseFormat)
	assert.Equal(t, "", convert("gpt-4o-mini-tts:generateContent", "openai").Params.ResponseFormat, "another provider keeps its own default")

	pcm := []byte{0, 1, 2, 3, 4, 5}
	geminiResp := &schemas.BifrostSpeechResponse{Audio: pcm}
	geminiResp.ExtraFields.RoutingInfo.Provider = schemas.Gemini
	converted, err := route.SpeechResponseConverter(nil, geminiResp)
	require.NoError(t, err)
	blob := converted.(*gemini.GenerateContentResponse).Candidates[0].Content.Parts[0].InlineData
	assert.Equal(t, "audio/L16;codec=pcm;rate=24000", blob.MIMEType)
	assert.Equal(t, "AAECAwQF", blob.Data, "the PCM is returned as Gemini sent it, with no WAV header")
}

// TXE-6539: the native route's done chunk carries the speech usage's output token details back
// as candidatesTokensDetails and thoughtsTokenCount.
func TestGenAISpeechStreamDoneResponseIncludesOutputTokenDetails(t *testing.T) {
	route := findGenAIRouteForTest(t, CreateGenAIRouteConfigs("/genai"), "/genai/v1beta/models/{model:*}", "POST")
	_, converted, err := route.StreamConfig.SpeechStreamResponseConverter(nil, &schemas.BifrostSpeechStreamResponse{
		Type: schemas.SpeechStreamResponseTypeDone,
		Usage: &schemas.SpeechUsage{
			InputTokens:        7,
			OutputTokens:       50,
			TotalTokens:        69,
			OutputTokenDetails: &schemas.SpeechUsageOutputTokenDetails{AudioTokens: 50, ReasoningTokens: 12},
		},
	})
	require.NoError(t, err)

	response := converted.(*gemini.GenerateContentResponse)
	require.NotNil(t, response.UsageMetadata)
	assert.Equal(t, int32(50), response.UsageMetadata.CandidatesTokenCount)
	assert.Equal(t, int32(12), response.UsageMetadata.ThoughtsTokenCount)
	require.Len(t, response.UsageMetadata.CandidatesTokensDetails, 1)
	assert.Equal(t, gemini.ModalityAudio, response.UsageMetadata.CandidatesTokensDetails[0].Modality)
	assert.Equal(t, int32(50), response.UsageMetadata.CandidatesTokensDetails[0].TokenCount)
}

func TestExtractAndSetModelAndRequestTypePreservesRawBodyForGenerateContent(t *testing.T) {
	rawBody := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generationConfig":{"responseJsonSchema":{"type":"object","properties":{"b":{"type":"string"},"a":{"type":"string"}}}}}`)
	ctx := &fasthttp.RequestCtx{}
	ctx.SetUserValue("model", "gemini-2.5-flash:generateContent")
	ctx.Request.Header.SetMethod("POST")
	ctx.Request.Header.Set("x-model-provider", "gemini")
	ctx.Request.SetBody(rawBody)

	req := &gemini.GeminiGenerationRequest{}
	require.NoError(t, sonic.Unmarshal(rawBody, req))
	bifrostCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	err := extractAndSetModelAndRequestType(ctx, bifrostCtx, req)
	require.NoError(t, err)

	assert.Equal(t, true, bifrostCtx.Value(schemas.BifrostContextKeyUseRawRequestBody))
	assert.Equal(t, rawBody, bifrostCtx.Value(genAIRawRequestBodyContextKey))
	_, hasRewriter := bifrostCtx.Value(schemas.BifrostContextKeyRawRequestBodyTextRewriter).(schemas.RawRequestBodyTextRewriter)
	assert.True(t, hasRewriter)
}

func TestExtractAndSetModelAndRequestTypeNoRawPassthroughWithoutExplicitGemini(t *testing.T) {
	// A bare model with no gemini/ prefix and no x-model-provider header may
	// resolve to Vertex (or another provider) downstream, so the raw-body
	// passthrough must not engage on the silent Gemini default.
	rawBody := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	ctx := &fasthttp.RequestCtx{}
	ctx.SetUserValue("model", "gemini-2.5-flash:generateContent")
	ctx.Request.Header.SetMethod("POST")
	ctx.Request.SetBody(rawBody)

	req := &gemini.GeminiGenerationRequest{}
	require.NoError(t, sonic.Unmarshal(rawBody, req))
	bifrostCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	err := extractAndSetModelAndRequestType(ctx, bifrostCtx, req)
	require.NoError(t, err)

	assert.Nil(t, bifrostCtx.Value(schemas.BifrostContextKeyUseRawRequestBody))
	assert.Nil(t, bifrostCtx.Value(genAIRawRequestBodyContextKey))
	assert.Nil(t, bifrostCtx.Value(schemas.BifrostContextKeyRawRequestBodyTextRewriter))
}

func TestExtractAndSetModelAndRequestTypeDoesNotRawPassthroughEmbedding(t *testing.T) {
	rawBody := []byte(`{"content":{"parts":[{"text":"hello"}]}}`)
	ctx := &fasthttp.RequestCtx{}
	ctx.SetUserValue("model", "gemini-embedding-001:embedContent")
	ctx.Request.Header.SetMethod("POST")
	ctx.Request.SetBody(rawBody)

	req := &gemini.GeminiEmbeddingRequest{}
	require.NoError(t, sonic.Unmarshal(rawBody, req))
	bifrostCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	err := extractAndSetModelAndRequestType(ctx, bifrostCtx, req)
	require.NoError(t, err)

	assert.Nil(t, bifrostCtx.Value(schemas.BifrostContextKeyUseRawRequestBody))
	assert.Nil(t, bifrostCtx.Value(genAIRawRequestBodyContextKey))
}

func TestExtractAndSetModelAndRequestTypeDetectsImageEditAtAnyPartPosition(t *testing.T) {
	textPart := `{"text":"change only the red area to black"}`
	imagePart := `{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="}}`
	cases := map[string]string{
		"image first": imagePart + "," + textPart,
		"text first":  textPart + "," + imagePart,
	}

	for name, parts := range cases {
		t.Run(name, func(t *testing.T) {
			rawBody := []byte(`{"contents":[{"role":"user","parts":[` + parts + `]}],"generationConfig":{"responseModalities":["IMAGE"]}}`)
			ctx := &fasthttp.RequestCtx{}
			ctx.SetUserValue("model", "vertex/gemini-3-pro-image:generateContent")
			ctx.Request.Header.SetMethod("POST")
			ctx.Request.SetBody(rawBody)

			_, reqType := extractModelAndRequestType(ctx)
			assert.Equal(t, schemas.ImageEditRequest, reqType)

			req := &gemini.GeminiGenerationRequest{}
			require.NoError(t, sonic.Unmarshal(rawBody, req))
			bifrostCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

			require.NoError(t, extractAndSetModelAndRequestType(ctx, bifrostCtx, req))
			assert.True(t, req.IsImageEdit)
			assert.False(t, req.IsImageGeneration)

			editReq := req.ToBifrostImageEditRequest(bifrostCtx)
			require.NotNil(t, editReq)
			assert.Equal(t, "change only the red area to black", editReq.Input.Prompt)
			assert.Len(t, editReq.Input.Images, 1)
		})
	}
}

func TestGenAIBatchCreateConverterCarriesRawBody(t *testing.T) {
	rawBody := []byte(`{"batch":{"inputConfig":{"requests":{"requests":[{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generationConfig":{"temperature":0.2}},"metadata":{"key":"req-1"}}]}}}}`)
	ctx := &fasthttp.RequestCtx{}
	ctx.SetUserValue("model", "gemini-2.5-flash:batchGenerateContent")
	ctx.Request.Header.SetMethod("POST")
	ctx.Request.Header.Set("x-model-provider", "gemini")
	ctx.Request.SetBody(rawBody)

	req := &gemini.GeminiBatchCreateRequest{}
	require.NoError(t, sonic.Unmarshal(rawBody, req))
	bifrostCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	require.NoError(t, extractAndSetModelAndRequestType(ctx, bifrostCtx, req))

	route := findGenAIRouteForTest(t, CreateGenAIRouteConfigs("/genai"), "/genai/v1beta/models/{model:*}", "POST")
	batchReq, err := route.BatchRequestConverter(bifrostCtx, req)
	require.NoError(t, err)
	require.NotNil(t, batchReq)
	require.NotNil(t, batchReq.CreateRequest)

	assert.Equal(t, rawBody, batchReq.CreateRequest.RawRequestBody)
	assert.Equal(t, true, bifrostCtx.Value(schemas.BifrostContextKeyUseRawRequestBody))
}

// TestGenAIBatchCreateConverterCarriesDisplayNameAndInlineTools verifies the typed batch
// create path (used when the request is not an explicit-gemini raw passthrough) carries the
// batch display name and every modeled inline-request field (tools, cachedContent) through.
func TestGenAIBatchCreateConverterCarriesDisplayNameAndInlineTools(t *testing.T) {
	rawBody := []byte(`{"batch":{"displayName":"my-batch","inputConfig":{"requests":{"requests":[{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"get_weather"}]}],"cachedContent":"cachedContents/abc"},"metadata":{"key":"req-1"}}]}}}}`)
	ctx := &fasthttp.RequestCtx{}
	ctx.SetUserValue("model", "gemini-2.5-flash:batchGenerateContent")
	ctx.Request.Header.SetMethod("POST")
	// No x-model-provider header, so the typed path (not raw passthrough) is exercised.
	ctx.Request.SetBody(rawBody)

	req := &gemini.GeminiBatchCreateRequest{}
	require.NoError(t, sonic.Unmarshal(rawBody, req))
	bifrostCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	require.NoError(t, extractAndSetModelAndRequestType(ctx, bifrostCtx, req))

	route := findGenAIRouteForTest(t, CreateGenAIRouteConfigs("/genai"), "/genai/v1beta/models/{model:*}", "POST")
	batchReq, err := route.BatchRequestConverter(bifrostCtx, req)
	require.NoError(t, err)
	require.NotNil(t, batchReq.CreateRequest)

	// #2: batch display name carried through.
	require.NotNil(t, batchReq.CreateRequest.DisplayName)
	assert.Equal(t, "my-batch", *batchReq.CreateRequest.DisplayName)

	// #3: inline tools + cachedContent survive (not dropped by the converter).
	require.Len(t, batchReq.CreateRequest.Requests, 1)
	body := batchReq.CreateRequest.Requests[0].Body
	assert.Contains(t, body, "tools")
	assert.Equal(t, "cachedContents/abc", body["cachedContent"])
	assert.Equal(t, "req-1", batchReq.CreateRequest.Requests[0].CustomID)
}

func TestGenAICachedContentCreateParserRejectsNonStringScalars(t *testing.T) {
	rawBody := []byte(`{"model":123,"ttl":3600}`)
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("POST")
	ctx.Request.SetBody(rawBody)

	route := findGenAIRouteForTest(t, CreateGenAICachedContentRouteConfigs("/genai", nil), "/genai/v1beta/cachedContents", "POST")
	req := route.GetRequestTypeInstance(context.Background())

	err := route.RequestParser(ctx, req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "model must be a string")
}

func TestGenAICachedContentCreateParserCarriesRawBody(t *testing.T) {
	rawBody := []byte(`{"model":"models/gemini-2.5-flash","contents":[{"role":"user","parts":[{"text":"alpha"}]}],"tools":[{"functionDeclarations":[{"name":"lookup","parametersJsonSchema":{"type":"object","properties":{"z":{"type":"string"},"a":{"type":"string"}}}}]}],"ttl":"3600s"}`)
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("POST")
	ctx.Request.SetBody(rawBody)

	route := findGenAIRouteForTest(t, CreateGenAICachedContentRouteConfigs("/genai", nil), "/genai/v1beta/cachedContents", "POST")
	req := route.GetRequestTypeInstance(context.Background())
	require.NoError(t, route.RequestParser(ctx, req))

	createReq := req.(*schemas.BifrostCachedContentCreateRequest)
	assert.Equal(t, rawBody, createReq.RawRequestBody)
	assert.Equal(t, "gemini-2.5-flash", createReq.Model)
	require.NotNil(t, createReq.TTL)
	assert.Equal(t, "3600s", *createReq.TTL)

	bifrostCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	converted, err := route.CachedContentRequestConverter(bifrostCtx, req)
	require.NoError(t, err)
	require.NotNil(t, converted)
	assert.Equal(t, true, bifrostCtx.Value(schemas.BifrostContextKeyUseRawRequestBody))
}

func TestCreateGenAIRouteConfigsIncludesRerankForCompositePrefixes(t *testing.T) {
	prefixes := []string{"/litellm", "/langchain", "/pydanticai"}

	for _, prefix := range prefixes {
		routes := CreateGenAIRouteConfigs(prefix)
		found := false
		for _, route := range routes {
			if route.Path == prefix+"/v1/rank" && route.Method == "POST" {
				found = true
				break
			}
		}
		assert.Truef(t, found, "expected rerank route for prefix %s", prefix)
	}
}

func TestGenAIRerankRequestConverter(t *testing.T) {
	route := createGenAIRerankRouteConfig("/genai")
	require.NotNil(t, route.RequestConverter)

	model := "semantic-ranker-default@latest"
	topN := 2
	content1 := "Paris is capital of France"
	content2 := "Berlin is capital of Germany"
	req := &vertex.VertexRankRequest{
		Model: &model,
		Query: "capital of france",
		Records: []vertex.VertexRankRecord{
			{ID: "rec-1", Content: &content1},
			{ID: "rec-2", Content: &content2},
		},
		TopN: &topN,
	}

	bifrostCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	bifrostReq, err := route.RequestConverter(bifrostCtx, req)
	require.NoError(t, err)
	require.NotNil(t, bifrostReq)
	require.NotNil(t, bifrostReq.RerankRequest)
	// Provider resolution is deferred to the route header and the modelcatalogresolver plugin.
	assert.Equal(t, schemas.ModelProvider(""), bifrostReq.RerankRequest.Provider)
	assert.Equal(t, "semantic-ranker-default@latest", bifrostReq.RerankRequest.Model)
	assert.Equal(t, "capital of france", bifrostReq.RerankRequest.Query)
	require.Len(t, bifrostReq.RerankRequest.Documents, 2)
	assert.Equal(t, "Paris is capital of France", bifrostReq.RerankRequest.Documents[0].Text)
	assert.Equal(t, "Berlin is capital of Germany", bifrostReq.RerankRequest.Documents[1].Text)
	require.NotNil(t, bifrostReq.RerankRequest.Params)
	require.NotNil(t, bifrostReq.RerankRequest.Params.TopN)
	assert.Equal(t, 2, *bifrostReq.RerankRequest.Params.TopN)
}

func TestGenAIRerankResponseConverterRestoresCallerRecordIDs(t *testing.T) {
	route := createGenAIRerankRouteConfig("/genai")
	require.NotNil(t, route.RerankResponseConverter)

	resp := &schemas.BifrostRerankResponse{
		Results: []schemas.RerankResult{
			{Index: 1, RelevanceScore: 0.88, Document: &schemas.RerankDocument{
				ID: new("doc-paris"), Text: "Paris is capital of France", Meta: map[string]interface{}{"title": "Paris"},
			}},
			{Index: 0, RelevanceScore: 0.12, Document: &schemas.RerankDocument{
				ID: new("doc-berlin"), Text: "Berlin is capital of Germany",
			}},
		},
		ExtraFields: schemas.BifrostResponseExtraFields{
			Provider: schemas.Vertex,
			// Raw carries synthetic idx:N record IDs, so it must never be returned.
			RawResponse: map[string]interface{}{"records": []interface{}{map[string]interface{}{"id": "idx:1"}}},
		},
	}

	converted, err := route.RerankResponseConverter(nil, resp)
	require.NoError(t, err)

	rankResp, ok := converted.(*vertex.VertexRankResponse)
	require.True(t, ok, "converter should emit *vertex.VertexRankResponse")
	require.Len(t, rankResp.Records, 2)
	assert.Equal(t, "doc-paris", rankResp.Records[0].ID)
	assert.InDelta(t, 0.88, rankResp.Records[0].Score, 1e-9)
	require.NotNil(t, rankResp.Records[0].Content)
	assert.Equal(t, "Paris is capital of France", *rankResp.Records[0].Content)
	require.NotNil(t, rankResp.Records[0].Title)
	assert.Equal(t, "Paris", *rankResp.Records[0].Title)
	assert.Equal(t, "doc-berlin", rankResp.Records[1].ID)
	assert.Nil(t, rankResp.Records[1].Title)
}

func TestGenAIRerankRequestConverterRequestsDocuments(t *testing.T) {
	route := createGenAIRerankRouteConfig("/genai")
	require.NotNil(t, route.RequestConverter)

	bifrostCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	bifrostReq, err := route.RequestConverter(bifrostCtx, &vertex.VertexRankRequest{
		Query:   "capital of france",
		Records: []vertex.VertexRankRecord{{ID: "doc-paris", Content: new("Paris is capital of France")}},
	})
	require.NoError(t, err)
	require.NotNil(t, bifrostReq.RerankRequest)
	require.NotNil(t, bifrostReq.RerankRequest.Params)
	// Ranked records are keyed by caller record ID, which only the document carries.
	require.NotNil(t, bifrostReq.RerankRequest.Params.ReturnDocuments)
	assert.True(t, *bifrostReq.RerankRequest.Params.ReturnDocuments)
}

func TestCreateGenAIRouteConfigsIncludesModelMetadataRoute(t *testing.T) {
	routes := CreateGenAIRouteConfigs("/genai")

	found := false
	for _, route := range routes {
		if route.Path == "/genai/v1beta/models/{model}" && route.Method == "GET" {
			found = true
			assert.Equal(t, schemas.ListModelsRequest, route.GetHTTPRequestType(nil))
			require.NotNil(t, route.PreCallback)
			require.NotNil(t, route.ListModelsResponseConverter)
			break
		}
	}

	assert.True(t, found, "expected model metadata route in genai route configs")
}

func TestExtractGeminiModelMetadataParams(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	ctx.SetUserValue("model", "models/gemini-3-pro-preview")

	listReq := &schemas.BifrostListModelsRequest{}
	bifrostCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	err := extractGeminiModelMetadataParams(ctx, bifrostCtx, listReq)
	require.NoError(t, err)
	assert.Equal(t, schemas.Gemini, listReq.Provider)
	assert.Equal(t, "/models/gemini-3-pro-preview", bifrostCtx.Value(schemas.BifrostContextKeyURLPath))
	assert.Equal(t, "gemini-3-pro-preview", bifrostCtx.Value(requestedGeminiModelMetadataContextKey))
}

func TestConvertGeminiModelMetadataResponse(t *testing.T) {
	bifrostCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	bifrostCtx.SetValue(requestedGeminiModelMetadataContextKey, "gemini-2.5-pro")

	resp := &schemas.BifrostListModelsResponse{
		Data: []schemas.Model{{ID: "gemini/gemini-2.5-pro", Name: schemas.Ptr("Gemini 2.5 Pro")}},
	}

	converted, err := convertGeminiModelMetadataResponse(bifrostCtx, resp)
	require.NoError(t, err)

	model, ok := converted.(gemini.GeminiModel)
	require.True(t, ok, "expected gemini.GeminiModel")
	assert.Equal(t, "models/gemini-2.5-pro", model.Name)
	assert.Equal(t, "Gemini 2.5 Pro", model.DisplayName)
}

func TestConvertGeminiModelMetadataResponse_MatchesRequestedModelNotFirst(t *testing.T) {
	bifrostCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	bifrostCtx.SetValue(requestedGeminiModelMetadataContextKey, "gemini-3-pro-preview")

	resp := &schemas.BifrostListModelsResponse{
		Data: []schemas.Model{
			{ID: "gemini/gemini-1.5-pro", Name: schemas.Ptr("Gemini 1.5 Pro")},
			{ID: "gemini/gemini-3-pro-preview", Name: schemas.Ptr("Gemini 3 Pro Preview")},
		},
	}

	converted, err := convertGeminiModelMetadataResponse(bifrostCtx, resp)
	require.NoError(t, err)

	model, ok := converted.(gemini.GeminiModel)
	require.True(t, ok, "expected gemini.GeminiModel")
	assert.Equal(t, "models/gemini-3-pro-preview", model.Name)
	assert.Equal(t, "Gemini 3 Pro Preview", model.DisplayName)
}

func TestConvertGeminiModelMetadataResponse_EmptyReturnsMinimalModel(t *testing.T) {
	bifrostCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	bifrostCtx.SetValue(requestedGeminiModelMetadataContextKey, "gemini-3-pro-preview")

	converted, err := convertGeminiModelMetadataResponse(bifrostCtx, &schemas.BifrostListModelsResponse{Data: []schemas.Model{}})
	require.NoError(t, err)
	model, ok := converted.(gemini.GeminiModel)
	require.True(t, ok, "expected gemini.GeminiModel")
	assert.Equal(t, "models/gemini-3-pro-preview", model.Name)
}

// Both generation endpoints normalize the same native thinking config before
// governance can switch Gemini and Vertex. Exercise that conversion explicitly;
// native Vertex project/location URLs use the separate passthrough router.
func TestGenAIFlashLiteMinimalThinkingAfterRouting(t *testing.T) {
	route := findGenAIRouteForTest(t, CreateGenAIRouteConfigs("/genai"), "/genai/v1beta/models/{model:*}", "POST")
	for _, source := range []schemas.ModelProvider{schemas.Gemini, schemas.Vertex} {
		for _, target := range []schemas.ModelProvider{schemas.Gemini, schemas.Vertex} {
			for _, method := range []string{"generateContent", "streamGenerateContent"} {
				t.Run(string(source)+" to "+string(target)+"/"+method, func(t *testing.T) {
					body := []byte(`{"contents":[{"role":"user","parts":[{"text":"Say OK."}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"MINIMAL"}}}`)
					requestCtx := &fasthttp.RequestCtx{}
					requestCtx.SetUserValue("model", string(source)+"/gemini-3.1-flash-lite:"+method)
					requestCtx.Request.Header.SetMethod("POST")
					requestCtx.Request.SetBody(body)
					ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
					native := &gemini.GeminiGenerationRequest{}
					require.NoError(t, sonic.Unmarshal(body, native))
					require.NoError(t, route.PreCallback(requestCtx, ctx, native))
					assert.Equal(t, method == "streamGenerateContent", native.Stream)
					converted, err := route.RequestConverter(ctx, native)
					require.NoError(t, err)
					require.NotNil(t, converted.ResponsesRequest)
					normalized := converted.ResponsesRequest
					require.Equal(t, source, normalized.Provider)
					normalized.Provider = target
					// Inspect the normalized path even when explicit Gemini selection
					// also retained raw bytes; rerouting must not depend on that shortcut.
					out, err := gemini.ToGeminiResponsesRequest(ctx, normalized)
					require.NoError(t, err)
					require.NotNil(t, out.GenerationConfig.ThinkingConfig)
					require.NotNil(t, out.GenerationConfig.ThinkingConfig.ThinkingLevel)
					assert.Equal(t, "minimal", *out.GenerationConfig.ThinkingConfig.ThinkingLevel)
					assert.Nil(t, out.GenerationConfig.ThinkingConfig.ThinkingBudget)
				})
			}
		}
	}
}

func genAIEmbeddingRoute(t *testing.T) *RouteConfig {
	t.Helper()
	routes := CreateGenAIRouteConfigs("/genai")
	for i := range routes {
		if routes[i].Path == "/genai/v1beta/models/{model:*}" && routes[i].EmbeddingResponseConverter != nil {
			return &routes[i]
		}
	}
	t.Fatal("no /genai/v1beta/models embedding route")
	return nil
}

// /genai :batchEmbedContents and :embedContent answer in Gemini's shape rebuilt from unified
// usage (TXE-6636, contract C5). The native response repeats only what the vendor reported:
// metadata.billableCharacterCount is the billed character count (Vertex text models), never
// the token count standing in for one, and nothing is written when nothing was reported.
func TestGenAIEmbeddingRouteResponseRepeatsOnlyReportedUsage(t *testing.T) {
	route := genAIEmbeddingRoute(t)
	data := []schemas.EmbeddingData{{Index: 0, Object: "embedding", Embedding: schemas.EmbeddingStruct{EmbeddingArray: []float64{0.1}}}}
	convert := func(ctx *schemas.BifrostContext, usage *schemas.BifrostLLMUsage) []byte {
		out, err := route.EmbeddingResponseConverter(ctx, &schemas.BifrostEmbeddingResponse{Data: data, Usage: usage})
		require.NoError(t, err)
		body, err := sonic.Marshal(out)
		require.NoError(t, err)
		return body
	}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	body := convert(ctx, &schemas.BifrostLLMUsage{PromptTokens: 7, TotalTokens: 7, BilledCharacters: schemas.Ptr(20)})
	assert.Equal(t, int64(20), gjson.GetBytes(body, "metadata.billableCharacterCount").Int(), "%s", body)
	assert.Equal(t, int64(7), gjson.GetBytes(body, "embeddings.0.statistics.tokenCount").Int(), "%s", body)

	body = convert(ctx, &schemas.BifrostLLMUsage{PromptTokens: 7, TotalTokens: 7})
	assert.False(t, gjson.GetBytes(body, "metadata").Exists(), "tokens are not billable characters: %s", body)

	body = convert(ctx, &schemas.BifrostLLMUsage{})
	assert.False(t, gjson.GetBytes(body, "metadata").Exists(), "%s", body)
	assert.False(t, gjson.GetBytes(body, "embeddings.0.statistics").Exists(), "no count was reported: %s", body)

	embedCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	embedCtx.SetValue(isGeminiEmbedContentRequestContextKey, true)
	body = convert(embedCtx, &schemas.BifrostLLMUsage{})
	assert.False(t, gjson.GetBytes(body, "embedding.statistics").Exists(), "no count was reported: %s", body)
	body = convert(embedCtx, &schemas.BifrostLLMUsage{PromptTokens: 5, TotalTokens: 5})
	assert.Equal(t, int64(5), gjson.GetBytes(body, "embedding.statistics.tokenCount").Int(), "%s", body)
}

// OpenAI-shaped ingress to Google (TXE-6636): /openai/v1/embeddings, and the same route
// under alias prefixes such as /litellm, decodes encoding_format into the unified
// parameters, so base64 for a gemini or vertex model is refused with
// caller_unsupported_parameter; the unified response returns billed_characters as reported.
func TestOpenAIEmbeddingsRouteToGoogleRefusesBase64(t *testing.T) {
	for _, prefix := range []string{"/openai", "/litellm"} {
		var route *RouteConfig
		routes := CreateOpenAIRouteConfigs(prefix, nil)
		for i := range routes {
			if routes[i].Path == prefix+"/v1/embeddings" {
				route = &routes[i]
			}
		}
		require.NotNil(t, route, prefix)
		for _, model := range []string{"gemini/gemini-embedding-001", "vertex/text-embedding-005"} {
			t.Run(prefix+"/"+model, func(t *testing.T) {
				toBifrost := func(body string) *schemas.BifrostEmbeddingRequest {
					native := route.GetRequestTypeInstance(context.Background())
					require.NoError(t, sonic.Unmarshal([]byte(body), native))
					ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
					bifrostReq, err := route.RequestConverter(ctx, native)
					require.NoError(t, err)
					return bifrostReq.EmbeddingRequest
				}
				err := gemini.RejectNonFloatEncodingFormat(toBifrost(`{"model":"` + model + `","input":"hi","encoding_format":"base64"}`))
				bifrostErr, ok := providerUtils.AsBifrostBadRequestError(err)
				require.True(t, ok, "want a caller error, got %v", err)
				assert.Equal(t, schemas.ErrorTypeCallerUnsupportedParameter, bifrostErr.ExtraFields.ErrorType)
				assert.Equal(t, "encoding_format", bifrostErr.Error.Param)
				require.NoError(t, gemini.RejectNonFloatEncodingFormat(toBifrost(`{"model":"`+model+`","input":"hi","encoding_format":"float"}`)))
			})
		}
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		resp := &schemas.BifrostEmbeddingResponse{Usage: &schemas.BifrostLLMUsage{PromptTokens: 5, TotalTokens: 5, BilledCharacters: schemas.Ptr(18)}}
		resp.ExtraFields.Provider = schemas.Vertex
		out, err := route.EmbeddingResponseConverter(ctx, resp)
		require.NoError(t, err)
		body, err := sonic.Marshal(out)
		require.NoError(t, err)
		assert.Equal(t, int64(18), gjson.GetBytes(body, "usage.billed_characters").Int(), "%s", body)
		assert.Equal(t, int64(5), gjson.GetBytes(body, "usage.prompt_tokens").Int(), "%s", body)
	}
}

// TXE-6433: a speech answer that is a link to the audio (DashScope Qwen3-TTS) reaches a caller
// on a route whose response converter writes only audio bytes. GenAI's generateContent would
// encode the empty Audio as inline data and answer 200 with no audio; the router answers the
// link as JSON instead, with the vendor's url, expires_at and usage, as /v1/audio/speech does.
func TestGenAISpeechRouteAnswersDashScopeAudioLink(t *testing.T) {
	const audioLink = "https://dashscope-result-sgp.oss-ap-southeast-1.aliyuncs.com/1d/a.wav?Expires=1766113409&Signature=x"
	var sent string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sent = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"request_id":"5c63c65c","output":{"finish_reason":"stop","audio":{"data":"","url":"` + audioLink +
			`","id":"audio_5c63c65c","expires_at":1766113409}},"usage":{"input_tokens":0,"output_tokens":0,"characters":5}}`))
	}))
	defer upstream.Close()

	account := &nativeMessagesTestAccount{
		configs: map[schemas.ModelProvider]*schemas.ProviderConfig{
			schemas.DashScope: {NetworkConfig: schemas.NetworkConfig{BaseURL: upstream.URL, AllowPrivateNetwork: true}},
		},
		keys: map[schemas.ModelProvider][]schemas.Key{
			schemas.DashScope: {{ID: "dashscope-key", Value: schemas.SecretVar{Val: "dashscope-secret"}, Models: schemas.WhiteList{"*"}, Weight: 1}},
		},
	}
	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: account, Logger: bifrost.NewNoOpLogger(), InitialPoolSize: 1,
	})
	if err != nil {
		t.Fatalf("init Bifrost: %v", err)
	}
	defer client.Shutdown()

	route := findGenAIRouteForTest(t, CreateGenAIRouteConfigs("/genai"), "/genai/v1beta/models/{model:*}", "POST")
	handler := NewGenericRouter(client, &mockHandlerStore{}, nil, nil, nil, bifrost.NewNoOpLogger()).createHandler(route)
	httpCtx := &fasthttp.RequestCtx{}
	httpCtx.Request.Header.SetMethod(fasthttp.MethodPost)
	httpCtx.Request.SetRequestURI("/genai/v1beta/models/dashscope/qwen3-tts-flash:generateContent")
	httpCtx.SetUserValue("model", "dashscope/qwen3-tts-flash:generateContent")
	httpCtx.Request.Header.SetContentType("application/json")
	httpCtx.Request.SetBody([]byte(`{"contents":[{"role":"user","parts":[{"text":"Hello"}]}],` +
		`"generationConfig":{"responseModalities":["AUDIO"],"speechConfig":{"voiceConfig":{"prebuiltVoiceConfig":{"voiceName":"Cherry"}}}}}`))
	handler(httpCtx)

	if httpCtx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("status = %d, body = %s", httpCtx.Response.StatusCode(), httpCtx.Response.Body())
	}
	if !strings.Contains(sent, `"voice":"Cherry"`) || !strings.Contains(sent, `"text":"Hello"`) {
		t.Fatalf("vendor body = %s, want the text and voice", sent)
	}
	var body struct {
		URL       string         `json:"url"`
		ExpiresAt int64          `json:"expires_at"`
		Usage     map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(httpCtx.Response.Body(), &body); err != nil {
		t.Fatalf("body is not JSON: %v: %s", err, httpCtx.Response.Body())
	}
	if body.URL != audioLink || body.ExpiresAt != 1766113409 {
		t.Fatalf("url, expires_at = %q, %d; want the vendor's link and expiry: %s", body.URL, body.ExpiresAt, httpCtx.Response.Body())
	}
	if body.Usage["billed_characters"] != float64(5) {
		t.Fatalf("usage = %v, want billed_characters 5", body.Usage)
	}
	if got := string(httpCtx.Response.Header.Peek("x-bifrost-usage")); !strings.Contains(got, `"billed_characters":5`) {
		t.Fatalf("x-bifrost-usage = %q, want billed_characters 5", got)
	}
}

// TestGenAIRoutesEveryGeminiBatchCreateMethod is the class check for TXE-7030: every method
// the Gemini provider creates a batch with (gemini.GeminiBatchCreateMethods) is a batch create
// on the native /genai route, with the method suffix stripped from the model. A batch method
// the provider gains without a native route fails here.
func TestGenAIRoutesEveryGeminiBatchCreateMethod(t *testing.T) {
	require.NotEmpty(t, gemini.GeminiBatchCreateMethods)
	for _, method := range gemini.GeminiBatchCreateMethods {
		t.Run(method, func(t *testing.T) {
			ctx := &fasthttp.RequestCtx{}
			ctx.SetUserValue("model", "gemini-embedding-001:"+method)
			_, requestType := extractModelAndRequestType(ctx)
			assert.Equal(t, schemas.BatchCreateRequest, requestType)
			assert.NotNil(t, ctx.UserValue(isGeminiBatchCreateRequestContextKey), "the router sends a batch create only when this marker is set")
			assert.Contains(t, gemini.GeminiRequestSuffixPaths, ":"+method, "the model keeps the method suffix")
			if method == gemini.GeminiAsyncBatchEmbedContentMethod {
				assert.NotNil(t, ctx.UserValue(isGeminiAsyncBatchEmbedContentRequestContextKey))
			} else {
				assert.Nil(t, ctx.UserValue(isGeminiAsyncBatchEmbedContentRequestContextKey))
			}
		})
	}
}

// TestGenAIAsyncBatchEmbedContentCreatesAndReadsEmbeddingBatch (TXE-7030): Google's SDK
// batches.create_embeddings through /genai creates one asyncBatchEmbedContent job at the
// vendor, and batches.get through /genai returns the job's embedding responses at
// metadata.output, where the SDK reads them (as dest.inlined_embed_content_responses). Run on the typed
// path (bare model) and on the raw passthrough (x-model-provider: gemini).
func TestGenAIAsyncBatchEmbedContentCreatesAndReadsEmbeddingBatch(t *testing.T) {
	// The request body Google's SDK sends (python-genai batches.py, _EmbedContentBatch_to_mldev).
	sdkBody := `{"batch":{"displayName":"emb-batch","inputConfig":{"requests":{"requests":[` +
		`{"request":{"content":{"parts":[{"text":"alpha"}]},"taskType":"RETRIEVAL_DOCUMENT","outputDimensionality":2}},` +
		`{"request":{"content":{"parts":[{"text":"beta"}]},"taskType":"RETRIEVAL_DOCUMENT","outputDimensionality":2}}]}}}}`
	const createdJob = `{"name":"batches/emb1","metadata":{"@type":"type.googleapis.com/google.ai.generativelanguage.v1main.EmbedContentBatch",` +
		`"name":"batches/emb1","model":"models/gemini-embedding-001","displayName":"emb-batch","state":"BATCH_STATE_PENDING",` +
		`"createTime":"2026-11-20T08:00:00Z","updateTime":"2026-11-20T08:00:00Z","batchStats":{"requestCount":"2","pendingRequestCount":"2"}}}`
	const doneJob = `{"name":"batches/emb1","done":true,"metadata":{"@type":"type.googleapis.com/google.ai.generativelanguage.v1main.EmbedContentBatch",` +
		`"name":"batches/emb1","model":"models/gemini-embedding-001","displayName":"emb-batch","state":"BATCH_STATE_SUCCEEDED",` +
		`"createTime":"2026-11-20T08:00:00Z","updateTime":"2026-11-20T08:05:00Z","endTime":"2026-11-20T08:05:00Z",` +
		`"batchStats":{"requestCount":"2","successfulRequestCount":"2"},` +
		`"output":{"inlinedResponses":{"inlinedResponses":[` +
		`{"response":{"embedding":{"values":[0.11,0.12]}}},{"response":{"embedding":{"values":[0.21,0.22]}}}]}}}}`

	for _, tc := range []struct {
		name   string
		header string
	}{
		{"typed path", ""},
		{"raw passthrough", "gemini"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type call struct{ method, path, body string }
			var calls []call
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				calls = append(calls, call{r.Method, r.URL.Path, string(body)})
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					_, _ = w.Write([]byte(doneJob))
					return
				}
				_, _ = w.Write([]byte(createdJob))
			}))
			defer upstream.Close()

			account := &nativeMessagesTestAccount{
				configs: map[schemas.ModelProvider]*schemas.ProviderConfig{
					schemas.Gemini: {NetworkConfig: schemas.NetworkConfig{BaseURL: upstream.URL + "/v1beta", AllowPrivateNetwork: true}},
				},
				keys: map[schemas.ModelProvider][]schemas.Key{
					schemas.Gemini: {{ID: "gemini-key", Value: schemas.SecretVar{Val: "gemini-secret"}, Models: schemas.WhiteList{"*"}, Weight: 1, UseForBatchAPI: schemas.Ptr(true)}},
				},
			}
			client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{Account: account, Logger: bifrost.NewNoOpLogger(), InitialPoolSize: 1})
			require.NoError(t, err)
			defer client.Shutdown()
			router := NewGenericRouter(client, &mockHandlerStore{}, nil, nil, nil, bifrost.NewNoOpLogger())
			routes := CreateGenAIRouteConfigs("/genai")

			// batches.create_embeddings
			create := &fasthttp.RequestCtx{}
			create.Request.Header.SetMethod(fasthttp.MethodPost)
			create.Request.SetRequestURI("/genai/v1beta/models/gemini-embedding-001:asyncBatchEmbedContent")
			create.SetUserValue("model", "gemini-embedding-001:asyncBatchEmbedContent")
			create.Request.Header.SetContentType("application/json")
			if tc.header != "" {
				create.Request.Header.Set("x-model-provider", tc.header)
			}
			create.Request.SetBody([]byte(sdkBody))
			createRoute := findGenAIRouteForTest(t, routes, "/genai/v1beta/models/{model:*}", "POST")
			// The router's request-type middleware runs before the handler.
			create.SetUserValue(schemas.BifrostContextKeyHTTPRequestType, createRoute.GetHTTPRequestType(create))
			router.createHandler(createRoute)(create)

			require.Equal(t, fasthttp.StatusOK, create.Response.StatusCode(), "%s", create.Response.Body())
			require.Len(t, calls, 1, "one create call: %+v", calls)
			assert.Equal(t, http.MethodPost, calls[0].method)
			assert.Equal(t, "/v1beta/models/gemini-embedding-001:asyncBatchEmbedContent", calls[0].path)
			sent := []byte(calls[0].body)
			assert.Equal(t, "alpha", gjson.GetBytes(sent, "batch.inputConfig.requests.requests.0.request.content.parts.0.text").String(), "%s", sent)
			assert.Equal(t, "beta", gjson.GetBytes(sent, "batch.inputConfig.requests.requests.1.request.content.parts.0.text").String(), "%s", sent)
			assert.Equal(t, "RETRIEVAL_DOCUMENT", gjson.GetBytes(sent, "batch.inputConfig.requests.requests.0.request.taskType").String(), "%s", sent)
			assert.Equal(t, "emb-batch", gjson.GetBytes(sent, "batch.displayName").String(), "%s", sent)
			created := create.Response.Body()
			assert.Equal(t, "batches/emb1", gjson.GetBytes(created, "name").String(), "%s", created)

			// batches.get
			get := &fasthttp.RequestCtx{}
			get.Request.Header.SetMethod(fasthttp.MethodGet)
			get.Request.SetRequestURI("/genai/v1beta/batches/emb1")
			get.SetUserValue("batch_id", "batches/emb1")
			getRoute := findGenAIRouteForTest(t, CreateGenAIBatchRouteConfigs("/genai", &mockHandlerStore{}), "/genai/v1beta/batches/{batch_id}", "GET")
			get.SetUserValue(schemas.BifrostContextKeyHTTPRequestType, getRoute.GetHTTPRequestType(get))
			router.createHandler(getRoute)(get)

			require.Equal(t, fasthttp.StatusOK, get.Response.StatusCode(), "%s", get.Response.Body())
			require.Len(t, calls, 2, "%+v", calls)
			assert.Equal(t, "/v1beta/batches/emb1", calls[1].path)
			job := get.Response.Body()
			assert.Equal(t, "batches/emb1", gjson.GetBytes(job, "name").String(), "%s", job)
			assert.Equal(t, "BATCH_STATE_SUCCEEDED", gjson.GetBytes(job, "metadata.state").String(), "%s", job)
			// python-genai reads metadata.output (_BatchJob_from_mldev) and, when an inline response
			// holds an embedding, renames inlinedResponses to inlinedEmbedContentResponses itself
			// (t_recv_batch_job_destination); it maps BATCH_STATE_* to JOB_STATE_* (t_job_state).
			// So the route hands back the vendor's output object unchanged.
			inlined := gjson.GetBytes(job, "metadata.output.inlinedResponses.inlinedResponses")
			require.True(t, inlined.IsArray(), "the SDK reads metadata.output.inlinedResponses.inlinedResponses: %s", job)
			require.Len(t, inlined.Array(), 2, "%s", job)
			assert.Equal(t, []float64{0.11, 0.12}, floats(inlined.Array()[0].Get("response.embedding.values")), "%s", job)
			assert.Equal(t, []float64{0.21, 0.22}, floats(inlined.Array()[1].Get("response.embedding.values")), "%s", job)
		})
	}
}

// TestGenAIBatchCreateChecksTheFileGeminiRuns (TXE-7030, Codex security finding on #407): a
// raw-passthrough batch create is forwarded as sent, so the file whose ownership is checked
// (InputFileID) is read from the body as Gemini reads it. A body that names one file in one
// spelling and another file in another is refused before anything reaches the vendor; a file
// named only in a proto-name spelling is still the file checked.
func TestGenAIBatchCreateChecksTheFileGeminiRuns(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"batches/f1","metadata":{"name":"batches/f1","state":"BATCH_STATE_PENDING"}}`))
	}))
	defer upstream.Close()
	account := &nativeMessagesTestAccount{
		configs: map[schemas.ModelProvider]*schemas.ProviderConfig{
			schemas.Gemini: {NetworkConfig: schemas.NetworkConfig{BaseURL: upstream.URL + "/v1beta", AllowPrivateNetwork: true}},
		},
		keys: map[schemas.ModelProvider][]schemas.Key{
			schemas.Gemini: {{ID: "gemini-key", Value: schemas.SecretVar{Val: "gemini-secret"}, Models: schemas.WhiteList{"*"}, Weight: 1, UseForBatchAPI: schemas.Ptr(true)}},
		},
	}
	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{Account: account, Logger: bifrost.NewNoOpLogger(), InitialPoolSize: 1})
	require.NoError(t, err)
	defer client.Shutdown()
	router := NewGenericRouter(client, &mockHandlerStore{}, nil, nil, nil, bifrost.NewNoOpLogger())
	createRoute := findGenAIRouteForTest(t, CreateGenAIRouteConfigs("/genai"), "/genai/v1beta/models/{model:*}", "POST")

	for _, body := range []string{
		`{"batch":{"inputConfig":{"fileName":"files/mine","file_name":"files/theirs"}}}`,
		`{"batch":{"inputConfig":{"fileName":"files/mine"},"input_config":{"file_name":"files/theirs"}}}`,
	} {
		for _, method := range gemini.GeminiBatchCreateMethods {
			t.Run(method+" "+body, func(t *testing.T) {
				calls = 0
				create := &fasthttp.RequestCtx{}
				create.Request.Header.SetMethod(fasthttp.MethodPost)
				create.Request.SetRequestURI("/genai/v1beta/models/gemini-embedding-001:" + method)
				create.SetUserValue("model", "gemini-embedding-001:"+method)
				create.Request.Header.SetContentType("application/json")
				create.Request.Header.Set("x-model-provider", "gemini")
				create.Request.SetBody([]byte(body))
				create.SetUserValue(schemas.BifrostContextKeyHTTPRequestType, createRoute.GetHTTPRequestType(create))
				router.createHandler(createRoute)(create)

				assert.Equal(t, fasthttp.StatusBadRequest, create.Response.StatusCode(), "%s", create.Response.Body())
				assert.Contains(t, string(create.Response.Body()), "two input files", "%s", create.Response.Body())
				assert.Zero(t, calls, "a refused create reaches no vendor")
			})
		}
	}

	t.Run("proto-name spelling is the file checked", func(t *testing.T) {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		raw := []byte(`{"batch":{"input_config":{"file_name":"files/theirs"}}}`)
		ctx.SetValue(genAIRawRequestBodyContextKey, raw)
		req := &gemini.GeminiBatchCreateRequest{}
		require.NoError(t, sonic.Unmarshal(raw, req))
		batch, err := createRoute.BatchRequestConverter(ctx, req)
		require.NoError(t, err)
		assert.Equal(t, "files/theirs", batch.CreateRequest.InputFileID, "the governance file-ownership check reads InputFileID")
	})
}

func floats(r gjson.Result) []float64 {
	var out []float64
	for _, v := range r.Array() {
		out = append(out, v.Float())
	}
	return out
}

// BenchmarkExtractModelAndRequestType measures the /genai ingress classification every native
// request runs. TXE-7030 replaced one HasSuffix(":batchGenerateContent") with a loop over
// gemini.GeminiBatchCreateMethods (two entries).
func BenchmarkExtractModelAndRequestType(b *testing.B) {
	for _, model := range []string{
		"gemini-2.5-flash:generateContent",
		"gemini-2.5-flash:batchGenerateContent",
		"gemini-embedding-001:asyncBatchEmbedContent",
	} {
		b.Run(model, func(b *testing.B) {
			ctx := &fasthttp.RequestCtx{}
			b.ReportAllocs()
			for b.Loop() {
				ctx.SetUserValue("model", model)
				_, _ = extractModelAndRequestType(ctx)
			}
		})
	}
}
