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

// A custom provider on the Replicate base is known by its own name: the core lists it under
// that name, and the video ids it creates route back to it.
func TestCustomReplicateProviderUsesItsOwnName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/models/fx-lab/fx-video/predictions", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"p1","model":"fx-lab/fx-video","status":"starting","created_at":"2026-11-03T10:00:00Z"}`))
	}))
	defer srv.Close()
	allowed := &schemas.AllowedRequests{VideoGeneration: true, VideoRetrieve: true}
	provider, err := replicate.NewReplicateProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: srv.URL},
		CustomProviderConfig: &schemas.CustomProviderConfig{CustomProviderKey: "tkp_10", BaseProviderType: schemas.Replicate,
			AllowedRequests: allowed},
	}, &testLogger{})
	require.NoError(t, err)
	require.Equal(t, schemas.ModelProvider("tkp_10"), provider.GetProviderKey())

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	defer ctx.Cancel()
	resp, bifrostErr := provider.VideoGeneration(ctx, schemas.Key{Value: *schemas.NewSecretVar("r8_test_key")},
		&schemas.BifrostVideoGenerationRequest{Provider: "tkp_10", Model: "fx-lab/fx-video", Input: &schemas.VideoGenerationInput{Prompt: "waves"}})
	require.Nil(t, bifrostErr)
	require.Equal(t, "p1:tkp_10", resp.ID)

	builtIn, err := replicate.NewReplicateProvider(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: srv.URL}}, &testLogger{})
	require.NoError(t, err)
	require.Equal(t, schemas.Replicate, builtIn.GetProviderKey())
}
