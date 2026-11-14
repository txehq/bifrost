package handlers

import (
	"net/http"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
)

// The live, realtime and WebSocket Responses sessions take their key from
// SelectKeyForProviderRequestType, which refuses an operation a custom provider's
// allowed_requests leaves out (TXE-7370). Before that check moved into key selection, the
// provider's own CheckOperationAllowed refused these sessions, with the error
// providerUtils.NewUnsupportedOperationError builds: code unsupported_operation and no type, so
// the realtime wire mapping answers it as type invalid_request_error, code unsupported_operation.
// These helpers give the caller that same refusal; every other key-selection error keeps the
// handler's own error (TXE-7423).

// selectKeyErrorCode returns unsupported_operation when err is an allowed_requests refusal, and
// fallback otherwise.
func selectKeyErrorCode(err error, fallback string) string {
	if _, ok := schemas.AsOperationNotAllowed(err); ok {
		return schemas.UnsupportedOperationCode
	}
	return fallback
}

// withSelectKeyRefusal returns the refusal the provider's own check gave, with status 400, when
// err is an allowed_requests refusal, and be unchanged otherwise. The refusal carries no type:
// the realtime wire mapping reads a type before a code, so a type would hide the code.
func withSelectKeyRefusal(be *schemas.BifrostError, err error) *schemas.BifrostError {
	refused, ok := schemas.AsOperationNotAllowed(err)
	if !ok {
		return be
	}
	refusal := providerUtils.NewUnsupportedOperationError(refused.RequestType, refused.Provider)
	refusal.StatusCode = schemas.Ptr(http.StatusBadRequest)
	return refusal
}
