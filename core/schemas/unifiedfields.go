package schemas

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// A field that cannot be delivered is an error (contract C4, work item BF4). When a
// request carries a unified field the provider does not take for that operation, or a
// value it cannot send as given, Bifrost answers 400 caller_unsupported_parameter naming
// the field in error.param. It never drops the field, changes it to another value, or
// clamps an out-of-range value into range.
//
// A provider opts in per operation by implementing UnifiedFieldDeclarer. Core checks the
// declaration in the request worker before a key is chosen, so a rejected request makes
// no upstream call. A provider that declares nothing for an operation is not checked.

// UnifiedFieldDeclarer is implemented by a provider that declares, per operation, which
// unified request fields it takes.
type UnifiedFieldDeclarer interface {
	// SupportedUnifiedFields returns the JSON names of the Params fields the provider
	// takes for requestType (the stream variant of an operation is its own request type).
	// declared is false when the provider makes no declaration for requestType, which
	// leaves that operation unchecked.
	SupportedUnifiedFields(requestType RequestType) (fields []string, declared bool)
}

// UnpricedUnifiedFields are the unified fields contract C4 exempts: they affect neither
// the price nor task recovery, so a provider that cannot send one may ignore it and the
// check never rejects it. prompt is listed for operations whose Params carry it.
var UnpricedUnifiedFields = []string{"prompt", "seed", "negative_prompt"}

// NewUnsupportedParameterError is the error for a unified field the provider cannot
// deliver as given: 400, error_type caller_unsupported_parameter, error.param = param.
// message says why, for the caller; empty gives a default naming the field. Providers
// return it for an out-of-range value instead of clamping it.
func NewUnsupportedParameterError(param, message string) *BifrostError {
	if message == "" {
		message = fmt.Sprintf("parameter %q is not supported by this provider for this operation", param)
	}
	return &BifrostError{
		StatusCode: Ptr(400),
		Error: &ErrorField{
			Type:    Ptr("invalid_request_error"),
			Code:    Ptr(string(ErrorTypeCallerUnsupportedParameter)),
			Message: message,
			Param:   param,
		},
		ExtraFields: BifrostErrorExtraFields{ErrorType: ErrorTypeCallerUnsupportedParameter},
	}
}

// CheckUnifiedFields returns the caller_unsupported_parameter error for the first
// (alphabetically) Params field set on req that is neither in supported nor in
// UnpricedUnifiedFields, and nil when every set field is one of them. It walks the cached,
// name-sorted field list of the Params type directly, so a request that passes allocates
// nothing: the worker runs it on every request of an operation a provider declares.
func CheckUnifiedFields(req *BifrostRequest, supported []string) *BifrostError {
	params := req.paramsValue()
	if !params.IsValid() {
		return nil
	}
	for _, f := range unifiedParamFields(params.Type()) {
		fv, ok := fieldByIndex(params, f.index)
		if !ok || fieldIsUnset(fv) {
			continue
		}
		if slices.Contains(supported, f.name) || slices.Contains(UnpricedUnifiedFields, f.name) {
			continue
		}
		return NewUnsupportedParameterError(f.name, "")
	}
	return nil
}

// CheckDeclaredUnifiedFields runs CheckUnifiedFields against what provider declares for
// req's request type. Nil when the provider declares nothing for it.
func CheckDeclaredUnifiedFields(provider any, req *BifrostRequest) *BifrostError {
	declarer, ok := provider.(UnifiedFieldDeclarer)
	if !ok || req == nil {
		return nil
	}
	supported, declared := declarer.SupportedUnifiedFields(req.RequestType)
	if !declared {
		return nil
	}
	return CheckUnifiedFields(req, supported)
}

// priceAffectingUnifiedFields is contract C4's table of unified fields per operation,
// less the three it exempts (UnpricedUnifiedFields) and less the request's own input
// ("input", "file"), which every converter writes and the extra-params merge already
// protects. Each of these fields changes what the vendor bills, or, for the task
// control fields of a video create, whether the caller can recover the task.
//
// It is one table for every provider. A request type absent from it (chat, responses,
// rerank, ...) has no C4 fields.
var priceAffectingUnifiedFields = func() map[RequestType][]string {
	image := []string{"n", "size", "quality", "aspect_ratio", "response_format", "output_format", "background", "input_images", "mask", "resolution"}
	speech := []string{"voice", "response_format", "speed", "instructions", "language_code", "stream_format"}
	transcription := []string{"language", "response_format", "timestamp_granularities"}
	video := append([]string{"seconds", "size", "audio", "input_reference", "output_format", "aspect_ratio", "resolution", "last_frame", "reference_images"}, TaskControlFields...)
	return map[RequestType][]string{
		EmbeddingRequest:             {"dimensions", "encoding_format", "task_type"},
		ImageGenerationRequest:       image,
		ImageGenerationStreamRequest: image,
		ImageEditRequest:             image,
		ImageEditStreamRequest:       image,
		VideoGenerationRequest:       video,
		SpeechRequest:                speech,
		SpeechStreamRequest:          speech,
		TranscriptionRequest:         transcription,
		TranscriptionStreamRequest:   transcription,
	}
}()

