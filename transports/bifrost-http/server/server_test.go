package server

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/capsohq/bifrost/framework/vectorstore"
	"github.com/capsohq/bifrost/plugins/semanticcache"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/agent"
	"github.com/capsohq/bifrost/core/network"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/capsohq/bifrost/framework/logstore"
	"github.com/capsohq/bifrost/framework/modelcatalog"
	"github.com/fasthttp/router"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/capsohq/bifrost/plugins/governance"
	"github.com/capsohq/bifrost/plugins/prompts"
	"github.com/capsohq/bifrost/plugins/telemetry"
	"github.com/capsohq/bifrost/transports/v2/bifrost-http/handlers"
	"github.com/capsohq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
)

func TestAgentRegistrationCallbacksRejectUninitializedGateway(t *testing.T) {
	t.Parallel()

	server := &BifrostHTTPServer{}
	ctx := context.Background()

	_, err := server.CreateAgentRegistration(ctx, agent.CreateRequest{})
	if !errors.Is(err, errAgentGatewayNotInitialized) {
		t.Fatalf("CreateAgentRegistration error = %v, want %v", err, errAgentGatewayNotInitialized)
	}

	_, err = server.UpdateAgentRegistration(ctx, "fixture", agent.UpdateRequest{})
	if !errors.Is(err, errAgentGatewayNotInitialized) {
		t.Fatalf("UpdateAgentRegistration error = %v, want %v", err, errAgentGatewayNotInitialized)
	}

	err = server.DeleteAgentRegistration(ctx, "fixture")
	if !errors.Is(err, errAgentGatewayNotInitialized) {
		t.Fatalf("DeleteAgentRegistration error = %v, want %v", err, errAgentGatewayNotInitialized)
	}
}

// reloadVirtualKeyConfigStore provides the persistence calls used by ReloadVirtualKey.
type reloadVirtualKeyConfigStore struct {
	configstore.ConfigStore
	vk *configstoreTables.TableVirtualKey
}

// RetryOnNotFound executes the supplied lookup once for deterministic tests.
func (s *reloadVirtualKeyConfigStore) RetryOnNotFound(ctx context.Context, fn func(context.Context) (any, error), _ int, _ time.Duration) (any, error) {
	return fn(ctx)
}

// GetVirtualKey returns the configured persisted virtual key.
func (s *reloadVirtualKeyConfigStore) GetVirtualKey(context.Context, string) (*configstoreTables.TableVirtualKey, error) {
	return s.vk, nil
}

// GetModelConfigsByScopeAndScopeIDs returns no scoped model configs.
func (s *reloadVirtualKeyConfigStore) GetModelConfigsByScopeAndScopeIDs(context.Context, string, []string, ...*gorm.DB) ([]configstoreTables.TableModelConfig, error) {
	return nil, nil
}

// reloadVirtualKeyGovernanceStore counts snapshot calls while delegating all
// mutation and direct lookup behavior to a real in-memory store.
type reloadVirtualKeyGovernanceStore struct {
	governance.GovernanceStore
	governanceDataCalls int
}

// GetGovernanceData records any accidental full-snapshot lookup.
func (s *reloadVirtualKeyGovernanceStore) GetGovernanceData(context.Context) *governance.GovernanceData {
	s.governanceDataCalls++
	return nil
}

// reloadVirtualKeyPlugin exposes the test governance store.
type reloadVirtualKeyPlugin struct {
	governance.BaseGovernancePlugin
	store governance.GovernanceStore
}

// GetName returns the standard governance plugin name.
func (p *reloadVirtualKeyPlugin) GetName() string { return governance.PluginName }

// GetGovernanceStore returns the test governance store.
func (p *reloadVirtualKeyPlugin) GetGovernanceStore() governance.GovernanceStore { return p.store }

// reloadVirtualKeyToolManager provides an empty MCP tool set.
type reloadVirtualKeyToolManager struct{}

// GetMCPServerInstructions returns no instructions.
func (reloadVirtualKeyToolManager) GetMCPServerInstructions(context.Context) string {
	return ""
}

// GetAvailableMCPTools returns no tools.
func (reloadVirtualKeyToolManager) GetAvailableMCPTools(context.Context) []schemas.ChatTool {
	return nil
}

// ExecuteChatMCPTool is unused by these tests.
func (reloadVirtualKeyToolManager) ExecuteChatMCPTool(context.Context, *schemas.ChatAssistantMessageToolCall) (*schemas.ChatMessage, *schemas.BifrostError) {
	return nil, nil
}

// ExecuteResponsesMCPTool is unused by these tests.
func (reloadVirtualKeyToolManager) ExecuteResponsesMCPTool(context.Context, *schemas.ResponsesToolMessage) (*schemas.ResponsesMessage, *schemas.BifrostError) {
	return nil, nil
}

// TestReloadVirtualKeyAvoidsGovernanceSnapshot pins that reloading one key reads and replaces it
// directly rather than through a full governance snapshot, which is what keeps a key edit cheap on
// a deployment holding many keys.
func TestReloadVirtualKeyAvoidsGovernanceSnapshot(t *testing.T) {
	handlers.SetLogger(noopTestLogger{})
	ctx := context.Background()
	baseStore, err := governance.NewLocalGovernanceStore(ctx, governance.NewMockLogger(), nil, &configstore.GovernanceConfig{}, nil, nil)
	if err != nil {
		t.Fatalf("create governance store: %v", err)
	}
	baseStore.CreateVirtualKeyInMemory(ctx, &configstoreTables.TableVirtualKey{ID: "vk-id", Name: "old", Value: *schemas.NewSecretVar("sk-bf-old")})
	store := &reloadVirtualKeyGovernanceStore{GovernanceStore: baseStore}
	plugin := &reloadVirtualKeyPlugin{store: store}
	persistedVK := &configstoreTables.TableVirtualKey{ID: "vk-id", Name: "new", Value: *schemas.NewSecretVar("sk-bf-new")}
	config := &lib.Config{
		ConfigStore:  &reloadVirtualKeyConfigStore{vk: persistedVK},
		ClientConfig: &configstore.ClientConfig{},
	}
	plugins := []schemas.BasePlugin{plugin}
	config.BasePlugins.Store(&plugins)
	mcpHandler, err := handlers.NewMCPServerHandler(ctx, config, reloadVirtualKeyToolManager{}, nil, nil, store)
	if err != nil {
		t.Fatalf("create MCP handler: %v", err)
	}
	server := &BifrostHTTPServer{Ctx: schemas.NewBifrostContext(ctx, schemas.NoDeadline), Config: config, MCPServerHandler: mcpHandler}

	if _, err := server.ReloadVirtualKey(ctx, persistedVK.ID); err != nil {
		t.Fatalf("ReloadVirtualKey returned unexpected error: %v", err)
	}
	if store.governanceDataCalls != 0 {
		t.Fatalf("GetGovernanceData called %d times, want 0", store.governanceDataCalls)
	}
	reloaded, ok := baseStore.GetVirtualKeyByID(ctx, "vk-id")
	if !ok || reloaded.Value.GetValue() != "sk-bf-new" {
		t.Fatalf("store does not answer with the reloaded key: %+v", reloaded)
	}
}

// batchReloadConfigStore serves the batched reads ReloadVirtualKeys makes and
// records the id lists it was asked for.
type batchReloadConfigStore struct {
	configstore.ConfigStore
	vks      map[string]*configstoreTables.TableVirtualKey
	mcs      []configstoreTables.TableModelConfig
	vkLoads  [][]string
	mcsLoads [][]string
}

// GetVirtualKeysByIDs returns the persisted keys among ids.
func (s *batchReloadConfigStore) GetVirtualKeysByIDs(_ context.Context, ids []string) ([]configstoreTables.TableVirtualKey, error) {
	s.vkLoads = append(s.vkLoads, append([]string(nil), ids...))
	var out []configstoreTables.TableVirtualKey
	for _, id := range ids {
		if vk, ok := s.vks[id]; ok {
			out = append(out, *vk)
		}
	}
	return out, nil
}

// GetModelConfigsByScopeAndScopeIDs returns the configured model configs scoped to ids.
func (s *batchReloadConfigStore) GetModelConfigsByScopeAndScopeIDs(_ context.Context, _ string, ids []string, _ ...*gorm.DB) ([]configstoreTables.TableModelConfig, error) {
	s.mcsLoads = append(s.mcsLoads, append([]string(nil), ids...))
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []configstoreTables.TableModelConfig
	for _, mc := range s.mcs {
		if mc.ScopeID != nil && want[*mc.ScopeID] {
			out = append(out, mc)
		}
	}
	return out, nil
}

// TestReloadVirtualKeysMatchesPerKeyReload pins that a batched reload leaves
// every key as ReloadVirtualKey would: the persisted key replaces the cached
// one, its VK-scoped model configs are installed and stale ones evicted. It
// reads each table once for all ids (deduplicated), skips an id with no row,
// and never takes a full governance snapshot.
func TestReloadVirtualKeysMatchesPerKeyReload(t *testing.T) {
	handlers.SetLogger(noopTestLogger{})
	previousLogger := logger
	logger = noopTestLogger{}
	t.Cleanup(func() { logger = previousLogger })
	ctx := context.Background()
	baseStore, err := governance.NewLocalGovernanceStore(ctx, governance.NewMockLogger(), nil, &configstore.GovernanceConfig{}, nil, nil)
	if err != nil {
		t.Fatalf("create governance store: %v", err)
	}
	vkScope := configstoreTables.ModelConfigScopeVirtualKey
	for _, id := range []string{"vk-a", "vk-b"} {
		baseStore.CreateVirtualKeyInMemory(ctx, &configstoreTables.TableVirtualKey{ID: id, Name: "old", Value: *schemas.NewSecretVar("sk-bf-old-" + id)})
	}
	scopeA, scopeB := "vk-a", "vk-b"
	baseStore.UpdateModelConfigInMemory(ctx, &configstoreTables.TableModelConfig{ID: "mc-stale", ModelName: "gpt-4o", Scope: vkScope, ScopeID: &scopeA})
	store := &reloadVirtualKeyGovernanceStore{GovernanceStore: baseStore}
	cs := &batchReloadConfigStore{
		vks: map[string]*configstoreTables.TableVirtualKey{
			"vk-a": {ID: "vk-a", Name: "new-a", Value: *schemas.NewSecretVar("sk-bf-new-a")},
			"vk-b": {ID: "vk-b", Name: "new-b", Value: *schemas.NewSecretVar("sk-bf-new-b")},
		},
		mcs: []configstoreTables.TableModelConfig{{ID: "mc-new", ModelName: "gpt-4o", Scope: vkScope, ScopeID: &scopeB}},
	}
	config := &lib.Config{ConfigStore: cs, ClientConfig: &configstore.ClientConfig{}}
	plugins := []schemas.BasePlugin{&reloadVirtualKeyPlugin{store: store}}
	config.BasePlugins.Store(&plugins)
	server := &BifrostHTTPServer{Ctx: schemas.NewBifrostContext(ctx, schemas.NoDeadline), Config: config}

	if err := server.ReloadVirtualKeys(ctx, []string{"vk-a", "vk-b", "vk-missing", "vk-a"}); err != nil {
		t.Fatalf("ReloadVirtualKeys returned unexpected error: %v", err)
	}
	for id, want := range map[string]string{"vk-a": "new-a", "vk-b": "new-b"} {
		vk, ok := baseStore.GetVirtualKeyByID(ctx, id)
		if !ok || vk.Name != want {
			t.Fatalf("%s not reloaded: %+v", id, vk)
		}
	}
	if got := baseStore.ScopedModelConfigIDs(vkScope, "vk-a"); len(got) != 0 {
		t.Fatalf("stale model configs for vk-a not evicted: %v", got)
	}
	if got := baseStore.ScopedModelConfigIDs(vkScope, "vk-b"); len(got) != 1 || got[0] != "mc-new" {
		t.Fatalf("model configs for vk-b = %v, want [mc-new]", got)
	}
	if len(cs.vkLoads) != 1 || len(cs.vkLoads[0]) != 3 || len(cs.mcsLoads) != 1 || len(cs.mcsLoads[0]) != 3 {
		t.Fatalf("want one deduplicated read per table, got keys %v, model configs %v", cs.vkLoads, cs.mcsLoads)
	}
	if store.governanceDataCalls != 0 {
		t.Fatalf("GetGovernanceData called %d times, want 0", store.governanceDataCalls)
	}
}

// agentGatewayRouteStore satisfies only the persistence calls the Agent Gateway
// startup path makes, so route registration can be exercised without a database.
type agentGatewayRouteStore struct {
	configstore.ConfigStore
}

func (*agentGatewayRouteStore) CreateAgentRegistration(context.Context, *schemas.AgentRegistration) error {
	return nil
}
func (*agentGatewayRouteStore) UpdateAgentRegistration(context.Context, *schemas.AgentRegistration) error {
	return nil
}
func (*agentGatewayRouteStore) ListAgentRegistrations(context.Context) ([]schemas.AgentRegistration, error) {
	return nil, nil
}
func (*agentGatewayRouteStore) GetAgentRegistration(context.Context, string) (*schemas.AgentRegistration, error) {
	return nil, agent.ErrNotFound
}
func (*agentGatewayRouteStore) DeleteAgentRegistration(context.Context, string) error { return nil }
func (*agentGatewayRouteStore) ListDueAgentPushDeliveries(context.Context, time.Time, int) ([]schemas.AgentPushDelivery, error) {
	return nil, nil
}
func (*agentGatewayRouteStore) PruneAgentPushDeliveries(context.Context, time.Time) error { return nil }
func (*agentGatewayRouteStore) GetVirtualKey(context.Context, string) (*configstoreTables.TableVirtualKey, error) {
	return nil, configstore.ErrNotFound
}
func (*agentGatewayRouteStore) GetVirtualKeyByValue(context.Context, string) (*configstoreTables.TableVirtualKey, error) {
	return nil, configstore.ErrNotFound
}

type agentGatewayRouteLogger struct{}

