package assemblyai

import (
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// parseAssemblyAIError converts a non-2xx Sync API response into a BifrostError that keeps
// the vendor's status. The body is RFC 9457 problem details: detail becomes the message and
// title, the only code-like member the vendor sends, becomes error.code verbatim. The
// error_type is left to the status classification (contract C6): the vendor documents no
// marker for a content block, so none is declared here.
func parseAssemblyAIError(resp *fasthttp.Response) *schemas.BifrostError {
	var errorResp AssemblyAIError
	bifrostErr := providerUtils.HandleProviderAPIError(resp, &errorResp)
	if errorResp.Title == "" && errorResp.Detail == "" {
		return bifrostErr
	}
	if bifrostErr.Error == nil {
		bifrostErr.Error = &schemas.ErrorField{}
	}
	if errorResp.Detail != "" {
		bifrostErr.Error.Message = errorResp.Detail
	} else {
		bifrostErr.Error.Message = errorResp.Title
	}
	if errorResp.Title != "" {
		bifrostErr.Error.Code = schemas.Ptr(errorResp.Title)
	}
	return bifrostErr
}
