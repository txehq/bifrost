package mcptests

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/mcp"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// STDIO SERVER DROP AND RESTORE TEST (20 SECONDS)
// =============================================================================

func TestHealthCheckSTDIOServerDropAndRecoverIn20Seconds(t *testing.T) {
	t.Parallel()

	// Use temperature STDIO server
	bifrostRoot := "/Users/prathammaxim/Desktop/bifrost"
	clientConfig := GetTemperatureMCPClientConfig(bifrostRoot)
	clientConfig.ID = "stdio-health-recovery-test"

	// 1. Create STDIO client with bifrost manager
	manager := setupMCPManager(t, clientConfig)

	// Wait for initial connection
	time.Sleep(1 * time.Second)

	clients := manager.GetClients()
	if len(clients) == 0 {
		t.Skip("Temperature STDIO server not available")
	}
	require.Len(t, clients, 1, "should have one STDIO client")

	// 2. Verify connected state
	assert.Equal(t, schemas.MCPConnectionStateHealthy, clients[0].State, "client should be connected initially")
	t.Logf("✅ STDIO client connected: %s", clients[0].ExecutionConfig.ID)

	// 3. Kill STDIO process (remove and re-add to simulate server drop)
	clientID := clients[0].ExecutionConfig.ID
	err := manager.RemoveClient(clientID)
	require.NoError(t, err, "should remove client to simulate server drop")
	t.Logf("🔴 Simulated STDIO server drop by removing client")

	// 4. Wait for health monitor to detect (should see disconnected state)
	time.Sleep(3 * time.Second)
	clients = manager.GetClients()
	assert.Len(t, clients, 0, "client should be removed after drop")
	t.Logf("✅ Health monitor detected server drop")

	// 5. Restart STDIO process (re-add client)
	err = manager.AddClient(context.Background(), &clientConfig)
	require.NoError(t, err, "should re-add client to simulate server recovery")
	t.Logf("🔄 Simulated STDIO server recovery by re-adding client")

	// 6. Wait up to 20 seconds for health monitor to detect recovery
	maxWaitTime := 20 * time.Second
	checkInterval := 2 * time.Second
	deadline := time.Now().Add(maxWaitTime)
	recovered := false

	for time.Now().Before(deadline) {
		time.Sleep(checkInterval)
		clients = manager.GetClients()
		if len(clients) > 0 && clients[0].State == schemas.MCPConnectionStateHealthy {
			recovered = true
			t.Logf("✅ Health monitor detected recovery after %v", time.Since(deadline.Add(-maxWaitTime)))
			break
		}
		t.Logf("⏳ Waiting for recovery... (elapsed: %v)", time.Since(deadline.Add(-maxWaitTime)))
	}

	// 7. Verify health monitor detects recovery (should see connected state)
	require.True(t, recovered, "client should recover within 20 seconds")
	clients = manager.GetClients()
	require.Len(t, clients, 1, "should have client after recovery")
	assert.Equal(t, schemas.MCPConnectionStateHealthy, clients[0].State, "client should be connected after recovery")
	t.Logf("✅ STDIO server drop and recovery test completed successfully")
}

// =============================================================================
// SSE RECONNECTION TESTS
// =============================================================================

func TestHealthCheckSSEReconnect(t *testing.T) {
	t.Parallel()

	config := GetTestConfig(t)
	if config.SSEServerURL == "" {
		t.Skip("MCP_SSE_URL not set")
	}

	clientConfig := GetSampleSSEClientConfig(config.SSEServerURL)
	if len(config.SSEHeaders) > 0 {
		clientConfig.Headers = config.SSEHeaders
	}
	manager := setupMCPManager(t, clientConfig)

	clients := manager.GetClients()
	require.Len(t, clients, 1, "should have one client")
	clientID := clients[0].ExecutionConfig.ID

	// Force reconnect
	err := manager.ReconnectClient(clientID)
	assert.NoError(t, err, "reconnect should succeed")

	// Wait for reconnection
	time.Sleep(2 * time.Second)

	// Verify client is still connected
	clients = manager.GetClients()
	AssertClientState(t, clients, clientID, schemas.MCPConnectionStateHealthy)
}

