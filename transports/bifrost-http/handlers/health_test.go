package handlers

import (
	"encoding/json"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/modelcatalog"
	"github.com/capsohq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestGetModelCatalogHealthUnavailable(t *testing.T) {
	handler := NewHealthHandler(&lib.Config{})
	ctx := &fasthttp.RequestCtx{}

	handler.getModelCatalogHealth(ctx)

	assert.Equal(t, fasthttp.StatusServiceUnavailable, ctx.Response.StatusCode())
}

func TestGetModelCatalogHealthOK(t *testing.T) {
	catalog := modelcatalog.NewTestCatalog(nil)
	provider := schemas.GLM
	modelData := &schemas.BifrostListModelsResponse{
		Data: []schemas.Model{{ID: "glm/glm-5"}},
	}
	catalog.RecordProviderModelDiscoveryResult(provider, false, modelData, nil)
	catalog.RecordProviderModelDiscoveryResult(provider, true, modelData, nil)

	handler := NewHealthHandler(&lib.Config{
		ModelCatalog: catalog,
	})
	ctx := &fasthttp.RequestCtx{}

	handler.getModelCatalogHealth(ctx)

	assert.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode())

	var response modelcatalog.ProviderModelSnapshotHealthReport
	err := json.Unmarshal(ctx.Response.Body(), &response)
	require.NoError(t, err)
	assert.Equal(t, modelcatalog.ProviderModelHealthHealthy, response.Status)
	assert.NotEmpty(t, response.Providers)
}
