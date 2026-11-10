package byteplus

import (
	"fmt"
	"maps"
	"strings"
	"time"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
)

// seedreamMaxImages is the most images one Seedream sequential generation returns, counting reference images.
const seedreamMaxImages = 15

// ToBytePlusImageGenerationRequest converts a Bifrost image generation request to the Seedream /images/generations body.
func ToBytePlusImageGenerationRequest(bifrostReq *schemas.BifrostImageGenerationRequest) (*BytePlusImageGenerationRequest, error) {
	if bifrostReq == nil || bifrostReq.Input == nil || strings.TrimSpace(bifrostReq.Input.Prompt) == "" {
		return nil, providerUtils.InvalidRequestErrorf("prompt is required")
	}
	req := &BytePlusImageGenerationRequest{
		Model:  bifrostReq.Model,
		Prompt: bifrostReq.Input.Prompt,
	}
	params := bifrostReq.Params
	if params == nil {
		return req, nil
	}
	if fieldErr := ValidateBytePlusImageParams(params, nil); fieldErr != nil {
		return nil, providerUtils.InvalidRequestErrorf("%s", fieldErr.Error.Message)
	}
	req.Seed = params.Seed
	req.ResponseFormat = toBytePlusImageResponseFormat(params.ResponseFormat)
	req.OutputFormat = params.OutputFormat
	req.Background = params.Background
	for _, image := range params.InputImages {
		url, err := schemas.SanitizeImageURL(image)
		if err != nil {
			return nil, providerUtils.InvalidRequestErrorf("invalid input image: %v", err)
		}
		req.Image = append(req.Image, url)
	}
	if len(params.ExtraParams) > 0 {
		req.ExtraParams = make(map[string]interface{}, len(params.ExtraParams))
		maps.Copy(req.ExtraParams, params.ExtraParams)
		liftBytePlusImageExtraParams(req)
	}
	req.Size = bytePlusImageSize(params.Size, string(params.Resolution), req.ExtraParams)
	return req, nil
}

// ToBytePlusImageEditRequest converts a Bifrost image edit request to the same Seedream endpoint, which edits when reference images are present.
func ToBytePlusImageEditRequest(bifrostReq *schemas.BifrostImageEditRequest) (*BytePlusImageGenerationRequest, error) {
	if bifrostReq == nil || bifrostReq.Input == nil || strings.TrimSpace(bifrostReq.Input.Prompt) == "" {
		return nil, providerUtils.InvalidRequestErrorf("prompt is required")
	}
	if len(bifrostReq.Input.Images) == 0 {
		return nil, providerUtils.InvalidRequestErrorf("at least one image is required")
	}
	req := &BytePlusImageGenerationRequest{
		Model:  bifrostReq.Model,
		Prompt: bifrostReq.Input.Prompt,
		Image:  make([]string, 0, len(bifrostReq.Input.Images)),
	}
	for _, image := range bifrostReq.Input.Images {
		switch {
		case len(image.Image) > 0:
			req.Image = append(req.Image, providerUtils.FileBytesToBase64DataURL(image.Image))
		case image.URL != "":
			url, err := schemas.SanitizeImageURL(image.URL)
			if err != nil {
				return nil, providerUtils.InvalidRequestErrorf("invalid input image: %v", err)
			}
			req.Image = append(req.Image, url)
		}
	}
	if len(req.Image) == 0 {
		return nil, providerUtils.InvalidRequestErrorf("at least one image is required")
	}
	params := bifrostReq.Params
	if params == nil {
		return req, nil
	}
	// The entry points refuse these first with error.param set; this keeps a direct caller of
	// the converter from sending a request that drops a field.
	if fieldErr := ValidateBytePlusImageParams(nil, params); fieldErr != nil {
		return nil, providerUtils.InvalidRequestErrorf("%s", fieldErr.Error.Message)
	}
	req.Seed = params.Seed
	req.ResponseFormat = toBytePlusImageResponseFormat(params.ResponseFormat)
	req.OutputFormat = params.OutputFormat
	req.Background = params.Background
	if len(params.ExtraParams) > 0 {
		req.ExtraParams = make(map[string]interface{}, len(params.ExtraParams))
		maps.Copy(req.ExtraParams, params.ExtraParams)
		liftBytePlusImageExtraParams(req)
	}
	req.Size = bytePlusImageSize(params.Size, string(params.Resolution), req.ExtraParams)
	return req, nil
}

