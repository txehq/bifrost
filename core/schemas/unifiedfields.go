package schemas

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
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

// UnifiedFieldsPresent returns, sorted, the JSON names of the fields set on the request's
// Params: non-nil pointers, non-empty slices and maps, and non-zero values. Fields that
// are not serialized (json:"-", such as ExtraParams) are not unified fields and are
// skipped. A request with no Params has none.
func (br *BifrostRequest) UnifiedFieldsPresent() []string {
	params := br.paramsValue()
	if !params.IsValid() {
		return nil
	}
	var fields []string
	collectSetFields(params, &fields)
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

// collectSetFields appends the JSON names of the set fields of struct v, descending into
// embedded structs the way encoding/json flattens them.
func collectSetFields(v reflect.Value, out *[]string) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		fv := v.Field(i)
		name, skip := jsonFieldName(sf)
		if skip {
			continue
		}
		if sf.Anonymous && name == "" {
			if fv.Kind() == reflect.Pointer {
				if fv.IsNil() {
					continue
				}
				fv = fv.Elem()
			}
			if fv.Kind() == reflect.Struct {
				collectSetFields(fv, out)
			}
			continue
		}
		if !sf.IsExported() || fieldIsUnset(fv) {
			continue
		}
		if name == "" {
			name = sf.Name
		}
		*out = append(*out, name)
	}
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
