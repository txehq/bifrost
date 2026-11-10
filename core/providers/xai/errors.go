package xai

import (
	"github.com/bytedance/sonic"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// XAIErrorResponse represents xAI's error response format
type XAIErrorResponse struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

// ParseXAIError parses xAI-specific error responses.
// xAI returns errors in format: {"code": "...", "error": "..."}
// Unlike OpenAI which uses: {"error": {"message": "...", "type": "...", "code": "..."}}
func ParseXAIError(resp *fasthttp.Response) *schemas.BifrostError {
	// Try to parse xAI error format
	var xaiErr XAIErrorResponse
	bifrostErr := providerUtils.HandleProviderAPIError(resp, &xaiErr)

	if bifrostErr == nil {
		return nil
	}

	// If we successfully parsed xAI format, extract the fields
	if xaiErr.Error != "" {
		if bifrostErr.Error == nil {
			bifrostErr.Error = &schemas.ErrorField{}
		}
		bifrostErr.Error.Message = xaiErr.Error
		if xaiErr.Code != "" {
			bifrostErr.Error.Code = schemas.Ptr(xaiErr.Code)
		}
	} else {
		applyOpenAIErrorEnvelope(resp, bifrostErr)
	}

	// A content block keeps the vendor's status and is declared caller_content_filtered
	// (fork, TXE-6606, contract C6), so it is never mistaken for a dead key on a 403.
	// xAI documents no block code of its own; the recognised codes are the shared ones.
	if schemas.IsContentFilterError(bifrostErr) {
		bifrostErr.ExtraFields.ErrorType = schemas.ErrorTypeCallerContentFiltered
	}

	return bifrostErr
}

// openAIErrorEnvelope is the OpenAI error shape, {"error": {"message", "type", "code",
// "param"}}, which xAI's OpenAI-compatible endpoints may answer with instead of its own.
type openAIErrorEnvelope struct {
	Error *struct {
		Message string  `json:"message"`
		Type    *string `json:"type"`
		Code    *string `json:"code"`
		Param   string  `json:"param"`
	} `json:"error"`
}

// applyOpenAIErrorEnvelope fills the error from an OpenAI-shaped body when the xAI shape
// did not match, so the vendor's message and code survive whichever shape xAI used (the
// image routes were parsed as OpenAI errors before they had an xAI handler, TXE-6606).
func applyOpenAIErrorEnvelope(resp *fasthttp.Response, bifrostErr *schemas.BifrostError) {
	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil || len(body) == 0 {
		return
	}
	var envelope openAIErrorEnvelope
	if sonic.Unmarshal(body, &envelope) != nil || envelope.Error == nil || envelope.Error.Message == "" {
		return
	}
	if bifrostErr.Error == nil {
		bifrostErr.Error = &schemas.ErrorField{}
	}
	bifrostErr.Error.Message = envelope.Error.Message
	bifrostErr.Error.Type = envelope.Error.Type
	bifrostErr.Error.Code = envelope.Error.Code
	if envelope.Error.Param != "" {
		bifrostErr.Error.Param = envelope.Error.Param
	}
}