// PriceAffectingUnifiedFields returns the unified fields contract C4 counts as
// price-affecting (or, for a video create, recovery-affecting) for requestType, and nil
// for an operation the table does not cover. The slice is shared; do not modify it.
func PriceAffectingUnifiedFields(requestType RequestType) []string {
	return priceAffectingUnifiedFields[requestType]
}

// VendorPriceFieldName is a vendor's own name for a price-affecting unified field, as one
// provider's converter reads it from the extra params on some operations.
type VendorPriceFieldName struct {
	Provider ModelProvider // the provider whose converter reads the name
	Name     string        // the vendor's name, as the converter reads it
	Field    string        // the unified field it stands for
	Requests []RequestType // the operations on which the converter reads it
}

// vendorPriceFieldNames maps each vendor-own spelling of a C4 price field that a
// converter reads from the extra params to the unified field (TXE-7065). The check below
// treats the vendor's name as the unified field: "ratio" on a BytePlus video is
// aspect_ratio, and is refused unless the request sent aspect_ratio with that value.
//
// A name is listed only when the provider's converter delivers the unified field, so a
// caller who is refused the vendor's name can send the unified one and get the same
// result. core's TestConvertersReadNoUnlistedVendorPriceName fails when a converter reads
// a price-looking extra param that is neither here nor in that test's list of names left
// out on purpose, with the reason for each.
var vendorPriceFieldNames = func() []VendorPriceFieldName {
	video := []RequestType{VideoGenerationRequest}
	speech := []RequestType{SpeechRequest, SpeechStreamRequest}
	transcription := []RequestType{TranscriptionRequest, TranscriptionStreamRequest}
	return []VendorPriceFieldName{
		{BytePlus, "ratio", "aspect_ratio", video},
		{MiniMax, "ratio", "aspect_ratio", video},
		{Gemini, "aspectRatio", "aspect_ratio", video},
		{Vertex, "aspectRatio", "aspect_ratio", video},
		{Runway, "references", "reference_images", video},
		{Runware, "outputFormat", "output_format", video},
		{Sarvam, "speaker", "voice", speech},
		{Sarvam, "pace", "speed", speech},
		{Sarvam, "target_language_code", "language_code", speech},
		{Sarvam, "language_code", "language", transcription},
	}
}()

// VendorPriceFieldNames returns the table of vendor-own names for price-affecting unified
// fields. The slice is shared; do not modify it.
func VendorPriceFieldNames() []VendorPriceFieldName {
	return vendorPriceFieldNames
}

// NestedPriceField is a field one provider's request body holds inside an object, written by
// its converter from a price-affecting unified field.
type NestedPriceField struct {
	Provider ModelProvider // the provider whose converter writes it
	Path     string        // where it is in the body, dotted: "parameters.durationSeconds"
	Field    string        // a unified field it is written from, named in the refusal
	Requests []RequestType // the operations on which the converter writes it
}

