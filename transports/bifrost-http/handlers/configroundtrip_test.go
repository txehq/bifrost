package handlers

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"

	"github.com/capsohq/bifrost/framework/configstore"
	configtables "github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/capsohq/bifrost/framework/logstore"
	"github.com/capsohq/bifrost/transports/v2/bifrost-http/lib"
)

// The client config GET /api/config hands out must save back unchanged through PUT /api/config
// (fork, TXE-7070). A gateway whose config never set log_retention_days read 0, which PUT refuses
// with "log_retention_days must be at least 1", so a settings page that changes one field could
// not save at all. These tests are the class check: they read the whole client_config, from memory
// and from the store, and save it back, so the next field whose zero read value fails a save-time
// check fails here too.

// bootRoundTripGateway loads a gateway from a config.json in dir, the way the binary starts, and
// returns a config handler over it.
func bootRoundTripGateway(t *testing.T, dir string, client map[string]any) (*ConfigHandler, *lib.Config) {
	t.Helper()
	datasheet := newDatasheetServer(t)
	data := map[string]any{
		"config_store": map[string]any{
			"enabled": true,
			"type":    "sqlite",
			"config":  map[string]any{"path": filepath.Join(dir, "config.db")},
		},
		// The datasheets come from loopback, so a boot makes no outbound call.
		"framework": map[string]any{
			"pricing": map[string]any{
				"pricing_url":               datasheet.URL + "/pricing.json",
				"model_parameters_url":      datasheet.URL + "/model-parameters.json",
				"mcp_library_sync_interval": 0,
			},
		},
	}
	if client != nil {
		data["client"] = client
	}
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), raw, 0o600))
	cfg, err := lib.LoadConfig(context.Background(), dir)
	require.NoError(t, err)
	t.Cleanup(func() {
		if cfg.ConfigStore != nil {
			_ = cfg.ConfigStore.Close(context.Background())
		}
	})
	return &ConfigHandler{store: cfg, configManager: stubConfigManager{}}, cfg
}

// readClientConfig answers GET /api/config (fromDB adds ?from_db=true) and returns its client_config.
func readClientConfig(t *testing.T, h *ConfigHandler, fromDB bool) map[string]json.RawMessage {
	t.Helper()
	var req fasthttp.Request
	req.Header.SetMethod("GET")
	uri := "/api/config"
	if fromDB {
		uri += "?from_db=true"
	}
	req.SetRequestURI(uri)
	ctx := initCtx(&req)
	h.getConfig(ctx)
	require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode(), string(ctx.Response.Body()))
	var body struct {
		ClientConfig map[string]json.RawMessage `json:"client_config"`
	}
	require.NoError(t, json.Unmarshal(ctx.Response.Body(), &body))
	require.NotEmpty(t, body.ClientConfig, "GET /api/config returned no client_config")
	return body.ClientConfig
}

// saveClientConfig sends client_config back with PUT /api/config, unchanged, and returns the status
// and body.
func saveClientConfig(t *testing.T, h *ConfigHandler, cc map[string]json.RawMessage) (int, string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"client_config": cc})
	require.NoError(t, err)
	ctx := putConfigCtx(string(raw))
	h.updateConfig(ctx)
	return ctx.Response.StatusCode(), string(ctx.Response.Body())
}

func retentionOf(t *testing.T, cc map[string]json.RawMessage) int {
	t.Helper()
	var days int
	require.NoError(t, json.Unmarshal(cc["log_retention_days"], &days))
	return days
}

func TestConfigRoundTrip_ReadClientConfigSavesBackUnchanged(t *testing.T) {
	SetLogger(&mockLogger{})
	cases := []struct {
		name   string
		client map[string]any
	}{
		// The ticket's gateway: a client section that sets other fields but not this one.
		{"client section without log_retention_days", map[string]any{"drop_excess_requests": false, "allow_per_request_raw_override": true}},
		// No client section at all: the gateway starts from the built-in defaults.
		{"no client section", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, cfg := bootRoundTripGateway(t, t.TempDir(), tc.client)
			for _, fromDB := range []bool{false, true} {
				cc := readClientConfig(t, h, fromDB)
				assert.Equal(t, logstore.DefaultLogRetentionDays, retentionOf(t, cc),
					"from_db=%v: a gateway that never set log_retention_days applies the documented default and must report it", fromDB)
				status, body := saveClientConfig(t, h, cc)
				require.Equal(t, fasthttp.StatusOK, status, "from_db=%v: the client_config GET returned does not save back unchanged: %s", fromDB, body)
			}
			assert.Equal(t, logstore.DefaultLogRetentionDays, cfg.ClientConfig.LogRetentionDays, "the save changed the retention the gateway applies")
		})
	}
}

// A store row written before this fix, or by a release that stored the omitted value, holds 0.
// The gateway restarted on it reports and applies the default, and the read saves back.
func TestConfigRoundTrip_StoredZeroRetentionReadsAsDefault(t *testing.T) {
	SetLogger(&mockLogger{})
	dir := t.TempDir()
	h, cfg := bootRoundTripGateway(t, dir, nil)
	// UpdateClientConfig does not write a zero over the column's default, so the row is set directly.
	rdb, ok := cfg.ConfigStore.(*configstore.RDBConfigStore)
	require.True(t, ok, "the sqlite config store is an RDBConfigStore")
	require.NoError(t, rdb.DB().Model(&configtables.TableClientConfig{}).Where("1 = 1").Update("log_retention_days", 0).Error)
	var raw int
	require.NoError(t, rdb.DB().Model(&configtables.TableClientConfig{}).Select("log_retention_days").Scan(&raw).Error)
	require.Equal(t, 0, raw, "the row does not hold 0, so this test would not exercise a stored 0")

	persisted, err := cfg.ConfigStore.GetClientConfig(context.Background())
	require.NoError(t, err)
	require.NotNil(t, persisted)
	assert.Equal(t, logstore.DefaultLogRetentionDays, persisted.LogRetentionDays,
		"the store read is what startup's log cleaner and the reload path apply; a stored 0 must read as the default")

	require.NoError(t, cfg.ConfigStore.Close(context.Background()))
	h, _ = bootRoundTripGateway(t, dir, nil)
	for _, fromDB := range []bool{false, true} {
		cc := readClientConfig(t, h, fromDB)
		assert.Equal(t, logstore.DefaultLogRetentionDays, retentionOf(t, cc), "from_db=%v", fromDB)
		status, body := saveClientConfig(t, h, cc)
		require.Equal(t, fasthttp.StatusOK, status, "from_db=%v: %s", fromDB, body)
	}
}

// An explicit value is kept: the default fills only a value that was never set.
func TestConfigRoundTrip_ExplicitRetentionIsKept(t *testing.T) {
	SetLogger(&mockLogger{})
	h, _ := bootRoundTripGateway(t, t.TempDir(), map[string]any{"log_retention_days": 30})
	for _, fromDB := range []bool{false, true} {
		cc := readClientConfig(t, h, fromDB)
		assert.Equal(t, 30, retentionOf(t, cc), "from_db=%v", fromDB)
		status, body := saveClientConfig(t, h, cc)
		require.Equal(t, fasthttp.StatusOK, status, "from_db=%v: %s", fromDB, body)
	}
}
