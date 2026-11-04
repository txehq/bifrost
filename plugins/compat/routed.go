package compat

import (
	"fmt"
	"net/http"

	"github.com/capsohq/bifrost/core/schemas"
)

// markRoutedConversion sends the attempt as the request type its target takes, by the
// catalog of the routing plugin that routed the request (BifrostContextKeyRoutedRequestTypes,
// indexed like BifrostContextKeyFallbackIndex, an absent index being the primary): when the
// target takes another type than the request's own, it sets BifrostContextKeyChangeRequestType to
// the conversion core makes (schemas.RequestTypeChange); otherwise it clears it, so the attempt
// goes out as the request is.
//
// The routing plugin lists a type for every attempt it sends and sends no attempt core cannot
// convert, so an attempt outside that is an attempt the routing plugin did not route, and it
// fails rather than going out as the caller's own type: an index that is not an int or not one
// of the attempts listed with 500 tokun_attempt_index_invalid, a target core cannot convert the
// request to with 501 tokun_route_plan_not_applied (Tokun Hot Path 重构, 执行和记录). Nothing
// falls back from either.
func markRoutedConversion(ctx *schemas.BifrostContext, requestType schemas.RequestType, routed []schemas.RequestType) *schemas.BifrostError {
	attempt := 0
	if value := ctx.Value(schemas.BifrostContextKeyFallbackIndex); value != nil {
		index, ok := value.(int)
		if !ok {
			return routedAttemptError(http.StatusInternalServerError, "tokun_attempt_index_invalid",
				fmt.Sprintf("the attempt index is a %T, not an int", value))
		}
		attempt = index
	}
	if attempt < 0 || attempt >= len(routed) {
		return routedAttemptError(http.StatusInternalServerError, "tokun_attempt_index_invalid",
			fmt.Sprintf("attempt %d is not one of the %d attempts the routing plugin routed", attempt, len(routed)))
	}
	change, ok := schemas.RequestTypeChange(requestType, routed[attempt])
	if !ok {
		return routedAttemptError(http.StatusNotImplemented, "tokun_route_plan_not_applied",
			fmt.Sprintf("attempt %d is routed to a target that takes %s, which a %s request cannot be sent as", attempt, routed[attempt], requestType))
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
	return nil
}

// routedAttemptError fails an attempt the routing plugin did not route. Nothing falls back from
// it: the other attempts were routed by the same plugin.
func routedAttemptError(status int, errorType, message string) *schemas.BifrostError {
	allowFallbacks := false
	return &schemas.BifrostError{
		IsBifrostError: true,
		StatusCode:     &status,
		Type:           &errorType,
		AllowFallbacks: &allowFallbacks,
		Error: &schemas.ErrorField{
			Type:    &errorType,
			Message: message,
		},
	}
}
