package googlecloudtts

import (
	"strconv"
	"time"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// retryInfoType is the @type of the error detail Google APIs use to say how long to wait.
const retryInfoType = "type.googleapis.com/google.rpc.RetryInfo"

// parseGoogleCloudTTSError converts a non-2xx text:synthesize response into a
// BifrostError. The status code is the vendor's. error.code carries the vendor's code and
// error.type its google.rpc.Code name, the same places the gemini provider puts them.
func parseGoogleCloudTTSError(resp *fasthttp.Response) *schemas.BifrostError {
	var errorResp GoogleCloudTTSError
	bifrostErr := providerUtils.HandleProviderAPIError(resp, &errorResp)
	if errorResp.Error == nil {
		return bifrostErr
	}
	if bifrostErr.Error == nil {
		bifrostErr.Error = &schemas.ErrorField{}
	}
	if errorResp.Error.Message != "" {
		bifrostErr.Error.Message = errorResp.Error.Message
	}
	if errorResp.Error.Code != 0 {
		bifrostErr.Error.Code = schemas.Ptr(strconv.Itoa(errorResp.Error.Code))
	}
	if errorResp.Error.Status != "" {
		bifrostErr.Error.Type = schemas.Ptr(errorResp.Error.Status)
	}
	for _, detail := range errorResp.Error.Details {
		if detail.Type != retryInfoType {
			continue
		}
		if delay, err := time.ParseDuration(detail.RetryDelay); err == nil {
			providerUtils.SetRetryAfter(bifrostErr, delay)
		}
		break
	}
	return bifrostErr
}
