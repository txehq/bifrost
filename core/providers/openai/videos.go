package openai

import (
	"encoding/base64"
	"fmt"
	"mime/multipart"
	"net/http"
	"strconv"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
)

// ToOpenAIVideoGenerationRequest converts a Bifrost Video Request to OpenAI format
func ToOpenAIVideoGenerationRequest(bifrostReq *schemas.BifrostVideoGenerationRequest) (*OpenAIVideoGenerationRequest, error) {
	if bifrostReq == nil || bifrostReq.Input == nil || bifrostReq.Input.Prompt == "" {
		return nil, fmt.Errorf("bifrost request, input, or prompt is nil/empty")
	}

	req := &OpenAIVideoGenerationRequest{
		Model:  bifrostReq.Model,
		Prompt: bifrostReq.Input.Prompt,
	}

	if bifrostReq.Input.InputReference != nil {
		// The vendor takes input_reference only as file bytes and Bifrost does not download a
		// link (contract C4 agreement four), so anything but a decodable data URL is the caller's
		// 400 naming the field.
		sanitizedURL, err := schemas.SanitizeImageURL(*bifrostReq.Input.InputReference)
		if err != nil {
			return nil, unsupportedVideoFieldError("input_reference", fmt.Sprintf("invalid input_reference: %v", err))
		}
		urlInfo := schemas.ExtractURLTypeInfo(sanitizedURL)
		if urlInfo.DataURLWithoutPrefix == nil {
			return nil, unsupportedVideoFieldError("input_reference", "input_reference must be a base64 data URL (e.g. data:image/png;base64,...); this provider does not take a link")
		}
		decoded, err := base64.StdEncoding.DecodeString(*urlInfo.DataURLWithoutPrefix)
		if err != nil {
			return nil, unsupportedVideoFieldError("input_reference", fmt.Sprintf("input_reference is not valid base64: %v", err))
		}
		req.InputReference = decoded
	}

	if bifrostReq.Params != nil {
		if bifrostReq.Params.Seconds != nil {
			req.Seconds = bifrostReq.Params.Seconds
		}

		// size sets the price, so one the vendor does not have is refused, never rewritten
		// (contract C4 agreement three).
		if bifrostReq.Params.Size != "" {
			if !ValidOpenAIVideoSizes[bifrostReq.Params.Size] {
				return nil, unsupportedVideoFieldError("size", fmt.Sprintf("size %q is not one of 720x1280, 1280x720, 1024x1792, 1792x1024", bifrostReq.Params.Size))
			}
			req.Size = bifrostReq.Params.Size
		} else {
			req.Size = string(DefaultOpenAIVideoSize)
		}

		req.ExtraParams = bifrostReq.Params.ExtraParams
	}

	return req, nil
}

// unsupportedVideoFieldError is a converter error that reaches the caller as 400
// caller_unsupported_parameter naming param, before any upstream call.
func unsupportedVideoFieldError(param, message string) error {
	return providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError(param, message))
}

// ToOpenAIVideoEditRequest converts a Bifrost video edit request to OpenAI format. OpenAI takes the
// source video either as an uploaded file or as the ID of a completed video; a URL has no
// equivalent on their API, so it is rejected rather than silently dropped — it is the sole input.
func ToOpenAIVideoEditRequest(bifrostReq *schemas.BifrostVideoEditRequest) (*OpenAIVideoEditRequest, error) {
	if bifrostReq == nil || bifrostReq.Input == nil || bifrostReq.Input.Prompt == "" {
		return nil, fmt.Errorf("bifrost request, input, or prompt is nil/empty")
	}

	req := &OpenAIVideoEditRequest{
		Model:  bifrostReq.Model,
		Prompt: bifrostReq.Input.Prompt,
	}

	source := bifrostReq.Input.Video
	switch {
	case len(source.Video) > 0:
		req.Video.Bytes = source.Video
	case source.ID != "":
		req.Video.ID = providerUtils.StripVideoIDProviderSuffix(source.ID, schemas.OpenAI)
	case source.URL != "":
		return nil, fmt.Errorf("openai video edit accepts an uploaded video or a video id, not a url")
	default:
		return nil, fmt.Errorf("source video is required")
	}

	if bifrostReq.Params != nil {
		req.ExtraParams = bifrostReq.Params.ExtraParams
	}

	return req, nil
}