// nestedPriceFields lists, per provider, the body fields below the top level that a
// converter writes from a C4 price field (TXE-7124). Extra params are merged into the
// converted body object by object, so an extra param named after the parent object adds the
// nested field when the request left the unified one out: {"parameters":
// {"durationSeconds": 8}} on a Veo request with no seconds. The vendor bills for a value
// the request does not show, and the check on top-level names never sees it. The check
// below refuses every path listed here.
//
// A path is listed when the converter writes it from a unified field, so a caller who is
// refused can send that field. Paths through a list (Gemini's requests[], Vertex's
// instances[]) are not listed: the merge never reaches into a list, and an extra param
// naming one the converter built is refused as a conflict.
// A path under an object that is itself a unified price field (Google Cloud
// Text-to-Speech's voice.languageCode) is not listed either: the extra param naming that
// object is refused by the top-level rule.
//
// core's TestNestedPriceFieldsAreDeclaredAndRefused sends every C4 field through every
// provider's converter and fails when a field lands on a nested path that is not listed
// here, or when a listed path is one no converter writes.
var nestedPriceFields = func() []NestedPriceField {
	image := []RequestType{ImageGenerationRequest, ImageGenerationStreamRequest, ImageEditRequest, ImageEditStreamRequest}
	imageGeneration := []RequestType{ImageGenerationRequest, ImageGenerationStreamRequest}
	imageSync := []RequestType{ImageGenerationRequest, ImageEditRequest}
	video := []RequestType{VideoGenerationRequest}
	speech := []RequestType{SpeechRequest, SpeechStreamRequest}
	transcription := []RequestType{TranscriptionRequest, TranscriptionStreamRequest}
	embedding := []RequestType{EmbeddingRequest}
	fields := []NestedPriceField{
		{Bedrock, "imageGenerationConfig.numberOfImages", "n", imageGeneration},
		{Bedrock, "imageGenerationConfig.quality", "quality", imageGeneration},
		{Bedrock, "imageGenerationConfig.width", "size", imageGeneration},
		{Bedrock, "imageGenerationConfig.height", "size", imageGeneration},
		{Bedrock, "embeddingConfig.outputEmbeddingLength", "dimensions", embedding},
		{Bedrock, "singleEmbeddingParams.embeddingDimension", "dimensions", embedding},
		{Bedrock, "singleEmbeddingParams.embeddingPurpose", "task_type", embedding},
		{BytePlus, "sequential_image_generation_options.max_images", "n", image},
		{DashScope, "parameters.n", "n", imageSync},
		{DashScope, "parameters.size", "size", append(append([]RequestType(nil), video...), imageSync...)},
		{DashScope, "parameters.resolution", "resolution", video},
		{DashScope, "parameters.ratio", "aspect_ratio", video},
		{DashScope, "parameters.duration", "seconds", video},
		{DashScope, "parameters.audio", "audio", video},
		{DashScope, "input.voice", "voice", speech},
		{DashScope, "input.language_type", "language_code", speech},
		{DashScope, "parameters.asr_options.language", "language", transcription},
		{Elevenlabs, "voice_settings.speed", "speed", speech},
		{GoogleCloudTTS, "audioConfig.audioEncoding", "response_format", speech},
		{GoogleCloudTTS, "audioConfig.speakingRate", "speed", speech},
		{Kling, "settings.duration", "seconds", video},
		{Kling, "settings.resolution", "resolution", video},
		{Kling, "settings.aspect_ratio", "aspect_ratio", video},
		{Kling, "settings.audio", "audio", video},
		{MiniMax, "audio_setting.format", "response_format", speech},
		{MiniMax, "voice_setting.speed", "speed", speech},
		{MiniMax, "voice_setting.voice_id", "voice", speech},
		{Replicate, "input.aspect_ratio", "aspect_ratio", image},
		{Replicate, "input.resolution", "size", image},
		{Replicate, "input.number_of_images", "n", image},
		{Replicate, "input.quality", "quality", image},
		{Replicate, "input.output_format", "output_format", image},
		{Replicate, "input.background", "background", image},
		{Replicate, "input.duration", "seconds", video},
		{XAI, "output_format.codec", "response_format", speech},
	}
	// Gemini and Vertex run the same converters for Imagen, Gemini image models, Veo and Omni.
	for _, provider := range []ModelProvider{Gemini, Vertex} {
		fields = append(fields,
			NestedPriceField{provider, "parameters.sampleCount", "n", image},
			NestedPriceField{provider, "parameters.aspectRatio", "aspect_ratio", image},
			NestedPriceField{provider, "parameters.sampleImageSize", "size", image},
			NestedPriceField{provider, "parameters.outputOptions.mimeType", "output_format", image},
			NestedPriceField{provider, "generationConfig.imageConfig.aspectRatio", "aspect_ratio", image},
			NestedPriceField{provider, "generationConfig.imageConfig.imageSize", "resolution", image},
			NestedPriceField{provider, "parameters.aspectRatio", "aspect_ratio", video},
			NestedPriceField{provider, "parameters.durationSeconds", "seconds", video},
			NestedPriceField{provider, "parameters.generateAudio", "audio", video},
			// An Omni model's video create is an Interactions API body (TXE-6538).
			NestedPriceField{provider, "response_format.duration", "seconds", video},
			NestedPriceField{provider, "response_format.aspect_ratio", "aspect_ratio", video},
			NestedPriceField{provider, "response_format.resolution", "resolution", video},
		)
	}
	fields = append(fields,
		NestedPriceField{Gemini, "generationConfig.speechConfig.voice_config.prebuiltVoiceConfig.voiceName", "voice", speech},
		NestedPriceField{Vertex, "parameters.outputDimensionality", "dimensions", embedding},
	)
	return fields
}()

// NestedPriceFields returns the table of nested body fields that hold a price-affecting
// value. The slice is shared; do not modify it.
func NestedPriceFields() []NestedPriceField {
	return nestedPriceFields
}

