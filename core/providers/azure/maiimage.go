package azure

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/capsohq/bifrost/core/providers/openai"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// Microsoft MAI-Image on Azure AI Foundry (fork, TXE-6543, contract C3, C4, C5, C9).
//
// A request whose model resolves to the mai-image family (schemas.ResolveFamily: the alias's
// model_family, model_name, deployment or key, else the request's model) goes to
// <endpoint>/mai/v1/images/generations, not Azure OpenAI's /openai/v1/images/generations. The key's
// endpoint is the Foundry resource, https://<resource>.services.ai.azure.com. The body is model
// (the deployment), prompt, and width and height, written from size. The response is JSON with
// data[].b64_json (one PNG); core writes usage.output_images from data[] as for every image
// response, and MAI reports no token usage.
//
// Documented at https://learn.microsoft.com/azure/foundry/foundry-models/how-to/use-foundry-models-mai
// (fetched 2026-11-20). Fields the API has no counterpart for are refused with 400
// caller_unsupported_parameter, before any upstream call, never dropped (C4):
//   - n other than 1: the API returns one image and has no n.
//   - size that is not WIDTHxHEIGHT. "auto" sends no width or height, the API's own default.
//   - response_format other than b64_json: the API returns no URL.
//   - output_format other than png: "The output format is always PNG".
//   - every other unified field but background, which the page's MAI-Image-2.7 sample sends and
//     which is passed as given, and the unpriced seed and negative_prompt, which are not sent.
//
// Streaming is not documented for MAI, so a streamed request is refused naming stream. Edits
// (/mai/v1/images/edits) are out of TXE-6543's scope and still take the Azure OpenAI route.

// maiImageSupportedFields are the unified generation fields the MAI body delivers.
var maiImageSupportedFields = []string{"background", "n", "output_format", "response_format", "size"}

// maiImageGenerationRequest is the body of POST /mai/v1/images/generations.
type maiImageGenerationRequest struct {
	Model       string                 `json:"model"`
	Prompt      string                 `json:"prompt"`
	Width       *int                   `json:"width,omitempty"`
	Height      *int                   `json:"height,omitempty"`
	Background  *string                `json:"background,omitempty"`
	ExtraParams map[string]interface{} `json:"-"`
}

// GetExtraParams implements providerUtils.RequestBodyWithExtraParams.
func (r *maiImageGenerationRequest) GetExtraParams() map[string]interface{} {
	return r.ExtraParams
}

// isMAIImageRequest reports whether a request for model goes to the MAI images API.
func isMAIImageRequest(ctx *schemas.BifrostContext, model string) bool {
	return schemas.ResolveFamily(ctx, model) == schemas.ModelFamilyMAIImage
}

// toMAIImageGenerationRequest builds the MAI body, or the 400 that refuses a field it cannot carry.
func toMAIImageGenerationRequest(request *schemas.BifrostImageGenerationRequest) (*maiImageGenerationRequest, *schemas.BifrostError) {
	if request == nil || request.Input == nil || request.Input.Prompt == "" {
		return nil, providerUtils.NewBifrostOperationError("image generation prompt is not provided", nil)
	}
	body := &maiImageGenerationRequest{Model: request.Model, Prompt: request.Input.Prompt}
	params := request.Params
	if params == nil {
		return body, nil
	}
	if err := schemas.CheckUnifiedFields(&schemas.BifrostRequest{RequestType: schemas.ImageGenerationRequest, ImageGenerationRequest: request}, maiImageSupportedFields); err != nil {
		return nil, err
	}
	if params.N != nil && *params.N != 1 {
		return nil, schemas.NewUnsupportedParameterError("n", fmt.Sprintf("MAI-Image returns one image per request; n must be 1, got %d", *params.N))
	}
	if params.ResponseFormat != nil && *params.ResponseFormat != "b64_json" {
		return nil, schemas.NewUnsupportedParameterError("response_format", "MAI-Image returns the image as b64_json only")
	}
	if params.OutputFormat != nil && !strings.EqualFold(*params.OutputFormat, "png") {
		return nil, schemas.NewUnsupportedParameterError("output_format", "MAI-Image returns PNG only")
	}
	if params.Size != nil && *params.Size != "auto" {
		width, height, ok := parseMAIImageSize(*params.Size)
		if !ok {
			return nil, schemas.NewUnsupportedParameterError("size", "MAI-Image takes size as WIDTHxHEIGHT in pixels, e.g. 1024x768")
		}
		body.Width, body.Height = &width, &height
	}
	body.Background = params.Background
	body.ExtraParams = params.ExtraParams
	return body, nil
}

// parseMAIImageSize splits "1024x768" into 1024 and 768. Both must be positive integers; the
// API's own limits (at least 768 each, a pixel budget per model) are the vendor's to answer.
func parseMAIImageSize(size string) (int, int, bool) {
	w, h, found := strings.Cut(strings.ToLower(strings.TrimSpace(size)), "x")
	if !found {
		return 0, 0, false
	}
	width, errW := strconv.Atoi(w)
	height, errH := strconv.Atoi(h)
	if errW != nil || errH != nil || width <= 0 || height <= 0 {
		return 0, 0, false
	}
	return width, height, true
}

// maiImageGeneration sends one generation to the MAI images API with the named key (C3).
func (provider *AzureProvider) maiImageGeneration(ctx *schemas.BifrostContext, key schemas.Key, endpoint string,
	request *schemas.BifrostImageGenerationRequest,
) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	body, convErr := toMAIImageGenerationRequest(request)
	if convErr != nil {
		return nil, convErr
	}
	jsonData, bifrostErr := providerUtils.CheckContextAndGetRequestBody(ctx, request,
		func() (providerUtils.RequestBodyWithExtraParams, error) { return body, nil })
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	authHeader, bifrostErr := provider.getAzureAuthHeaders(ctx, key, false)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(strings.TrimRight(endpoint, "/") + "/mai/v1/images/generations")
	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType("application/json")
	for k, v := range authHeader {
		req.Header.Set(k, v)
	}
	req.SetBody(jsonData)

	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if bifrostErr != nil {
		return nil, providerUtils.EnrichError(ctx, bifrostErr, jsonData, nil, sendBackRawRequest, sendBackRawResponse, latency)
	}
	providerResponseHeaders := providerUtils.ExtractProviderResponseHeaders(resp)
	ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, providerResponseHeaders)

	if resp.StatusCode() != fasthttp.StatusOK {
		return nil, providerUtils.EnrichError(ctx, openai.ParseOpenAIError(resp), jsonData, nil, sendBackRawRequest, sendBackRawResponse, latency)
	}

	respBody, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err)
	}
	response := &schemas.BifrostImageGenerationResponse{}
	rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponse(respBody, response, jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if response.Model == "" {
		response.Model = request.Model
	}
	response.ExtraFields.Latency = latency.Milliseconds()
	response.ExtraFields.ProviderResponseHeaders = providerResponseHeaders
	if sendBackRawRequest {
		response.ExtraFields.RawRequest = rawRequest
	}
	if sendBackRawResponse {
		response.ExtraFields.RawResponse = rawResponse
	}
	return response, nil
}

// maiImageStreamRefusal is the 400 for a streamed MAI generation: the API documents no stream.
func maiImageStreamRefusal() *schemas.BifrostError {
	return schemas.NewUnsupportedParameterError("stream", "MAI-Image on Azure AI Foundry does not stream; send the request without stream")
}