// ToBifrostVideoEditRequest converts an inbound OpenAI-format video edit request to Bifrost format.
func ToBifrostVideoEditRequest(openaiReq *OpenAIVideoEditRequest) *schemas.BifrostVideoEditRequest {
	if openaiReq == nil {
		return nil
	}

	provider, model := schemas.ParseModelString(openaiReq.Model, "")
	// A "provider/model" string wins; otherwise fall back to whatever the transport resolved, which
	// is the only routing signal when the caller sends no model at all.
	if provider == "" {
		provider = openaiReq.Provider
	}

	return &schemas.BifrostVideoEditRequest{
		Provider: provider,
		Model:    model,
		Input: &schemas.VideoEditInput{
			Prompt: openaiReq.Prompt,
			Video: schemas.VideoInput{
				ID:    openaiReq.Video.ID,
				Video: openaiReq.Video.Bytes,
			},
		},
		Fallbacks: schemas.ParseFallbacks(openaiReq.Fallbacks),
	}
}

// parseVideoEditFormDataBodyFromRequest writes an uploaded-file video edit to a multipart form.
func parseVideoEditFormDataBodyFromRequest(writer *multipart.Writer, openaiReq *OpenAIVideoEditRequest) *schemas.BifrostError {
	if err := writer.WriteField("prompt", openaiReq.Prompt); err != nil {
		return providerUtils.NewBifrostOperationError("failed to write prompt field", err)
	}

	if openaiReq.Model != "" {
		if err := writer.WriteField("model", openaiReq.Model); err != nil {
			return providerUtils.NewBifrostOperationError("failed to write model field", err)
		}
	}

	mimeType := http.DetectContentType(openaiReq.Video.Bytes)
	if !validOpenAIVideoEditMimeTypes[mimeType] {
		mimeType = "video/mp4"
	}
	part, err := writer.CreatePart(map[string][]string{
		"Content-Disposition": {fmt.Sprintf(`form-data; name="video"; filename="video%s"`, openAIVideoEditExtensions[mimeType])},
		"Content-Type":        {mimeType},
	})
	if err != nil {
		return providerUtils.NewBifrostOperationError("failed to create form part for video", err)
	}
	if _, err := part.Write(openaiReq.Video.Bytes); err != nil {
		return providerUtils.NewBifrostOperationError("failed to write video file data", err)
	}

	if err := writer.Close(); err != nil {
		return providerUtils.NewBifrostOperationError("failed to close multipart writer", err)
	}

	return nil
}

var validOpenAIVideoEditMimeTypes = map[string]bool{
	"video/mp4":       true,
	"video/webm":      true,
	"video/quicktime": true,
}

var openAIVideoEditExtensions = map[string]string{
	"video/mp4":       ".mp4",
	"video/webm":      ".webm",
	"video/quicktime": ".mov",
}

func ToOpenAIVideoRemixRequest(bifrostReq *schemas.BifrostVideoRemixRequest) (*OpenAIVideoRemixRequest, error) {
	if bifrostReq == nil || bifrostReq.Input == nil || bifrostReq.Input.Prompt == "" {
		return nil, fmt.Errorf("bifrost request, input, or prompt is nil/empty")
	}

	req := &OpenAIVideoRemixRequest{
		Prompt: bifrostReq.Input.Prompt,
	}

	return req, nil
}

func ToBifrostVideoRemixRequest(openaiReq *OpenAIVideoRemixRequest) *schemas.BifrostVideoRemixRequest {
	if openaiReq == nil || openaiReq.Prompt == "" {
		return nil
	}

	provider := openaiReq.Provider
	if provider == "" {
		provider = schemas.OpenAI
	}

	return &schemas.BifrostVideoRemixRequest{
		ID:       openaiReq.ID,
		Provider: provider,
		Input: &schemas.VideoGenerationInput{
			Prompt: openaiReq.Prompt,
		},
	}
}

