package dashscope

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
)

// Wan text-to-image and Wan image edit on the image task routes (fork, TXE-7050, contract C7
// BF16, items B6 and B9). Wan's image API is task-based: a create is one POST to
// image-generation/generation with X-DashScope-Async: enable, and the task is then queried and
// cancelled through DashScope's task API, as Wan video is. The synchronous image routes keep
// refusing a Wan model (checkDashScopeSyncImageModel).

const (
	// dashScopeImageTaskPath is the async create of Wan's new image protocol (wan2.6-t2i and the
	// Wan image generation and edit models): input.messages and parameters, the shape
	// ToDashScopeImageGenerationRequest builds.
	dashScopeImageTaskPath = "/api/v1/services/aigc/image-generation/generation"

	// dashScopeImageLinkLifetime is how long DashScope keeps a task and its result links: 24
	// hours. expires_at is counted from the task's completion (TXE-7050).
	dashScopeImageLinkLifetime = 24 * time.Hour
)

// wanLegacyImagePrefixes are the Wan image models on the old protocol (text2image/image-synthesis,
// results in output.results[].url): wan2.5-t2i-preview and earlier. They are not served on the
// task routes: their body and result shape differ, and the newer models replace them. A request
// naming one is refused before any upstream call.
var wanLegacyImagePrefixes = []string{"wan2.5", "wan2.2", "wan2.1", "wanx"}

// The unified fields the Wan image task creates deliver (contract C4). resolution and size both
// reach DashScope as parameters.size: Wan takes a tier ("1K", "2K", "4K" on wan2.7-image-pro) or
// width*height there. No task control field is declared: DashScope has no callback, external task
// id, deadline or storage target, so core refuses all four before a key is chosen (contract C7).
var (
	dashScopeImageTaskGenerationFields = []string{"n", "size", "resolution", "response_format", "input_images"}
	dashScopeImageTaskEditFields       = []string{"n", "size", "resolution", "response_format"}
)

// checkDashScopeImageTaskModel refuses a model the image task routes cannot serve: anything but a
// Wan image model on the new protocol.
func checkDashScopeImageTaskModel(model string) *schemas.BifrostError {
	m := strings.ToLower(strings.TrimSpace(model))
	switch dashScopeImageFamilyOf(model) {
	case dashScopeImageWan:
		for _, prefix := range wanLegacyImagePrefixes {
			if strings.HasPrefix(m, prefix) {
				return newDashScopeBadRequest("model", fmt.Sprintf(
					"dashscope: model %q is on Wan's earlier image protocol (text2image/image-synthesis), which the image task routes do not serve; use wan2.6-t2i or a later Wan image model", model))
			}
		}
		return nil
	case dashScopeImageQwen, dashScopeImageZ:
		return newDashScopeBadRequest("model", fmt.Sprintf(
			"dashscope: model %q is synchronous; use POST /v1/images/generations or /v1/images/edits", model))
	}
	bifrostErr := providerUtils.NewBifrostBadRequestError(fmt.Sprintf(
		"dashscope: model %q is not a Wan image model the image task routes serve (expected wan2.6-t2i or a later wan* image model)", model))
	bifrostErr.Error.Param = "model"
	bifrostErr.ExtraFields.ErrorType = schemas.ErrorTypeCallerModelUnknown
	return bifrostErr
}

// wanImageTaskSize is the parameters.size a Wan image task sends: the resolution tier verbatim,
// or size as width*height. Both set is refused: they name the same parameter.
func wanImageTaskSize(size *string, resolution schemas.TolerantString) (*string, *schemas.BifrostError) {
	tier := strings.TrimSpace(string(resolution))
	hasSize := size != nil && strings.TrimSpace(*size) != ""
	if tier != "" && hasSize {
		return nil, newDashScopeBadRequest("resolution",
			"dashscope: resolution and size both set Wan's parameters.size; send one")
	}
	if tier != "" {
		// Only a tier Wan takes, in its spelling: anything else would reach parameters.size as
		// sent while pricing read an unknown tier at the base rate. width*height is size's job.
		upper := strings.ToUpper(tier)
		for _, known := range wanImageTiers {
			if upper == known {
				return &upper, nil
			}
		}
		return nil, schemas.NewUnsupportedParameterError("resolution", fmt.Sprintf(
			"dashscope: Wan takes resolution as 1K, 2K or 4K; %q cannot be delivered (send width and height as size)", schemas.BoundEchoedName(tier)))
	}
	return nil, nil
}

