package schemas

import "fmt"

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
// fal-ai's num_images, and replicate's number_of_images and num_outputs. A count sent under one of
// them asks the vendor for several images as n does.
var imageCountExtraParams = []string{"num_images", "number_of_images", "num_outputs"}

// RequestedStreamImageCount returns the largest image count a streamed image request asks for and
// the field that asks for it: n, or a vendor name of n in extra params. It returns 0 and "" for a
// request that asks for no count, and for any other request type.
func RequestedStreamImageCount(req *BifrostRequest) (int, string) {
	if req == nil {
		return 0, ""
	}
	var n *int
	var extra map[string]interface{}
	switch {
	case req.ImageGenerationRequest != nil && req.ImageGenerationRequest.Params != nil:
		n, extra = req.ImageGenerationRequest.Params.N, req.ImageGenerationRequest.Params.ExtraParams
	case req.ImageEditRequest != nil && req.ImageEditRequest.Params != nil:
		n, extra = req.ImageEditRequest.Params.N, req.ImageEditRequest.Params.ExtraParams
	default:
		return 0, ""
	}
	count, param := 0, ""
	if n != nil && *n > count {
		count, param = *n, "n"
	}
	for _, name := range imageCountExtraParams {
		if v, ok := SafeExtractInt(extra[name]); ok && v > count {
			count, param = v, name
		}
	}
	return count, param
}

// NewImageStreamCountError is the 400 caller_unsupported_parameter answered to a streamed image
// request for more than one image on a provider whose stream does not relay and count every image.
func NewImageStreamCountError(provider ModelProvider, param string, count int) *BifrostError {
	return NewUnsupportedParameterError(param, fmt.Sprintf(
		"%s must be 1 when stream is true: the %s image stream does not deliver and count more than one image, got %s=%d; send the request without stream to get %d images",
		param, provider, param, count, count))
}
