package modelcatalog

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
	"github.com/capsohq/bifrost/framework/modelcatalog/datasheet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Init starts background sync workers that consult the custom sync gate, and an embedder
// may install the gate after Init returns, so setting it must not race their reads
// (run under -race).
func TestSetShouldSyncGateRacesBackgroundSyncs(t *testing.T) {
	mc := &ModelCatalog{logger: bifrost.NewDefaultLogger(schemas.LogLevelError)}
	closed := func(context.Context) bool { return false }
	mc.SetShouldSyncGate(closed)

	ctx := context.Background()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_, _ = mc.runParamsSync(ctx)
			_ = mc.syncMCPLibrary(ctx)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			mc.SetShouldSyncGate(closed)
		}
	}()
	wg.Wait()

	if mc.syncGateAllows(ctx) {
		t.Fatal("a closed gate must refuse syncs")
	}
	mc.SetShouldSyncGate(nil)
	if !mc.syncGateAllows(ctx) {
		t.Fatal("with no gate every sync is allowed")
	}
}

// newSyncTestCatalog builds a catalog whose pricing and model-parameters URLs
// point at local files, with no config store. A lock manager without a store
// grants every lock, so syncTick runs without a database.
func newSyncTestCatalog(t *testing.T, gateOpen bool) (*ModelCatalog, *int) {
	t.Helper()
	dir := t.TempDir()
	pricingPath := filepath.Join(dir, "pricing.json")
	paramsPath := filepath.Join(dir, "params.json")
	require.NoError(t, os.WriteFile(pricingPath, []byte(`{"gpt-4o":{"provider":"openai","mode":"chat","input_cost_per_token":0.0000025,"output_cost_per_token":0.00001}}`), 0o600))
	require.NoError(t, os.WriteFile(paramsPath, []byte(`{"gpt-4o":{"supports_reasoning":false}}`), 0o600))

	logger := bifrost.NewNoOpLogger()
	ds := datasheet.New(nil, logger, datasheet.Config{
		URL:                "file://" + pricingPath,
		ModelParametersURL: "file://" + paramsPath,
		SyncInterval:       time.Hour,
	})
	mc := NewTestCatalogWithDatasheet(ds)
	mc.logger = logger
	mc.distributedLockManager = configstore.NewDistributedLockManager(nil, logger)

	hookCalls := 0
	mc.SetShouldSyncGate(func(context.Context) bool { return gateOpen })
	mc.SetAfterSyncHook(func(context.Context) { hookCalls++ })
	return mc, &hookCalls
}

// In cluster mode the gate is "am I the leader". A pod whose gate rejects the
// sync wrote nothing, so it must not broadcast a reload: with every pod doing
// so, one hourly cycle cost N*(N-1) full catalog reads. It must not mark itself
// synced either, so a pod that becomes leader picks up the overdue sync on its
// next tick instead of waiting a full interval.
func TestSyncTickGateClosedSkipsHookAndMarkSynced(t *testing.T) {
	mc, hookCalls := newSyncTestCatalog(t, false)

	mc.syncTick(context.Background())

	require.Equal(t, 0, *hookCalls, "after-sync hook fired on a pod whose sync was gated off")
	require.True(t, mc.datasheet.LastSyncedAt().IsZero(), "gated-off pod marked itself synced")
}

// The leader path: a sync that ran broadcasts exactly once and marks the pod synced.
func TestSyncTickGateOpenFiresHookOnce(t *testing.T) {
	mc, hookCalls := newSyncTestCatalog(t, true)

	mc.syncTick(context.Background())

	require.Equal(t, 1, *hookCalls)
	require.False(t, mc.datasheet.LastSyncedAt().IsZero(), "leader did not mark itself synced")
}

// Leadership can move after syncTick's pre-lock gate check, so one of the two
// runs may sync while the other is skipped. Peers must still reload what was
// written, but the node must not count the cycle as synced: a half-skipped
// cycle stays overdue and the next tick on the leader retries it.
func TestSyncTickGateFlipMidSyncFiresHookButDoesNotMarkSynced(t *testing.T) {
	mc, hookCalls := newSyncTestCatalog(t, true)
	// Call 1 is the pre-lock check; of the two in-run checks, which run on
	// separate goroutines, exactly one is allowed through.
	var gateCalls atomic.Int32
	mc.SetShouldSyncGate(func(context.Context) bool { return gateCalls.Add(1) <= 2 })

	mc.syncTick(context.Background())

	require.Equal(t, int32(3), gateCalls.Load())
	require.Equal(t, 1, *hookCalls, "peers must reload the half that was written")
	require.True(t, mc.datasheet.LastSyncedAt().IsZero(), "half-skipped cycle marked the node synced")
}

func TestForceReloadPricingGateClosedSkipsHook(t *testing.T) {
	mc, hookCalls := newSyncTestCatalog(t, false)

	require.NoError(t, mc.ForceReloadPricing(context.Background()))

	require.Equal(t, 0, *hookCalls, "after-sync hook fired on a force reload whose sync was gated off")
}