// nestedPriceFieldsByProvider is nestedPriceFields per provider, each path split into its
// names, so a request for a provider with none reads one map entry.
var nestedPriceFieldsByProvider = func() map[ModelProvider][]nestedPricePath {
	byProvider := map[ModelProvider][]nestedPricePath{}
	for i := range nestedPriceFields {
		nested := &nestedPriceFields[i]
		byProvider[nested.Provider] = append(byProvider[nested.Provider], nestedPricePath{field: nested, names: strings.Split(nested.Path, ".")})
	}
	return byProvider
}()

type nestedPricePath struct {
	field *NestedPriceField
	names []string
}

// SameJSONFieldName reports whether a and b name the same field once case and
// underscores are ignored: "max_tokens", "maxTokens" and "MaxTokens" are one field.
// Case is folded the way the most permissive decoder folds it, Go's encoding/json,
// which uses Unicode simple folding: "\u017ftream" (long s) and "tas\u212a" (Kelvin
// sign) are read there as "stream" and "task", so they are the same field here too.
// It allocates nothing.
//
// It is the one comparison for a caller-supplied field name against a body field: the
// extra-params merge (core/providers/utils) and the nested price-field check below both use
// it, so a spelling the merge would apply to a body object is a spelling the check reads.
func SameJSONFieldName(a, b string) bool {
	i, j := 0, 0
	for {
		for i < len(a) && a[i] == '_' {
			i++
		}
		for j < len(b) && b[j] == '_' {
			j++
		}
		if i == len(a) || j == len(b) {
			return i == len(a) && j == len(b)
		}
		if ca, cb := a[i], b[j]; ca < utf8.RuneSelf && cb < utf8.RuneSelf {
			// Both ASCII, the usual case: no decoding.
			if ca != cb && foldJSONFieldRune(rune(ca)) != foldJSONFieldRune(rune(cb)) {
				return false
			}
			i++
			j++
			continue
		}
		ra, sizeA := utf8.DecodeRuneInString(a[i:])
		rb, sizeB := utf8.DecodeRuneInString(b[j:])
		if ra != rb && foldJSONFieldRune(ra) != foldJSONFieldRune(rb) {
			return false
		}
		i += sizeA
		j += sizeB
	}
}

// foldJSONFieldRune returns the smallest rune of r's simple case-folding orbit, so two
// runes that fold together map to one value ('K', 'k' and the Kelvin sign all give 'K').
func foldJSONFieldRune(r rune) rune {
	if r < utf8.RuneSelf {
		if 'a' <= r && r <= 'z' {
			return r - ('a' - 'A')
		}
		// 'K' and 'S' have non-ASCII members in their orbits, but they are the smallest.
		return r
	}
	smallest := r
	for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
		if next < smallest {
			smallest = next
		}
	}
	return smallest
}

// extraParamObject returns value as the JSON object the extra-params merge would apply, and
// whether it is one. The merge serializes each extra param and treats whatever comes out as
// an object as one, so a Go caller's map[string]int, struct or json.RawMessage is an object
// there exactly as a map[string]interface{} is, and must be one here. The values a JSON body
// decodes to are answered without serializing anything.
func extraParamObject(value interface{}) (map[string]interface{}, bool) {
	switch v := value.(type) {
	case map[string]interface{}:
		return v, true
	case nil, string, bool, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64,
		[]interface{}, []string, json.Number:
		return nil, false
	}
	encoded, err := MarshalSorted(value)
	if err != nil {
		return nil, false
	}
	trimmed := bytes.TrimSpace(encoded)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	var object map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if decoder.Decode(&object) != nil {
		return nil, false
	}
	return object, true
}

// nestedExtraParamSet reports whether extraParams sets a non-empty value at a body path,
// following objects only and reading each name as SameJSONFieldName does, which is how the
// merge reads it. Every spelling of
// a name is followed, not only the first: the merge applies each of them to the same body
// object, so {"Parameters": {}, "parameters": {"durationSeconds": 8}} sets the field. keys
// receives the names of the first such path in sorted order, as the caller spelled them.
func nestedExtraParamSet(current map[string]interface{}, names []string, keys []string) bool {
	name, last := names[0], len(names) == 1
	found := false
	var best [8]string
	for candidate, value := range current {
		if !SameJSONFieldName(candidate, name) {
			continue
		}
		// Not better than the path already found at this level: nothing under it is.
		if found && candidate >= keys[0] {
			continue
		}
		if last {
			if extraParamIsEmpty(value) {
				continue
			}
		} else {
			next, isObject := extraParamObject(value)
			if !isObject || !nestedExtraParamSet(next, names[1:], best[1:len(names)]) {
				continue
			}
			copy(keys[1:], best[1:len(names)])
		}
		keys[0], found = candidate, true
	}
	return found
}

