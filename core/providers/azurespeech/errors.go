package azurespeech

import (
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// parseAzureSpeechError converts a non-2xx Azure AI Speech response into a BifrostError
// that keeps the vendor's status. error.code is the vendor's code verbatim and
// error.message its message; when the body carries an innerError with its own message,
// that more specific message is appended. A response with no parsable body (the text to
// speech API often sends none) keeps what HandleProviderAPIError derived from the status.
//
// No error is declared caller_content_filtered here: Azure AI Speech documents no marker
// for a content block on either API, so none is guessed.
func parseAzureSpeechError(resp *fasthttp.Response) *schemas.BifrostError {
	var errorResp AzureSpeechError
	bifrostErr := providerUtils.HandleProviderAPIError(resp, &errorResp)

	code, message, inner := errorResp.Code, errorResp.Message, errorResp.InnerError
	if errorResp.Error != nil && (errorResp.Error.Code != "" || errorResp.Error.Message != "") {
		code, message, inner = errorResp.Error.Code, errorResp.Error.Message, errorResp.Error.InnerError
	}
	if code == "" && message == "" {
		return bifrostErr
	}

	if bifrostErr.Error == nil {
		bifrostErr.Error = &schemas.ErrorField{}
	}
	if message != "" {
		if inner != nil && inner.Message != "" && inner.Message != message {
			message += " (" + inner.Message + ")"
		}
		bifrostErr.Error.Message = message
	}
	if code != "" {
		bifrostErr.Error.Code = schemas.Ptr(code)
	}
	return bifrostErr
}
