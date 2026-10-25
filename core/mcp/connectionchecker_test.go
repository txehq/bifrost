package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// ClientConnectionChecker.performCheck — the per-call (conn==nil) branches.
// Uses fakeAdminCredStore / buildAdminDiscoveryHTTPServer from
// admin_tool_discovery_test.go (same package).
// =============================================================================

// newSyncTestClientState builds a minimal MCPClientState + config pair for
// the performCheck tests below, mirroring newAuthRetryClientState's shape
// (auth_retry_test.go) but with a settable AuthType and Conn.
func newSyncTestClientState(clientID, clientName string, authType schemas.MCPAuthType, tools map[string]schemas.ChatTool) (*schemas.MCPClientState, *schemas.MCPClientConfig) {
	config := &schemas.MCPClientConfig{
		ID:       clientID,
		Name:     clientName,
		AuthType: authType,
	}
	state := &schemas.MCPClientState{
		Name:            clientName,
		ExecutionConfig: config,
		Conn:            nil, // every test here exercises the conn==nil branch
		ToolMap:         tools,
		ToolNameMapping: map[string]string{},
	}
	return state, config
}

// TestPerformCheck_NilConn_PerUserOAuth_AttemptsAdminDiscovery confirms a
// per_user_oauth client with no live conn attempts admin tool discovery
// (observed here via the credStore.AdminConnectionHeaders call count, since
// RequiresPerCallConnection auth types never hold a conn to reuse in the
// first place).
func TestPerformCheck_NilConn_PerUserOAuth_AttemptsAdminDiscovery(t *testing.T) {
	cred := &fakeAdminCredStore{err: errors.New("admin credential unavailable")}
	manager := &MCPManager{
		credStore: cred,
		logger:    &MockLogger{},
		clientMap: map[string]*schemas.MCPClientState{},
	}

	existingTools := map[string]schemas.ChatTool{"oauth-client-existing": {}}
	state, config := newSyncTestClientState("client-1", "oauth-client", schemas.MCPAuthTypePerUserOauth, existingTools)
	manager.clientMap[config.ID] = state

	checker := NewClientConnectionChecker(manager, config.ID, time.Minute, false, &MockLogger{})
	checker.performCheck()

	// performCheck wraps admin discovery in ExecuteWithRetry(ProbeRetryConfig)
	// same as every other check — this unclassified error doesn't match
	// isTransientError's permanent list, so it retries (MaxRetries=3, 4 total
	// attempts) before finally giving up and marking Unstable.
	require.Equal(t, ProbeRetryConfig.MaxRetries+1, cred.callCount(), "performCheck should attempt admin tool discovery (calling credStore.AdminConnectionHeaders), retried like every other check, instead of silently skipping")
	// Admin discovery failed, so existing tools must be kept intact (same
	// "keep existing tools on error" contract as the live-conn path).
	require.Equal(t, existingTools, manager.clientMap[config.ID].ToolMap)
	require.Equal(t, schemas.MCPConnectionStateUnstable, manager.clientMap[config.ID].State)
}

// TestPerformCheck_NilConn_PerUserHeaders_AttemptsAdminDiscovery is the
// per_user_headers counterpart of the test above.
func TestPerformCheck_NilConn_PerUserHeaders_AttemptsAdminDiscovery(t *testing.T) {
	cred := &fakeAdminCredStore{err: errors.New("admin credential unavailable")}
	manager := &MCPManager{
		credStore: cred,
		logger:    &MockLogger{},
		clientMap: map[string]*schemas.MCPClientState{},
	}

	existingTools := map[string]schemas.ChatTool{"headers-client-existing": {}}
	state, config := newSyncTestClientState("client-2", "headers-client", schemas.MCPAuthTypePerUserHeaders, existingTools)
	manager.clientMap[config.ID] = state

	checker := NewClientConnectionChecker(manager, config.ID, time.Minute, false, &MockLogger{})
	checker.performCheck()

	require.Equal(t, ProbeRetryConfig.MaxRetries+1, cred.callCount(), "performCheck should attempt admin tool discovery instead of silently skipping, retried like every other check")
	require.Equal(t, existingTools, manager.clientMap[config.ID].ToolMap)
}