func (agentGatewayRouteLogger) Debug(string, ...any)                   {}
func (agentGatewayRouteLogger) Info(string, ...any)                    {}
func (agentGatewayRouteLogger) Warn(string, ...any)                    {}
func (agentGatewayRouteLogger) Error(string, ...any)                   {}
func (agentGatewayRouteLogger) Fatal(string, ...any)                   {}
func (agentGatewayRouteLogger) SetLevel(schemas.LogLevel)              {}
func (agentGatewayRouteLogger) SetOutputType(schemas.LoggerOutputType) {}
func (agentGatewayRouteLogger) LogHTTPRequest(schemas.LogLevel, string) schemas.LogEventBuilder {
	return schemas.NoopLogEvent
}

func TestAgentGatewayRoutesRegisteredAfterStartupInitialization(t *testing.T) {
	store := &agentGatewayRouteStore{}
	// Protocol endpoints answer only with an explicit public URL (upstream #8526); without
	// one every protocol route is a 503, so the 404 below would not prove the route reached
	// the handler.
	s := &BifrostHTTPServer{
		Config: &lib.Config{
			ConfigStore:  store,
			ClientConfig: &configstore.ClientConfig{A2AExternalClientURL: schemas.NewSecretVar("https://agents.example.com")},
		},
		Router: router.New(),
	}

	requireNoError := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	SetLogger(agentGatewayRouteLogger{})
	t.Cleanup(func() { SetLogger(nil) })
	requireNoError(s.InitializeAgentGateway(context.Background(), nil, nil))
	firstHandler := s.AgentGatewayHandler
	if firstHandler == nil {
		t.Fatal("expected Agent Gateway handler to be initialized")
	}
	defer s.CloseAgentGateway()
	requireNoError(s.InitializeAgentGateway(context.Background(), nil, nil))
	if s.AgentGatewayHandler != firstHandler {
		t.Fatal("expected Agent Gateway handler to be constructed only once")
	}

	s.AgentGatewayHandler.RegisterManagementRoutes(s.Router)
	s.AgentGatewayHandler.RegisterProtocolRoutes(s.Router)
	s.Router.NotFound = func(ctx *fasthttp.RequestCtx) { ctx.SetStatusCode(fasthttp.StatusTeapot) }

	for _, route := range []struct {
		method     string
		path       string
		wantStatus int
	}{
		{fasthttp.MethodGet, "/api/agents", fasthttp.StatusOK},
		{fasthttp.MethodGet, "/agents/a2a/missing/.well-known/agent-card.json", fasthttp.StatusNotFound},
	} {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.SetMethod(route.method)
		ctx.Request.SetRequestURI(route.path)
		s.Router.Handler(ctx)
		if ctx.Response.StatusCode() != route.wantStatus {
			t.Fatalf("route %s returned %d, want %d", route.path, ctx.Response.StatusCode(), route.wantStatus)
		}
	}
}

// TestConfig is a sample config struct for testing
type TestConfig struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Count   int    `json:"count"`
}

type updateStatusOnlyConfigStore struct {
	configstore.ConfigStore
	calls []schemas.KeyStatus
}

type noopTestLogger struct{}

func (noopTestLogger) Debug(string, ...any)                   {}
func (noopTestLogger) Info(string, ...any)                    {}
func (noopTestLogger) Warn(string, ...any)                    {}
func (noopTestLogger) Error(string, ...any)                   {}
func (noopTestLogger) Fatal(string, ...any)                   {}
func (noopTestLogger) SetLevel(schemas.LogLevel)              {}
func (noopTestLogger) SetOutputType(schemas.LoggerOutputType) {}
func (noopTestLogger) LogHTTPRequest(schemas.LogLevel, string) schemas.LogEventBuilder {
	return schemas.NoopLogEvent
}

func (s *updateStatusOnlyConfigStore) UpdateStatus(ctx context.Context, provider schemas.ModelProvider, keyID string, status, errorMsg string) error {
	s.calls = append(s.calls, schemas.KeyStatus{
		Provider: provider,
		KeyID:    keyID,
		Status:   schemas.KeyStatusType(status),
		Error:    &schemas.BifrostError{Error: &schemas.ErrorField{Message: errorMsg}},
	})
	return nil
}

func TestUpdateKeyStatus_KeylessProviderUpdatesProviderStatusInMemory(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	store := &updateStatusOnlyConfigStore{}
	server := &BifrostHTTPServer{
		Config: &lib.Config{
			ConfigStore: store,
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				"mock-openai": {
					CustomProviderConfig: &schemas.CustomProviderConfig{IsKeyLess: true},
					Status:               "unknown",
				},
			},
		},
	}

	server.updateKeyStatus(context.Background(), []schemas.KeyStatus{{
		Provider: "mock-openai",
		KeyID:    "",
		Status:   schemas.KeyStatusListModelsFailed,
		Error:    &schemas.BifrostError{Error: &schemas.ErrorField{Message: "preview missing model"}},
	}})

	provider := server.Config.Providers["mock-openai"]
	if provider.Status != string(schemas.KeyStatusListModelsFailed) {
		t.Fatalf("expected provider status %q, got %q", schemas.KeyStatusListModelsFailed, provider.Status)
	}
	if provider.Description != "preview missing model" {
		t.Fatalf("expected provider description to be updated, got %q", provider.Description)
	}
	if len(store.calls) != 1 {
		t.Fatalf("expected one status update call, got %d", len(store.calls))
	}
	if store.calls[0].Provider != "mock-openai" || store.calls[0].KeyID != "" {
		t.Fatalf("expected provider-level status update, got provider=%q keyID=%q", store.calls[0].Provider, store.calls[0].KeyID)
	}
}

func TestUpdateKeyStatus_EmptyKeyIDDoesNotOverwriteKeyedProviderStatus(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	store := &updateStatusOnlyConfigStore{}
	server := &BifrostHTTPServer{
		Config: &lib.Config{
			ConfigStore: store,
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				"openai": {
					Keys:   []schemas.Key{{ID: "key-1"}},
					Status: "healthy",
				},
			},
		},
	}

	server.updateKeyStatus(context.Background(), []schemas.KeyStatus{{
		Provider: "openai",
		KeyID:    "",
		Status:   schemas.KeyStatusListModelsFailed,
		Error:    &schemas.BifrostError{Error: &schemas.ErrorField{Message: "malformed status"}},
	}})

	provider := server.Config.Providers["openai"]
	if provider.Status != "healthy" {
		t.Fatalf("expected keyed provider status to remain unchanged, got %q", provider.Status)
	}
	if provider.Description != "" {
		t.Fatalf("expected keyed provider description to remain unchanged, got %q", provider.Description)
	}
	if len(store.calls) != 1 {
		t.Fatalf("expected one status update call, got %d", len(store.calls))
	}
	if store.calls[0].Provider != "openai" || store.calls[0].KeyID != "" {
		t.Fatalf("expected DB status update to retain empty key ID, got provider=%q keyID=%q", store.calls[0].Provider, store.calls[0].KeyID)
	}
}

// TestUpdateKeyStatus_SkipsWriteWhenUnchanged pins the write-gating that makes
// the background refresher affordable: ConfigStore.UpdateStatus is an
// unconditional SQL UPDATE, so a status that has not moved must not reach it.
// Without this gate a 30-key deployment refreshing on an interval writes 30
// no-op UPDATEs per pass, per node, forever.
func TestUpdateKeyStatus_SkipsWriteWhenUnchanged(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	store := &updateStatusOnlyConfigStore{}
	server := &BifrostHTTPServer{
		Config: &lib.Config{
			ConfigStore: store,
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				"openai": {
					Keys: []schemas.Key{{
						ID:          "key-1",
						Status:      schemas.KeyStatusListModelsFailed,
						Description: "upstream 503",
					}},
				},
			},
		},
	}

	unchanged := []schemas.KeyStatus{{
		Provider: "openai",
		KeyID:    "key-1",
		Status:   schemas.KeyStatusListModelsFailed,
		Error:    &schemas.BifrostError{Error: &schemas.ErrorField{Message: "upstream 503"}},
	}}

	server.updateKeyStatus(context.Background(), unchanged)
	server.updateKeyStatus(context.Background(), unchanged)

	if len(store.calls) != 0 {
		t.Fatalf("expected no status update calls for an unchanged status, got %d", len(store.calls))
	}

	// A genuine change must still get through, otherwise the gate would hide
	// a key recovering or breaking.
	server.updateKeyStatus(context.Background(), []schemas.KeyStatus{{
		Provider: "openai",
		KeyID:    "key-1",
		Status:   schemas.KeyStatusSuccess,
	}})

	if len(store.calls) != 1 {
		t.Fatalf("expected one status update call after the status changed, got %d", len(store.calls))
	}
	if got := server.Config.Providers["openai"].Keys[0].Status; got != schemas.KeyStatusSuccess {
		t.Fatalf("expected in-memory status to become success, got %q", got)
	}
	if got := server.Config.Providers["openai"].Keys[0].Description; got != "" {
		t.Fatalf("expected in-memory description to be cleared, got %q", got)
	}
}

// TestUpdateKeyStatus_WritesWhenKeyMissingFromMemory guards the fallback: with
// nothing to compare against, the gate must let the write through rather than
// silently swallowing it.
func TestUpdateKeyStatus_WritesWhenKeyMissingFromMemory(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	store := &updateStatusOnlyConfigStore{}
	server := &BifrostHTTPServer{
		Config: &lib.Config{
			ConfigStore: store,
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				"openai": {Keys: []schemas.Key{{ID: "key-1"}}},
			},
		},
	}

	server.updateKeyStatus(context.Background(), []schemas.KeyStatus{{
		Provider: "openai",
		KeyID:    "key-unknown",
		Status:   schemas.KeyStatusSuccess,
	}})

	if len(store.calls) != 1 {
		t.Fatalf("expected the write to proceed for an unknown key, got %d calls", len(store.calls))
	}
}

// TestRefreshAllLiveModels_NilClientIsNoop covers the boot-order guard: the
// refresher can fire before or after the client exists, and dereferencing a
// nil client would panic the background goroutine.
func TestRefreshAllLiveModels_NilClientIsNoop(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	server := &BifrostHTTPServer{
		Config: &lib.Config{
			ModelCatalog: modelcatalog.NewTestCatalog(nil),
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				"custom-provider": {Keys: []schemas.Key{{ID: "key-1"}}},
			},
		},
	}

	// Reaching the end without panicking is the assertion: s.Client is nil, so
	// any scheduled fetch would dereference it.
	server.RefreshAllLiveModels(context.Background())

	if models := server.Config.ModelCatalog.GetModelsForProvider("custom-provider"); len(models) != 0 {
		t.Fatalf("expected no live models to be written, got %v", models)
	}
}

// TestRefreshAllLiveModels_SkipsProvidersWithNoEnabledKeys cross-checks that
// the per-provider skip rules still apply when the fan-out is driven by the
// background pass rather than the bootstrap loop.
func TestRefreshAllLiveModels_SkipsProvidersWithNoEnabledKeys(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	server := &BifrostHTTPServer{
		Config: &lib.Config{
			ModelCatalog: modelcatalog.NewTestCatalog(nil),
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				"all-disabled": {Keys: []schemas.Key{
					{ID: "key-1", Enabled: schemas.Ptr(false)},
					{ID: "key-2", Enabled: schemas.Ptr(false)},
				}},
				"no-keys": {},
			},
		},
	}
	// Non-nil sentinel would be required for a fetch to be attempted; leaving
	// Client nil means any scheduled fetch panics and fails this test.
	server.RefreshAllLiveModels(context.Background())
}

// TestStartLiveModelRefresher_ZeroIntervalDisabled pins the opt-out: a
// non-positive interval must spawn nothing and still return a usable stop func.
func TestStartLiveModelRefresher_ZeroIntervalDisabled(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	server := &BifrostHTTPServer{}

	for _, interval := range []time.Duration{0, -time.Second} {
		before := runtime.NumGoroutine()
		stop := server.startLiveModelRefresher(context.Background(), interval)
		if stop == nil {
			t.Fatalf("interval %v: expected a non-nil stop func", interval)
		}
		if after := runtime.NumGoroutine(); after > before {
			t.Fatalf("interval %v: expected no goroutine to be spawned, went from %d to %d", interval, before, after)
		}
		stop() // must not panic
	}
}

// TestStartLiveModelRefresher_StopsOnContextCancel makes sure the refresher
// does not outlive the server: a leaked ticker would keep calling every
// upstream forever after shutdown.
func TestStartLiveModelRefresher_StopsOnContextCancel(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	server := &BifrostHTTPServer{}

	baseline := runtime.NumGoroutine()
	stop := server.startLiveModelRefresher(context.Background(), 10*time.Millisecond)

	// Let it turn over a few cycles so we are stopping a running loop, not one
	// that never started.
	time.Sleep(50 * time.Millisecond)

	stop()

	// Poll rather than sleeping a fixed amount: goroutine teardown is not
	// synchronous with cancel, and a generous window beats a flaky one.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("refresher goroutine still running 2s after stop: baseline %d, now %d", baseline, runtime.NumGoroutine())
}

// TestJitteredInterval_StaysWithinBounds pins the jitter window. Pods in a
// deployment boot together, so this is what keeps their refresh cycles from
// converging into a synchronized burst against every upstream.
func TestJitteredInterval_StaysWithinBounds(t *testing.T) {
	base := time.Hour
	lower := time.Duration(float64(base) * (1 - liveRefreshJitterFraction))
	upper := time.Duration(float64(base) * (1 + liveRefreshJitterFraction))

	seen := make(map[time.Duration]struct{})
	for range 1000 {
		got := jitteredInterval(base)
		if got < lower || got > upper {
			t.Fatalf("jitteredInterval(%v) = %v, want within [%v, %v]", base, got, lower, upper)
		}
		seen[got] = struct{}{}
	}
	// A constant would satisfy the bounds check above but defeat the purpose.
	if len(seen) < 2 {
		t.Fatalf("expected jittered intervals to vary, got %d distinct value(s)", len(seen))
	}
}

