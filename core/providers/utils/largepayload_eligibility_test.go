package utils

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// The shared entry points enforce the large-payload eligibility rule (TXE-7082): a body is
// streamed raw only for an attempt whose stamp allows it, and with no stamp it never is.

type stagedRequest struct{}

func (stagedRequest) GetRawRequestBody() []byte { return nil }

type convertedBody struct{}

func (convertedBody) GetExtraParams() map[string]interface{} { return nil }

func stagedLargePayloadCtx(body string) *schemas.BifrostContext {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyLargePayloadMode, true)
	ctx.SetValue(schemas.BifrostContextKeyLargePayloadReader, io.Reader(strings.NewReader(body)))
	ctx.SetValue(schemas.BifrostContextKeyLargePayloadContentLength, len(body))
	ctx.SetValue(schemas.BifrostContextKeyLargePayloadContentType, "application/json")
	return ctx
}

func TestCheckContextAndGetRequestBodyRefusesAStagedBodyItMayNotForward(t *testing.T) {
	for _, tt := range []struct {
		name  string
		stamp *schemas.LargePayloadEligibility
	}{
		{"no decision on the context", nil},
		{"the attempt converts", &schemas.LargePayloadEligibility{Reason: "mistral converts"}},
		{"only the response is raw", &schemas.LargePayloadEligibility{RawResponse: true, Reason: "unified request"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := stagedLargePayloadCtx(`{"model":"m","input":"x"}`)
			if tt.stamp != nil {
				ctx.SetValue(schemas.BifrostContextKeyLargePayloadEligibility, *tt.stamp)
			}
			converterCalls := 0
			body, bifrostErr := CheckContextAndGetRequestBody(ctx, stagedRequest{}, func() (RequestBodyWithExtraParams, error) {
				converterCalls++
				return convertedBody{}, nil
			})
			if bifrostErr == nil {
				t.Fatalf("got body %q and no error, want the 413 refusal", body)
			}
			if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusRequestEntityTooLarge {
				t.Errorf("status = %v, want 413", bifrostErr.StatusCode)
			}
			if bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeCallerInvalidRequest {
				t.Errorf("error_type = %q", bifrostErr.ExtraFields.ErrorType)
			}
			if converterCalls != 0 {
				t.Errorf("the converter ran %d times on a request with no parsed input", converterCalls)
			}
			if IsLargePayloadPassthroughEnabled(ctx) {
				t.Error("IsLargePayloadPassthroughEnabled is true for an attempt that may not forward the body")
			}
			req := fasthttp.AcquireRequest()
			defer fasthttp.ReleaseRequest(req)
			if ApplyLargePayloadRequestBody(ctx, req) {
				t.Error("the staged body was applied to the upstream request")
			}
		})
	}
}

func TestCheckContextAndGetRequestBodyStreamsAStagedBodyForAnEligibleAttempt(t *testing.T) {
	ctx := stagedLargePayloadCtx(`{"model":"openai/gpt-5","input":"x"}`)
	ctx.SetValue(schemas.BifrostContextKeyLargePayloadEligibility, schemas.LargePayloadEligibility{RawRequest: true, RawResponse: true})
	body, bifrostErr := CheckContextAndGetRequestBody(ctx, stagedRequest{}, func() (RequestBodyWithExtraParams, error) {
		t.Error("the converter ran for a body that is streamed as written")
		return convertedBody{}, nil
	})
	if bifrostErr != nil || body != nil {
		t.Fatalf("got body %q, err %v; want nil, nil (the body is the staged stream)", body, bifrostErr)
	}
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	if !ApplyLargePayloadRequestBody(ctx, req) {
		t.Fatal("the staged body was not applied")
	}
}

// A request that is not staged is untouched by the rule: the converter runs as always.
func TestCheckContextAndGetRequestBodyConvertsWhenNothingIsStaged(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	converterCalls := 0
	if _, bifrostErr := CheckContextAndGetRequestBody(ctx, stagedRequest{}, func() (RequestBodyWithExtraParams, error) {
		converterCalls++
		return convertedBody{}, nil
	}); bifrostErr != nil {
		t.Fatalf("unexpected error: %v", bifrostErr.Error.Message)
	}
	if converterCalls != 1 {
		t.Errorf("converter calls = %d, want 1", converterCalls)
	}
}

// A response the attempt must convert is returned whole above the threshold, with a known
// length and without one, and nothing is left on the context for the transport to stream.
func TestFinalizeResponseWithLargeDetectionBuffersAResponseThatMustBeConverted(t *testing.T) {
	payload := `{"data":"` + strings.Repeat("x", 200*1024) + `","usage":{"total_tokens":7}}`
	for _, chunked := range []bool{false, true} {
		t.Run(fmt.Sprintf("chunked=%v", chunked), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if chunked {
					w.WriteHeader(http.StatusOK)
					half := len(payload) / 2
					_, _ = io.WriteString(w, payload[:half])
					w.(http.Flusher).Flush()
					_, _ = io.WriteString(w, payload[half:])
					return
				}
				w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
				_, _ = io.WriteString(w, payload)
			}))
			defer srv.Close()

			ctx, resp, release := largeResponseRequest(t, srv, 5*time.Second)
			defer release()
			defer fasthttp.ReleaseResponse(resp)
			ctx.SetValue(schemas.BifrostContextKeyLargePayloadEligibility, schemas.LargePayloadEligibility{Reason: "cohere converts"})

			body, isLarge, bifrostErr := FinalizeResponseWithLargeDetection(ctx, resp, nil)
			if bifrostErr != nil {
				t.Fatalf("FinalizeResponseWithLargeDetection: %s", bifrostErr.Error.Message)
			}
			if isLarge {
				t.Fatal("a response that must be converted took the raw streaming path")
			}
			if string(body) != payload {
				t.Fatalf("got %d bytes, want the whole %d byte body", len(body), len(payload))
			}
			if ctx.Value(schemas.BifrostContextKeyLargeResponseMode) != nil || ctx.Value(schemas.BifrostContextKeyLargeResponseReader) != nil {
				t.Error("large-response state was left on the context")
			}
		})
	}
}

func TestSetupStreamingPassthroughNeedsAnEligibleAttempt(t *testing.T) {
	for _, eligible := range []bool{false, true} {
		t.Run(fmt.Sprintf("eligible=%v", eligible), func(t *testing.T) {
			ctx := stagedLargePayloadCtx(`{}`)
			if eligible {
				ctx.SetValue(schemas.BifrostContextKeyLargePayloadEligibility, schemas.LargePayloadEligibility{RawRequest: true, RawResponse: true})
			}
			resp := fasthttp.AcquireResponse()
			resp.SetBodyStream(strings.NewReader("data: {}\n\n"), -1)
			got := SetupStreamingPassthrough(ctx, resp)
			if got != eligible {
				t.Fatalf("SetupStreamingPassthrough = %v, want %v", got, eligible)
			}
			if reader, ok := ctx.Value(schemas.BifrostContextKeyLargeResponseReader).(io.Closer); ok {
				_ = reader.Close()
			} else {
				fasthttp.ReleaseResponse(resp)
			}
		})
	}
}