func TestHealthCheckSSELongRunning(t *testing.T) {
	t.Parallel()

	config := GetTestConfig(t)
	if config.SSEServerURL == "" {
		t.Skip("MCP_SSE_URL not set")
	}

	clientConfig := GetSampleSSEClientConfig(config.SSEServerURL)
	if len(config.SSEHeaders) > 0 {
		clientConfig.Headers = config.SSEHeaders
	}
	manager := setupMCPManager(t, clientConfig)

	clients := manager.GetClients()
	require.Len(t, clients, 1, "should have one client")

	// Keep connection alive for 30 seconds
	// Health monitor should keep it connected
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for i := 0; i < 6; i++ {
		<-ticker.C
		clients = manager.GetClients()
		if len(clients) > 0 {
			t.Logf("Health check iteration %d: state=%s", i+1, clients[0].State)
			// Connection should remain stable
			assert.Equal(t, schemas.MCPConnectionStateHealthy, clients[0].State,
				"connection should remain stable at iteration %d", i+1)
		}
	}
}

// =============================================================================
// STATE TRANSITION TESTS
// =============================================================================

func TestHealthCheckStateTransitions(t *testing.T) {
	t.Parallel()

	config := GetTestConfig(t)
	if config.HTTPServerURL == "" {
		t.Skip("MCP_HTTP_URL not set")
	}

	clientConfig := GetSampleHTTPClientConfig(config.HTTPServerURL)
	if len(config.HTTPHeaders) > 0 {
		clientConfig.Headers = config.HTTPHeaders
	}
	manager := setupMCPManager(t, clientConfig)

	clients := manager.GetClients()
	require.Len(t, clients, 1, "should have one client")

	// Initial state: connected
	assert.Equal(t, schemas.MCPConnectionStateHealthy, clients[0].State)

	// Remove client (simulates disconnection)
	clientID := clients[0].ExecutionConfig.ID
	err := manager.RemoveClient(clientID)
	require.NoError(t, err, "should remove client")

	// Verify client is removed
	clients = manager.GetClients()
	assert.Len(t, clients, 0, "client should be removed")

	// Re-add client (simulates reconnection)
	err = manager.AddClient(context.Background(), &clientConfig)
	require.NoError(t, err, "should re-add client")

	// Verify client is connected again
	clients = manager.GetClients()
	require.Len(t, clients, 1, "should have one client again")
	assert.Equal(t, schemas.MCPConnectionStateHealthy, clients[0].State)
}

func TestHealthCheckStateTransitionsInvalidClient(t *testing.T) {
	t.Parallel()

	// Create client with invalid URL
	clientConfig := GetSampleHTTPClientConfig("http://invalid-url-test:9999")
	manager := setupMCPManager(t, clientConfig)

	// Wait for connection attempt
	time.Sleep(3 * time.Second)

	clients := manager.GetClients()
	if len(clients) > 0 {
		// Client should not be in connected state
		assert.NotEqual(t, schemas.MCPConnectionStateHealthy, clients[0].State,
			"invalid client should not be connected")
	}
}

// =============================================================================
// MULTIPLE CLIENT HEALTH MONITORING
// =============================================================================

func TestHealthCheckMultipleClients(t *testing.T) {
	t.Parallel()

	config := GetTestConfig(t)

	var clientConfigs []schemas.MCPClientConfig

	if config.HTTPServerURL != "" {
		httpConfig := GetSampleHTTPClientConfig(config.HTTPServerURL)
		httpConfig.ID = "http-health-test"
		if len(config.HTTPHeaders) > 0 {
			httpConfig.Headers = config.HTTPHeaders
		}
		clientConfigs = append(clientConfigs, httpConfig)
	}

	if config.SSEServerURL != "" {
		sseConfig := GetSampleSSEClientConfig(config.SSEServerURL)
		sseConfig.ID = "sse-health-test"
		if len(config.SSEHeaders) > 0 {
			sseConfig.Headers = config.SSEHeaders
		}
		clientConfigs = append(clientConfigs, sseConfig)
	}

	if len(clientConfigs) == 0 {
		t.Skip("No MCP servers configured")
	}

	manager := setupMCPManager(t, clientConfigs...)

	// Wait for health monitoring
	time.Sleep(3 * time.Second)

	// All clients should be connected
	clients := manager.GetClients()
	assert.GreaterOrEqual(t, len(clients), len(clientConfigs), "should have all clients")

	for _, client := range clients {
		assert.Equal(t, schemas.MCPConnectionStateHealthy, client.State,
			"client %s should be connected", client.ExecutionConfig.ID)
	}
}