// validateDashScopeImageTask checks a Wan image task create before any upstream call: the model,
// the shared image parameter rules, and resolution against size.
func validateDashScopeImageTask(model string, n *int, size, responseFormat *string, resolution schemas.TolerantString, extraParams map[string]interface{}) (*string, *schemas.BifrostError) {
	if bifrostErr := checkDashScopeImageTaskModel(model); bifrostErr != nil {
		return nil, bifrostErr
	}
	if name := wanNonASCIIExtraName(extraParams); name != "" {
		return nil, schemas.NewUnsupportedParameterError(name, fmt.Sprintf(
			"dashscope: extra param %q cannot be delivered on the image task routes: a name outside ASCII may be read by DashScope as a field that sets what the task is billed for, and no DashScope field has one", name))
	}
	if bifrostErr := validateDashScopeImageParams(dashScopeImageWan, n, size, responseFormat, extraParams); bifrostErr != nil {
		return nil, bifrostErr
	}
	// An extra parameters object naming n or size (parameters.n, parameters.size, in the spellings
	// schemas.SameJSONFieldName reads) is refused by core before a key is chosen:
	// schemas.NestedPriceFields lists both paths for the image task request types (TXE-7499). The
	// separator aliases DashScope may also read are refused below (wanBilledParameterAlias).
	if name := wanInterleaveSwitch(extraParams); name != "" {
		return nil, schemas.NewUnsupportedParameterError(name, fmt.Sprintf(
			"dashscope: %s cannot be delivered on the image task routes: it may switch on Wan's interleaved mode, which makes up to max_images images whatever n says, while a task create is billed for the images it asks for (a name outside ASCII is refused for the same reason)", name))
	}
	if name, field := wanBilledParameterAlias(extraParams); name != "" {
		return nil, schemas.NewUnsupportedParameterError(name, fmt.Sprintf(
			"dashscope: extra params cannot set %s: DashScope may read it as parameters.%s, which sets what the task is billed for; send the request's own %s field", name, field, field))
	}
	return wanImageTaskSize(size, resolution)
}

// wanIsParametersKey reports whether an extra param named key is merged into DashScope's
// parameters object: as the merge reads it (providerUtils.ExtraParamNamesField, which folds
// "parameterſ" to parameters), or as DashScope might (wanSameName, which drops separators).
func wanIsParametersKey(key string) bool {
	return providerUtils.ExtraParamNamesField(key, "parameters") || wanSameName(key, "parameters")
}

// wanNonASCIIExtraName returns an extra param name with a character outside ASCII, at the top
// level or inside an extra parameters object, whatever its value; "" when there is none. JSON
// parsers fold such names differently (Go reads "ſ" as s, Java lower-cases "İ" to i), so one may
// be a billed field to DashScope that no check here reads as one. No DashScope field has one.
func wanNonASCIIExtraName(extraParams map[string]interface{}) string {
	for key, value := range extraParams {
		if !isASCII(key) {
			return key
		}
		if object, ok := schemas.ExtraParamObject(value); ok && wanIsParametersKey(key) {
			for nested := range object {
				if !isASCII(nested) {
					return key + "." + nested
				}
			}
		}
	}
	return ""
}

// wanFoldedName is name's ASCII letters and digits in lower case: every separator dropped, the
// widest reading a JSON matcher on DashScope's side might give it ("s-ize" and "S_I_Z_E" are size).
func wanFoldedName(name string) string {
	var folded strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			folded.WriteByte(c)
		}
	}
	return folded.String()
}

