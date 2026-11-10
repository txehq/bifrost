package schemas

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
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
					// A value the unified field would have taken: a list of URLs for the one
					// list field, a string for every other.
					var planted interface{} = "planted"
					if field == "reference_images" {
						planted = []interface{}{"https://example.invalid/a.png"}
					}
					req := extraOnlyRequest(t, requestType, map[string]interface{}{spelling: planted, "watermark": false})
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
		{"reference image objects, which the unified list cannot hold, travel as an extra param by design", &BifrostRequest{
			RequestType: VideoGenerationRequest,
			VideoGenerationRequest: &BifrostVideoGenerationRequest{Input: &VideoGenerationInput{Prompt: "p"},
				Params: &VideoGenerationParameters{ExtraParams: map[string]interface{}{
					"reference_images": []interface{}{map[string]interface{}{"uri": "https://example.invalid/a.png", "tag": "fox"}},
				}}},
		}},
		{"a non-array reference_images value", &BifrostRequest{
			RequestType: VideoGenerationRequest,
			VideoGenerationRequest: &BifrostVideoGenerationRequest{Input: &VideoGenerationInput{Prompt: "p"},
				Params: &VideoGenerationParameters{ExtraParams: map[string]interface{}{"reference_images": "https://example.invalid/a.png"}}},
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
		{"a copy of a number the request sent, as an int and as the float a JSON body decodes to", imageReq(&ImageGenerationParameters{
			N: Ptr(2), ExtraParams: map[string]interface{}{"n": 2, "N": float64(2)},
		})},
		{"a copy of a list the request sent", &BifrostRequest{
			RequestType: VideoGenerationRequest,
			VideoGenerationRequest: &BifrostVideoGenerationRequest{Input: &VideoGenerationInput{Prompt: "p"},
				Params: &VideoGenerationParameters{ReferenceImages: VideoReferenceImages{"https://example.invalid/a.png"},
					ExtraParams: map[string]interface{}{"reference_images": []interface{}{"https://example.invalid/a.png"}}}},
		}},
		{"empty values the handlers mirror supply no field", &BifrostRequest{
			RequestType: VideoGenerationRequest,
			VideoGenerationRequest: &BifrostVideoGenerationRequest{Input: &VideoGenerationInput{Prompt: "p"},
				Params: &VideoGenerationParameters{ExtraParams: map[string]interface{}{
					"reference_images": []interface{}{}, "resolution": "", "aspect_ratio": "", "last_frame": map[string]interface{}{},
				}}},
		}},
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
		{"a copy that differs from a number the request sent", imageReq(&ImageGenerationParameters{
			N: Ptr(1), ExtraParams: map[string]interface{}{"n": 8},
		}), "n"},
		{"a copy that differs from a list the request sent", &BifrostRequest{
			RequestType: VideoGenerationRequest,
			VideoGenerationRequest: &BifrostVideoGenerationRequest{Input: &VideoGenerationInput{Prompt: "p"},
				Params: &VideoGenerationParameters{ReferenceImages: VideoReferenceImages{"https://example.invalid/a.png"},
					ExtraParams: map[string]interface{}{"reference_images": []interface{}{"https://example.invalid/b.png"}}}},
		}, "reference_images"},
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
		{"reference image URLs the unified list would have held", &BifrostRequest{
			RequestType: VideoGenerationRequest,
			VideoGenerationRequest: &BifrostVideoGenerationRequest{Input: &VideoGenerationInput{Prompt: "p"},
				Params: &VideoGenerationParameters{ExtraParams: map[string]interface{}{"reference_images": []interface{}{"https://example.invalid/a.png"}}}},
		}, "reference_images"},
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
	// TXE-7124: an extra param that holds an object is walked against the provider's nested
	// price fields. Gemini has ten for image generation; OpenAI has none.
	nestedCases := []struct {
		name     string
		provider ModelProvider
		req      *BifrostRequest
	}{
		{"image_object_extra_no_nested_fields_for_provider", OpenAI, imageReq(&ImageGenerationParameters{ExtraParams: map[string]interface{}{"metadata": map[string]interface{}{"trace": "a"}}})},
		{"image_object_extra_not_a_price_field", Gemini, imageReq(&ImageGenerationParameters{ExtraParams: map[string]interface{}{"parameters": map[string]interface{}{"addWatermark": false}}})},
		{"image_nested_refused", Gemini, imageReq(&ImageGenerationParameters{ExtraParams: map[string]interface{}{"parameters": map[string]interface{}{"sampleCount": 4}}})},
	}
	for _, tc := range nestedCases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = CheckExtraParamPriceFieldsFor(tc.provider, tc.req)
			}
		})
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				// The worker's form: the vendor name table is scanned for each extra
				// param that is not a unified name.
				_ = CheckExtraParamPriceFieldsFor(OpenAI, tc.req)
			}
		})
	}
}

