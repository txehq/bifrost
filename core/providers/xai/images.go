package xai

// xAI image generation and edit against the Bifrost–Tokun contract (fork, TXE-6606):
// C4 (deliver every price-affecting field or refuse it), C5 (vendor-reported usage) and
// C6 (blocks). xAI's image API: POST /v1/images/generations and POST /v1/images/edits,
// both JSON (the edit endpoint does not take multipart).

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/capsohq/bifrost/core/providers/openai"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

const (
	xaiImageGenerationsPath = "/v1/images/generations"
	xaiImageEditsPath       = "/v1/images/edits"

	// xaiDeferredParam is xAI's switch that turns an image request into a task polled at
	// GET /v1/images/{request_id}. Bifrost's sync image routes make one upstream call and
	// return the image (contract C3), so the switch is refused rather than sent.
	xaiDeferredParam = "deferred"
)

// xaiImageModelsWithQuality are the models xAI documents as taking quality ("The parameter
// is only supported for grok-imagine-image-2.0"). Any other model refuses the field.
var xaiImageModelsWithQuality = []string{"grok-imagine-image-2.0"}

// xaiImageResolutions are the resolution tiers xAI takes, in its own lower-case spelling.
// The unified field is written upper case ("1K", "2K"); any other tier is refused.
var xaiImageResolutions = []string{"1k", "1.5k", "2k"}

// xaiRespectModerationKey marks a response whose images carry xAI's moderation flag.
var xaiRespectModerationKey = []byte(`"respect_moderation"`)

// xaiImageEditFields are the unified image edit fields the edit body carries. Every other
// price-affecting field on an edit (size, mask, background, output_format, ...) has no xAI
// counterpart and is refused with caller_unsupported_parameter instead of being dropped.
var xaiImageEditFields = []string{"n", "response_format", "aspect_ratio", "resolution", "quality", "user"}

// xaiImageGenerationFields are the unified image generation fields xAI's /v1/images/generations
// takes (fork, TXE-6960, contract C4). Every other unified field is refused with
// caller_unsupported_parameter before any upstream call, never dropped or forwarded:
//   - size: xAI sizes an image by aspect_ratio and resolution, and a pixel size does not map
//     onto one documented pair (1024x1024 is 1:1, but at which tier?), so it is refused, not
//     guessed;
//   - background, moderation, partial_images, output_format, input_images, type and
//     num_inference_steps: xAI's REST reference documents none of them;
//   - style and output_compression: not price-affecting under C4, so dropping them would be
//     allowed, but they are refused like every field xAI does not deliver, so the caller is
//     told rather than receiving an image without them. seed and negative_prompt stay exempt
//     (UnpricedUnifiedFields).
var xaiImageGenerationFields = []string{"n", "response_format", "aspect_ratio", "resolution", "quality", "user"}

// XAIImageSource is one source image of an edit: a public URL or a data URI.
type XAIImageSource struct {
	URL  string `json:"url"`
	Type string `json:"type"`
}

// XAIImageEditRequest is the JSON body of POST /v1/images/edits. One source image goes in
// image; several (xAI takes up to five, referred to as <IMAGE_0>, <IMAGE_1>, ... in the
// prompt) go in images, which xAI makes mutually exclusive with image.
type XAIImageEditRequest struct {
	Model          string           `json:"model"`
	Prompt         string           `json:"prompt"`
	Image          *XAIImageSource  `json:"image,omitempty"`
	Images         []XAIImageSource `json:"images,omitempty"`
	N              *int             `json:"n,omitempty"`
	ResponseFormat *string          `json:"response_format,omitempty"`
	AspectRatio    *string          `json:"aspect_ratio,omitempty"`
	Resolution     *string          `json:"resolution,omitempty"`
	Quality        *string          `json:"quality,omitempty"`
	User           *string          `json:"user,omitempty"`

	ExtraParams map[string]interface{} `json:"-"`
}

