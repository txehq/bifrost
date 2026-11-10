package gemini

import (
	"context"
	"encoding/base64"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
)

// ToBifrostImageGenerationRequest converts a Gemini generation request to a Bifrost image generation request
func (request *GeminiGenerationRequest) ToBifrostImageGenerationRequest(ctx *schemas.BifrostContext) *schemas.BifrostImageGenerationRequest {
	if request == nil {
		return nil
	}

	// Parse provider from model string (e.g., "openai/gpt-image-1" -> provider="openai", model="gpt-image-1")
	// This allows cross-provider routing through the GenAI endpoint
	provider, model := schemas.ParseModelString(request.Model, "")

	bifrostReq := &schemas.BifrostImageGenerationRequest{
		Provider: provider,
		Model:    model,
		Input:    &schemas.ImageGenerationInput{},
		Params:   &schemas.ImageGenerationParameters{},
	}

	fallbacks := schemas.ParseFallbacks(request.Fallbacks)
	bifrostReq.Fallbacks = fallbacks

	// First, try to extract prompt from Imagen format (instances)
	if len(request.Instances) > 0 && request.Instances[0].Prompt != "" {
		bifrostReq.Input.Prompt = request.Instances[0].Prompt

		// Extract Imagen parameters
		if request.Parameters != nil {
			if request.Parameters.SampleCount != nil {
				bifrostReq.Params.N = request.Parameters.SampleCount
			}
			// Convert Imagen size format to standard format
			if request.Parameters.SampleImageSize != nil || request.Parameters.AspectRatio != nil {
				size := convertImagenFormatToSize(request.Parameters.SampleImageSize, request.Parameters.AspectRatio)
				if size != "" && strings.ToLower(size) != "auto" {
					bifrostReq.Params.Size = &size
				}
			}

			// Map additional parameters to ExtraParams if not in Bifrost schema
			if bifrostReq.Params.ExtraParams == nil {
				bifrostReq.Params.ExtraParams = make(map[string]interface{})
			}

			if request.Parameters.PersonGeneration != nil {
				bifrostReq.Params.ExtraParams["personGeneration"] = *request.Parameters.PersonGeneration
			}
			if request.Parameters.Seed != nil {
				bifrostReq.Params.Seed = request.Parameters.Seed
			}
			if request.Parameters.NegativePrompt != nil {
				bifrostReq.Params.NegativePrompt = request.Parameters.NegativePrompt
			}
			if request.Parameters.Language != nil {
				bifrostReq.Params.ExtraParams["language"] = *request.Parameters.Language
			}
			if request.Parameters.EnhancePrompt != nil {
				bifrostReq.Params.ExtraParams["enhancePrompt"] = *request.Parameters.EnhancePrompt
			}
			if request.Parameters.AddWatermark != nil {
				bifrostReq.Params.ExtraParams["addWatermark"] = *request.Parameters.AddWatermark
			}
			if len(request.Parameters.SafetySettings) > 0 {
				bifrostReq.Params.ExtraParams["safetySettings"] = request.Parameters.SafetySettings
			}
		}
		return bifrostReq
	}

	// Fall back to standard Gemini format (contents)
	if len(request.Contents) > 0 {
		for _, content := range request.Contents {
			for _, part := range content.Parts {
				if part != nil && part.Text != "" {
					bifrostReq.Input.Prompt = part.Text
					break
				}
			}
			if bifrostReq.Input.Prompt != "" {
				break
			}
		}
	}

	if request.GenerationConfig.ImageConfig != nil {
		ic := request.GenerationConfig.ImageConfig
		if strings.TrimSpace(ic.ImageSize) != "" || strings.TrimSpace(ic.AspectRatio) != "" {
			size := convertImagenFormatToSize(&ic.ImageSize, &ic.AspectRatio)
			if size != "" {
				bifrostReq.Params.Size = &size
			}
		}
		if aspectRatio := strings.TrimSpace(ic.AspectRatio); aspectRatio != "" {
			bifrostReq.Params.AspectRatio = &aspectRatio
		}
	}

	return bifrostReq
}