func toBytePlusImageResponseFormat(format *string) *string {
	if format == nil {
		return nil
	}
	switch *format {
	case "b64_json", "base64":
		return schemas.Ptr("b64_json")
	case "url":
		return schemas.Ptr("url")
	}
	return nil
}

// bytePlusImageSize is the Seedream size for a request: the unified resolution tier ("1K",
// "2K", "4K"), which Seedream takes in size, or the caller's size. "auto" leaves the size to
// Seedream. ValidateBytePlusImageParams has already refused a request that sets both. The
// resolution extra param the HTTP layer keeps beside the typed field (TXE-6518) is consumed
// here so it does not also reach Ark as an unknown field.
func bytePlusImageSize(size *string, resolution string, extraParams map[string]interface{}) *string {
	if extraResolution, ok := extraParams["resolution"].(string); ok {
		delete(extraParams, "resolution")
		if resolution == "" {
			resolution = extraResolution
		}
	}
	if resolution != "" {
		// A copy, so only a request that sets a resolution pays the heap allocation; taking the
		// parameter's address would move it to the heap on every call.
		tier := resolution
		return &tier
	}
	if size != nil && *size != "" && *size != "auto" {
		return size
	}
	return nil
}

// ValidateBytePlusImageParams refuses, before any upstream call, a unified image field that
// Seedream cannot take as given (contract C4): 400 caller_unsupported_parameter naming the
// field in error.param. Seedream has no quality, aspect ratio, negative prompt, inference
// step count or mask, and no way to ask for exactly n images: its sequential generation
// returns up to max_images, as many as the model decides, so n>1 is refused rather than
// turned into "up to n". A caller that wants a Seedream group generation asks for it in
// Seedream's own fields (sequential_image_generation and its options) as extra params.
// The generation and edit parameters are passed as one or the other; nil passes.
func ValidateBytePlusImageParams(gen *schemas.ImageGenerationParameters, edit *schemas.ImageEditParameters) *schemas.BifrostError {
	var (
		quality, aspectRatio, negativePrompt, size *string
		resolution                                 string
		steps, n                                   *int
		mask                                       []byte
	)
	switch {
	case gen != nil:
		quality, aspectRatio, negativePrompt = gen.Quality, gen.AspectRatio, gen.NegativePrompt
		steps, n, size, resolution = gen.NumInferenceSteps, gen.N, gen.Size, string(gen.Resolution)
	case edit != nil:
		quality, aspectRatio, negativePrompt = edit.Quality, edit.AspectRatio, edit.NegativePrompt
		steps, n, size, resolution = edit.NumInferenceSteps, edit.N, edit.Size, string(edit.Resolution)
		mask = edit.Mask
	default:
		return nil
	}
	notTaken := func(param string) *schemas.BifrostError {
		return schemas.NewUnsupportedParameterError(param, fmt.Sprintf("%s is not supported by byteplus seedream images", param))
	}
	switch {
	case quality != nil && *quality != "":
		return notTaken("quality")
	case aspectRatio != nil && *aspectRatio != "":
		return notTaken("aspect_ratio")
	case negativePrompt != nil && *negativePrompt != "":
		return notTaken("negative_prompt")
	case steps != nil:
		return notTaken("num_inference_steps")
	case len(mask) > 0:
		return notTaken("mask")
	}
	if n != nil {
		switch {
		case *n < 1 || *n > seedreamMaxImages:
			return schemas.NewUnsupportedParameterError("n", fmt.Sprintf("n must be between 1 and %d for byteplus seedream images, got %d", seedreamMaxImages, *n))
		case *n > 1:
			return schemas.NewUnsupportedParameterError("n", "byteplus seedream cannot generate an exact number of images; n must be 1 (use sequential_image_generation for a group of up to max_images)")
		}
	}
	if resolution != "" && size != nil && *size != "" && *size != "auto" {
		return schemas.NewUnsupportedParameterError("resolution", "byteplus seedream takes one of size and resolution, not both")
	}
	return nil
}