// TestPerformCheck_NilConn_SharedAuthType_Sticky_NeverAttemptsAdminDiscovery
// is the regression-risk case for a genuinely sticky shared client (e.g.
// "headers" or "none") with Conn==nil because it's mid-(re)connect, not
// per-call. performCheck must never route it into admin discovery — this
// branch instead attempts a direct reconnect, covered separately by
// TestPerformCheck_NilConn_SharedAuthType_TriggersReconnect below. Uses the
// real credstore.CredStore (not a hardcoded-per-call fake) so the routing
// decision reflects actual NeedsSessionStickiness semantics: a shared type
// with needs_session_stickiness=false is legitimately per-call and DOES
// attempt admin discovery now — see
// TestPerformCheck_NilConn_SharedAuthType_PerCall_SuccessUpdatesToolMap.
func TestPerformCheck_NilConn_SharedAuthType_Sticky_NeverAttemptsAdminDiscovery(t *testing.T) {
	for _, authType := range []schemas.MCPAuthType{schemas.MCPAuthTypeHeaders, schemas.MCPAuthTypeNone, schemas.MCPAuthTypeOauth} {
		t.Run(string(authType), func(t *testing.T) {
			manager := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)

			existingTools := map[string]schemas.ChatTool{"shared-client-existing": {}}
			state, config := newSyncTestClientState("client-3", "shared-client", authType, existingTools)
			config.ConnectionType = schemas.MCPConnectionTypeHTTP
			config.NeedsSessionStickiness = schemas.Ptr(true)
			config.ConnectionString = schemas.NewSecretVar("http://127.0.0.1:0/mcp") // unreachable — the reconnect attempt fails fast, which is fine
			manager.mu.Lock()
			manager.clientMap[config.ID] = state
			manager.mu.Unlock()

			checker := NewClientConnectionChecker(manager, config.ID, time.Minute, false, &MockLogger{})
			checker.performCheck()

			require.Eventually(t, func() bool {
				_, inFlightOrDone := manager.reconnectingClients.Load(config.ID)
				return inFlightOrDone
			}, 2*time.Second, 10*time.Millisecond, "a sticky client with no live connection must trigger a direct reconnect attempt, not admin discovery")

			manager.mu.RLock()
			toolMap := manager.clientMap[config.ID].ToolMap
			manager.mu.RUnlock()
			require.Equal(t, existingTools, toolMap, "must leave the existing tool map untouched — only a real reconnect's own tools/list replaces it")
		})
	}
}

// TestPerformCheck_NilConn_SharedAuthType_PerCall_SuccessUpdatesToolMap pins
// the fix for the shared-per-call tool-discovery bug at the full
// performCheck level (not just performAdminToolDiscovery in isolation): a
// shared oauth/headers/none client running per-call
// (needs_session_stickiness nil/false) must have its tool map populated by
// the periodic checker, exactly like a per_user_oauth client already does
// (TestPerformCheck_NilConn_PerUserOAuth_SuccessUpdatesToolMap).
func TestPerformCheck_NilConn_SharedAuthType_PerCall_SuccessUpdatesToolMap(t *testing.T) {
	for _, authType := range []schemas.MCPAuthType{schemas.MCPAuthTypeHeaders, schemas.MCPAuthTypeOauth} {
		t.Run(string(authType), func(t *testing.T) {
			ts, _ := buildAdminDiscoveryHTTPServer(t)

			cred := &fakeAdminCredStore{headers: http.Header{"Authorization": []string{"Bearer shared-token"}}}
			manager := &MCPManager{
				credStore: cred,
				logger:    &MockLogger{},
				clientMap: map[string]*schemas.MCPClientState{},
			}

			state, config := newSyncTestClientState("client-shared-percall", "shared-client", authType, map[string]schemas.ChatTool{})
			config.ConnectionType = schemas.MCPConnectionTypeHTTP
			config.ConnectionString = schemas.NewSecretVar(ts.URL)
			manager.clientMap[config.ID] = state

			checker := NewClientConnectionChecker(manager, config.ID, time.Minute, false, &MockLogger{})
			checker.performCheck()

			require.Equal(t, 1, cred.callCount())
			updated := manager.clientMap[config.ID].ToolMap
			require.Contains(t, updated, "shared-client-echo", "a successful admin-discovery cycle should populate the client's tool map")
			require.Equal(t, schemas.MCPConnectionStateHealthy, manager.clientMap[config.ID].State)
		})
	}
}