// wanBilledParameterAlias returns an extra param, at the top level or inside an extra parameters
// object (GetExtraParams puts both in parameters), whose folded name is a parameter the task is
// billed by (n, size), and that parameter; "" when there is none. The spellings SameJSONFieldName
// reads are refused by core (TXE-7499); this catches the separators it keeps ("s-ize", "-n").
func wanBilledParameterAlias(extraParams map[string]interface{}) (name, field string) {
	check := func(candidate string) string {
		folded := wanFoldedName(candidate)
		for _, field := range dashScopeImagePriceParameters {
			if folded == field {
				return field
			}
		}
		return ""
	}
	for key, value := range extraParams {
		if object, ok := schemas.ExtraParamObject(value); ok && wanIsParametersKey(key) {
			for nested := range object {
				if field := check(nested); field != "" {
					return key + "." + nested, field
				}
			}
			continue
		}
		if field := check(key); field != "" {
			return key, field
		}
	}
	return "", ""
}

// Wan's interleaved text-and-image mode is switched on by enable_interleave and max_images, and
// there max_images, not n, sets how many images are made (and billed). A task create is billed for
// the images it asks for at submission (BilledImages), and the query is not billed, so a count the
// create cannot know is refused rather than under-billed (wanNamesInterleave).

// wanSameName reports whether a and b name the same DashScope field. It is
// schemas.SameJSONFieldName (case and underscores ignored) with hyphens ignored too: Alibaba's
// Java services may read JSON with a matcher that folds '-' as well as '_' (fastjson's smart
// match), so a spelling Bifrost reads as another field could be the switch to DashScope.
func wanSameName(a, b string) bool {
	if isASCII(a) && isASCII(b) {
		// Every separator dropped, not only '_' and '-' (wanFoldedName).
		return wanFoldedName(a) == wanFoldedName(b)
	}
	return schemas.SameJSONFieldName(strings.ReplaceAll(a, "-", ""), strings.ReplaceAll(b, "-", ""))
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// wanNamesInterleave reports whether name could be one of the switches that make the image count
// something other than n to some JSON matcher: its letters, folded, contain "interleave" or
// "maximages" (wan2.6-image's interleaved mode) or "sequential" (wan2.7-image's image set mode,
// parameters.enable_sequential, where n is only an upper bound of up to 12). This fails closed on
// spellings no exact rule anticipates (an "is" prefix on a boolean, other separators), at the cost
// of refusing a vendor field that happens to contain those words; there is none documented.
//
// A name with any character outside ASCII is treated as one too. Case-folding rules differ between
// JSON parsers (Go folds "ſ" to s, Java's lower-casing turns "İ" into i), so no single fold can
// promise to read such a name as DashScope does, and no documented DashScope field has one.
func wanNamesInterleave(name string) bool {
	if !isASCII(name) {
		return true
	}
	folded := wanFoldedName(name)
	return strings.Contains(folded, "interleave") || strings.Contains(folded, "maximages") ||
		strings.Contains(folded, "sequential")
}

// wanInterleaveSwitch returns the name of an interleaved-mode switch among the extra params, at
// the top level or inside an extra parameters object (GetExtraParams puts both in parameters),
// in the caller's spelling; "" when there is none. enable_interleave set to false is not one.
func wanInterleaveSwitch(extraParams map[string]interface{}) string {
	check := func(name string, value interface{}) string {
		if !wanNamesInterleave(name) || value == nil {
			return ""
		}
		if on, isBool := value.(bool); isBool && !on {
			return ""
		}
		return name
	}
	for name, value := range extraParams {
		if object, ok := schemas.ExtraParamObject(value); ok && wanIsParametersKey(name) {
			for nested, nestedValue := range object {
				if found := check(nested, nestedValue); found != "" {
					return name + "." + found
				}
			}
			continue
		}
		if found := check(name, value); found != "" {
			return found
		}
	}
	return ""
}

// ImageGenerationTask creates a Wan text-to-image task.
func (provider *DashScopeProvider) ImageGenerationTask(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostImageGenerationRequest) (*schemas.BifrostImageTaskResponse, *schemas.BifrostError) {
	if request == nil {
		return nil, providerUtils.NewBifrostBadRequestError("dashscope: image generation task request is nil")
	}
	if bifrostErr := checkDashScopeImageTaskModel(request.Model); bifrostErr != nil {
		return nil, bifrostErr
	}
	rawBody, bifrostErr := dashScopeRawImageBody(ctx, request, schemas.ImageGenerationTaskRequest)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if rawBody != nil {
		billed, billedParams, countErr := wanRawImageTaskBilling(rawBody, request.Model)
		if countErr != nil {
			return nil, countErr
		}
		return provider.submitImageTask(ctx, key, schemas.ImageGenerationTaskRequest, request.Model, rawBody, billed, billedParams)
	}
	var tierSize, size *string
	var n *int
	if p := request.Params; p != nil {
		n, size = p.N, p.Size
		tierSize, bifrostErr = validateDashScopeImageTask(request.Model, p.N, p.Size, p.ResponseFormat, p.Resolution, p.ExtraParams)
		if bifrostErr != nil {
			return nil, bifrostErr
		}
		for _, image := range p.InputImages {
			if !isDashScopeImageReference(image) {
				return nil, schemas.NewUnsupportedParameterError("input_images",
					"dashscope takes an input image as an http(s) link or a base64 data URI; bare base64 cannot be delivered")
			}
		}
	}
	jsonData, bifrostErr := providerUtils.CheckContextAndGetRequestBody(ctx, request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			body, err := ToDashScopeImageGenerationRequest(request)
			return withWanImageTaskSize(body, tierSize), err
		})
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	return provider.submitImageTask(ctx, key, schemas.ImageGenerationTaskRequest, request.Model, jsonData,
		wanImageTaskCount(request.Model, n), wanConvertedBilledParams(request.Model, size, tierSize))
}