func (request *GeminiGenerationRequest) ToBifrostImageEditRequest(ctx *schemas.BifrostContext) *schemas.BifrostImageEditRequest {
	if request == nil {
		return nil
	}

	// Parse provider from model string (e.g., "openai/gpt-image-1" -> provider="openai", model="gpt-image-1")
	// This allows cross-provider routing through the GenAI endpoint
	provider, model := schemas.ParseModelString(request.Model, "")

	bifrostReq := &schemas.BifrostImageEditRequest{
		Provider: provider,
		Model:    model,
		Input:    &schemas.ImageEditInput{},
		Params:   &schemas.ImageEditParameters{},
	}

	fallbacks := schemas.ParseFallbacks(request.Fallbacks)
	bifrostReq.Fallbacks = fallbacks

	// Initialize ExtraParams if not present
	if bifrostReq.Params.ExtraParams == nil {
		bifrostReq.Params.ExtraParams = make(map[string]interface{})
	}

	// First, try to extract prompt from Imagen format (instances)
	if len(request.Instances) > 0 && request.Instances[0].Prompt != "" {
		bifrostReq.Input.Prompt = request.Instances[0].Prompt

		// Extract all images from ReferenceImages using a loop
		var images []schemas.ImageInput
		var mask []byte
		var maskMode string
		var dilation *float64
		var maskClasses []int

		for _, refImage := range request.Instances[0].ReferenceImages {
			if refImage.ReferenceType == "REFERENCE_TYPE_RAW" {
				// Decode base64 image data
				imageBytes, err := base64.StdEncoding.DecodeString(refImage.ReferenceImage.BytesBase64Encoded)
				if err != nil {
					continue // Skip invalid images
				}
				images = append(images, schemas.ImageInput{
					Image: imageBytes,
				})
			} else if refImage.ReferenceType == "REFERENCE_TYPE_MASK" {
				// Extract mask data if present
				if refImage.ReferenceImage.BytesBase64Encoded != "" {
					maskBytes, err := base64.StdEncoding.DecodeString(refImage.ReferenceImage.BytesBase64Encoded)
					if err == nil {
						mask = maskBytes
					}
				}
				// Extract mask configuration
				if refImage.MaskImageConfig != nil {
					if refImage.MaskImageConfig.MaskMode != "" {
						maskMode = refImage.MaskImageConfig.MaskMode
					}
					if refImage.MaskImageConfig.Dilation != nil {
						dilation = refImage.MaskImageConfig.Dilation
					}
					if len(refImage.MaskImageConfig.MaskClasses) > 0 {
						maskClasses = refImage.MaskImageConfig.MaskClasses
					}

				}
			}
		}

		// Set mask if present
		if len(mask) > 0 {
			bifrostReq.Params.Mask = mask
		}

		// Store mask configuration in ExtraParams
		if maskMode != "" {
			bifrostReq.Params.ExtraParams["maskMode"] = maskMode
		}
		if dilation != nil {
			bifrostReq.Params.ExtraParams["dilation"] = *dilation
		}
		if len(maskClasses) > 0 {
			bifrostReq.Params.ExtraParams["maskClasses"] = maskClasses
		}

		if len(images) == 0 {
			return nil // No valid images found
		}
		bifrostReq.Input.Images = images

		// Extract Imagen parameters
		if request.Parameters != nil {
			if request.Parameters.SampleCount != nil {
				bifrostReq.Params.N = request.Parameters.SampleCount
			}
			// Convert Imagen size format to standard format
			if request.Parameters.SampleImageSize != nil || request.Parameters.AspectRatio != nil {
				size := convertImagenFormatToSize(request.Parameters.SampleImageSize, request.Parameters.AspectRatio)
				if size != "" && strings.ToLower(size) != "auto" {
					bifrostReq.Params.Size = &size
				}
			}

			// Extract output format and compression from OutputOptions
			if request.Parameters.OutputOptions != nil {
				if request.Parameters.OutputOptions.MimeType != nil {
					outputFormat := convertMimeTypeToExtension(*request.Parameters.OutputOptions.MimeType)
					if outputFormat != "" {
						bifrostReq.Params.OutputFormat = &outputFormat
					}
				}
				if request.Parameters.OutputOptions.CompressionQuality != nil {
					bifrostReq.Params.OutputCompression = request.Parameters.OutputOptions.CompressionQuality
				}
			}

			// Extract edit mode and map to type
			if request.Parameters.EditMode != nil {
				editType := mapImagenEditModeToType(*request.Parameters.EditMode)
				if editType != "" {
					bifrostReq.Params.Type = &editType
				}
			}

			if request.Parameters.Seed != nil {
				bifrostReq.Params.Seed = request.Parameters.Seed
			}
			if request.Parameters.NegativePrompt != nil {
				bifrostReq.Params.NegativePrompt = request.Parameters.NegativePrompt
			}

			if request.Parameters.PersonGeneration != nil {
				bifrostReq.Params.ExtraParams["personGeneration"] = *request.Parameters.PersonGeneration
			}
			if request.Parameters.Language != nil {
				bifrostReq.Params.ExtraParams["language"] = *request.Parameters.Language
			}
			if request.Parameters.EnhancePrompt != nil {
				bifrostReq.Params.ExtraParams["enhancePrompt"] = *request.Parameters.EnhancePrompt
			}
			if request.Parameters.AddWatermark != nil {
				bifrostReq.Params.ExtraParams["addWatermark"] = *request.Parameters.AddWatermark
			}
			if len(request.Parameters.SafetySettings) > 0 {
				bifrostReq.Params.ExtraParams["safetySettings"] = request.Parameters.SafetySettings
			}
			if request.Parameters.GuidanceScale != nil {
				bifrostReq.Params.ExtraParams["guidanceScale"] = *request.Parameters.GuidanceScale
			}
			if request.Parameters.BaseSteps != nil {
				bifrostReq.Params.ExtraParams["baseSteps"] = *request.Parameters.BaseSteps
			}
			if request.Parameters.IncludeRaiReason != nil {
				bifrostReq.Params.ExtraParams["includeRaiReason"] = *request.Parameters.IncludeRaiReason
			}
			if request.Parameters.IncludeSafetyAttributes != nil {
				bifrostReq.Params.ExtraParams["includeSafetyAttributes"] = *request.Parameters.IncludeSafetyAttributes
			}
			if request.Parameters.StorageUri != nil {
				bifrostReq.Params.ExtraParams["storageUri"] = *request.Parameters.StorageUri
			}
		}
		return bifrostReq
	}

	// Fall back to standard Gemini format (contents)
	if len(request.Contents) > 0 {
		var images []schemas.ImageInput
		for _, content := range request.Contents {
			for _, part := range content.Parts {
				if part != nil {
					if part.Text != "" {
						bifrostReq.Input.Prompt = part.Text
					}
					// Extract images from InlineData
					if part.InlineData != nil && part.InlineData.Data != "" {
						imageBytes, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
						if err == nil {
							images = append(images, schemas.ImageInput{
								Image: imageBytes,
							})
						}
					}
				}
			}
		}
		if len(images) > 0 {
			bifrostReq.Input.Images = images
		}
	}

	if request.GenerationConfig.ImageConfig != nil {
		ic := request.GenerationConfig.ImageConfig
		if strings.TrimSpace(ic.ImageSize) != "" || strings.TrimSpace(ic.AspectRatio) != "" {
			size := convertImagenFormatToSize(&ic.ImageSize, &ic.AspectRatio)
			if size != "" {
				bifrostReq.Params.Size = &size
			}
		}
		if aspectRatio := strings.TrimSpace(ic.AspectRatio); aspectRatio != "" {
			bifrostReq.Params.AspectRatio = &aspectRatio
		}
	}

	return bifrostReq
}

// convertImagenFormatToSize converts Imagen sampleImageSize and aspectRatio to standard WxH format
// Supports aspect ratios: "1:1", "3:4", "4:3", "9:16", "16:9" (supported for Imagen models)
func convertImagenFormatToSize(sampleImageSize *string, aspectRatio *string) string {
	// Default size based on imageSize parameter
	baseSize := 1024
	if sampleImageSize != nil {
		normalizedSize := strings.ToLower(strings.TrimSpace(*sampleImageSize))
		switch normalizedSize {
		case "1k":
			baseSize = 1024
		case "2k":
			baseSize = 2048
		case "4k":
			baseSize = 4096
		}
	}

	// Apply aspect ratio
	if aspectRatio != nil {
		switch strings.TrimSpace(*aspectRatio) {
		case "1:1":
			return strconv.Itoa(baseSize) + "x" + strconv.Itoa(baseSize)
		case "3:4":
			return strconv.Itoa(baseSize*3/4) + "x" + strconv.Itoa(baseSize)
		case "4:3":
			return strconv.Itoa(baseSize) + "x" + strconv.Itoa(baseSize*3/4)
		case "9:16":
			return strconv.Itoa(baseSize*9/16) + "x" + strconv.Itoa(baseSize)
		case "16:9":
			return strconv.Itoa(baseSize) + "x" + strconv.Itoa(baseSize*9/16)
		}
	}

	// Default to square
	return strconv.Itoa(baseSize) + "x" + strconv.Itoa(baseSize)
}