// TestBeginKeyRefresh_CoordinatesAtKeyGranularity covers the per-key in-flight
// guard: duplicate key refreshes collapse, while distinct keys and providers
// remain independent.
func TestBeginKeyRefresh_CoordinatesAtKeyGranularity(t *testing.T) {
	server := &BifrostHTTPServer{}

	release, ok := server.beginKeyRefresh("openai", "key-1")
	if !ok {
		t.Fatal("expected the first claim to succeed")
	}
	if _, ok := server.beginKeyRefresh("openai", "key-1"); ok {
		t.Fatal("expected a concurrent claim for the same key to be rejected")
	}

	otherKeyRelease, ok := server.beginKeyRefresh("openai", "key-2")
	if !ok {
		t.Fatal("expected a different key for the same provider to succeed")
	}
	otherKeyRelease()

	otherProviderRelease, ok := server.beginKeyRefresh("anthropic", "key-1")
	if !ok {
		t.Fatal("expected a claim for a different provider to succeed")
	}
	otherProviderRelease()

	release()
	release, ok = server.beginKeyRefresh("openai", "key-1")
	if !ok {
		t.Fatal("expected the key to be claimable again after release")
	}
	release()
}

func TestBeginAllKeysRefresh_IsExclusiveWithinProvider(t *testing.T) {
	server := &BifrostHTTPServer{}

	keyRelease, ok := server.beginKeyRefresh("openai", "key-1")
	if !ok {
		t.Fatal("expected the key claim to succeed")
	}
	if _, ok := server.beginAllKeysRefresh("openai"); ok {
		t.Fatal("expected all-keys refresh to conflict with an active key refresh")
	}
	keyRelease()

	allRelease, ok := server.beginAllKeysRefresh("openai")
	if !ok {
		t.Fatal("expected all-keys refresh to succeed after the key refresh completed")
	}
	if _, ok := server.beginAllKeysRefresh("openai"); ok {
		t.Fatal("expected a second all-keys refresh to be rejected")
	}
	if _, ok := server.beginKeyRefresh("openai", "key-2"); ok {
		t.Fatal("expected key refresh to conflict with an active all-keys refresh")
	}

	otherProviderRelease, ok := server.beginKeyRefresh("anthropic", "key-1")
	if !ok {
		t.Fatal("expected another provider to remain independent")
	}
	otherProviderRelease()

	allRelease()
	allRelease, ok = server.beginAllKeysRefresh("openai")
	if !ok {
		t.Fatal("expected all-keys refresh to be claimable again after release")
	}
	allRelease()
}

func TestKeyEnabled(t *testing.T) {
	tests := []struct {
		name string
		key  schemas.Key
		want bool
	}{
		{"nil Enabled defaults to enabled", schemas.Key{}, true},
		{"explicit true is enabled", schemas.Key{Enabled: schemas.Ptr(true)}, true},
		{"explicit false is disabled", schemas.Key{Enabled: schemas.Ptr(false)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := keyEnabled(tt.key); got != tt.want {
				t.Fatalf("keyEnabled(%+v) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

// TestRefreshLiveModelsForProvider_AllKeysDisabled cross-checks the "0
// enabled keys" case for issue #5037: discovery must skip scheduling any
// fetch (and therefore never touch s.Client, left nil here) rather than
// attempting doomed-to-fail per-key calls.
func TestRefreshLiveModelsForProvider_AllKeysDisabled(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	server := &BifrostHTTPServer{
		Config: &lib.Config{
			ModelCatalog: modelcatalog.NewTestCatalog(nil),
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				"custom-provider": {},
			},
		},
	}

	keys := []schemas.Key{
		{ID: "key-1", Enabled: schemas.Ptr(false)},
		{ID: "key-2", Enabled: schemas.Ptr(false)},
	}

	// Must return without panicking: s.Client is nil, so any scheduled fetch
	// would panic on the first ListModelsRequest call. Reaching the end of
	// this test proves no fetch was scheduled for either disabled key.
	server.RefreshLiveModelsForProvider(context.Background(), "custom-provider", keys)

	if models := server.Config.ModelCatalog.GetModelsForProvider("custom-provider"); len(models) != 0 {
		t.Fatalf("expected no live models to be written when all keys are disabled, got %v", models)
	}
}

// TestRefreshLiveModelsForProvider_KeylessWithResidualDisabledKeys is the
// regression guard for issue #8500: a custom provider created keyed, whose
// only key was disabled before it was switched to keyless, kept that disabled
// key in its config. Discovery counted enabled keys before asking whether the
// provider needs keys at all, so it logged "no enabled keys configured" and
// never fetched, leaving the provider with no models. Core ignores keys
// entirely for keyless providers, so discovery must take the keyless path
// whatever residual keys are still stored.
func TestRefreshLiveModelsForProvider_KeylessWithResidualDisabledKeys(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"keyless-model","object":"model","owned_by":"test"}]}`))
	}))
	defer upstream.Close()

	const provider schemas.ModelProvider = "keyless-provider"
	cfg := &lib.Config{
		ModelCatalog: modelcatalog.NewTestCatalog(nil),
		Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
			provider: {
				Keys: []schemas.Key{{ID: "residual-key", Value: *schemas.NewSecretVar("sk-residual"), Models: schemas.WhiteList{"*"}, Weight: 1, Enabled: schemas.Ptr(false)}},
				NetworkConfig: &schemas.NetworkConfig{
					BaseURL:                        upstream.URL,
					AllowPrivateNetwork:            true,
					DefaultRequestTimeoutInSeconds: 5,
				},
				CustomProviderConfig: &schemas.CustomProviderConfig{
					IsKeyLess:        true,
					BaseProviderType: schemas.OpenAI,
				},
			},
		},
	}
	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: lib.NewBaseAccount(cfg),
		Logger:  bifrost.NewDefaultLogger(schemas.LogLevelError),
	})
	require.NoError(t, err)
	defer client.Shutdown()

	server := &BifrostHTTPServer{Config: cfg, Client: client}
	keys, err := cfg.GetProviderKeysRaw(provider)
	require.NoError(t, err)

	server.RefreshLiveModelsForProvider(context.Background(), provider, keys)

	require.NotZero(t, calls.Load(), "keyless provider with a residual disabled key never reached the upstream /v1/models")
	require.NotEmpty(t, cfg.ModelCatalog.GetModelsForProvider(provider), "keyless provider with a residual disabled key has no live models after discovery")
}

// TestRefreshLiveModelsForProvider_KeylessWithResidualEnabledKey covers the
// other half of issue #8500's root cause: a keyless provider whose residual key
// is still enabled was fanned out per key and cached under that key's ID, even
// though core ignores keys for keyless providers. Its entries must land under
// the "" sentinel that the OnKey* helpers and ReloadProvider's keep set use.
func TestRefreshLiveModelsForProvider_KeylessWithResidualEnabledKey(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"keyless-model","object":"model","owned_by":"test"}]}`))
	}))
	defer upstream.Close()

	const provider schemas.ModelProvider = "keyless-provider"
	cfg := &lib.Config{
		ModelCatalog: modelcatalog.NewTestCatalog(nil),
		Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
			provider: {
				Keys: []schemas.Key{{ID: "residual-key", Value: *schemas.NewSecretVar("sk-residual"), Models: schemas.WhiteList{"*"}, Weight: 1}},
				NetworkConfig: &schemas.NetworkConfig{
					BaseURL:                        upstream.URL,
					AllowPrivateNetwork:            true,
					DefaultRequestTimeoutInSeconds: 5,
				},
				CustomProviderConfig: &schemas.CustomProviderConfig{
					IsKeyLess:        true,
					BaseProviderType: schemas.OpenAI,
				},
			},
		},
	}
	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: lib.NewBaseAccount(cfg),
		Logger:  bifrost.NewDefaultLogger(schemas.LogLevelError),
	})
	require.NoError(t, err)
	defer client.Shutdown()

	server := &BifrostHTTPServer{Config: cfg, Client: client}
	keys, err := cfg.GetProviderKeysRaw(provider)
	require.NoError(t, err)

	server.RefreshLiveModelsForProvider(context.Background(), provider, keys)
	require.NotEmpty(t, cfg.ModelCatalog.GetModelsForProvider(provider), "keyless provider has no live models after discovery")

	// Dropping the residual key's entries must leave the catalog intact, and
	// dropping the sentinel's must empty it: everything was cached under "".
	cfg.ModelCatalog.InvalidateLive(provider, "residual-key")
	require.NotEmpty(t, cfg.ModelCatalog.GetModelsForProvider(provider), "keyless models were cached under the residual key's ID")
	cfg.ModelCatalog.InvalidateLive(provider, "")
	require.Empty(t, cfg.ModelCatalog.GetModelsForProvider(provider), "keyless models were not cached under the \"\" sentinel")
}

// TestOnKeyAdded_DisabledKeySkipsFetch cross-checks that adding a key that
// is already disabled never schedules a discovery fetch for it.
func TestOnKeyAdded_DisabledKeySkipsFetch(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	disabledKey := schemas.Key{ID: "key-1", Enabled: schemas.Ptr(false)}
	server := &BifrostHTTPServer{
		Config: &lib.Config{
			ModelCatalog: modelcatalog.NewTestCatalog(nil),
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				"custom-provider": {Keys: []schemas.Key{disabledKey}},
			},
		},
	}

	// s.Client is left nil: if the disabled-key guard regresses, the fetch
	// goroutine would dereference it and panic, failing this test.
	if err := server.OnKeyAdded(context.Background(), "custom-provider", disabledKey); err != nil {
		t.Fatalf("OnKeyAdded returned unexpected error: %v", err)
	}

	if models := server.Config.ModelCatalog.GetModelsForProvider("custom-provider"); len(models) != 0 {
		t.Fatalf("expected no live models to be written for a disabled key, got %v", models)
	}
}

// TestOnKeyUpdated_DisabledKeySkipsFetchButInvalidatesCache cross-checks
// that toggling a key to disabled still evicts its stale cached models
// (InvalidateLive) even though the refetch is correctly skipped.
func TestOnKeyUpdated_DisabledKeySkipsFetchButInvalidatesCache(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	catalog := modelcatalog.NewTestCatalog(nil)
	catalog.UpsertLive("custom-provider", "key-1", false, []string{"custom-provider/some-model"})

	disabledKey := schemas.Key{ID: "key-1", Enabled: schemas.Ptr(false)}
	server := &BifrostHTTPServer{
		Config: &lib.Config{
			ModelCatalog: catalog,
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				"custom-provider": {Keys: []schemas.Key{disabledKey}},
			},
		},
	}

	if models := server.Config.ModelCatalog.GetModelsForProvider("custom-provider"); len(models) == 0 {
		t.Fatalf("expected seeded live model before update, got none")
	}

	// s.Client is left nil: if the disabled-key guard regresses, the fetch
	// goroutine would dereference it and panic, failing this test.
	if err := server.OnKeyUpdated(context.Background(), "custom-provider", disabledKey); err != nil {
		t.Fatalf("OnKeyUpdated returned unexpected error: %v", err)
	}

	if models := server.Config.ModelCatalog.GetModelsForProvider("custom-provider"); len(models) != 0 {
		t.Fatalf("expected InvalidateLive to drop the disabled key's cached models, got %v", models)
	}
}

// reloadProviderConfigStore is the minimum ConfigStore ReloadProvider needs:
// it only ever calls GetProvider before touching the model catalog.
type reloadProviderConfigStore struct {
	configstore.ConfigStore
	provider *configstoreTables.TableProvider
}

func (s *reloadProviderConfigStore) GetProvider(context.Context, schemas.ModelProvider) (*configstoreTables.TableProvider, error) {
	return s.provider, nil
}

// newReloadProviderServer builds a BifrostHTTPServer wired for ReloadProvider
// with model discovery guaranteed to write nothing.
//
// Disabling list_models via AllowedRequests makes FetchAndStoreLiveForKey
// return before it issues a request, which is the same observable outcome as
// the transient failures this suite is about (upstream 5xx, rate limit,
// timeout, refused connection): that function writes to the catalog only on a
// successful response, so every failure mode is indistinguishable from here.
// It also lets Client stay a bare non-nil pointer, so any regression that does
// attempt a fetch panics instead of silently reaching the network.
// The custom provider config is set on both the stored row and the in-memory
// config on purpose: ReloadProvider reads the keyless flag off the row it
// loads from the config store, while the isKeylessProvider helper that
// RefreshLiveModelsForProvider and the OnKey* handlers use reads the in-memory
// copy. A fixture that populated only one would exercise a state the running
// server never has.
func newReloadProviderServer(catalog *modelcatalog.ModelCatalog, provider schemas.ModelProvider, keys []schemas.Key, keyless bool) *BifrostHTTPServer {
	customProviderConfig := &schemas.CustomProviderConfig{
		IsKeyLess:       keyless,
		AllowedRequests: &schemas.AllowedRequests{ListModels: false},
	}
	return &BifrostHTTPServer{
		Ctx:    schemas.NewBifrostContext(context.Background(), schemas.NoDeadline),
		Client: &bifrost.Bifrost{},
		Config: &lib.Config{
			ConfigStore: &reloadProviderConfigStore{provider: &configstoreTables.TableProvider{
				Name:                 string(provider),
				CustomProviderConfig: customProviderConfig,
			}},
			ModelCatalog: catalog,
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				provider: {
					Keys:                 keys,
					CustomProviderConfig: customProviderConfig,
				},
			},
		},
	}
}

// TestReloadProvider_FailedRefetchKeepsPreviousCatalog is the regression guard
// for issue #5554: ReloadProvider used to wipe the provider's whole live
// catalog before re-fetching, so a re-fetch that wrote nothing left the
// provider with no live models at all. Editing an unrelated provider field
// while the upstream was briefly unhealthy therefore emptied GET /v1/models
// until a later edit or a restart repaired it.
func TestReloadProvider_FailedRefetchKeepsPreviousCatalog(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	catalog := modelcatalog.NewTestCatalog(nil)
	catalog.UpsertLive("custom-provider", "key-1", false, []string{"some-model"})
	catalog.UpsertLive("custom-provider", "key-1", true, []string{"some-model", "unlisted-model"})

	server := newReloadProviderServer(catalog, "custom-provider", []schemas.Key{{ID: "key-1"}}, false)

	if _, err := server.ReloadProvider(context.Background(), "custom-provider", false); err != nil {
		t.Fatalf("ReloadProvider returned unexpected error: %v", err)
	}

	if got := catalog.GetModelsForProvider("custom-provider"); !slices.Equal(got, []string{"some-model"}) {
		t.Errorf("filtered catalog after failed refetch = %v, want [some-model] retained", got)
	}
	if got := catalog.GetUnfilteredModelsForProvider("custom-provider"); !slices.Equal(got, []string{"some-model", "unlisted-model"}) {
		t.Errorf("unfiltered catalog after failed refetch = %v, want [some-model unlisted-model] retained", got)
	}
}

