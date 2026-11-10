package minimax

import (
	"fmt"
	"slices"
	"strings"

	"github.com/capsohq/bifrost/core/providers/openai"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// parseMiniMaxError converts a non-2xx MiniMax response into a BifrostError.
// MiniMax uses an OpenAI-style error object on newer routes and a base_resp envelope on older ones.
func parseMiniMaxError(resp *fasthttp.Response) *schemas.BifrostError {
	bifrostErr := openai.ParseOpenAIError(resp)
	if body, err := providerUtils.CheckAndDecodeBody(resp); err == nil {
		if msg := providerUtils.GetJSONField(body, "base_resp.status_msg").String(); msg != "" && strings.HasPrefix(bifrostErr.Error.Message, "provider API error") {
			bifrostErr.Error.Message = msg
			if code := providerUtils.GetJSONField(body, "base_resp.status_code"); code.Exists() {
				codeStr := code.String()
				bifrostErr.Error.Code = &codeStr
				// The status stays MiniMax's own (contract C6); only a content block needs
				// declaring, since the status alone reads as a parameter or key error.
				if slices.Contains(miniMaxContentFilterCodes, int(code.Int())) {
					bifrostErr.ExtraFields.ErrorType = schemas.ErrorTypeCallerContentFiltered
				}
			}
		}
	}
	return bifrostErr
}

// miniMaxBaseRespError returns an error for a non-zero base_resp status, or nil on success.
// MiniMax reports these inside an HTTP 200 with no result, so the status and error_type
// come from miniMaxStatusToHTTP (contract C6): error.code is the vendor's code verbatim.
func miniMaxBaseRespError(baseResp *MiniMaxBaseResp) *schemas.BifrostError {
	if baseResp == nil || baseResp.StatusCode == 0 {
		return nil
	}
	mapped := miniMaxStatusToHTTP(baseResp.StatusCode)
	message := baseResp.StatusMsg
	if message == "" {
		message = fmt.Sprintf("minimax error %d", baseResp.StatusCode)
	}
	bifrostErr := providerUtils.NewProviderAPIError(message, nil, mapped.status, schemas.Ptr(mapped.errType), nil)
	bifrostErr.Error.Code = schemas.Ptr(fmt.Sprintf("%d", baseResp.StatusCode))
	bifrostErr.ExtraFields.ErrorType = mapped.errorType
	return bifrostErr
}

// miniMaxBaseRespErrorFromBody reads base_resp out of a raw body and maps it like miniMaxBaseRespError.
func miniMaxBaseRespErrorFromBody(body []byte) *schemas.BifrostError {
	code := providerUtils.GetJSONField(body, "base_resp.status_code")
	if !code.Exists() || code.Int() == 0 {
		return nil
	}
	return miniMaxBaseRespError(&MiniMaxBaseResp{
		StatusCode: int(code.Int()),
		StatusMsg:  providerUtils.GetJSONField(body, "base_resp.status_msg").String(),
	})
}

// miniMaxStatus is how one MiniMax business status code is reported: the HTTP status
// Bifrost returns, error.type, and extra_fields.error_type.
type miniMaxStatus struct {
	status    int
	errType   string
	errorType schemas.ErrorType
}

// miniMaxContentFilterCodes are the codes MiniMax uses when it blocks the caller's input
// (1026) or the generated output (1027) as sensitive.
var miniMaxContentFilterCodes = []int{1026, 1027}

// miniMaxStatusToHTTP maps MiniMax business status codes
// (platform.minimax.io/docs/api-reference/errorcode) to what Bifrost returns. Contract C6:
// a customer cause (parameters, content filtering) is a 400; a vendor cause (rate limit,
// key, balance, timeout, server fault, or a code MiniMax has not documented) is a 502,
// with a provider_* error_type that says which. The error_type, not the status, is what
// key rotation reads for a vendor cause (ClassifyFailure).
func miniMaxStatusToHTTP(code int) miniMaxStatus {
	switch code {
	case 1026, 1027:
		return miniMaxStatus{fasthttp.StatusBadRequest, "content_filter", schemas.ErrorTypeCallerContentFiltered}
	case 1042, 1043, 1044, 2013, 20132, 2037, 2039, 2042, 2048, 2054:
		return miniMaxStatus{fasthttp.StatusBadRequest, "invalid_request_error", schemas.ErrorTypeCallerInvalidRequest}
	case 1002, 1039, 1041, 2045, 2056:
		return miniMaxStatus{fasthttp.StatusBadGateway, "rate_limit_error", schemas.ErrorTypeProviderRateLimited}
	case 1004, 2049:
		return miniMaxStatus{fasthttp.StatusBadGateway, "authentication_error", schemas.ErrorTypeProviderAuthFailed}
	case 1008:
		return miniMaxStatus{fasthttp.StatusBadGateway, "insufficient_balance", schemas.ErrorTypeProviderBilling}
	case 1001:
		return miniMaxStatus{fasthttp.StatusBadGateway, "timeout_error", schemas.ErrorTypeProviderTimeout}
	default:
		return miniMaxStatus{fasthttp.StatusBadGateway, "server_error", schemas.ErrorTypeProviderServerError}
	}
}