// liftBytePlusImageExtraParams moves the Seedream fields Bifrost has typed slots for out of the passthrough map.
func liftBytePlusImageExtraParams(req *BytePlusImageGenerationRequest) {
	if v, ok := req.ExtraParams["watermark"].(bool); ok {
		req.Watermark = &v
		delete(req.ExtraParams, "watermark")
	}
	if v, ok := req.ExtraParams["sequential_image_generation"].(string); ok && v != "" {
		req.SequentialImageGeneration = &v
		delete(req.ExtraParams, "sequential_image_generation")
	}
	if raw, ok := req.ExtraParams["sequential_image_generation_options"]; ok {
		// A value that does not decode stays in the passthrough map for Ark to judge.
		if opts, err := schemas.ConvertViaJSON[BytePlusSequentialImageOptions](raw); err == nil && opts.MaxImages > 0 {
			req.SequentialImageGenerationOptions = &opts
			delete(req.ExtraParams, "sequential_image_generation_options")
		}
	}
}

// ToBifrostImageGenerationResponse converts a Seedream response to Bifrost format. An image
// Seedream refused carries an error instead of a payload: it takes no slot in Data and is
// counted in ContentFilter with its vendor code (contract C6). A response whose every image
// was refused is still a 200 with an empty Data. InternalServiceError, after which Ark stops
// generating, is Ark's own failure rather than a refusal of the caller's content, so it is
// not counted; a response with no image delivered and none refused is an error.
func ToBifrostImageGenerationResponse(resp *BytePlusImageGenerationResponse, model string) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	if resp == nil {
		return nil, providerUtils.NewBifrostOperationError("image generation response is nil", nil)
	}
	if resp.Error != nil && resp.Error.Message != "" {
		return nil, bytePlusVendorErrorObject(resp.Error)
	}
	data := make([]schemas.ImageData, 0, len(resp.Data))
	var filter *schemas.ContentFilterInfo
	var internalErr *BytePlusError
	for _, image := range resp.Data {
		if image.URL != "" || image.B64JSON != "" {
			data = append(data, schemas.ImageData{URL: image.URL, B64JSON: image.B64JSON, Index: len(data)})
			continue
		}
		if image.Error != nil && image.Error.Code == bytePlusInternalServiceError {
			if internalErr == nil {
				internalErr = image.Error
			}
			continue
		}
		if image.Error == nil {
			continue
		}
		if filter == nil {
			filter = &schemas.ContentFilterInfo{}
		}
		filter.FilteredCount++
		if image.Error.Code != "" {
			filter.Reasons = append(filter.Reasons, image.Error.Code)
		}
	}
	if len(data) == 0 && filter == nil {
		if internalErr != nil {
			return nil, bytePlusVendorErrorObject(internalErr)
		}
		return nil, schemas.NewProviderEmptyResultError("byteplus returned no images and no image errors")
	}
	created := resp.Created
	if created == 0 {
		created = resp.CreatedAt
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	if resp.Model != "" {
		model = resp.Model
	}
	bifrostResp := &schemas.BifrostImageGenerationResponse{
		Created:       created,
		Model:         model,
		Data:          data,
		ContentFilter: filter,
	}
	bifrostResp.Usage = ToBifrostImageUsage(resp.Usage)
	return bifrostResp, nil
}