// ToBifrostImageGenerationResponse converts a generateContent response of an image model.
//
// Fork (TXE-6526, contract C5 and C6). A vendor 200 is one of three things:
//   - images came: they are Data, and usage.output_images is their count;
//   - no image came and the vendor marked a block (promptFeedback.blockReason, a safety
//     finishReason, or a candidate that answered in text only): a 200 with "data": [] and
//     content_filter carrying the vendor's values verbatim (see geminiImageBlockReasons);
//   - no image came and nothing marks a block: 502 provider_empty_result, with the vendor's
//     finishReason as error.code and its finishMessage as the message when it sent them.
//
// Usage is the vendor's usageMetadata whenever it sent one, on a block too.
func (response *GenerateContentResponse) ToBifrostImageGenerationResponse() (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	bifrostResp := &schemas.BifrostImageGenerationResponse{
		ID:    response.ResponseID,
		Model: response.ModelVersion,
		Data:  []schemas.ImageData{},
	}

	// Extract image data from the parts of every candidate. Index is the part's index in its
	// candidate, as before.
	var firstOutputFormat string
	for _, candidate := range response.Candidates {
		if candidate == nil || candidate.Content == nil {
			continue
		}
		for idx, part := range candidate.Content.Parts {
			if part == nil || part.InlineData == nil {
				continue
			}
			if len(bifrostResp.Data) == 0 {
				// Convert MIME type to file extension for OutputFormat
				firstOutputFormat = convertMimeTypeToExtension(part.InlineData.MIMEType)
			}
			bifrostResp.Data = append(bifrostResp.Data, schemas.ImageData{
				B64JSON: part.InlineData.Data,
				Index:   idx,
			})
		}
	}

	// Set usage information with modality details
	bifrostResp.Usage = convertGeminiUsageMetadataToImageUsage(response.UsageMetadata)

	if len(bifrostResp.Data) > 0 {
		bifrostResp.ImageGenerationResponseParameters = &schemas.ImageGenerationResponseParameters{
			OutputFormat: firstOutputFormat,
		}
	} else {
		reasons, blocked := geminiImageBlockReasons(response)
		if !blocked {
			return nil, geminiImageEmptyResultError(response)
		}
		// One generateContent call asks for one image (n other than 1 is refused), so a block
		// filters exactly the one image requested.
		bifrostResp.ContentFilter = &schemas.ContentFilterInfo{FilteredCount: 1, Reasons: reasons}
	}

	// usage.output_images is the count of images the vendor delivered (0 on a block).
	if bifrostResp.Usage == nil {
		bifrostResp.Usage = &schemas.ImageUsage{}
	}
	bifrostResp.Usage.OutputImages = schemas.Ptr(len(bifrostResp.Data))

	return bifrostResp, nil
}

// geminiImageBlockFinishReasons are the finishReason values with which Gemini says it withheld
// the output for safety or policy. Any other finishReason on a candidate with no content (OTHER,
// NO_IMAGE, IMAGE_OTHER, MAX_TOKENS, ...) is not a block.
var geminiImageBlockFinishReasons = []FinishReason{
	FinishReasonSafety,
	FinishReasonRecitation,
	FinishReasonBlocklist,
	FinishReasonProhibitedContent,
	FinishReasonSPII,
	FinishReasonImageSafety,
	FinishReasonImageProhibitedContent,
	FinishReasonImageRecitation,
}

// geminiImageBlockReasons reports whether a generateContent response that delivered no image
// carries a block marker, and the vendor's values for content_filter.reasons, verbatim and in
// the order the vendor sent them: promptFeedback.blockReason; then, per candidate that is a
// marker, its finishReason and its finishMessage when present. A candidate is a marker when its
// finishReason is one of geminiImageBlockFinishReasons, or when it answered in text only (the
// model declined in prose; its finishReason is then usually STOP).
func geminiImageBlockReasons(response *GenerateContentResponse) (reasons []string, blocked bool) {
	if response.PromptFeedback != nil && response.PromptFeedback.BlockReason != "" {
		blocked = true
		reasons = append(reasons, response.PromptFeedback.BlockReason)
	}
	for _, candidate := range response.Candidates {
		if candidate == nil {
			continue
		}
		if !slices.Contains(geminiImageBlockFinishReasons, candidate.FinishReason) && !geminiCandidateHasText(candidate) {
			continue
		}
		blocked = true
		if candidate.FinishReason != "" {
			reasons = append(reasons, string(candidate.FinishReason))
		}
		if candidate.FinishMessage != "" {
			reasons = append(reasons, candidate.FinishMessage)
		}
	}
	return reasons, blocked
}

// geminiCandidateHasText reports whether the candidate carries answer text (thought summaries
// are not an answer).
func geminiCandidateHasText(candidate *Candidate) bool {
	if candidate.Content == nil {
		return false
	}
	for _, part := range candidate.Content.Parts {
		if part != nil && part.Text != "" && !part.Thought {
			return true
		}
	}
	return false
}

// geminiImageEmptyResultError is the error for a vendor 200 with no image and no block marker:
// 502 provider_empty_result (contract C6). The first candidate's finishReason and finishMessage
// are kept as error.code and the message.
func geminiImageEmptyResultError(response *GenerateContentResponse) *schemas.BifrostError {
	var code, message string
	for _, candidate := range response.Candidates {
		if candidate != nil {
			code, message = string(candidate.FinishReason), candidate.FinishMessage
			break
		}
	}
	if message == "" {
		if len(response.Candidates) == 0 {
			message = "No candidates found in response"
		} else {
			message = "the provider returned a successful response with no image"
		}
	}
	bifrostErr := schemas.NewProviderEmptyResultError(message)
	if code != "" {
		bifrostErr.Error.Code = schemas.Ptr(code)
	}
	return bifrostErr
}

// geminiImageUnifiedFields are the unified image fields the Gemini API image models (Nano
// Banana, served by generateContent) take on generation and on edit: size and aspect_ratio
// become imageConfig.aspectRatio and imageSize, resolution becomes imageConfig.imageSize, and n
// and response_format are accepted only with the one value the vendor delivers (see
// checkGeminiImageValues). Every other typed parameter (quality, output_format, background,
// input_images, mask, input_fidelity, ...) cannot be delivered and is refused.
var geminiImageUnifiedFields = []string{"aspect_ratio", "n", "resolution", "response_format", "size"}

// CheckGeminiImageGenerationRequest refuses, with 400 caller_unsupported_parameter naming the
// field in error.param, an image generation request for a Gemini API image model that carries a
// field the vendor cannot be sent as given (fork, TXE-6526, contract C4). The gemini provider
// calls it before building the upstream request, so a refused request makes no upstream call.
// Imagen models take a different field set and are not checked here.
func CheckGeminiImageGenerationRequest(request *schemas.BifrostImageGenerationRequest) *schemas.BifrostError {
	if request == nil || request.Params == nil {
		return nil
	}
	if bifrostErr := schemas.CheckUnifiedFields(&schemas.BifrostRequest{ImageGenerationRequest: request}, geminiImageUnifiedFields); bifrostErr != nil {
		return bifrostErr
	}
	return checkGeminiImageValues(request.Params.N, request.Params.Size, request.Params.ResponseFormat)
}

// CheckGeminiImageEditRequest is CheckGeminiImageGenerationRequest for an image edit. It also
// refuses an input image given as a data URL that cannot be decoded: such an image cannot be
// sent, and dropping it would turn the edit into a different request.
func CheckGeminiImageEditRequest(request *schemas.BifrostImageEditRequest) *schemas.BifrostError {
	if request == nil {
		return nil
	}
	if request.Input != nil {
		for i := range request.Input.Images {
			image := &request.Input.Images[i]
			if len(image.Image) == 0 && strings.HasPrefix(image.URL, "data:") {
				if _, isBase64, _, ok := schemas.ParseDataURL(image.URL); !ok || !isBase64 {
					return schemas.NewUnsupportedParameterError("image", "an input image given as a data URL must be base64-encoded")
				}
			}
		}
	}
	if request.Params == nil {
		return nil
	}
	if bifrostErr := schemas.CheckUnifiedFields(&schemas.BifrostRequest{ImageEditRequest: request}, geminiImageUnifiedFields); bifrostErr != nil {
		return bifrostErr
	}
	return checkGeminiImageValues(request.Params.N, request.Params.Size, request.Params.ResponseFormat)
}