// TestReloadProvider_PrunesRemovedAndDisabledKeys pins the half of the old
// invalidate that must survive the fix. Retaining is not "skip the cleanup":
// keys that left the provider's set, and keys that are still configured but
// disabled, must still lose their cached entries. Disabled keys matter because
// RefreshLiveModelsForProvider never re-fetches for them, so a retained entry
// would be served indefinitely for a key core rejects at routing time.
func TestReloadProvider_PrunesRemovedAndDisabledKeys(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	catalog := modelcatalog.NewTestCatalog(nil)
	catalog.UpsertLive("custom-provider", "key-enabled", false, []string{"kept-model"})
	catalog.UpsertLive("custom-provider", "key-disabled", false, []string{"disabled-model"})
	catalog.UpsertLive("custom-provider", "key-removed", false, []string{"removed-model"})
	catalog.UpsertLive("custom-provider", "key-removed", true, []string{"removed-model"})

	keys := []schemas.Key{
		{ID: "key-enabled"},
		{ID: "key-disabled", Enabled: schemas.Ptr(false)},
	}
	server := newReloadProviderServer(catalog, "custom-provider", keys, false)

	if _, err := server.ReloadProvider(context.Background(), "custom-provider", false); err != nil {
		t.Fatalf("ReloadProvider returned unexpected error: %v", err)
	}

	if got := catalog.GetModelsForProvider("custom-provider"); !slices.Equal(got, []string{"kept-model"}) {
		t.Errorf("filtered catalog = %v, want only [kept-model] (disabled and removed keys pruned)", got)
	}
	if got := catalog.GetUnfilteredModelsForProvider("custom-provider"); len(got) != 0 {
		t.Errorf("unfiltered catalog = %v, want empty (only the removed key had an unfiltered entry)", got)
	}
}

// TestReloadProvider_KeylessProviderRetainsSentinelEntry covers keyless
// providers (Vertex workload identity, Bedrock IAM, custom keyless), whose
// entries are cached under the "" key ID rather than a real key ID. The retain
// set has to name that sentinel explicitly or the fix would still empty every
// keyless provider's catalog on a failed refetch.
func TestReloadProvider_KeylessProviderRetainsSentinelEntry(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	catalog := modelcatalog.NewTestCatalog(nil)
	catalog.UpsertLive("keyless-provider", "", false, []string{"keyless-model"})

	server := newReloadProviderServer(catalog, "keyless-provider", nil, true)

	if _, err := server.ReloadProvider(context.Background(), "keyless-provider", false); err != nil {
		t.Fatalf("ReloadProvider returned unexpected error: %v", err)
	}

	if got := catalog.GetModelsForProvider("keyless-provider"); !slices.Equal(got, []string{"keyless-model"}) {
		t.Errorf("keyless catalog after failed refetch = %v, want [keyless-model] retained", got)
	}
}

// TestReloadProvider_ZeroKeyProviderClaimsModelsOnlyWhenKeyless pins that a
// custom provider reloaded with zero keys is a catalog candidate for a bare
// model name only when it is keyless. One that needs keys (its only key was
// rejected at save) used to claim every model and win the resolver's
// alphabetical pick, so POST /openai/v1/embeddings {"model":"text-embedding-005"}
// failed with no_key_supports_model instead of reaching vertex.
func TestReloadProvider_ZeroKeyProviderClaimsModelsOnlyWhenKeyless(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	const provider schemas.ModelProvider = "harness-oauthkey-run"
	for _, keyless := range []bool{false, true} {
		t.Run(fmt.Sprintf("keyless=%v", keyless), func(t *testing.T) {
			catalog := modelcatalog.NewTestCatalog(nil)
			server := newReloadProviderServer(catalog, provider, nil, keyless)
			if _, err := server.ReloadProvider(context.Background(), provider, false); err != nil {
				t.Fatalf("ReloadProvider returned unexpected error: %v", err)
			}
			got := slices.Contains(catalog.GetProvidersForModel("text-embedding-005"), provider)
			if got != keyless {
				t.Fatalf("zero-key provider (keyless=%v) is a candidate for text-embedding-005: %v, want %v", keyless, got, keyless)
			}
		})
	}
}

// TestReloadProvider_KeylessPrunesResidualKeyEntries pins that a keyless
// provider keeps only its "" sentinel entry across a reload. Entries cached
// under a residual key's ID (written before issue #8500's fix, when keyless
// discovery still fanned out per enabled key) are never refetched, so keeping
// them would advertise their models indefinitely.
func TestReloadProvider_KeylessPrunesResidualKeyEntries(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	catalog := modelcatalog.NewTestCatalog(nil)
	catalog.UpsertLive("keyless-provider", "", false, []string{"keyless-model"})
	catalog.UpsertLive("keyless-provider", "residual-key", false, []string{"residual-model"})

	server := newReloadProviderServer(catalog, "keyless-provider", []schemas.Key{{ID: "residual-key"}}, true)

	if _, err := server.ReloadProvider(context.Background(), "keyless-provider", false); err != nil {
		t.Fatalf("ReloadProvider returned unexpected error: %v", err)
	}

	if got := catalog.GetModelsForProvider("keyless-provider"); !slices.Equal(got, []string{"keyless-model"}) {
		t.Errorf("keyless catalog after reload = %v, want only [keyless-model] (residual key entry pruned)", got)
	}
}

// TestReloadProvider_NoKeysDropsEverything pins the branch that must keep
// wiping: a keyed provider with no keys left can serve nothing, so no cached
// entry is still meaningful and none will ever be refreshed.
func TestReloadProvider_NoKeysDropsEverything(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	catalog := modelcatalog.NewTestCatalog(nil)
	catalog.UpsertLive("custom-provider", "key-1", false, []string{"orphaned-model"})

	server := newReloadProviderServer(catalog, "custom-provider", nil, false)

	if _, err := server.ReloadProvider(context.Background(), "custom-provider", false); err != nil {
		t.Fatalf("ReloadProvider returned unexpected error: %v", err)
	}

	if got := catalog.GetModelsForProvider("custom-provider"); len(got) != 0 {
		t.Errorf("catalog for keyless-less provider = %v, want empty", got)
	}
}

// TestReloadProvider_ConcurrentReloadsAndReadsAreRaceFree covers the layer
// above the store. ReloadProvider's catalog work is three separate store calls
// (SetKeyConfigForProvider, RetainLiveKeys, then the per-key upserts inside
// RefreshLiveModelsForProvider), so it is atomic per call but not as a
// sequence. Two provider edits landing at once, or an edit landing while
// GET /v1/models is being served, must stay race-free and leave the catalog
// coherent even though the interleaving of those three steps is unspecified.
//
// The keep map is built fresh inside each ReloadProvider call and never
// escapes it, which is what keeps concurrent callers from sharing it.
func TestReloadProvider_ConcurrentReloadsAndReadsAreRaceFree(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	catalog := modelcatalog.NewTestCatalog(nil)
	catalog.UpsertLive("custom-provider", "key-1", false, []string{"some-model"})
	catalog.UpsertLive("custom-provider", "key-1", true, []string{"some-model", "unlisted-model"})

	server := newReloadProviderServer(catalog, "custom-provider", []schemas.Key{{ID: "key-1"}}, false)

	const reloaders = 4
	const readers = 4
	const iterations = 50

	var wg sync.WaitGroup
	for range reloaders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				if _, err := server.ReloadProvider(context.Background(), "custom-provider", false); err != nil {
					t.Errorf("concurrent ReloadProvider returned error: %v", err)
					return
				}
			}
		}()
	}
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				_ = catalog.GetModelsForProvider("custom-provider")
				_ = catalog.GetUnfilteredModelsForProvider("custom-provider")
			}
		}()
	}
	wg.Wait()

	// Retention is not just race-free but stable under repetition: no
	// interleaving of concurrent reloads may drop an entry whose key is
	// present and enabled in every one of them.
	if got := catalog.GetModelsForProvider("custom-provider"); !slices.Equal(got, []string{"some-model"}) {
		t.Errorf("filtered catalog after concurrent reloads = %v, want [some-model] retained", got)
	}
	if got := catalog.GetUnfilteredModelsForProvider("custom-provider"); !slices.Equal(got, []string{"some-model", "unlisted-model"}) {
		t.Errorf("unfiltered catalog after concurrent reloads = %v, want both entries retained", got)
	}
}

func TestMarshalPluginConfig_WithPointerType(t *testing.T) {
	// Test case 1: source is already *T
	expected := &TestConfig{
		Name:    "test-plugin",
		Enabled: true,
		Count:   42,
	}

	result, err := MarshalPluginConfig[TestConfig](expected)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if result != expected {
		t.Errorf("Expected same pointer, got different pointer")
	}

	if result.Name != expected.Name {
		t.Errorf("Expected Name=%s, got %s", expected.Name, result.Name)
	}
	if result.Enabled != expected.Enabled {
		t.Errorf("Expected Enabled=%v, got %v", expected.Enabled, result.Enabled)
	}
	if result.Count != expected.Count {
		t.Errorf("Expected Count=%d, got %d", expected.Count, result.Count)
	}
}

func TestMarshalPluginConfig_WithMap(t *testing.T) {
	// Test case 2: source is map[string]any
	configMap := map[string]any{
		"name":    "test-plugin",
		"enabled": true,
		"count":   42,
	}

	result, err := MarshalPluginConfig[TestConfig](configMap)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if result == nil {
		t.Fatal("Expected non-nil result")
	}

	if result.Name != "test-plugin" {
		t.Errorf("Expected Name=test-plugin, got %s", result.Name)
	}
	if result.Enabled != true {
		t.Errorf("Expected Enabled=true, got %v", result.Enabled)
	}
	if result.Count != 42 {
		t.Errorf("Expected Count=42, got %d", result.Count)
	}
}

func TestMarshalPluginConfig_WithString(t *testing.T) {
	// Test case 3: source is string (JSON)
	configStr := `{"name":"test-plugin","enabled":true,"count":42}`

	result, err := MarshalPluginConfig[TestConfig](configStr)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if result == nil {
		t.Fatal("Expected non-nil result")
	}

	if result.Name != "test-plugin" {
		t.Errorf("Expected Name=test-plugin, got %s", result.Name)
	}
	if result.Enabled != true {
		t.Errorf("Expected Enabled=true, got %v", result.Enabled)
	}
	if result.Count != 42 {
		t.Errorf("Expected Count=42, got %d", result.Count)
	}
}

func TestMarshalPluginConfig_WithInvalidType(t *testing.T) {
	// Test case 4: source is invalid type (should return error)
	invalidSource := 12345

	result, err := MarshalPluginConfig[TestConfig](invalidSource)
	if err == nil {
		t.Fatal("Expected error for invalid type, got nil")
	}

	if result != nil {
		t.Errorf("Expected nil result for invalid type, got %v", result)
	}

	expectedError := "invalid config type"
	if err.Error() != expectedError {
		t.Errorf("Expected error message '%s', got '%s'", expectedError, err.Error())
	}
}

func TestMarshalPluginConfig_WithInvalidJSONString(t *testing.T) {
	// Test case 5: source is string but invalid JSON
	invalidJSON := `{"name":"test-plugin","enabled":true,count:42}` // missing quotes around count

	result, err := MarshalPluginConfig[TestConfig](invalidJSON)
	if err == nil {
		t.Fatal("Expected error for invalid JSON, got nil")
	}

	if result != nil {
		t.Errorf("Expected nil result for invalid JSON, got %v", result)
	}
}

func TestMarshalPluginConfig_WithInvalidMapData(t *testing.T) {
	// Test case 6: source is map but contains invalid data types
	configMap := map[string]any{
		"name":    "test-plugin",
		"enabled": "not-a-boolean", // wrong type
		"count":   42,
	}

	result, err := MarshalPluginConfig[TestConfig](configMap)
	if err == nil {
		t.Fatal("Expected error for invalid map data, got nil")
	}

	if result != nil {
		t.Errorf("Expected nil result for invalid map data, got %v", result)
	}
}

func TestMarshalPluginConfig_WithEmptyMap(t *testing.T) {
	// Test case 7: source is empty map (should work, return zero values)
	configMap := map[string]any{}

	result, err := MarshalPluginConfig[TestConfig](configMap)
	if err != nil {
		t.Fatalf("Expected no error for empty map, got: %v", err)
	}

	if result == nil {
		t.Fatal("Expected non-nil result")
	}

	// All fields should have zero values
	if result.Name != "" {
		t.Errorf("Expected empty Name, got %s", result.Name)
	}
	if result.Enabled != false {
		t.Errorf("Expected Enabled=false, got %v", result.Enabled)
	}
	if result.Count != 0 {
		t.Errorf("Expected Count=0, got %d", result.Count)
	}
}

func TestMarshalPluginConfig_WithEmptyString(t *testing.T) {
	// Test case 8: source is empty string (should fail as invalid JSON)
	configStr := ""

	result, err := MarshalPluginConfig[TestConfig](configStr)
	if err == nil {
		t.Fatal("Expected error for empty string, got nil")
	}

	if result != nil {
		t.Errorf("Expected nil result for empty string, got %v", result)
	}
}

func TestMarshalPluginConfig_WithNil(t *testing.T) {
	// Test case 9: source is nil (should return error as invalid type)
	result, err := MarshalPluginConfig[TestConfig](nil)
	if err == nil {
		t.Fatal("Expected error for nil source, got nil")
	}

	if result != nil {
		t.Errorf("Expected nil result for nil source, got %v", result)
	}
}

// Benchmark tests
func BenchmarkMarshalPluginConfig_WithPointerType(b *testing.B) {
	config := &TestConfig{
		Name:    "test-plugin",
		Enabled: true,
		Count:   42,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = MarshalPluginConfig[TestConfig](config)
	}
}

func BenchmarkMarshalPluginConfig_WithMap(b *testing.B) {
	configMap := map[string]any{
		"name":    "test-plugin",
		"enabled": true,
		"count":   42,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = MarshalPluginConfig[TestConfig](configMap)
	}
}

