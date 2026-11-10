package vertex

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/capsohq/bifrost/core/providers/gemini"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// Vertex serves Gemini text-to-speech through the same generateContent request the Gemini
// API takes (TXE-6540). The request body, the response conversion, the block and truncation
// errors and the stream reader are the Gemini provider's; this file adds the Vertex URL and
// credentials.

// SupportedUnifiedFields implements schemas.UnifiedFieldDeclarer for speech: the fields the
// Gemini generateContent conversion delivers. Core refuses any other speech field with
// caller_unsupported_parameter before a key is selected (contract C4).
func (provider *VertexProvider) SupportedUnifiedFields(requestType schemas.RequestType) ([]string, bool) {
	switch requestType {
	case schemas.SpeechRequest, schemas.SpeechStreamRequest:
		return gemini.SpeechUnifiedFields, true
	}
	return nil, false
}

// isVertexGeminiSpeechModel reports whether Vertex serves speech for model: a Gemini model,
// or a tuned Gemini model addressed by its numeric endpoint ID.
func isVertexGeminiSpeechModel(ctx *schemas.BifrostContext, model string) bool {
	return schemas.IsGeminiModelFamily(ctx, model) || schemas.IsAllDigitsASCII(model)
}

// vertexGeminiSpeechRequestBody builds the body Vertex generateContent takes for a speech
// request: the Gemini API's body, without the fields Vertex reads from elsewhere.
func vertexGeminiSpeechRequestBody(request *schemas.BifrostSpeechRequest) (*VertexRawRequestBody, error) {
	reqBody, err := gemini.ToGeminiSpeechRequest(request)
	if err != nil {
		return nil, err
	}
	extraParams := reqBody.GetExtraParams()
	stripVertexGeminiUnsupportedFields(reqBody)
	rawBody, err := providerUtils.MarshalSorted(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}
	rawBody, err = providerUtils.DeleteJSONField(rawBody, "region")
	if err != nil {
		return nil, fmt.Errorf("failed to delete region field: %w", err)
	}
	return &VertexRawRequestBody{RawBody: rawBody, ExtraParams: extraParams}, nil
}

// vertexGeminiSpeechTarget is where one speech request goes and how it authenticates.
type vertexGeminiSpeechTarget struct {
	url     string            // complete URL, with the API key as a query parameter when the key has one
	headers map[string]string // Authorization, when the key authenticates with OAuth2 credentials
}

// geminiSpeechTarget resolves the URL and credentials for method (":generateContent" or
// ":streamGenerateContent") on the request's model. It makes no upstream call for an API key;
// OAuth2 credentials use the provider's cached token source.
func (provider *VertexProvider) geminiSpeechTarget(ctx *schemas.BifrostContext, key schemas.Key, model string, method string, stream bool) (*vertexGeminiSpeechTarget, *schemas.BifrostError) {
	projectID := resolveVertexProjectID(ctx, key)
	if projectID == "" {
		return nil, providerUtils.NewConfigurationError("project ID is not set")
	}
	region := resolveVertexRegion(ctx, key)
	if region == "" {
		return nil, providerUtils.NewConfigurationError("region is not set in key config")
	}
	projectNumber := resolveVertexProjectNumber(ctx, key)
	if schemas.IsAllDigitsASCII(model) && projectNumber == "" {
		return nil, providerUtils.NewConfigurationError("project number is not set for fine-tuned models")
	}

	target := &vertexGeminiSpeechTarget{
		url:     getCompleteURLForGeminiEndpoint(model, region, projectID, projectNumber, method),
		headers: map[string]string{},
	}
	// query is "alt=sse", "key=<key>", both joined, or empty.
	query := ""
	if stream {
		query = "alt=sse"
		target.headers["Accept"] = "text/event-stream"
		target.headers["Cache-Control"] = "no-cache"
	}
	if apiKey := key.Value.GetValue(); apiKey != "" {
		if query != "" {
			query += "&"
		}
		query += "key=" + url.QueryEscape(apiKey)
	} else {
		tokenSource, err := provider.getAuthTokenSource(key)
		if err != nil {
			return nil, providerUtils.NewBifrostOperationError("error creating auth token source", err)
		}
		token, err := tokenSource.Token()
		if err != nil {
			return nil, providerUtils.NewBifrostOperationError("error getting token", err)
		}
		target.headers["Authorization"] = "Bearer " + token.AccessToken
	}
	if query != "" {
		target.url += "?" + query
	}
	return target, nil
}