// ImageEditTask creates a Wan image edit task. Input images go in input.messages[].content[].image:
// a link unchanged, bytes as a base64 data URI (ToDashScopeImageEditRequest).
func (provider *DashScopeProvider) ImageEditTask(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostImageEditRequest) (*schemas.BifrostImageTaskResponse, *schemas.BifrostError) {
	if request == nil {
		return nil, providerUtils.NewBifrostBadRequestError("dashscope: image edit task request is nil")
	}
	if bifrostErr := checkDashScopeImageTaskModel(request.Model); bifrostErr != nil {
		return nil, bifrostErr
	}
	rawBody, bifrostErr := dashScopeRawImageBody(ctx, request, schemas.ImageEditTaskRequest)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if rawBody != nil {
		billed, billedParams, countErr := wanRawImageTaskBilling(rawBody, request.Model)
		if countErr != nil {
			return nil, countErr
		}
		return provider.submitImageTask(ctx, key, schemas.ImageEditTaskRequest, request.Model, rawBody, billed, billedParams)
	}
	var tierSize, size *string
	var n *int
	if p := request.Params; p != nil {
		n, size = p.N, p.Size
		tierSize, bifrostErr = validateDashScopeImageTask(request.Model, p.N, p.Size, p.ResponseFormat, p.Resolution, p.ExtraParams)
		if bifrostErr != nil {
			return nil, bifrostErr
		}
	}
	jsonData, bifrostErr := providerUtils.CheckContextAndGetRequestBody(ctx, request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			body, err := ToDashScopeImageEditRequest(request)
			return withWanImageTaskSize(body, tierSize), err
		})
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	return provider.submitImageTask(ctx, key, schemas.ImageEditTaskRequest, request.Model, jsonData,
		wanImageTaskCount(request.Model, n), wanConvertedBilledParams(request.Model, size, tierSize))
}

// wanDefaultImageCount is the number of images Wan makes when a create names no n: Alibaba's Wan
// text-to-image reference documents n as defaulting to 4, and each image is billed. The Wan 2.7
// image models default to 1 (wanImageTaskDefaults).
const wanDefaultImageCount = 4

// wanImageTaskDefaults are the image count and resolution tier a Wan model makes when a create
// names neither n nor a size. Alibaba's Wan image generation and editing reference: wan2.7-image
// and wan2.7-image-pro default to n=1 and size 2K. Every other Wan image model defaults to n=4,
// and its size default is width*height priced at the base rate, so no tier is recorded.
func wanImageTaskDefaults(model string) (count int, tier string) {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "wan2.7-image") {
		return 1, "2K"
	}
	return wanDefaultImageCount, ""
}

