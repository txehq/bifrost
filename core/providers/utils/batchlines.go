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

// batchLineLimits is the most inline lines each provider's BatchCreate takes: the vendor's
// published per-batch maximum, read on 2026-11-18. A batch the vendor would refuse anyway is
// refused here before any line is converted, so a body under the request-size limit (about
// 676,000 minimal chat lines at 100 MiB) cannot buy that many conversions for nothing
// (TXE-7696).
//
//   - Anthropic 100,000: "A Message Batch is limited to either 100,000 Message requests or
//     256 MB in size" (https://platform.claude.com/docs/en/build-with-claude/batch-processing).
//   - OpenAI 50,000: "a single batch may include up to 50,000 requests"
//     (https://developers.openai.com/api/docs/guides/batch).
//   - Azure 100,000: "Maximum requests per file 100,000"
//     (https://learn.microsoft.com/en-us/azure/ai-foundry/openai/quotas-limits).
//   - Vertex 200,000: "A single batch job may include up to 200,000 requests"
//     (https://docs.cloud.google.com/vertex-ai/generative-ai/docs/multimodal/batch-prediction-gemini).
//   - Bedrock 100,000: the default "Records per batch inference job" quota for every model
//     listed (adjustable per account; https://docs.aws.amazon.com/general/latest/gr/bedrock.html).
//   - Gemini 200,000: the Gemini API publishes no request count, only "under 20MB" for an
//     inline batch (https://ai.google.dev/gemini-api/docs/batch-mode); Vertex's count for the
//     same models bounds it.
//   - xAI 100,000: xAI publishes no hard count; "extremely large batches (>100,000 requests)
//     may be throttled" (https://docs.x.ai/developers/advanced-api-usage/batch-api).
var batchLineLimits = map[schemas.ModelProvider]int{
	schemas.Anthropic: 100_000,
	schemas.OpenAI:    50_000,
	schemas.Azure:     100_000,
	schemas.Vertex:    200_000,
	schemas.Bedrock:   100_000,
	schemas.Gemini:    200_000,
	schemas.XAI:       100_000,
}

// BatchLineLimit returns the most inline lines provider's BatchCreate takes, and false for a
// provider with no limit set.
func BatchLineLimit(provider schemas.ModelProvider) (int, bool) {
	limit, ok := batchLineLimits[provider]
	return limit, ok
}

// CheckBatchLineLimit refuses a batch create with more inline lines than provider's vendor
// accepts: a 400 caller_invalid_request on requests that names the limit. Every BatchCreate
// calls it before it reads anything else from the request, so no line is converted and
// nothing is sent; TestEveryBatchCreateChecksTheLineLimitFirst fails a provider that does
// not. A provider with no limit set fails closed with a 500, never with no limit.
func CheckBatchLineLimit(provider schemas.ModelProvider, lines int) *schemas.BifrostError {
	limit, ok := batchLineLimits[provider]
	if !ok {
		return NewBifrostOperationError(fmt.Sprintf("no batch line limit is set for provider %s", provider), nil)
	}
	if lines <= limit {
		return nil
	}
	bifrostErr := NewBifrostBadRequestError(fmt.Sprintf("a %s batch takes at most %d requests and this one has %d: split it into batches of %d or fewer", provider, limit, lines, limit))
	bifrostErr.Error.Param = "requests"
	return bifrostErr
}

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
