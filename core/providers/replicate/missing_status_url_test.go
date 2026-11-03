package replicate_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/capsohq/bifrost/core/providers/replicate"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

// An unfinished prediction whose response carries no urls fails the request; it used to
// dereference the missing urls and crash the process.
func TestUnfinishedPredictionWithoutStatusURLFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"p1","model":"black-forest-labs/flux-1.1-pro","status":"starting"}`))
	}))
	defer srv.Close()
	provider, err := replicate.NewReplicateProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: srv.URL, DefaultRequestTimeoutInSeconds: 5}}, &testLogger{})
	require.NoError(t, err)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	defer ctx.Cancel()
	resp, bifrostErr := provider.ImageGeneration(ctx, schemas.Key{Value: *schemas.NewSecretVar("r8_test_key")},
		&schemas.BifrostImageGenerationRequest{Provider: schemas.Replicate, Model: "black-forest-labs/flux-1.1-pro",
			Input: &schemas.ImageGenerationInput{Prompt: "a lighthouse"}})
	require.Nil(t, resp)
	require.NotNil(t, bifrostErr)
	require.Contains(t, bifrostErr.Error.Error.Error(), "no status URL")
}
