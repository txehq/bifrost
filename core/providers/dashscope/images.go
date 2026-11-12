package dashscope

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// Qwen-Image and Z-Image on DashScope's synchronous endpoint. Sources, read 2026-11-11:
//
//   - Qwen-Image generation: https://www.alibabacloud.com/help/en/model-studio/qwen-image-api
//   - Qwen-Image edit: https://www.alibabacloud.com/help/en/model-studio/qwen-image-edit-api
//   - Z-Image: https://www.alibabacloud.com/help/en/model-studio/z-image-api-reference
//   - model overview: https://www.alibabacloud.com/help/en/model-studio/text-to-image
//
// One request is one DashScope call, and DashScope finishes generation before it answers.

// dashScopeImageSyncPath is the endpoint Qwen-Image (generation and edit) and Z-Image share.
const dashScopeImageSyncPath = "/api/v1/services/aigc/multimodal-generation/generation"

// dashScopeImageFamily selects which parameters a model takes and which route serves it.
type dashScopeImageFamily int

const (
	dashScopeImageUnknown dashScopeImageFamily = iota
	dashScopeImageQwen                         // qwen-image-*: generation and edit, sync
	dashScopeImageZ                            // z-image-*: generation only, sync, one image
	dashScopeImageWan                          // wan*: task-based, not served on the sync routes
)

// dashScopeImageFamilyOf maps a DashScope model name to its family by the documented prefixes.
func dashScopeImageFamilyOf(model string) dashScopeImageFamily {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(m, "qwen-image"):
		return dashScopeImageQwen
	case strings.HasPrefix(m, "z-image"):
		return dashScopeImageZ
	case strings.HasPrefix(m, "wan"):
		return dashScopeImageWan
	default:
		return dashScopeImageUnknown
	}
}

// The unified fields the sync image operations deliver (contract C4). Core refuses any other
// with caller_unsupported_parameter before a key is chosen; seed and negative_prompt are
// unpriced and never refused. n and size reach DashScope as parameters.n and parameters.size;
// response_format is taken only at the one value DashScope produces, a link.
//
// resolution is not declared. The ticket expected a Qwen-Image 3.0 resolution parameter, and
// Alibaba's documentation has none: the Qwen-Image reference lists size, n, prompt_extend,
// negative_prompt, seed and watermark and names only the 2.0 series, and the model overview
// says the 3.0 and 2.1 series take size as width*height from 512*512 to 2048*2048 with no tier
// values. Neither page gives a width and height for "1K" or "2K" on Qwen-Image, so a tier
// cannot be converted to a size without inventing one, and a request carrying resolution is
// refused. The caller sends the size it wants.
var (
	dashScopeImageGenerationFields = []string{"n", "size", "response_format", "input_images"}
	dashScopeImageEditFields       = []string{"n", "size", "response_format"}
)

// dashScopeImagePriceParameters are the parameters the converter writes from a price-affecting
// unified field. schemas.NestedPriceFields lists the same two paths for core's check of an extra
// parameters object.
var dashScopeImagePriceParameters = []string{"n", "size"}

// dashScopeImageTypedExtras are the vendor switches the converter writes into parameters from
// the request's extra params (contract C4 agreement two). Both are documented booleans.
var dashScopeImageTypedExtras = []string{"prompt_extend", "watermark"}

// DashScopeImageRequest is the body of the multimodal-generation endpoint.
type DashScopeImageRequest struct {
	Model       string                    `json:"model"`
	Input       DashScopeImageInput       `json:"input"`
	Parameters  *DashScopeImageParameters `json:"parameters,omitempty"`
	ExtraParams map[string]interface{}    `json:"-"`
}