// wanImageTaskCount is the number of images a Wan task create asks DashScope for, and so bills:
// n, or the model's default when the request names none.
func wanImageTaskCount(model string, n *int) int {
	if n != nil && *n > 0 {
		return *n
	}
	count, _ := wanImageTaskDefaults(model)
	return count
}

// wanConvertedBilledParams is what a converted create is priced at, read from what the converter
// sends as parameters.size: the validated tier (tierSize, upper case), the size as the converter
// converts it (written WIDTHxHEIGHT), or, when the request names neither, the model's default
// tier. It is never nil, so core never prices a Wan create from its own reading of the request,
// which could differ from what DashScope receives.
func wanConvertedBilledParams(model string, size, tierSize *string) *schemas.ImageGenerationResponseParameters {
	if tierSize != nil {
		return &schemas.ImageGenerationResponseParameters{Resolution: *tierSize}
	}
	if size != nil && strings.TrimSpace(*size) != "" {
		if dimensions, ok := toDashScopeImageSize(*size); ok {
			return &schemas.ImageGenerationResponseParameters{Size: strings.Replace(dimensions, "*", "x", 1)}
		}
	}
	_, tier := wanImageTaskDefaults(model)
	return &schemas.ImageGenerationResponseParameters{Resolution: tier}
}

// wanImageTiers are the parameters.size values Wan takes as a resolution tier rather than as
// width*height (wan2.7-image-pro also takes 4K).
var wanImageTiers = []string{"1K", "2K", "4K"}

// wanRawImageTaskBilling reads what a body the caller wrote whole (large payload passthrough) asks
// Wan to make: parameters.n, or Wan's default count when it names none, and the size or tier in
// parameters.size, which core cannot read from a request it never parsed. It fails closed: a body
// that is not one JSON object, that names parameters, n or size more than once in any spelling
// (a parser that took another copy than Bifrost's would make, and bill, something else), or whose
// n or size Bifrost cannot read, is refused, as is one that turns on interleaved mode
// (wanInterleaveSwitch). So is a body whose model is missing, repeated or not the request's
// model: governance chose the key, the budget and the price by the request's model, and DashScope
// runs the body's.
func wanRawImageTaskBilling(body []byte, model string) (int, *schemas.ImageGenerationResponseParameters, *schemas.BifrostError) {
	top, err := providerUtils.JSONObjectMembers(body)
	if err != nil {
		return 0, nil, newDashScopeBadRequest("", "dashscope: the raw request body of an image task must be one JSON object: "+err.Error())
	}
	modelValue, bifrostErr := onlyMember(top, "model", "model")
	if bifrostErr != nil {
		return 0, nil, bifrostErr
	}
	var bodyModel string
	if modelValue == nil || json.Unmarshal(modelValue, &bodyModel) != nil || bodyModel != model {
		return 0, nil, newDashScopeBadRequest("model", fmt.Sprintf(
			"dashscope: the raw request body must name the request's model %q, once", model))
	}
	defaultCount, defaultTier := wanImageTaskDefaults(model)
	defaults := &schemas.ImageGenerationResponseParameters{Resolution: defaultTier}
	parameters, bifrostErr := onlyMember(top, "parameters", "parameters")
	if bifrostErr != nil || parameters == nil {
		return defaultCount, defaults, bifrostErr
	}
	members, err := providerUtils.JSONObjectMembers(parameters)
	if err != nil {
		return 0, nil, newDashScopeBadRequest("parameters", "dashscope: parameters must be a JSON object: "+err.Error())
	}
	for _, member := range members {
		value := strings.TrimSpace(string(member.Value))
		if wanNamesInterleave(member.Name) && value != "false" && value != "null" {
			return 0, nil, schemas.NewUnsupportedParameterError("parameters."+member.Name, fmt.Sprintf(
				"dashscope: parameters.%s cannot be delivered on the image task routes: it may switch on Wan's interleaved mode, which makes up to max_images images whatever n says, while a task create is billed for the images it asks for (a name outside ASCII is refused for the same reason)", member.Name))
		}
	}
	count := defaultCount
	nValue, bifrostErr := onlyMember(members, "n", "parameters.n")
	if bifrostErr != nil {
		return 0, nil, bifrostErr
	}
	if nValue != nil {
		var n int
		if json.Unmarshal(nValue, &n) != nil || n <= 0 {
			return 0, nil, newDashScopeBadRequest("parameters.n", "dashscope: parameters.n must be a positive whole number")
		}
		count = n
	}
	sizeValue, bifrostErr := onlyMember(members, "size", "parameters.size")
	if bifrostErr != nil {
		return 0, nil, bifrostErr
	}
	if sizeValue == nil {
		// The body names no size: Wan makes the model's default. The non-nil value keeps core from
		// pricing the create from the parsed request's params, which a raw body does not follow.
		return count, defaults, nil
	}
	var size string
	if json.Unmarshal(sizeValue, &size) != nil {
		return 0, nil, newDashScopeBadRequest("parameters.size", "dashscope: parameters.size must be a string")
	}
	tier := strings.ToUpper(strings.TrimSpace(size))
	for _, known := range wanImageTiers {
		if tier == known {
			return count, &schemas.ImageGenerationResponseParameters{Resolution: tier}, nil
		}
	}
	if dimensions, ok := toDashScopeImageSize(size); ok {
		return count, &schemas.ImageGenerationResponseParameters{Size: strings.Replace(dimensions, "*", "x", 1)}, nil
	}
	return 0, nil, newDashScopeBadRequest("parameters.size", fmt.Sprintf(
		"dashscope: parameters.size %q is neither a tier (1K, 2K, 4K) nor width*height", schemas.BoundEchoedName(size)))
}

