package cohere

import (
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

func parseCohereError(resp *fasthttp.Response) *schemas.BifrostError {
	var errorResp CohereError
	bifrostErr := providerUtils.HandleProviderAPIError(resp, &errorResp)
	bifrostErr.Type = &errorResp.Type
	if bifrostErr.Error == nil {
		bifrostErr.Error = &schemas.ErrorField{}
	}
	bifrostErr.Error.Message = errorResp.Message
	if errorResp.ID != "" {
		bifrostErr.EventID = &errorResp.ID
	}
	if errorResp.Code != nil {
		bifrostErr.Error.Code = errorResp.Code
	}
	if errorResp.Type != "" && bifrostErr.Error.Type == nil {
		typeCopy := errorResp.Type
		bifrostErr.Error.Type = &typeCopy
	}
	if errorType, ok := cohereStatusErrorType(resp.StatusCode()); ok {
		bifrostErr.ExtraFields.ErrorType = errorType
	}
	return bifrostErr
}

// cohereStatusErrorType says whose fault a Cohere error status is
// (docs.cohere.com/reference/errors), for chat and embeddings alike. Cohere's v2 error body
// is a flat {"id", "message"} with no type or code, so the status is all there is to go on.
// The status itself is returned unchanged (contract C6). Cohere's own statuses that the
// generic status rules misread are the reason this is declared here: 498 is an invalid
// API key, not a malformed request. A status Cohere does not document is left undeclared
// for schemas.ClassifyErrorType.
//
// Cohere documents no error that marks a content block, so nothing here declares
// ErrorTypeCallerContentFiltered.
func cohereStatusErrorType(status int) (schemas.ErrorType, bool) {
	switch status {
	case 400, 422:
		return schemas.ErrorTypeCallerInvalidRequest, true
	case 401, 403, 498:
		return schemas.ErrorTypeProviderAuthFailed, true
	case 402:
		return schemas.ErrorTypeProviderBilling, true
	case 404:
		// Cohere's chat and embed routes address nothing but the model.
		return schemas.ErrorTypeCallerModelUnknown, true
	case 429:
		return schemas.ErrorTypeProviderRateLimited, true
	case 499:
		return schemas.ErrorTypeCallerCancelled, true
	case 500, 501:
		return schemas.ErrorTypeProviderServerError, true
	case 503:
		return schemas.ErrorTypeProviderOverloaded, true
	case 504:
		return schemas.ErrorTypeProviderTimeout, true
	}
	return "", false
}

// CohereErrorResponse is the body Cohere returns on a failed request. Verified against the live
// v2 API, which replies with a flat {"id", "message"} on 400, 401 and 404 alike.
type CohereErrorResponse struct {
	ID      string `json:"id,omitempty"`
	Message string `json:"message"`
}

// ToCohereError converts a Bifrost error into Cohere's error wire shape.
func ToCohereError(bifrostErr *schemas.BifrostError) *CohereErrorResponse {
	if bifrostErr == nil {
		return nil
	}

	errorResponse := &CohereErrorResponse{}
	if bifrostErr.EventID != nil {
		errorResponse.ID = *bifrostErr.EventID
	}
	if bifrostErr.Error != nil {
		errorResponse.Message = bifrostErr.Error.Message
	}
	return errorResponse
}
