package server

import (
	"context"
	"path/filepath"
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
	"github.com/capsohq/bifrost/framework/jobaccounting"
	"github.com/capsohq/bifrost/plugins/logging"
	"github.com/capsohq/bifrost/transports/v2/bifrost-http/handlers"
	"github.com/capsohq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/fasthttp/router"
	"github.com/valyala/fasthttp"
)

// The sweeper skips the plugin pipeline, so nothing upstream resolves a key for it.
// Pinning to the key that created the job is what stops core from polling with an
// arbitrary one — which for a video is not a preference but a correctness matter:
// OpenAI scopes a video id to its creating key.
func TestInternalJobContextPinsTheCreatingKey(t *testing.T) {
	ctx := internalJobContext(context.Background(), "key-abc")

	keyID, ok := ctx.Value(schemas.BifrostContextKeyAPIKeyID).(string)
	if !ok || keyID != "key-abc" {
		t.Fatalf("api key id = %q (ok=%v), want key-abc", keyID, ok)
	}
}

// BifrostContextKeyAPIKeyID is a reserved key: writes to it are dropped while the
// plugin pipeline holds restricted writes. This context never enters that phase, so
// the write must land — if that ever changes, the pin silently stops working and
// the sweeper goes back to polling with whatever key core picks.
func TestInternalJobContextReservedWriteIsNotDropped(t *testing.T) {
	ctx := internalJobContext(context.Background(), "key-reserved")
	if _, ok := ctx.Value(schemas.BifrostContextKeyAPIKeyID).(string); !ok {
		t.Fatal("a reserved-key write on a fresh sweeper context must not be dropped")
	}
	// Same reasoning as the skip flags this context has always set.
	if skip, ok := ctx.Value(schemas.BifrostContextKeySkipBudgetAndRateLimits).(bool); !ok || !skip {
		t.Fatal("expected the sweeper context to keep skipping budgets")
	}
}

// A job row written before the key was recorded, or by a path that never had one,
// must still poll rather than pin to the empty string and match nothing.
func TestInternalJobContextWithoutKeyLeavesSelectionOpen(t *testing.T) {
	for _, selectedKeyID := range []string{"", "   "} {
		ctx := internalJobContext(context.Background(), selectedKeyID)
		if _, ok := ctx.Value(schemas.BifrostContextKeyAPIKeyID).(string); ok {
			t.Fatalf("selectedKeyID %q must leave key selection unpinned", selectedKeyID)
		}
	}
}

// The sweeper's own invariants must survive the pin.
func TestInternalJobContextKeepsSweeperInvariants(t *testing.T) {
	ctx := internalJobContext(context.Background(), "key-abc")

	if skip, ok := ctx.Value(schemas.BifrostContextKeySkipPluginPipeline).(bool); !ok || !skip {
		t.Fatal("the sweeper must not re-enter the plugin pipeline")
	}
	if requestID, ok := ctx.Value(schemas.BifrostContextKeyRequestID).(string); !ok || requestID == "" {
		t.Fatal("every sweeper call needs its own request id")
	}
}

// Both job kinds poll through the same pinned context. An earlier revision gave
// batch an unpinned retry on failure, on the theory that a batch id is org-scoped
// so any key could rescue it. That fires on every transient error — an outage, a
// rate limit — doubling load on an upstream already failing, to cover only a key
// deleted mid-flight; and that case is already answered by the job parking as
// unpriceable and by /v1/batches/{id}/results settling inline with the caller's own
// key. One code path, no retry.
func TestInternalJobContextIsTheOnlySweeperContext(t *testing.T) {
	batchCtx := internalJobContext(context.Background(), "key-batch")
	videoCtx := internalJobContext(context.Background(), "key-video")

	for name, ctx := range map[string]*schemas.BifrostContext{"batch": batchCtx, "video": videoCtx} {
		if _, ok := ctx.Value(schemas.BifrostContextKeyAPIKeyID).(string); !ok {
			t.Fatalf("%s: both kinds must poll pinned to their creating key", name)
		}
	}
}