func BenchmarkMarshalPluginConfig_WithString(b *testing.B) {
	configStr := `{"name":"test-plugin","enabled":true,"count":42}`

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = MarshalPluginConfig[TestConfig](configStr)
	}
}

// Complex config for additional testing
type ComplexConfig struct {
	Settings map[string]string `json:"settings"`
	Tags     []string          `json:"tags"`
	Metadata map[string]any    `json:"metadata"`
	Nested   *TestConfig       `json:"nested"`
}

func TestMarshalPluginConfig_WithComplexType(t *testing.T) {
	// Test with a more complex nested structure
	configMap := map[string]any{
		"settings": map[string]any{
			"key1": "value1",
			"key2": "value2",
		},
		"tags": []any{"tag1", "tag2", "tag3"},
		"metadata": map[string]any{
			"version": "1.0.0",
			"author":  "test",
		},
		"nested": map[string]any{
			"name":    "nested-config",
			"enabled": true,
			"count":   10,
		},
	}

	result, err := MarshalPluginConfig[ComplexConfig](configMap)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if result == nil {
		t.Fatal("Expected non-nil result")
	}

	if len(result.Settings) != 2 {
		t.Errorf("Expected 2 settings, got %d", len(result.Settings))
	}
	if len(result.Tags) != 3 {
		t.Errorf("Expected 3 tags, got %d", len(result.Tags))
	}
	if result.Nested == nil {
		t.Fatal("Expected non-nil nested config")
	}
	if result.Nested.Name != "nested-config" {
		t.Errorf("Expected nested name=nested-config, got %s", result.Nested.Name)
	}
}

type fakePromptsPlugin struct {
	name    string
	reloads int
	err     error
}

func (p *fakePromptsPlugin) GetName() string { return p.name }
func (p *fakePromptsPlugin) Cleanup() error  { return nil }
func (p *fakePromptsPlugin) Reload(context.Context) error {
	p.reloads++
	return p.err
}

// nonReloadingPlugin is registered under the prompts plugin name but cannot reload.
type nonReloadingPlugin struct{ name string }

func (p *nonReloadingPlugin) GetName() string { return p.name }
func (p *nonReloadingPlugin) Cleanup() error  { return nil }

func promptCacheServer(plugins ...schemas.BasePlugin) *BifrostHTTPServer {
	SetLogger(bifrost.NewNoOpLogger())
	config := &lib.Config{}
	config.BasePlugins.Store(&plugins)
	return &BifrostHTTPServer{
		Config: config,
		Ctx:    schemas.NewBifrostContext(context.Background(), schemas.NoDeadline),
	}
}

func TestReloadPromptCacheReloadsThePlugin(t *testing.T) {
	plugin := &fakePromptsPlugin{name: "prompts"}
	server := promptCacheServer(plugin)

	require.NoError(t, server.ReloadPromptCache(context.Background()))
	require.Equal(t, 1, plugin.reloads)
}

// A prompt write must not fail just because the prompts plugin is disabled.
func TestReloadPromptCacheWithoutPluginIsNoOp(t *testing.T) {
	server := promptCacheServer()

	require.NoError(t, server.ReloadPromptCache(context.Background()))
}

func TestReloadPromptCacheUsesPluginNameFromContext(t *testing.T) {
	plugin := &fakePromptsPlugin{name: "enterprise-prompts"}
	server := promptCacheServer(plugin)
	ctx := context.WithValue(context.Background(), schemas.BifrostContextKeyPromptsPluginName, "enterprise-prompts")
	server.Ctx = schemas.NewBifrostContext(ctx, schemas.NoDeadline)

	require.NoError(t, server.ReloadPromptCache(context.Background()))
	require.Equal(t, 1, plugin.reloads, "enterprise registers the plugin under its own name")
}

func TestReloadPromptCacheReturnsPluginError(t *testing.T) {
	plugin := &fakePromptsPlugin{name: "prompts", err: errors.New("database unavailable")}
	server := promptCacheServer(plugin)

	require.ErrorContains(t, server.ReloadPromptCache(context.Background()), "database unavailable")
}

// A plugin that cannot reload must not fail the write, but it does warn rather than pass silently.
func TestReloadPromptCacheWithIncompatiblePluginIsNoOp(t *testing.T) {
	server := promptCacheServer(&nonReloadingPlugin{name: "prompts"})

	require.NoError(t, server.ReloadPromptCache(context.Background()))
}

// GetConfiguredProviders hands back the live provider map uncopied, which is safe for the callers
// that index it once. The name listing cannot be written that way: provider add, delete and status
// updates all write to that map in place under the write lock, so a caller ranging it after the read
// lock is released hits a concurrent map iteration and write. That is fatal rather than merely
// stale — it takes the process down — so the slice is built under the lock instead. Run with -race.
func TestGetConfiguredProviderNamesIsSafeAgainstConcurrentProviderEdits(t *testing.T) {
	config := &lib.Config{
		Providers: map[schemas.ModelProvider]configstore.ProviderConfig{schemas.OpenAI: {}},
	}
	store := &GovernanceInMemoryStore{Config: config}

	churn := []schemas.ModelProvider{"churn-a", "churn-b", "churn-c"}
	done := make(chan struct{})
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			// The same in-place mutation AddProvider and RemoveProvider make.
			for _, provider := range churn {
				config.Mu.Lock()
				config.Providers[provider] = configstore.ProviderConfig{}
				delete(config.Providers, provider)
				config.Mu.Unlock()
			}
		}
	}()

	for range 500 {
		if names := store.GetConfiguredProviderNames(); len(names) == 0 {
			t.Fatal("expected the configured providers to be listed")
		}
	}

	close(done)
	writers.Wait()
}

func TestNotificationPublisher_ResolvesPublisherSetAfterRegistration(t *testing.T) {
	s := &BifrostHTTPServer{Config: &lib.Config{}}
	// Captured while the notification service does not exist yet, as RegisterAPIRoutes
	// does when Bootstrap has not run.
	publish := s.notificationPublisher()
	require.NotNil(t, publish, "a handler registered early must still reach the publisher set later")

	var got []schemas.NotificationInput
	s.Config.NotificationPublisher = func(_ context.Context, input schemas.NotificationInput) (*schemas.Notification, error) {
		got = append(got, input)
		return &schemas.Notification{}, nil
	}
	_, err := publish(context.Background(), schemas.NotificationInput{Title: "late"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "late", got[0].Title)
}

// TestReloadProxyConfigUpdatesHTTPClientFactory pins that saving the global proxy reaches
// the factory behind webhooks, skills and plugin downloads, not only provider inference.
func TestReloadProxyConfigUpdatesHTTPClientFactory(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	factory := network.NewHTTPClientFactory(nil, nil)
	s := &BifrostHTTPServer{Config: &lib.Config{}, HTTPClientFactory: factory}
	proxy := &configstoreTables.GlobalProxyConfig{Enabled: true, Type: network.GlobalProxyTypeHTTP, URL: "http://10.0.0.9:3128", EnableForAPI: true}

	if err := s.ReloadProxyConfig(context.Background(), proxy); err != nil {
		t.Fatalf("ReloadProxyConfig: %v", err)
	}
	got := factory.GetProxyConfig()
	if got == nil || !got.Enabled || got.URL != proxy.URL || !got.EnableForAPI {
		t.Fatalf("factory proxy config = %+v, want the reloaded one", got)
	}
}

// TestReloadProxyConfigUpdatesConfigFactory pins that a server running its own bootstrap
// (enterprise never sets s.HTTPClientFactory) still pushes a proxy change to the
// config's factory, the one registered as the process default.
func TestReloadProxyConfigUpdatesConfigFactory(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	factory := network.NewHTTPClientFactory(nil, nil)
	s := &BifrostHTTPServer{Config: &lib.Config{HTTPClientFactory: factory}}
	proxy := &configstoreTables.GlobalProxyConfig{Enabled: true, Type: network.GlobalProxyTypeHTTP, URL: "http://10.0.0.9:3128", EnableForAPI: true}
	if err := s.ReloadProxyConfig(context.Background(), proxy); err != nil {
		t.Fatalf("ReloadProxyConfig: %v", err)
	}
	if got := factory.GetProxyConfig(); got == nil || got.URL != proxy.URL {
		t.Fatalf("config factory proxy = %+v, want the reloaded one", got)
	}
}

// TestReloadProxyConfigAcceptsRemoval pins that removing the global proxy (a nil config,
// which enterprise passes on removal) clears it everywhere instead of panicking.
func TestReloadProxyConfigAcceptsRemoval(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	factory := network.NewHTTPClientFactory(&network.GlobalProxyConfig{Enabled: true, URL: "http://10.0.0.9:3128", EnableForAPI: true}, nil)
	s := &BifrostHTTPServer{Config: &lib.Config{HTTPClientFactory: factory}}
	if err := s.ReloadProxyConfig(context.Background(), nil); err != nil {
		t.Fatalf("ReloadProxyConfig(nil): %v", err)
	}
	if got := factory.GetProxyConfig(); got != nil {
		t.Fatalf("factory proxy = %+v, want none after removal", got)
	}
}

// ctxSensitivePromptStore fails any read whose context is already cancelled. That is how
// a test observes the context ReloadPlugin handed the plugin constructor: prompts.Init
// calls loadCache(ctx) and returns its error.
type ctxSensitivePromptStore struct {
	configstore.ConfigStore
	gotCtx context.Context
}

// errStopAfterInstantiation ends ReloadPlugin at the constructor, before it needs a live
// Bifrost client, so the test can inspect the context the constructor was given.
var errStopAfterInstantiation = errors.New("stop after instantiation")

func (s *ctxSensitivePromptStore) GetPrompts(ctx context.Context, _ *string) ([]configstoreTables.TablePrompt, error) {
	s.gotCtx = ctx
	return nil, errStopAfterInstantiation
}

func (s *ctxSensitivePromptStore) GetAllPromptVersions(ctx context.Context) ([]configstoreTables.TablePromptVersion, error) {
	return nil, ctx.Err()
}

// A plugin outlives the request that loaded it, so ReloadPlugin must hand its constructor
// the server-lifetime context, never the pooled *fasthttp.RequestCtx an admin reload
// arrives on. Deriving long-lived work from the request context is what produced the
// "missing cancel error" panic on shutdown.
func TestReloadPluginGivesTheConstructorTheServerContext(t *testing.T) {
	SetLogger(bifrost.NewNoOpLogger())

	serverCtx, cancelServer := schemas.NewBifrostContextWithCancel(
		context.WithValue(context.Background(), schemas.BifrostContextKeyGovernancePluginName, "governance-custom"),
	)
	defer cancelServer()

	store := &ctxSensitivePromptStore{}
	s := &BifrostHTTPServer{Ctx: serverCtx, Config: &lib.Config{ConfigStore: store}}

	// The admin reload request is already over by the time the constructor runs.
	reloadReqCtx, cancelRequest := context.WithCancel(context.Background())
	cancelRequest()

	err := s.ReloadPlugin(reloadReqCtx, prompts.PluginName, nil, nil, nil, nil)

	if store.gotCtx == nil {
		t.Fatal("the plugin constructor never reached the store")
	}
	if cerr := store.gotCtx.Err(); cerr != nil {
		t.Errorf("constructor context was already cancelled (%v); it must be the server context, not the reload request's", cerr)
	}
	if !errors.Is(err, errStopAfterInstantiation) {
		t.Errorf("ReloadPlugin error = %v, want the constructor's own error", err)
	}
	if got := governancePluginNameFromContext(store.gotCtx); got != "governance-custom" {
		t.Errorf("constructor context governance plugin name = %q, want it inherited from the server context", got)
	}
}

// mismatchVectorStore reports every namespace as already holding vectors of another size, which is
// how a semantic cache config change that the store cannot serve fails to load.
type mismatchVectorStore struct {
	vectorstore.VectorStore
}

func (mismatchVectorStore) CreateNamespace(_ context.Context, namespace string, dimension int, _ map[string]vectorstore.VectorStoreProperties) error {
	return fmt.Errorf("%w: namespace %q already exists with dimension 1536 but config requires %d", vectorstore.ErrDimensionMismatch, namespace, dimension)
}

// loadedStubPlugin stands in for an instance that is already running under a name.
type loadedStubPlugin struct{ name string }

func (p loadedStubPlugin) GetName() string { return p.name }
func (p loadedStubPlugin) Cleanup() error  { return nil }

// A reload whose new instance fails to load leaves the running one in place, so the status keeps
// describing that instance rather than reporting an error for a plugin that is still serving. With
// nothing running, the failure is the plugin's state and is reported as an error.
func TestReloadPluginFailureKeepsTheRunningInstanceStatus(t *testing.T) {
	SetLogger(bifrost.NewNoOpLogger())
	directOnly := map[string]any{"dimension": 1}

	t.Run("an instance is running", func(t *testing.T) {
		cfg := &lib.Config{VectorStore: mismatchVectorStore{}}
		running := []schemas.BasePlugin{loadedStubPlugin{name: semanticcache.PluginName}}
		cfg.BasePlugins.Store(&running)
		cfg.UpdatePluginOverallStatus(semanticcache.PluginName, semanticcache.PluginName, schemas.PluginStatusActive, nil, []schemas.PluginType{schemas.PluginTypeLLM})
		s := &BifrostHTTPServer{Ctx: schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), Config: cfg}

		err := s.ReloadPlugin(context.Background(), semanticcache.PluginName, nil, directOnly, nil, nil)
		require.ErrorIs(t, err, vectorstore.ErrDimensionMismatch)

		status, ok := cfg.GetPluginStatusByName(semanticcache.PluginName)
		require.True(t, ok)
		assert.Equal(t, schemas.PluginStatusActive, status.Status, "the running instance is still serving")
	})

	t.Run("nothing is running", func(t *testing.T) {
		cfg := &lib.Config{VectorStore: mismatchVectorStore{}}
		s := &BifrostHTTPServer{Ctx: schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), Config: cfg}

		err := s.ReloadPlugin(context.Background(), semanticcache.PluginName, nil, directOnly, nil, nil)
		require.ErrorIs(t, err, vectorstore.ErrDimensionMismatch)

		status, ok := cfg.GetPluginStatusByName(semanticcache.PluginName)
		require.True(t, ok)
		assert.Equal(t, schemas.PluginStatusError, status.Status)
	})
}

