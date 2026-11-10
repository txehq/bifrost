package tencent

import (
	"fmt"
	"maps"
	"strings"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
)

// TokenHub paths of the two Hy image models (product 1823, document 135745).
const (
	tencentImageV3Path  = "/v1/wand/hunyuan-image/v3-generation"
	tencentImageV35Path = "/v1/wand/hunyuan-image/v35-generation"
)

// tencentImageFamily selects the request shape and the default path of a model.
type tencentImageFamily int

const (
	tencentImageUnknown tencentImageFamily = iota
	tencentImageV3                         // hy-image-v3: prompt + images
	tencentImageV35                        // hy-image-v3.5-*: chat-style messages
)

// tencentImageFamilyOf maps a TokenHub model name to its API. The v3.5 prefix is tested first
// because "hy-image-v3" is a prefix of it.
func tencentImageFamilyOf(model string) tencentImageFamily {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(m, "hy-image-v3.5"):
		return tencentImageV35
	case m == "hy-image-v3" || strings.HasPrefix(m, "hy-image-v3-"):
		return tencentImageV3
	default:
		return tencentImageUnknown
	}
}

func (f tencentImageFamily) defaultPath() string {
	if f == tencentImageV35 {
		return tencentImageV35Path
	}
	return tencentImageV3Path
}

// tencentImageGenerationFields are the unified image generation fields this provider delivers
// (contract C4). size, seed and input_images reach TokenHub unchanged; n and response_format
// are taken only at the one value TokenHub produces (validateTencentImageParams). Any other
// unified field is refused by core with caller_unsupported_parameter before a key is chosen.
var tencentImageGenerationFields = []string{"n", "size", "seed", "input_images", "response_format"}

// newTencentModelUnknownError refuses a model this provider has no API shape for. TokenHub's two
// image endpoints take different bodies, so guessing one would send a malformed request.
func newTencentModelUnknownError(model string) *schemas.BifrostError {
	bifrostErr := providerUtils.NewBifrostBadRequestError(fmt.Sprintf(
		"tencent: model %q is not a TokenHub Hy image model (expected hy-image-v3 or hy-image-v3.5-*)", model))
	bifrostErr.ExtraFields.ErrorType = schemas.ErrorTypeCallerModelUnknown
	return bifrostErr
}

// validateTencentImageParams refuses values TokenHub cannot produce. TokenHub has no image count
// parameter and returns only image URLs, so n other than 1 and response_format other than url
// cannot be delivered; they are refused rather than dropped (contract C4 agreement three).
func validateTencentImageParams(params *schemas.ImageGenerationParameters) *schemas.BifrostError {
	if params == nil {
		return nil
	}
	if bifrostErr := rejectTencentBodyKeysInExtraParams(params.ExtraParams); bifrostErr != nil {
		return bifrostErr
	}
	if params.N != nil && *params.N != 1 {
		return schemas.NewUnsupportedParameterError("n", fmt.Sprintf(
			"tencent TokenHub generates one image per request; n=%d cannot be delivered", *params.N))
	}
	if params.ResponseFormat != nil && *params.ResponseFormat != "" && *params.ResponseFormat != "url" {
		return schemas.NewUnsupportedParameterError("response_format", fmt.Sprintf(
			"tencent TokenHub returns image URLs only; response_format %q cannot be delivered", *params.ResponseFormat))
	}
	return nil
}

// tencentConvertedBodyKeys are the body fields the converter writes. With
// x-bf-passthrough-extra-params an extra param of the same name would overwrite the converted
// value after governance and pricing have read the request: an extra model would run a model the
// caller was not checked for, and an extra size would change the price tier unseen.
var tencentConvertedBodyKeys = []string{"model", "prompt", "messages", "images", "size", "seed"}

// rejectTencentBodyKeysInExtraParams refuses an extra param that names a converted body field,
// in any letter case: a differently cased key would sit beside the converter's, and a decoder
// that folds case could read either.
func rejectTencentBodyKeysInExtraParams(extraParams map[string]interface{}) *schemas.BifrostError {
	for name := range extraParams {
		for _, key := range tencentConvertedBodyKeys {
			if strings.EqualFold(name, key) {
				bifrostErr := providerUtils.NewBifrostBadRequestError(fmt.Sprintf(
					"tencent: extra params cannot set %q; use the request's own %s field", name, key))
				bifrostErr.Error.Param = name
				bifrostErr.ExtraFields.ErrorType = schemas.ErrorTypeCallerInvalidRequest
				return bifrostErr
			}
		}
	}
	return nil
}