// The video settler recognises a custom provider through this resolver, so a custom provider
// built on a video provider has its jobs settled and billed.
func TestBifrostVideoRetrieverResolvesCustomBaseProvider(t *testing.T) {
	retriever := &bifrostVideoRetriever{config: &lib.Config{Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
		"my-minimax":    {CustomProviderConfig: &schemas.CustomProviderConfig{CustomProviderKey: "my-minimax", BaseProviderType: schemas.MiniMax}},
		schemas.MiniMax: {},
	}}}
	if got := retriever.BaseProvider("my-minimax"); got != schemas.MiniMax {
		t.Errorf("BaseProvider(my-minimax) = %q, want minimax", got)
	}
	if got := retriever.BaseProvider(schemas.MiniMax); got != "" {
		t.Errorf("BaseProvider(minimax) = %q, want empty for a built-in provider", got)
	}
	if got := retriever.BaseProvider("unknown"); got != "" {
		t.Errorf("BaseProvider(unknown) = %q, want empty", got)
	}
	if got := (&bifrostVideoRetriever{}).BaseProvider("my-minimax"); got != "" {
		t.Errorf("a retriever without config resolved %q", got)
	}
	var _ jobaccounting.BaseProviderResolver = retriever
}

// client.disable_provider_job_accounting (fork, TXE-6519) reaches the logging plugin through this
// wiring, with or without a client: a disabled deployment records no provider job and starts no
// sweeper, and the default leaves accounting on, as before the switch existed.
func TestWireBatchAccountingSweeperAppliesProviderJobAccountingSwitch(t *testing.T) {
	SetLogger(bifrost.NewNoOpLogger())
	for _, tc := range []struct {
		name     string
		disabled bool
	}{{"default keeps accounting on", false}, {"switched off", true}} {
		t.Run(tc.name, func(t *testing.T) {
			plugin := &logging.LoggerPlugin{}
			config := &lib.Config{ClientConfig: &configstore.ClientConfig{DisableProviderJobAccounting: tc.disabled}}
			config.BasePlugins.Store(&[]schemas.BasePlugin{plugin})
			s := &BifrostHTTPServer{Config: config}
			s.WireBatchAccountingSweeper()
			if got := plugin.ProviderJobAccountingEnabled(); got != !tc.disabled {
				t.Fatalf("ProviderJobAccountingEnabled() = %v with disable_provider_job_accounting=%v", got, tc.disabled)
			}
		})
	}
}

// A client-config save that flips the switch is honoured without a restart. The save path
// publishes the new client config into the live struct and only then asks for the reload
// (handlers/config.go), so the reload cannot see the change in the config; it reconciles the
// logging plugin's own state instead (fork, TXE-6519).
func TestReloadClientConfigReconcilesProviderJobAccounting(t *testing.T) {
	SetLogger(bifrost.NewNoOpLogger())
	ctx := context.Background()
	store := newSQLiteConfigStore(t)

	plugin := &logging.LoggerPlugin{}
	live := lib.DefaultClientConfig
	config := &lib.Config{ClientConfig: &live, ConfigStore: store}
	config.BasePlugins.Store(&[]schemas.BasePlugin{plugin})
	s := &BifrostHTTPServer{Config: config}
	s.WireBatchAccountingSweeper()
	if !plugin.ProviderJobAccountingEnabled() {
		t.Fatal("accounting must start on with the default client config")
	}

	save := func(disabled bool) {
		t.Helper()
		stored := lib.DefaultClientConfig
		stored.DisableProviderJobAccounting = disabled
		if err := store.UpdateClientConfig(ctx, &stored); err != nil {
			t.Fatalf("UpdateClientConfig() error = %v", err)
		}
		// What the config handler does before it asks for the reload: the live config
		// already carries the saved value when ReloadClientConfigFromConfigStore runs.
		config.Mu.Lock()
		*config.ClientConfig = stored
		config.Mu.Unlock()
		if err := s.ReloadClientConfigFromConfigStore(ctx); err != nil {
			t.Fatalf("ReloadClientConfigFromConfigStore() error = %v", err)
		}
	}

	save(true)
	if plugin.ProviderJobAccountingEnabled() {
		t.Fatal("a reload after a save that sets the switch must stop provider job accounting")
	}
	save(false)
	if !plugin.ProviderJobAccountingEnabled() {
		t.Fatal("a reload after a save that clears the switch must resume provider job accounting")
	}
}