// checkGeminiImageValues refuses the values of supported fields that cannot be sent as given:
// n other than 1 (one generateContent call returns one image), a size from which no aspect
// ratio and resolution tier can be derived ("auto" leaves both to the vendor), and a
// response_format other than b64_json (the vendor returns image bytes, never a URL).
func checkGeminiImageValues(n *int, size *string, responseFormat *string) *schemas.BifrostError {
	if n != nil && *n != 1 {
		return schemas.NewUnsupportedParameterError("n", "this model returns one image per request; n must be 1")
	}
	if size != nil && strings.ToLower(*size) != "auto" {
		if aspectRatio, imageSize := utils.ConvertSizeToAspectRatioAndResolution(*size); aspectRatio == "" || imageSize == "" {
			return schemas.NewUnsupportedParameterError("size", fmt.Sprintf("size %q has no aspect ratio (1:1, 3:4, 4:3, 9:16, 16:9) and resolution tier (up to 4096 per edge) this model takes; send aspect_ratio and resolution instead", *size))
		}
	}
	if responseFormat != nil && *responseFormat != "b64_json" {
		return schemas.NewUnsupportedParameterError("response_format", "this model returns image bytes; response_format must be b64_json")
	}
	return nil
}

// geminiImageEditInputPart converts one input image of an edit to a request part: image bytes
// and a base64 data URL become inlineData; any other URL is sent unchanged as
// fileData{fileUri} (fork, TXE-6526, contract C4: Bifrost does not download an input URL),
// with the MIME type its extension names when it names one. Nil when the image carries nothing
// that can be sent.
func geminiImageEditInputPart(image *schemas.ImageInput) *Part {
	if len(image.Image) > 0 {
		// Detect MIME type from image bytes
		mimeType := http.DetectContentType(image.Image)
		// Fallback to PNG if detection fails
		if mimeType == "application/octet-stream" || mimeType == "" {
			mimeType = "image/png"
		}
		return &Part{InlineData: &Blob{MIMEType: mimeType, Data: encodeBytesToBase64String(image.Image)}}
	}
	if image.URL == "" {
		return nil
	}
	if strings.HasPrefix(image.URL, "data:") {
		mediaType, isBase64, payload, ok := schemas.ParseDataURL(image.URL)
		if !ok || !isBase64 {
			return nil
		}
		return &Part{InlineData: &Blob{MIMEType: mediaType, Data: payload}}
	}
	return &Part{FileData: &FileData{FileURI: image.URL, MIMEType: mimeTypeFromURI(image.URL)}}
}

// SetGeminiImageEditInputImages writes usage.input_images on an edit response: the number of
// input images that were sent to the vendor (fork, TXE-6526, contract C5; gemini-3-pro-image
// bills input images per image). Gemini reports input images only as tokens
// (promptTokensDetails[IMAGE]), so the count is of the image parts of the request.
func SetGeminiImageEditInputImages(response *schemas.BifrostImageGenerationResponse, request *schemas.BifrostImageEditRequest) {
	if response == nil || request == nil || request.Input == nil {
		return
	}
	count := 0
	for i := range request.Input.Images {
		if geminiImageEditInputSent(&request.Input.Images[i]) {
			count++
		}
	}
	if response.Usage == nil {
		response.Usage = &schemas.ImageUsage{}
	}
	response.Usage.InputImages = schemas.Ptr(count)
}

// geminiImageEditInputSent reports whether geminiImageEditInputPart sends the image, without
// building the part.
func geminiImageEditInputSent(image *schemas.ImageInput) bool {
	if len(image.Image) > 0 {
		return true
	}
	if !strings.HasPrefix(image.URL, "data:") {
		return image.URL != ""
	}
	_, isBase64, _, ok := schemas.ParseDataURL(image.URL)
	return ok && isBase64
}

func ToGeminiImageGenerationRequest(bifrostReq *schemas.BifrostImageGenerationRequest) *GeminiGenerationRequest {
	if bifrostReq == nil {
		return nil
	}

	bifrostReq.Model = NormalizeModelName(bifrostReq.Model)

	// Create the base Gemini generation request
	geminiReq := &GeminiGenerationRequest{
		Model: bifrostReq.Model,
	}

	// Set response modalities to indicate this is an image generation request
	geminiReq.GenerationConfig.ResponseModalities = []Modality{ModalityImage}

	// Convert parameters to generation config
	if bifrostReq.Params != nil {
		// Clone: the conversion runs once per retry/fallback attempt on the same
		// Bifrost request, and the consumed keys are deleted from the outbound map.
		geminiReq.ExtraParams = maps.Clone(bifrostReq.Params.ExtraParams)

		// Prefer explicit aspect_ratio; fall back to deriving aspect ratio + resolution from size.
		imageConfig := &GeminiImageConfig{}
		if bifrostReq.Params.Size != nil && strings.ToLower(*bifrostReq.Params.Size) != "auto" {
			aspectRatio, imageSize := utils.ConvertSizeToAspectRatioAndResolution(*bifrostReq.Params.Size)
			imageConfig.AspectRatio = aspectRatio
			imageConfig.ImageSize = imageSize
		}
		if bifrostReq.Params.AspectRatio != nil && *bifrostReq.Params.AspectRatio != "" {
			imageConfig.AspectRatio = *bifrostReq.Params.AspectRatio
		}
		// Fork (TXE-6526, contract C4): the unified resolution tier goes to imageSize as sent and
		// wins over the tier derived from size, the way aspect_ratio wins over the derived ratio.
		// The handlers also keep the field in ExtraParams (schemas.UnifiedImageFields); it is
		// converted here, so the extra must not reach the vendor as a top-level "resolution".
		if resolution := string(bifrostReq.Params.Resolution); resolution != "" {
			imageConfig.ImageSize = resolution
			delete(geminiReq.ExtraParams, "resolution")
		}
		if imageConfig.AspectRatio != "" || imageConfig.ImageSize != "" {
			geminiReq.GenerationConfig.ImageConfig = imageConfig
		}

		// Handle extra parameters
		if bifrostReq.Params.ExtraParams != nil {
			// Safety settings - support both camelCase (canonical) and snake_case (legacy) keys
			if safetySettings, ok := schemas.SafeExtractFromMap(bifrostReq.Params.ExtraParams, "safetySettings"); ok {
				delete(geminiReq.ExtraParams, "safetySettings")
				if settings, ok := SafeExtractSafetySettings(safetySettings); ok {
					geminiReq.SafetySettings = settings
				}
			} else if safetySettings, ok := schemas.SafeExtractFromMap(bifrostReq.Params.ExtraParams, "safety_settings"); ok {
				delete(geminiReq.ExtraParams, "safety_settings")
				if settings, ok := SafeExtractSafetySettings(safetySettings); ok {
					geminiReq.SafetySettings = settings
				}
			}

			// Cached content - support both camelCase (canonical) and snake_case (legacy) keys
			if cachedContent, ok := schemas.SafeExtractString(bifrostReq.Params.ExtraParams["cachedContent"]); ok {
				delete(geminiReq.ExtraParams, "cachedContent")
				geminiReq.CachedContent = cachedContent
			} else if cachedContent, ok := schemas.SafeExtractString(bifrostReq.Params.ExtraParams["cached_content"]); ok {
				delete(geminiReq.ExtraParams, "cached_content")
				geminiReq.CachedContent = cachedContent
			}

			// Labels
			if labels, ok := schemas.SafeExtractFromMap(bifrostReq.Params.ExtraParams, "labels"); ok {
				switch m := labels.(type) {
				case map[string]string:
					delete(geminiReq.ExtraParams, "labels")
					geminiReq.Labels = m
				case map[string]interface{}:
					out := make(map[string]string, len(m))
					for k, v := range m {
						if s, ok := schemas.SafeExtractString(v); ok {
							out[k] = s
						}
					}
					if len(out) > 0 {
						delete(geminiReq.ExtraParams, "labels")
						geminiReq.Labels = out
					}
				}
			}
		}
	}

	if bifrostReq.Input == nil {
		return nil
	}

	// Create parts for image gen request
	parts := []*Part{
		{
			Text: bifrostReq.Input.Prompt,
		},
	}

	geminiReq.Contents = []Content{
		{
			Role:  RoleUser,
			Parts: parts,
		},
	}

	// Gemini image generation returns a single image per call, so N is not sent as
	// GenerationConfig.CandidateCount. The gemini provider refuses n other than 1 before this
	// conversion (CheckGeminiImageGenerationRequest); a caller of this converter that does not run
	// that check still has N dropped.

	return geminiReq
}