// GetExtraParams returns the caller's remaining extra params placed where the shared merger
// should put them, under x-bf-passthrough-extra-params. DashScope reads every generation
// switch inside parameters and none at the top level of the body, so an extra param goes
// inside parameters, and an extra param that is itself a parameters object is merged into it.
//
// The names the shared extra-params rule refuses stay at the top level, where it refuses them,
// whether the caller wrote them at the top or inside a parameters object: model and input,
// which the converter set, and the names the gateway alone decides (stream, model_id,
// deployment and the rest of providerUtils.ExtraParamUnsettableField). Nesting them would
// carry them past the rule, and parameters.stream is how DashScope turns streaming on.
func (r *DashScopeImageRequest) GetExtraParams() map[string]interface{} {
	if len(r.ExtraParams) == 0 {
		return nil
	}
	nested := make(map[string]interface{}, len(r.ExtraParams))
	merged := map[string]interface{}{"parameters": nested}
	place := func(name string, value interface{}) {
		if providerUtils.ExtraParamNamesField(name, "model", "input") || providerUtils.ExtraParamUnsettableField(name) != "" {
			merged[name] = value
			return
		}
		nested[name] = value
	}
	for name, value := range r.ExtraParams {
		if providerUtils.ExtraParamNamesField(name, "parameters") {
			object, ok := value.(map[string]interface{})
			if !ok {
				merged[name] = value
				continue
			}
			for k, v := range object {
				place(k, v)
			}
			continue
		}
		place(name, value)
	}
	if len(nested) == 0 {
		delete(merged, "parameters")
	}
	return merged
}

// DashScopeImageInput holds the single user turn DashScope accepts.
type DashScopeImageInput struct {
	Messages []DashScopeImageMessage `json:"messages"`
}

// DashScopeImageMessage is one turn: its role and its content parts.
type DashScopeImageMessage struct {
	Role    string                  `json:"role"`
	Content []DashScopeImageContent `json:"content"`
}