// nestedPriceFieldAmong returns the first nested price field of baseProvider and
// requestType, in the caller's spelling and sorted order, that extraParams sets, and the
// unified field it stands for. "" when it sets none.
func nestedPriceFieldAmong(baseProvider ModelProvider, requestType RequestType, extraParams map[string]interface{}) (param, field string) {
	paths := nestedPriceFieldsByProvider[baseProvider]
	if len(paths) == 0 {
		return "", ""
	}
	// Only an extra param that holds an object can carry a nested field. The common
	// request has none, and the table is not read for it.
	hasObject := false
	for _, value := range extraParams {
		if object, isObject := extraParamObject(value); isObject && len(object) > 0 {
			hasObject = true
			break
		}
	}
	if !hasObject {
		return "", ""
	}
	var keys [8]string
	for i := range paths {
		path := &paths[i]
		if len(path.names) > len(keys) || !slices.Contains(path.field.Requests, requestType) {
			continue
		}
		if !nestedExtraParamSet(extraParams, path.names, keys[:len(path.names)]) {
			continue
		}
		spelled := strings.Join(keys[:len(path.names)], ".")
		if param == "" || spelled < param {
			param, field = spelled, path.field.Field
		}
	}
	return param, field
}

// priceFieldNamed returns the price-affecting unified field of requestType that an extra
// param named key stands for on baseProvider: the unified name itself in any letter
// case, or the provider's own name for it. "" when it stands for none.
func priceFieldNamed(baseProvider ModelProvider, requestType RequestType, table []string, key string) string {
	for _, candidate := range table {
		if strings.EqualFold(key, candidate) {
			return candidate
		}
	}
	if baseProvider == "" {
		return ""
	}
	for i := range vendorPriceFieldNames {
		alias := &vendorPriceFieldNames[i]
		if alias.Provider == baseProvider && strings.EqualFold(key, alias.Name) && slices.Contains(alias.Requests, requestType) {
			return alias.Field
		}
	}
	return ""
}

// CheckExtraParamPriceFields refuses a request that names a price-affecting unified
// field among its extra params without sending the field itself (contract C4, agreement
// three; TXE-7059). Such a field is merged into the vendor's body, or read there by the
// converter, with no unified field showing it: the vendor bills for a size, a count or
// a duration the gateway never saw. The refusal is 400 caller_unsupported_parameter
// naming the extra param in error.param, on every provider: there is nothing for a
// provider to declare. Core runs it in the request worker before a key is chosen, so a
// refused request makes no upstream call.
//
// Four things are not refused:
//
//   - A field the request did send, with the same value. The HTTP handlers also keep some
//     unified fields in the extra params under the same name (UnifiedImageFields,
//     UnifiedVideoGenerationFields), and converters read them there. A copy that differs
//     from what the request sent is refused: the converter would send the vendor the copy.
//   - A value the unified field cannot hold. Those fields decode tolerantly (TolerantString,
//     VideoReferenceImages): Runware 3D's numeric resolution and Runway's reference image
//     objects leave the typed field empty and travel as an extra param by design. Only a
//     value the field would have taken is the field under another route.
//   - An empty value (nil, "", [] or {}): it supplies no field. A client that serializes
//     an optional list as [] is not refused.
//   - A vendor's own name for the field that no converter is known to read. The unified
//     name is compared ignoring letter case only, so "Size" is size but "aspectRatio" is
//     not aspect_ratio by spelling. The names converters do read are listed per provider
//     in vendorPriceFieldNames and refused by CheckExtraParamPriceFieldsFor.
func CheckExtraParamPriceFields(req *BifrostRequest) *BifrostError {
	return CheckExtraParamPriceFieldsFor("", req)
}

// CheckExtraParamPriceFieldsFor is CheckExtraParamPriceFields for a request served by
// baseProvider (the base provider type, for a custom provider). It also refuses that
// provider's own names for the price-affecting fields (vendorPriceFieldNames), which its
// converter would read: a value under the vendor's name is no more visible on the request
// than one under the unified name. And it refuses a price field nested inside an extra
// param that names one of the provider's body objects (nestedPriceFields), whatever the
// request sent: the unified field is the only way to set it. The request worker calls this
// form.
func CheckExtraParamPriceFieldsFor(baseProvider ModelProvider, req *BifrostRequest) *BifrostError {
	if req == nil {
		return nil
	}
	table := priceAffectingUnifiedFields[req.RequestType]
	if len(table) == 0 {
		return nil
	}
	params := req.paramsValue()
	if bifrostErr := checkExtraParamPriceFields(baseProvider, req, table, params); bifrostErr != nil {
		return bifrostErr
	}
	// An embedding item may carry its own params.
	if req.EmbeddingRequest != nil {
		for i := range req.EmbeddingRequest.Input {
			item := req.EmbeddingRequest.Input[i].Params
			if item == nil || len(item.ExtraParams) == 0 {
				continue
			}
			if bifrostErr := checkExtraParamPriceFields(baseProvider, req, table, reflect.ValueOf(item).Elem()); bifrostErr != nil {
				return bifrostErr
			}
		}
	}
	return nil
}