// GetExtraParams implements providerUtils.RequestBodyWithExtraParams.
func (r *XAIImageEditRequest) GetExtraParams() map[string]interface{} {
	return r.ExtraParams
}

// xaiImageModerationFlags reads the per-image moderation flag from an xAI image response.
// xAI's SDK documents respect_moderation as "Whether the image respects moderation rules";
// an image with the flag false is one xAI blocked.
type xaiImageModerationFlags struct {
	Data []struct {
		RespectModeration *bool `json:"respect_moderation,omitempty"`
	} `json:"data"`
}

// checkXAIImageRequest refuses, before any upstream call, what an xAI image request cannot
// deliver as given: the deferred switch, and quality on a model that does not take it.
func checkXAIImageRequest(model string, quality *string, extraParams map[string]interface{}) *schemas.BifrostError {
	if _, ok := extraParams[xaiDeferredParam]; ok {
		return schemas.NewUnsupportedParameterError(xaiDeferredParam,
			"deferred image requests are not supported: the image routes return the image from one upstream call")
	}
	if quality != nil && !slices.Contains(xaiImageModelsWithQuality, model) {
		return schemas.NewUnsupportedParameterError("quality",
			fmt.Sprintf("model %q does not take quality; xAI accepts it only on %s", model, strings.Join(xaiImageModelsWithQuality, ", ")))
	}
	return nil
}

// toXAIImageResolution converts the unified resolution tier to xAI's spelling, refusing a
// tier xAI does not offer. An empty tier (the field absent) converts to "".
func toXAIImageResolution(resolution schemas.TolerantString) (string, *schemas.BifrostError) {
	if resolution == "" {
		return "", nil
	}
	tier := strings.ToLower(strings.TrimSpace(string(resolution)))
	if !slices.Contains(xaiImageResolutions, tier) {
		return "", schemas.NewUnsupportedParameterError("resolution",
			fmt.Sprintf("resolution %q is not offered by xAI; it takes %s", string(resolution), strings.Join(xaiImageResolutions, ", ")))
	}
	return tier, nil
}

// withoutExtraParam returns extraParams without key, never mutating the caller's map.
func withoutExtraParam(extraParams map[string]interface{}, key string) map[string]interface{} {
	if _, ok := extraParams[key]; !ok {
		return extraParams
	}
	out := maps.Clone(extraParams)
	delete(out, key)
	return out
}

// toXAIImageGenerationRequest builds the generation body: the OpenAI shape xAI's endpoint
// takes, with quality kept and the unified resolution tier converted to xAI's field. The
// handlers also keep resolution in the extra params (TXE-6518); the converted typed value
// replaces it so the body never carries two spellings.
func toXAIImageGenerationRequest(request *schemas.BifrostImageGenerationRequest) (*openai.OpenAIImageGenerationRequest, *schemas.BifrostError) {
	body := openai.ToOpenAIImageGenerationRequest(request)
	if body == nil {
		return nil, providerUtils.NewBifrostOperationError("invalid image generation request: a prompt is required", nil)
	}
	if request.Params != nil && request.Params.Resolution != "" {
		resolution, bifrostErr := toXAIImageResolution(request.Params.Resolution)
		if bifrostErr != nil {
			return nil, bifrostErr
		}
		body.Resolution = schemas.TolerantString(resolution)
		body.ExtraParams = withoutExtraParam(body.ExtraParams, "resolution")
	}
	return body, nil
}