// ValidateImagenImageGenerationParams rejects the unified fields Imagen generation cannot deliver.
// Fork (TXE-6536, contract C4 agreement three): Imagen's :predict has no quality or background
// parameter, and both can change what a request is priced at, so they return 400
// caller_unsupported_parameter instead of being dropped. Every Imagen generation path calls this
// before building the upstream request; a rejected request makes no upstream call.
func ValidateImagenImageGenerationParams(params *schemas.ImageGenerationParameters) *schemas.BifrostError {
	if params == nil {
		return nil
	}
	return rejectImagenUnsupportedParams(params.Quality, params.Background)
}

// ValidateImagenImageEditParams is ValidateImagenImageGenerationParams for Imagen edit.
func ValidateImagenImageEditParams(params *schemas.ImageEditParameters) *schemas.BifrostError {
	if params == nil {
		return nil
	}
	return rejectImagenUnsupportedParams(params.Quality, params.Background)
}

func rejectImagenUnsupportedParams(quality, background *string) *schemas.BifrostError {
	if quality != nil {
		return schemas.NewUnsupportedParameterError("quality", "Imagen models do not support the quality parameter")
	}
	if background != nil {
		return schemas.NewUnsupportedParameterError("background", "Imagen models do not support the background parameter")
	}
	return nil
}

// ToImagenImageGenerationRequest converts a Bifrost Image Request to Imagen format
func ToImagenImageGenerationRequest(bifrostReq *schemas.BifrostImageGenerationRequest) *GeminiImagenRequest {
	if bifrostReq == nil || bifrostReq.Input == nil {
		return nil
	}

	bifrostReq.Model = NormalizeModelName(bifrostReq.Model)

	// Create instances array with prompt
	prompt := bifrostReq.Input.Prompt
	instances := []ImagenInstance{
		{
			Prompt: prompt,
		},
	}

	req := &GeminiImagenRequest{
		Instances:  instances,
		Parameters: GeminiImagenParameters{},
	}

	if bifrostReq.Params != nil {
		if bifrostReq.Params.N != nil {
			req.Parameters.SampleCount = bifrostReq.Params.N
		}

		// Handle size conversion
		if bifrostReq.Params.Size != nil && strings.ToLower(*bifrostReq.Params.Size) != "auto" {
			aspectRatio, imageSize := utils.ConvertSizeToAspectRatioAndResolution(*bifrostReq.Params.Size)
			if imageSize != "" {
				req.Parameters.SampleImageSize = &imageSize
			}
			if aspectRatio != "" {
				req.Parameters.AspectRatio = &aspectRatio
			}
		}

		// Explicit aspect_ratio overrides the size-derived ratio.
		if bifrostReq.Params.AspectRatio != nil && *bifrostReq.Params.AspectRatio != "" {
			req.Parameters.AspectRatio = bifrostReq.Params.AspectRatio
		}

		// Handle output format conversion to mimeType
		outputFormat := ""
		if bifrostReq.Params.OutputFormat != nil {
			outputFormat = *bifrostReq.Params.OutputFormat
		}

		if outputFormat != "" {
			mimeType := convertOutputFormatToMimeType(outputFormat)
			if mimeType != "" {
				if req.Parameters.OutputOptions == nil {
					req.Parameters.OutputOptions = &ImagenOutputOptions{}
				}
				req.Parameters.OutputOptions.MimeType = &mimeType
			}
		}

		if bifrostReq.Params.Seed != nil {
			req.Parameters.Seed = bifrostReq.Params.Seed
		}
		if bifrostReq.Params.NegativePrompt != nil {
			req.Parameters.NegativePrompt = bifrostReq.Params.NegativePrompt
		}

		// Handle extra parameters for Imagen-specific fields
		if bifrostReq.Params.ExtraParams != nil {
			req.ExtraParams = maps.Clone(bifrostReq.Params.ExtraParams)
			if addWatermark, ok := schemas.SafeExtractBoolPointer(bifrostReq.Params.ExtraParams["addWatermark"]); ok {
				delete(req.ExtraParams, "addWatermark")
				req.Parameters.AddWatermark = addWatermark
			}
			if sampleImageSize, ok := schemas.SafeExtractString(bifrostReq.Params.ExtraParams["sampleImageSize"]); ok {
				delete(req.ExtraParams, "sampleImageSize")
				req.Parameters.SampleImageSize = &sampleImageSize
			}

			if aspectRatio, ok := schemas.SafeExtractString(bifrostReq.Params.ExtraParams["aspectRatio"]); ok {
				delete(req.ExtraParams, "aspectRatio")
				req.Parameters.AspectRatio = &aspectRatio
			}

			if personGeneration, ok := schemas.SafeExtractString(bifrostReq.Params.ExtraParams["personGeneration"]); ok {
				delete(req.ExtraParams, "personGeneration")
				req.Parameters.PersonGeneration = &personGeneration
			}

			// Map language from ExtraParams
			if language, ok := schemas.SafeExtractString(bifrostReq.Params.ExtraParams["language"]); ok {
				delete(req.ExtraParams, "language")
				req.Parameters.Language = &language
			}

			// Map enhancePrompt from ExtraParams
			if enhancePrompt, ok := schemas.SafeExtractBoolPointer(bifrostReq.Params.ExtraParams["enhancePrompt"]); ok {
				delete(req.ExtraParams, "enhancePrompt")
				req.Parameters.EnhancePrompt = enhancePrompt
			}

			// Map safetySettings from ExtraParams
			if safetySettings, ok := schemas.SafeExtractFromMap(bifrostReq.Params.ExtraParams, "safetySettings"); ok {
				if settings, ok := SafeExtractSafetySettings(safetySettings); ok {
					delete(req.ExtraParams, "safetySettings")
					req.Parameters.SafetySettings = settings
				}
			}

			// Imagen reports raiFilteredReason only when this is set. Passed through when the
			// caller sends it, as the edit converter does; never added on the caller's behalf.
			if includeRaiReason, ok := schemas.SafeExtractBoolPointer(bifrostReq.Params.ExtraParams["includeRaiReason"]); ok {
				delete(req.ExtraParams, "includeRaiReason")
				req.Parameters.IncludeRaiReason = includeRaiReason
			}
		}
	}

	return req
}