// semantic_cache configs are checked when the plugin is about to run, not when one is saved. The
// plugins API loads a plugin only when it is enabled, so a cache that no longer passes (an
// embedding provider since deleted, a row saved before validation existed) can still be disabled.
// A config refused at load wraps lib.ErrInvalidPluginConfig and touches no plugin status.
func TestReloadPluginValidatesSemanticCacheConfig(t *testing.T) {
	SetLogger(bifrost.NewNoOpLogger())
	cfg := &lib.Config{
		Providers:   map[schemas.ModelProvider]configstore.ProviderConfig{schemas.OpenAI: {}},
		VectorStore: mismatchVectorStore{},
	}
	s := &BifrostHTTPServer{Ctx: schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), Config: cfg}

	rejected := []struct {
		name   string
		config map[string]any
		want   string
	}{
		{"empty config", map[string]any{}, "requires 'provider' for semantic mode"},
		{"dimension 0", map[string]any{"dimension": 0}, "'dimension' must be >= 1"},
		{"provider with dimension 1", map[string]any{"provider": "openai", "dimension": 1}, "requires 'dimension' > 1 when 'provider' is set"},
		{"provider without embedding_model", map[string]any{"provider": "openai", "dimension": 1536}, "requires 'embedding_model'"},
		{"empty embedding_model", map[string]any{"provider": "openai", "embedding_model": "", "dimension": 1536}, "non-empty 'embedding_model'"},
		{"provider not configured", map[string]any{"provider": "missing", "embedding_model": "text-embedding-3-small", "dimension": 1536}, "failed to get provider config"},
		{"ttl abc", map[string]any{"dimension": 1, "ttl": "abc"}, "failed to parse TTL duration string 'abc'"},
		{"negative ttl", map[string]any{"dimension": 1, "ttl": "-5s"}, "TTL must be non-negative"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			err := s.ReloadPlugin(context.Background(), semanticcache.PluginName, nil, tc.config, nil, nil)
			require.ErrorIs(t, err, lib.ErrInvalidPluginConfig)
			assert.Contains(t, err.Error(), tc.want)
			_, hasStatus := cfg.GetPluginStatusByName(semanticcache.PluginName)
			assert.False(t, hasStatus, "a refused config must leave no plugin status behind")
		})
	}

	t.Run("saving does not validate, so a cache that no longer passes can be disabled", func(t *testing.T) {
		_, err := s.NormalizePluginConfig(semanticcache.PluginName, map[string]any{"provider": "missing", "embedding_model": "text-embedding-3-small", "dimension": 1536})
		require.NoError(t, err)
	})

	t.Run("a valid config goes on to load, nulls included", func(t *testing.T) {
		// The mismatch store refuses every namespace, so a config that passed validation ends in
		// the store's error, not the validation one.
		err := s.ReloadPlugin(context.Background(), semanticcache.PluginName, nil, map[string]any{"provider": nil, "embedding_model": nil, "dimension": 1, "ttl": "10m"}, nil, nil)
		require.ErrorIs(t, err, vectorstore.ErrDimensionMismatch)
		assert.NotErrorIs(t, err, lib.ErrInvalidPluginConfig)
	})
}

// The accessor's own contract: it tracks the server, not the request that called it.
func TestPluginInitContextOutlivesTheReloadRequest(t *testing.T) {
	serverCtx, cancelServer := schemas.NewBifrostContextWithCancel(
		context.WithValue(context.Background(), schemas.BifrostContextKeyGovernancePluginName, "governance-custom"),
	)
	defer cancelServer()
	initCtx := (&BifrostHTTPServer{Ctx: serverCtx}).PluginInitContext()

	if got := governancePluginNameFromContext(initCtx); got != "governance-custom" {
		t.Errorf("governance plugin name = %q, want it inherited from the server context", got)
	}
	// Shutdown must still reach the plugin, or nothing releases its long-lived work.
	cancelServer()
	select {
	case <-initCtx.Done():
	case <-time.After(time.Second):
		t.Error("plugin init context outlived server shutdown; it must be the server context")
	}
}

// A server with no context yet must still yield a usable, never-cancelled context rather
// than a nil one a constructor would panic on.
func TestPluginInitContextFallsBackToBackground(t *testing.T) {
	initCtx := (&BifrostHTTPServer{}).PluginInitContext()
	if initCtx == nil {
		t.Fatal("PluginInitContext returned nil")
	}
	select {
	case <-initCtx.Done():
		t.Error("fallback context is already cancelled")
	default:
	}
}

// issue8212CleanupEvent records when a real deletion ran and which cutoff it used.
type issue8212CleanupEvent struct {
	startedAt time.Time
	cutoff    time.Time
	deleted   int64
	err       error
}

// issue8212RetentionObserver records real SQLite deletions without changing
// the cutoff, batch size, or result.
type issue8212RetentionObserver struct {
	manager logstore.LogRetentionManager
	events  chan issue8212CleanupEvent
}

// failingRetentionConfigStore simulates a failed configuration save.
type failingRetentionConfigStore struct {
	configstore.ConfigStore
}

// UpdateClientConfig rejects the save so the test can verify retention stays unchanged.
func (failingRetentionConfigStore) UpdateClientConfig(context.Context, *configstore.ClientConfig) error {
	return fmt.Errorf("client config persistence failed")
}

// DeleteLogsBatch delegates to SQLite and reports the completed deletion.
func (m *issue8212RetentionObserver) DeleteLogsBatch(ctx context.Context, cutoff time.Time, size int) (int64, error) {
	startedAt := time.Now()
	deleted, err := m.manager.DeleteLogsBatch(ctx, cutoff, size)
	m.events <- issue8212CleanupEvent{startedAt: startedAt, cutoff: cutoff, deleted: deleted, err: err}
	return deleted, err
}

// issue8212AwaitCleanup waits for a deletion and fails if it errors or never runs.
func issue8212AwaitCleanup(t *testing.T, m *issue8212RetentionObserver) issue8212CleanupEvent {
	t.Helper()
	select {
	case event := <-m.events:
		if event.err != nil {
			t.Fatalf("real SQLite deletion failed: %v", event.err)
		}
		return event
	case <-time.After(10 * time.Second):
		t.Fatal("scheduled cleanup did not run")
		return issue8212CleanupEvent{}
	}
}

// TestIssue8212RetentionChange verifies saved retention reaches the next scheduled
// pass of the same running cleaner, using virtual time and real SQLite stores.
func TestIssue8212RetentionChange(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		oldDays, newDays     int
		wantStatus, wantDays int
		failPersistence      bool
	}{
		{"increase_365_to_1000", 365, 1000, 200, 1000, false},
		{"decrease_1000_to_365", 1000, 365, 200, 365, false},
		{"reject_zero", 365, 0, 400, 365, false},
		{"reject_negative", 365, -1, 400, 365, false},
		{"persistence_failure", 365, 1000, 500, 365, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := context.Background()
				logger = noopTestLogger{}
				handlers.SetLogger(noopTestLogger{})
				cs, err := configstore.NewConfigStore(ctx, &configstore.Config{
					Enabled: true, Type: configstore.ConfigStoreTypeSQLite,
					Config: &configstore.SQLiteConfig{Path: filepath.Join(t.TempDir(), "config.db")},
				}, noopTestLogger{})
				if err != nil {
					t.Fatal(err)
				}
				defer cs.Close(ctx)
				ls, err := logstore.NewLogStore(ctx, &logstore.Config{
					Enabled: true, Type: logstore.LogStoreTypeSQLite,
					Config: &logstore.SQLiteConfig{Path: filepath.Join(t.TempDir(), "logs.db")},
				}, noopTestLogger{})
				if err != nil {
					t.Fatal(err)
				}
				defer ls.Close(ctx)
				initial := &configstore.ClientConfig{LogRetentionDays: tc.oldDays}
				if err := cs.UpdateClientConfig(ctx, initial); err != nil {
					t.Fatal(err)
				}
				initial, err = cs.GetClientConfig(ctx)
				if err != nil {
					t.Fatal(err)
				}
				cfg := &lib.Config{ConfigStore: cs, LogsStore: ls, ClientConfig: initial}
				s := &BifrostHTTPServer{Config: cfg}
				manager, ok := ls.(logstore.LogRetentionManager)
				if !ok {
					t.Fatal("SQLite store does not support retention")
				}
				observer := &issue8212RetentionObserver{manager: manager, events: make(chan issue8212CleanupEvent, 64)}
				s.LogsCleaner = logstore.NewLogsCleaner(observer, logstore.CleanerConfig{RetentionDays: initial.LogRetentionDays}, noopTestLogger{})
				s.LogsCleaner.StartCleanupRoutine()
				defer func() {
					s.LogsCleaner.StopCleanupRoutine()
					synctest.Wait()
				}()
				startup := issue8212AwaitCleanup(t, observer)
				if startup.deleted != 0 {
					t.Fatalf("expected empty startup pass, got %d deletions", startup.deleted)
				}
				// Wait for the initial pass to finish and the daily timer to be armed.
				// Virtual time stays still while this goroutine seeds and saves.
				synctest.Wait()
				if tc.failPersistence {
					cfg.ConfigStore = failingRetentionConfigStore{ConfigStore: cs}
				}

				seed := func(id string, ageDays int) {
					ts := time.Now().UTC().AddDate(0, 0, -ageDays)
					if err := ls.Create(ctx, &logstore.Log{ID: id, Timestamp: ts, CreatedAt: ts,
						Object: "chat_completion", Provider: "openai", Model: "test", Status: "success"}); err != nil {
						t.Fatal(err)
					}
				}
				for _, age := range []int{200, 500, 1200} {
					seed(fmt.Sprintf("age-%d", age), age)
				}
				for _, age := range []int{200, 500, 1200} {
					present, err := ls.IsLogEntryPresent(ctx, fmt.Sprintf("age-%d", age))
					if err != nil || !present {
						t.Fatalf("seed age %d missing before save: %v", age, err)
					}
				}

				// Exercise the production route and the actual server reload callback.
				r := router.New()
				handlers.NewConfigHandler(s, cfg).RegisterRoutes(r)
				var req fasthttp.Request
				req.Header.SetMethod("PUT")
				req.Header.SetContentType("application/json")
				req.SetRequestURI("/api/config")
				saveConfig := *initial
				saveConfig.LogRetentionDays = tc.newDays
				body, err := providerUtils.MarshalSorted(struct {
					ClientConfig *configstore.ClientConfig `json:"client_config"`
				}{ClientConfig: &saveConfig})
				if err != nil {
					t.Fatal(err)
				}
				req.SetBody(body)
				var requestCtx fasthttp.RequestCtx
				requestCtx.Init(&req, nil, nil)
				r.Handler(&requestCtx)
				if requestCtx.Response.StatusCode() != tc.wantStatus {
					t.Fatalf("PUT /api/config returned %d, want %d: %s", requestCtx.Response.StatusCode(), tc.wantStatus, requestCtx.Response.Body())
				}
				savedAt := time.Now()
				persisted, err := cs.GetClientConfig(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if persisted.LogRetentionDays != tc.wantDays || cfg.ClientConfig.LogRetentionDays != tc.wantDays {
					t.Fatalf("unexpected retention: DB=%d live=%d expected=%d", persisted.LogRetentionDays, cfg.ClientConfig.LogRetentionDays, tc.wantDays)
				}
				t.Logf("PUT /api/config status=%d; DB retention=%d; live retention=%d", requestCtx.Response.StatusCode(), persisted.LogRetentionDays, cfg.ClientConfig.LogRetentionDays)
				// Cross the production interval (24h plus 15-30m jitter) without
				// restarting the cleaner or changing its scheduler.
				time.Sleep(25 * time.Hour)
				synctest.Wait()

				next := issue8212AwaitCleanup(t, observer)
				if !next.startedAt.After(savedAt) {
					t.Fatal("expected a cleanup pass that started after the save")
				}
				usedDays := int(math.Round(time.Since(next.cutoff).Hours() / 24))
				t.Logf("next scheduled cleanup: retention=%d days; actual SQLite deletions=%d", usedDays, next.deleted)
				for _, age := range []int{200, 500, 1200} {
					present, err := ls.IsLogEntryPresent(ctx, fmt.Sprintf("age-%d", age))
					if err != nil {
						t.Fatal(err)
					}
					want := age < tc.wantDays
					t.Logf("log age=%d days: present=%t; expected=%t", age, present, want)
					if present != want {
						t.Errorf("log age %d days present=%t, want %t for retention=%d", age, present, want, tc.wantDays)
					}
				}
				if usedDays != tc.wantDays {
					t.Errorf("scheduled cleaner used the wrong retention: got %d days, want %d", usedDays, tc.wantDays)
				}

				// A fresh cleaner from persisted settings is the process-restart control.
				s.LogsCleaner.StopCleanupRoutine()
				synctest.Wait()
				func() {
					seed("restart-age-500", 500)
					freshObserver := &issue8212RetentionObserver{manager: manager, events: make(chan issue8212CleanupEvent, 64)}
					fresh := logstore.NewLogsCleaner(freshObserver, logstore.CleanerConfig{RetentionDays: persisted.LogRetentionDays}, noopTestLogger{})
					fresh.StartCleanupRoutine()
					defer func() {
						fresh.StopCleanupRoutine()
						synctest.Wait()
					}()
					event := issue8212AwaitCleanup(t, freshObserver)
					freshDays := int(math.Round(time.Since(event.cutoff).Hours() / 24))
					present, err := ls.IsLogEntryPresent(ctx, "restart-age-500")
					if err != nil {
						t.Fatal(err)
					}
					t.Logf("fresh cleaner retention=%d; 500-day log present=%t", freshDays, present)
					if freshDays != tc.wantDays || present != (500 < tc.wantDays) {
						t.Fatal("fresh cleaner did not honor saved retention")
					}
				}()
			})
		})
	}
}

