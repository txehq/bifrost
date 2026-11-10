package schemas

import (
	"reflect"
	"testing"
)

type declaringProvider struct {
	fields   map[RequestType][]string
	declares bool
}

func (d declaringProvider) SupportedUnifiedFields(rt RequestType) ([]string, bool) {
	f, ok := d.fields[rt]
	return f, ok && d.declares
}

func imageReq(params *ImageGenerationParameters) *BifrostRequest {
	return &BifrostRequest{
		RequestType: ImageGenerationRequest,
		ImageGenerationRequest: &BifrostImageGenerationRequest{
			Provider: OpenAI, Model: "m", Input: &ImageGenerationInput{Prompt: "a cat"}, Params: params,
		},
	}
}

func TestUnifiedFieldsPresentListsSetParamsOnly(t *testing.T) {
	req := imageReq(&ImageGenerationParameters{
		N:           Ptr(2),
		Size:        Ptr("1024x1024"),
		InputImages: []string{},                       // empty: not set
		ExtraParams: map[string]interface{}{"x": "y"}, // json:"-": not a unified field
	})
	got := req.UnifiedFieldsPresent()
	if want := []string{"n", "size"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("UnifiedFieldsPresent() = %v, want %v", got, want)
	}
	if got := imageReq(nil).UnifiedFieldsPresent(); len(got) != 0 {
		t.Fatalf("no Params gave %v", got)
	}
	video := &BifrostRequest{RequestType: VideoGenerationRequest, VideoGenerationRequest: &BifrostVideoGenerationRequest{
		Params: &VideoGenerationParameters{Size: "720x1280", Seconds: Ptr("8")},
	}}
	if got, want := video.UnifiedFieldsPresent(), []string{"seconds", "size"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("video UnifiedFieldsPresent() = %v, want %v", got, want)
	}
}

// BF4: a unified field the provider did not declare is a 400 caller_unsupported_parameter
// naming the field; the unpriced fields are never rejected.
func TestCheckDeclaredUnifiedFields(t *testing.T) {
	provider := declaringProvider{declares: true, fields: map[RequestType][]string{ImageGenerationRequest: {"size", "quality"}}}

	err := CheckDeclaredUnifiedFields(provider, imageReq(&ImageGenerationParameters{N: Ptr(2), Size: Ptr("1024x1024")}))
	if err == nil {
		t.Fatal("undeclared n was accepted")
	}
	if err.StatusCode == nil || *err.StatusCode != 400 {
		t.Fatalf("status = %v, want 400", err.StatusCode)
	}
	if err.ExtraFields.ErrorType != ErrorTypeCallerUnsupportedParameter {
		t.Fatalf("error_type = %q", err.ExtraFields.ErrorType)
	}
	if err.Error == nil || err.Error.Param != "n" {
		t.Fatalf("error.param = %v, want n", err.Error)
	}
	if got := ClassifyErrorType(err, ImageGenerationRequest); got != ErrorTypeCallerUnsupportedParameter {
		t.Fatalf("classified as %q", got)
	}

	if err := CheckDeclaredUnifiedFields(provider, imageReq(&ImageGenerationParameters{Size: Ptr("1024x1024"), Seed: Ptr(7), NegativePrompt: Ptr("dogs")})); err != nil {
		t.Fatalf("declared and unpriced fields were rejected: %v", err.Error.Param)
	}

	// No declaration for the operation, or no declarer at all: unchecked.
	undeclared := declaringProvider{declares: false, fields: provider.fields}
	if err := CheckDeclaredUnifiedFields(undeclared, imageReq(&ImageGenerationParameters{N: Ptr(2)})); err != nil {
		t.Fatal("a provider that declares nothing was checked")
	}
	if err := CheckDeclaredUnifiedFields(struct{}{}, imageReq(&ImageGenerationParameters{N: Ptr(2)})); err != nil {
		t.Fatal("a non-declarer was checked")
	}
}

func TestNewUnsupportedParameterErrorForOutOfRangeValue(t *testing.T) {
	err := NewUnsupportedParameterError("duration_seconds", "duration_seconds must be between 0.5 and 30")
	if err.IsBifrostError || *err.StatusCode != 400 || err.Error.Param != "duration_seconds" ||
		err.ExtraFields.ErrorType != ErrorTypeCallerUnsupportedParameter {
		t.Fatalf("unexpected error %+v", err)
	}
}

func TestVendor200Errors(t *testing.T) {
	empty := NewProviderEmptyResultError("")
	if *empty.StatusCode != 502 || empty.IsBifrostError || empty.ExtraFields.ErrorType != ErrorTypeProviderEmptyResult {
		t.Fatalf("empty result: %+v", empty)
	}
	cases := []struct {
		cause  VendorErrorCause
		status int
		want   ErrorType
	}{
		{VendorErrorCauseCaller, 400, ErrorTypeCallerInvalidRequest},
		{VendorErrorCauseContentFiltered, 400, ErrorTypeCallerContentFiltered},
		{VendorErrorCauseProvider, 502, ErrorTypeProviderServerError},
	}
	for _, tc := range cases {
		e := NewVendorErrorObjectError(tc.cause, "1026", "vendor says no")
		if *e.StatusCode != tc.status || e.ExtraFields.ErrorType != tc.want || e.Error.Code == nil || *e.Error.Code != "1026" {
			t.Errorf("cause %d: status %d type %q code %v", tc.cause, *e.StatusCode, e.ExtraFields.ErrorType, e.Error.Code)
		}
	}
}

// The request worker runs CheckDeclaredUnifiedFields on every request. A provider that
// declares nothing (every provider today) must cost only the interface check.
func BenchmarkCheckDeclaredUnifiedFields(b *testing.B) {
	req := imageReq(&ImageGenerationParameters{N: Ptr(2), Size: Ptr("1024x1024"), Quality: Ptr("high")})
	declarer := declaringProvider{declares: true, fields: map[RequestType][]string{ImageGenerationRequest: {"n", "size", "quality"}}}
	rejecting := declaringProvider{declares: true, fields: map[RequestType][]string{ImageGenerationRequest: {"size"}}}
	cases := []struct {
		name     string
		provider any
	}{
		{"non_declarer", struct{}{}},
		{"declarer_accepts", declarer},
		{"declarer_rejects", rejecting},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = CheckDeclaredUnifiedFields(tc.provider, req)
			}
		})
	}
}
