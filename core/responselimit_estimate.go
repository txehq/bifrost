package bifrost

import (
	"errors"

	"github.com/capsohq/bifrost/core/network"
	"github.com/capsohq/bifrost/core/schemas"
)

// attachResponseLimitImageEstimate puts Bifrost's estimate of the vendor's bill on a refusal
// for an image answer larger than client.max_response_body_size_mb (fork, TXE-7302). The vendor
// may have completed and billed the request; the refusal carried no usage, so governance charged
// the virtual key nothing and a key holder could spend the operator's vendor account outside the
// key's budget. Image generation, edit and variation are the request types whose price the request
// fixes (n, size, quality, resolution), so their refusal carries that estimate and governance
// charges it. The refusal stays bifrost_internal: Tokun still charges the caller nothing
// (contract C6). Any other request type, or any other error, is left as it is.
func attachResponseLimitImageEstimate(req *schemas.BifrostRequest, bifrostErr *schemas.BifrostError) {
	if bifrostErr == nil || bifrostErr.Error == nil || bifrostErr.ExtraFields.EstimatedImageUsage != nil {
		return
	}
	if !errors.Is(bifrostErr.Error.Error, network.ErrResponseBodyTooLarge) {
		return
	}
	bifrostErr.ExtraFields.EstimatedImageUsage = schemas.EstimateImageUsageFromRequest(req)
}