// convertMimeTypeToExtension converts MIME type to file extension
// Maps "image/png" -> "png", "image/jpeg" -> "jpeg", "image/webp" -> "webp"
// For unknown MIME types, extracts the subtype after '/' as fallback
func convertMimeTypeToExtension(mimeType string) string {
	if mimeType == "" {
		return ""
	}
	mimeType = strings.ToLower(strings.TrimSpace(mimeType))
	if semi := strings.Index(mimeType, ";"); semi != -1 {
		mimeType = strings.TrimSpace(mimeType[:semi])
	}
	switch mimeType {
	case "image/png":
		return "png"
	case "image/jpeg", "image/jpg":
		return "jpeg"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	default:
		// Extract subtype after '/' if present, otherwise return empty string
		if idx := strings.Index(mimeType, "/"); idx != -1 && idx+1 < len(mimeType) {
			return mimeType[idx+1:]
		}
		return ""
	}
}

// convertOutputFormatToMimeType converts Bifrost output_format to Imagen mimeType
// Maps "png" -> "image/png", "jpg"/"jpeg" -> "image/jpeg", "webp" -> "image/webp"
// Returns empty string for unsupported formats
func convertOutputFormatToMimeType(outputFormat string) string {
	format := strings.ToLower(strings.TrimSpace(outputFormat))
	switch format {
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	default:
		return ""
	}
}

// ToBifrostImageGenerationResponse converts an Imagen response to Bifrost format.
//
// Fork (TXE-6536, contract C5 and C6): only predictions that carry image bytes reach Data, numbered
// without gaps. A prediction with no bytes and a raiFilteredReason was blocked: it takes no slot and
// is reported in ContentFilter with the vendor's reason verbatim. A prediction with neither is not a
// blocked image (includeSafetyAttributes appends one holding only the prompt's safety attributes), so
// it is skipped without being counted. Usage.OutputImages is the number delivered, 0 included.
func (response *GeminiImagenResponse) ToBifrostImageGenerationResponse() *schemas.BifrostImageGenerationResponse {
	if response == nil {
		return nil
	}

	bifrostResp := &schemas.BifrostImageGenerationResponse{
		Data: make([]schemas.ImageData, 0, len(response.Predictions)),
	}

	var filteredReasons []string
	for _, prediction := range response.Predictions {
		if prediction.BytesBase64Encoded == "" {
			if prediction.RaiFilteredReason != "" {
				filteredReasons = append(filteredReasons, prediction.RaiFilteredReason)
			}
			continue
		}

		// The output format comes from the first delivered image, which is not
		// predictions[0] when that one was blocked.
		if prediction.MimeType != "" && len(bifrostResp.Data) == 0 {
			bifrostResp.ImageGenerationResponseParameters = &schemas.ImageGenerationResponseParameters{
				OutputFormat: convertMimeTypeToExtension(prediction.MimeType),
			}
		}

		bifrostResp.Data = append(bifrostResp.Data, schemas.ImageData{
			B64JSON: prediction.BytesBase64Encoded,
			Index:   len(bifrostResp.Data),
		})
	}

	if len(filteredReasons) > 0 {
		bifrostResp.ContentFilter = &schemas.ContentFilterInfo{
			FilteredCount: len(filteredReasons),
			Reasons:       filteredReasons,
		}
	}
	bifrostResp.Usage = &schemas.ImageUsage{OutputImages: schemas.Ptr(len(bifrostResp.Data))}

	return bifrostResp
}

// ToGeminiImageGenerationResponse converts a BifrostImageGenerationResponse back to Gemini format
func ToGeminiImageGenerationResponse(ctx context.Context, bifrostResp *schemas.BifrostImageGenerationResponse) (*GenerateContentResponse, error) {
	if bifrostResp == nil {
		return nil, nil
	}

	geminiResp := &GenerateContentResponse{
		ResponseID:   bifrostResp.ID,
		ModelVersion: bifrostResp.Model,
	}

	// Convert image data to candidate parts
	if len(bifrostResp.Data) > 0 {
		parts := make([]*Part, 0, len(bifrostResp.Data))
		for i := range bifrostResp.Data {
			imageData := &bifrostResp.Data[i]
			// Determine MIME type - convert file extension back to MIME type
			mimeType := "image/png" // default
			if bifrostResp.ImageGenerationResponseParameters != nil && bifrostResp.ImageGenerationResponseParameters.OutputFormat != "" {
				mimeType = convertOutputFormatToMimeType(bifrostResp.ImageGenerationResponseParameters.OutputFormat)
				if mimeType == "" {
					// Fallback: if conversion fails, assume PNG
					mimeType = "image/png"
				}
			}
			if imageData.B64JSON == "" && imageData.URL != "" {
				// Fetch image from URL with context-aware timeout and size limit
				downloadedImage, err := downloadImageFromURL(ctx, imageData.URL)
				if err != nil {
					return nil, fmt.Errorf("failed to download image from URL: %w", err)
				}
				imageData.B64JSON = downloadedImage
			}
			part := &Part{
				InlineData: &Blob{
					Data:     imageData.B64JSON,
					MIMEType: mimeType,
				},
			}
			parts = append(parts, part)
		}

		geminiResp.Candidates = []*Candidate{
			{
				Content: &Content{
					Role:  RoleModel,
					Parts: parts,
				},
				FinishReason: FinishReasonStop,
			},
		}
	} else if bifrostResp.ContentFilter != nil && len(bifrostResp.ContentFilter.Reasons) > 0 {
		// A block (fork, TXE-6526): the native shape of "no image" is a candidate with no content
		// whose finishReason says why. content_filter.reasons does not record whether its first
		// value was promptFeedback.blockReason or a finishReason, so both come back as the
		// candidate's finishReason; a second value is the vendor's finishMessage.
		candidate := &Candidate{FinishReason: FinishReason(bifrostResp.ContentFilter.Reasons[0])}
		if len(bifrostResp.ContentFilter.Reasons) > 1 {
			candidate.FinishMessage = bifrostResp.ContentFilter.Reasons[1]
		}
		geminiResp.Candidates = []*Candidate{candidate}
	}

	// Convert usage metadata with modality details
	geminiResp.UsageMetadata = convertBifrostImageUsageToGeminiUsageMetadata(bifrostResp.Usage)

	return geminiResp, nil
}

