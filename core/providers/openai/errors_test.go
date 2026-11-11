package openai

import (
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

func TestResponsesStreamError_NormalizesAzureShapeAndEmptyDetails(t *testing.T) {
	cases := []struct {
		wire, wantMessage string
	}{
		{`{"type":"error","error":{"type":"too_many_requests","code":"no_capacity","message":"capacity"}}`, "capacity"},
		{`{"type":"response.failed","response":{"error":{"code":"context_length_exceeded","message":"input is too large"}}}`, "input is too large"},
		{`{"type":"error","error":{}}`, "provider stream error (error)"},
	}
	for _, tc := range cases {
		var response schemas.BifrostResponsesStreamResponse
		if err := schemas.Unmarshal([]byte(tc.wire), &response); err != nil {
			t.Fatalf("unmarshal stream error: %v", err)
		}
		got := responsesStreamError(&response)
		if got == nil || got.Error == nil || got.Error.Message != tc.wantMessage {
			t.Fatalf("normalized error = %v, want message %q", got, tc.wantMessage)
		}
	}
}

func TestParseOpenAIError_FallbackMessageWhenProviderBodyIsNonOpenAIShape(t *testing.T) {
	var resp fasthttp.Response
	resp.SetStatusCode(fasthttp.StatusUnprocessableEntity)
	resp.SetBodyString(`{"detail":[{"loc":["body","messages",0,"role"],"msg":"value is not a valid enumeration member"}]}`)

	errResp := ParseOpenAIError(&resp)
	if errResp == nil || errResp.Error == nil {
		t.Fatal("expected non-nil error response")
	}
	if errResp.Error.Message == "" {
		t.Fatal("expected non-empty error message")
	}
	if errResp.Error.Message != "provider API error (status 422)" {
		t.Fatalf("expected fallback message, got %q", errResp.Error.Message)
	}
}

func TestParseOpenAIError_PreservesProviderMessageWhenPresent(t *testing.T) {
	var resp fasthttp.Response
	resp.SetStatusCode(fasthttp.StatusUnprocessableEntity)
	resp.SetBodyString(`{"error":{"message":"unsupported role: developer","type":"invalid_request_error","param":"messages.0.role","code":"invalid_value"}}`)

	errResp := ParseOpenAIError(&resp)
	if errResp == nil || errResp.Error == nil {
		t.Fatal("expected non-nil error response")
	}
	if errResp.Error.Message != "unsupported role: developer" {
		t.Fatalf("expected provider message, got %q", errResp.Error.Message)
	}
}

func TestParseOpenAIError_FallbackMessageWhenBodyIsEmpty(t *testing.T) {
	var resp fasthttp.Response
	resp.SetStatusCode(fasthttp.StatusBadRequest)
	resp.SetBody(nil)

	errResp := ParseOpenAIError(&resp)
	if errResp == nil || errResp.Error == nil {
		t.Fatal("expected non-nil error response")
	}
	// HandleProviderAPIError returns ErrProviderResponseEmpty with HTTP status for empty bodies.
	expectedMsg := schemas.ErrProviderResponseEmpty + " (HTTP 400)"
	if errResp.Error.Message != expectedMsg {
		t.Fatalf("expected %q, got %q", expectedMsg, errResp.Error.Message)
	}
}

func TestParseOpenAIError_WhitespaceProviderMessageFallsBack(t *testing.T) {
	var resp fasthttp.Response
	resp.SetStatusCode(fasthttp.StatusBadRequest)
	resp.SetBodyString(`{"error":{"message":"   ","type":"invalid_request_error"}}`)

	errResp := ParseOpenAIError(&resp)
	if errResp == nil || errResp.Error == nil {
		t.Fatal("expected non-nil error response")
	}
	if errResp.Error.Message != "provider API error (status 400)" {
		t.Fatalf("expected fallback message, got %q", errResp.Error.Message)
	}
}

func TestParseOpenAIError_DefaultStatusCodeFallsBackWithStatusNumber(t *testing.T) {
	var resp fasthttp.Response
	// fasthttp defaults zero-value response status code to 200.
	resp.SetBodyString(`{"error":{"message":""}}`)

	errResp := ParseOpenAIError(&resp)
	if errResp == nil || errResp.Error == nil {
		t.Fatal("expected non-nil error response")
	}
	if errResp.Error.Message != "provider API error (status 200)" {
		t.Fatalf("expected fallback message with default status, got %q", errResp.Error.Message)
	}
}

// An SSE error rides a committed HTTP 200, so the error carries no status of its
// own. Left nil it reaches metrics as a caller 400 and ClassifyFailure has nothing
// to act on.
func TestResponsesStreamError_CarriesGatewayStatus(t *testing.T) {
	cases := []struct {
		name, wire string
		want       int
	}{
		{
			"overload with no recognizable signal defaults to 502",
			`{"type":"response.failed","response":{"error":{"code":"server_error","message":"Our servers are currently overloaded. Please try again later."}}}`,
			fasthttp.StatusBadGateway,
		},
		{
			"empty error object still gets a status",
			`{"type":"error","error":{}}`,
			fasthttp.StatusBadGateway,
		},
		{
			// 502 would read as transient and retry the same key instead of rotating.
			"rate limit by type keeps its 429",
			`{"type":"error","error":{"type":"too_many_requests","code":"no_capacity","message":"capacity"}}`,
			fasthttp.StatusTooManyRequests,
		},
		{
			"quota by code keeps its 429",
			`{"type":"error","error":{"code":"insufficient_quota","message":"quota"}}`,
			fasthttp.StatusTooManyRequests,
		},
		{
			// 502 would retry a request that can never succeed.
			"context length is a caller fault",
			`{"type":"response.failed","response":{"error":{"code":"context_length_exceeded","message":"input is too large"}}}`,
			fasthttp.StatusBadRequest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var response schemas.BifrostResponsesStreamResponse
			if err := schemas.Unmarshal([]byte(tc.wire), &response); err != nil {
				t.Fatalf("unmarshal stream error: %v", err)
			}
			got := responsesStreamError(&response)
			if got.StatusCode == nil {
				t.Fatalf("StatusCode is nil, want %d", tc.want)
			}
			if *got.StatusCode != tc.want {
				t.Fatalf("StatusCode = %d, want %d", *got.StatusCode, tc.want)
			}
			// The status is what makes this reach 5xx dashboards rather than 400.
			if got.EffectiveHTTPStatus() != tc.want {
				t.Fatalf("EffectiveHTTPStatus() = %d, want %d", got.EffectiveHTTPStatus(), tc.want)
			}
		})
	}
}

