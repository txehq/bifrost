package deepgram

import (
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// parseDeepgramError converts a non-200 Deepgram response into a BifrostError that keeps
// the vendor's status code. error.message is Deepgram's text and error.code its code
// (err_code, error_code or category) verbatim. extra_fields.error_type is not declared
// here: the status code classifies it (401/403 provider_auth_failed, 402 provider_billing,
// 429 provider_rate_limited, 400 caller_invalid_request, 5xx provider_*), the same way for
// every provider (schemas.ClassifyErrorType, contract C6).
func parseDeepgramError(resp *fasthttp.Response) *schemas.BifrostError {
	var errorResp DeepgramError
	bifrostErr := providerUtils.HandleProviderAPIError(resp, &errorResp)
	if bifrostErr.Error == nil {
		bifrostErr.Error = &schemas.ErrorField{}
	}
	if message := errorResp.text(); message != "" {
		bifrostErr.Error.Message = message
	}
	if code := errorResp.code(); code != "" {
		bifrostErr.Error.Code = schemas.Ptr(code)
	}
	return bifrostErr
}

// text returns Deepgram's error text: err_msg, or message with details appended.
func (e *DeepgramError) text() string {
	if e.ErrMsg != "" {
		return e.ErrMsg
	}
	if e.Message != "" && e.Details != "" {
		return e.Message + " " + e.Details
	}
	return e.Message
}

// code returns Deepgram's error code under whichever key the body used.
func (e *DeepgramError) code() string {
	switch {
	case e.ErrCode != "":
		return e.ErrCode
	case e.ErrorCode != "":
		return e.ErrorCode
	}
	return e.Category
}