// TXE-7065: a provider's own name for a price-affecting field is checked like the unified
// field, on that provider and those operations only. The loop runs over the table, so a
// name added there is tested without a new case.
func TestCheckExtraParamPriceFieldsForRefusesEveryVendorPriceName(t *testing.T) {
	if len(vendorPriceFieldNames) == 0 {
		t.Fatal("the vendor name table is empty")
	}
	for _, alias := range vendorPriceFieldNames {
		for _, requestType := range alias.Requests {
			name := string(alias.Provider) + "/" + string(requestType) + "/" + alias.Name
			t.Run(name, func(t *testing.T) {
				if !slices.Contains(priceAffectingUnifiedFields[requestType], alias.Field) {
					t.Fatalf("%s maps to %q, which is not a price-affecting field of %s", alias.Name, alias.Field, requestType)
				}
				// A value the unified field would have taken.
				var planted interface{} = "planted"
				if alias.Field == "reference_images" {
					planted = []interface{}{"https://example.invalid/a.png"}
				}
				req := extraOnlyRequest(t, requestType, map[string]interface{}{alias.Name: planted})

				bifrostErr := CheckExtraParamPriceFieldsFor(alias.Provider, req)
				if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 ||
					bifrostErr.ExtraFields.ErrorType != ErrorTypeCallerUnsupportedParameter || bifrostErr.Error.Param != alias.Name {
					t.Fatalf("want 400 %s with error.param %q, got %+v", ErrorTypeCallerUnsupportedParameter, alias.Name, bifrostErr)
				}
				if !strings.Contains(bifrostErr.Error.Message, alias.Field) {
					t.Errorf("message %q does not name the unified field %q to send instead", bifrostErr.Error.Message, alias.Field)
				}
				// The name means nothing on a provider whose converter does not read it.
				if alias.Name != alias.Field {
					if other := CheckExtraParamPriceFieldsFor("some-other-provider", req); other != nil {
						t.Errorf("refused on a provider that does not read %q: %+v", alias.Name, other)
					}
					if unscoped := CheckExtraParamPriceFields(req); unscoped != nil {
						t.Errorf("refused with no provider given: %+v", unscoped)
					}
				}
			})
		}
	}
}

