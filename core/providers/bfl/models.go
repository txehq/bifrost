package bfl

import (
	"regexp"
	"strings"
	"time"
)

// inputImageForm is how a BFL endpoint takes input images.
type inputImageForm int

const (
	inputImagesNone     inputImageForm = iota // the endpoint takes no input image
	inputImagesArray                          // FLUX 3: one "images" array
	inputImagesNumbered                       // input_image, input_image_2, ...
	inputImageFill                            // fill: one "image", plus "mask"
)

// modelSpec is what one BFL image endpoint takes, from BFL's OpenAPI (api.bfl.ai/openapi.json).
// A unified field the endpoint has no counterpart for is refused with
// caller_unsupported_parameter, never dropped (contract C4); seed alone is dropped, as C4 allows.
type modelSpec struct {
	images       inputImageForm
	maxImages    int  // the most input images the endpoint takes
	size         bool // width and height
	aspectRatio  bool
	resolution   bool
	seed         bool
	outputFormat bool
	steps        bool
	webhook      bool          // webhook_url and webhook_secret
	linkLifetime time.Duration // how long BFL keeps the result links (contract C7)
}

// Link lifetimes BFL documents: one hour for FLUX 3, ten minutes for every other model.
const (
	flux3LinkLifetime = time.Hour
	otherLinkLifetime = 10 * time.Minute
)

// modelSpecs are the endpoints the BFL vendor sheet sells, with the endpoints that share
// their schema. Keys are the model names, which are also the endpoint paths (/v1/{model}).
var modelSpecs = map[string]modelSpec{
	// Flux3ImageInputs and Flux3ImageKleinInputs: images[] (1 to 10), aspect_ratio, resolution,
	// width/height; neither endpoint has webhook_url. Only Klein has seed, output_format, steps.
	"flux-3-image":       {images: inputImagesArray, maxImages: 10, size: true, aspectRatio: true, resolution: true, linkLifetime: flux3LinkLifetime},
	"flux-3-image-klein": {images: inputImagesArray, maxImages: 10, size: true, aspectRatio: true, resolution: true, seed: true, outputFormat: true, steps: true, linkLifetime: flux3LinkLifetime},
	// Flux2KleinInputs: input_image to input_image_4.
	"flux-2-klein-4b":         {images: inputImagesNumbered, maxImages: 4, size: true, seed: true, outputFormat: true, webhook: true, linkLifetime: otherLinkLifetime},
	"flux-2-klein-9b":         {images: inputImagesNumbered, maxImages: 4, size: true, seed: true, outputFormat: true, webhook: true, linkLifetime: otherLinkLifetime},
	"flux-2-klein-9b-preview": {images: inputImagesNumbered, maxImages: 4, size: true, seed: true, outputFormat: true, webhook: true, linkLifetime: otherLinkLifetime},
	// Flux2Inputs and Flux2FlexInputs: input_image to input_image_8.
	"flux-2-pro":         {images: inputImagesNumbered, maxImages: 8, size: true, seed: true, outputFormat: true, webhook: true, linkLifetime: otherLinkLifetime},
	"flux-2-pro-preview": {images: inputImagesNumbered, maxImages: 8, size: true, seed: true, outputFormat: true, webhook: true, linkLifetime: otherLinkLifetime},
	"flux-2-max":         {images: inputImagesNumbered, maxImages: 8, size: true, seed: true, outputFormat: true, webhook: true, linkLifetime: otherLinkLifetime},
	"flux-2-flex":        {images: inputImagesNumbered, maxImages: 8, size: true, seed: true, outputFormat: true, steps: true, webhook: true, linkLifetime: otherLinkLifetime},
	// FluxPro11Inputs and FluxDevInputs: width/height, no input image (image_prompt is Redux,
	// an extra field).
	"flux-pro-1.1": {size: true, seed: true, outputFormat: true, webhook: true, linkLifetime: otherLinkLifetime},
	"flux-dev":     {size: true, seed: true, outputFormat: true, steps: true, webhook: true, linkLifetime: otherLinkLifetime},
	// FluxKontextProInputs (kontext-max shares it): input_image to input_image_4, aspect_ratio,
	// no width/height.
	"flux-kontext-pro": {images: inputImagesNumbered, maxImages: 4, aspectRatio: true, seed: true, outputFormat: true, webhook: true, linkLifetime: otherLinkLifetime},
	"flux-kontext-max": {images: inputImagesNumbered, maxImages: 4, aspectRatio: true, seed: true, outputFormat: true, webhook: true, linkLifetime: otherLinkLifetime},
	// FluxProFillInputs: one image, and mask.
	"flux-pro-1.0-fill": {images: inputImageFill, maxImages: 1, seed: true, outputFormat: true, steps: true, webhook: true, linkLifetime: otherLinkLifetime},
}

// specFor returns the spec of model. A model outside the table gets the empty spec: only the
// prompt and the extra fields reach it, and every unified field that needs converting is refused,
// so nothing is guessed. Its links get the ten-minute lifetime BFL documents for every model
// other than FLUX 3.
func specFor(model string) modelSpec {
	if spec, ok := modelSpecs[model]; ok {
		return spec
	}
	lifetime := otherLinkLifetime
	if isFlux3Model(model) {
		lifetime = flux3LinkLifetime
	}
	return modelSpec{linkLifetime: lifetime}
}

// isFlux3Model reports whether model is a FLUX 3 model, whose links live an hour.
func isFlux3Model(model string) bool {
	return strings.HasPrefix(model, "flux-3-")
}

// modelPathPattern is the shape of a BFL model name, which is also its endpoint path segment.
// It keeps a caller's model from adding path segments ("../") to the upstream URL.
var modelPathPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.\-]*$`)
