package modelcatalog

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
	"github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/capsohq/bifrost/framework/modelcatalog/datasheet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

// signalLogger discards everything and closes a channel the first time a
// message containing each watched substring is logged.
type signalLogger struct {
	schemas.Logger
	mu      sync.Mutex
	watched map[string]chan struct{}
}

func newSignalLogger(substrings ...string) *signalLogger {
	l := &signalLogger{Logger: bifrost.NewNoOpLogger(), watched: make(map[string]chan struct{})}
	for _, s := range substrings {
		l.watched[s] = make(chan struct{})
	}
	return l
}

func (l *signalLogger) record(msg string, args ...any) {
	line := fmt.Sprintf(msg, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	for s, ch := range l.watched {
		if strings.Contains(line, s) {
			close(ch)
			delete(l.watched, s)
		}
	}
}

func (l *signalLogger) Info(msg string, args ...any)  { l.record(msg, args...) }
func (l *signalLogger) Warn(msg string, args ...any)  { l.record(msg, args...) }
func (l *signalLogger) Error(msg string, args ...any) { l.record(msg, args...) }

// heldStartupLockStore holds the startup-sync distributed locks until release
// is closed, so the background startup syncs reach the gate only after Init
// has returned, the window the CI failure hit by chance.
type heldStartupLockStore struct {
	configstore.ConfigStore
	release chan struct{}
}

func (s *heldStartupLockStore) TryAcquireLock(ctx context.Context, lock *tables.TableDistributedLock) (bool, error) {
	if strings.HasSuffix(lock.LockKey, "_startup_sync") {
		select {
		case <-s.release:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return s.ConfigStore.TryAcquireLock(ctx, lock)
}

// TestSetShouldSyncGateDuringInitBackgroundSync reproduces TXE-4929: when the
// DB already holds pricing, Init returns while a startup sync is still running
// in the background, and that sync reads the gate the caller installs right
// after Init. Under -race the unsynchronised gate field failed this test on
// every run.
//
// Without the held lock the startup syncs usually read the gate before Init's
// last database call, which orders the read before the caller's write and
// hides the race; that is why the CI failure was intermittent.
func TestSetShouldSyncGateDuringInitBackgroundSync(t *testing.T) {
	ctx := context.Background()
	store, err := configstore.NewConfigStore(ctx, &configstore.Config{
		Enabled: true,
		Type:    configstore.ConfigStoreTypeSQLite,
		Config:  &configstore.SQLiteConfig{Path: filepath.Join(t.TempDir(), "config.db")},
	}, bifrost.NewNoOpLogger())
	require.NoError(t, err)
	t.Cleanup(func() { store.Close(context.Background()) })

	pricingURL, paramsURL := datasheetFixtureURLs(t)
	mcpDisabled := MCPLibrarySyncDisabled
	cfg := &Config{PricingURL: &pricingURL, ModelParametersURL: &paramsURL, MCPLibrarySyncInterval: &mcpDisabled}

	// First boot: empty DB, so Init syncs the fixtures in the foreground.
	seed, err := Init(ctx, cfg, store, bifrost.NewNoOpLogger())
	require.NoError(t, err)
	require.NoError(t, seed.Cleanup())

	// Second boot: the DB has data, so both startup syncs run in the background.
	const pricingDone = "background startup pricing sync"
	const paramsDone = "background startup model parameters sync"
	logger := newSignalLogger(pricingDone, paramsDone)
	finished := []chan struct{}{logger.watched[pricingDone], logger.watched[paramsDone]}
	held := &heldStartupLockStore{ConfigStore: store, release: make(chan struct{})}
	mc, err := Init(ctx, cfg, held, logger)
	require.NoError(t, err)
	defer mc.Cleanup()

	// Release the locks before installing the gate, not after: a release after
	// the write would order the write before the syncs' reads.
	close(held.release)
	var gateCalls atomic.Int32
	mc.SetShouldSyncGate(func(context.Context) bool {
		gateCalls.Add(1)
		return false
	})

	// Wait until both background syncs have consulted the gate. No sleeps:
	// the logger signals once each sync has finished.
	for _, ch := range finished {
		select {
		case <-ch:
		case <-time.After(30 * time.Second):
			t.Fatal("background startup sync did not finish")
		}
	}
	t.Logf("installed gate consulted %d time(s) by the background startup syncs", gateCalls.Load())
}

// TestSyncCallbacksConcurrentSetAndRun hammers the setters against the sync
// paths that read them. Every gate returns false, so no sync does I/O.
func TestSyncCallbacksConcurrentSetAndRun(t *testing.T) {
	mc := NewTestCatalog(nil)
	mc.logger = bifrost.NewNoOpLogger()
	deny := func(context.Context) bool { return false }
	mc.SetShouldSyncGate(deny)

	var hookCalls atomic.Int32
	hook := func(context.Context) { hookCalls.Add(1) }

	const workers, iterations = 4, 200
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < iterations; j++ {
				mc.SetShouldSyncGate(deny)
				mc.SetAfterSyncHook(hook)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < iterations; j++ {
				assert.NoError(t, mc.ForceReloadPricing(context.Background()))
				_, err := mc.ForceReloadMCPLibrary(context.Background())
				assert.NoError(t, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	assert.Positive(t, hookCalls.Load())
}

// TestSyncGateSemantics pins the gate contract on the pricing sync path, using
// a catalog without a config store whose datasheet loads from local fixtures.
func TestSyncGateSemantics(t *testing.T) {
	pricingURL, paramsURL := datasheetFixtureURLs(t)
	newCatalog := func() *ModelCatalog {
		mc := NewTestCatalogWithDatasheet(datasheet.New(nil, bifrost.NewNoOpLogger(), datasheet.Config{
			URL:                pricingURL,
			ModelParametersURL: paramsURL,
		}))
		mc.logger = bifrost.NewNoOpLogger()
		return mc
	}
	allow := func(context.Context) bool { return true }
	deny := func(context.Context) bool { return false }

	t.Run("nil gate allows sync", func(t *testing.T) {
		mc := newCatalog()
		require.NoError(t, mc.ForceReloadPricing(context.Background()))
		assert.True(t, mc.hasPricingData())
	})

	t.Run("true gate allows sync", func(t *testing.T) {
		mc := newCatalog()
		mc.SetShouldSyncGate(allow)
		require.NoError(t, mc.ForceReloadPricing(context.Background()))
		assert.True(t, mc.hasPricingData())
	})

	t.Run("false gate skips sync but still runs the hook", func(t *testing.T) {
		mc := newCatalog()
		mc.SetShouldSyncGate(deny)
		var hookCalls atomic.Int32
		mc.SetAfterSyncHook(func(context.Context) { hookCalls.Add(1) })
		require.NoError(t, mc.ForceReloadPricing(context.Background()))
		assert.False(t, mc.hasPricingData())
		assert.EqualValues(t, 1, hookCalls.Load())
	})

	t.Run("replacing and clearing the gate takes effect", func(t *testing.T) {
		mc := newCatalog()
		mc.SetShouldSyncGate(deny)
		require.NoError(t, mc.ForceReloadPricing(context.Background()))
		require.False(t, mc.hasPricingData())

		mc.SetShouldSyncGate(allow)
		require.NoError(t, mc.ForceReloadPricing(context.Background()))
		assert.True(t, mc.hasPricingData())

		mc2 := newCatalog()
		mc2.SetShouldSyncGate(deny)
		mc2.SetShouldSyncGate(nil)
		require.NoError(t, mc2.ForceReloadPricing(context.Background()))
		assert.True(t, mc2.hasPricingData())
	})

	t.Run("callbacks may call the setters", func(t *testing.T) {
		mc := newCatalog()
		var gateCalls, hookCalls atomic.Int32
		mc.SetShouldSyncGate(func(context.Context) bool {
			gateCalls.Add(1)
			mc.SetShouldSyncGate(deny)
			return false
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
			t.Fatal("ForceReloadPricing deadlocked: a callback was invoked while syncMu was held")
		}
		// The pricing and params syncs consult the gate concurrently, so the
		// original gate runs once or twice before its replacement is seen. The
		// hook runs once and clears itself.
		assert.Positive(t, gateCalls.Load())
		assert.EqualValues(t, 1, hookCalls.Load())
		require.NoError(t, mc.ForceReloadPricing(context.Background()))
		assert.EqualValues(t, 1, hookCalls.Load())
		assert.False(t, mc.hasPricingData())
	})
}