// Speech performs a Gemini speech synthesis request through Vertex generateContent.
func (provider *VertexProvider) Speech(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostSpeechRequest) (*schemas.BifrostSpeechResponse, *schemas.BifrostError) {
	if !isVertexGeminiSpeechModel(ctx, request.Model) {
		return nil, providerUtils.NewUnsupportedOperationError(schemas.SpeechRequest, provider.GetProviderKey())
	}
	// A value Gemini cannot take as given is refused before anything is sent (contract C4).
	if bifrostErr := gemini.ValidateSpeechRequest(request); bifrostErr != nil {
		return nil, bifrostErr
	}

	jsonBody, bifrostErr := providerUtils.CheckContextAndGetRequestBody(ctx, request, func() (providerUtils.RequestBodyWithExtraParams, error) {
		return vertexGeminiSpeechRequestBody(request)
	})
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	target, bifrostErr := provider.geminiSpeechTarget(ctx, key, request.Model, ":generateContent", false)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType("application/json")
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	for name, value := range target.headers {
		req.Header.Set(name, value)
	}
	req.SetRequestURI(target.url)
	usedLargePayloadBody := providerUtils.ApplyLargePayloadRequestBody(ctx, req)
	if !usedLargePayloadBody {
		req.SetBody(jsonBody)
	}

	// The answer is always read whole and converted: the audio is base64 inside the vendor's
	// JSON, so large-response passthrough would hand the caller that envelope instead of audio.
	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if bifrostErr != nil {
		return nil, providerUtils.EnrichError(ctx, bifrostErr, jsonBody, nil, provider.sendBackRawRequest, provider.sendBackRawResponse, latency)
	}
	if usedLargePayloadBody {
		providerUtils.DrainLargePayloadRemainder(ctx)
	}
	providerResponseHeaders := providerUtils.ExtractProviderResponseHeaders(resp)
	ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, providerResponseHeaders)

	if resp.StatusCode() != fasthttp.StatusOK {
		// Remove client from pool for authentication/authorization errors
		if resp.StatusCode() == fasthttp.StatusUnauthorized || resp.StatusCode() == fasthttp.StatusForbidden {
			provider.removeVertexClient(key)
		}
		return nil, providerUtils.EnrichError(ctx, parseVertexError(resp), jsonBody, nil, provider.sendBackRawRequest, provider.sendBackRawResponse, latency)
	}

	responseBody, decodeErr := providerUtils.CheckAndDecodeBody(resp)
	if decodeErr != nil {
		return nil, providerUtils.EnrichError(ctx, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, decodeErr), jsonBody, nil, provider.sendBackRawRequest, provider.sendBackRawResponse, latency)
	}

	geminiResponse := gemini.GenerateContentResponse{}
	rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, responseBody, &geminiResponse, jsonBody, providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest), providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse))
	if bifrostErr != nil {
		return nil, providerUtils.EnrichError(ctx, bifrostErr, jsonBody, responseBody, provider.sendBackRawRequest, provider.sendBackRawResponse, latency)
	}

	// A blocked speech comes back as the caller's content block with its billed usage (contract C6).
	response, speechErr := gemini.SpeechResponseFromGenerateContent(ctx, request, &geminiResponse)
	if speechErr != nil {
		return nil, providerUtils.EnrichError(ctx, speechErr, jsonBody, responseBody, provider.sendBackRawRequest, provider.sendBackRawResponse, latency)
	}

	response.ExtraFields.Latency = latency.Milliseconds()
	response.ExtraFields.ProviderResponseHeaders = providerResponseHeaders
	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		response.ExtraFields.RawRequest = rawRequest
	}
	if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
		response.ExtraFields.RawResponse = rawResponse
	}
	return response, nil
}

// SpeechStream performs a streaming Gemini speech synthesis request through Vertex
// streamGenerateContent, read by the Gemini provider's speech stream reader.
func (provider *VertexProvider) SpeechStream(ctx *schemas.BifrostContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.BifrostSpeechRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	if !isVertexGeminiSpeechModel(ctx, request.Model) {
		return nil, providerUtils.NewUnsupportedOperationError(schemas.SpeechStreamRequest, provider.GetProviderKey())
	}
	// A value Gemini cannot take as given is refused before anything is sent (contract C4).
	if bifrostErr := gemini.ValidateSpeechRequest(request); bifrostErr != nil {
		return nil, bifrostErr
	}

	jsonBody, bifrostErr := providerUtils.CheckContextAndGetRequestBody(ctx, request, func() (providerUtils.RequestBodyWithExtraParams, error) {
		return vertexGeminiSpeechRequestBody(request)
	})
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	target, bifrostErr := provider.geminiSpeechTarget(ctx, key, request.Model, ":streamGenerateContent", true)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	return gemini.HandleGeminiSpeechStream(
		ctx,
		provider.streamingClient,
		target.url,
		jsonBody,
		target.headers,
		provider.networkConfig.ExtraHeaders,
		provider.networkConfig.StreamIdleTimeoutInSeconds,
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		request,
		postHookRunner,
		provider.logger,
		postHookSpanFinalizer,
	)
}
