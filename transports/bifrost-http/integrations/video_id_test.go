package integrations

import (
	"net/url"
	"testing"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// The OpenAI-compatible /openai/v1/videos/{video_id} routes read the raw path segment
// (the router matches on PathOriginal). The ID a provider receives must decode to the
// upstream ID exactly once more, as on the native /v1/videos routes.
func TestExtractVideoIDFromPathRoundTripsUpstreamIDs(t *testing.T) {
	upstreamIDs := []string{
		"vid_123",
		"projects/p/locations/us-central1/publishers/google/models/veo-3.1/operations/7281",
		"task:with:colons",
		"literal%41percent",
	}
	for _, id := range upstreamIDs {
		scoped := providerUtils.AddVideoIDProviderSuffix(id, schemas.Replicate)
		ctx := &fasthttp.RequestCtx{}
		ctx.SetUserValue("video_id", url.PathEscape(scoped))

		var req schemas.BifrostVideoDownloadRequest
		require.NoError(t, extractVideoIDFromPath(nil)(ctx, nil, &req), id)
		require.Equal(t, schemas.Replicate, req.Provider, id)
		require.Equal(t, id, providerUtils.StripVideoIDProviderSuffix(req.ID, schemas.Replicate), id)
	}
}
