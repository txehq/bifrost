package schemas

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// Streamed image count (fork, TXE-8077, contract C4 and C5).
//
// An image stream's final frame is what usage.output_images is read from, and what the stream is
// billed at. A handler that is asked for several images must either relay every one of them and
// report on the final frame the number it delivered, or refuse the request. The handlers that do
// neither (TXE-7116 found the shared OpenAI ones; TXE-8077 found replicate and huggingface) relay
// the images under one index and end on a frame that carries one image and no count, so the
// caller is billed for one image while the vendor makes and bills n.
//
// The rule, applied by core on every provider attempt: a streamed image generation or edit that
// asks for more than one image reaches only a provider that declares, through ImageStreamCounter,
// that its stream relays and counts every image. Every other provider's attempt is refused before
// any upstream call (NewImageStreamCountError), so a provider added later is covered without a
// change of its own. The non-streaming routes still take any n.

// ImageStreamCounter is implemented by a provider whose image stream relays every image the
// vendor makes, each under its own index, and writes on the final frame a usage.output_images
// equal to the number delivered. A provider without it, or one whose StreamsEveryImage is false
// for the request type, is refused a streamed request for more than one image.
type ImageStreamCounter interface {
	StreamsEveryImage(requestType RequestType) bool
}

// IsImageStreamRequestType reports whether t is a streamed image generation or edit.
func IsImageStreamRequestType(t RequestType) bool {
	return t == ImageGenerationStreamRequest || t == ImageEditStreamRequest
}

// ImageStreamRequestTypes lists every request type IsImageStreamRequestType accepts.
var ImageStreamRequestTypes = []RequestType{ImageGenerationStreamRequest, ImageEditStreamRequest}

// imageCountExtraParams are the vendor names of n that reach a vendor unchanged as extra params:
// fal-ai's num_images, replicate's number_of_images and num_outputs, and the names hosted diffusion
// models commonly give the same count (batch_size, num_images_per_prompt, num_samples, samples,
// max_images). A count sent under one of them asks the vendor for several images as n does. The
// list cannot be complete, since a hosted model names its own inputs; TXE-8082 covers a stream
// that delivers more images than it reports.
var imageCountExtraParams = []string{"num_images", "number_of_images", "num_outputs", "batch_size", "num_images_per_prompt", "num_samples", "samples", "max_images"}

// RequestedStreamImageCount reports whether a streamed image request may ask for more than one
// image, and if so the field that asks and the value it gave: n above 1, or a vendor name of n in
// extra params whose value is not exactly 0 or 1. Any other value under a vendor name ("3.0", " 3",
// 2.5, -1, 1e20, true) is counted as asking for more, because the vendor may read it as a count:
// the check fails closed. It reports false for a request that
// asks for at most one image, and for any other request type.
func RequestedStreamImageCount(req *BifrostRequest) (bool, string, string) {
	if req == nil {
		return false, "", ""
	}
	var n *int
	var extra map[string]interface{}
	switch {
	case req.ImageGenerationRequest != nil && req.ImageGenerationRequest.Params != nil:
		n, extra = req.ImageGenerationRequest.Params.N, req.ImageGenerationRequest.Params.ExtraParams
	case req.ImageEditRequest != nil && req.ImageEditRequest.Params != nil:
		n, extra = req.ImageEditRequest.Params.N, req.ImageEditRequest.Params.ExtraParams
	default:
		return false, "", ""
	}
	if n != nil && *n > 1 {
		return true, "n", strconv.Itoa(*n)
	}
	for _, name := range imageCountExtraParams {
		value, present := extra[name]
		if !present || value == nil {
			continue
		}
		if isZeroOrOneImage(value) {
			continue
		}
		return true, name, BoundEchoedName(fmt.Sprint(value))
	}
	return false, "", ""
}

// isZeroOrOneImage reports whether value asks for at most one image: exactly 0 or 1 as a number of
// any JSON-decoded or Go numeric type, or as the string "0" or "1". Everything else, a negative or
// out-of-range number included, is read as asking for more, so no conversion can wrap a large
// value into an allowed one.
func isZeroOrOneImage(value interface{}) bool {
	switch v := value.(type) {
	case float64:
		return v == 0 || v == 1
	case float32:
		return v == 0 || v == 1
	case int:
		return v == 0 || v == 1
	case int8:
		return v == 0 || v == 1
	case int16:
		return v == 0 || v == 1
	case int32:
		return v == 0 || v == 1
	case int64:
		return v == 0 || v == 1
	case uint:
		return v == 0 || v == 1
	case uint8:
		return v == 0 || v == 1
	case uint16:
		return v == 0 || v == 1
	case uint32:
		return v == 0 || v == 1
	case uint64:
		return v == 0 || v == 1
	case json.Number:
		return v == "0" || v == "1"
	case string:
		return v == "0" || v == "1"
	}
	return false
}

// NewImageStreamCountError is the 400 caller_unsupported_parameter answered to a streamed image
// request for more than one image on a provider whose stream does not relay and count every image.
func NewImageStreamCountError(provider ModelProvider, param, value string) *BifrostError {
	return NewUnsupportedParameterError(param, fmt.Sprintf(
		"%s must be 1 when stream is true: the %s image stream does not deliver and count more than one image, got %s=%s; send the request without stream to get several images",
		param, provider, param, value))
}

// SetDeliveredImages writes on the final frame of an image stream the number of finished images
// the stream delivered (fork, TXE-8082, contract C5): usage.output_images, which the caller and
// Tokun read, and the internal NImages count the cost path prices a stream by. A count the vendor
// reported itself is kept. A negative delivered count writes nothing.
func (r *BifrostImageGenerationStreamResponse) SetDeliveredImages(delivered int) {
	if r == nil || delivered < 0 {
		return
	}
	if r.Usage == nil {
		r.Usage = &ImageUsage{}
	}
	if r.Usage.OutputImages == nil {
		r.Usage.OutputImages = &delivered
	}
	if r.Usage.OutputTokensDetails == nil {
		r.Usage.OutputTokensDetails = &ImageTokenDetails{}
	}
	if r.Usage.OutputTokensDetails.NImages == 0 && *r.Usage.OutputImages > 0 {
		r.Usage.OutputTokensDetails.NImages = *r.Usage.OutputImages
	}
}
