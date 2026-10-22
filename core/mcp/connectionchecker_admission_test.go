package mcp

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReconnectAfterFailedCheck_SuccessorInstalledBeforeAdmission covers the gap
// between the checker's generation check and its reconnect taking the exclusive
// operation slot. A reconnect that finishes inside that gap installs a healthy
// successor; the checker's reconnect, admitted afterwards, must recheck the
// generation it was owed for and leave the successor alone.
func TestReconnectAfterFailedCheck_SuccessorInstalledBeforeAdmission(t *testing.T) {
	ts, _ := buildExpiringSessionMCPServer(t)
	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
	config := &schemas.MCPClientConfig{
		ID:                     "client-reconnect-admission",
		Name:                   "admission-client",
		AuthType:               schemas.MCPAuthTypeHeaders,
		ConnectionType:         schemas.MCPConnectionTypeHTTP,
		ConnectionString:       schemas.NewSecretVar(ts.URL),
		NeedsSessionStickiness: schemas.Ptr(true),
		ToolsToExecute:         []string{"*"},
	}
	require.NoError(t, m.connectToMCPClient(context.Background(), config))
	before, ok := snapshotClientState(m, config.ID)
	require.True(t, ok)
	failedGeneration := before.ConnGeneration

	checker := NewClientConnectionChecker(m, config.ID, time.Minute, false, &MockLogger{})
	// One P: the checker's reconnect goroutine cannot run until this one blocks,
	// so the successor below deterministically lands inside the gap.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	checker.reconnectAfterFailedCheck(config.Name, failedGeneration)
	m.mu.Lock()
	m.clientMap[config.ID].ConnGeneration = failedGeneration + 1 // a concurrent reconnect installed a successor
	m.mu.Unlock()

	require.Eventually(t, func() bool {
		_, admitted := m.reconnectingClients.Load(config.ID)
		return admitted
	}, 5*time.Second, 5*time.Millisecond, "the checker's reconnect must still reach the exclusive slot")
	finished, _ := m.AwaitReconnect(config.ID, 10*time.Second)
	require.True(t, finished)

	after, ok := snapshotClientState(m, config.ID)
	require.True(t, ok)
	assert.Equal(t, failedGeneration+1, after.ConnGeneration, "the successor installed after the generation check must not be replaced")
	assert.True(t, before.Conn == after.Conn, "the successor's connection must stay installed")
}