func TestCheckExtraParamPriceFieldsForVendorNameBesideTheUnifiedField(t *testing.T) {
	video := func(params *VideoGenerationParameters) *BifrostRequest {
		return &BifrostRequest{RequestType: VideoGenerationRequest,
			VideoGenerationRequest: &BifrostVideoGenerationRequest{Input: &VideoGenerationInput{Prompt: "p"}, Params: params}}
	}
	speech := func(params *SpeechParameters) *BifrostRequest {
		return &BifrostRequest{RequestType: SpeechRequest,
			SpeechRequest: &BifrostSpeechRequest{Input: &SpeechInput{Input: "hi"}, Params: params}}
	}
	embedding := func(params *EmbeddingParameters, items ...EmbeddingInputItem) *BifrostRequest {
		text := "hi"
		input := append(EmbeddingInput{{Content: EmbeddingContent{{Type: EmbeddingContentPartTypeText, Text: &text}}}}, items...)
		return &BifrostRequest{RequestType: EmbeddingRequest,
			EmbeddingRequest: &BifrostEmbeddingRequest{Input: input, Params: params}}
	}
	voice := "anushka"
	accepted := []struct {
		name     string
		provider ModelProvider
		req      *BifrostRequest
	}{
		{"the vendor's name with the value the request sent", BytePlus, video(&VideoGenerationParameters{
			AspectRatio: "9:16", ExtraParams: map[string]interface{}{"ratio": "9:16", "aspect_ratio": "9:16"}})},
		{"a number under the vendor's name equal to the unified one", Sarvam, speech(&SpeechParameters{
			Speed: Ptr(1.25), ExtraParams: map[string]interface{}{"pace": 1.25}})},
		{"the voice, whose unified field is not a plain string", Sarvam, speech(&SpeechParameters{
			VoiceConfig: &SpeechVoiceInput{Voice: &voice}, ExtraParams: map[string]interface{}{"speaker": "anushka"}})},
		{"reference objects under Runway's own name, which the unified list cannot hold", Runway, video(&VideoGenerationParameters{
			ExtraParams: map[string]interface{}{"references": []interface{}{map[string]interface{}{"uri": "https://example.invalid/a.png", "tag": "fox"}}}})},
		{"Gemini's task type beside the typed task_type", Gemini, embedding(&EmbeddingParameters{
			TaskType: Ptr("RETRIEVAL_DOCUMENT"), ExtraParams: map[string]interface{}{"taskType": "RETRIEVAL_DOCUMENT"}})},
		{"the task type name on a provider whose converter does not read it", OpenAI, embedding(&EmbeddingParameters{
			ExtraParams: map[string]interface{}{"taskType": "RETRIEVAL_DOCUMENT"}})},
		{"an operation the name is not read on", Runware, imageReq(&ImageGenerationParameters{
			ExtraParams: map[string]interface{}{"outputFormat": "WEBP"}})},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			if bifrostErr := CheckExtraParamPriceFieldsFor(tc.provider, tc.req); bifrostErr != nil {
				t.Fatalf("refused: %+v", bifrostErr)
			}
		})
	}
	refused := []struct {
		name      string
		provider  ModelProvider
		req       *BifrostRequest
		wantParam string
	}{
		{"the vendor's name with another value than the request sent", MiniMax, video(&VideoGenerationParameters{
			AspectRatio: "16:9", ExtraParams: map[string]interface{}{"ratio": "9:16"}}), "ratio"},
		{"Gemini's task type without the typed task_type", Gemini, embedding(&EmbeddingParameters{
			ExtraParams: map[string]interface{}{"taskType": "RETRIEVAL_DOCUMENT"}}), "taskType"},
		{"Gemini's task type with another value than the typed task_type", Gemini, embedding(&EmbeddingParameters{
			TaskType: Ptr("RETRIEVAL_QUERY"), ExtraParams: map[string]interface{}{"taskType": "RETRIEVAL_DOCUMENT"}}), "taskType"},
		{"Gemini's task type in one input item's own params", Gemini, embedding(nil, EmbeddingInputItem{
			Params: &EmbeddingParameters{ExtraParams: map[string]interface{}{"taskType": "RETRIEVAL_DOCUMENT"}}}), "taskType"},
		{"in any letter case", Gemini, video(&VideoGenerationParameters{
			ExtraParams: map[string]interface{}{"ASPECTRATIO": "9:16"}}), "ASPECTRATIO"},
		{"a pace that differs from the speed the request sent", Sarvam, speech(&SpeechParameters{
			Speed: Ptr(1.0), ExtraParams: map[string]interface{}{"pace": 2.5}}), "pace"},
		{"BFL's duration as a number, with no seconds sent (TXE-6617)", BFL, video(&VideoGenerationParameters{
			ExtraParams: map[string]interface{}{"duration": 10}}), "duration"},
		{"BFL's generate_audio, with no audio sent (TXE-6617)", BFL, video(&VideoGenerationParameters{
			ExtraParams: map[string]interface{}{"generate_audio": true}}), "generate_audio"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			bifrostErr := CheckExtraParamPriceFieldsFor(tc.provider, tc.req)
			if bifrostErr == nil || bifrostErr.Error.Param != tc.wantParam || bifrostErr.ExtraFields.ErrorType != ErrorTypeCallerUnsupportedParameter {
				t.Fatalf("want a refusal naming %q, got %+v", tc.wantParam, bifrostErr)
			}
		})
	}
}

// nestedExtra builds the extra params that set value at a dotted path, each segment spelled
// by spell: {"parameters": {"durationSeconds": 8}} for "parameters.durationSeconds".
func nestedExtra(path string, value interface{}, spell func(string) string) (extra map[string]interface{}, spelled string) {
	segments := strings.Split(path, ".")
	names := make([]string, len(segments))
	for i, segment := range segments {
		names[i] = spell(segment)
	}
	var current interface{} = value
	for i := len(names) - 1; i >= 0; i-- {
		current = map[string]interface{}{names[i]: current}
	}
	return current.(map[string]interface{}), strings.Join(names, ".")
}