// ReadsRawImageBody reports the operations whose raw body DashScope reads the pricing dimensions
// of (schemas.RawImageBodyReader, TXE-7502): the two Wan image task creates. Core refuses raw mode
// on the sync image routes, whose bodies DashScope does not read.
func (provider *DashScopeProvider) ReadsRawImageBody(requestType schemas.RequestType) bool {
	return requestType == schemas.ImageGenerationTaskRequest || requestType == schemas.ImageEditTaskRequest
}

// ReadRawImageBody reads a Wan image task create's raw body: the images it asks for and its size
// or tier (wanRawImageTaskBilling), which the create is billed for.
func (provider *DashScopeProvider) ReadRawImageBody(requestType schemas.RequestType, model string, body []byte) (*schemas.RawImageBilling, *schemas.BifrostError) {
	if !provider.ReadsRawImageBody(requestType) {
		return nil, schemas.NewRawImageBodyUnpricedError(provider.GetProviderKey(), requestType)
	}
	if bifrostErr := checkDashScopeImageTaskModel(model); bifrostErr != nil {
		return nil, bifrostErr
	}
	count, params, bifrostErr := wanRawImageTaskBilling(body, model)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	return &schemas.RawImageBilling{Images: count, Parameters: params}, nil
}

// onlyMember returns the value of the one member named field, in any spelling wanSameName
// treats as it, or nil when there is none. More than one is refused, naming param.
func onlyMember(members []providerUtils.JSONMember, field, param string) (json.RawMessage, *schemas.BifrostError) {
	found, seen := providerUtils.OnlyJSONMember(members, field, wanSameName)
	if seen > 1 {
		return nil, newDashScopeBadRequest(param, fmt.Sprintf("dashscope: %s is named %d times in the raw request body; name it once", param, seen))
	}
	return found, nil
}

// withWanImageTaskSize writes the resolution tier into parameters.size. A size set from the
// request's own size field is already there, converted to width*height.
func withWanImageTaskSize(body *DashScopeImageRequest, tierSize *string) *DashScopeImageRequest {
	if body == nil || tierSize == nil {
		return body
	}
	if body.Parameters == nil {
		body.Parameters = &DashScopeImageParameters{}
	}
	body.Parameters.Size = tierSize
	return body
}