// toXAIImageEditRequest builds the edit body. Each source image is sent as given when it is
// a URL or data URI (contract C4 agreement four: Bifrost does not download a URL) and as a
// data URI when the caller sent bytes.
func toXAIImageEditRequest(request *schemas.BifrostImageEditRequest) (*XAIImageEditRequest, *schemas.BifrostError) {
	if request == nil || request.Input == nil || strings.TrimSpace(request.Input.Prompt) == "" {
		return nil, providerUtils.NewBifrostOperationError("invalid image edit request: a prompt is required", nil)
	}
	if len(request.Input.Images) == 0 {
		return nil, providerUtils.NewBifrostOperationError("invalid image edit request: at least one source image is required", nil)
	}
	sources := make([]XAIImageSource, 0, len(request.Input.Images))
	for i, image := range request.Input.Images {
		source, err := toXAIImageSource(image)
		if err != nil {
			return nil, providerUtils.NewBifrostOperationError(fmt.Sprintf("invalid image edit request: source image %d: %v", i, err), nil)
		}
		sources = append(sources, source)
	}

	body := &XAIImageEditRequest{Model: request.Model, Prompt: request.Input.Prompt}
	if len(sources) == 1 {
		body.Image = &sources[0]
	} else {
		body.Images = sources
	}
	if params := request.Params; params != nil {
		resolution, bifrostErr := toXAIImageResolution(params.Resolution)
		if bifrostErr != nil {
			return nil, bifrostErr
		}
		body.N = params.N
		body.ResponseFormat = params.ResponseFormat
		body.AspectRatio = params.AspectRatio
		if resolution != "" {
			body.Resolution = &resolution
		}
		body.Quality = params.Quality
		body.User = params.User
		body.ExtraParams = params.ExtraParams
		if resolution != "" {
			body.ExtraParams = withoutExtraParam(body.ExtraParams, "resolution")
		}
	}
	return body, nil
}

// toXAIImageSource converts one edit source image. A URL (including a data URI) is sent
// unchanged; bytes become a data URI whose media type is sniffed from the bytes.
func toXAIImageSource(image schemas.ImageInput) (XAIImageSource, error) {
	if url := strings.TrimSpace(image.URL); url != "" {
		return XAIImageSource{URL: url, Type: "image_url"}, nil
	}
	if len(image.Image) == 0 {
		return XAIImageSource{}, fmt.Errorf("carries neither a URL nor image bytes")
	}
	mediaType := http.DetectContentType(image.Image)
	if !strings.HasPrefix(mediaType, "image/") {
		return XAIImageSource{}, fmt.Errorf("bytes are not an image (detected %s)", mediaType)
	}
	return XAIImageSource{
		URL:  "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(image.Image),
		Type: "image_url",
	}, nil
}

// ImageGeneration performs an image generation request to the xAI API.
func (provider *XAIProvider) ImageGeneration(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostImageGenerationRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	if request == nil {
		return nil, providerUtils.NewBifrostOperationError("invalid request: nil", nil)
	}
	if rawBody, ok := xaiRawImageBody(ctx, request); ok {
		return provider.doXAIImageRequest(ctx, key, xaiImageGenerationsPath, rawBody)
	}
	if params := request.Params; params != nil {
		if bifrostErr := checkXAIImageRequest(request.Model, params.Quality, params.ExtraParams); bifrostErr != nil {
			return nil, bifrostErr
		}
	}
	if bifrostErr := schemas.CheckUnifiedFields(&schemas.BifrostRequest{RequestType: schemas.ImageGenerationRequest, ImageGenerationRequest: request}, xaiImageGenerationFields); bifrostErr != nil {
		return nil, bifrostErr
	}

	// A large-payload passthrough streams the caller's body unchanged; the OpenAI-shaped
	// handler owns that path, as before this provider had its own image handler.
	if providerUtils.IsLargePayloadPassthroughEnabled(ctx) {
		response, bifrostErr := openai.HandleOpenAIImageGenerationRequest(
			ctx,
			provider.client,
			provider.networkConfig.BaseURL+providerUtils.GetPathFromContext(ctx, xaiImageGenerationsPath),
			request,
			key,
			provider.networkConfig.ExtraHeaders,
			nil,
			provider.GetProviderKey(),
			providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
			providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
			provider.logger,
		)
		if bifrostErr != nil {
			return nil, bifrostErr
		}
		response.Usage.NormalizeProviderCost()
		return response, nil
	}

	// Converted up front: a field xAI cannot take is refused before any upstream call.
	body, bifrostErr := toXAIImageGenerationRequest(request)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	jsonData, bifrostErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			return body, nil
		})
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	return provider.doXAIImageRequest(ctx, key, xaiImageGenerationsPath, jsonData)
}

