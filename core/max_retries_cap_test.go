package bifrost

import (
	"context"
	"testing"
	"time"

	schemas "github.com/capsohq/bifrost/core/schemas"
)

// A request that sets x-bf-max-retries is retried at most that many times, and never more
// than its provider allows: a media generation can share a provider with embeddings.
func TestExecuteRequestWithRetries_PerRequestCap(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure int
		capped    *int
		want      int
	}{
		{"the provider's count", 3, nil, 4},
		{"capped at zero", 3, schemas.Ptr(0), 1},
		{"capped at one", 3, schemas.Ptr(1), 2},
		{"a cap above the provider's count does not raise it", 1, schemas.Ptr(5), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := createTestConfig(tc.configure, time.Millisecond, time.Millisecond)
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			ctx.SetValue(schemas.BifrostContextKeyTracer, &schemas.NoOpTracer{})
			if tc.capped != nil {
				ctx.SetValue(schemas.BifrostContextKeyMaxRetries, *tc.capped)
			}
			calls := 0
			handler := func(_ schemas.Key) (string, *schemas.BifrostError) {
				calls++
				return "", &schemas.BifrostError{StatusCode: schemas.Ptr(500), Error: &schemas.ErrorField{Message: "upstream unavailable"}}
			}
			_, err := executeRequestWithRetries(ctx, config, handler, nil, schemas.ImageGenerationRequest, schemas.OpenAI, "gpt-image-2",
				&schemas.BifrostRequest{}, NewDefaultLogger(schemas.LogLevelError))
			if err == nil || calls != tc.want {
				t.Fatalf("%d attempts, err %v; want %d", calls, err, tc.want)
			}
		})
	}
}