// snakeCase and camelCase respell one body field name the other way.
func snakeCase(name string) string {
	var out strings.Builder
	for i, r := range name {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				out.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		out.WriteRune(r)
	}
	return out.String()
}

func camelCase(name string) string {
	parts := strings.Split(name, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

// TXE-7124: a price field nested inside an extra param that names one of the provider's
// body objects is refused, on that provider and those operations, however the caller spells
// the names, and whether or not the request also sent the unified field. The loop runs over
// the table, so a path added there is tested without a new case.
func TestCheckExtraParamPriceFieldsForRefusesEveryNestedPriceField(t *testing.T) {
	if len(nestedPriceFields) == 0 {
		t.Fatal("the nested price field table is empty")
	}
	spellings := map[string]func(string) string{
		"as listed":  func(s string) string { return s },
		"snake_case": snakeCase,
		"camelCase":  camelCase,
		"UPPER":      strings.ToUpper,
	}
	seen := map[string]bool{}
	for _, nested := range nestedPriceFields {
		if !strings.Contains(nested.Path, ".") {
			t.Errorf("%s %q is not nested; a top-level name belongs in vendorPriceFieldNames", nested.Provider, nested.Path)
		}
		for _, requestType := range nested.Requests {
			id := string(nested.Provider) + "/" + string(requestType) + "/" + nested.Path
			if seen[id] {
				t.Errorf("%s is listed twice", id)
			}
			seen[id] = true
			if !slices.Contains(priceAffectingUnifiedFields[requestType], nested.Field) {
				t.Errorf("%s names %q, which is not a price-affecting field of %s", id, nested.Field, requestType)
			}
			for label, spell := range spellings {
				t.Run(id+"/"+label, func(t *testing.T) {
					extra, spelled := nestedExtra(nested.Path, "planted", spell)
					extra["watermark"] = false
					req := extraOnlyRequest(t, requestType, extra)
					bifrostErr := CheckExtraParamPriceFieldsFor(nested.Provider, req)
					if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 ||
						bifrostErr.ExtraFields.ErrorType != ErrorTypeCallerUnsupportedParameter || bifrostErr.Error.Param != spelled {
						t.Fatalf("want 400 %s with error.param %q, got %+v", ErrorTypeCallerUnsupportedParameter, spelled, bifrostErr)
					}
					if !strings.Contains(bifrostErr.Error.Message, nested.Field) {
						t.Errorf("message %q does not name the unified field %q to send instead", bifrostErr.Error.Message, nested.Field)
					}
					// The path means nothing on a provider whose body has no such object.
					if other := CheckExtraParamPriceFieldsFor("some-other-provider", req); other != nil {
						t.Errorf("refused on a provider that has no such body field: %+v", other)
					}
					if unscoped := CheckExtraParamPriceFields(req); unscoped != nil {
						t.Errorf("refused with no provider given: %+v", unscoped)
					}
				})
			}
		}
	}
}

// What the nested check leaves alone, and what it does not let through.
func TestCheckExtraParamPriceFieldsForNestedBoundaries(t *testing.T) {
	video := func(params *VideoGenerationParameters) *BifrostRequest {
		return &BifrostRequest{RequestType: VideoGenerationRequest, VideoGenerationRequest: &BifrostVideoGenerationRequest{Input: &VideoGenerationInput{Prompt: "p"}, Params: params}}
	}
	object := func(pairs ...interface{}) map[string]interface{} {
		out := map[string]interface{}{}
		for i := 0; i < len(pairs); i += 2 {
			out[pairs[i].(string)] = pairs[i+1]
		}
		return out
	}
	accepted := []struct {
		name     string
		provider ModelProvider
		req      *BifrostRequest
	}{
		{"a sibling the converter does not write from a price field", Gemini, video(&VideoGenerationParameters{ExtraParams: object("parameters", object("personGeneration", "allow_adult", "negativePrompt", "blur"))})},
		{"an empty nested value supplies nothing", Gemini, video(&VideoGenerationParameters{ExtraParams: object("parameters", object("durationSeconds", nil, "aspectRatio", ""))})},
		{"an empty object", Gemini, video(&VideoGenerationParameters{ExtraParams: object("parameters", object())})},
		{"the parent is not an object", Gemini, video(&VideoGenerationParameters{ExtraParams: object("parameters", "durationSeconds")})},
		{"the same names under another parent", Gemini, video(&VideoGenerationParameters{ExtraParams: object("metadata", object("durationSeconds", 8))})},
		{"a path of another operation", Gemini, video(&VideoGenerationParameters{ExtraParams: object("parameters", object("sampleCount", 4))})},
		{"a typed map with no price field", Gemini, video(&VideoGenerationParameters{ExtraParams: object("parameters", map[string]string{"personGeneration": "allow_adult"})})},
		{"raw JSON that is not an object", Gemini, video(&VideoGenerationParameters{ExtraParams: object("parameters", json.RawMessage(`["durationSeconds"]`))})},
		{"a value that cannot be serialized", Gemini, video(&VideoGenerationParameters{ExtraParams: object("parameters", make(chan int))})},
		{"an operation outside the C4 table", Gemini, &BifrostRequest{RequestType: ChatCompletionRequest, ChatRequest: &BifrostChatRequest{Params: &ChatParameters{ExtraParams: object("generationConfig", object("imageConfig", object("aspectRatio", "16:9")))}}}},
	}
	for _, tc := range accepted {
		t.Run("accepted/"+tc.name, func(t *testing.T) {
			if bifrostErr := CheckExtraParamPriceFieldsFor(tc.provider, tc.req); bifrostErr != nil {
				t.Fatalf("refused: %+v", bifrostErr.Error)
			}
		})
	}
	refused := []struct {
		name     string
		provider ModelProvider
		req      *BifrostRequest
		param    string
	}{
		{"beside the unified field, with the same meaning", Gemini, video(&VideoGenerationParameters{Seconds: Ptr("8"), ExtraParams: object("parameters", object("durationSeconds", 8))}), "parameters.durationSeconds"},
		{"three levels down", Gemini, extraOnlyRequest(t, ImageGenerationRequest, object("generation_config", object("image_config", object("image_size", "4K")))), "generation_config.image_config.image_size"},
		{"false is a value", Gemini, video(&VideoGenerationParameters{ExtraParams: object("parameters", object("generateAudio", false))}), "parameters.generateAudio"},
		{"the first in sorted order is named", Gemini, video(&VideoGenerationParameters{ExtraParams: object("parameters", object("durationSeconds", 8, "aspectRatio", "9:16"))}), "parameters.aspectRatio"},
		{"a custom provider is checked as its base", Replicate, video(&VideoGenerationParameters{ExtraParams: object("input", object("duration", 10))}), "input.duration"},
		// The merge applies every spelling of a parent to the same body object, so an empty
		// one must not hide a populated one, whichever sorts first.
		{"an empty spelling of the parent before a populated one", Gemini, video(&VideoGenerationParameters{ExtraParams: object("Parameters", object(), "parameters", object("durationSeconds", 8))}), "parameters.durationSeconds"},
		{"an empty spelling of the parent after a populated one", Gemini, video(&VideoGenerationParameters{ExtraParams: object("PARAMETERS", object("durationSeconds", 8), "parameters", object())}), "PARAMETERS.durationSeconds"},
		{"an empty spelling of the field beside a set one", Gemini, video(&VideoGenerationParameters{ExtraParams: object("parameters", object("DurationSeconds", nil, "duration_seconds", 8))}), "parameters.duration_seconds"},
		{"a parent that is not an object beside one that is", Gemini, video(&VideoGenerationParameters{ExtraParams: object("Parameters", "x", "parameters", object("durationSeconds", 8))}), "parameters.durationSeconds"},
		// The merge serializes an extra param and applies whatever is a JSON object, so a Go
		// caller's typed map, struct or raw JSON is an object to the check too, at any level.
		{"a typed map", Gemini, video(&VideoGenerationParameters{ExtraParams: object("parameters", map[string]int{"durationSeconds": 8})}), "parameters.durationSeconds"},
		{"raw JSON", Gemini, video(&VideoGenerationParameters{ExtraParams: object("parameters", json.RawMessage(`{"durationSeconds": 8}`))}), "parameters.durationSeconds"},
		{"a struct", Gemini, video(&VideoGenerationParameters{ExtraParams: object("parameters", struct {
			DurationSeconds int `json:"durationSeconds"`
		}{8})}), "parameters.durationSeconds"},
		{"a pointer to a map", Gemini, video(&VideoGenerationParameters{ExtraParams: object("parameters", &map[string]interface{}{"durationSeconds": 8})}), "parameters.durationSeconds"},
		{"a typed map in the middle, three levels down", Gemini, extraOnlyRequest(t, ImageGenerationRequest, object("generationConfig", map[string]map[string]string{"imageConfig": {"imageSize": "4K"}})), "generationConfig.imageConfig.imageSize"},
		{"raw JSON in the middle", Gemini, extraOnlyRequest(t, ImageGenerationRequest, object("generationConfig", object("imageConfig", json.RawMessage(`{"imageSize":"4K"}`)))), "generationConfig.imageConfig.imageSize"},
		// The merge folds names as Go's encoding/json does, so the long s is an s.
		{"a parent spelled with a letter that folds to the same one", Gemini, video(&VideoGenerationParameters{ExtraParams: object("parameter\u017f", object("duration\u017feconds", 8))}), "parameter\u017f.duration\u017feconds"},
		{"two populated spellings: the first in sorted order is named", Gemini, video(&VideoGenerationParameters{ExtraParams: object("parameters", object("durationSeconds", 8), "Parameters", object("durationSeconds", 4))}), "Parameters.durationSeconds"},
		{"an empty middle object beside a populated one, three levels down", Gemini, extraOnlyRequest(t, ImageGenerationRequest, object("generationConfig", object("ImageConfig", object(), "imageConfig", object("imageSize", "4K")))), "generationConfig.imageConfig.imageSize"},
	}
	for _, tc := range refused {
		t.Run("refused/"+tc.name, func(t *testing.T) {
			bifrostErr := CheckExtraParamPriceFieldsFor(tc.provider, tc.req)
			if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 ||
				bifrostErr.ExtraFields.ErrorType != ErrorTypeCallerUnsupportedParameter || bifrostErr.Error.Param != tc.param {
				t.Fatalf("want 400 %s with error.param %q, got %+v", ErrorTypeCallerUnsupportedParameter, tc.param, bifrostErr)
			}
		})
	}
	// A top-level refusal wins: it is the older rule and names the simpler fix.
	both := video(&VideoGenerationParameters{ExtraParams: object("seconds", "8", "parameters", object("durationSeconds", 8))})
	if bifrostErr := CheckExtraParamPriceFieldsFor(Gemini, both); bifrostErr == nil || bifrostErr.Error.Param != "seconds" {
		t.Fatalf("want the top-level name refused first, got %+v", bifrostErr)
	}
}

// A refusal echoes at most a bounded amount of the caller's own input (fork, TXE-7142): a
// megabyte-scale, invalid UTF-8 field name or value does not make a megabyte-scale error.
func TestUnsupportedParameterErrorIsBounded(t *testing.T) {
	huge := strings.Repeat("\xff\x00a", 400000)
	err := NewUnsupportedParameterError(huge, "")
	param, _ := err.Error.Param.(string)
	if len(param) > MaxEchoedNameBytes || !utf8.ValidString(param) {
		t.Errorf("param is %d bytes (valid UTF-8 %v), want at most %d", len(param), utf8.ValidString(param), MaxEchoedNameBytes)
	}
	if len(err.Error.Message) > MaxRefusalMessageBytes {
		t.Errorf("default message is %d bytes, want at most %d", len(err.Error.Message), MaxRefusalMessageBytes)
	}
	err = NewUnsupportedParameterError("size", "size "+huge+" is not a size")
	if len(err.Error.Message) > MaxRefusalMessageBytes || !utf8.ValidString(err.Error.Message) {
		t.Errorf("message is %d bytes, want at most %d of valid UTF-8", len(err.Error.Message), MaxRefusalMessageBytes)
	}
	if got := NewUnsupportedParameterError("seconds", "seconds must be 4 to 15").Error; got.Param != "seconds" || got.Message != "seconds must be 4 to 15" {
		t.Errorf("a short refusal changed: %+v", got)
	}
	if got := BoundEchoedName("résumé"); got != "résumé" {
		t.Errorf("BoundEchoedName(%q) = %q, want it unchanged", "résumé", got)
	}
	if got := BoundEchoedName(strings.Repeat("é", 40)); len(got) > MaxEchoedNameBytes || !utf8.ValidString(got) {
		t.Errorf("BoundEchoedName cut a multibyte rune: %q", got)
	}
}

// BenchmarkNewUnsupportedParameterError is a refusal with an ordinary short message, the cost
// the bound adds to every caller_unsupported_parameter refusal (TXE-7142).
func BenchmarkNewUnsupportedParameterError(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = NewUnsupportedParameterError("seconds", "seconds must be a whole number between 4 and 15 for this model")
	}
}