// ImageEdit performs an image edit request to xAI's /v1/images/edits with a JSON body.
func (provider *XAIProvider) ImageEdit(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostImageEditRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	if request == nil {
		return nil, providerUtils.NewBifrostOperationError("invalid request: nil", nil)
	}
	// Large-payload mode streams the caller's multipart body upstream unparsed, but xAI's edit
	// takes only JSON with each source image inlined, which means holding the whole upload in
	// memory: the cost the mode exists to avoid. The shared refusal says so instead of the
	// request failing on the unparsed (empty) input.
	if refusal := providerUtils.LargePayloadRequestRefusal(ctx, schemas.XAI, schemas.ImageEditRequest); refusal != nil {
		return nil, refusal
	}
	if rawBody, ok := xaiRawImageBody(ctx, request); ok {
		return provider.doXAIImageRequest(ctx, key, xaiImageEditsPath, rawBody)
	}
	if params := request.Params; params != nil {
		if bifrostErr := checkXAIImageRequest(request.Model, params.Quality, params.ExtraParams); bifrostErr != nil {
			return nil, bifrostErr
		}
	}
	if bifrostErr := schemas.CheckUnifiedFields(&schemas.BifrostRequest{RequestType: schemas.ImageEditRequest, ImageEditRequest: request}, xaiImageEditFields); bifrostErr != nil {
		return nil, bifrostErr
	}

	// Converted up front: a field xAI cannot take is refused before any upstream call.
	body, bifrostErr := toXAIImageEditRequest(request)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	jsonData, bifrostErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			return body, nil
		})
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	return provider.doXAIImageRequest(ctx, key, xaiImageEditsPath, jsonData)
}

// xaiRawImageBody returns the caller's own body when raw-request-body mode is on. That body
// reaches xAI as written, so the typed fields are neither checked nor converted: they are not
// what is sent, and a field they carry must not refuse a body that does not contain it.
func xaiRawImageBody(ctx *schemas.BifrostContext, request providerUtils.RequestBodyGetter) ([]byte, bool) {
	if providerUtils.IsLargePayloadPassthroughEnabled(ctx) {
		return nil, false
	}
	return providerUtils.CheckAndGetRawRequestBody(ctx, request)
}

