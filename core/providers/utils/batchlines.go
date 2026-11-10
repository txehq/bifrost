package utils

import (
	"errors"
	"fmt"
	"strings"

	"github.com/capsohq/bifrost/core/schemas"
)

// Shared by every provider that converts inline batch lines (Gemini, Vertex, Anthropic): a
// line the caller got wrong is a 400 naming the line, and a line naming a model other than
// the batch's is refused, so the same rule holds whichever vendor runs the batch.

// NewBatchLineBadRequestError answers a batch line the caller got wrong with a 400 that
// names the line and the field in error.param. A line error marked Unsupported is a
// well-formed field this provider's batch cannot take (a url it cannot run, an Imagen model
// on an image line, a second model, a second url): error_type caller_unsupported_parameter
// (contract C4). Any other is a malformed line (a missing body, a body the operation cannot
// parse, neither a url nor messages): caller_invalid_request.
func NewBatchLineBadRequestError(index int, err error) *schemas.BifrostError {
	var lineErr *schemas.BatchLineError
	if !errors.As(err, &lineErr) {
		return NewBifrostOperationError(fmt.Sprintf("failed to convert batch request item %d", index), err)
	}
	lineErr.Index = index
	param := fmt.Sprintf("requests[%d].%s", index, lineErr.Param)
	if lineErr.Unsupported {
		bifrostErr := schemas.NewUnsupportedParameterError(param, lineErr.Error())
		bifrostErr.IsBifrostError = true
		return bifrostErr
	}
	bifrostErr := NewBifrostBadRequestError(lineErr.Error())
	bifrostErr.Error.Param = param
	return bifrostErr
}

// CheckBatchLineModel refuses a line whose body names a model other than the batch's: a
// batch runs one model, and a line asking for another would silently run on the batch's.
// resolve maps a caller's model name to the model the key serves it as.
func CheckBatchLineModel(item schemas.BatchRequestItem, provider schemas.ModelProvider, batchModel string, resolve func(string) string) error {
	lineModel, ok := item.RequestBody()["model"].(string)
	if !ok || lineModel == "" {
		return nil
	}
	// Both names are reduced to the bare model id before they are compared: a line may say
	// gemini-2.5-flash where the batch says publishers/google/models/gemini-2.5-flash.
	bareName := func(model string) string {
		_, bare := schemas.ParseModelString(model, provider)
		if i := strings.LastIndex(bare, "models/"); i >= 0 {
			bare = bare[i+len("models/"):]
		}
		return bare
	}
	line, batch := bareName(lineModel), bareName(batchModel)
	if line == batch || lineModel == batchModel {
		return nil
	}
	if resolve != nil {
		if resolved := resolve(line); resolved == batchModel || bareName(resolved) == batch {
			return nil
		}
	}
	return &schemas.BatchLineError{CustomID: item.CustomID, Param: "body.model", Unsupported: true, Message: fmt.Sprintf("model %s differs from the batch's model %s: a batch runs one model", lineModel, batchModel)}
}