func checkExtraParamPriceFields(baseProvider ModelProvider, req *BifrostRequest, table []string, params reflect.Value) *BifrostError {
	if !params.IsValid() {
		return nil
	}
	extraField := params.FieldByName("ExtraParams")
	if !extraField.IsValid() || extraField.Kind() != reflect.Map || extraField.Len() == 0 {
		return nil
	}
	extraParams, ok := extraField.Interface().(map[string]interface{})
	if !ok {
		return nil
	}
	// The common request names no C4 field among its extra params: nothing is sorted
	// or allocated for it.
	var refused, refusedField string
	for key, value := range extraParams {
		field := priceFieldNamed(baseProvider, req.RequestType, table, key)
		if field == "" || extraParamIsEmpty(value) {
			continue
		}
		if sent, isSet := unifiedFieldSent(req, params, field); isSet {
			// The request sent the field, and the handlers keep a copy among the extra
			// params for the converters that read it there. A copy that says something
			// else is not a copy: the converter would send the vendor a value the
			// unified field does not show.
			if sent.sameValueAs(value) {
				continue
			}
		} else if !unifiedFieldWouldHold(params, field, value) {
			continue
		}
		if refused == "" || key < refused {
			refused, refusedField = key, field
		}
	}
	if refused == "" {
		if nested, nestedField := nestedPriceFieldAmong(baseProvider, req.RequestType, extraParams); nested != "" {
			return NewUnsupportedParameterError(nested, fmt.Sprintf(
				"extra params cannot set %q: it affects the price of this request, so it is set through the request's own %s field, never through an extra param",
				nested, nestedField))
		}
		return nil
	}
	return NewUnsupportedParameterError(refused, fmt.Sprintf(
		"extra params cannot set %q: %s affects the price of this request, so it is sent as the request's own %s field, never as an extra param",
		refused, refusedField, refusedField))
}

// extraParamIsEmpty reports whether an extra param carries nothing: nil, "", or an empty
// list or object. The handlers keep a unified field among the extra params even when the
// caller sent it empty (reference_images: []), and an empty value supplies no field.
func extraParamIsEmpty(value interface{}) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case []interface{}:
		return len(v) == 0
	case []string:
		return len(v) == 0
	case map[string]interface{}:
		return len(v) == 0
	}
	return false
}

// sentUnifiedField is the value of a unified field the request sent.
type sentUnifiedField struct {
	text   string        // the value, when it is a string
	isText bool          // whether it is a string
	value  reflect.Value // the field itself, otherwise
}

// sameValueAs reports whether an extra param's value is a copy of the field the request
// sent. Two strings are compared directly, which is the copy the handlers keep. Anything
// else is compared as JSON, so 2 and 2.0, or a []string and the []interface{} a JSON body
// decodes to, are the same value; a value that cannot be compared is not a copy.
func (f sentUnifiedField) sameValueAs(extra interface{}) bool {
	if planted, isString := extra.(string); isString && f.isText {
		return planted == f.text
	}
	var sent interface{} = f.text
	if !f.isText {
		if !f.value.IsValid() || !f.value.CanInterface() {
			return false
		}
		sent = f.value.Interface()
	}
	sentJSON, err := MarshalSorted(sent)
	if err != nil {
		return false
	}
	extraJSON, err := MarshalSorted(extra)
	if err != nil {
		return false
	}
	var sentValue, extraValue interface{}
	if Unmarshal(sentJSON, &sentValue) != nil || Unmarshal(extraJSON, &extraValue) != nil {
		return false
	}
	return reflect.DeepEqual(sentValue, extraValue)
}