func ToGeminiImageEditRequest(bifrostReq *schemas.BifrostImageEditRequest) *GeminiGenerationRequest {
	if bifrostReq == nil || bifrostReq.Input == nil || len(bifrostReq.Input.Images) == 0 {
		return nil
	}

	bifrostReq.Model = NormalizeModelName(bifrostReq.Model)

	// Create the base Gemini generation request
	geminiReq := &GeminiGenerationRequest{
		Model: bifrostReq.Model,
	}
	// Set response modalities to indicate this is an image generation request
	geminiReq.GenerationConfig.ResponseModalities = []Modality{ModalityImage}

	// Convert parameters to generation config
	if bifrostReq.Params != nil {
		geminiReq.ExtraParams = maps.Clone(bifrostReq.Params.ExtraParams)

		// Prefer explicit aspect_ratio; fall back to deriving aspect ratio + resolution from size.
		imageConfig := &GeminiImageConfig{}
		if bifrostReq.Params.Size != nil && strings.ToLower(*bifrostReq.Params.Size) != "auto" {
			aspectRatio, imageSize := utils.ConvertSizeToAspectRatioAndResolution(*bifrostReq.Params.Size)
			imageConfig.AspectRatio = aspectRatio
			imageConfig.ImageSize = imageSize
		}
		if bifrostReq.Params.AspectRatio != nil && *bifrostReq.Params.AspectRatio != "" {
			imageConfig.AspectRatio = *bifrostReq.Params.AspectRatio
		}
		// Fork (TXE-6526, contract C4): the unified resolution tier goes to imageSize as sent and
		// wins over the tier derived from size, the way aspect_ratio wins over the derived ratio.
		// The handlers also keep the field in ExtraParams (schemas.UnifiedImageFields); it is
		// converted here, so the extra must not reach the vendor as a top-level "resolution".
		if resolution := string(bifrostReq.Params.Resolution); resolution != "" {
			imageConfig.ImageSize = resolution
			delete(geminiReq.ExtraParams, "resolution")
		}
		if imageConfig.AspectRatio != "" || imageConfig.ImageSize != "" {
			geminiReq.GenerationConfig.ImageConfig = imageConfig
		}

		// Handle extra parameters
		if bifrostReq.Params.ExtraParams != nil {
			// Safety settings - support both camelCase (canonical) and snake_case (legacy) keys
			if safetySettings, ok := schemas.SafeExtractFromMap(bifrostReq.Params.ExtraParams, "safetySettings"); ok {
				delete(geminiReq.ExtraParams, "safetySettings")
				if settings, ok := SafeExtractSafetySettings(safetySettings); ok {
					geminiReq.SafetySettings = settings
				}
			} else if safetySettings, ok := schemas.SafeExtractFromMap(bifrostReq.Params.ExtraParams, "safety_settings"); ok {
				delete(geminiReq.ExtraParams, "safety_settings")
				if settings, ok := SafeExtractSafetySettings(safetySettings); ok {
					geminiReq.SafetySettings = settings
				}
			}

			// Cached content - support both camelCase (canonical) and snake_case (legacy) keys
			if cachedContent, ok := schemas.SafeExtractString(bifrostReq.Params.ExtraParams["cachedContent"]); ok {
				delete(geminiReq.ExtraParams, "cachedContent")
				geminiReq.CachedContent = cachedContent
			} else if cachedContent, ok := schemas.SafeExtractString(bifrostReq.Params.ExtraParams["cached_content"]); ok {
				delete(geminiReq.ExtraParams, "cached_content")
				geminiReq.CachedContent = cachedContent
			}

			// Labels
			if labels, ok := schemas.SafeExtractFromMap(bifrostReq.Params.ExtraParams, "labels"); ok {
				switch m := labels.(type) {
				case map[string]string:
					delete(geminiReq.ExtraParams, "labels")
					geminiReq.Labels = m
				case map[string]interface{}:
					out := make(map[string]string, len(m))
					for k, v := range m {
						if s, ok := schemas.SafeExtractString(v); ok {
							out[k] = s
						}
					}
					if len(out) > 0 {
						delete(geminiReq.ExtraParams, "labels")
						geminiReq.Labels = out
					}
				}
			}
		}
	}

	if bifrostReq.Input == nil {
		return nil
	}

	// Create parts for image gen request
	parts := []*Part{
		{
			Text: bifrostReq.Input.Prompt,
		},
	}

	for i := range bifrostReq.Input.Images {
		if part := geminiImageEditInputPart(&bifrostReq.Input.Images[i]); part != nil {
			parts = append(parts, part)
		}
	}

	geminiReq.Contents = []Content{
		{
			Role:  RoleUser,
			Parts: parts,
		},
	}

	return geminiReq
}

// extractIntArray safely extracts an array of integers from an interface{} value
func extractIntArray(v interface{}) []int {
	if v == nil {
		return nil
	}

	// Handle []interface{} (common JSON unmarshaling result)
	if arr, ok := v.([]interface{}); ok {
		result := make([]int, 0, len(arr))
		for _, item := range arr {
			switch val := item.(type) {
			case int:
				result = append(result, val)
			case int64:
				result = append(result, int(val))
			case float64:
				result = append(result, int(val))
			}
		}
		return result
	}

	// Handle []int directly
	if arr, ok := v.([]int); ok {
		return arr
	}

	// Handle []int64
	if arr, ok := v.([]int64); ok {
		result := make([]int, len(arr))
		for i, val := range arr {
			result[i] = int(val)
		}
		return result
	}

	return nil
}

// mapTypeToImagenEditMode maps Bifrost image edit type to Imagen editMode
// Supported edit modes:
//   - "inpainting" -> EDIT_MODE_INPAINT_INSERTION: Add objects from a given prompt
//   - "outpainting" -> EDIT_MODE_OUTPAINT: Extend image beyond its borders
//   - "inpaint_removal" -> EDIT_MODE_INPAINT_REMOVAL: Remove objects and fill in background
//   - "bgswap" -> EDIT_MODE_BGSWAP: Swap background while preserving foreground objects
func mapTypeToImagenEditMode(editType string) string {
	switch strings.ToLower(editType) {
	case "inpainting":
		return "EDIT_MODE_INPAINT_INSERTION"
	case "outpainting":
		return "EDIT_MODE_OUTPAINT"
	case "inpaint_removal":
		return "EDIT_MODE_INPAINT_REMOVAL"
	case "bgswap":
		return "EDIT_MODE_BGSWAP"
	default:
		return ""
	}
}

// mapImagenEditModeToType maps Imagen editMode to Bifrost image edit type
// This is the reverse mapping of mapTypeToImagenEditMode
func mapImagenEditModeToType(editMode string) string {
	switch strings.ToUpper(editMode) {
	case "EDIT_MODE_INPAINT_INSERTION":
		return "inpainting"
	case "EDIT_MODE_OUTPAINT":
		return "outpainting"
	case "EDIT_MODE_INPAINT_REMOVAL":
		return "inpainting"
	case "EDIT_MODE_BGSWAP":
		return "bgswap"
	default:
		return ""
	}
}