// submitImageTask sends the one create and returns the task object DashScope's answer gives.
// billedImages is the number of images the create asks for, which the create is billed for
// (BifrostImageTaskResponse.BilledImages). billedParams are the pricing dimensions of a raw body, which
// core cannot read (nil on the converted path, where core reads them from the request).
func (provider *DashScopeProvider) submitImageTask(ctx *schemas.BifrostContext, key schemas.Key, requestType schemas.RequestType, model string, jsonData []byte, billedImages int, billedParams *schemas.ImageGenerationResponseParameters) (*schemas.BifrostImageTaskResponse, *schemas.BifrostError) {
	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)
	submittedAt := time.Now().Unix()
	body, latency, bifrostErr := provider.doDashScopeRequest(ctx, key, requestType, http.MethodPost, dashScopeImageTaskPath, jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	var task DashScopeImageTaskResponse
	rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &task, jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	if task.Output == nil || task.Output.TaskID == "" {
		if task.Code != "" {
			taskErr := &DashScopeTaskResponse{Code: task.Code, Message: task.Message}
			return nil, providerUtils.EnrichError(ctx, dashScopeErrorObjectError(taskErr), jsonData, body, sendBackRawRequest, sendBackRawResponse, latency)
		}
		return nil, providerUtils.EnrichError(ctx, providerUtils.NewBifrostOperationError("dashscope returned no task_id", nil), jsonData, body, sendBackRawRequest, sendBackRawResponse, latency)
	}
	result := toBifrostImageTask(&task)
	result.ID = providerUtils.AddVideoIDProviderSuffix(task.Output.TaskID, provider.GetProviderKey())
	result.Model = model
	result.BilledImages = billedImages
	result.BilledParameters = billedParams
	if result.CreatedAt == 0 {
		// The create response carries no submit_time; a later query reports the vendor's.
		result.CreatedAt = submittedAt
	}
	result.ExtraFields.Latency = latency.Milliseconds()
	if sendBackRawRequest {
		result.ExtraFields.RawRequest = rawRequest
	}
	if sendBackRawResponse {
		result.ExtraFields.RawResponse = rawResponse
	}
	return result, nil
}

// ImageTaskRetrieve queries a task once: GET /api/v1/tasks/{id}.
func (provider *DashScopeProvider) ImageTaskRetrieve(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostImageTaskRetrieveRequest) (*schemas.BifrostImageTaskResponse, *schemas.BifrostError) {
	providerName := provider.GetProviderKey()
	taskID := providerUtils.StripVideoIDProviderSuffix(request.ID, providerName)
	escapedTaskID, idErr := providerUtils.EscapeResourceID(taskID, "task_id")
	if idErr != nil {
		return nil, idErr
	}
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)
	body, latency, bifrostErr := provider.doDashScopeRequest(ctx, key, schemas.ImageTaskRetrieveRequest, http.MethodGet, dashScopeTasksPath+"/"+escapedTaskID, nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	var task DashScopeImageTaskResponse
	_, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &task, nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	if task.Output == nil {
		if task.Code != "" {
			taskErr := &DashScopeTaskResponse{Code: task.Code, Message: task.Message}
			return nil, providerUtils.EnrichError(ctx, dashScopeErrorObjectError(taskErr), nil, body, false, sendBackRawResponse, latency)
		}
		return nil, providerUtils.EnrichError(ctx, providerUtils.NewBifrostOperationError("dashscope returned no task", nil), nil, body, false, sendBackRawResponse, latency)
	}
	result := toBifrostImageTask(&task)
	result.ID = providerUtils.AddVideoIDProviderSuffix(taskID, providerName)
	result.ExtraFields.Latency = latency.Milliseconds()
	if sendBackRawResponse {
		result.ExtraFields.RawResponse = rawResponse
	}
	return result, nil
}