// TestPerformCheck_NilConn_SharedAuthType_TriggersReconnect covers the
// behavior change from the old ClientToolSyncer: a sticky client with no
// live connection (e.g. still Unstable from a prior failed connect) is no
// longer silently skipped by the periodic checker — it now attempts a
// reconnect directly, closing the gap where only a separate, slower
// consecutive-failure-counting health monitor used to eventually notice.
// Observed via the manager's exclusive-op registry (the same guard
// ReconnectClient/connectToMCPClient use), since the attempt itself runs in
// a background goroutine and — with no real server behind config's
// ConnectionString — fails fast, which is fine: only that it was attempted
// matters here.
func TestPerformCheck_NilConn_SharedAuthType_TriggersReconnect(t *testing.T) {
	manager := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)

	config := &schemas.MCPClientConfig{
		ID:                     "client-reconnect-trigger",
		Name:                   "shared-client",
		AuthType:               schemas.MCPAuthTypeHeaders,
		ConnectionType:         schemas.MCPConnectionTypeHTTP,
		ConnectionString:       schemas.NewSecretVar("http://127.0.0.1:0/mcp"), // unreachable — attempt must still be made
		NeedsSessionStickiness: schemas.Ptr(true),                              // sticky: routes through the reconnect-on-nil-conn branch under test, not checkPerCall
	}
	manager.mu.Lock()
	manager.clientMap[config.ID] = &schemas.MCPClientState{
		Name:            config.Name,
		ExecutionConfig: config,
		State:           schemas.MCPConnectionStateUnstable,
		Conn:            nil,
	}
	manager.mu.Unlock()

	checker := NewClientConnectionChecker(manager, config.ID, time.Minute, false, &MockLogger{})
	checker.performCheck()

	require.Eventually(t, func() bool {
		_, inFlightOrDone := manager.reconnectingClients.Load(config.ID)
		return inFlightOrDone
	}, 2*time.Second, 10*time.Millisecond, "performCheck must trigger a reconnect attempt for a sticky client with no live connection")
}

// TestPerformCheck_NilConn_PerUserOAuth_SuccessUpdatesToolMap is an
// end-to-end happy-path check: when admin discovery succeeds, the
// discovered tools actually replace the client's ToolMap, using a real
// upstream HTTP MCP server (buildAdminDiscoveryHTTPServer, shared with
// admin_tool_discovery_test.go) rather than mocking the connect/discover
// cycle away.
func TestPerformCheck_NilConn_PerUserOAuth_SuccessUpdatesToolMap(t *testing.T) {
	ts, _ := buildAdminDiscoveryHTTPServer(t)

	cred := &fakeAdminCredStore{headers: http.Header{"Authorization": []string{"Bearer admin-token"}}}
	manager := &MCPManager{
		credStore: cred,
		logger:    &MockLogger{},
		clientMap: map[string]*schemas.MCPClientState{},
	}

	state, config := newSyncTestClientState("client-4", "oauth-client", schemas.MCPAuthTypePerUserOauth, map[string]schemas.ChatTool{})
	config.ConnectionType = schemas.MCPConnectionTypeHTTP
	config.ConnectionString = schemas.NewSecretVar(ts.URL)
	manager.clientMap[config.ID] = state

	checker := NewClientConnectionChecker(manager, config.ID, time.Minute, false, &MockLogger{})
	checker.performCheck()

	require.Equal(t, 1, cred.callCount())
	updated := manager.clientMap[config.ID].ToolMap
	require.Contains(t, updated, "oauth-client-echo", "a successful admin-discovery cycle should populate the client's tool map")
	require.Equal(t, schemas.MCPConnectionStateHealthy, manager.clientMap[config.ID].State)
}