func (req *OpenAIVideoGenerationRequest) ToBifrostVideoGenerationRequest(ctx *schemas.BifrostContext) *schemas.BifrostVideoGenerationRequest {
	if req == nil {
		return nil
	}

	provider, model := schemas.ParseModelString(req.Model, "")

	input := &schemas.VideoGenerationInput{
		Prompt:   req.Prompt,
		VideoURI: req.VideoURI,
	}
	if req.InputReference != nil {
		input.InputReference = schemas.Ptr(providerUtils.FileBytesToBase64DataURL(req.InputReference))
	}

	params := &req.VideoGenerationParameters
	addUnifiedVideoFieldCopies(params)
	return &schemas.BifrostVideoGenerationRequest{
		Provider:  provider,
		Model:     model,
		Input:     input,
		Params:    params,
		Fallbacks: schemas.ParseFallbacks(req.Fallbacks),
	}
}

// addUnifiedVideoFieldCopies keeps each unified video field the request set in ExtraParams too,
// under its own name, as the native /v1/videos handler does (schemas.UnifiedVideoGenerationFields).
// Several converters still read those fields only from ExtraParams, so without the copy an
// /openai/v1/videos request, JSON or multipart, reached them without the field (fork, TXE-7126).
// A copy never replaces an extra param the caller sent under the same name.
func addUnifiedVideoFieldCopies(params *schemas.VideoGenerationParameters) {
	copies := [...]struct {
		name  string
		value any
		set   bool
	}{
		{"aspect_ratio", string(params.AspectRatio), params.AspectRatio != ""},
		{"resolution", string(params.Resolution), params.Resolution != ""},
		{"last_frame", string(params.LastFrame), params.LastFrame != ""},
		{"reference_images", []string(params.ReferenceImages), len(params.ReferenceImages) > 0},
	}
	for _, c := range copies {
		if !c.set {
			continue
		}
		if params.ExtraParams == nil {
			params.ExtraParams = make(map[string]any, len(copies))
		}
		if _, sent := params.ExtraParams[c.name]; !sent {
			params.ExtraParams[c.name] = c.value
		}
	}
}

// videoGenerationFormFieldNames are the form fields parseVideoGenerationFormDataBodyFromRequest
// writes from the typed request, other than model. An extra param naming one of them, in any
// spelling, is not written a second time: the typed value is the one sent.
var videoGenerationFormFieldNames = []string{"prompt", "seconds", "size", "input_reference"}

