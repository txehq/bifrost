package kling

import (
	"fmt"
	"strconv"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// Kling service codes this provider classifies itself (kling.ai/document-api "Error Codes").
// Every other code is classified from the HTTP status.
const (
	klingCodeArrears          = 1101 // account in arrears (postpaid)
	klingCodePackageExhausted = 1102 // resource pack exhausted or expired (prepaid)
	klingCodeContentSecurity  = 1301 // "Trigger the platform's content security policy"
)

// klingContentFilterErrorType is the error.type of a content block.
const klingContentFilterErrorType = "content_filter"

// klingErrorTypeOf returns the error_type a Kling service code declares, or "" to leave the
// classification to the HTTP status. 1301 is the content block of a create call (contract C6);
// 1101 and 1102 are billing states Kling answers with 429, which the status alone would call a
// rate limit.
func klingErrorTypeOf(code int) schemas.ErrorType {
	switch code {
	case klingCodeContentSecurity:
		return schemas.ErrorTypeCallerContentFiltered
	case klingCodeArrears, klingCodePackageExhausted:
		return schemas.ErrorTypeProviderBilling
	default:
		return ""
	}
}

// klingHTTPStatusOf is the HTTP status Kling documents for a service code. It is used only for an
// error envelope that arrives inside a 2xx answer; a non-2xx answer keeps the vendor's own status.
// A code the table does not list is a vendor failure: 502.
func klingHTTPStatusOf(code int) int {
	switch {
	case code >= 1000 && code <= 1004:
		return fasthttp.StatusUnauthorized
	case code >= 1100 && code <= 1102:
		return fasthttp.StatusTooManyRequests
	case code == 1103:
		return fasthttp.StatusForbidden
	case code == 1200 || code == 1201:
		return fasthttp.StatusBadRequest
	case code == 1202 || code == 1203:
		return fasthttp.StatusNotFound
	case code == 1300 || code == 1301:
		return fasthttp.StatusBadRequest
	case code >= 1302 && code <= 1304:
		return fasthttp.StatusTooManyRequests
	case code == 5000:
		return fasthttp.StatusInternalServerError
	case code == 5001:
		return fasthttp.StatusServiceUnavailable
	case code == 5002:
		return fasthttp.StatusGatewayTimeout
	default:
		return fasthttp.StatusBadGateway
	}
}

// applyKlingEnvelope writes Kling's service code, message and request id onto an error: the code
// verbatim in error.code, the request id in error.event_id, and the error_type the code declares.
func applyKlingEnvelope(bifrostErr *schemas.BifrostError, envelope *KlingEnvelope) {
	if bifrostErr.Error == nil {
		bifrostErr.Error = &schemas.ErrorField{}
	}
	if envelope.Code != 0 {
		bifrostErr.Error.Code = schemas.Ptr(strconv.Itoa(envelope.Code))
	}
	if envelope.Message != "" {
		bifrostErr.Error.Message = envelope.Message
	}
	if envelope.RequestID != "" {
		bifrostErr.Error.EventID = schemas.Ptr(envelope.RequestID)
	}
	if errorType := klingErrorTypeOf(envelope.Code); errorType != "" {
		bifrostErr.ExtraFields.ErrorType = errorType
		if errorType == schemas.ErrorTypeCallerContentFiltered {
			bifrostErr.Error.Type = schemas.Ptr(klingContentFilterErrorType)
		}
	}
}

// parseKlingError converts a non-2xx Kling response into a BifrostError that keeps the vendor's
// HTTP status and carries its service code verbatim.
func parseKlingError(resp *fasthttp.Response) *schemas.BifrostError {
	var envelope KlingEnvelope
	bifrostErr := providerUtils.HandleProviderAPIError(resp, &envelope)
	applyKlingEnvelope(bifrostErr, &envelope)
	if bifrostErr.Error.Message == "" {
		bifrostErr.Error.Message = fmt.Sprintf("kling returned HTTP %d", resp.StatusCode())
	}
	return bifrostErr
}

// klingEnvelopeError returns the error a 2xx answer carries in its envelope (code other than 0),
// or nil. The status is the one Kling documents for the code (contract C6: an error object inside
// a vendor 200 is an error).
func klingEnvelopeError(envelope *KlingEnvelope) *schemas.BifrostError {
	if envelope.Code == 0 {
		return nil
	}
	message := envelope.Message
	if message == "" {
		message = fmt.Sprintf("kling error %d", envelope.Code)
	}
	bifrostErr := providerUtils.NewProviderAPIError(message, nil, klingHTTPStatusOf(envelope.Code), nil, nil)
	applyKlingEnvelope(bifrostErr, envelope)
	return bifrostErr
}

// newKlingTaskNotFoundError is the answer to a query Kling returns no task for: 404, so a caller
// recovering by its own reference can tell "no such task, submit again" from a vendor failure.
func newKlingTaskNotFoundError(idKind, id string) *schemas.BifrostError {
	bifrostErr := providerUtils.NewProviderAPIError(
		fmt.Sprintf("kling has no task with %s %q", idKind, id), nil, fasthttp.StatusNotFound, schemas.Ptr("not_found_error"), nil)
	bifrostErr.Error.Code = schemas.Ptr("task_not_found")
	return bifrostErr
}