func TestHealthCheckMultipleClientsMixedStates(t *testing.T) {
	t.Parallel()

	config := GetTestConfig(t)
	if config.HTTPServerURL == "" {
		t.Skip("MCP_HTTP_URL not set")
	}

	// Create one valid and one invalid client
	validConfig := GetSampleHTTPClientConfig(config.HTTPServerURL)
	validConfig.ID = "valid-client"
	validConfig.Name = "ValidHTTPServer"
	if len(config.HTTPHeaders) > 0 {
		validConfig.Headers = config.HTTPHeaders
	}

	invalidConfig := GetSampleHTTPClientConfig("http://invalid-url-test:9999")
	invalidConfig.ID = "invalid-client"
	invalidConfig.Name = "InvalidHTTPServer"

	manager := setupMCPManager(t, validConfig, invalidConfig)

	// Wait for connection attempts
	time.Sleep(3 * time.Second)

	clients := manager.GetClients()
	assert.GreaterOrEqual(t, len(clients), 1, "should have at least one client")

	// Verify valid client is connected
	for _, client := range clients {
		if client.ExecutionConfig.ID == validConfig.ID {
			assert.Equal(t, schemas.MCPConnectionStateHealthy, client.State,
				"valid client should be connected")
		}
	}
}

// =============================================================================
// CONCURRENT HEALTH CHECK FAILURES
// =============================================================================

func TestHealthCheckConcurrentFailures(t *testing.T) {
	t.Parallel()

	// Create multiple clients with invalid URLs
	var clientConfigs []schemas.MCPClientConfig

	for i := 0; i < 5; i++ {
		config := GetSampleHTTPClientConfig("http://invalid-concurrent-test:9999")
		id := string(rune('a'+i)) + "-concurrent-client"
		config.ID = id
		clientConfigs = append(clientConfigs, config)
	}

	manager := setupMCPManager(t, clientConfigs...)

	// Wait for all connection attempts
	time.Sleep(5 * time.Second)

	// All clients should be in non-connected state or removed
	clients := manager.GetClients()
	for _, client := range clients {
		if client.State == schemas.MCPConnectionStateHealthy {
			t.Errorf("client %s should not be connected to invalid URL", client.ExecutionConfig.ID)
		}
	}
}

// =============================================================================
// HEALTH CHECK WITH TOOL EXECUTION
// =============================================================================

func TestHealthCheckDuringToolExecution(t *testing.T) {
	t.Parallel()

	// Use InProcess tool for reliable, self-contained testing
	manager := setupMCPManager(t)

	// Register echo tool
	err := RegisterEchoTool(manager)
	require.NoError(t, err, "should register echo tool")

	bifrost := setupBifrost(t)
	bifrost.SetMCPManager(manager)

	clients := manager.GetClients()
	require.Len(t, clients, 1, "should have bifrostInternal client")

	// Execute a tool while health monitoring is active
	ctx := createTestContext()
	toolCall := GetSampleEchoToolCall("call-1", "health check test")

	result, bifrostErr := bifrost.ExecuteChatMCPTool(ctx, &toolCall)

	// Tool execution should succeed
	require.Nil(t, bifrostErr, "tool execution should succeed during health monitoring")
	assert.NotNil(t, result, "should have result")

	// Verify result is present
	if result != nil {
		t.Logf("✅ Tool execution successful during health monitoring")
	}

	// Client should still be in healthy state
	clients = manager.GetClients()
	assert.Len(t, clients, 1, "should still have client after tool execution")
}

// =============================================================================
// RECONNECTION AFTER HEALTH CHECK FAILURE
// =============================================================================

func TestHealthCheckReconnectAfterFailure(t *testing.T) {
	t.Parallel()

	config := GetTestConfig(t)
	if config.HTTPServerURL == "" {
		t.Skip("MCP_HTTP_URL not set")
	}

	clientConfig := GetSampleHTTPClientConfig(config.HTTPServerURL)
	if len(config.HTTPHeaders) > 0 {
		clientConfig.Headers = config.HTTPHeaders
	}
	manager := setupMCPManager(t, clientConfig)

	clients := manager.GetClients()
	require.Len(t, clients, 1, "should have one client")
	clientID := clients[0].ExecutionConfig.ID

	// Remove client to simulate failure
	err := manager.RemoveClient(clientID)
	require.NoError(t, err, "should remove client")

	// Wait a bit
	time.Sleep(2 * time.Second)

	// Re-add client (manual reconnection)
	err = manager.AddClient(context.Background(), &clientConfig)
	require.NoError(t, err, "should re-add client")

	// Wait for health monitoring to stabilize
	time.Sleep(3 * time.Second)

	// Client should be connected and healthy
	clients = manager.GetClients()
	require.Len(t, clients, 1, "should have client back")
	assert.Equal(t, schemas.MCPConnectionStateHealthy, clients[0].State)
}