// unifiedFieldSent reports whether the request sent the unified field named field (a
// Params field that is set, or the one C4 field that lives on the request's input), and
// the value it sent.
func unifiedFieldSent(req *BifrostRequest, params reflect.Value, field string) (sentUnifiedField, bool) {
	if field == "input_reference" && req.VideoGenerationRequest != nil {
		input := req.VideoGenerationRequest.Input
		if input == nil || input.InputReference == nil || *input.InputReference == "" {
			return sentUnifiedField{}, false
		}
		return sentUnifiedField{text: *input.InputReference, isText: true}, true
	}
	for _, f := range unifiedParamFields(params.Type()) {
		if f.name != field {
			continue
		}
		fv, ok := fieldByIndex(params, f.index)
		if !ok || fieldIsUnset(fv) {
			return sentUnifiedField{}, false
		}
		// A pointer is kept as it is for the comparison, so a type whose MarshalJSON has
		// a pointer receiver (the speech voice) is encoded as the wire shows it.
		elem := fv
		if elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}
		if elem.Kind() == reflect.String {
			return sentUnifiedField{text: elem.String(), isText: true}, true
		}
		return sentUnifiedField{value: fv}, true
	}
	return sentUnifiedField{}, false
}

// unifiedFieldWouldHold reports whether the unified field named field would have taken
// value had the request sent it there. The fork's tolerant types take one shape only and
// leave anything else to the extra params: a TolerantString takes a string, and
// VideoReferenceImages an array of strings (Runway's [{"uri": ..., "tag": ...}] objects
// travel as an extra param by design). Every other field is taken to hold what it is given.
func unifiedFieldWouldHold(params reflect.Value, field string, value interface{}) bool {
	for _, f := range unifiedParamFields(params.Type()) {
		if f.name != field {
			continue
		}
		fv, ok := fieldByIndex(params, f.index)
		if !ok {
			return true
		}
		switch fv.Type() {
		case reflect.TypeOf(TolerantString("")):
			_, isString := value.(string)
			return isString
		case reflect.TypeOf(VideoReferenceImages(nil)):
			switch list := value.(type) {
			case []string:
				return true
			case []interface{}:
				for _, entry := range list {
					if _, isString := entry.(string); !isString {
						return false
					}
				}
				return true
			}
			return false
		}
		return true
	}
	return true
}

// UnifiedFieldsPresent returns, sorted, the JSON names of the fields set on the request's
// Params: non-nil pointers, non-empty slices and maps, and non-zero values. Fields that
// are not serialized (json:"-", such as ExtraParams) are not unified fields and are
// skipped. A request with no Params has none.
func (br *BifrostRequest) UnifiedFieldsPresent() []string {
	params := br.paramsValue()
	if !params.IsValid() {
		return nil
	}
	list := unifiedParamFields(params.Type())
	fields := make([]string, 0, len(list))
	for _, f := range list {
		fv, ok := fieldByIndex(params, f.index)
		if !ok || fieldIsUnset(fv) {
			continue
		}
		fields = append(fields, f.name)
	}
	slices.Sort(fields)
	return fields
}

// paramsValue returns the Params struct of the one operation request br carries, or the
// zero Value when it carries none or that request has no Params.
func (br *BifrostRequest) paramsValue() reflect.Value {
	if br == nil {
		return reflect.Value{}
	}
	v := reflect.ValueOf(br).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if f.Kind() != reflect.Pointer || f.IsNil() || f.Elem().Kind() != reflect.Struct {
			continue
		}
		params := f.Elem().FieldByName("Params")
		if !params.IsValid() {
			continue
		}
		if params.Kind() == reflect.Pointer {
			if params.IsNil() {
				return reflect.Value{}
			}
			params = params.Elem()
		}
		if params.Kind() != reflect.Struct {
			return reflect.Value{}
		}
		return params
	}
	return reflect.Value{}
}

// unifiedParamField is one serialized field of a Params struct: its JSON name and its
// index path (through embedded structs, as encoding/json flattens them).
type unifiedParamField struct {
	name  string
	index []int
}

// unifiedParamFieldsByType caches unifiedParamFields per Params type, so the tags are read
// once per type, not once per request. Bounded by the number of Params types (one per
// operation).
var unifiedParamFieldsByType sync.Map // reflect.Type -> []unifiedParamField

// unifiedParamFields lists the serialized fields of Params struct type t, sorted by JSON name.
func unifiedParamFields(t reflect.Type) []unifiedParamField {
	if cached, ok := unifiedParamFieldsByType.Load(t); ok {
		return cached.([]unifiedParamField)
	}
	var fields []unifiedParamField
	collectParamFields(t, nil, &fields)
	slices.SortStableFunc(fields, func(a, b unifiedParamField) int { return strings.Compare(a.name, b.name) })
	actual, _ := unifiedParamFieldsByType.LoadOrStore(t, fields)
	return actual.([]unifiedParamField)
}

