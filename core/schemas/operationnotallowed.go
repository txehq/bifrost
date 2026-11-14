package schemas

import "fmt"

// UnsupportedOperationCode is the error code a refusal carries when a custom provider's
// allowed_requests does not list the operation (the code CheckOperationAllowed sets).
const UnsupportedOperationCode = "unsupported_operation"

// OperationNotAllowedError is the error SelectKeyForProviderRequestType returns when a custom
// provider's allowed_requests does not list the operation (TXE-7370). The live, realtime and
// WebSocket handlers that select a key this way answer it as a 400 whose code is
// UnsupportedOperationCode, the refusal those sessions gave before the check moved into key
// selection (TXE-7423).
type OperationNotAllowedError struct {
	RequestType RequestType
	Provider    ModelProvider
}

func (e *OperationNotAllowedError) Error() string {
	return fmt.Sprintf("%s is not supported by %s provider", e.RequestType, e.Provider)
}
