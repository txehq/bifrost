package replicate

import (
	"fmt"
	"strconv"
	"strings"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
)

func ToReplicateVideoGenerationInput(bifrostReq *schemas.BifrostVideoGenerationRequest) (*ReplicatePredictionRequest, error) {
	if bifrostReq == nil || bifrostReq.Input == nil {
		return nil, fmt.Errorf("bifrost request or input is nil")
	}
	// A hosted model picks its own length when the create names none, and Replicate's prediction
	// reports neither the rendered duration nor a cost, so such a job could not be priced: it
	// would settle unpriced and never be charged to a budget (TXE-7297). The create must name
	// seconds; nothing is sent to Replicate otherwise. The value must be a whole number greater
	// than zero: "0" or "-3" would reach Replicate, and the pricer reads neither as a duration.
	if bifrostReq.Params == nil || bifrostReq.Params.Seconds == nil || strings.TrimSpace(*bifrostReq.Params.Seconds) == "" {
		return nil, providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError("seconds",
			"a Replicate video create must name seconds: the hosted model picks its own length otherwise, and Replicate reports neither the duration it rendered nor a cost, so the job could not be priced"))
	}
	seconds, err := strconv.Atoi(strings.TrimSpace(*bifrostReq.Params.Seconds))
	if err != nil || seconds <= 0 {
		return nil, providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError("seconds",
			"a Replicate video create must name seconds as a whole number greater than zero, the duration the job is priced at"))
	}

	input := &ReplicatePredictionRequestInput{
		Prompt:   &bifrostReq.Input.Prompt,
		Duration: &seconds,
	}

	if bifrostReq.Input.InputReference != nil {
		// convert input reference to base64
		// if provider is openai, set input reference to base64
		sanitizedURL, err := schemas.SanitizeImageURL(*bifrostReq.Input.InputReference)
		if err != nil {
			return nil, fmt.Errorf("invalid input reference: %w", err)
		}
		if strings.HasPrefix(bifrostReq.Model, string(schemas.OpenAI)) {
			input.InputReference = schemas.Ptr(sanitizedURL)
		} else {
			input.Image = schemas.Ptr(sanitizedURL)
		}
	}

	// Map parameters if available
	if bifrostReq.Params != nil {
		params := bifrostReq.Params

		if params.Seed != nil {
			input.Seed = params.Seed
		}

		if params.NegativePrompt != nil {
			input.NegativePrompt = params.NegativePrompt
		}

		if params.ExtraParams != nil {
			input.ExtraParams = params.ExtraParams
		}
	}

	request := &ReplicatePredictionRequest{
		Input: input,
	}

	// Check if model is a version ID and set version field accordingly
	if isVersionID(bifrostReq.Model) {
		request.Version = &bifrostReq.Model
	}

	if bifrostReq.Params != nil && bifrostReq.Params.ExtraParams != nil {
		request.ExtraParams = bifrostReq.Params.ExtraParams
		if webhook, ok := schemas.SafeExtractStringPointer(bifrostReq.Params.ExtraParams["webhook"]); ok {
			delete(request.ExtraParams, "webhook")
			request.Webhook = webhook
		}
		if webhookEventsFilter, ok := schemas.SafeExtractStringSlice(bifrostReq.Params.ExtraParams["webhook_events_filter"]); ok {
			delete(request.ExtraParams, "webhook_events_filter")
			request.WebhookEventsFilter = webhookEventsFilter
		}
	}

	return request, nil
}

func ToBifrostVideoGenerationResponse(prediction *ReplicatePredictionResponse) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	if prediction == nil {
		return nil, &schemas.BifrostError{
			IsBifrostError: true,
			Error: &schemas.ErrorField{
				Message: "prediction response is nil",
			},
		}
	}

	response := &schemas.BifrostVideoGenerationResponse{
		ID:        prediction.ID,
		CreatedAt: ParseReplicateTimestamp(prediction.CreatedAt),
		Model:     prediction.Model,
		Object:    "video",
	}

	// Map Replicate status to Bifrost video status.
	switch prediction.Status {
	case ReplicatePredictionStatusStarting:
		response.Status = schemas.VideoStatusQueued
	case ReplicatePredictionStatusProcessing:
		response.Status = schemas.VideoStatusInProgress
	case ReplicatePredictionStatusSucceeded:
		response.Status = schemas.VideoStatusCompleted
	case ReplicatePredictionStatusFailed, ReplicatePredictionStatusCanceled:
		response.Status = schemas.VideoStatusFailed
	default:
		response.Status = schemas.VideoStatusQueued
	}

	// Surface provider error details on failed terminal states.
	if response.Status == schemas.VideoStatusFailed {
		errorMsg := "prediction failed"
		errorCode := string(prediction.Status)
		if prediction.Error != nil && *prediction.Error != "" {
			errorMsg = *prediction.Error
		}
		response.Error = &schemas.VideoCreateError{
			Code:    errorCode,
			Message: errorMsg,
		}
	}

	if prediction.CompletedAt != nil {
		response.CompletedAt = schemas.Ptr(ParseReplicateTimestamp(*prediction.CompletedAt))
	}

	// Convert output to ImageData
	// Replicate output can be either a string (single URL) or array of strings
	if prediction.Output != nil {
		if prediction.Output.OutputStr != nil && *prediction.Output.OutputStr != "" {
			response.Videos = append(response.Videos, schemas.VideoOutput{
				Type:        schemas.VideoOutputTypeURL,
				URL:         schemas.Ptr(*prediction.Output.OutputStr),
				ContentType: "video/mp4",
			})
		} else if len(prediction.Output.OutputArray) > 0 {
			for _, url := range prediction.Output.OutputArray {
				response.Videos = append(response.Videos, schemas.VideoOutput{
					Type:        schemas.VideoOutputTypeURL,
					URL:         schemas.Ptr(url),
					ContentType: "video/mp4",
				})
			}
		}
	}

	return response, nil
}
