package schemas

import "fmt"

// Raw image bodies (fork, TXE-7502).
//
// In raw request body mode (BifrostContextKeyUseRawRequestBody) a provider sends the caller's
// body as written, and core never parses it. An image request is priced by what it asks the
// vendor for: the size, the quality, the resolution tier, the steps and the image count. Core
// reads those from the typed request (BackfillParams, BackfillCreateParams), which a raw body does
// not follow, so a raw body naming 2048x2048 or 4K was priced at the default.
//
// The rule, applied by core on every provider attempt: a priced image operation takes a raw
// body only from a provider that reads that body's pricing dimensions itself, strictly
// (RawImageBodyReader). Every other provider's attempt is refused before any upstream call
// (NewRawImageBodyUnpricedError), so a provider added later is covered without a change of its
// own. The reading is what the request is billed at: core writes it onto the response in place of
// its own reading of the typed request.

// BifrostContextKeyRawImageBilling holds the *RawImageBilling of the current attempt: what its
// raw body asks the vendor for (set by bifrost - DO NOT SET THIS MANUALLY).
const BifrostContextKeyRawImageBilling BifrostContextKey = "bifrost-raw-image-billing"

// IsPricedImageRequestType reports whether t is an image operation priced by what its request
// asks for: the sync and streamed generation, edit and variation, and the two task creates.
// A task query and a cancel are not billed.
func IsPricedImageRequestType(t RequestType) bool {
	switch t {
	case ImageGenerationRequest, ImageGenerationStreamRequest,
		ImageEditRequest, ImageEditStreamRequest,
		ImageVariationRequest,
		ImageGenerationTaskRequest, ImageEditTaskRequest:
		return true
	}
	return false
}

// PricedImageRequestTypes lists every request type IsPricedImageRequestType accepts.
var PricedImageRequestTypes = []RequestType{
	ImageGenerationRequest, ImageGenerationStreamRequest,
	ImageEditRequest, ImageEditStreamRequest,
	ImageVariationRequest,
	ImageGenerationTaskRequest, ImageEditTaskRequest,
}

// RawImageBilling is what a raw image body asks the vendor to make, and so what it is billed at.
type RawImageBilling struct {
	// Images is the number of images the body asks for, when the vendor bills a task create for
	// them (BifrostImageTaskResponse.BilledImages). 0 leaves the count to the delivered images.
	Images int
	// InputImages is the number of input images the body carries, 0 when the price does not
	// depend on it.
	InputImages int
	// Parameters are the body's pricing dimensions, in the form the cost path reads them (a size
	// written WIDTHxHEIGHT, a tier written 1K). Never nil: a body that names no dimension is
	// priced at the vendor's default, never at the typed request's.
	Parameters *ImageGenerationResponseParameters
}

// RawImageBodyReader is implemented by a provider that reads the pricing dimensions of a raw
// image body itself. A provider without it, or one whose ReadsRawImageBody is false for the
// request type, is refused raw mode on every priced image operation.
type RawImageBodyReader interface {
	// ReadsRawImageBody reports whether the provider reads raw bodies of requestType.
	ReadsRawImageBody(requestType RequestType) bool
	// ReadRawImageBody reads what body asks the vendor for. It must be strict: a body that is
	// not one JSON object, that names a pricing field more than once in any spelling the vendor
	// may read as that field, or whose pricing fields cannot be read, is refused, since the vendor
	// could read another copy than Bifrost did and make, and bill, something else.
	ReadRawImageBody(requestType RequestType, model string, body []byte) (*RawImageBilling, *BifrostError)
}

// NewRawImageBodyUnpricedError is the refusal of a raw image body on a priced operation whose
// provider does not read one: 400 before any upstream call.
func NewRawImageBodyUnpricedError(provider ModelProvider, requestType RequestType) *BifrostError {
	return &BifrostError{
		IsBifrostError: true,
		StatusCode:     Ptr(400),
		Error: &ErrorField{
			Type:    Ptr("invalid_request_error"),
			Code:    Ptr("raw_request_body_unsupported"),
			Message: fmt.Sprintf("%s does not take a raw request body on %s: the request is priced by the size, tier and image count it asks for, and Bifrost does not read them from a body it sends unparsed. Send the request without raw request body mode", provider, requestType),
		},
		ExtraFields: BifrostErrorExtraFields{
			ErrorType:   ErrorTypeCallerInvalidRequest,
			Provider:    provider,
			RequestType: requestType,
		},
	}
}

// RawImageBillingFromContext returns the attempt's raw image billing, or nil when the attempt
// sent no raw image body.
func RawImageBillingFromContext(ctx interface{ Value(any) any }) *RawImageBilling {
	if ctx == nil {
		return nil
	}
	billing, _ := ctx.Value(BifrostContextKeyRawImageBilling).(*RawImageBilling)
	return billing
}

// RawImageRequestBody returns the raw body the typed image request of req carries, and nil when
// req is no image request.
func RawImageRequestBody(req *BifrostRequest) []byte {
	if req == nil {
		return nil
	}
	switch {
	case req.ImageGenerationRequest != nil:
		return req.ImageGenerationRequest.GetRawRequestBody()
	case req.ImageEditRequest != nil:
		return req.ImageEditRequest.GetRawRequestBody()
	case req.ImageVariationRequest != nil:
		return req.ImageVariationRequest.GetRawRequestBody()
	}
	return nil
}