// collectParamFields appends the serialized fields of struct type t, descending into
// embedded structs the way encoding/json flattens them.
func collectParamFields(t reflect.Type, prefix []int, out *[]unifiedParamField) {
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		name, skip := jsonFieldName(sf)
		if skip {
			continue
		}
		index := append(append([]int(nil), prefix...), i)
		if sf.Anonymous && name == "" {
			ft := sf.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				collectParamFields(ft, index, out)
			}
			continue
		}
		if !sf.IsExported() {
			continue
		}
		if name == "" {
			name = sf.Name
		}
		*out = append(*out, unifiedParamField{name: name, index: index})
	}
}

// fieldByIndex follows index from v, reporting false when it passes a nil embedded pointer.
func fieldByIndex(v reflect.Value, index []int) (reflect.Value, bool) {
	for n, i := range index {
		if n > 0 && v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return reflect.Value{}, false
			}
			v = v.Elem()
		}
		v = v.Field(i)
	}
	return v, true
}

// jsonFieldName returns the field's JSON name ("" when the tag names none) and whether
// encoding/json skips it.
func jsonFieldName(sf reflect.StructField) (string, bool) {
	tag, ok := sf.Tag.Lookup("json")
	if !ok {
		return "", false
	}
	if tag == "-" {
		return "", true
	}
	name, _, _ := strings.Cut(tag, ",")
	return name, false
}

// fieldIsUnset reports whether a Params field carries nothing: a nil pointer, an empty
// slice or map, or a zero value.
func fieldIsUnset(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		return v.IsNil()
	case reflect.Slice, reflect.Map:
		return v.Len() == 0
	}
	return v.IsZero()
}

// Vendor-200 errors (contract C6, convention one). A vendor that answers 200 without a
// normal response is turned into an error here, so every provider reports it the same way.

// NewProviderEmptyResultError is the error for a vendor 200 that carries no output and no
// error object (an ElevenLabs 200 with no audio): 502, error_type provider_empty_result.
// The vendor answered, so it is not a Bifrost failure (is_bifrost_error stays false).
func NewProviderEmptyResultError(message string) *BifrostError {
	if message == "" {
		message = "the provider returned a successful response with no output"
	}
	return &BifrostError{
		StatusCode: Ptr(502),
		Error: &ErrorField{
			Type:    Ptr(string(ErrorTypeProviderEmptyResult)),
			Message: message,
		},
		ExtraFields: BifrostErrorExtraFields{ErrorType: ErrorTypeProviderEmptyResult},
	}
}

// VendorErrorCause says whose fault an error object inside a vendor 200 is.
type VendorErrorCause int

const (
	// VendorErrorCauseProvider: the vendor failed. 502, provider_server_error.
	VendorErrorCauseProvider VendorErrorCause = iota
	// VendorErrorCauseCaller: the caller's parameters. 400, caller_invalid_request.
	VendorErrorCauseCaller
	// VendorErrorCauseContentFiltered: the vendor blocked the caller's content. 400,
	// caller_content_filtered.
	VendorErrorCauseContentFiltered
)

// NewVendorErrorObjectError is the error for a vendor 200 that carries an error object
// and no normal response (MiniMax's base_resp.status_code ≠ 0): 400 when the caller caused
// it, 502 when the vendor did. error.code carries the vendor's code verbatim. Each
// provider decides the cause from its own codes.
func NewVendorErrorObjectError(cause VendorErrorCause, vendorCode, message string) *BifrostError {
	status, errorType := 502, ErrorTypeProviderServerError
	switch cause {
	case VendorErrorCauseCaller:
		status, errorType = 400, ErrorTypeCallerInvalidRequest
	case VendorErrorCauseContentFiltered:
		status, errorType = 400, ErrorTypeCallerContentFiltered
	}
	field := &ErrorField{Message: message}
	if vendorCode != "" {
		field.Code = Ptr(vendorCode)
	}
	return &BifrostError{
		StatusCode:  Ptr(status),
		Error:       field,
		ExtraFields: BifrostErrorExtraFields{ErrorType: errorType},
	}
}

// ErrorExtraFieldsFromResponse carries a finished response's routed identity onto the
// error that replaces it, so the error reports the provider, model and route that served.
func ErrorExtraFieldsFromResponse(extra BifrostResponseExtraFields, errorType ErrorType) BifrostErrorExtraFields {
	return BifrostErrorExtraFields{
		RoutingInfo:            extra.RoutingInfo,
		Provider:               extra.Provider,
		OriginalModelRequested: extra.OriginalModelRequested,
		ResolvedModelUsed:      extra.ResolvedModelUsed,
		RequestType:            extra.RequestType,
		Latency:                extra.Latency,
		RawResponse:            extra.RawResponse,
		ErrorType:              errorType,
	}
}