// DashScopeImageContent is one content part: an image (a link or a data URI) or text.
type DashScopeImageContent struct {
	Image            string `json:"image,omitempty"`
	Text             string `json:"text,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// DashScopeImageParameters are the documented generation parameters.
type DashScopeImageParameters struct {
	N              *int    `json:"n,omitempty"`
	Size           *string `json:"size,omitempty"`
	NegativePrompt *string `json:"negative_prompt,omitempty"`
	Seed           *int    `json:"seed,omitempty"`
	PromptExtend   *bool   `json:"prompt_extend,omitempty"`
	Watermark      *bool   `json:"watermark,omitempty"`
}

// DashScopeImageResponse is the 200 body of the multimodal-generation endpoint. code and
// message are DashScope's error fields, read in case a 200 carries them with no output.
type DashScopeImageResponse struct {
	RequestID string                `json:"request_id,omitempty"`
	Output    *DashScopeImageOutput `json:"output,omitempty"`
	Usage     *DashScopeImageUsage  `json:"usage,omitempty"`
	Code      string                `json:"code,omitempty"`
	Message   string                `json:"message,omitempty"`
}

// DashScopeImageOutput holds the generated choices.
type DashScopeImageOutput struct {
	Choices []DashScopeImageChoice `json:"choices,omitempty"`
}

// DashScopeImageChoice is one choice; its message content holds the image links.
type DashScopeImageChoice struct {
	FinishReason string                `json:"finish_reason,omitempty"`
	Message      DashScopeImageMessage `json:"message"`
}

// DashScopeImageUsage is DashScope's usage report. image_count is the billing quantity. The
// token counts are reported by Z-Image, and are zero unless prompt_extend ran.
type DashScopeImageUsage struct {
	ImageCount          *int                              `json:"image_count,omitempty"`
	Width               int                               `json:"width,omitempty"`
	Height              int                               `json:"height,omitempty"`
	InputTokens         int                               `json:"input_tokens,omitempty"`
	OutputTokens        int                               `json:"output_tokens,omitempty"`
	TotalTokens         int                               `json:"total_tokens,omitempty"`
	OutputTokensDetails *DashScopeImageOutputTokenDetails `json:"output_tokens_details,omitempty"`
}

// DashScopeImageOutputTokenDetails carries the reasoning tokens of a rewritten prompt.
type DashScopeImageOutputTokenDetails struct {
	ReasoningTokens *int `json:"reasoning_tokens,omitempty"`
}

// newDashScopeBadRequest is a 400 caller_invalid_request naming the offending field.
func newDashScopeBadRequest(param, message string) *schemas.BifrostError {
	bifrostErr := providerUtils.NewBifrostBadRequestError(message)
	bifrostErr.Error.Param = param
	bifrostErr.ExtraFields.ErrorType = schemas.ErrorTypeCallerInvalidRequest
	return bifrostErr
}

// checkDashScopeSyncImageModel refuses a model the sync image routes cannot serve: one this
// provider has no documented request shape for, and a Wan model, whose API is task-based.
func checkDashScopeSyncImageModel(model string, requestType schemas.RequestType) (dashScopeImageFamily, *schemas.BifrostError) {
	family := dashScopeImageFamilyOf(model)
	switch family {
	case dashScopeImageQwen:
		return family, nil
	case dashScopeImageZ:
		if requestType == schemas.ImageEditRequest {
			return family, newDashScopeBadRequest("model", fmt.Sprintf(
				"dashscope: model %q generates from text only; DashScope documents no image input for Z-Image", model))
		}
		return family, nil
	case dashScopeImageWan:
		return family, newDashScopeBadRequest("model", fmt.Sprintf(
			"dashscope: model %q is task-based and is not served on the synchronous image routes", model))
	}
	bifrostErr := providerUtils.NewBifrostBadRequestError(fmt.Sprintf(
		"dashscope: model %q is not a DashScope image model this provider converts (expected qwen-image-* or z-image-*)", model))
	bifrostErr.Error.Param = "model"
	bifrostErr.ExtraFields.ErrorType = schemas.ErrorTypeCallerModelUnknown
	return family, bifrostErr
}

// rejectDashScopeTaskControlExtras refuses the four task control fields (contract C7). The sync
// image routes have no typed home for them, so they arrive as extra params; DashScope's sync
// endpoint has no callback, external id, deadline or storage target to send them to, and
// forwarding one as an unknown parameter would let the caller believe it took effect.
func rejectDashScopeTaskControlExtras(extraParams map[string]interface{}) *schemas.BifrostError {
	names := make([]string, 0, len(extraParams))
	for name, value := range extraParams {
		names = append(names, name)
		// An extra parameters object is merged into parameters (GetExtraParams), so a task
		// control field written inside it would reach DashScope the same way.
		if object, ok := value.(map[string]interface{}); ok && providerUtils.ExtraParamNamesField(name, "parameters") {
			for nested := range object {
				names = append(names, nested)
			}
		}
	}
	for _, field := range schemas.TaskControlFields {
		for _, name := range names {
			if schemas.SameJSONFieldName(name, field) {
				return schemas.NewUnsupportedParameterError(field, fmt.Sprintf(
					"dashscope: %s cannot be delivered; DashScope's synchronous image endpoint has no such parameter", field))
			}
		}
	}
	return nil
}

// validateDashScopeImageParams refuses values DashScope cannot produce, before any upstream
// call (contract C4 agreement three): nothing is dropped, changed or clamped.
func validateDashScopeImageParams(family dashScopeImageFamily, n *int, size, responseFormat *string, extraParams map[string]interface{}) *schemas.BifrostError {
	if bifrostErr := rejectDashScopeTaskControlExtras(extraParams); bifrostErr != nil {
		return bifrostErr
	}
	if responseFormat != nil && *responseFormat != "" && *responseFormat != "url" {
		return schemas.NewUnsupportedParameterError("response_format", fmt.Sprintf(
			"dashscope returns image links only; response_format %q cannot be delivered", *responseFormat))
	}
	if family == dashScopeImageZ && n != nil && *n != 1 {
		return schemas.NewUnsupportedParameterError("n", fmt.Sprintf(
			"dashscope Z-Image generates one image per request; n=%d cannot be delivered", *n))
	}
	if size != nil && *size != "" {
		if _, ok := toDashScopeImageSize(*size); !ok {
			return schemas.NewUnsupportedParameterError("size", fmt.Sprintf(
				"dashscope takes size as width and height in pixels (1024x1024); %q cannot be converted", *size))
		}
	}
	// n and size are the request's own fields. An extra param that names one in another
	// spelling ("N", "Size") would be written into parameters beside it, or in its place when
	// the request left it out, and DashScope would bill for a value the request does not show.
	for name := range extraParams {
		for _, field := range dashScopeImagePriceParameters {
			if schemas.SameJSONFieldName(name, field) {
				return newDashScopeBadRequest(name, fmt.Sprintf(
					"dashscope: extra params cannot set %q; send the value in the request's own %s field", name, field))
			}
		}
	}
	for _, name := range dashScopeImageTypedExtras {
		if value, ok := extraParams[name]; ok {
			if _, isBool := value.(bool); !isBool {
				return newDashScopeBadRequest(name, fmt.Sprintf("dashscope: %s must be a boolean", name))
			}
		}
	}
	return nil
}

// toDashScopeImageSize converts the unified WIDTHxHEIGHT into DashScope's WIDTH*HEIGHT. A size
// already written with "*" is kept. Anything that is not two positive integers ("auto", "1K")
// reports false: DashScope's range differs per model and is left to DashScope to enforce.
func toDashScopeImageSize(size string) (string, bool) {
	s := strings.TrimSpace(size)
	sep := strings.IndexAny(s, "xX*")
	if sep <= 0 || sep == len(s)-1 {
		return "", false
	}
	width, errW := strconv.Atoi(s[:sep])
	height, errH := strconv.Atoi(s[sep+1:])
	if errW != nil || errH != nil || width <= 0 || height <= 0 {
		return "", false
	}
	return strconv.Itoa(width) + "*" + strconv.Itoa(height), true
}

// dashScopeImageParameters builds the parameters object and returns the extra params left for
// the passthrough merge. Z-Image documents neither n (one image, refused above at any other
// value) nor negative_prompt, so neither is sent to it; negative_prompt is unpriced and may be
// left out (contract C4).
func dashScopeImageParameters(family dashScopeImageFamily, n *int, size *string, seed *int, negativePrompt *string, extraParams map[string]interface{}) (*DashScopeImageParameters, map[string]interface{}) {
	params := &DashScopeImageParameters{Seed: seed}
	if family != dashScopeImageZ {
		params.N = n
		if negativePrompt != nil && *negativePrompt != "" {
			params.NegativePrompt = negativePrompt
		}
	}
	if size != nil && *size != "" {
		if converted, ok := toDashScopeImageSize(*size); ok {
			params.Size = &converted
		}
	}
	var remaining map[string]interface{}
	for name, value := range extraParams {
		switch name {
		case "prompt_extend":
			if b, ok := value.(bool); ok {
				params.PromptExtend = &b
				continue
			}
		case "watermark":
			if b, ok := value.(bool); ok {
				params.Watermark = &b
				continue
			}
		}
		if remaining == nil {
			remaining = make(map[string]interface{}, len(extraParams))
		}
		remaining[name] = value
	}
	if *params == (DashScopeImageParameters{}) {
		params = nil
	}
	return params, remaining
}

// isDashScopeImageReference reports whether s is a form DashScope accepts for an input image:
// a public http(s) link or a base64 data URI.
func isDashScopeImageReference(s string) bool {
	lower := strings.ToLower(s)
	return strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "data:")
}

// newDashScopeImageRequest assembles the body: the input images in order, then the prompt, as
// DashScope's edit sample orders them.
func newDashScopeImageRequest(model, prompt string, images []string, params *DashScopeImageParameters, extra map[string]interface{}) *DashScopeImageRequest {
	content := make([]DashScopeImageContent, 0, len(images)+1)
	for _, image := range images {
		content = append(content, DashScopeImageContent{Image: image})
	}
	content = append(content, DashScopeImageContent{Text: prompt})
	return &DashScopeImageRequest{
		Model:       model,
		Input:       DashScopeImageInput{Messages: []DashScopeImageMessage{{Role: "user", Content: content}}},
		Parameters:  params,
		ExtraParams: extra,
	}
}

// ToDashScopeImageGenerationRequest converts a unified image generation request to the
// multimodal-generation body. input_images are sent unchanged, links and data URIs alike
// (contract C4 agreement four).
func ToDashScopeImageGenerationRequest(bifrostReq *schemas.BifrostImageGenerationRequest) (*DashScopeImageRequest, error) {
	if bifrostReq == nil || bifrostReq.Input == nil || strings.TrimSpace(bifrostReq.Input.Prompt) == "" {
		return nil, providerUtils.InvalidRequestErrorf("prompt is required")
	}
	family := dashScopeImageFamilyOf(bifrostReq.Model)
	var params *DashScopeImageParameters
	var extra map[string]interface{}
	var images []string
	if p := bifrostReq.Params; p != nil {
		images = p.InputImages
		params, extra = dashScopeImageParameters(family, p.N, p.Size, p.Seed, p.NegativePrompt, p.ExtraParams)
	}
	return newDashScopeImageRequest(bifrostReq.Model, bifrostReq.Input.Prompt, images, params, extra), nil
}

// ToDashScopeImageEditRequest converts a unified image edit request to the multimodal-generation
// body. An input image given as a link is sent unchanged; one given as bytes is sent as a base64
// data URI, the only byte form DashScope takes (contract C4 agreement four).
func ToDashScopeImageEditRequest(bifrostReq *schemas.BifrostImageEditRequest) (*DashScopeImageRequest, error) {
	if bifrostReq == nil || bifrostReq.Input == nil || strings.TrimSpace(bifrostReq.Input.Prompt) == "" {
		return nil, providerUtils.InvalidRequestErrorf("prompt is required")
	}
	if len(bifrostReq.Input.Images) == 0 {
		return nil, providerUtils.InvalidRequestErrorf("at least one input image is required")
	}
	images := make([]string, 0, len(bifrostReq.Input.Images))
	for i, image := range bifrostReq.Input.Images {
		switch {
		case image.URL != "":
			images = append(images, image.URL)
		case len(image.Image) > 0:
			images = append(images, providerUtils.FileBytesToBase64DataURL(image.Image))
		default:
			return nil, providerUtils.InvalidRequestErrorf("input image %d has neither a URL nor bytes", i)
		}
	}
	family := dashScopeImageFamilyOf(bifrostReq.Model)
	var params *DashScopeImageParameters
	var extra map[string]interface{}
	if p := bifrostReq.Params; p != nil {
		params, extra = dashScopeImageParameters(family, p.N, p.Size, p.Seed, p.NegativePrompt, p.ExtraParams)
	}
	return newDashScopeImageRequest(bifrostReq.Model, bifrostReq.Input.Prompt, images, params, extra), nil
}

// ToBifrostImageGenerationResponse converts a multimodal-generation 200. Every image link in
// output.choices[].message.content[] becomes one data[] item, in DashScope's order. A 200 with
// no image is an error: DashScope's own error object when the body carries one, otherwise
// provider_empty_result (contract C6).
//
// Usage holds only what DashScope reported (contract C5): image_count as output_images, and
// Z-Image's token counts when they are not zero. created is the time Bifrost read the answer,
// because DashScope's body carries no timestamp.
func ToBifrostImageGenerationResponse(resp *DashScopeImageResponse, model string, now time.Time) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	if resp == nil {
		return nil, providerUtils.NewBifrostOperationError("dashscope image response is nil", nil)
	}
	var data []schemas.ImageData
	if resp.Output != nil {
		for _, choice := range resp.Output.Choices {
			var revisedPrompt string
			for _, part := range choice.Message.Content {
				if part.Text != "" {
					revisedPrompt = part.Text
				}
			}
			for _, part := range choice.Message.Content {
				if part.Image == "" {
					continue
				}
				data = append(data, schemas.ImageData{URL: part.Image, RevisedPrompt: revisedPrompt, Index: len(data)})
			}
		}
	}
	if len(data) == 0 {
		if resp.Code != "" {
			return nil, schemas.NewVendorErrorObjectError(dashScopeVendorErrorCause(resp.Code), resp.Code, resp.Message)
		}
		return nil, schemas.NewProviderEmptyResultError("dashscope returned 200 with no image and no error")
	}

	bifrostResp := &schemas.BifrostImageGenerationResponse{
		ID:      resp.RequestID,
		Created: now.Unix(),
		Model:   model,
		Data:    data,
	}
	if usage := resp.Usage; usage != nil {
		unified := &schemas.ImageUsage{
			OutputImages: usage.ImageCount,
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
			TotalTokens:  usage.TotalTokens,
		}
		if details := usage.OutputTokensDetails; details != nil && details.ReasoningTokens != nil {
			unified.OutputTokensDetails = &schemas.ImageTokenDetails{ReasoningTokens: details.ReasoningTokens}
		}
		bifrostResp.Usage = unified
		if usage.Width > 0 && usage.Height > 0 {
			bifrostResp.ImageGenerationResponseParameters = &schemas.ImageGenerationResponseParameters{
				Size: fmt.Sprintf("%dx%d", usage.Width, usage.Height),
			}
		}
	}
	return bifrostResp, nil
}

// dashScopeVendorErrorCause says whose fault an error object inside a 200 is, from the same
// code table the error statuses use: moderation is the caller's content, a vendor-side class
// is the vendor's, and any other code (InvalidParameter and its kin) is the caller's request.
func dashScopeVendorErrorCause(code string) schemas.VendorErrorCause {
	switch dashScopeErrorType(code) {
	case schemas.ErrorTypeCallerContentFiltered:
		return schemas.VendorErrorCauseContentFiltered
	case "", schemas.ErrorTypeCallerModelUnknown:
		return schemas.VendorErrorCauseCaller
	}
	return schemas.VendorErrorCauseProvider
}

// ImageGeneration generates images with Qwen-Image or Z-Image on DashScope's sync endpoint.
func (provider *DashScopeProvider) ImageGeneration(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostImageGenerationRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	if request == nil {
		return nil, providerUtils.NewBifrostBadRequestError("dashscope: image generation request is nil")
	}
	family, bifrostErr := checkDashScopeSyncImageModel(request.Model, schemas.ImageGenerationRequest)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	rawBody, bifrostErr := dashScopeRawImageBody(ctx, request)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if rawBody != nil {
		return provider.syncImage(ctx, key, schemas.ImageGenerationRequest, request.Model, rawBody)
	}
	if p := request.Params; p != nil {
		if bifrostErr := validateDashScopeImageParams(family, p.N, p.Size, p.ResponseFormat, p.ExtraParams); bifrostErr != nil {
			return nil, bifrostErr
		}
		if len(p.InputImages) > 0 && family == dashScopeImageZ {
			return nil, schemas.NewUnsupportedParameterError("input_images",
				"dashscope Z-Image generates from text only; input_images cannot be delivered")
		}
		for _, image := range p.InputImages {
			if !isDashScopeImageReference(image) {
				return nil, schemas.NewUnsupportedParameterError("input_images",
					"dashscope takes an input image as an http(s) link or a base64 data URI; bare base64 cannot be delivered")
			}
		}
	}
	jsonData, bifrostErr := providerUtils.CheckContextAndGetRequestBody(ctx, request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			return ToDashScopeImageGenerationRequest(request)
		})
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	return provider.syncImage(ctx, key, schemas.ImageGenerationRequest, request.Model, jsonData)
}

// ImageEdit edits images with Qwen-Image on DashScope's sync endpoint.
func (provider *DashScopeProvider) ImageEdit(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostImageEditRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	if request == nil {
		return nil, providerUtils.NewBifrostBadRequestError("dashscope: image edit request is nil")
	}
	family, bifrostErr := checkDashScopeSyncImageModel(request.Model, schemas.ImageEditRequest)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	rawBody, bifrostErr := dashScopeRawImageBody(ctx, request)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if rawBody != nil {
		return provider.syncImage(ctx, key, schemas.ImageEditRequest, request.Model, rawBody)
	}
	if p := request.Params; p != nil {
		if bifrostErr := validateDashScopeImageParams(family, p.N, p.Size, p.ResponseFormat, p.ExtraParams); bifrostErr != nil {
			return nil, bifrostErr
		}
	}
	jsonData, bifrostErr := providerUtils.CheckContextAndGetRequestBody(ctx, request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			return ToDashScopeImageEditRequest(request)
		})
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	return provider.syncImage(ctx, key, schemas.ImageEditRequest, request.Model, jsonData)
}

// dashScopeRawImageBody returns the caller's own body when raw-request-body mode is on. That
// body reaches DashScope as written, so the typed fields are neither checked nor converted:
// they are not what is sent, and a value they carry must not refuse a body that does not
// contain it. A large-payload passthrough is refused: DashScope takes a JSON body with the
// images inlined, and there is no converted body to send in its place.
func dashScopeRawImageBody(ctx *schemas.BifrostContext, request providerUtils.RequestBodyGetter) ([]byte, *schemas.BifrostError) {
	if providerUtils.IsLargePayloadPassthroughEnabled(ctx) {
		return nil, providerUtils.NewBifrostBadRequestError("large payload passthrough is not supported for dashscope images: DashScope takes a JSON body with the images inlined, not a streamed upload; send a smaller upload or the image as a URL")
	}
	rawBody, ok := providerUtils.CheckAndGetRawRequestBody(ctx, request)
	if !ok {
		return nil, nil
	}
	if len(rawBody) == 0 {
		return nil, providerUtils.NewBifrostBadRequestError("dashscope: raw request body mode is on and the request carries no raw body")
	}
	return rawBody, nil
}

// syncImage posts one body to the multimodal-generation endpoint and converts the answer. A
// non-2xx answer becomes an error that keeps DashScope's status, code and body.
func (provider *DashScopeProvider) syncImage(ctx *schemas.BifrostContext, key schemas.Key, requestType schemas.RequestType, model string, jsonData []byte) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	// The shared request helper keeps DashScope's response headers (Retry-After, the request
	// id) for the caller, on a success and on an error alike.
	body, latency, bifrostErr := provider.doDashScopeRequest(ctx, key, requestType, fasthttp.MethodPost, dashScopeImageSyncPath, jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	var dashScopeResp DashScopeImageResponse
	rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &dashScopeResp, jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	bifrostResp, bifrostErr := ToBifrostImageGenerationResponse(&dashScopeResp, model, time.Now())
	if bifrostErr != nil {
		return nil, providerUtils.EnrichError(ctx, bifrostErr, jsonData, body, sendBackRawRequest, sendBackRawResponse, latency)
	}
	bifrostResp.ExtraFields.Latency = latency.Milliseconds()
	if sendBackRawRequest {
		bifrostResp.ExtraFields.RawRequest = rawRequest
	}
	if sendBackRawResponse {
		bifrostResp.ExtraFields.RawResponse = rawResponse
	}
	return bifrostResp, nil
}