// doXAIImageRequest posts an image body, parses an error with xAI's own error shape, and
// normalises a success per contract C5 and C6.
func (provider *XAIProvider) doXAIImageRequest(ctx *schemas.BifrostContext, key schemas.Key, path string, jsonData []byte) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	// A response over the configured large-response threshold is handed to the transport as a
	// stream (LargeResponseReader), which then owns resp; every other path releases it here.
	respOwned := true
	defer func() {
		if respOwned {
			fasthttp.ReleaseResponse(resp)
		}
	}()
	activeClient := providerUtils.PrepareResponseStreaming(ctx, provider.client, resp)

	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(provider.networkConfig.BaseURL + providerUtils.GetPathFromContext(ctx, path))
	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType("application/json")
	for k, v := range openai.BearerAuthHeader(key) {
		req.Header.Set(k, v)
	}
	req.SetBody(jsonData)

	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, activeClient, req, resp)
	defer wait()
	if bifrostErr != nil {
		return nil, providerUtils.EnrichError(ctx, bifrostErr, jsonData, nil, sendBackRawRequest, sendBackRawResponse, latency)
	}
	providerResponseHeaders := providerUtils.ExtractProviderResponseHeaders(resp)
	ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, providerResponseHeaders)

	if resp.StatusCode() < fasthttp.StatusOK || resp.StatusCode() >= fasthttp.StatusMultipleChoices {
		providerUtils.MaterializeStreamErrorBody(ctx, resp)
		return nil, providerUtils.EnrichError(ctx, ParseXAIError(resp), jsonData, nil, sendBackRawRequest, sendBackRawResponse, latency)
	}

	// The returned body is an owned copy (or nil when the response is large).
	body, isLarge, bifrostErr := providerUtils.FinalizeResponseWithLargeDetection(ctx, resp, provider.logger)
	if bifrostErr != nil {
		return nil, providerUtils.EnrichError(ctx, bifrostErr, jsonData, nil, sendBackRawRequest, sendBackRawResponse, latency)
	}
	if isLarge {
		// The transport streams xAI's body to the caller unchanged, as the OpenAI-shaped
		// handler did before this handler existed; the C5/C6 normalisation below needs the
		// whole body in memory, which the configured threshold forbids.
		respOwned = false
		return &schemas.BifrostImageGenerationResponse{
			ExtraFields: schemas.BifrostResponseExtraFields{Latency: latency.Milliseconds(), ProviderResponseHeaders: providerResponseHeaders},
		}, nil
	}

	response := &schemas.BifrostImageGenerationResponse{}
	rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, response, jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	// The moderation flags are decoded only when xAI sent one, so an ordinary response pays
	// a byte scan, not a second decode or an allocation.
	var flags *xaiImageModerationFlags
	if bytes.Contains(body, xaiRespectModerationKey) {
		flags = &xaiImageModerationFlags{}
		if err := sonic.Unmarshal(body, flags); err != nil {
			return nil, providerUtils.EnrichError(ctx, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseUnmarshal, err), jsonData, body, sendBackRawRequest, sendBackRawResponse, latency)
		}
	}
	normalizeXAIImageResponse(response, flags)

	response.ExtraFields.Latency = latency.Milliseconds()
	response.ExtraFields.ProviderResponseHeaders = providerResponseHeaders
	if sendBackRawRequest {
		response.ExtraFields.RawRequest = rawRequest
	}
	// xAI's body still carries an image it marked respect_moderation: false, so the raw copy is
	// withheld when one was blocked: attaching it would hand the caller the image that data and
	// content_filter say was not delivered.
	if sendBackRawResponse && response.ContentFilter == nil {
		response.ExtraFields.RawResponse = rawResponse
	}
	return response, nil
}

// normalizeXAIImageResponse applies contract C5 and C6 to a decoded xAI image response:
//   - an image xAI marked respect_moderation: false is blocked: it leaves data and is counted
//     in content_filter.filtered_count (xAI gives no reason code, so reasons stays empty);
//   - usage.output_images is left to BackfillParams (TXE-6438), which core runs on every image
//     response and which counts the images left in data, so a blocked image is not counted;
//   - usage.vendor_cost is cost_in_usd_ticks, in unit usd_ticks, when xAI reported it, and
//     usage.cost is derived from it as before.
func normalizeXAIImageResponse(response *schemas.BifrostImageGenerationResponse, flags *xaiImageModerationFlags) {
	if response == nil {
		return
	}
	filtered := 0
	if flags != nil && len(flags.Data) == len(response.Data) {
		delivered := make([]schemas.ImageData, 0, len(response.Data))
		for i, image := range response.Data {
			if respect := flags.Data[i].RespectModeration; respect != nil && !*respect {
				filtered++
				continue
			}
			delivered = append(delivered, image)
		}
		response.Data = delivered
	}
	if filtered > 0 {
		response.ContentFilter = &schemas.ContentFilterInfo{FilteredCount: filtered}
	}

	if response.Usage == nil {
		response.Usage = &schemas.ImageUsage{}
	}
	if ticks := response.Usage.CostInUsdTicks; ticks != nil {
		response.Usage.VendorCost = &schemas.VendorCost{Amount: float64(*ticks), Unit: schemas.VendorCostUnitUSDTicks}
	}
	response.Usage.NormalizeProviderCost()
}
