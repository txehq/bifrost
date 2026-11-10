package schemas

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
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
// UnpricedUnifiedFields, and nil when every set field is one of them.
func CheckUnifiedFields(req *BifrostRequest, supported []string) *BifrostError {
	for _, field := range req.UnifiedFieldsPresent() {
		if slices.Contains(supported, field) || slices.Contains(UnpricedUnifiedFields, field) {
			continue
		}
		return NewUnsupportedParameterError(field, "")
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

// CheckExtraParamPriceFields refuses a request that names a price-affecting unified
// field among its extra params without sending the field itself (contract C4, agreement
// three; TXE-7059). Such a field is merged into the vendor's body, or read there by the
// converter, with no unified field showing it: the vendor bills for a size, a count or
// a duration the gateway never saw. The refusal is 400 caller_unsupported_parameter
// naming the extra param in error.param, on every provider: there is nothing for a
// provider to declare. Core runs it in the request worker before a key is chosen, so a
// refused request makes no upstream call.
//
// Three things are not refused:
//
//   - A field the request did send, with the same value. The HTTP handlers also keep some
//     unified fields in the extra params under the same name (UnifiedImageFields,
//     UnifiedVideoGenerationFields), and converters read them there. A string copy that
//     differs from the string the request sent is refused: the converter would send the
//     vendor the copy.
//   - A value the unified field cannot hold. Those fields decode tolerantly (TolerantString,
//     VideoReferenceImages): Runware 3D's numeric resolution and Runway's reference image
//     objects leave the typed field empty and travel as an extra param by design. Only a
//     value the field would have taken is the field under another route.
//   - A vendor's own name for the field (BytePlus ratio, Gemini aspectRatio). The name is
//     compared ignoring letter case only, so "Size" is size but "aspectRatio" is not
//     aspect_ratio.
func CheckExtraParamPriceFields(req *BifrostRequest) *BifrostError {
	if req == nil {
		return nil
	}
	table := priceAffectingUnifiedFields[req.RequestType]
	if len(table) == 0 {
		return nil
	}
	params := req.paramsValue()
	if bifrostErr := checkExtraParamPriceFields(req, table, params); bifrostErr != nil {
		return bifrostErr
	}
	// An embedding item may carry its own params.
	if req.EmbeddingRequest != nil {
		for i := range req.EmbeddingRequest.Input {
			item := req.EmbeddingRequest.Input[i].Params
			if item == nil || len(item.ExtraParams) == 0 {
				continue
			}
			if bifrostErr := checkExtraParamPriceFields(req, table, reflect.ValueOf(item).Elem()); bifrostErr != nil {
				return bifrostErr
			}
		}
	}
	return nil
}

func checkExtraParamPriceFields(req *BifrostRequest, table []string, params reflect.Value) *BifrostError {
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
		field := ""
		for _, candidate := range table {
			if strings.EqualFold(key, candidate) {
				field = candidate
				break
			}
		}
		if field == "" || value == nil {
			continue
		}
		if sent, isSet := unifiedFieldSent(req, params, field); isSet {
			// The request sent the field, and the handlers keep a copy among the extra
			// params for the converters that read it there. A copy that says something
			// else is not a copy: the converter would send the vendor a value the
			// unified field does not show.
			if planted, isString := value.(string); !isString || sent == "" || planted == sent {
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
		return nil
	}
	return NewUnsupportedParameterError(refused, fmt.Sprintf(
		"extra params cannot set %q: %s affects the price of this request, so it is sent as the request's own %s field, never as an extra param",
		refused, refusedField, refusedField))
}

// unifiedFieldSent reports whether the request sent the unified field named field (a
// Params field that is set, or the one C4 field that lives on the request's input), and
// its value when that value is a string, "" otherwise.
func unifiedFieldSent(req *BifrostRequest, params reflect.Value, field string) (value string, isSet bool) {
	if field == "input_reference" && req.VideoGenerationRequest != nil {
		input := req.VideoGenerationRequest.Input
		if input == nil || input.InputReference == nil || *input.InputReference == "" {
			return "", false
		}
		return *input.InputReference, true
	}
	for _, f := range unifiedParamFields(params.Type()) {
		if f.name != field {
			continue
		}
		fv, ok := fieldByIndex(params, f.index)
		if !ok || fieldIsUnset(fv) {
			return "", false
		}
		if fv.Kind() == reflect.Pointer {
			fv = fv.Elem()
		}
		if fv.Kind() == reflect.String {
			return fv.String(), true
		}
		return "", true
	}
	return "", false
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

// unifiedParamFields lists the serialized fields of Params struct type t.
func unifiedParamFields(t reflect.Type) []unifiedParamField {
	if cached, ok := unifiedParamFieldsByType.Load(t); ok {
		return cached.([]unifiedParamField)
	}
	var fields []unifiedParamField
	collectParamFields(t, nil, &fields)
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
