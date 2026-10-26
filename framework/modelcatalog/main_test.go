package modelcatalog

import (
	"context"
	"sync"
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
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
			_ = mc.runParamsSync(ctx)
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