// The telemetry plugin reads config.CustomLabels, but the loader only ever seeded it
// from client_config.prometheus_labels, so the plugin's own custom_labels were accepted
// and ignored. Both name extra Prometheus labels, so they union.
func TestMergeCustomLabels(t *testing.T) {
	for _, tc := range []struct {
		label  string
		client []string
		plugin []string
		want   []string
	}{
		{"plugin labels alone are honoured", nil, []string{"team"}, []string{"team"}},
		{"client labels alone still work", []string{"environment"}, nil, []string{"environment"}},
		{"both sources union, client first", []string{"environment"}, []string{"team"}, []string{"environment", "team"}},
		{"duplicates collapse", []string{"team"}, []string{"team", "region"}, []string{"team", "region"}},
		{"blank entries are dropped", []string{""}, []string{"team", ""}, []string{"team"}},
		{"neither set yields nil", nil, nil, nil},
	} {
		t.Run(tc.label, func(t *testing.T) {
			got := mergeCustomLabels(tc.client, tc.plugin)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("label %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// The merge must not write into either input's spare capacity, which is how the
// duplicate-label 500s happened before.
func TestMergeCustomLabelsDoesNotAliasItsInputs(t *testing.T) {
	client := make([]string, 1, 8) // spare capacity for an append to scribble into
	client[0] = "environment"

	got := mergeCustomLabels(client, []string{"team"})
	if len(client) != 1 || client[0] != "environment" {
		t.Errorf("client slice mutated: %v", client)
	}
	got[0] = "clobbered"
	if client[0] != "environment" {
		t.Errorf("writing to the result changed the input: %v", client)
	}
}

// newTelemetryPluginForTest builds a telemetry plugin with its own registry, so a test
// can tell which instance recorded a request.
func newTelemetryPluginForTest(t *testing.T) (*telemetry.PrometheusPlugin, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	p, err := telemetry.Init(&telemetry.Config{Registry: reg}, nil, bifrost.NewNoOpLogger())
	if err != nil {
		t.Fatalf("telemetry.Init: %v", err)
	}
	return p, reg
}

func httpRequestsTotal(t *testing.T, reg *prometheus.Registry) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var total float64
	for _, f := range families {
		if f.GetName() != "http_requests_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			total += m.GetCounter().GetValue()
		}
	}
	return total
}

// A config reload constructs a fresh telemetry plugin. The middleware is built once at
// startup, so capturing the plugin pointer there left it recording into the orphaned
// instance and http_* series stopped appearing on /metrics after any reload. Resolving
// per request means the live instance gets the writes.
func TestPrometheusHTTPMiddlewareFollowsAReloadedPlugin(t *testing.T) {
	SetLogger(bifrost.NewNoOpLogger())

	first, firstReg := newTelemetryPluginForTest(t)
	second, secondReg := newTelemetryPluginForTest(t)

	cfg := &lib.Config{}
	cfg.BasePlugins.Store(&[]schemas.BasePlugin{first})
	s := &BifrostHTTPServer{Config: cfg}

	// Built once, as it is at startup, and reused across the reload below.
	mw := s.prometheusHTTPMiddleware()
	handler := mw(func(ctx *fasthttp.RequestCtx) { ctx.Response.SetStatusCode(200) })

	send := func() {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetRequestURI("/v1/chat/completions")
		ctx.Request.Header.SetMethod("POST")
		handler(ctx)
	}

	send()
	if got := httpRequestsTotal(t, firstReg); got != 1 {
		t.Fatalf("first instance recorded %v requests, want 1", got)
	}

	// The reload: a freshly constructed plugin replaces the one the middleware saw.
	cfg.BasePlugins.Store(&[]schemas.BasePlugin{second})
	send()

	if got := httpRequestsTotal(t, secondReg); got != 1 {
		t.Errorf("reloaded instance recorded %v requests, want 1; the middleware is still writing to the orphaned plugin", got)
	}
	if got := httpRequestsTotal(t, firstReg); got != 1 {
		t.Errorf("orphaned instance recorded %v requests, want it left at 1", got)
	}
}

// With no telemetry plugin loaded the request must still be served.
func TestPrometheusHTTPMiddlewarePassesThroughWithoutThePlugin(t *testing.T) {
	SetLogger(bifrost.NewNoOpLogger())

	s := &BifrostHTTPServer{Config: &lib.Config{}}
	served := false
	handler := s.prometheusHTTPMiddleware()(func(ctx *fasthttp.RequestCtx) { served = true })

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.SetRequestURI("/v1/chat/completions")
	handler(ctx)

	if !served {
		t.Error("request was not served when no telemetry plugin is loaded")
	}
}

// TestGovernanceInMemoryStoreProvidersSurviveEdits pins that the provider map the governance
// plugin reads (batchBaseProvider, modelMatcher) can be indexed after the accessor has released
// its lock while a provider edit writes to the live map: the accessor must hand back a copy, not
// the map itself, or the read is a fatal concurrent map access under load.
// The per-request model check reads one provider through GetConfiguredProvider: it takes the read
// lock and copies nothing, unlike GetConfiguredProviders, which snapshots the whole map.
func TestGovernanceInMemoryStoreReadsOneProviderWithoutCopying(t *testing.T) {
	config := &lib.Config{Providers: map[schemas.ModelProvider]configstore.ProviderConfig{schemas.OpenAI: {}, schemas.Anthropic: {}}}
	store := &GovernanceInMemoryStore{Config: config}
	var single interface {
		GetConfiguredProvider(schemas.ModelProvider) (configstore.ProviderConfig, bool)
	} = store

	_, ok := single.GetConfiguredProvider(schemas.OpenAI)
	assert.True(t, ok)
	_, ok = single.GetConfiguredProvider(schemas.Gemini)
	assert.False(t, ok)
	assert.Zero(t, testing.AllocsPerRun(100, func() { _, _ = single.GetConfiguredProvider(schemas.OpenAI) }), "a one-provider read allocates nothing")
}

// BenchmarkGovernanceInMemoryStoreProviderLookup compares reading one provider out of a snapshot of
// the whole map (what a model check did) with reading it alone, across twenty configured providers.
func BenchmarkGovernanceInMemoryStoreProviderLookup(b *testing.B) {
	providers := map[schemas.ModelProvider]configstore.ProviderConfig{}
	for i := 0; i < 20; i++ {
		providers[schemas.ModelProvider(fmt.Sprintf("provider-%d", i))] = configstore.ProviderConfig{}
	}
	providers[schemas.OpenAI] = configstore.ProviderConfig{}
	store := &GovernanceInMemoryStore{Config: &lib.Config{Providers: providers}}
	b.Run("whole_map", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = store.GetConfiguredProviders()[schemas.OpenAI]
		}
	})
	b.Run("single", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _ = store.GetConfiguredProvider(schemas.OpenAI)
		}
	})
}

func TestGovernanceInMemoryStoreProvidersSurviveEdits(t *testing.T) {
	t.Parallel()
	config := &lib.Config{Providers: map[schemas.ModelProvider]configstore.ProviderConfig{schemas.OpenAI: {}}}
	store := &GovernanceInMemoryStore{Config: config}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			providers := store.GetConfiguredProviders()
			_, _ = providers[schemas.OpenAI]
			_, _ = providers[schemas.Anthropic]
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			config.Mu.Lock()
			config.Providers[schemas.Anthropic] = configstore.ProviderConfig{}
			delete(config.Providers, schemas.Anthropic)
			config.Mu.Unlock()
		}
	}()
	wg.Wait()

	providers := store.GetConfiguredProviders()
	config.Mu.Lock()
	config.Providers[schemas.Gemini] = configstore.ProviderConfig{}
	config.Mu.Unlock()
	_, leaked := providers[schemas.Gemini]
	assert.False(t, leaked, "the returned map is a snapshot, not the live map")
}

func startCustomAudioGateway(t *testing.T, dir string) (*BifrostHTTPServer, string, func()) {
	t.Helper()
	log := bifrost.NewNoOpLogger()
	SetLogger(log)
	lib.SetLogger(log)
	handlers.SetLogger(log)
	s := NewBifrostHTTPServer("txe5087", embed.FS{})
	s.AppDir = dir
	require.NoError(t, s.Bootstrap(context.Background()))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- s.Server.Serve(listener) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			s.shutdownGracefully()
			require.NoError(t, <-done)
			require.ErrorIs(t, listener.Close(), net.ErrClosed)
		})
	}
	t.Cleanup(stop)
	return s, "http://" + listener.Addr().String(), stop
}

type customAudioUpstream struct {
	t                  *testing.T
	server             *httptest.Server
	audio              []byte
	speechCalls        atomic.Int32
	transcriptionCalls atomic.Int32
	selectedKey        atomic.Value
	cancelled          chan struct{}
}

func (u *customAudioUpstream) serveChat(w http.ResponseWriter, r *http.Request) {
	t := u.t
	if r.URL.Path == "/openai/v1/models" || r.URL.Path == "/anthropic/v1/models" {
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, `{"data":[{"id":"owned-chat","object":"model","type":"model","display_name":"Owned chat"}]}`)
		require.NoError(t, err)
		return
	}
	if r.URL.Path == "/openai/v1/chat/completions" {
		require.Equal(t, "Bearer owned-openai-value", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, `{"id":"chatcmpl-owned","object":"chat.completion","model":"owned-chat","choices":[{"index":0,"message":{"role":"assistant","content":"Owned chat"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		require.NoError(t, err)
		return
	}
	if r.URL.Path == "/anthropic/v1/messages" {
		require.Equal(t, "owned-anthropic-value", r.Header.Get("x-api-key"))
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, `{"id":"msg-owned","type":"message","role":"assistant","model":"owned-chat","content":[{"type":"text","text":"Owned chat"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		require.NoError(t, err)
		return
	}
	t.Fatalf("unexpected chat upstream path: %s", r.URL.Path)
}

// ServeHTTP 校验并记录任务自有 upstream 的请求。
func (u *customAudioUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t := u.t
	if strings.HasPrefix(r.URL.Path, "/openai/") || strings.HasPrefix(r.URL.Path, "/anthropic/") {
		u.serveChat(w, r)
		return
	}
	if r.URL.Path == "/reseller/v1/models" {
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, `[{"model_id":"eleven_turbo_v2_5","name":"Owned speech","can_do_text_to_speech":true}]`)
		require.NoError(t, err)
		return
	}
	u.selectedKey.Store(r.Header.Get("xi-api-key"))
	if r.URL.Path == "/reseller/v1/speech-to-text" {
		u.serveTranscription(w, r)
		return
	}
	u.serveSpeech(w, r)
}

func (u *customAudioUpstream) serveTranscription(w http.ResponseWriter, r *http.Request) {
	t := u.t
	u.transcriptionCalls.Add(1)
	require.NoError(t, r.ParseMultipartForm(1024*1024))
	require.Equal(t, "scribe_v2", r.FormValue("model_id"))
	file, _, err := r.FormFile("file")
	require.NoError(t, err)
	data, err := io.ReadAll(file)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	require.Equal(t, []byte("owned-upload"), data)
	w.Header().Set("Content-Type", "application/json")
	_, err = io.WriteString(w, `{"text":"Owned transcription","language_code":"eng","language_probability":1,"words":[],"audio_duration_secs":1}`)
	require.NoError(t, err)
}

func (u *customAudioUpstream) serveSpeech(w http.ResponseWriter, r *http.Request) {
	t, audio := u.t, u.audio
	require.True(t, r.URL.Path == "/reseller/v1/text-to-speech/owned-voice" || r.URL.Path == "/reseller/v1/text-to-speech/owned-voice/stream", r.URL.Path)
	u.speechCalls.Add(1)
	var request struct {
		Text  string `json:"text"`
		Model string `json:"model_id"`
	}
	require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
	require.Equal(t, "eleven_turbo_v2_5", request.Model)
	if request.Text == "fail" {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, err := io.WriteString(w, `{"detail":{"status":"owned_failure","message":"controlled failure"}}`)
		require.NoError(t, err)
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("request-id", "owned-request")
	w.Header().Set("x-character-count", "5")
	if request.Text == "empty" {
		w.Header().Set("Content-Length", "0")
		return
	}
	if request.Text == "truncated" {
		w.Header().Set("Content-Length", "100")
		_, err := io.WriteString(w, "partial-audio")
		require.NoError(t, err)
		w.(http.Flusher).Flush()
		return
	}
	if request.Text == "short" {
		w.Header().Set("Content-Length", "28")
		_, err := io.WriteString(w, "TXE-5087-owned-audio-payload")
		require.NoError(t, err)
		return
	}
	if request.Text == "cancel" {
		_, err := w.Write(audio)
		require.NoError(t, err)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			u.cancelled <- struct{}{}
		case <-time.After(5 * time.Second):
			t.Error("upstream stream was not cancelled")
		}
		return
	}
	if request.Text == "slow" {
		_, err := w.Write(audio)
		require.NoError(t, err)
		w.(http.Flusher).Flush()
		time.Sleep(1200 * time.Millisecond)
	}
	_, err := w.Write(audio)
	require.NoError(t, err)
}

const customAudioProvider = "tkp-native-elevenlabs"
const customAudioPolicy = `{"base_provider_type":"elevenlabs","allowed_requests":{"list_models":true,"speech":true,"speech_stream":true,"transcription":true}}`

type customAudioGatewayFixture struct {
	t            *testing.T
	upstream     *customAudioUpstream
	gateway      *BifrostHTTPServer
	base         string
	dir          string
	stop         func()
	client       *http.Client
	selectedName string
	network      string
	settings     string
	keys         []byte
}

func newCustomAudioGatewayFixture(t *testing.T) *customAudioGatewayFixture {
	t.Helper()
	upstream := &customAudioUpstream{t: t, audio: bytes.Repeat([]byte("owned-audio"), 512), cancelled: make(chan struct{}, 1)}
	upstream.server = httptest.NewServer(upstream)
	t.Cleanup(upstream.server.Close)
	dir := t.TempDir()
	for name, data := range map[string]string{"pricing.json": "{}", "params.json": "{}", "mcp.json": `{"servers":[]}`} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(data), 0600))
	}
	config := fmt.Sprintf(`{"setup_token":"txe-5087-setup","client":{"enable_logging":false,"disable_content_logging":true,"enforce_auth_on_inference":false},"providers":{},"framework":{"pricing":{"pricing_url":%q,"model_parameters_url":%q,"mcp_library_url":%q,"mcp_library_sync_interval":0,"live_models_sync_interval":0}}}`, "file://"+filepath.Join(dir, "pricing.json"), "file://"+filepath.Join(dir, "params.json"), "file://"+filepath.Join(dir, "mcp.json"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(config), 0600))
	gateway, base, stop := startCustomAudioGateway(t, dir)
	client := &http.Client{Timeout: 10 * time.Second}
	network := fmt.Sprintf(`{"base_url":%q,"allow_private_network":true,"default_request_timeout_in_seconds":1,"max_retries":0,"retry_backoff_initial":500,"retry_backoff_max":5000}`, upstream.server.URL+"/reseller")
	return &customAudioGatewayFixture{t: t, upstream: upstream, gateway: gateway, base: base, dir: dir, stop: stop,
		client: client, selectedName: "owned-selected", network: network,
		settings: `"network_config":` + network + `,"concurrency_and_buffer_size":{"concurrency":1000,"buffer_size":5000},"custom_provider_config":` + customAudioPolicy}
}