// ImageTaskCancel cancels a task with one call: POST /api/v1/tasks/{id}/cancel. DashScope cancels
// only a PENDING task; for any other state it answers 400 UnsupportedOperation, returned as
// provider_cancel_unsupported. The cancel deletes nothing, so a finished task's images stay.
func (provider *DashScopeProvider) ImageTaskCancel(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostImageTaskCancelRequest) (*schemas.BifrostImageTaskResponse, *schemas.BifrostError) {
	providerName := provider.GetProviderKey()
	taskID := providerUtils.StripVideoIDProviderSuffix(request.ID, providerName)
	escapedTaskID, idErr := providerUtils.EscapeResourceID(taskID, "task_id")
	if idErr != nil {
		return nil, idErr
	}
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)
	body, latency, bifrostErr := provider.doDashScopeRequest(ctx, key, schemas.ImageTaskCancelRequest, http.MethodPost, dashScopeTasksPath+"/"+escapedTaskID+"/cancel", nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		if isDashScopeCancelRefused(bifrostErr) {
			unsupported := schemas.NewImageTaskCancelUnsupportedError(bifrostErr.Error.Message)
			unsupported.ExtraFields.RawResponse = bifrostErr.ExtraFields.RawResponse
			unsupported.ExtraFields.Latency = bifrostErr.ExtraFields.Latency
			return nil, unsupported
		}
		return nil, bifrostErr
	}
	// DashScope's answer to a cancel is a request id and no task: the task object carries the id
	// and status cancelled, and no provider_status, since the vendor reported no state.
	result := &schemas.BifrostImageTaskResponse{
		ID:     providerUtils.AddVideoIDProviderSuffix(taskID, providerName),
		Object: schemas.ImageTaskObject,
		Status: schemas.VideoStatusCancelled,
		Data:   []schemas.TaskArtifact{},
	}
	result.ExtraFields.Latency = latency.Milliseconds()
	if sendBackRawResponse && len(body) > 0 {
		var rawResponse interface{}
		if err := schemas.Unmarshal(body, &rawResponse); err == nil {
			result.ExtraFields.RawResponse = rawResponse
		} else {
			result.ExtraFields.RawResponse = string(body)
		}
	}
	return result, nil
}

// toBifrostImageTask converts a Wan image task (a create answer or a query) to the task object,
// with no id suffix and no model: DashScope's task carries neither. The states map one to one, as
// for video (dashScopeVideoStatuses).
func toBifrostImageTask(task *DashScopeImageTaskResponse) *schemas.BifrostImageTaskResponse {
	output := task.Output
	result := &schemas.BifrostImageTaskResponse{
		Object:         schemas.ImageTaskObject,
		ProviderStatus: output.TaskStatus,
		CreatedAt:      parseDashScopeTaskTime(output.SubmitTime),
		Data:           []schemas.TaskArtifact{},
	}
	status, ok := dashScopeVideoStatuses.Lookup(output.TaskStatus)
	if !ok {
		// A state DashScope added that Bifrost does not know is not known to be terminal.
		status = schemas.VideoStatusInProgress
	}
	result.Status = status
	if status.IsTerminal() {
		if endTime := parseDashScopeTaskTime(output.EndTime); endTime > 0 {
			result.CompletedAt = schemas.Ptr(endTime)
		}
	}
	if output.TaskStatus == dashScopeTaskFailed && (output.Code != "" || output.Message != "") {
		result.Error = &schemas.TaskError{Code: output.Code, Message: output.Message}
		for _, code := range dashScopeContentFilterCodes {
			if output.Code == code {
				result.ContentFilter = &schemas.ContentFilterInfo{FilteredCount: 1, Reasons: []string{output.Code}}
			}
		}
	}
	if status == schemas.VideoStatusCompleted {
		for _, choice := range output.Choices {
			for _, part := range choice.Message.Content {
				if part.Image != "" {
					result.Data = append(result.Data, schemas.TaskArtifact{Type: schemas.VideoOutputTypeURL, URL: schemas.Ptr(part.Image), ContentType: wanImageContentType(part.Image)})
				}
			}
		}
		// The links live 24 hours from the task's completion.
		if result.CompletedAt != nil {
			result.ExpiresAt = schemas.Ptr(*result.CompletedAt + int64(dashScopeImageLinkLifetime/time.Second))
		}
	}
	if usage := task.Usage; usage != nil && usage.ImageCount != nil {
		result.Usage = &schemas.ImageUsage{OutputImages: usage.ImageCount}
	}
	return result
}

// wanImageContentType is a result link's image type, read from its path's extension, and PNG when
// the path names no extension Bifrost knows.
func wanImageContentType(link string) string {
	if parsed, err := url.Parse(link); err == nil {
		switch strings.ToLower(path.Ext(parsed.Path)) {
		case ".jpg", ".jpeg":
			return "image/jpeg"
		case ".webp":
			return "image/webp"
		}
	}
	return "image/png"
}
