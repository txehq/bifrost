package bifrost

import "github.com/capsohq/bifrost/core/schemas"

// decideRawImageBodyForAttempt applies the raw image body rule (schemas.RawImageBodyReader,
// TXE-7502) to one provider attempt. A priced image request in raw request body mode reaches the
// provider only when the provider reads that body's pricing dimensions itself; its reading is
// stored on the context for backfillImageResponse and the task create path, which bill it in
// place of the typed request. Any other attempt is refused before an upstream call. It runs per
// attempt, after the passthrough helpers that may switch raw mode off, so a fallback decides
// afresh and an attempt that converts is never refused.
func decideRawImageBodyForAttempt(ctx *schemas.BifrostContext, provider schemas.Provider, requestType schemas.RequestType, req *schemas.BifrostRequest) *schemas.BifrostError {
	ctx.ClearValue(schemas.BifrostContextKeyRawImageBilling)
	if !schemas.IsPricedImageRequestType(requestType) {
		return nil
	}
	if useRaw, _ := ctx.Value(schemas.BifrostContextKeyUseRawRequestBody).(bool); !useRaw {
		return nil
	}
	reader, ok := provider.(schemas.RawImageBodyReader)
	if !ok || !reader.ReadsRawImageBody(requestType) {
		return schemas.NewRawImageBodyUnpricedError(provider.GetProviderKey(), requestType)
	}
	_, model, _ := req.GetRequestFields()
	billing, bifrostErr := reader.ReadRawImageBody(requestType, model, schemas.RawImageRequestBody(req))
	if bifrostErr != nil {
		return bifrostErr
	}
	if billing == nil || billing.Parameters == nil {
		// A reader that read nothing would leave the typed request to price the body.
		return schemas.NewRawImageBodyUnpricedError(provider.GetProviderKey(), requestType)
	}
	ctx.SetValue(schemas.BifrostContextKeyRawImageBilling, billing)
	return nil
}

// applyRawImageTaskBilling writes the attempt's raw image billing onto a task create's response,
// the one billed image task response.
func applyRawImageTaskBilling(ctx *schemas.BifrostContext, response *schemas.BifrostImageTaskResponse) {
	billing := schemas.RawImageBillingFromContext(ctx)
	if billing == nil || response == nil {
		return
	}
	if billing.Images > 0 {
		response.BilledImages = billing.Images
	}
	params := *billing.Parameters
	response.BilledParameters = &params
}