func (f *customAudioGatewayFixture) call(method, path, body, contentType string, want int) ([]byte, http.Header) {
	t := f.t
	t.Helper()
	req, err := http.NewRequest(method, f.base+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("x-bf-api-key", f.selectedName)
	if strings.HasPrefix(path, "/api/") {
		req.Header.Set(handlers.SetupTokenHeader, "txe-5087-setup")
	}
	resp, err := f.client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, want, resp.StatusCode, string(data))
	return data, resp.Header
}

func (f *customAudioGatewayFixture) registerAndUpdate() {
	t, call, provider := f.t, f.call, customAudioProvider
	t.Helper()
	settings := f.settings
	call("POST", "/api/providers", `{"provider":"`+provider+`",`+settings+`}`, "application/json", 200)
	for _, key := range []string{"owned-other", "owned-selected"} {
		call("POST", "/api/providers/"+provider+"/keys", fmt.Sprintf(`{"name":%q,"value":%q,"models":["*"],"weight":1}`, key, key+"-value"), "application/json", 200)
	}
	f.assertContract(true)
	f.keys, _ = call("GET", "/api/providers/"+provider+"/keys", "", "application/json", 200)
	call("PUT", "/api/providers/"+provider, `{`+settings+`}`, "application/json", 200)
	f.assertKeys()
	call("PUT", "/api/providers/"+provider, `{"custom_provider_config":`+customAudioPolicy+`}`, "application/json", 400)
	f.assertKeys()
	f.assertContract(true)
}

func (f *customAudioGatewayFixture) assertKeys() {
	f.t.Helper()
	updated, _ := f.call("GET", "/api/providers/"+customAudioProvider+"/keys", "", "application/json", 200)
	require.JSONEq(f.t, string(f.keys), string(updated))
}

func (f *customAudioGatewayFixture) assertContract(speechAllowed bool) {
	t, call, provider := f.t, f.call, customAudioProvider
	t.Helper()
	data, _ := call("GET", "/api/providers/"+provider, "", "application/json", 200)
	var cfg handlers.ProviderResponse
	require.NoError(t, json.Unmarshal(data, &cfg))
	require.Equal(t, schemas.ModelProvider(provider), cfg.Name)
	require.Equal(t, schemas.SupportedBaseProviders, cfg.SupportedBaseProviders)
	require.Equal(t, "active", cfg.ProviderStatus)
	require.Equal(t, f.upstream.server.URL+"/reseller", cfg.NetworkConfig.BaseURL)
	require.Contains(t, string(data), `"max_retries":0`)
	require.Equal(t, 500*time.Millisecond, cfg.NetworkConfig.RetryBackoffInitial)
	require.Equal(t, 5*time.Second, cfg.NetworkConfig.RetryBackoffMax)
	require.Equal(t, schemas.ConcurrencyAndBufferSize{Concurrency: 1000, BufferSize: 5000}, cfg.ConcurrencyAndBufferSize)
	require.Equal(t, schemas.Elevenlabs, cfg.CustomProviderConfig.BaseProviderType)
	require.True(t, cfg.CustomProviderConfig.AllowedRequests.ListModels)
	require.Equal(t, speechAllowed, cfg.CustomProviderConfig.AllowedRequests.Speech)
	require.True(t, cfg.CustomProviderConfig.AllowedRequests.SpeechStream)
	require.True(t, cfg.CustomProviderConfig.AllowedRequests.Transcription)
}

func (f *customAudioGatewayFixture) setSpeechAllowed(allowed bool) {
	f.t.Helper()
	policy := fmt.Sprintf(`{"base_provider_type":"elevenlabs","allowed_requests":{"list_models":true,"speech":%t,"speech_stream":true,"transcription":true}}`, allowed)
	settings := `"network_config":` + f.network + `,"concurrency_and_buffer_size":{"concurrency":1000,"buffer_size":5000},"custom_provider_config":` + policy
	f.call("PUT", "/api/providers/"+customAudioProvider, `{`+settings+`}`, "application/json", 200)
}

func (f *customAudioGatewayFixture) speech(text, stream string, want int) ([]byte, http.Header) {
	f.t.Helper()
	return f.call("POST", "/v1/audio/speech", fmt.Sprintf(`{"model":"%s/eleven_turbo_v2_5","voice":"owned-voice","input":%q%s}`, customAudioProvider, text, stream), "application/json", want)
}

func (f *customAudioGatewayFixture) assertSpeech() {
	t, speech, provider := f.t, f.speech, customAudioProvider
	t.Helper()
	audio := f.upstream.audio
	data, headers := speech("hello", "", 200)
	require.Equal(t, audio, data)
	require.Equal(t, provider, headers.Get(lib.HeaderBifrostProvider))
	require.Equal(t, "owned-selected-value", f.upstream.selectedKey.Load())
	data, headers = speech("slow", `,"stream_format":"sse"`, 200)
	require.Contains(t, string(data), "speech.audio.done")
	require.Equal(t, append(append([]byte{}, audio...), audio...), decodeCustomAudio(t, data))
	require.Equal(t, provider, headers.Get(lib.HeaderBifrostProvider))
	data, _ = speech("short", `,"stream_format":"sse"`, 200)
	require.Equal(t, []byte("TXE-5087-owned-audio-payload"), decodeCustomAudio(t, data))
	data, _ = speech("empty", `,"stream_format":"sse"`, 200)
	require.Empty(t, decodeCustomAudio(t, data))
	require.Contains(t, string(data), "speech.audio.done")
	data, _ = speech("truncated", `,"stream_format":"sse"`, 200)
	require.Equal(t, []byte("partial-audio"), decodeCustomAudio(t, data))
	require.Contains(t, string(data), `"error"`)
	require.NotContains(t, string(data), "speech.audio.done")
}

func decodeCustomAudio(t *testing.T, data []byte) []byte {
	t.Helper()
	var result []byte
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Audio []byte `json:"audio"`
		}
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event))
		if event.Type == "speech.audio.delta" {
			result = append(result, event.Audio...)
		}
	}
	return result
}

func customAudioUpload(t *testing.T) (string, string) {
	t.Helper()
	var upload bytes.Buffer
	writer := multipart.NewWriter(&upload)
	require.NoError(t, writer.WriteField("model", customAudioProvider+"/scribe_v2"))
	file, err := writer.CreateFormFile("file", "owned.wav")
	require.NoError(t, err)
	_, err = file.Write([]byte("owned-upload"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return upload.String(), writer.FormDataContentType()
}

func (f *customAudioGatewayFixture) assertTranscription() {
	t := f.t
	t.Helper()
	body, contentType := customAudioUpload(t)
	data, headers := f.call("POST", "/v1/audio/transcriptions", body, contentType, 200)
	require.Contains(t, string(data), "Owned transcription")
	require.Equal(t, customAudioProvider, headers.Get(lib.HeaderBifrostProvider))
	require.Equal(t, "owned-selected-value", f.upstream.selectedKey.Load())
	require.EqualValues(t, 1, f.upstream.transcriptionCalls.Load())
}

func (f *customAudioGatewayFixture) assertOneUpstreamAttempt() {
	f.t.Helper()
	before := f.upstream.speechCalls.Load()
	f.speech("fail", "", 503)
	require.Equal(f.t, before+1, f.upstream.speechCalls.Load())
}

func (f *customAudioGatewayFixture) assertCancellation() {
	t, provider := f.t, customAudioProvider
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "POST", f.base+"/v1/audio/speech", strings.NewReader(fmt.Sprintf(`{"model":"%s/eleven_turbo_v2_5","voice":"owned-voice","input":"cancel","stream_format":"sse"}`, provider)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-bf-api-key", "owned-selected")
	resp, err := f.client.Do(req)
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode)
	cancel()
	require.NoError(t, resp.Body.Close())
	select {
	case <-f.upstream.cancelled:
	case <-time.After(6 * time.Second):
		t.Fatal("gateway did not cancel upstream stream")
	}
}

func (f *customAudioGatewayFixture) assertPolicy() {
	t, call, speech, provider := f.t, f.call, f.speech, customAudioProvider
	t.Helper()
	body, contentType := customAudioUpload(t)
	for _, operation := range []string{"speech", "speech_stream", "transcription"} {
		policy := fmt.Sprintf(`{"base_provider_type":"elevenlabs","allowed_requests":{"list_models":true,"speech":%t,"speech_stream":%t,"transcription":%t}}`, operation != "speech", operation != "speech_stream", operation != "transcription")
		settings := `"network_config":` + f.network + `,"concurrency_and_buffer_size":{"concurrency":1000,"buffer_size":5000},"custom_provider_config":` + policy
		call("PUT", "/api/providers/"+provider, `{`+settings+`}`, "application/json", 200)
		before := f.upstream.speechCalls.Load() + f.upstream.transcriptionCalls.Load()
		switch operation {
		case "speech":
			speech("hello", "", 400)
		case "speech_stream":
			speech("hello", `,"stream_format":"sse"`, 400)
		case "transcription":
			call("POST", "/v1/audio/transcriptions", body, contentType, 400)
		}
		require.Equal(t, before, f.upstream.speechCalls.Load()+f.upstream.transcriptionCalls.Load(), operation)
	}
	call("POST", "/api/providers", `{"provider":"owned-unsupported","custom_provider_config":{"base_provider_type":"azure"}}`, "application/json", 400)
}

func (f *customAudioGatewayFixture) assertControlProviders() {
	t, call := f.t, f.call
	t.Helper()
	for _, channel := range []struct{ name, base, prefix, key string }{
		{"elevenlabs", "", "/reseller", "owned-builtin"},
		{"owned-openai-custom", "openai", "/openai", "owned-openai"},
		{"owned-anthropic-custom", "anthropic", "/anthropic", "owned-anthropic"},
	} {
		custom := ""
		if channel.base != "" {
			custom = fmt.Sprintf(`,"custom_provider_config":{"base_provider_type":%q}`, channel.base)
		}
		call("POST", "/api/providers", fmt.Sprintf(`{"provider":%q,"network_config":{"base_url":%q,"allow_private_network":true,"max_retries":0}%s}`, channel.name, f.upstream.server.URL+channel.prefix, custom), "application/json", 200)
		call("POST", "/api/providers/"+channel.name+"/keys", fmt.Sprintf(`{"name":%q,"value":%q,"models":["*"],"weight":1}`, channel.key, channel.key+"-value"), "application/json", 200)
		f.selectedName = channel.key
		var data []byte
		var headers http.Header
		if channel.base == "" {
			data, headers = call("POST", "/v1/audio/speech", `{"model":"elevenlabs/eleven_turbo_v2_5","voice":"owned-voice","input":"hello"}`, "application/json", 200)
			require.Equal(t, f.upstream.audio, data)
			require.Equal(t, "owned-builtin-value", f.upstream.selectedKey.Load())
			data, _ = call("POST", "/v1/audio/speech", `{"model":"elevenlabs/eleven_turbo_v2_5","voice":"owned-voice","input":"short","stream_format":"sse"}`, "application/json", 200)
			require.Equal(t, []byte("TXE-5087-owned-audio-payload"), decodeCustomAudio(t, data))
		} else {
			data, headers = call("POST", "/v1/chat/completions", fmt.Sprintf(`{"model":"%s/owned-chat","messages":[{"role":"user","content":"hello"}],"max_tokens":8}`, channel.name), "application/json", 200)
			require.Contains(t, string(data), "Owned chat")
		}
		require.Equal(t, channel.name, headers.Get(lib.HeaderBifrostProvider))
	}
}

func (f *customAudioGatewayFixture) assertKeyStatusSnapshot() {
	t, gateway := f.t, f.gateway
	provider := schemas.ModelProvider(customAudioProvider)
	t.Helper()
	snapshot, err := gateway.Config.GetProviderConfigRaw(provider)
	require.NoError(t, err)
	oldKey := snapshot.Keys[0]
	gateway.updateKeyStatus(context.Background(), []schemas.KeyStatus{{
		Provider: provider,
		KeyID:    oldKey.ID,
		Status:   schemas.KeyStatusListModelsFailed,
		Error:    &schemas.BifrostError{Error: &schemas.ErrorField{Message: "owned status update"}},
	}})
	require.Equal(t, oldKey.Status, snapshot.Keys[0].Status)
	require.Equal(t, oldKey.Description, snapshot.Keys[0].Description)
	updated, err := gateway.Config.GetProviderConfigRaw(provider)
	require.NoError(t, err)
	require.Equal(t, schemas.KeyStatusListModelsFailed, updated.Keys[0].Status)
	require.Equal(t, "owned status update", updated.Keys[0].Description)
}

func TestCustomElevenlabsGateway(t *testing.T) {
	f := newCustomAudioGatewayFixture(t)
	f.registerAndUpdate()
	f.assertSpeech()
	f.assertTranscription()
	f.assertOneUpstreamAttempt()
	f.setSpeechAllowed(false)
	f.assertContract(false)
	f.assertKeys()
	f.stop()
	f.gateway, f.base, f.stop = startCustomAudioGateway(t, f.dir)
	f.assertContract(false)
	f.assertKeys()
	f.setSpeechAllowed(true)
	f.assertContract(true)
	f.assertOneUpstreamAttempt()
	f.assertCancellation()
	f.assertPolicy()
	f.assertControlProviders()
	f.assertKeyStatusSnapshot()
	f.stop()
}

// reloadVirtualKeyConfigStore provides the persistence calls used by ReloadVirtualKey.
