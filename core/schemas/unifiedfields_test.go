package schemas

import (
	"reflect"
	"slices"
	"strings"
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

// extraOnlyRequest builds a request of requestType whose only parameters are extra params.
func extraOnlyRequest(t testing.TB, requestType RequestType, extra map[string]interface{}) *BifrostRequest {
	t.Helper()
	req := &BifrostRequest{RequestType: requestType}
	switch requestType {
	case EmbeddingRequest:
		req.EmbeddingRequest = &BifrostEmbeddingRequest{Params: &EmbeddingParameters{ExtraParams: extra}}
	case ImageGenerationRequest, ImageGenerationStreamRequest:
		req.ImageGenerationRequest = &BifrostImageGenerationRequest{Input: &ImageGenerationInput{Prompt: "a cat"}, Params: &ImageGenerationParameters{ExtraParams: extra}}
	case ImageEditRequest, ImageEditStreamRequest:
		req.ImageEditRequest = &BifrostImageEditRequest{Input: &ImageEditInput{Prompt: "a cat"}, Params: &ImageEditParameters{ExtraParams: extra}}
	case VideoGenerationRequest:
		req.VideoGenerationRequest = &BifrostVideoGenerationRequest{Input: &VideoGenerationInput{Prompt: "a cat"}, Params: &VideoGenerationParameters{ExtraParams: extra}}
	case SpeechRequest, SpeechStreamRequest:
		req.SpeechRequest = &BifrostSpeechRequest{Input: &SpeechInput{Input: "hi"}, Params: &SpeechParameters{ExtraParams: extra}}
	case TranscriptionRequest, TranscriptionStreamRequest:
		req.TranscriptionRequest = &BifrostTranscriptionRequest{Input: &TranscriptionInput{File: []byte("a")}, Params: &TranscriptionParameters{ExtraParams: extra}}
	default:
		t.Fatalf("no builder for %s; add one when the C4 table gains an operation", requestType)
	}
	return req
}

// TXE-7059, contract C4 agreement three on the extra-param channel. For every operation
// in the table and every price-affecting field of it: the field named among the extra
// params of a request that did not send it is a 400 caller_unsupported_parameter naming
// the extra param as the caller spelled it. The loop runs over the table itself, so an
// operation or a field added there is tested without a new case.
func TestCheckExtraParamPriceFieldsRefusesEveryC4FieldOfEveryOperation(t *testing.T) {
	if len(priceAffectingUnifiedFields) == 0 {
		t.Fatal("the C4 table is empty")
	}
	for requestType, fields := range priceAffectingUnifiedFields {
		for _, field := range fields {
			for _, spelling := range []string{field, strings.ToUpper(field), strings.ToUpper(field[:1]) + field[1:]} {
				t.Run(string(requestType)+"/"+spelling, func(t *testing.T) {
					req := extraOnlyRequest(t, requestType, map[string]interface{}{spelling: "planted", "watermark": false})
					bifrostErr := CheckExtraParamPriceFields(req)
					if bifrostErr == nil {
						t.Fatalf("extra param %q was not refused", spelling)
					}
					if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 ||
						bifrostErr.ExtraFields.ErrorType != ErrorTypeCallerUnsupportedParameter || bifrostErr.Error.Param != spelling {
						t.Fatalf("want 400 %s with error.param %q, got %+v", ErrorTypeCallerUnsupportedParameter, spelling, bifrostErr)
					}
					if !strings.Contains(bifrostErr.Error.Message, field) {
						t.Errorf("message %q does not name the unified field %q", bifrostErr.Error.Message, field)
					}
				})
			}
		}
		// The fields C4 exempts are not refused on any operation.
		for _, unpriced := range UnpricedUnifiedFields {
			if bifrostErr := CheckExtraParamPriceFields(extraOnlyRequest(t, requestType, map[string]interface{}{unpriced: "x"})); bifrostErr != nil && !slices.Contains(fields, unpriced) {
				t.Errorf("%s: unpriced field %q among the extra params was refused: %+v", requestType, unpriced, bifrostErr)
			}
		}
	}
	for _, unpriced := range UnpricedUnifiedFields {
		for requestType, fields := range priceAffectingUnifiedFields {
			if slices.Contains(fields, unpriced) {
				t.Errorf("%s lists %q, which contract C4 exempts", requestType, unpriced)
			}
		}
	}
}

func TestCheckExtraParamPriceFieldsLeavesEverythingElseAlone(t *testing.T) {
	inputReference := "https://example.invalid/first.png"
	cases := []struct {
		name string
		req  *BifrostRequest
	}{
		{"no extra params", imageReq(&ImageGenerationParameters{Size: Ptr("1024x1024")})},
		{"no params at all", imageReq(nil)},
		{"a vendor switch with no general meaning", imageReq(&ImageGenerationParameters{ExtraParams: map[string]interface{}{"watermark": false, "camera_fixed": true}})},
		{"the request sent the field; the handlers keep it among the extra params too", imageReq(&ImageGenerationParameters{
			Resolution: "2K", ExtraParams: map[string]interface{}{"resolution": "2K"},
		})},
		{"the request sent size; a converter reads the mirror", imageReq(&ImageGenerationParameters{
			Size: Ptr("1024x1024"), ExtraParams: map[string]interface{}{"size": "1024x1024"},
		})},
		{"a value the tolerant unified field cannot hold travels as an extra param by design", &BifrostRequest{
			RequestType: VideoGenerationRequest,
			VideoGenerationRequest: &BifrostVideoGenerationRequest{Input: &VideoGenerationInput{Prompt: "p"},
				Params: &VideoGenerationParameters{ExtraParams: map[string]interface{}{"resolution": 1024}}},
		}},
		{"a vendor's own name for the field", &BifrostRequest{
			RequestType: VideoGenerationRequest,
			VideoGenerationRequest: &BifrostVideoGenerationRequest{Input: &VideoGenerationInput{Prompt: "p"},
				Params: &VideoGenerationParameters{ExtraParams: map[string]interface{}{"ratio": "16:9", "aspectRatio": "16:9", "lastFrame": "x", "referenceImages": []interface{}{"x"}}}},
		}},
		{"input_reference sent on the input", &BifrostRequest{
			RequestType: VideoGenerationRequest,
			VideoGenerationRequest: &BifrostVideoGenerationRequest{Input: &VideoGenerationInput{Prompt: "p", InputReference: &inputReference},
				Params: &VideoGenerationParameters{ExtraParams: map[string]interface{}{"input_reference": inputReference}}},
		}},
		{"the request sent a non-string field; its copy is not compared", imageReq(&ImageGenerationParameters{
			N: Ptr(2), ExtraParams: map[string]interface{}{"n": 2},
		})},
		{"a nil value", imageReq(&ImageGenerationParameters{ExtraParams: map[string]interface{}{"size": nil}})},
		{"an operation outside the C4 table", &BifrostRequest{
			RequestType: ChatCompletionRequest,
			ChatRequest: &BifrostChatRequest{Params: &ChatParameters{ExtraParams: map[string]interface{}{"size": "x", "n": 4, "voice": "x"}}},
		}},
		{"a field of another operation's table", extraOnlyRequest(t, EmbeddingRequest, map[string]interface{}{"size": "x", "voice": "x", "seconds": "8"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if bifrostErr := CheckExtraParamPriceFields(tc.req); bifrostErr != nil {
				t.Fatalf("refused: %+v", bifrostErr)
			}
		})
	}
	if bifrostErr := CheckExtraParamPriceFields(nil); bifrostErr != nil {
		t.Fatalf("nil request refused: %+v", bifrostErr)
	}
}

func TestCheckExtraParamPriceFieldsSpecificRefusals(t *testing.T) {
	text := "hi"
	cases := []struct {
		name      string
		req       *BifrostRequest
		wantParam string
	}{
		{"the first in sorted order is named", imageReq(&ImageGenerationParameters{ExtraParams: map[string]interface{}{"size": "x", "n": 4, "quality": "hd"}}), "n"},
		{"another field is sent, this one is not", imageReq(&ImageGenerationParameters{Size: Ptr("1024x1024"), ExtraParams: map[string]interface{}{"size": "1024x1024", "n": 4}}), "n"},
		{"a copy that differs from the unified field the request sent", imageReq(&ImageGenerationParameters{
			Resolution: "1K", ExtraParams: map[string]interface{}{"resolution": "4K"},
		}), "resolution"},
		{"a copy that differs from a pointer field the request sent", imageReq(&ImageGenerationParameters{
			Size: Ptr("1024x1024"), ExtraParams: map[string]interface{}{"Size": "4096x4096"},
		}), "Size"},
		{"an input_reference that differs from the one on the input", &BifrostRequest{
			RequestType: VideoGenerationRequest,
			VideoGenerationRequest: &BifrostVideoGenerationRequest{Input: &VideoGenerationInput{Prompt: "p", InputReference: Ptr("https://example.invalid/first.png")},
				Params: &VideoGenerationParameters{ExtraParams: map[string]interface{}{"input_reference": "https://example.invalid/other.png"}}},
		}, "input_reference"},
		{"a string the tolerant field would have held", &BifrostRequest{
			RequestType: VideoGenerationRequest,
			VideoGenerationRequest: &BifrostVideoGenerationRequest{Input: &VideoGenerationInput{Prompt: "p"},
				Params: &VideoGenerationParameters{ExtraParams: map[string]interface{}{"resolution": "1080p"}}},
		}, "resolution"},
		{"a task control field", &BifrostRequest{
			RequestType: VideoGenerationRequest,
			VideoGenerationRequest: &BifrostVideoGenerationRequest{Input: &VideoGenerationInput{Prompt: "p"},
				Params: &VideoGenerationParameters{ExtraParams: map[string]interface{}{"callback_url": "https://example.invalid/hook"}}},
		}, "callback_url"},
		{"input_reference not sent on the input", &BifrostRequest{
			RequestType: VideoGenerationRequest,
			VideoGenerationRequest: &BifrostVideoGenerationRequest{Input: &VideoGenerationInput{Prompt: "p"},
				Params: &VideoGenerationParameters{ExtraParams: map[string]interface{}{"input_reference": "https://example.invalid/a.png"}}},
		}, "input_reference"},
		{"an embedding item's own extra params", &BifrostRequest{
			RequestType: EmbeddingRequest,
			EmbeddingRequest: &BifrostEmbeddingRequest{Input: []EmbeddingInputItem{
				{Content: EmbeddingContent{{Type: EmbeddingContentPartTypeText, Text: &text}}},
				{Content: EmbeddingContent{{Type: EmbeddingContentPartTypeText, Text: &text}}, Params: &EmbeddingParameters{ExtraParams: map[string]interface{}{"dimensions": 256}}},
			}},
		}, "dimensions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bifrostErr := CheckExtraParamPriceFields(tc.req)
			if bifrostErr == nil || bifrostErr.Error.Param != tc.wantParam || bifrostErr.ExtraFields.ErrorType != ErrorTypeCallerUnsupportedParameter {
				t.Fatalf("want a refusal naming %q, got %+v", tc.wantParam, bifrostErr)
			}
		})
	}
}

// The request worker runs CheckExtraParamPriceFields on every request. Without extra
// params it must cost a map lookup; with extra params that name no C4 field, a scan of
// them and no allocation.
func BenchmarkCheckExtraParamPriceFields(b *testing.B) {
	cases := []struct {
		name string
		req  *BifrostRequest
	}{
		{"chat_not_in_table", &BifrostRequest{RequestType: ChatCompletionRequest, ChatRequest: &BifrostChatRequest{Params: &ChatParameters{ExtraParams: map[string]interface{}{"top_k": 4}}}}},
		{"image_no_extra_params", imageReq(&ImageGenerationParameters{N: Ptr(2), Size: Ptr("1024x1024")})},
		{"image_vendor_extra_params", imageReq(&ImageGenerationParameters{Size: Ptr("1024x1024"), ExtraParams: map[string]interface{}{"watermark": false, "camera_fixed": true}})},
		{"image_mirrored_field", imageReq(&ImageGenerationParameters{Resolution: "2K", ExtraParams: map[string]interface{}{"resolution": "2K"}})},
		{"image_refused", imageReq(&ImageGenerationParameters{ExtraParams: map[string]interface{}{"size": "4096x4096"}})},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = CheckExtraParamPriceFields(tc.req)
			}
		})
	}
}