// bytePlusVendorErrorObject is the error for an error object Ark returned inside a 200
// (contract C6): a content refusal is the caller's, InternalServiceError is Ark's, and any
// other code is taken as the caller's parameters. error.code is Ark's code verbatim.
func bytePlusVendorErrorObject(failure *BytePlusError) *schemas.BifrostError {
	cause := schemas.VendorErrorCauseCaller
	switch {
	case isBytePlusContentFilterCode(failure.Code):
		cause = schemas.VendorErrorCauseContentFiltered
	case failure.Code == bytePlusInternalServiceError:
		cause = schemas.VendorErrorCauseProvider
	}
	message := failure.Message
	if message == "" {
		message = "byteplus returned an error: " + failure.Code
	}
	return schemas.NewVendorErrorObjectError(cause, failure.Code, message)
}

// isBytePlusContentFilterCode reports whether an Ark error code is a moderation refusal of
// the caller's content. Ark names every such code after the content it found sensitive:
// InputTextSensitiveContentDetected, InputImageSensitiveContentDetected,
// OutputImageSensitiveContentDetected, SensitiveContentDetected and its dotted sub-codes.
func isBytePlusContentFilterCode(code string) bool {
	return strings.Contains(code, "SensitiveContentDetected")
}

// MarkBytePlusContentFilterError declares a vendor HTTP error whose Ark code is a moderation
// refusal as caller_content_filtered (contract C6), keeping the vendor's status code. It
// returns err for chaining; nil and other errors pass through unchanged.
func MarkBytePlusContentFilterError(err *schemas.BifrostError) *schemas.BifrostError {
	if err == nil || err.IsBifrostError || err.Error == nil || err.Error.Code == nil {
		return err
	}
	if isBytePlusContentFilterCode(*err.Error.Code) {
		err.ExtraFields.ErrorType = schemas.ErrorTypeCallerContentFiltered
	}
	return err
}

// ToBifrostImageUsage converts Seedream usage to Bifrost image usage. generated_images is the
// vendor's own count of images delivered, so it is reported as usage.output_images (C5).
func ToBifrostImageUsage(usage *BytePlusImageUsage) *schemas.ImageUsage {
	if usage == nil {
		return nil
	}
	return &schemas.ImageUsage{
		OutputTokens: usage.OutputTokens,
		TotalTokens:  usage.TotalTokens,
		OutputImages: schemas.Ptr(usage.GeneratedImages),
		OutputTokensDetails: &schemas.ImageTokenDetails{
			NImages:     usage.GeneratedImages,
			ImageTokens: usage.OutputTokens,
		},
	}
}

// bytePlusImageStreamError builds the error for a failed stream event. A per-image failure is a moderation
// refusal unless Ark reports an internal error, the only per-image failure that aborts the stream; an error
// event without an image index rejects the whole request.
func bytePlusImageStreamError(failure *BytePlusError, perImage bool) *schemas.BifrostError {
	message := "byteplus returned no images"
	var code string
	if failure != nil {
		code = failure.Code
		if failure.Message != "" {
			message = failure.Message
		}
	}
	status, errType := 400, "invalid_request_error"
	if perImage {
		errType = "content_filter"
	}
	if code == bytePlusInternalServiceError {
		status, errType = 500, "server_error"
	}
	bifrostErr := providerUtils.NewProviderAPIError(message, nil, status, schemas.Ptr(errType), nil)
	if code != "" {
		bifrostErr.Error.Code = schemas.Ptr(code)
	}
	if perImage && status == 400 {
		// Every image of the stream was refused: the stream has no response to carry a
		// content_filter, so the refusal is the error (contract C6).
		bifrostErr.ExtraFields.ErrorType = schemas.ErrorTypeCallerContentFiltered
	}
	return bifrostErr
}

// bytePlusInternalServiceError is the per-image error code after which Ark stops generating.
const bytePlusInternalServiceError = "InternalServiceError"
