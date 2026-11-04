package compat

import (
	"fmt"

	"github.com/capsohq/bifrost/core/schemas"
)

// markRoutedConversion sends the attempt as the request type its target takes, by the
// catalog of the routing plugin that routed the request (BifrostContextKeyRoutedRequestTypes,
// indexed like BifrostContextKeyFallbackIndex): when the target takes another type than the
// request's own, it sets BifrostContextKeyChangeRequestType to the conversion core makes
// (schemas.RequestTypeChange); otherwise it clears it, so the attempt goes out as the request
// is. An attempt whose index or target the routing plugin did not list, or whose target core
// cannot convert to, is left as it is: the routing plugin sends no such attempt.
func markRoutedConversion(ctx *schemas.BifrostContext, requestType schemas.RequestType, routed []schemas.RequestType) {
	attempt := 0
	if index, ok := ctx.Value(schemas.BifrostContextKeyFallbackIndex).(int); ok {
		attempt = index
	}
	if attempt < 0 || attempt >= len(routed) {
		return
	}
	change, ok := schemas.RequestTypeChange(requestType, routed[attempt])
	if !ok {
		return
	}
	// The value is a constant of each case, which the context holds without allocating.
	switch change {
	case "":
		ctx.ClearValue(schemas.BifrostContextKeyChangeRequestType)
	case schemas.ChatCompletionRequest:
		ctx.SetValue(schemas.BifrostContextKeyChangeRequestType, schemas.ChatCompletionRequest)
	case schemas.ResponsesRequest:
		ctx.SetValue(schemas.BifrostContextKeyChangeRequestType, schemas.ResponsesRequest)
	default:
		panic(fmt.Sprintf("compat: schemas.RequestTypeChange returned the conversion %q, which core does not make", change))
	}
}