// The whole save path, PUT /api/config through the real config handler with this server as its
// manager, flips the logging plugin without a restart (fork, TXE-6519).
func TestConfigAPISaveFlipsProviderJobAccounting(t *testing.T) {
	SetLogger(bifrost.NewNoOpLogger())
	handlers.SetLogger(bifrost.NewNoOpLogger())
	ctx := context.Background()
	store := newSQLiteConfigStore(t)

	plugin := &logging.LoggerPlugin{}
	live := lib.DefaultClientConfig
	config := &lib.Config{ClientConfig: &live, ConfigStore: store}
	config.BasePlugins.Store(&[]schemas.BasePlugin{plugin})
	if err := store.UpdateClientConfig(ctx, &live); err != nil {
		t.Fatalf("UpdateClientConfig() error = %v", err)
	}
	s := &BifrostHTTPServer{Config: config}
	s.WireBatchAccountingSweeper()
	if !plugin.ProviderJobAccountingEnabled() {
		t.Fatal("accounting must start on with the default client config")
	}

	r := router.New()
	handlers.NewConfigHandler(s, config).RegisterRoutes(r)
	put := func(body string) {
		t.Helper()
		var req fasthttp.Request
		req.Header.SetMethod(fasthttp.MethodPut)
		req.SetRequestURI("/api/config")
		req.Header.SetContentType("application/json")
		req.SetBodyString(body)
		rc := &fasthttp.RequestCtx{}
		rc.Init(&req, nil, nil)
		r.Handler(rc)
		if rc.Response.StatusCode() != fasthttp.StatusOK {
			t.Fatalf("PUT /api/config = %d: %s", rc.Response.StatusCode(), rc.Response.Body())
		}
	}

	// The compat block is sent as the live config holds it: an omitted block decodes to the
	// compat defaults, and the handler would then reload the compat plugin through a client
	// this test does not have. The save under test is the switch alone.
	compat := `"compat":{"convert_text_to_chat":false,"convert_chat_to_responses":false,"should_drop_params":false,"should_convert_params":false,"azure_deepseek":false,"force_reasoning_only_models_to_responses":true,"convert_decision_to_responses":false}`
	put(`{"client_config":{"log_retention_days":8,` + compat + `,"disable_provider_job_accounting":true}}`)
	if plugin.ProviderJobAccountingEnabled() {
		t.Fatal("a save that sets the switch must stop provider job accounting")
	}
	if config.ShouldRunProviderJobAccounting() {
		t.Fatal("the live config must carry the saved switch")
	}
	put(`{"client_config":{"log_retention_days":8,` + compat + `,"disable_provider_job_accounting":false}}`)
	if !plugin.ProviderJobAccountingEnabled() {
		t.Fatal("a save that clears the switch must resume provider job accounting")
	}
}

// newSQLiteConfigStore is a fresh config store with every migration applied, closed with the test.
func newSQLiteConfigStore(t *testing.T) configstore.ConfigStore {
	t.Helper()
	ctx := context.Background()
	store, err := configstore.NewConfigStore(ctx, &configstore.Config{
		Enabled: true,
		Type:    configstore.ConfigStoreTypeSQLite,
		Config:  &configstore.SQLiteConfig{Path: filepath.Join(t.TempDir(), "config.db")},
	}, bifrost.NewNoOpLogger())
	if err != nil {
		t.Fatalf("NewConfigStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close(ctx) })
	return store
}