// The chat, text, speech, transcription and image streaming paths unmarshal the SSE
// error body straight into a BifrostError. OpenAI's body has no status_code field, so
// every one of them used to resolve to a caller 400 with nothing for ClassifyFailure
// to act on.
func TestApplyStreamErrorStatus(t *testing.T) {
	cases := []struct {
		name, wire string
		want       int
	}{
		{
			"overload gets the gateway default",
			`{"error":{"message":"Our servers are currently overloaded. Please try again later.","type":"server_error"}}`,
			fasthttp.StatusBadGateway,
		},
		{
			"rate limit keeps its 429",
			`{"error":{"message":"Rate limit reached","type":"rate_limit_error","code":"rate_limit_exceeded"}}`,
			fasthttp.StatusTooManyRequests,
		},
		{
			"context length is a caller fault",
			`{"error":{"message":"too long","code":"context_length_exceeded"}}`,
			fasthttp.StatusBadRequest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var bifrostErr schemas.BifrostError
			if err := schemas.Unmarshal([]byte(tc.wire), &bifrostErr); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if bifrostErr.StatusCode != nil {
				t.Fatalf("precondition: the wire body should carry no status, got %d", *bifrostErr.StatusCode)
			}
			applyStreamErrorStatus(&bifrostErr)
			if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != tc.want {
				t.Fatalf("StatusCode = %v, want %d", bifrostErr.StatusCode, tc.want)
			}
		})
	}
}