// ToImagenImageEditRequest converts a BifrostImageEditRequest to Imagen edit format
// Mask modes (via ExtraParams["maskMode"]):
//   - MASK_MODE_USER_PROVIDED: Use the mask from Params.Mask (default if mask is provided)
//   - MASK_MODE_BACKGROUND: Auto-generated mask from background segmentation
//   - MASK_MODE_FOREGROUND: Auto-generated mask from foreground segmentation
//   - MASK_MODE_SEMANTIC: Auto-generated mask from semantic segmentation with mask class
//
// Mask dilation (via ExtraParams["dilation"]):
//   - Optional float in range [0, 1]. Percentage of image width to dilate (grow) the mask by
//   - Recommended values by edit mode:
//   - EDIT_MODE_INPAINT_INSERTION: 0.01
//   - EDIT_MODE_INPAINT_REMOVAL: 0.01
//   - EDIT_MODE_BGSWAP: 0.0
//   - EDIT_MODE_OUTPAINT: 0.01-0.03
//
// Mask classes (via ExtraParams["maskClasses"]):
//   - Optional list of integers. Mask classes for MASK_MODE_SEMANTIC mode
func ToImagenImageEditRequest(bifrostReq *schemas.BifrostImageEditRequest) *GeminiImagenRequest {
	if bifrostReq == nil || bifrostReq.Input == nil || len(bifrostReq.Input.Images) == 0 {
		return nil
	}

	bifrostReq.Model = NormalizeModelName(bifrostReq.Model)

	req := &GeminiImagenRequest{
		Parameters: GeminiImagenParameters{},
	}

	var refImages []ImagenReferenceImage
	refID := 1

	for _, img := range bifrostReq.Input.Images {
		refImages = append(refImages, ImagenReferenceImage{
			ReferenceType: "REFERENCE_TYPE_RAW",
			ReferenceID:   refID,
			ReferenceImage: ImagenReferenceData{
				BytesBase64Encoded: base64.StdEncoding.EncodeToString(img.Image),
			},
		})
		refID++
	}

	// Handle mask configuration
	if bifrostReq.Params != nil {
		var maskMode string
		var hasMaskData bool
		var dilation *float64
		var maskClasses []int
		req.ExtraParams = maps.Clone(bifrostReq.Params.ExtraParams)
		// Check if user provided a mask
		if len(bifrostReq.Params.Mask) > 0 {
			hasMaskData = true
			maskMode = "MASK_MODE_USER_PROVIDED" // Default when mask is provided
		}

		// Extract optional parameters from ExtraParams
		if bifrostReq.Params.ExtraParams != nil {
			// Allow override or specification of mask mode
			if v, ok := schemas.SafeExtractString(bifrostReq.Params.ExtraParams["maskMode"]); ok {
				delete(req.ExtraParams, "maskMode")
				maskMode = v
			}

			// Extract dilation (range [0, 1])
			if v, ok := schemas.SafeExtractFloat64Pointer(bifrostReq.Params.ExtraParams["dilation"]); ok {
				// Validate dilation is in valid range
				if *v >= 0 && *v <= 1 {
					delete(req.ExtraParams, "dilation")
					dilation = v
				}
			}

			// Extract maskClasses (for MASK_MODE_SEMANTIC)
			if v, ok := bifrostReq.Params.ExtraParams["maskClasses"]; ok {
				delete(req.ExtraParams, "maskClasses")
				maskClasses = extractIntArray(v)
			}
		}

		// Add mask reference if we have mask data or a mask mode is specified
		if hasMaskData || maskMode != "" {
			maskRef := ImagenReferenceImage{
				ReferenceType: "REFERENCE_TYPE_MASK",
				ReferenceID:   refID,
				MaskImageConfig: &ImagenMaskImageConfig{
					MaskMode:    maskMode,
					Dilation:    dilation,
					MaskClasses: maskClasses,
				},
			}

			// Only include mask data if provided
			if hasMaskData {
				maskRef.ReferenceImage = ImagenReferenceData{
					BytesBase64Encoded: base64.StdEncoding.EncodeToString(bifrostReq.Params.Mask),
				}
			}

			refImages = append(refImages, maskRef)
		}
	}

	req.Instances = append(req.Instances, ImagenInstance{
		ReferenceImages: refImages,
		Prompt:          bifrostReq.Input.Prompt,
	})

	// Set parameters
	if bifrostReq.Params != nil {
		if bifrostReq.Params.N != nil {
			req.Parameters.SampleCount = bifrostReq.Params.N
		}

		// Derive aspect ratio + resolution from size (edit params carry no typed aspect_ratio).
		if bifrostReq.Params.Size != nil && strings.ToLower(*bifrostReq.Params.Size) != "auto" {
			aspectRatio, imageSize := utils.ConvertSizeToAspectRatioAndResolution(*bifrostReq.Params.Size)
			if imageSize != "" {
				req.Parameters.SampleImageSize = &imageSize
			}
			if aspectRatio != "" {
				req.Parameters.AspectRatio = &aspectRatio
			}
		}

		if bifrostReq.Params.OutputFormat != nil {
			mimeType := convertOutputFormatToMimeType(*bifrostReq.Params.OutputFormat)
			if mimeType != "" {
				req.Parameters.OutputOptions = &ImagenOutputOptions{MimeType: &mimeType}
			}
		}
		if bifrostReq.Params.OutputCompression != nil {
			if req.Parameters.OutputOptions == nil {
				req.Parameters.OutputOptions = &ImagenOutputOptions{}
			}
			req.Parameters.OutputOptions.CompressionQuality = bifrostReq.Params.OutputCompression
		}

		// Map Bifrost type to Imagen editMode
		if bifrostReq.Params.Type != nil {
			editMode := mapTypeToImagenEditMode(*bifrostReq.Params.Type)
			if editMode != "" {
				req.Parameters.EditMode = &editMode
			}
		}

		if bifrostReq.Params.NegativePrompt != nil {
			req.Parameters.NegativePrompt = bifrostReq.Params.NegativePrompt
		}

		if bifrostReq.Params.Seed != nil {
			req.Parameters.Seed = bifrostReq.Params.Seed
		}

		if bifrostReq.Params.ExtraParams != nil {
			// Only use editMode from ExtraParams if Type was not set
			if bifrostReq.Params.Type == nil {
				if v, ok := schemas.SafeExtractString(bifrostReq.Params.ExtraParams["editMode"]); ok {
					delete(req.ExtraParams, "editMode")
					req.Parameters.EditMode = &v
				}
			}
			if v, ok := schemas.SafeExtractIntPointer(bifrostReq.Params.ExtraParams["guidanceScale"]); ok {
				delete(req.ExtraParams, "guidanceScale")
				req.Parameters.GuidanceScale = v
			}
			if v, ok := schemas.SafeExtractIntPointer(bifrostReq.Params.ExtraParams["baseSteps"]); ok {
				delete(req.ExtraParams, "baseSteps")
				req.Parameters.BaseSteps = v
			}
			if v, ok := schemas.SafeExtractBoolPointer(bifrostReq.Params.ExtraParams["addWatermark"]); ok {
				delete(req.ExtraParams, "addWatermark")
				req.Parameters.AddWatermark = v
			}
			if v, ok := schemas.SafeExtractBoolPointer(bifrostReq.Params.ExtraParams["includeRaiReason"]); ok {
				delete(req.ExtraParams, "includeRaiReason")
				req.Parameters.IncludeRaiReason = v
			}
			if v, ok := schemas.SafeExtractBoolPointer(bifrostReq.Params.ExtraParams["includeSafetyAttributes"]); ok {
				delete(req.ExtraParams, "includeSafetyAttributes")
				req.Parameters.IncludeSafetyAttributes = v
			}
			if v, ok := schemas.SafeExtractString(bifrostReq.Params.ExtraParams["personGeneration"]); ok {
				delete(req.ExtraParams, "personGeneration")
				req.Parameters.PersonGeneration = &v
			}
			if v, ok := schemas.SafeExtractString(bifrostReq.Params.ExtraParams["language"]); ok {
				delete(req.ExtraParams, "language")
				req.Parameters.Language = &v
			}
			if v, ok := schemas.SafeExtractString(bifrostReq.Params.ExtraParams["storageUri"]); ok {
				delete(req.ExtraParams, "storageUri")
				req.Parameters.StorageUri = &v
			}
			if v, ok := SafeExtractSafetySettings(bifrostReq.Params.ExtraParams["safetySettings"]); ok {
				delete(req.ExtraParams, "safetySettings")
				req.Parameters.SafetySettings = v
			}
		}
	}

	return req
}
