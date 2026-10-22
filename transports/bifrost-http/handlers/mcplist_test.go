package handlers

import (
	"context"
	"encoding/json"
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/mcp"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
	"github.com/capsohq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// emptyEngineMCPManager is an engine with no connected clients: the list
// endpoint then projects every client from its stored row alone.
type emptyEngineMCPManager struct {
	mcp.MCPManagerInterface
}

func (emptyEngineMCPManager) GetClients() []schemas.MCPClientState { return nil }

// TestGetMCPClients_ProjectsMaxInstructionsLength pins that the list response
// carries a client's per-client instruction cap. The edit form is filled from
// this response and submits the whole config back, so a missing field reads as
// zero and an unrelated edit would silently reset the override.
func TestGetMCPClients_ProjectsMaxInstructionsLength(t *testing.T) {
	SetLogger(&mockLogger{})
	store := newRealOAuth2Store(t)
	require.NoError(t, store.CreateMCPClientConfig(context.Background(), &schemas.MCPClientConfig{
		ID:                    "capped-client",
		Name:                  "capped",
		ConnectionType:        schemas.MCPConnectionTypeHTTP,
		ConnectionString:      schemas.NewSecretVar("https://mcp.example.com/mcp"),
		AuthType:              schemas.MCPAuthTypeNone,
		MaxInstructionsLength: 1000,
	}))
	h := &MCPHandler{
		client: &bifrost.Bifrost{MCPManager: emptyEngineMCPManager{}},
		store:  &lib.Config{ConfigStore: store, ClientConfig: &configstore.ClientConfig{}, MCPConfig: &schemas.MCPConfig{}},
	}

	// Init gives the context a server, so the store can use it as a context.Context.
	var request fasthttp.Request
	request.Header.SetMethod(fasthttp.MethodGet)
	request.SetRequestURI("/api/mcp/clients")
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&request, nil, nil)
	h.getMCPClients(ctx)
	require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode(), string(ctx.Response.Body()))

	var body struct {
		Clients []struct {
			Config struct {
				ID                    string `json:"client_id"`
				MaxInstructionsLength int    `json:"max_instructions_length"`
			} `json:"config"`
		} `json:"clients"`
	}
	require.NoError(t, json.Unmarshal(ctx.Response.Body(), &body))
	require.Len(t, body.Clients, 1)
	assert.Equal(t, 1000, body.Clients[0].Config.MaxInstructionsLength, "the list must carry the per-client instruction cap")
}