// An upstream status already on the error must not be overwritten.
func TestApplyStreamErrorStatus_PreservesExistingStatus(t *testing.T) {
	bifrostErr := &schemas.BifrostError{StatusCode: schemas.Ptr(429), Error: &schemas.ErrorField{Message: "x"}}
	applyStreamErrorStatus(bifrostErr)
	if *bifrostErr.StatusCode != 429 {
		t.Fatalf("StatusCode = %d, want 429 preserved", *bifrostErr.StatusCode)
	}
}

// A prompt the vendor's content filter blocked is classed caller_content_filtered, with the
// vendor's status and code kept (contract C6). The Azure body is the documented one for a
// blocked prompt; azure parses its errors with ParseOpenAIError too.
func TestParseOpenAIError_ContentFilterIsCallerContentFiltered(t *testing.T) {
	cases := []struct {
		name, body, wantCode string
		status               int
		wantType             schemas.ErrorType
	}{
		{
			name:   "azure prompt blocked",
			status: fasthttp.StatusBadRequest,
			body: `{"error":{"message":"The response was filtered due to the prompt triggering Azure OpenAI's content management policy. Please modify your prompt and retry. To learn more about our content filtering policies please read our documentation: https://go.microsoft.com/fwlink/?linkid=2198766","type":null,"param":"prompt","code":"content_filter","status":400,` +
				`"innererror":{"code":"ResponsibleAIPolicyViolation","content_filter_result":{"hate":{"filtered":false,"severity":"safe"},"jailbreak":{"filtered":true,"detected":true},"self_harm":{"filtered":false,"severity":"safe"},"sexual":{"filtered":false,"severity":"safe"},"violence":{"filtered":false,"severity":"safe"}}}}}`,
			wantCode: "content_filter",
			wantType: schemas.ErrorTypeCallerContentFiltered,
		},
		{
			name:     "openai content policy violation",
			status:   fasthttp.StatusBadRequest,
			body:     `{"error":{"message":"Your request was rejected as a result of our safety system.","type":"invalid_request_error","param":null,"code":"content_policy_violation"}}`,
			wantCode: "content_policy_violation",
			wantType: schemas.ErrorTypeCallerContentFiltered,
		},
		{
			name:     "an ordinary 400 declares nothing",
			status:   fasthttp.StatusBadRequest,
			body:     `{"error":{"message":"unsupported role: developer","type":"invalid_request_error","param":"messages.0.role","code":"invalid_value"}}`,
			wantCode: "invalid_value",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var resp fasthttp.Response
			resp.SetStatusCode(tc.status)
			resp.SetBodyString(tc.body)

			got := ParseOpenAIError(&resp)
			if got == nil || got.Error == nil {
				t.Fatal("expected a parsed error")
			}
			if got.StatusCode == nil || *got.StatusCode != tc.status {
				t.Fatalf("status = %v, want the vendor's %d", got.StatusCode, tc.status)
			}
			if got.Error.Code == nil || *got.Error.Code != tc.wantCode {
				t.Fatalf("error.code = %v, want the vendor's %q", got.Error.Code, tc.wantCode)
			}
			if got.ExtraFields.ErrorType != tc.wantType {
				t.Fatalf("extra_fields.error_type = %q, want %q", got.ExtraFields.ErrorType, tc.wantType)
			}
		})
	}
}

// BenchmarkParseOpenAIError is the error path every OpenAI-shape provider shares; the
// content-filter check adds one code comparison to it.
func BenchmarkParseOpenAIError(b *testing.B) {
	body := []byte(`{"error":{"message":"unsupported role: developer","type":"invalid_request_error","param":"messages.0.role","code":"invalid_value"}}`)
	var resp fasthttp.Response
	resp.SetStatusCode(fasthttp.StatusBadRequest)
	b.ReportAllocs()
	for b.Loop() {
		resp.SetBody(body)
		if got := ParseOpenAIError(&resp); got == nil {
			b.Fatal("nil error")
		}
	}
}

