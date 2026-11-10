package dashscope

import (
	"strings"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// parseDashScopeError converts a non-2xx DashScope response into a BifrostError. The
// vendor's status code is kept, its code goes to error.code verbatim, and error_type is
// declared for the codes whose status alone would misattribute them (see
// dashScopeErrorType). Every other code is classified from its status.
func parseDashScopeError(resp *fasthttp.Response) *schemas.BifrostError {
	var errorResp DashScopeError
	bifrostErr := providerUtils.HandleProviderAPIError(resp, &errorResp)
	if bifrostErr.Error == nil {
		bifrostErr.Error = &schemas.ErrorField{}
	}
	if errorResp.Message != "" {
		bifrostErr.Error.Message = errorResp.Message
	}
	if bifrostErr.Error.Message == "" {
		bifrostErr.Error.Message = "DashScope API request failed"
	}
	if errorResp.Code != "" {
		bifrostErr.Error.Code = schemas.Ptr(errorResp.Code)
		if errorType := dashScopeErrorType(errorResp.Code); errorType != "" {
			bifrostErr.ExtraFields.ErrorType = errorType
		}
	}
	return bifrostErr
}

// dashScopeErrorType maps DashScope's documented error codes onto the error_type
// vocabulary where the status code alone would put them in the wrong family:
//
//   - Arrearage arrives as a 400 but means the account is overdue, a vendor billing cause.
//   - AllocationQuota.FreeTierOnly arrives as a 403 but means the free quota is used up and
//     the account has not enabled paid use.
//   - Throttling* is a rate limit whatever status carries it.
//   - InvalidApiKey and AccessDenied* are key problems.
//   - ModelNotFound names the model, which a 404 on a task route would not say.
//   - InternalError* and ServiceUnavailable are the vendor's own failures.
//
// "" leaves the error to ClassifyErrorType's status rules. DataInspectionFailed (content
// moderation) is one of those for now: it classifies as caller_invalid_request from its
// 400 until the vocabulary gains caller_content_filtered (TXE-6517).
func dashScopeErrorType(code string) schemas.ErrorType {
	switch {
	case code == "Arrearage", code == "AllocationQuota.FreeTierOnly":
		return schemas.ErrorTypeProviderBilling
	case code == "Throttling", strings.HasPrefix(code, "Throttling."):
		return schemas.ErrorTypeProviderRateLimited
	case code == "InvalidApiKey", code == "AccessDenied", strings.HasPrefix(code, "AccessDenied."), code == "Model.AccessDenied":
		return schemas.ErrorTypeProviderAuthFailed
	case code == "ModelNotFound":
		return schemas.ErrorTypeCallerModelUnknown
	case code == "InternalError", strings.HasPrefix(code, "InternalError."):
		return schemas.ErrorTypeProviderServerError
	case code == "ServiceUnavailable":
		return schemas.ErrorTypeProviderOverloaded
	}
	return ""
}