func TestForceReloadPricingGateOpenFiresHookOnce(t *testing.T) {
	mc, hookCalls := newSyncTestCatalog(t, true)

	require.NoError(t, mc.ForceReloadPricing(context.Background()))

	require.Equal(t, 1, *hookCalls)
}

// datasheetFixtureURLs returns file:// URLs for the local datasheet fixtures,
// so no test here reaches the network.
func datasheetFixtureURLs(t *testing.T) (pricingURL, paramsURL string) {
	t.Helper()
	abs := func(rel string) string {
		p, err := filepath.Abs(rel)
		require.NoError(t, err)
		return "file://" + p
	}
	return abs("datasheet/testdata/pricing.json"), abs("datasheet/testdata/model-parameters.json")
}

// newFixtureCatalog builds a catalog without a config store whose datasheet
// loads from the local fixtures.
func newFixtureCatalog(t *testing.T) *ModelCatalog {
	t.Helper()
	pricingURL, paramsURL := datasheetFixtureURLs(t)
	mc := NewTestCatalogWithDatasheet(datasheet.New(nil, bifrost.NewNoOpLogger(), datasheet.Config{
		URL:                pricingURL,
		ModelParametersURL: paramsURL,
	}))
	mc.logger = bifrost.NewNoOpLogger()
	return mc
}

// TestSyncCallbacksConcurrentSetAndRun hammers the setters against the sync
// paths that read them (TXE-4929). An embedder may install the after-sync hook
// while ForceReloadPricing or a background tick is reading it, so under -race
// a plain field fails here. The gate is open so every reload runs a sync from
// the local fixtures and reads the hook: a hook fires only after a sync that
// ran (upstream #8438).
func TestSyncCallbacksConcurrentSetAndRun(t *testing.T) {
	mc := newFixtureCatalog(t)
	allow := func(context.Context) bool { return true }
	mc.SetShouldSyncGate(allow)

	var hookCalls atomic.Int32
	hook := func(context.Context) { hookCalls.Add(1) }

	const workers, iterations = 4, 50
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < iterations; j++ {
				mc.SetShouldSyncGate(allow)
				mc.SetAfterSyncHook(hook)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < iterations; j++ {
				assert.NoError(t, mc.ForceReloadPricing(context.Background()))
			}
		}()
	}
	close(start)
	wg.Wait()
	assert.Positive(t, hookCalls.Load())
}

// TestSyncGateSemantics pins the gate contract on the pricing sync path.
// PR #13's "false gate skips sync but still runs the hook" case is gone:
// upstream #8438 fires the hook only after a sync that ran, which
// TestForceReloadPricingGateClosedSkipsHook pins instead.
func TestSyncGateSemantics(t *testing.T) {
	allow := func(context.Context) bool { return true }
	deny := func(context.Context) bool { return false }

	t.Run("nil gate allows sync", func(t *testing.T) {
		mc := newFixtureCatalog(t)
		require.NoError(t, mc.ForceReloadPricing(context.Background()))
		assert.True(t, mc.hasPricingData())
	})

	t.Run("true gate allows sync", func(t *testing.T) {
		mc := newFixtureCatalog(t)
		mc.SetShouldSyncGate(allow)
		require.NoError(t, mc.ForceReloadPricing(context.Background()))
		assert.True(t, mc.hasPricingData())
	})

	t.Run("replacing and clearing the gate takes effect", func(t *testing.T) {
		mc := newFixtureCatalog(t)
		mc.SetShouldSyncGate(deny)
		require.NoError(t, mc.ForceReloadPricing(context.Background()))
		require.False(t, mc.hasPricingData())

		mc.SetShouldSyncGate(allow)
		require.NoError(t, mc.ForceReloadPricing(context.Background()))
		assert.True(t, mc.hasPricingData())

		mc2 := newFixtureCatalog(t)
		mc2.SetShouldSyncGate(deny)
		mc2.SetShouldSyncGate(nil)
		require.NoError(t, mc2.ForceReloadPricing(context.Background()))
		assert.True(t, mc2.hasPricingData())
	})

	t.Run("callbacks may call the setters", func(t *testing.T) {
		mc := newFixtureCatalog(t)
		var gateCalls, hookCalls atomic.Int32
		mc.SetShouldSyncGate(func(context.Context) bool {
			gateCalls.Add(1)
			mc.SetShouldSyncGate(allow)
			return true
		})
		mc.SetAfterSyncHook(func(context.Context) {
			hookCalls.Add(1)
			mc.SetAfterSyncHook(nil)
		})

		done := make(chan error, 1)
		go func() { done <- mc.ForceReloadPricing(context.Background()) }()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("ForceReloadPricing deadlocked: a callback was invoked while a lock was held")
		}
		// The pricing and params syncs consult the gate concurrently, so the
		// original gate runs once or twice before its replacement is seen. The
		// hook runs once and clears itself.
		assert.Positive(t, gateCalls.Load())
		assert.EqualValues(t, 1, hookCalls.Load())
		assert.True(t, mc.hasPricingData())
		require.NoError(t, mc.ForceReloadPricing(context.Background()))
		assert.EqualValues(t, 1, hookCalls.Load())
	})
}
