package tencent

import (
	"fmt"

	"github.com/capsohq/bifrost/core/providers/openai"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// tencentContentFilterCode is TokenHub's error.code for a moderation block of the input or the
// output ("input moderation rejected").
const tencentContentFilterCode = "content_filter"

// tencentModerationStatus is the HTTP status TokenHub documents for a moderation block
// ("输入、输出审核不通过（内容安全拦截）").
const tencentModerationStatus = fasthttp.StatusUnprocessableEntity

// parseTencentError converts a non-2xx TokenHub response, whose body is an OpenAI-style error
// object, into a BifrostError that keeps the vendor's status and code. A moderation block (422,
// or code content_filter under any status) is declared caller_content_filtered (contract C6).
func parseTencentError(resp *fasthttp.Response) *schemas.BifrostError {
	bifrostErr := openai.ParseOpenAIError(resp)
	code := ""
	if bifrostErr.Error != nil && bifrostErr.Error.Code != nil {
		code = *bifrostErr.Error.Code
	}
	if resp.StatusCode() == tencentModerationStatus || code == tencentContentFilterCode {
		bifrostErr.ExtraFields.ErrorType = schemas.ErrorTypeCallerContentFiltered
	}
	return bifrostErr
}

// tencentResponseError returns the error a 200 body carries, or nil. v3.5 reports a failure,
// including a moderation block, as finish_reason "error" with an error object and no image.
// Per contract C6 that is an error: 400 caller_content_filtered for a block, 502 for any other
// vendor failure, with the vendor's code verbatim.
func tencentResponseError(resp *TencentImageResponse) *schemas.BifrostError {
	if resp.Error == nil {
		return nil
	}
	code := tencentErrorCode(resp.Error.Code)
	message := resp.Error.Message
	if message == "" {
		message = fmt.Sprintf("tencent image generation failed (code %q)", code)
	}
	errType := resp.Error.Type
	status := fasthttp.StatusBadGateway
	if code == tencentContentFilterCode {
		status = fasthttp.StatusBadRequest
	}
	var typePtr *string
	if errType != "" {
		typePtr = schemas.Ptr(errType)
	}
	bifrostErr := providerUtils.NewProviderAPIError(message, nil, status, typePtr, nil)
	if code != "" {
		bifrostErr.Error.Code = schemas.Ptr(code)
	}
	if code == tencentContentFilterCode {
		bifrostErr.ExtraFields.ErrorType = schemas.ErrorTypeCallerContentFiltered
	}
	return bifrostErr
}

// tencentErrorCode renders TokenHub's error.code, documented as a string, as text whatever JSON
// type it arrives in.
func tencentErrorCode(code interface{}) string {
	switch v := code.(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		return fmt.Sprintf("%.0f", v)
	default:
		return fmt.Sprint(v)
	}
}