// TestSetState_StaleGeneration_DoesNotOverwriteState pins the gap CodeRabbit
// flagged: recordFailure/recordSuccess (via setState) had no generation
// guard at all, unlike writeBackTools' existing one for tool-map writes. A
// check that started before a sticky->per-call transition (which closes the
// connection but, before this fix, never bumped ConnGeneration) can complete
// afterward — StopChecking doesn't cancel an in-flight check — and its state
// write would land on an entry that's since moved on, e.g. marking Unstable
// a client that no longer has a persistent connection to be unstable about.
// setState must drop a write whose captured generation no longer matches the
// client's current one, mirroring writeBackTools' own guard exactly.
func TestSetState_StaleGeneration_DoesNotOverwriteState(t *testing.T) {
	manager := &MCPManager{
		logger:    &MockLogger{},
		clientMap: map[string]*schemas.MCPClientState{},
	}
	state, config := newSyncTestClientState("client-gen", "gen-client", schemas.MCPAuthTypeHeaders, nil)
	state.State = schemas.MCPConnectionStateHealthy
	state.ConnGeneration = 6 // already bumped past what the stale check captured
	manager.clientMap[config.ID] = state

	checker := NewClientConnectionChecker(manager, config.ID, time.Minute, false, &MockLogger{})

	// A check that captured generation 5 (before the transition) finishing
	// late must not move the state at all.
	checker.setState(schemas.MCPConnectionStateUnstable, 5, schemas.MCPConnectionFailureStagePing, errors.New("stale"))
	require.Equal(t, schemas.MCPConnectionStateHealthy, manager.clientMap[config.ID].State, "a stale-generation state write must be dropped")
	require.Nil(t, manager.clientMap[config.ID].LastFailure, "a dropped state write must not leave a failure record behind either")

	// A check whose captured generation still matches the client's current
	// one must apply normally — the guard isn't a blanket no-op.
	checker.setState(schemas.MCPConnectionStateUnstable, 6, schemas.MCPConnectionFailureStagePing, errors.New("current"))
	require.Equal(t, schemas.MCPConnectionStateUnstable, manager.clientMap[config.ID].State, "a current-generation state write must still apply")
	require.NotNil(t, manager.clientMap[config.ID].LastFailure, "an applied failure write records its reason")
	require.Equal(t, "current", manager.clientMap[config.ID].LastFailure.Message)
}

// TestSetState_FiresStateChangeCallbackOnGenuineTransition verifies
// ClientConnectionChecker.setState — the single writer every periodic-check
// transition in this file funnels through — fires the manager's registered
// state-change callback exactly once per genuine transition, with the
// correct old/new states, and not at all when the target state matches the
// current one.
func TestSetState_FiresStateChangeCallbackOnGenuineTransition(t *testing.T) {
	manager := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
	config := &schemas.MCPClientConfig{ID: "client-checker-callback", Name: "checker-callback-client"}

	type call struct {
		clientID, name     string
		oldState, newState schemas.MCPConnectionState
	}
	var calls []call
	manager.SetStateChangeCallback(func(clientID, name string, oldState, newState schemas.MCPConnectionState) {
		calls = append(calls, call{clientID, name, oldState, newState})
	})

	manager.mu.Lock()
	manager.clientMap[config.ID] = &schemas.MCPClientState{
		Name:            config.Name,
		ExecutionConfig: config,
		State:           schemas.MCPConnectionStateHealthy,
	}
	manager.mu.Unlock()

	checker := NewClientConnectionChecker(manager, config.ID, time.Minute, false, &MockLogger{})

	// Healthy -> Unstable: a genuine transition, must fire once. Generation 0
	// matches this client's zero-value ConnGeneration (never bumped here).
	checker.recordFailure(config.Name, schemas.MCPConnectionFailureStagePing, errors.New("boom"), 0)
	require.Len(t, calls, 1)
	assert.Equal(t, config.ID, calls[0].clientID)
	assert.Equal(t, config.Name, calls[0].name)
	assert.Equal(t, schemas.MCPConnectionStateHealthy, calls[0].oldState)
	assert.Equal(t, schemas.MCPConnectionStateUnstable, calls[0].newState)

	// Unstable -> Unstable: a no-op, must not fire again.
	checker.recordFailure(config.Name, schemas.MCPConnectionFailureStagePing, errors.New("boom again"), 0)
	require.Len(t, calls, 1, "a repeat failure that doesn't change state must not re-fire the callback")

	// Unstable -> Healthy: a genuine transition, must fire again.
	checker.recordSuccess(config.Name, 0)
	require.Len(t, calls, 2)
	assert.Equal(t, schemas.MCPConnectionStateUnstable, calls[1].oldState)
	assert.Equal(t, schemas.MCPConnectionStateHealthy, calls[1].newState)
}

// =============================================================================
// ClientConnectionChecker.performCheck: the live-connection (conn != nil)
// branch's background reconnect.
// =============================================================================

// sessionInvalidator fronts a real streamable-HTTP MCP server and reproduces
// the upstream shape behind this regression: the server expires a persistent
// MCP session, so every later request carrying that Mcp-Session-Id is
// rejected permanently (422 "Unexpected message, expect initialize request"),
// while a sessionless initialize is still served normally. A dead session is
// therefore unrecoverable by re-probing, and recoverable only by dialling a
// fresh connection.
type sessionInvalidator struct {
	mu       sync.Mutex
	lastSeen string
	dead     map[string]bool
}