// parseVideoGenerationFormDataBodyFromRequest parses the video generation request and writes it to
// the multipart form. Extra params become form fields when passthroughExtraParams is set
// (x-bf-passthrough-extra-params, contract C4 agreement two).
func parseVideoGenerationFormDataBodyFromRequest(writer *multipart.Writer, openaiReq *OpenAIVideoGenerationRequest, providerName schemas.ModelProvider, passthroughExtraParams bool) *schemas.BifrostError {
	// An extra param naming the model is refused whether or not extra params reach the form,
	// as on every route where they reach the body (the extra-params rule,
	// providerUtils.MergeExtraParamsIntoJSON): a caller who nests a model there is told so on
	// videos as well (fork, TXE-7018).
	if param, field, names := providerUtils.ExtraParamNamingASetField(openaiReq.ExtraParams, []string{"model"}); names {
		bifrostErr, _ := providerUtils.AsBifrostBadRequestError(providerUtils.ExtraParamConflictError(param, field))
		return bifrostErr
	}
	// Add prompt field (required)
	if openaiReq.Prompt == "" {
		return providerUtils.NewBifrostOperationError("prompt is required", nil)
	}
	if err := writer.WriteField("prompt", openaiReq.Prompt); err != nil {
		return providerUtils.NewBifrostOperationError("failed to write prompt field", err)
	}

	// Add optional model field
	if openaiReq.Model != "" {
		if err := writer.WriteField("model", openaiReq.Model); err != nil {
			return providerUtils.NewBifrostOperationError("failed to write model field", err)
		}
	}

	// Add optional seconds field
	if openaiReq.Seconds != nil {
		if err := writer.WriteField("seconds", *openaiReq.Seconds); err != nil {
			return providerUtils.NewBifrostOperationError("failed to write seconds field", err)
		}
	}

	// Add optional size field
	if openaiReq.Size != "" {
		if err := writer.WriteField("size", openaiReq.Size); err != nil {
			return providerUtils.NewBifrostOperationError("failed to write size field", err)
		}
	}

	// Add optional input_reference field (image or video file)
	if len(openaiReq.InputReference) > 0 {
		// Detect MIME type
		mimeType := http.DetectContentType(openaiReq.InputReference)

		// Validate and set proper MIME type
		validMimeTypes := map[string]bool{
			"image/jpeg": true,
			"image/png":  true,
			"image/webp": true,
			"video/mp4":  true,
		}

		if !validMimeTypes[mimeType] {
			// Default to image/png if not detected properly
			mimeType = "image/png"
		}

		// Determine filename based on MIME type
		var filename string
		switch mimeType {
		case "image/jpeg":
			filename = "input_reference.jpg"
		case "image/webp":
			filename = "input_reference.webp"
		case "video/mp4":
			filename = "input_reference.mp4"
		default:
			filename = "input_reference.png"
		}

		// Create form part with proper Content-Type header
		part, err := writer.CreatePart(map[string][]string{
			"Content-Disposition": {fmt.Sprintf(`form-data; name="input_reference"; filename="%s"`, filename)},
			"Content-Type":        {mimeType},
		})
		if err != nil {
			return providerUtils.NewBifrostOperationError("failed to create form part for input_reference", err)
		}
		if _, err := part.Write(openaiReq.InputReference); err != nil {
			return providerUtils.NewBifrostOperationError("failed to write input_reference file data", err)
		}
	}

	if passthroughExtraParams {
		if bifrostErr := writeExtraParamFormFields(writer, openaiReq.ExtraParams, videoGenerationFormFieldNames); bifrostErr != nil {
			return bifrostErr
		}
	}

	// Close the multipart writer
	if err := writer.Close(); err != nil {
		return providerUtils.NewBifrostOperationError("failed to close multipart writer", err)
	}

	return nil
}

// openAIVideoContentType is the type of the artifact behind /videos/{id}/content: the vendor
// serves only MP4 there.
const openAIVideoContentType = "video/mp4"

// openAIVideoStatusTable maps the vendor's video statuses (OpenAI's video object and Azure's
// VideoStatus in v1-preview). Azure's enum is open, so a status it adds later maps to in_progress
// through VideoStatusTable.Apply and stays readable in provider_status.
var openAIVideoStatusTable = providerUtils.VideoStatusTable{
	"queued":      schemas.VideoStatusQueued,
	"in_progress": schemas.VideoStatusInProgress,
	"completed":   schemas.VideoStatusCompleted,
	"failed":      schemas.VideoStatusFailed,
}

// fillOpenAIVideoTaskObject completes a decoded create or query response into the task object of
// contract C7. status and provider_status come from the same vendor string. At completed, the
// vendor's seconds become usage.output_seconds (C5) and the artifact, which needs the vendor's key,
// is a content entry the caller fetches through GET /v1/videos/{id}/content (C9). Run before any
// inline download, so a caller that asked for the bytes gets them in place of the content entry.
func fillOpenAIVideoTaskObject(response *schemas.BifrostVideoGenerationResponse) {
	if response == nil || response.Status == "" {
		return
	}
	openAIVideoStatusTable.Apply(response, string(response.Status))
	if response.Status != schemas.VideoStatusCompleted {
		return
	}
	if response.Seconds != nil {
		if seconds, err := strconv.ParseFloat(*response.Seconds, 64); err == nil {
			if response.Usage == nil {
				response.Usage = &schemas.VideoUsage{}
			}
			response.Usage.OutputSeconds = &seconds
		}
	}
	response.Videos = append(response.Videos, schemas.VideoOutput{
		Type:        schemas.VideoOutputTypeContent,
		ContentType: openAIVideoContentType,
	})
}