// TestParseOpenAIError_AzureResponsibleAIPolicyViolation pins contract C6 with the error body of
// Azure's pinned v1-preview schema: the vendor's status and outer code stay, inner_error.code
// reaches its fixed place, and the block is classified as the caller's content.
func TestParseOpenAIError_AzureResponsibleAIPolicyViolation(t *testing.T) {
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	resp.SetStatusCode(fasthttp.StatusBadRequest)
	resp.SetBodyString(`{"error":{"code":"invalid_prompt","message":"Your request was rejected by the safety system.","param":"prompt","type":"invalid_request_error","inner_error":{"code":"ResponsibleAIPolicyViolation","revised_prompt":"a fox","error_details":{"sexual":{"filtered":true,"severity":"high"}}}}}`)

	bifrostErr := ParseOpenAIError(resp)
	if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != fasthttp.StatusBadRequest {
		t.Fatalf("status = %v, want the vendor's 400", bifrostErr.StatusCode)
	}
	if bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != "invalid_prompt" {
		t.Fatalf("error.code = %v, want the vendor's outer code", bifrostErr.Error.Code)
	}
	if bifrostErr.Error.InnerCode == nil || *bifrostErr.Error.InnerCode != "ResponsibleAIPolicyViolation" {
		t.Fatalf("error.inner_code = %v", bifrostErr.Error.InnerCode)
	}
	if got := bifrostErr.ResolveErrorType(); got != schemas.ErrorTypeCallerContentFiltered {
		t.Fatalf("error_type = %q, want %q", got, schemas.ErrorTypeCallerContentFiltered)
	}
	if bifrostErr.ExtraFields.RawResponse == nil {
		t.Fatal("raw_response lost the vendor's error_details")
	}
}

// TestParseOpenAIError_OtherInnerCodeIsKeptButNotAFilter: inner_code is carried for any vendor
// value, while only the policy violation is the caller's content. A body without inner_error is
// unchanged.
func TestParseOpenAIError_OtherInnerCodeIsKeptButNotAFilter(t *testing.T) {
	parse := func(body string) *schemas.BifrostError {
		resp := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseResponse(resp)
		resp.SetStatusCode(fasthttp.StatusBadRequest)
		resp.SetBodyString(body)
		return ParseOpenAIError(resp)
	}
	other := parse(`{"error":{"code":"bad","message":"x","inner_error":{"code":"SomethingElse"}}}`)
	if other.Error.InnerCode == nil || *other.Error.InnerCode != "SomethingElse" {
		t.Fatalf("error.inner_code = %v", other.Error.InnerCode)
	}
	if got := other.ResolveErrorType(); got == schemas.ErrorTypeCallerContentFiltered {
		t.Fatalf("error_type = %q for a code that is not a content block", got)
	}
	plain := parse(`{"error":{"code":"bad","message":"x"}}`)
	if plain.Error.InnerCode != nil {
		t.Fatalf("error.inner_code = %v, want none", *plain.Error.InnerCode)
	}
}

func benchmarkParseOpenAIError(b *testing.B, body string) {
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	resp.SetStatusCode(fasthttp.StatusBadRequest)
	resp.SetBodyString(body)
	b.ReportAllocs()
	for b.Loop() {
		ParseOpenAIError(resp)
	}
}

// BenchmarkParseOpenAIErrorInnerCode measures the inner code step: it is read from the body the
// parser already decoded, so a body without inner_error adds only map lookups.
func BenchmarkParseOpenAIErrorInnerCode(b *testing.B) {
	b.Run("no_inner_error", func(b *testing.B) {
		benchmarkParseOpenAIError(b, `{"error":{"code":"bad","message":"x","type":"invalid_request_error"}}`)
	})
	b.Run("inner_error", func(b *testing.B) {
		benchmarkParseOpenAIError(b, `{"error":{"code":"invalid_prompt","message":"x","inner_error":{"code":"ResponsibleAIPolicyViolation"}}}`)
	})
}