// expireCurrentSession invalidates whatever session the server last served,
// standing in for a server-side session timeout.
func (s *sessionInvalidator) expireCurrentSession() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastSeen != "" {
		s.dead[s.lastSeen] = true
	}
}

// reject reports whether r belongs to an expired session, writing the
// upstream's own error response when it does. Sessionless requests (the
// initialize of a brand-new connection) always pass through.
func (s *sessionInvalidator) reject(w http.ResponseWriter, r *http.Request) bool {
	sessionID := r.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead[sessionID] {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte("Unexpected message, expect initialize request"))
		return true
	}
	s.lastSeen = sessionID
	return false
}

// buildExpiringSessionMCPServer starts a streamable-HTTP MCP server (one
// "echo" tool) whose sessions the test can invalidate at will.
func buildExpiringSessionMCPServer(t *testing.T) (*httptest.Server, *sessionInvalidator) {
	t.Helper()

	s := server.NewMCPServer("test-session-expiry", "1.0.0", server.WithToolCapabilities(true))
	s.AddTool(
		mcpgo.NewTool("echo", mcpgo.WithDescription("Echo tool")),
		func(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			return mcpgo.NewToolResultText("ok"), nil
		},
	)

	streamable := server.NewStreamableHTTPServer(s)
	invalidator := &sessionInvalidator{dead: map[string]bool{}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if invalidator.reject(w, r) {
			return
		}
		streamable.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts, invalidator
}

// TestPerformCheck_LiveConn_FailedCheck_TriggersReconnect pins the behavior
// ClientConnectionChecker's own doc comment promises for the live-connection
// branch: a failed check of the current sticky connection marks the client
// Unstable AND starts a background reconnect. Without that reconnect the
// failed connection stays installed on the entry, so every later tick
// re-probes the same dead session and the conn == nil reconnect branch is
// never reached: the client stays Unstable until an administrator
// reconnects it by hand.
func TestPerformCheck_LiveConn_FailedCheck_TriggersReconnect(t *testing.T) {
	ts, upstream := buildExpiringSessionMCPServer(t)

	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
	config := &schemas.MCPClientConfig{
		ID:                     "client-live-check-reconnect",
		Name:                   "sticky-http-client",
		AuthType:               schemas.MCPAuthTypeHeaders,
		ConnectionType:         schemas.MCPConnectionTypeHTTP,
		ConnectionString:       schemas.NewSecretVar(ts.URL),
		NeedsSessionStickiness: schemas.Ptr(true), // sticky: holds the persistent connection under test
		ToolsToExecute:         []string{"*"},
	}
	require.NoError(t, m.connectToMCPClient(context.Background(), config))

	before, ok := snapshotClientState(m, config.ID)
	require.True(t, ok)
	require.NotNil(t, before.Conn)
	require.Equal(t, schemas.MCPConnectionStateHealthy, before.State)

	// The upstream expires the session behind the live connection: list_tools
	// over it now fails permanently, while a fresh dial would still succeed.
	upstream.expireCurrentSession()

	checker := NewClientConnectionChecker(m, config.ID, time.Minute, false, &MockLogger{})
	next, onSteady := checker.performCheck()
	assert.Equal(t, UnstableConnectionCheckInterval, next, "a failed live check must stay on the tight recovery cadence")
	assert.False(t, onSteady)

	unstable, ok := snapshotClientState(m, config.ID)
	require.True(t, ok)
	require.Equal(t, schemas.MCPConnectionStateUnstable, unstable.State, "the failed check must mark the client unstable")

	// Observed through the manager's exclusive-op registry, the same guard
	// ReconnectClient dedupes concurrent attempts with, and what the conn ==
	// nil branch's own reconnect is asserted through elsewhere.
	require.Eventually(t, func() bool {
		_, inFlightOrDone := m.reconnectingClients.Load(config.ID)
		return inFlightOrDone
	}, 2*time.Second, 10*time.Millisecond, "a failed check of the live sticky connection must start the background reconnect")

	// Registering the attempt is not the point on its own: the dead session
	// must actually be replaced by a fresh one.
	require.Eventually(t, func() bool {
		after, exists := snapshotClientState(m, config.ID)
		return exists &&
			after.State == schemas.MCPConnectionStateHealthy &&
			after.ConnGeneration > before.ConnGeneration
	}, 10*time.Second, 20*time.Millisecond, "the background reconnect must install a fresh connection and return the client to healthy")

	after, ok := snapshotClientState(m, config.ID)
	require.True(t, ok)
	assert.NotSame(t, before.Conn, after.Conn, "the dead connection must be replaced, not re-probed forever")
}

// TestReconnectAfterFailedCheck_StaleOrAuthoritativeState_DoesNotReconnect
// covers the guard half. A check runs unlocked and can take seconds, so by the
// time it fails the entry may have moved on: a reconnect already swapped in a
// fresh connection (the failure proves only that the replaced one was dead,
// and redialling would tear down its healthy successor), or an authoritative
// state was written that must not be dialled out of. Disabled and NeedsReauth
// are checked separately from the generation on purpose, since neither
// DisableClient nor CloseAndMarkNeedsReauth bumps ConnGeneration when it
// clears the connection.
func TestReconnectAfterFailedCheck_StaleOrAuthoritativeState_DoesNotReconnect(t *testing.T) {
	const currentGeneration = 6

	tests := []struct {
		name            string
		state           schemas.MCPConnectionState
		checkGeneration uint64
		wantReconnect   bool
	}{
		{
			name:            "a check that spanned a reconnect must not redial its successor",
			state:           schemas.MCPConnectionStateUnstable,
			checkGeneration: currentGeneration - 1,
		},
		{
			name:            "a client disabled mid-check must not be dialled",
			state:           schemas.MCPConnectionStateDisabled,
			checkGeneration: currentGeneration,
		},
		{
			name:            "a credential confirmed dead has nothing to reconnect with",
			state:           schemas.MCPConnectionStateNeedsReauth,
			checkGeneration: currentGeneration,
		},
		{
			name:            "a current-generation failure on a live client does redial",
			state:           schemas.MCPConnectionStateUnstable,
			checkGeneration: currentGeneration,
			wantReconnect:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
			config := &schemas.MCPClientConfig{
				ID:                     "client-reconnect-guard",
				Name:                   "guarded-client",
				AuthType:               schemas.MCPAuthTypeHeaders,
				ConnectionType:         schemas.MCPConnectionTypeHTTP,
				ConnectionString:       schemas.NewSecretVar("http://127.0.0.1:0/mcp"), // unreachable: only the attempt matters
				NeedsSessionStickiness: schemas.Ptr(true),
			}
			m.mu.Lock()
			m.clientMap[config.ID] = &schemas.MCPClientState{
				Name:            config.Name,
				ExecutionConfig: config,
				State:           tc.state,
				ConnGeneration:  currentGeneration,
			}
			m.mu.Unlock()

			checker := NewClientConnectionChecker(m, config.ID, time.Minute, false, &MockLogger{})
			checker.reconnectAfterFailedCheck(config.Name, tc.checkGeneration)

			if tc.wantReconnect {
				require.Eventually(t, func() bool {
					_, inFlightOrDone := m.reconnectingClients.Load(config.ID)
					return inFlightOrDone
				}, 2*time.Second, 10*time.Millisecond, "the guards must not be a blanket no-op")
				return
			}
			require.Never(t, func() bool {
				_, inFlightOrDone := m.reconnectingClients.Load(config.ID)
				return inFlightOrDone
			}, 500*time.Millisecond, 25*time.Millisecond, "no reconnect may be started for this client")
		})
	}
}

