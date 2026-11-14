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

// AsOperationNotAllowed returns the OperationNotAllowedError in err's chain, if there is one. It
// walks the chain with type assertions, so unlike errors.As it allocates nothing for an error that
// is not one: the session handlers call it on every key-selection error.
func AsOperationNotAllowed(err error) (*OperationNotAllowedError, bool) {
	for err != nil {
		if refused, ok := err.(*OperationNotAllowedError); ok {
			return refused, true
		}
		switch wrapped := err.(type) {
		case interface{ Unwrap() error }:
			err = wrapped.Unwrap()
		case interface{ Unwrap() []error }:
			for _, inner := range wrapped.Unwrap() {
				if refused, ok := AsOperationNotAllowed(inner); ok {
					return refused, true
				}
			}
			return nil, false
		default:
			return nil, false
		}
	}
	return nil, false
}
