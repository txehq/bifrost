package modelcatalog

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderModelSnapshotHealthReportHealthy(t *testing.T) {
	mc := NewTestCatalog(nil)
	provider := schemas.GLM
	modelData := &schemas.BifrostListModelsResponse{
		Data: []schemas.Model{
			{ID: "glm/glm-5"},
			{ID: "glm/glm-4.7"},
			{ID: "glm/glm-5"},
		},
	}

	mc.RecordProviderModelDiscoveryResult(provider, false, modelData, nil)
	mc.RecordProviderModelDiscoveryResult(provider, true, modelData, nil)
	mc.UpsertModelDataForProvider(provider, modelData, nil)
	mc.UpsertUnfilteredModelDataForProvider(provider, modelData)
	mc.mu.Lock()
	mc.providerModelSnapshots[provider] = []string{"glm-5", "glm-4.7"}
	mc.providerModelSources[provider] = ProviderModelSourceLiveDiscovery
	mc.unfilteredProviderModelSources[provider] = ProviderModelSourceLiveDiscovery
	mc.updateProviderModelHealthSnapshotUpdatedAtLocked(provider, time.Now().UTC())
	mc.mu.Unlock()

	report := mc.GetProviderModelSnapshotHealthReport()
	item, ok := getProviderSnapshotHealth(report.Providers, provider)
	require.True(t, ok)

	assert.Equal(t, ProviderModelHealthHealthy, report.Status)
	assert.Equal(t, ProviderModelHealthHealthy, item.Status)
	assert.Equal(t, ProviderModelSourceLiveDiscovery, item.FilteredSource)
	assert.Equal(t, ProviderModelSourceLiveDiscovery, item.UnfilteredSource)
	assert.Equal(t, 2, item.SnapshotModelCount)
	assert.NotNil(t, item.LastSnapshotUpdated)
	assert.Equal(t, 2, item.FilteredDiscovery.LastModelsCount)
	assert.Equal(t, 2, item.UnfilteredDiscovery.LastModelsCount)
}

func TestProviderModelSnapshotHealthReportError(t *testing.T) {
	mc := NewTestCatalog(nil)
	provider := schemas.MiniMax
	successData := &schemas.BifrostListModelsResponse{
		Data: []schemas.Model{
			{ID: "minimax/MiniMax-M2.5"},
		},
	}

	mc.RecordProviderModelDiscoveryResult(provider, false, successData, nil)
	mc.RecordProviderModelDiscoveryResult(provider, true, successData, nil)
	mc.RecordProviderModelDiscoveryResult(
		provider,
		false,
		nil,
		&schemas.BifrostError{Error: &schemas.ErrorField{Message: "provider list models failed"}},
	)

	report := mc.GetProviderModelSnapshotHealthReport()
	item, ok := getProviderSnapshotHealth(report.Providers, provider)
	require.True(t, ok)

	assert.Equal(t, ProviderModelHealthError, report.Status)
	assert.Equal(t, ProviderModelHealthError, item.Status)
	assert.Equal(t, "provider list models failed", item.FilteredDiscovery.LastError)
	assert.Equal(t, ProviderModelHealthHealthy, item.UnfilteredDiscovery.Status)
}

func TestProviderModelSnapshotHealthReportStale(t *testing.T) {
	mc := NewTestCatalog(nil)
	provider := schemas.Moonshot
	successData := &schemas.BifrostListModelsResponse{
		Data: []schemas.Model{
			{ID: "moonshot/kimi-k2.5"},
		},
	}

	mc.RecordProviderModelDiscoveryResult(provider, false, successData, nil)
	mc.RecordProviderModelDiscoveryResult(provider, true, successData, nil)

	mc.mu.Lock()
	state := mc.providerModelHealth[provider]
	staleTime := time.Now().UTC().Add(-2 * DefaultProviderModelSnapshotStaleAfter)
	state.Filtered.LastAttemptAt = staleTime
	state.Filtered.LastSuccessAt = staleTime
	state.Unfiltered.LastAttemptAt = staleTime
	state.Unfiltered.LastSuccessAt = staleTime
	mc.providerModelHealth[provider] = state
	mc.mu.Unlock()

	report := mc.GetProviderModelSnapshotHealthReport()
	item, ok := getProviderSnapshotHealth(report.Providers, provider)
	require.True(t, ok)

	assert.Equal(t, ProviderModelHealthDegraded, report.Status)
	assert.Equal(t, ProviderModelHealthStale, item.Status)
}

func TestGetPersistedProviderModelHealthState_IncludesSourceOnlyEntries(t *testing.T) {
	mc := NewTestCatalog(nil)
	provider := schemas.GLM

	mc.providerModelSources[provider] = ProviderModelSourceDefaultSeed
	mc.unfilteredProviderModelSources[provider] = ProviderModelSourcePersistedSnapshot

	persistedState := mc.getPersistedProviderModelHealthState()
	entry, ok := persistedState[string(provider)]
	require.True(t, ok)
	assert.Equal(t, ProviderModelSourceDefaultSeed, entry.FilteredSource)
	assert.Equal(t, ProviderModelSourcePersistedSnapshot, entry.UnfilteredSource)
}

func TestGetPersistedProviderModelHealthState_IncludesDiscoveryState(t *testing.T) {
	mc := NewTestCatalog(nil)
	provider := schemas.MiniMax
	modelData := &schemas.BifrostListModelsResponse{
		Data: []schemas.Model{
			{ID: "minimax/MiniMax-M2.5"},
		},
	}

	mc.RecordProviderModelDiscoveryResult(provider, false, modelData, nil)

	persistedState := mc.getPersistedProviderModelHealthState()
	entry, ok := persistedState[string(provider)]
	require.True(t, ok)
	assert.NotZero(t, entry.Filtered.LastAttemptAt)
	assert.NotZero(t, entry.Filtered.LastSuccessAt)
	assert.Equal(t, 1, entry.Filtered.LastModelsCount)
}

func TestProviderModelHealthPersistenceDebounced(t *testing.T) {
	mc := NewTestCatalog(nil)
	mc.done = make(chan struct{})
	mc.providerModelHealthPersistDebounce = 25 * time.Millisecond

	var persistCount atomic.Int32
	mc.providerModelHealthPersistCallback = func() {
		persistCount.Add(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	mc.startProviderModelHealthPersistWorker(ctx)

	provider := schemas.GLM
	modelData := &schemas.BifrostListModelsResponse{
		Data: []schemas.Model{
			{ID: "glm/glm-5"},
		},
	}

	for i := 0; i < 5; i++ {
		mc.RecordProviderModelDiscoveryResult(provider, false, modelData, nil)
	}

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(1), persistCount.Load())

	cancel()
	close(mc.done)
	mc.wg.Wait()
}

func getProviderSnapshotHealth(
	items []ProviderModelSnapshotHealth,
	provider schemas.ModelProvider,
) (ProviderModelSnapshotHealth, bool) {
	for _, item := range items {
		if item.Provider == provider {
			return item, true
		}
	}
	return ProviderModelSnapshotHealth{}, false
}