// =============================================================================
// SSE: a dropped event stream is repaired by the periodic checker, not by
// anything SSE-specific.
// =============================================================================

// TestSSEStreamDropped_CheckerReconnects covers the SSE half of the same gap
// the STDIO respawn test covers in the integration suite, and the gap is wider
// than handleSSEConnectionLost suggests: for a server-side close, mcp-go never
// invokes the OnConnectionLost hook at all. The entry keeps reading healthy, on
// a connection that is already dead, with no failure recorded. Nothing converts
// transport death into Conn == nil either, so the client sits on the checker's
// live-connection branch and that branch's reconnect is the only thing that can
// heal it. The assertions below pin that, rather than a loss callback that does
// not fire here.
//
// httptest's CloseClientConnections drops the established stream while the
// server keeps accepting new ones, which is exactly the recoverable shape: an
// idle timeout or a proxy hang-up rather than a server that has gone away.
func TestSSEStreamDropped_CheckerReconnects(t *testing.T) {
	s := server.NewMCPServer("test-sse-reconnect", "1.0.0", server.WithToolCapabilities(true))
	s.AddTool(
		mcpgo.NewTool("echo", mcpgo.WithDescription("Echo tool")),
		func(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			return mcpgo.NewToolResultText("ok"), nil
		},
	)
	ts := server.NewTestServer(s)
	t.Cleanup(ts.Close)

	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
	config := &schemas.MCPClientConfig{
		ID:               "client-sse-reconnect",
		Name:             "sse-client",
		AuthType:         schemas.MCPAuthTypeNone,
		ConnectionType:   schemas.MCPConnectionTypeSSE,
		ConnectionString: schemas.NewSecretVar(ts.URL + "/sse"),
		ToolsToExecute:   []string{"*"},
	}
	require.NoError(t, m.connectToMCPClient(context.Background(), config))
	// Registered after ts.Close above so it runs BEFORE it (t.Cleanup is LIFO):
	// a live SSE stream keeps httptest.Server.Close blocked indefinitely.
	t.Cleanup(func() {
		_ = m.RemoveClient(config.ID)
		ts.CloseClientConnections()
	})

	before, ok := snapshotClientState(m, config.ID)
	require.True(t, ok)
	require.NotNil(t, before.Conn)
	require.Equal(t, schemas.MCPConnectionStateHealthy, before.State)

	// Drop every established stream. The server keeps listening, so a fresh
	// dial still succeeds.
	ts.CloseClientConnections()

	// Register a tool the dead session never saw. ToolMap is deliberately kept
	// as last-known-good when a connection fails, so asserting the original echo
	// tool at the end would pass on stale state; only a tool that appeared after
	// the drop proves list_tools actually re-ran over the replacement.
	s.AddTool(
		mcpgo.NewTool("reconnected", mcpgo.WithDescription("registered only after the stream was dropped")),
		func(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			return mcpgo.NewToolResultText("ok"), nil
		},
	)

	// Nothing notices the drop on its own. Asserting Conn != nil here would be
	// vacuous (it was already true before the drop), so assert the whole
	// unchanged shape instead, and assert that it stays that way: no loss
	// callback fires, no failure is recorded, the connection is not detached,
	// and the client still advertises itself as healthy while being unusable.
	// That is the precise reason the periodic checker is the only repair path
	// for SSE, and it is what the branch under test has to act on.
	require.Never(t, func() bool {
		st, exists := snapshotClientState(m, config.ID)
		return !exists ||
			st.State != schemas.MCPConnectionStateHealthy ||
			st.ConnGeneration != before.ConnGeneration ||
			st.Conn == nil ||
			st.LastFailure != nil
	}, 1*time.Second, 100*time.Millisecond, "a dropped SSE stream is not self-detected: the entry must stay healthy on its dead connection until the checker probes it")

	// Why the post-reconnect tool assertion is not vacuous: right now the entry
	// still carries the pre-drop list, and cannot possibly carry the tool that
	// was registered after it. Asserting the original echo tool at the end would
	// therefore pass on this stale state alone.
	stale, ok := snapshotClientState(m, config.ID)
	require.True(t, ok)
	require.Contains(t, stale.ToolMap, config.Name+"-echo", "the pre-drop tool list is retained as last-known-good")
	require.NotContains(t, stale.ToolMap, config.Name+"-reconnected", "and cannot contain a tool registered after the drop until rediscovery runs")

	checker := NewClientConnectionChecker(m, config.ID, time.Minute, false, &MockLogger{})
	checker.performCheck()

	require.Eventually(t, func() bool {
		after, exists := snapshotClientState(m, config.ID)
		return exists &&
			after.State == schemas.MCPConnectionStateHealthy &&
			after.ConnGeneration > before.ConnGeneration
	}, 30*time.Second, 100*time.Millisecond, "the checker's reconnect must re-establish the stream")

	after, ok := snapshotClientState(m, config.ID)
	require.True(t, ok)
	assert.NotSame(t, before.Conn, after.Conn, "the dead stream must be replaced, not re-probed")
	assert.Contains(t, after.ToolMap, config.Name+"-reconnected",
		"rediscovery must run over the replacement connection, and only a tool registered after the drop can show that")
	assert.Contains(t, after.ToolMap, config.Name+"-echo", "without losing what the client already had")
}