// ToTencentImageGenerationRequest converts a unified image generation request to the body of the
// model's TokenHub endpoint.
func ToTencentImageGenerationRequest(bifrostReq *schemas.BifrostImageGenerationRequest) (providerUtils.RequestBodyWithExtraParams, error) {
	if bifrostReq == nil || bifrostReq.Input == nil || strings.TrimSpace(bifrostReq.Input.Prompt) == "" {
		return nil, providerUtils.InvalidRequestErrorf("prompt is required")
	}
	var size *string
	var seed *int
	var inputImages []string
	var extra map[string]interface{}
	if params := bifrostReq.Params; params != nil {
		if params.Size != nil && *params.Size != "" {
			size = params.Size
		}
		seed = params.Seed
		inputImages = params.InputImages
		if len(params.ExtraParams) > 0 {
			extra = maps.Clone(params.ExtraParams)
		}
	}

	switch tencentImageFamilyOf(bifrostReq.Model) {
	case tencentImageV35:
		content := make([]TencentImageContent, 0, 1+len(inputImages))
		content = append(content, TencentImageContent{Type: "text", Text: bifrostReq.Input.Prompt})
		for _, image := range inputImages {
			content = append(content, TencentImageContent{Type: "image_url", ImageURL: &TencentImageURLPart{URL: image}})
		}
		return &TencentImageV35Request{
			Model:       bifrostReq.Model,
			Messages:    []TencentImageMessage{{Role: "user", Content: content}},
			Size:        size,
			Seed:        seed,
			ExtraParams: extra,
		}, nil
	case tencentImageV3:
		return &TencentImageV3Request{
			Model:       bifrostReq.Model,
			Prompt:      bifrostReq.Input.Prompt,
			Images:      inputImages,
			Size:        size,
			Seed:        seed,
			ExtraParams: extra,
		}, nil
	default:
		return nil, providerUtils.InvalidRequestErrorf("tencent: model %q is not a TokenHub Hy image model", bifrostReq.Model)
	}
}

// ToBifrostImageGenerationResponse converts a TokenHub image response. A response that carries an
// error object (v3.5 reports a failure as finish_reason "error" on a 200) becomes an error, and a
// 200 with neither an image nor an error becomes provider_empty_result (contract C6).
//
// Usage holds only what TokenHub reported. TokenHub reports one quantity, total_tokens, which is
// the image-token count the price book bills per image (Hy-Image-3.0: 20,000; 3.5 Preview: 15,000
// at 1K or 2K, 20,000 at 4K); the documentation says v3.5 does not split prompt and completion.
// It is written as total_tokens and as the output image tokens. output_images is the number of
// images in TokenHub's body. Nothing is filled from the request or a default.
func ToBifrostImageGenerationResponse(resp *TencentImageResponse, model string) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	if resp == nil {
		return nil, providerUtils.NewBifrostOperationError("tencent image generation response is nil", nil)
	}
	if bifrostErr := tencentResponseError(resp); bifrostErr != nil {
		return nil, bifrostErr
	}

	data := make([]schemas.ImageData, 0, len(resp.Data)+len(resp.Choices))
	var size string
	for _, image := range resp.Data {
		if image.URL == "" {
			continue
		}
		data = append(data, schemas.ImageData{URL: image.URL, RevisedPrompt: image.RevisedPrompt, Index: len(data)})
	}
	for _, choice := range resp.Choices {
		image := choice.Delta.Image
		if image == nil || image.URL == "" {
			continue
		}
		data = append(data, schemas.ImageData{URL: image.URL, Index: len(data)})
		if image.Width > 0 && image.Height > 0 {
			size = fmt.Sprintf("%dx%d", image.Width, image.Height)
		}
	}
	if len(data) == 0 {
		bifrostErr := providerUtils.NewProviderAPIError("tencent returned 200 with no image and no error", nil, 502, schemas.Ptr("server_error"), nil)
		bifrostErr.ExtraFields.ErrorType = schemas.ErrorTypeProviderEmptyResult
		return nil, bifrostErr
	}

	bifrostResp := &schemas.BifrostImageGenerationResponse{
		ID:      resp.ID,
		Created: resp.Created,
		Model:   model,
		Data:    data,
		Usage:   tencentImageUsage(resp, len(data)),
	}
	if size != "" {
		bifrostResp.ImageGenerationResponseParameters = &schemas.ImageGenerationResponseParameters{Size: size}
	}
	return bifrostResp, nil
}

// tencentImageUsage builds the unified usage from TokenHub's report. tokenhub_usage is TokenHub's
// billing count; v3.5 also echoes it as usage, which is read only when tokenhub_usage is absent.
func tencentImageUsage(resp *TencentImageResponse, outputImages int) *schemas.ImageUsage {
	usage := &schemas.ImageUsage{OutputImages: schemas.Ptr(outputImages)}
	reported := resp.TokenhubUsage
	if reported == nil || reported.TotalTokens == nil {
		reported = resp.Usage
	}
	if reported != nil && reported.TotalTokens != nil {
		tokens := *reported.TotalTokens
		usage.TotalTokens = tokens
		usage.OutputTokens = tokens
		usage.OutputTokensDetails = &schemas.ImageTokenDetails{ImageTokens: tokens}
	}
	return usage
}
