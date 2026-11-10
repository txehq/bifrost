package lib

import (
	"errors"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

func TestSanitizeBifrostErrorForClientHidesInternalDetails(t *testing.T) {
	statusCode := fasthttp.StatusInternalServerError
	err := &schemas.BifrostError{
		IsBifrostError: true,
		StatusCode:     &statusCode,
		Error: &schemas.ErrorField{
			Message: "failed to create customer: pq: duplicate key value violates unique constraint users_email_key",
			Error:   errors.New("goroutine 1 [running]:\nmain.handler\n\t/app/server.go:42"),
			Param:   "users_email_key",
		},
	}

	sanitized := SanitizeBifrostErrorForClient(err)

	if sanitized == err {
		t.Fatal("expected sanitizer to return a copy")
	}
	if sanitized.Error.Message != ClientSafeInternalErrorMessage {
		t.Fatalf("expected generic message, got %q", sanitized.Error.Message)
	}
	if sanitized.Error.Error != nil {
		t.Fatalf("expected sensitive nested error to be removed, got %v", sanitized.Error.Error)
	}
	if sanitized.Error.Param != nil {
		t.Fatalf("expected param to be removed, got %v", sanitized.Error.Param)
	}
	if err.Error.Message == ClientSafeInternalErrorMessage || err.Error.Error == nil || err.Error.Param == nil {
		t.Fatal("expected original error to remain unchanged")
	}
}

func TestSanitizeBifrostErrorForClientPreservesClientValidationMessage(t *testing.T) {
	statusCode := fasthttp.StatusBadRequest
	err := &schemas.BifrostError{
		StatusCode: &statusCode,
		Error: &schemas.ErrorField{
			Message: "model is required",
			Error:   errors.New("missing model"),
			Param:   "model",
		},
	}

	sanitized := SanitizeBifrostErrorForClient(err)

	if sanitized.Error.Message != "model is required" {
		t.Fatalf("expected validation message to be preserved, got %q", sanitized.Error.Message)
	}
	if sanitized.Error.Param != "model" {
		t.Fatalf("expected param to be preserved, got %v", sanitized.Error.Param)
	}
	if sanitized.Error.Error == nil {
		t.Fatal("expected non-sensitive nested error to be preserved")
	}
}

func TestSanitizeBifrostErrorForClientPreservesNonSensitiveServerMessage(t *testing.T) {
	statusCode := fasthttp.StatusInternalServerError
	err := &schemas.BifrostError{
		StatusCode: &statusCode,
		Error: &schemas.ErrorField{
			Message: "failed to reload config",
		},
	}

	sanitized := SanitizeBifrostErrorForClient(err)

	if sanitized.Error.Message != "failed to reload config" {
		t.Fatalf("expected non-sensitive server message to be preserved, got %q", sanitized.Error.Message)
	}
}

// BF8: the copy a client receives always carries extra_fields.error_type, and the
// caller's error is left as it was.
func TestSanitizeBifrostErrorForClientResolvesErrorType(t *testing.T) {
	status := fasthttp.StatusForbidden
	err := &schemas.BifrostError{StatusCode: &status, Error: &schemas.ErrorField{Message: "forbidden"}}
	sanitized := SanitizeBifrostErrorForClient(err)
	if got := sanitized.ExtraFields.ErrorType; got != schemas.ErrorTypeProviderAuthFailed {
		t.Fatalf("error_type = %q, want %q", got, schemas.ErrorTypeProviderAuthFailed)
	}
	if err.ExtraFields.ErrorType != "" {
		t.Fatalf("the caller's error was changed: %q", err.ExtraFields.ErrorType)
	}
	declared := &schemas.BifrostError{StatusCode: &status, ExtraFields: schemas.BifrostErrorExtraFields{ErrorType: schemas.ErrorTypePolicyModelBlocked}}
	if got := SanitizeBifrostErrorForClient(declared).ExtraFields.ErrorType; got != schemas.ErrorTypePolicyModelBlocked {
		t.Fatalf("a declaration was not kept: %q", got)
	}
}

// SanitizeBifrostErrorForClient runs once per error a client receives; it now also
// resolves the error type on its copy.
func BenchmarkSanitizeBifrostErrorForClient(b *testing.B) {
	status := fasthttp.StatusTooManyRequests
	err := &schemas.BifrostError{StatusCode: &status, Error: &schemas.ErrorField{Message: "Rate limit reached for requests"}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = SanitizeBifrostErrorForClient(err)
	}
}
