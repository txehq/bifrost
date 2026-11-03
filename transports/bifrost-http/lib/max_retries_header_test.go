package lib

import (
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// x-bf-max-retries sets the request's retry cap; a value that is not a non-negative
// integer sets nothing.
func TestConvertToBifrostContext_MaxRetriesHeader(t *testing.T) {
	for value, want := range map[string]any{"0": 0, "2": 2, " 1 ": 1, "-1": nil, "many": nil} {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.Set("x-bf-max-retries", value)
		converted, cancel := ConvertToBifrostContext(ctx, testHandlerStore{})
		got := converted.Value(schemas.BifrostContextKeyMaxRetries)
		cancel()
		if want == nil && got != nil || want != nil && got != want {
			t.Errorf("x-bf-max-retries %q: %v, want %v", value, got, want)
		}
	}
}