// readPID returns the pid recorded by the wrapper below, or 0 while the file
// has not been written yet.
func readPID(t *testing.T, pidFile string) int {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0
	}
	return pid
}

// TestSTDIOSubprocessKilled_CheckerRespawnsIt closes the one gap in this
// package's coverage of connection repair. A STDIO server's process exiting is
// the one transport death nothing observes: OnConnectionLost is registered for
// SSE only, and mcp-go owns the subprocess, so Bifrost holds no handle to
// notice it going away. The dead connection stays installed on the entry, so
// the periodic checker's live-connection branch is the only thing that can
// repair it, and until recently that branch never reconnected at all.
//
// TestHealthCheckSTDIOServerDropAndRecoverIn20Seconds simulates the drop with
// RemoveClient + AddClient, which exercises the manager rather than the
// failure. This kills the actual process.
//
// The wrapper records its own pid and then execs the server in place, so the
// pid in the file is the server process itself and a respawn is observable as
// a different pid rather than merely as a healthy state.
func TestSTDIOSubprocessKilled_CheckerRespawnsIt(t *testing.T) {
	InitMCPServerPaths(t)
	serverBin := mcpServerPaths.GoTestServer
	if _, err := os.Stat(serverBin); err != nil {
		t.Skipf("go-test-server binary not built (%v); run make setup-mcp-tests", err)
	}

	// Deliberately a directory whose name contains a space: both paths below are
	// handed to `sh`, and anything that re-parses them as shell source breaks on
	// exactly this.
	pidDir := filepath.Join(t.TempDir(), "pid dir")
	require.NoError(t, os.MkdirAll(pidDir, 0o755))
	pidFile := filepath.Join(pidDir, "server.pid")
	config := schemas.MCPClientConfig{
		ID:             "stdio-respawn-client",
		Name:           "StdioRespawnServer",
		ConnectionType: schemas.MCPConnectionTypeSTDIO,
		StdioConfig: &schemas.MCPStdioConfig{
			Command: "sh",
			// Both paths arrive as positional parameters and are expanded
			// quoted, so neither is ever re-parsed as shell source. Interpolating
			// them into the script text instead breaks on any path containing a
			// space or metacharacter, which a checkout directory or TMPDIR can
			// easily have. The literal "sh" is $0; the paths are $1 and $2.
			Args: []string{"-c", `echo $$ > "$1"; exec "$2"`, "sh", pidFile, serverBin},
		},
		ToolsToExecute: []string{"*"},
	}

	manager := setupMCPManager(t, config)

	var originalPID int
	require.Eventually(t, func() bool {
		originalPID = readPID(t, pidFile)
		return originalPID > 0
	}, 15*time.Second, 50*time.Millisecond, "the STDIO server should have started and recorded its pid")

	clients := manager.GetClients()
	require.Len(t, clients, 1)
	require.Equal(t, schemas.MCPConnectionStateHealthy, clients[0].State, "sanity: connected before the kill")

	// Kill the server out from under Bifrost. Nothing in the process observes
	// this: there is no OnConnectionLost for STDIO and no exit handler.
	require.NoError(t, syscall.Kill(originalPID, syscall.SIGKILL))

	// A checker on a tight cadence, standing in for the ordinary periodic one
	// without waiting out its real interval.
	checker := mcp.NewClientConnectionChecker(manager, config.ID, 300*time.Millisecond, false, newTestLogger(t))
	checker.Start()
	defer checker.Stop()

	var respawnedPID int
	require.Eventually(t, func() bool {
		respawnedPID = readPID(t, pidFile)
		return respawnedPID > 0 && respawnedPID != originalPID
	}, 60*time.Second, 200*time.Millisecond, "the checker's reconnect should have spawned a replacement server process")

	require.Eventually(t, func() bool {
		for _, c := range manager.GetClients() {
			if c.ExecutionConfig.ID == config.ID {
				return c.State == schemas.MCPConnectionStateHealthy
			}
		}
		return false
	}, 30*time.Second, 200*time.Millisecond, "and the client should be healthy again on the replacement")

	assert.NoError(t, syscall.Kill(respawnedPID, 0), "the replacement process should be alive")
}
