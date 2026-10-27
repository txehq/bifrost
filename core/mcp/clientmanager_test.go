package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/network"
	"github.com/capsohq/bifrost/core/network/proxytest"
	"github.com/capsohq/bifrost/core/schemas"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateSTDIOConnectionAllowsInlineEnvAssignments(t *testing.T) {
	t.Parallel()

	config := &schemas.MCPClientConfig{
		Name:           "test-stdio-client",
		ConnectionType: schemas.MCPConnectionTypeSTDIO,
		StdioConfig: &schemas.MCPStdioConfig{
			Command: "echo",
			Envs:    []string{"TEST_STDIO_ENV_ASSIGNMENT=inline-value"},
		},
	}

	_, _, err := (&MCPManager{}).createSTDIOConnection(context.Background(), config, nil)
	require.NoError(t, err)
}

func TestCreateSTDIOConnectionAllowsSetReferencedEnvVars(t *testing.T) {
	t.Setenv("TEST_STDIO_ENV_REFERENCE_SET", "set-value")

	config := &schemas.MCPClientConfig{
		Name:           "test-stdio-client",
		ConnectionType: schemas.MCPConnectionTypeSTDIO,
		StdioConfig: &schemas.MCPStdioConfig{
			Command: "echo",
			Envs:    []string{"TEST_STDIO_ENV_REFERENCE_SET"},
		},
	}

	_, _, err := (&MCPManager{}).createSTDIOConnection(context.Background(), config, nil)
	require.NoError(t, err)
}

func TestCreateSTDIOConnectionRequiresReferencedEnvVars(t *testing.T) {
	t.Setenv("TEST_STDIO_ENV_REFERENCE_MISSING", "")

	config := &schemas.MCPClientConfig{
		Name:           "test-stdio-client",
		ConnectionType: schemas.MCPConnectionTypeSTDIO,
		StdioConfig: &schemas.MCPStdioConfig{
			Command: "echo",
			Envs:    []string{"TEST_STDIO_ENV_REFERENCE_MISSING"},
		},
	}

	_, _, err := (&MCPManager{}).createSTDIOConnection(context.Background(), config, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "environment variable TEST_STDIO_ENV_REFERENCE_MISSING is not set")
}

func TestCreateSTDIOConnectionRejectsEmptyEnvAssignmentName(t *testing.T) {
	t.Parallel()

	config := &schemas.MCPClientConfig{
		Name:           "test-stdio-client",
		ConnectionType: schemas.MCPConnectionTypeSTDIO,
		StdioConfig: &schemas.MCPStdioConfig{
			Command: "echo",
			Envs:    []string{"=inline-value"},
		},
	}

	_, _, err := (&MCPManager{}).createSTDIOConnection(context.Background(), config, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "environment variable name is empty")
}

// TestCloseAndMarkNeedsReauth_ClosesLiveConnectionAndFlipsState covers the
// core behavior this method exists for: after OAuth credential rotation, a
// shared client's live connection (still bound to the now-invalidated
// Authorization header) must be torn down and the entry flipped to
// needs_reauth, without attempting a new dial.
func TestCloseAndMarkNeedsReauth_ClosesLiveConnectionAndFlipsState(t *testing.T) {
	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, expiredOAuthCredStore{}, nil, nil)
	config := newSharedOAuthClientConfig("client-rotated")

	cancelCalled := false
	m.mu.Lock()
	m.clientMap[config.ID] = &schemas.MCPClientState{
		Name:            config.Name,
		ExecutionConfig: config,
		State:           schemas.MCPConnectionStateHealthy,
		CancelFunc:      func() { cancelCalled = true },
	}
	m.mu.Unlock()

	require.NoError(t, m.CloseAndMarkNeedsReauth("client-rotated"))

	m.mu.RLock()
	state := *m.clientMap["client-rotated"]
	m.mu.RUnlock()

	assert.True(t, cancelCalled, "the live connection's cancel func must be invoked")
	assert.Nil(t, state.CancelFunc)
	assert.Nil(t, state.Conn)
	assert.Equal(t, schemas.MCPConnectionStateNeedsReauth, state.State)
	require.NotNil(t, state.LastFailure, "needs_reauth carries its own explanation")
	assert.Equal(t, schemas.MCPConnectionFailureStageCredential, state.LastFailure.Stage)
	assert.Equal(t, errCredentialRotated.Error(), state.LastFailure.Message)
}

// TestFailConnectAttempt_NeedsReauthTransition_FiresStateChangeCallback
// verifies that failConnectAttempt's reactive classification of a connect
// failure as permanent (needsReauth=true) fires the registered state-change
// callback with the correct old/new states — this transition (unlike
// CloseAndMarkNeedsReauth) has no admin-API call site of its own for a
// caller to observe the change through otherwise.
func TestFailConnectAttempt_NeedsReauthTransition_FiresStateChangeCallback(t *testing.T) {
	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, expiredOAuthCredStore{}, nil, nil)
	config := newSharedOAuthClientConfig("client-fail-reauth")

	type call struct {
		clientID, name     string
		oldState, newState schemas.MCPConnectionState
	}
	var calls []call
	m.SetStateChangeCallback(func(clientID, name string, oldState, newState schemas.MCPConnectionState) {
		calls = append(calls, call{clientID, name, oldState, newState})
	})

	entry := &schemas.MCPClientState{
		Name:            config.Name,
		ExecutionConfig: config,
		State:           schemas.MCPConnectionStateUnstable,
	}
	m.mu.Lock()
	m.clientMap[config.ID] = entry
	m.mu.Unlock()

	m.failConnectAttempt(entry, config, nil, nil, nil, nil, true, errors.New("oauth2 token expired"))

	require.Len(t, calls, 1, "the callback must fire exactly once for a genuine state transition")
	assert.Equal(t, config.ID, calls[0].clientID)
	assert.Equal(t, config.Name, calls[0].name)
	assert.Equal(t, schemas.MCPConnectionStateUnstable, calls[0].oldState)
	assert.Equal(t, schemas.MCPConnectionStateNeedsReauth, calls[0].newState)

	m.mu.RLock()
	state := m.clientMap[config.ID].State
	m.mu.RUnlock()
	assert.Equal(t, schemas.MCPConnectionStateNeedsReauth, state)
}

// TestFailConnectAttempt_NoStateChange_DoesNotFireCallback verifies the
// callback is a pure transition signal — reclassifying a client that's
// already Unstable as Unstable again must not fire it.
func TestFailConnectAttempt_NoStateChange_DoesNotFireCallback(t *testing.T) {
	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, expiredOAuthCredStore{}, nil, nil)
	config := newSharedOAuthClientConfig("client-no-change")

	fired := false
	m.SetStateChangeCallback(func(clientID, name string, oldState, newState schemas.MCPConnectionState) {
		fired = true
	})

	entry := &schemas.MCPClientState{
		Name:            config.Name,
		ExecutionConfig: config,
		State:           schemas.MCPConnectionStateUnstable,
	}
	m.mu.Lock()
	m.clientMap[config.ID] = entry
	m.mu.Unlock()

	m.failConnectAttempt(entry, config, nil, nil, nil, nil, false, errors.New("connection refused"))

	assert.False(t, fired, "the callback must not fire when the transition is a no-op")
}

// TestCloseAndMarkNeedsReauth_MissingClient_Errors covers the not-found path.
func TestCloseAndMarkNeedsReauth_MissingClient_Errors(t *testing.T) {
	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, expiredOAuthCredStore{}, nil, nil)
	require.Error(t, m.CloseAndMarkNeedsReauth("does-not-exist"))
}

// TestCloseAndMarkNeedsReauth_PerUserAuth_ReturnsNotApplicable covers
// per_user_oauth/per_user_headers clients, which hold no persistent shared
// connection: rotation for these auth types must not error out the caller
// (the HTTP handler filters ErrMCPReconnectNotApplicable), and must not
// touch the entry's state.
func TestCloseAndMarkNeedsReauth_PerUserAuth_ReturnsNotApplicable(t *testing.T) {
	// nil credStore: NewMCPManager defaults to a real credstore.CredStore,
	// whose RequiresPerCallConnection actually varies by auth type — unlike
	// expiredOAuthCredStore (used by the other tests here), which hardcodes
	// false regardless of AuthType.
	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, nil, nil)
	config := &schemas.MCPClientConfig{ID: "client-per-user", Name: "per-user-client", AuthType: schemas.MCPAuthTypePerUserOauth}

	m.mu.Lock()
	m.clientMap[config.ID] = &schemas.MCPClientState{
		Name:            config.Name,
		ExecutionConfig: config,
		State:           schemas.MCPConnectionStateHealthy,
	}
	m.mu.Unlock()

	err := m.CloseAndMarkNeedsReauth(config.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, schemas.ErrMCPReconnectNotApplicable))

	m.mu.RLock()
	state := *m.clientMap[config.ID]
	m.mu.RUnlock()
	assert.Equal(t, schemas.MCPConnectionStateHealthy, state.State, "state must be untouched for a not-applicable auth type")
}

// TestCloseAndMarkNeedsReauth_Disabled_IsNoOp covers a rotation racing a
// disable: DisableClient's state is authoritative and must not be
// resurrected into needs_reauth by a rotation that started before it.
func TestCloseAndMarkNeedsReauth_Disabled_IsNoOp(t *testing.T) {
	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, expiredOAuthCredStore{}, nil, nil)
	config := newSharedOAuthClientConfig("client-disabled")

	m.mu.Lock()
	m.clientMap[config.ID] = &schemas.MCPClientState{
		Name:            config.Name,
		ExecutionConfig: config,
		State:           schemas.MCPConnectionStateDisabled,
	}
	m.mu.Unlock()

	require.NoError(t, m.CloseAndMarkNeedsReauth("client-disabled"))

	m.mu.RLock()
	state := *m.clientMap["client-disabled"]
	m.mu.RUnlock()
	assert.Equal(t, schemas.MCPConnectionStateDisabled, state.State)
}

// TestEnableClient_GuardLostLeavesDisabledUntouched pins the ordering
// CodeRabbit flagged: EnableClient must acquire the exclusive per-client
// operation guard (beginExclusiveClientOp) before flipping
// ExecutionConfig.Disabled, not after. Flipping it first and only then
// losing the guard to a concurrent operation (e.g. ReconnectClient) would
// leave the entry looking enabled to that other caller's connectToMCPClient
// call while State is still Disabled, letting it dial the entry out from
// under this failed EnableClient — which then returns "already in progress"
// even though the client was actually just connected by the race winner.
func TestEnableClient_GuardLostLeavesDisabledUntouched(t *testing.T) {
	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, expiredOAuthCredStore{}, nil, nil)
	config := newSharedOAuthClientConfig("client-enable-race")
	config.Disabled = true

	m.mu.Lock()
	m.clientMap[config.ID] = &schemas.MCPClientState{
		Name:            config.Name,
		ExecutionConfig: config,
		State:           schemas.MCPConnectionStateDisabled,
	}
	m.mu.Unlock()

	// Simulate a concurrent operation (e.g. ReconnectClient) already holding
	// the exclusive guard for this client when EnableClient is called.
	finish, ok := m.beginExclusiveClientOp(config.ID)
	require.True(t, ok)
	defer finish(nil)

	err := m.EnableClient(config.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already in progress")

	m.mu.RLock()
	state := *m.clientMap[config.ID]
	m.mu.RUnlock()
	assert.True(t, state.ExecutionConfig.Disabled, "a failed EnableClient (guard already held elsewhere) must not have flipped Disabled — otherwise a concurrent connectToMCPClient could dial the still-Disabled-State entry")
	assert.Equal(t, schemas.MCPConnectionStateDisabled, state.State)
}

// TestUpdateClientCredentials_PerCallShared_Healthy_RefreshesToolsSynchronously
// pins the per-call half of the reauthorize symmetry: a sticky client's
// credential update goes through connectToMCPClient, which always re-runs
// tool discovery, while a per-call shared client used to return the
// not-applicable sentinel and keep serving its stale tool list until the
// periodic checker's next tick (minutes away on a Healthy client). A
// credential update on an already-verified per-call shared client must
// instead refresh tools synchronously with the now-current credential.
func TestUpdateClientCredentials_PerCallShared_Healthy_RefreshesToolsSynchronously(t *testing.T) {
	ts, _ := buildAdminDiscoveryHTTPServer(t)

	// nil credStore: NewMCPManager defaults to a real credstore.CredStore,
	// whose RequiresPerCallConnection actually ANDs auth type with
	// NeedsSessionStickiness (unlike expiredOAuthCredStore, used elsewhere
	// in this file, which hardcodes false regardless of either).
	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, nil, nil)
	config := &schemas.MCPClientConfig{
		ID:               "client-shared-percall-refresh",
		Name:             "shared-percall-client-refresh",
		AuthType:         schemas.MCPAuthTypeHeaders,
		ConnectionType:   schemas.MCPConnectionTypeHTTP,
		ConnectionString: schemas.NewSecretVar(ts.URL),
		Headers:          map[string]schemas.SecretVar{"Authorization": *schemas.NewSecretVar("Bearer rotated-token")},
		// NeedsSessionStickiness left nil on purpose: the default value.
	}

	m.mu.Lock()
	m.clientMap[config.ID] = &schemas.MCPClientState{
		Name:            config.Name,
		ExecutionConfig: config,
		State:           schemas.MCPConnectionStateHealthy,
		ToolMap:         make(map[string]schemas.ChatTool),
		ToolNameMapping: make(map[string]string),
	}
	m.mu.Unlock()

	err := m.UpdateClientCredentials(config.ID, config)
	require.NoError(t, err, "a per-call shared client's credential update must succeed by refreshing tools, not report not-applicable")

	m.mu.RLock()
	state := *m.clientMap[config.ID]
	m.mu.RUnlock()
	assert.Contains(t, state.ToolMap, "shared-percall-client-refresh-echo", "tools must be refreshed synchronously, not deferred to the periodic checker")
}

// TestSetClientTools_ReplacesStaleTools pins SetClientTools' replace
// semantics: every caller passes a complete discovery result, so a tool the
// upstream removed between discoveries must disappear from the in-memory
// ToolMap. The old merge behavior kept such tools alive in memory (and on
// the hosted MCP surface, which serves from ToolMap) while the DB, persisted
// from the passed-in set via the tools-change callback, had already dropped
// them.
func TestSetClientTools_ReplacesStaleTools(t *testing.T) {
	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, nil, nil)
	config := &schemas.MCPClientConfig{ID: "client-replace-tools", Name: "replace-tools-client"}

	m.mu.Lock()
	m.clientMap[config.ID] = &schemas.MCPClientState{
		Name:            config.Name,
		ExecutionConfig: config,
		State:           schemas.MCPConnectionStateHealthy,
		ToolMap: map[string]schemas.ChatTool{
			"replace-tools-client-removed": {},
		},
		ToolNameMapping: map[string]string{"replace-tools-client-removed": "removed"},
	}
	m.mu.Unlock()

	m.SetClientTools(config.ID,
		map[string]schemas.ChatTool{"replace-tools-client-kept": {}},
		map[string]string{"replace-tools-client-kept": "kept"},
		"",
	)

	m.mu.RLock()
	state := *m.clientMap[config.ID]
	m.mu.RUnlock()
	assert.Contains(t, state.ToolMap, "replace-tools-client-kept")
	assert.NotContains(t, state.ToolMap, "replace-tools-client-removed", "a tool absent from the fresh discovery result must not survive in memory")
	assert.Equal(t, map[string]string{"replace-tools-client-kept": "kept"}, state.ToolNameMapping)
}

// TestUpdateClientCredentials_PerCallSharedOAuth_DiscoveryFailureFailsUpdate
// pins the strict half of the same symmetry: a sticky reconnect whose
// tools/list fails is a failed reconnect, so a per-call refresh whose
// discovery fails must fail the update with a real error, not the
// not-applicable sentinel; the periodic checker retries on its own cadence
// afterwards. No OAuth provider is configured here, so resolving the shared
// credential for discovery fails deterministically.
func TestUpdateClientCredentials_PerCallSharedOAuth_DiscoveryFailureFailsUpdate(t *testing.T) {
	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, nil, nil)
	config := &schemas.MCPClientConfig{
		ID:             "client-shared-percall",
		Name:           "shared-percall-client",
		AuthType:       schemas.MCPAuthTypeOauth,
		ConnectionType: schemas.MCPConnectionTypeHTTP,
		// NeedsSessionStickiness left nil on purpose: the default value.
	}

	m.mu.Lock()
	m.clientMap[config.ID] = &schemas.MCPClientState{
		Name:            config.Name,
		ExecutionConfig: config,
		State:           schemas.MCPConnectionStateHealthy,
	}
	m.mu.Unlock()

	err := m.UpdateClientCredentials(config.ID, config)
	require.Error(t, err)
	assert.False(t, errors.Is(err, schemas.ErrMCPReconnectNotApplicable), "a failed tool refresh is a real failure, not the not-applicable no-op")
	assert.Contains(t, err.Error(), "tool discovery")
}

// TestUpdateClientCredentials_PerCallPerUser_ReturnsReconnectNotApplicable
// pins that per-user auth types keep the not-applicable sentinel: the
// credentials updated through here are never the retained admin discovery
// credential (the admin-verify paths own that flow and re-discover
// themselves), so there is genuinely nothing to refresh.
func TestUpdateClientCredentials_PerCallPerUser_ReturnsReconnectNotApplicable(t *testing.T) {
	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, nil, nil)
	config := &schemas.MCPClientConfig{
		ID:             "client-per-user-creds",
		Name:           "per-user-creds-client",
		AuthType:       schemas.MCPAuthTypePerUserOauth,
		ConnectionType: schemas.MCPConnectionTypeHTTP,
	}

	m.mu.Lock()
	m.clientMap[config.ID] = &schemas.MCPClientState{
		Name:            config.Name,
		ExecutionConfig: config,
		State:           schemas.MCPConnectionStateHealthy,
	}
	m.mu.Unlock()

	err := m.UpdateClientCredentials(config.ID, config)
	require.Error(t, err)
	assert.True(t, errors.Is(err, schemas.ErrMCPReconnectNotApplicable))
}

// TestUpdateClientCredentials_PerCallShared_Disabled_SkipsRefresh pins the
// Disabled guard on the per-call refresh: SetClientTools forces a client
// back to Healthy, so a disabled client must keep the sentinel (the fresh
// credential is picked up when the client is re-enabled, which runs its own
// discovery) rather than get resurrected by a credential update.
func TestUpdateClientCredentials_PerCallShared_Disabled_SkipsRefresh(t *testing.T) {
	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, nil, nil)
	config := &schemas.MCPClientConfig{
		ID:             "client-shared-percall-disabled",
		Name:           "shared-percall-client-disabled",
		AuthType:       schemas.MCPAuthTypeOauth,
		ConnectionType: schemas.MCPConnectionTypeHTTP,
	}

	m.mu.Lock()
	m.clientMap[config.ID] = &schemas.MCPClientState{
		Name:            config.Name,
		ExecutionConfig: config,
		State:           schemas.MCPConnectionStateDisabled,
	}
	m.mu.Unlock()

	err := m.UpdateClientCredentials(config.ID, config)
	require.Error(t, err)
	assert.True(t, errors.Is(err, schemas.ErrMCPReconnectNotApplicable))

	m.mu.RLock()
	state := *m.clientMap[config.ID]
	m.mu.RUnlock()
	assert.Equal(t, schemas.MCPConnectionStateDisabled, state.State, "a credential update must not resurrect a disabled client")
}

// TestUpdateClientCredentials_PerCallSharedOAuth_PendingVerification_TransitionsToHealthy
// pins the second half of the same bug report: a per-call shared-OAuth
// client's FIRST connection (still in PendingVerification — e.g. a
// config.json-bootstrapped client whose admin just completed the OAuth
// browser flow) must not be treated as "nothing to do" the way an
// already-Healthy per-call client's reauthorize is. Skipping the transition
// left the client permanently stuck showing pending_verification in the UI
// (with no way to retry, since the pending OAuth stash this endpoint
// requires is already cleared by the caller) until a restart re-ran
// AddClient from scratch and performed the transition correctly.
func TestUpdateClientCredentials_PerCallSharedOAuth_PendingVerification_TransitionsToHealthy(t *testing.T) {
	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, nil, nil)
	config := &schemas.MCPClientConfig{
		ID:             "client-pending-percall",
		Name:           "pending-percall-client",
		AuthType:       schemas.MCPAuthTypeOauth,
		ConnectionType: schemas.MCPConnectionTypeHTTP,
	}

	m.mu.Lock()
	m.clientMap[config.ID] = &schemas.MCPClientState{
		Name:            config.Name,
		ExecutionConfig: config,
		State:           schemas.MCPConnectionStatePendingVerification,
		ToolMap:         make(map[string]schemas.ChatTool),
		ToolNameMapping: make(map[string]string),
	}
	m.mu.Unlock()

	err := m.UpdateClientCredentials(config.ID, config)
	require.NoError(t, err, "the first connection out of pending_verification must succeed, not report not-applicable")

	m.mu.RLock()
	state := *m.clientMap[config.ID]
	m.mu.RUnlock()
	assert.Equal(t, schemas.MCPConnectionStateHealthy, state.State, "must transition out of pending_verification, mirroring AddClient's own per-call setup")

	// Without a connection checker, nothing would ever discover this
	// client's tools afterward (no persistent Conn, no DiscoveredTools to
	// restore for a client completing its first connection here) — see
	// TestAddClient_PerCallConnection_StartsConnectionChecker for the
	// AddClient-side counterpart of this same fix.
	m.checkerManager.mu.RLock()
	_, hasChecker := m.checkerManager.checkers[config.ID]
	m.checkerManager.mu.RUnlock()
	assert.True(t, hasChecker, "must start a connection checker so tools actually get discovered")

	// A second call, now that the client is already Healthy, is the plain
	// reauthorize case: it must attempt a synchronous tool refresh with the
	// updated credential rather than report not-applicable. No OAuth
	// provider is configured here, so resolving the credential for that
	// discovery fails, and the failure must surface as a real error (not
	// the sentinel).
	err = m.UpdateClientCredentials(config.ID, config)
	require.Error(t, err)
	assert.False(t, errors.Is(err, schemas.ErrMCPReconnectNotApplicable))
	assert.Contains(t, err.Error(), "tool discovery")
}

// TestUpdateClientCredentials_PerCallSharedType_PendingVerification_DiscoversToolsSynchronously
// pins the second half of the same bug report: a connection checker alone
// is not enough, because ClientConnectionChecker.Start uses the SLOW
// healthyInterval (not the fast Unstable one) for a client that already
// starts Healthy — so without a synchronous first discovery pass, a shared
// oauth/headers/none per-call client completing OAuth/verification here
// would sit at Healthy/0-tools for up to healthyInterval before its first
// real check ever ran.
func TestUpdateClientCredentials_PerCallSharedType_PendingVerification_DiscoversToolsSynchronously(t *testing.T) {
	ts, _ := buildAdminDiscoveryHTTPServer(t)

	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, nil, nil)
	config := &schemas.MCPClientConfig{
		ID:               "client-pending-percall-sync",
		Name:             "pending-percall-client-sync",
		AuthType:         schemas.MCPAuthTypeHeaders,
		ConnectionType:   schemas.MCPConnectionTypeHTTP,
		ConnectionString: schemas.NewSecretVar(ts.URL),
		Headers:          map[string]schemas.SecretVar{"Authorization": *schemas.NewSecretVar("Bearer shared-update-token")},
	}

	m.mu.Lock()
	m.clientMap[config.ID] = &schemas.MCPClientState{
		Name:            config.Name,
		ExecutionConfig: config,
		State:           schemas.MCPConnectionStatePendingVerification,
		ToolMap:         make(map[string]schemas.ChatTool),
		ToolNameMapping: make(map[string]string),
	}
	m.mu.Unlock()

	err := m.UpdateClientCredentials(config.ID, config)
	require.NoError(t, err)

	m.mu.RLock()
	state := *m.clientMap[config.ID]
	m.mu.RUnlock()
	assert.Equal(t, schemas.MCPConnectionStateHealthy, state.State)
	assert.Contains(t, state.ToolMap, "pending-percall-client-sync-echo", "tools must be discovered synchronously, not deferred entirely to the periodic checker")
}

// TestReconnectClient_PerCallSharedOAuth_ReturnsReconnectNotApplicable pins
// the same message-wording bug as TestUpdateClientCredentials's sibling:
// ReconnectClient's per-call guard used to unconditionally say "per-user
// auth clients", even though RequiresPerCallConnection is also true for a
// genuinely shared oauth/headers client running per-call via
// needs_session_stickiness nil/false. The sentinel was always correct; only
// the message text was misleading for this case.
func TestReconnectClient_PerCallSharedOAuth_ReturnsReconnectNotApplicable(t *testing.T) {
	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, nil, nil)
	config := &schemas.MCPClientConfig{
		ID:             "client-shared-percall-reconnect",
		Name:           "shared-percall-client-reconnect",
		AuthType:       schemas.MCPAuthTypeOauth,
		ConnectionType: schemas.MCPConnectionTypeHTTP,
		// NeedsSessionStickiness left nil on purpose: the default value.
	}

	m.mu.Lock()
	m.clientMap[config.ID] = &schemas.MCPClientState{
		Name:            config.Name,
		ExecutionConfig: config,
		State:           schemas.MCPConnectionStateHealthy,
	}
	m.mu.Unlock()

	err := m.ReconnectClient(config.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, schemas.ErrMCPReconnectNotApplicable))
	assert.NotContains(t, err.Error(), "per-user", "a genuinely shared client must not be told it's a per-user auth client")
}

// TestCloseAndMarkNeedsReauth_PerCallSharedOAuth_ReturnsReconnectNotApplicable
// is the CloseAndMarkNeedsReauth counterpart of the same message-wording fix.
func TestCloseAndMarkNeedsReauth_PerCallSharedOAuth_ReturnsReconnectNotApplicable(t *testing.T) {
	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, nil, nil)
	config := &schemas.MCPClientConfig{
		ID:             "client-shared-percall-close",
		Name:           "shared-percall-client-close",
		AuthType:       schemas.MCPAuthTypeOauth,
		ConnectionType: schemas.MCPConnectionTypeHTTP,
	}

	m.mu.Lock()
	m.clientMap[config.ID] = &schemas.MCPClientState{
		Name:            config.Name,
		ExecutionConfig: config,
		State:           schemas.MCPConnectionStateHealthy,
	}
	m.mu.Unlock()

	err := m.CloseAndMarkNeedsReauth(config.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, schemas.ErrMCPReconnectNotApplicable))
	assert.NotContains(t, err.Error(), "per-user", "a genuinely shared client must not be told it's a per-user auth client")
}

// TestBuildTLSHTTPClientNeverFallsBackToUnguardedDefault proves buildTLSHTTPClient
// always returns a non-nil client (with or without TLS configured) so callers
// never fall back to the mcp-go library's own default HTTP client, which
// carries no dial guard at all.
func TestBuildTLSHTTPClientNeverFallsBackToUnguardedDefault(t *testing.T) {
	for _, tlsCfg := range []*schemas.MCPTLSConfig{nil, {InsecureSkipVerify: true}} {
		httpClient, err := (&MCPManager{logger: defaultLogger}).buildTLSHTTPClient(&schemas.MCPClientConfig{TLSConfig: tlsCfg})
		require.NoError(t, err)
		require.NotNil(t, httpClient)

		transport, ok := httpClient.Transport.(*http.Transport)
		require.True(t, ok)
		require.NotNil(t, transport.DialContext)
	}
}

// TestBuildTLSHTTPClientBlocksLinkLocal proves the dial-time guard refuses
// link-local destinations (including the 169.254.169.254 cloud metadata
// endpoint) - the one class of target with no legitimate MCP use case under
// any deployment topology, authenticated or not.
func TestBuildTLSHTTPClientBlocksLinkLocal(t *testing.T) {
	httpClient, err := (&MCPManager{logger: defaultLogger}).buildTLSHTTPClient(&schemas.MCPClientConfig{})
	require.NoError(t, err)
	transport := httpClient.Transport.(*http.Transport)

	_, err = transport.DialContext(context.Background(), "tcp", "169.254.169.254:80")
	require.Error(t, err)
	require.Contains(t, err.Error(), "blocked connection to link-local address")
}

// TestBuildTLSHTTPClientAllowsLoopback proves loopback MCP servers keep
// working: connecting to a local MCP tool server on the same host is the
// documented primary HTTP-client use case
// (docs/mcp/connecting-to-servers.mdx), so the dial-time guard here must not
// reject it. The unauthenticated-caller case is refused earlier, at the HTTP
// handler layer (rejectPrivateMCPTargetIfAuthBypassed), not here.
func TestBuildTLSHTTPClientAllowsLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Errorf("failed to close test listener: %v", err)
		}
	})
	// The accepted server side is handed back to the test body so both ends are closed, and
	// their Close results checked, on the test goroutine rather than after it may have ended.
	accepted := make(chan net.Conn, 1)
	go func() {
		if conn, err := ln.Accept(); err == nil {
			accepted <- conn
		}
	}()

	httpClient, err := (&MCPManager{logger: defaultLogger}).buildTLSHTTPClient(&schemas.MCPClientConfig{})
	require.NoError(t, err)
	transport := httpClient.Transport.(*http.Transport)

	conn, err := transport.DialContext(context.Background(), "tcp", ln.Addr().String())
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	select {
	case server := <-accepted:
		require.NoError(t, server.Close())
	case <-time.After(5 * time.Second):
		t.Fatal("listener never accepted the dialed connection")
	}
}

// TestBuildTLSHTTPClientRoutesThroughConfiguredProxy proves the client honors
// the transport's proxy selector for a permitted destination, which is what a
// deployment with no direct egress depends on: the request must reach the
// proxy, and the destination must never be dialed directly. This is the
// behavior core v1.8.5 through v1.9.1 broke by nulling the selector.
//
// The proxy is injected by swapping DefaultTransport.Proxy rather than via
// HTTP_PROXY, because ProxyFromEnvironment reads the environment once per
// process and would not see a t.Setenv made after any earlier test used it.
// Every test here that touches DefaultTransport.Proxy therefore stays
// sequential (no t.Parallel) and restores the original selector on cleanup.
func TestBuildTLSHTTPClientRoutesThroughConfiguredProxy(t *testing.T) {
	// The destination must never see a connection: with a proxy selected,
	// only the proxy may be dialed.
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := targetLn.Close(); err != nil {
			t.Errorf("failed to close destination listener: %v", err)
		}
	})
	var targetHits atomic.Int32
	go func() {
		for {
			conn, err := targetLn.Accept()
			if err != nil {
				return
			}
			targetHits.Add(1)
			_ = conn.Close()
		}
	}()

	// A minimal proxy: records what it was asked for, answers a plain-HTTP
	// absolute-URL request itself, and refuses a CONNECT, since tunnelling
	// TLS is not what this test is about; seeing the CONNECT is enough.
	var (
		proxyHits atomic.Int32
		mu        sync.Mutex
		lastReq   string
	)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		mu.Lock()
		lastReq = r.Method + " " + r.Host
		mu.Unlock()
		if r.Method == http.MethodConnect {
			http.Error(w, "no tunnel in this test", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("via-proxy"))
	}))
	t.Cleanup(proxy.Close)
	proxyURL, err := url.Parse(proxy.URL)
	require.NoError(t, err)
	seen := func() string {
		mu.Lock()
		defer mu.Unlock()
		return lastReq
	}

	base, ok := http.DefaultTransport.(*http.Transport)
	require.True(t, ok)
	origProxy := base.Proxy
	base.Proxy = http.ProxyURL(proxyURL)
	t.Cleanup(func() { base.Proxy = origProxy })

	httpClient, err := (&MCPManager{logger: defaultLogger}).buildTLSHTTPClient(&schemas.MCPClientConfig{})
	require.NoError(t, err)
	httpClient.Timeout = 5 * time.Second
	target := targetLn.Addr().String()

	t.Run("http", func(t *testing.T) {
		resp, err := httpClient.Get("http://" + target + "/mcp")
		require.NoError(t, err)
		body, readErr := io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())
		require.NoError(t, readErr)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "via-proxy", string(body), "the response must come from the proxy")
		require.Equal(t, "GET "+target, seen(), "the proxy must be asked for the MCP destination")
	})
	t.Run("https", func(t *testing.T) {
		resp, err := httpClient.Get("https://" + target + "/mcp")
		if resp != nil {
			require.NoError(t, resp.Body.Close())
		}
		require.Error(t, err, "the test proxy refuses tunnels, so the request must fail at the proxy")
		require.Equal(t, "CONNECT "+target, seen(), "an https destination must be tunnelled through the proxy")
	})
	require.Equal(t, int32(2), proxyHits.Load(), "one proxy request per scheme")
	require.Zero(t, targetHits.Load(), "with a proxy selected the destination must never be dialed directly")
}

// TestBuildTLSHTTPClientRefusesProxyingBlockedLiteral proves the dial-time
// guard cannot be sidestepped by a proxy. When a proxy is selected,
// http.Transport hands DialContext the proxy's address rather than the MCP
// destination, so PrivateNetworkDialContext alone would validate the proxy
// and let the proxy forward to the blocked target. The selector wrapper must
// refuse a blocked IP-literal destination (link-local, and the cloud metadata
// endpoints that live outside link-local) before the proxy is ever contacted.
func TestBuildTLSHTTPClientRefusesProxyingBlockedLiteral(t *testing.T) {
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := proxyLn.Close(); err != nil {
			t.Errorf("failed to close proxy listener: %v", err)
		}
	})
	// Any accepted connection is itself the failure; its Close result is passed back to the
	// test body rather than reported from the goroutine, which may outlive the subtest.
	var proxyHits atomic.Int32
	proxyCloseErrs := make(chan error, 8)
	go func() {
		for {
			conn, err := proxyLn.Accept()
			if err != nil {
				return
			}
			proxyHits.Add(1)
			if err := conn.Close(); err != nil {
				select {
				case proxyCloseErrs <- err:
				default:
				}
			}
		}
	}()
	proxyURL, err := url.Parse("http://" + proxyLn.Addr().String())
	require.NoError(t, err)

	base, ok := http.DefaultTransport.(*http.Transport)
	require.True(t, ok)
	origProxy := base.Proxy
	base.Proxy = http.ProxyURL(proxyURL)
	t.Cleanup(func() { base.Proxy = origProxy })

	targets := []struct {
		host string
		want string
	}{
		{"169.254.169.254", "blocked connection to link-local address"},
		{"100.100.100.200", "blocked connection to cloud metadata endpoint"},
		{"[fd00:ec2::254]", "blocked connection to cloud metadata endpoint"},
	}
	for _, scheme := range []string{"http", "https"} {
		for _, tc := range targets {
			t.Run(scheme+"/"+tc.host, func(t *testing.T) {
				httpClient, err := (&MCPManager{logger: defaultLogger}).buildTLSHTTPClient(&schemas.MCPClientConfig{})
				require.NoError(t, err)
				httpClient.Timeout = 5 * time.Second

				resp, err := httpClient.Get(scheme + "://" + tc.host + "/latest/meta-data/")
				if resp != nil {
					require.NoError(t, resp.Body.Close())
				}
				require.Error(t, err, "a blocked MCP target must be refused even when a proxy is configured")
				require.Contains(t, err.Error(), tc.want,
					"the refusal must come from the destination policy, not from the proxy")
				require.Zero(t, proxyHits.Load(), "the configured proxy must never be contacted for a guarded MCP request")
				select {
				case closeErr := <-proxyCloseErrs:
					t.Errorf("failed to close a proxy-side connection: %v", closeErr)
				default:
				}
			})
		}
	}
}

// TestBuildTLSHTTPClientProxiedHostnameNeedsLocalDNS pins the fork's proxied-name
// contract (upstream hands an unresolved name to the proxy): a name the gateway cannot
// resolve is refused before the proxy is contacted, since the proxy's resolver could map
// it to a blocked address, while a name that resolves to a permitted address is still sent
// to the proxy under its original hostname.
func TestBuildTLSHTTPClientProxiedHostnameNeedsLocalDNS(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []string
	)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.Host)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("via-proxy"))
	}))
	t.Cleanup(proxy.Close)
	proxyURL, err := url.Parse(proxy.URL)
	require.NoError(t, err)

	base, ok := http.DefaultTransport.(*http.Transport)
	require.True(t, ok)
	origProxy := base.Proxy
	base.Proxy = http.ProxyURL(proxyURL)
	t.Cleanup(func() { base.Proxy = origProxy })

	httpClient, err := (&MCPManager{logger: defaultLogger}).buildTLSHTTPClient(&schemas.MCPClientConfig{})
	require.NoError(t, err)
	httpClient.Timeout = 5 * time.Second

	_, err = httpClient.Get("http://mcp.invalid/mcp")
	require.Error(t, err, "a hostname the gateway cannot resolve must not be sent to the proxy")

	resp, err := httpClient.Get("http://localhost/mcp")
	require.NoError(t, err)
	body, readErr := io.ReadAll(resp.Body)
	require.NoError(t, resp.Body.Close())
	require.NoError(t, readErr)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "via-proxy", string(body))
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"GET localhost"}, requests, "only the resolvable name reaches the proxy, under its original hostname")
}

// TestBuildTLSHTTPClientBypassedProxyDialsDirect covers the NO_PROXY shape: the
// selector is present but returns no proxy for the request, so the transport
// dials the destination itself and the dial-time guard is what applies. A
// loopback destination is reached directly and a link-local one is still
// blocked, exactly as when no proxy is configured at all.
func TestBuildTLSHTTPClientBypassedProxyDialsDirect(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("direct"))
	}))
	t.Cleanup(target.Close)

	base, ok := http.DefaultTransport.(*http.Transport)
	require.True(t, ok)
	origProxy := base.Proxy
	base.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	t.Cleanup(func() { base.Proxy = origProxy })

	httpClient, err := (&MCPManager{logger: defaultLogger}).buildTLSHTTPClient(&schemas.MCPClientConfig{})
	require.NoError(t, err)
	httpClient.Timeout = 5 * time.Second

	resp, err := httpClient.Get(target.URL + "/mcp")
	require.NoError(t, err)
	body, readErr := io.ReadAll(resp.Body)
	require.NoError(t, resp.Body.Close())
	require.NoError(t, readErr)
	require.Equal(t, "direct", string(body))
	require.Equal(t, int32(1), targetHits.Load(), "a bypassed proxy means the destination is dialed directly")

	resp, err = httpClient.Get("http://169.254.169.254/latest/meta-data/")
	if resp != nil {
		require.NoError(t, resp.Body.Close())
	}
	require.Error(t, err)
	require.Contains(t, err.Error(), "blocked connection to link-local address",
		"on the direct path the dial-time guard still refuses link-local")
}

// TestMCPProxySelectorPassthrough pins the wrapper's two pass-through
// behaviors: a nil selector stays nil so a transport without one is unchanged,
// and an error from the wrapped selector is returned as-is.
func TestMCPProxySelectorPassthrough(t *testing.T) {
	require.Nil(t, mcpProxySelector(nil, false, net.DefaultResolver))

	wantErr := errors.New("selector failed")
	sel := mcpProxySelector(func(*http.Request) (*url.URL, error) { return nil, wantErr }, false, net.DefaultResolver)
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1/mcp", nil)
	require.NoError(t, err)
	_, err = sel(req)
	require.ErrorIs(t, err, wantErr)
}

// A proxied hostname is resolved on the gateway before the proxy is chosen. A name the
// gateway cannot resolve is refused rather than handed to the proxy, whose resolver could
// map it to a blocked address; a name that resolves to a permitted address goes through.
func TestMCPProxySelectorResolvesProxiedHostnames(t *testing.T) {
	proxyURL, err := url.Parse("http://127.0.0.1:3128")
	require.NoError(t, err)
	sel := mcpProxySelector(http.ProxyURL(proxyURL), false, net.DefaultResolver)

	for _, target := range []string{"http://bifrost.invalid/mcp", "https://bifrost.invalid:8443/mcp"} {
		req, err := http.NewRequest(http.MethodGet, target, nil)
		require.NoError(t, err)
		got, err := sel(req)
		require.Error(t, err, "%s must not reach the proxy unresolved", target)
		require.Nil(t, got)
	}

	req, err := http.NewRequest(http.MethodGet, "http://localhost/mcp", nil)
	require.NoError(t, err)
	got, err := sel(req)
	require.NoError(t, err, "a resolvable private-network name is permitted, as on the direct path")
	require.Equal(t, proxyURL, got)

	req, err = http.NewRequest(http.MethodGet, "http://169.254.169.254/latest/meta-data/", nil)
	require.NoError(t, err)
	_, err = sel(req)
	require.Error(t, err, "a link-local literal is still refused before the proxy")
}

// =============================================================================
// pending_verification is authoritative: it means a human still has to
// complete a one-time setup step, and nothing automatic may overwrite it with
// a state that hides the CTA the UI shows for it.
// =============================================================================

// newPendingClientConfig builds a client that AddClient would have parked in
// pending_verification: an OAuth-based client carrying the inline oauth_config
// block from config.json that no admin has authorized yet.
func newPendingClientConfig(id string, authType schemas.MCPAuthType) *schemas.MCPClientConfig {
	return &schemas.MCPClientConfig{
		ID:                 id,
		Name:               "pending-" + id,
		AuthType:           authType,
		ConnectionType:     schemas.MCPConnectionTypeHTTP,
		ConnectionString:   schemas.NewSecretVar("http://127.0.0.1:0/mcp"),
		PendingOAuthConfig: &schemas.OAuth2Config{},
	}
}

// TestEnableClient_StillPendingVerification_KeepsThatState covers the path that
// loses the signal. AddClient's disabled branch runs before its
// pending_verification branches, so a client declared both disabled and
// unauthorized is registered as Disabled with its PendingOAuthConfig intact.
// Enabling it then took the ordinary route: the per-call branch marked it
// Healthy outright, and the sticky branch dialled with a credential that does
// not exist yet. Either way the "an admin must authorize this" state was gone,
// and with it the Verify CTA that is the only way to resolve it.
func TestEnableClient_StillPendingVerification_KeepsThatState(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*schemas.MCPClientConfig)
		authType schemas.MCPAuthType
	}{
		{name: "per_user_oauth", authType: schemas.MCPAuthTypePerUserOauth},
		{name: "oauth", authType: schemas.MCPAuthTypeOauth},
		{
			// per_user_headers is pending only while DiscoveredTools is nil:
			// AddClient nil-checks it on purpose so a verified server that
			// legitimately exposes zero tools is not re-parked on every reload.
			name:     "per_user_headers with no discovery yet",
			authType: schemas.MCPAuthTypePerUserHeaders,
			mutate: func(c *schemas.MCPClientConfig) {
				c.PendingOAuthConfig = nil
				c.DiscoveredTools = nil
			},
		},
		{
			// token_exchange follows the same nil rule. An initialized empty map
			// would mean "verified, and the server has no tools", which is not
			// pending: see TestAwaitsAdminVerification_NilIsTheDiscriminator.
			name:     "token_exchange with no discovery yet",
			authType: schemas.MCPAuthTypeTokenExchange,
			mutate: func(c *schemas.MCPClientConfig) {
				c.PendingOAuthConfig = nil
				c.DiscoveredTools = nil
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
			defer m.checkerManager.StopAll()

			config := newPendingClientConfig("client-enable-pending", tc.authType)
			if tc.mutate != nil {
				tc.mutate(config)
			}
			config.Disabled = true
			m.mu.Lock()
			m.clientMap[config.ID] = &schemas.MCPClientState{
				Name:            config.Name,
				ExecutionConfig: config,
				State:           schemas.MCPConnectionStateDisabled,
				ToolMap:         map[string]schemas.ChatTool{},
				ToolNameMapping: map[string]string{},
				ConnectionInfo:  &schemas.MCPClientConnectionInfo{Type: config.ConnectionType},
			}
			m.mu.Unlock()

			require.NoError(t, m.EnableClient(config.ID), "enabling must succeed: the client is legitimately enabled, just not authorized yet")

			m.mu.RLock()
			state := m.clientMap[config.ID].State
			disabled := m.clientMap[config.ID].ExecutionConfig.Disabled
			m.mu.RUnlock()

			assert.Equal(t, schemas.MCPConnectionStatePendingVerification, state,
				"an enabled but never-authorized client is awaiting an admin, not healthy and not broken")
			assert.False(t, disabled, "the enable itself still stands")
		})
	}
}

// TestPerformCheck_PendingVerification_StaysQuiet pins the checker half. A
// client awaiting its one-time admin flow has no credential to check with, so
// a check can only rediscover what is already known and would overwrite the
// state with Unstable, replacing an actionable "authorize this" with a generic
// "something is wrong". Same treatment NeedsReauth already gets.
func TestPerformCheck_PendingVerification_StaysQuiet(t *testing.T) {
	cred := &fakeAdminCredStore{err: errors.New("admin credential unavailable")}
	m := &MCPManager{credStore: cred, logger: &MockLogger{}, clientMap: map[string]*schemas.MCPClientState{}}

	config := newPendingClientConfig("client-check-pending", schemas.MCPAuthTypePerUserOauth)
	m.clientMap[config.ID] = &schemas.MCPClientState{
		Name:            config.Name,
		ExecutionConfig: config,
		State:           schemas.MCPConnectionStatePendingVerification,
	}

	checker := NewClientConnectionChecker(m, config.ID, time.Minute, false, &MockLogger{})
	next, onSteady := checker.performCheck()

	assert.Equal(t, 0, cred.callCount(), "no discovery may be attempted without the credential the admin has yet to supply")
	assert.Equal(t, schemas.MCPConnectionStatePendingVerification, m.clientMap[config.ID].State)
	assert.True(t, onSteady, "and the timer stays on the relaxed cadence, like NeedsReauth")
	assert.Equal(t, time.Minute, next)
}

// TestSetState_PendingVerification_NotOverwritten guards the same invariant at
// the single writer, so a check already in flight when a client is parked in
// pending_verification cannot land on it afterwards.
func TestSetState_PendingVerification_NotOverwritten(t *testing.T) {
	m := &MCPManager{logger: &MockLogger{}, clientMap: map[string]*schemas.MCPClientState{}}
	config := newPendingClientConfig("client-setstate-pending", schemas.MCPAuthTypeOauth)
	m.clientMap[config.ID] = &schemas.MCPClientState{
		Name:            config.Name,
		ExecutionConfig: config,
		State:           schemas.MCPConnectionStatePendingVerification,
	}

	checker := NewClientConnectionChecker(m, config.ID, time.Minute, false, &MockLogger{})

	checker.setState(schemas.MCPConnectionStateUnstable, 0, schemas.MCPConnectionFailureStageListTools, errors.New("late failure"))
	assert.Equal(t, schemas.MCPConnectionStatePendingVerification, m.clientMap[config.ID].State)
	assert.Nil(t, m.clientMap[config.ID].LastFailure, "a dropped write must not leave a failure record behind either")

	checker.setState(schemas.MCPConnectionStateHealthy, 0, "", nil)
	assert.Equal(t, schemas.MCPConnectionStatePendingVerification, m.clientMap[config.ID].State,
		"a synthetic success must not silently promote an unauthorized client either")
}

// TestAddClient_TokenExchange_VerifiedWithZeroTools_IsNotReParked pins the
// discriminator between "never verified" and "verified, and the server exposes
// no tools". It is nil-ness, not length, and the store is what makes it so:
// BeforeSave marshals DiscoveredTools only when it is non-nil, so a verified
// zero-tool server persists "{}", and AfterFind unmarshals that back into a
// non-nil empty map. A client that never completed verification round-trips as
// nil instead.
//
// Reading an empty map as "not verified" therefore re-parks a perfectly
// verified client in pending_verification on every single reload, which is the
// re-park the per-user-headers branch nil-checks specifically to avoid. Token
// exchange was len-checking and had that bug.
func TestAddClient_TokenExchange_VerifiedWithZeroTools_IsNotReParked(t *testing.T) {
	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
	defer m.checkerManager.StopAll()

	config := newPendingClientConfig("client-tokenexchange-zero-tools", schemas.MCPAuthTypeTokenExchange)
	// AddClient validates the name; the sibling tests here bypass it by writing
	// into clientMap directly, so they never needed a conforming one.
	config.Name = "tokenexchangezerotools"
	config.PendingOAuthConfig = nil
	config.TokenExchange = &schemas.MCPTokenExchangeConfig{
		Audience: "https://example.invalid/api",
		ClientID: schemas.NewSecretVar("exchange-app"),
	}
	// Exactly what AfterFind produces for a verified server with no tools.
	config.DiscoveredTools = map[string]schemas.ChatTool{}

	require.NoError(t, m.AddClient(context.Background(), config))

	m.mu.RLock()
	state := m.clientMap[config.ID].State
	m.mu.RUnlock()

	assert.NotEqual(t, schemas.MCPConnectionStatePendingVerification, state,
		"a verified token-exchange client that simply exposes no tools must not be sent back through admin verification")
}

// TestAwaitsAdminVerification_NilIsTheDiscriminator states the same rule
// directly, for both auth types whose pending-ness is decided by the tool map.
func TestAwaitsAdminVerification_NilIsTheDiscriminator(t *testing.T) {
	for _, authType := range []schemas.MCPAuthType{schemas.MCPAuthTypePerUserHeaders, schemas.MCPAuthTypeTokenExchange} {
		t.Run(string(authType), func(t *testing.T) {
			config := &schemas.MCPClientConfig{AuthType: authType}

			config.DiscoveredTools = nil
			assert.True(t, awaitsAdminVerification(config), "a nil tool map means verification never ran")

			config.DiscoveredTools = map[string]schemas.ChatTool{}
			assert.False(t, awaitsAdminVerification(config), "a non-nil empty map means verification ran and found nothing")

			config.DiscoveredTools = map[string]schemas.ChatTool{"a": {}}
			assert.False(t, awaitsAdminVerification(config))
		})
	}
}

// newVerifiedClientConfig builds a client whose one-time admin verification has
// already run: DiscoveredTools is non-nil, which is the whole of what
// awaitsAdminVerification reads for these auth types.
func newVerifiedClientConfig(id string, authType schemas.MCPAuthType, discovered map[string]schemas.ChatTool) *schemas.MCPClientConfig {
	config := &schemas.MCPClientConfig{
		ID:                        id,
		Name:                      "verified" + id,
		AuthType:                  authType,
		ConnectionType:            schemas.MCPConnectionTypeHTTP,
		ConnectionString:          schemas.NewSecretVar("http://127.0.0.1:0/mcp"),
		DiscoveredTools:           discovered,
		DiscoveredToolNameMapping: map[string]string{},
	}
	if authType == schemas.MCPAuthTypeTokenExchange {
		config.TokenExchange = &schemas.MCPTokenExchangeConfig{
			Audience: "https://example.invalid/api",
			ClientID: schemas.NewSecretVar("exchange-app"),
		}
	}
	return config
}

// TestUpdateClient_VerifiedClientStaysVerified pins that editing a client does
// not un-verify it.
//
// awaitsAdminVerification reads the manager's in-memory config, and for
// per_user_headers and token_exchange the whole of what it reads is whether
// DiscoveredTools is nil. UpdateClient rebuilds that config field by field from
// two lists, "copy from the existing config" and "copy from the update", and
// DiscoveredTools was on neither, so every edit niled it. Nothing read it after
// registration until this predicate did, and from then on an ordinary edit
// (renaming the server, changing a timeout) left a verified, healthy client
// reading as never verified: enabling it parked it back in
// pending_verification, taking away a working client until an admin re-verified
// something they had already verified.
//
// The update handler is the reason the edit carries no DiscoveredTools: it
// builds the config it hands UpdateClient from the request, and only sets the
// field when the same request re-ran verification.
func TestUpdateClient_VerifiedClientStaysVerified(t *testing.T) {
	cases := []struct {
		name       string
		authType   schemas.MCPAuthType
		discovered map[string]schemas.ChatTool
	}{
		{
			name:       "per_user_headers",
			authType:   schemas.MCPAuthTypePerUserHeaders,
			discovered: map[string]schemas.ChatTool{"verifiedclient-lookup": {}},
		},
		{
			name:       "token_exchange",
			authType:   schemas.MCPAuthTypeTokenExchange,
			discovered: map[string]schemas.ChatTool{"verifiedclient-lookup": {}},
		},
		{
			// Verified, and the server exposes nothing. Still verified: nil-ness
			// is the discriminator, so the empty map has to survive as an empty
			// map rather than collapse to nil.
			name:       "verified with zero tools",
			authType:   schemas.MCPAuthTypePerUserHeaders,
			discovered: map[string]schemas.ChatTool{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
			defer m.checkerManager.StopAll()

			config := newVerifiedClientConfig("client", tc.authType, tc.discovered)
			m.mu.Lock()
			m.clientMap[config.ID] = &schemas.MCPClientState{
				Name:            config.Name,
				ExecutionConfig: config,
				State:           schemas.MCPConnectionStateHealthy,
				ToolMap:         map[string]schemas.ChatTool{},
				ToolNameMapping: map[string]string{},
				ConnectionInfo:  &schemas.MCPClientConnectionInfo{Type: config.ConnectionType},
			}
			m.mu.Unlock()
			require.False(t, awaitsAdminVerification(config), "fixture check: the client starts out verified")

			// An ordinary edit, shaped the way the update handler shapes it: the
			// editable fields, and no DiscoveredTools.
			edit := *config
			edit.DiscoveredTools = nil
			edit.DiscoveredToolNameMapping = nil
			edit.ToolExecutionTimeout = 45 * time.Second
			require.NoError(t, m.UpdateClient(config.ID, &edit))

			m.mu.RLock()
			updated := m.clientMap[config.ID].ExecutionConfig
			m.mu.RUnlock()
			assert.Equal(t, 45*time.Second, updated.ToolExecutionTimeout, "fixture check: the edit itself applied")
			require.NotNil(t, updated.DiscoveredTools, "an edit must not discard the record that verification ran")
			assert.Equal(t, tc.discovered, updated.DiscoveredTools)
			assert.NotNil(t, updated.DiscoveredToolNameMapping)
			assert.False(t, awaitsAdminVerification(updated), "an edited client is still a verified client")

			// The symptom on this path: disable it, enable it, and it must come
			// back as the working client it was.
			require.NoError(t, m.DisableClient(config.ID))
			require.NoError(t, m.EnableClient(config.ID))
			m.mu.RLock()
			state := m.clientMap[config.ID].State
			m.mu.RUnlock()
			assert.NotEqual(t, schemas.MCPConnectionStatePendingVerification, state,
				"enabling an edited, verified client must not send it back through admin verification")
		})
	}
}

// TestUpdateClient_ReverifiedToolsReplaceTheOldOnes is the other half of the
// carry-forward rule. An update whose request re-ran verification arrives with
// the tools that run discovered, and those are the current truth: keeping the
// old set over them would pin a stale tool list to the config.
func TestUpdateClient_ReverifiedToolsReplaceTheOldOnes(t *testing.T) {
	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
	defer m.checkerManager.StopAll()

	config := newVerifiedClientConfig("client", schemas.MCPAuthTypePerUserHeaders,
		map[string]schemas.ChatTool{"verifiedclient-old": {}})
	m.mu.Lock()
	m.clientMap[config.ID] = &schemas.MCPClientState{
		Name:            config.Name,
		ExecutionConfig: config,
		State:           schemas.MCPConnectionStateHealthy,
		ToolMap:         map[string]schemas.ChatTool{},
		ToolNameMapping: map[string]string{},
		ConnectionInfo:  &schemas.MCPClientConnectionInfo{Type: config.ConnectionType},
	}
	m.mu.Unlock()

	edit := *config
	edit.DiscoveredTools = map[string]schemas.ChatTool{"verifiedclient-new": {}}
	edit.DiscoveredToolNameMapping = map[string]string{"new": "new"}
	require.NoError(t, m.UpdateClient(config.ID, &edit))

	m.mu.RLock()
	updated := m.clientMap[config.ID].ExecutionConfig
	m.mu.RUnlock()
	assert.Equal(t, map[string]schemas.ChatTool{"verifiedclient-new": {}}, updated.DiscoveredTools)
	assert.Equal(t, map[string]string{"new": "new"}, updated.DiscoveredToolNameMapping)
}

// TestUpdateClient_RenameRebindsTheVerifiedTools pins the second job
// DiscoveredTools has. It is not only the "verification ran" marker: a per-call
// client's ToolMap is restored from it, keys and all. UpdateClient rebinds the
// live ToolMap to the new name on a rename, so a verification record carried
// forward under the old name would disagree with it, and the next restore
// (enable, after a disable) would bring the tools back under a name the client
// no longer has.
func TestUpdateClient_RenameRebindsTheVerifiedTools(t *testing.T) {
	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
	defer m.checkerManager.StopAll()

	lookup := schemas.ChatTool{Function: &schemas.ChatToolFunction{Name: "verifiedclient-lookup"}}
	config := newVerifiedClientConfig("client", schemas.MCPAuthTypePerUserHeaders,
		map[string]schemas.ChatTool{"verifiedclient-lookup": lookup})
	m.mu.Lock()
	m.clientMap[config.ID] = &schemas.MCPClientState{
		Name:            config.Name,
		ExecutionConfig: config,
		State:           schemas.MCPConnectionStateHealthy,
		ToolMap:         map[string]schemas.ChatTool{"verifiedclient-lookup": lookup},
		ToolNameMapping: map[string]string{},
		ConnectionInfo:  &schemas.MCPClientConnectionInfo{Type: config.ConnectionType},
	}
	m.mu.Unlock()

	edit := *config
	edit.Name = "renamedclient"
	edit.DiscoveredTools = nil
	edit.DiscoveredToolNameMapping = nil
	require.NoError(t, m.UpdateClient(config.ID, &edit))

	m.mu.RLock()
	updated := m.clientMap[config.ID].ExecutionConfig
	m.mu.RUnlock()
	require.Contains(t, updated.DiscoveredTools, "renamedclient-lookup", "the verification record follows the rename")
	assert.NotContains(t, updated.DiscoveredTools, "verifiedclient-lookup")
	assert.Equal(t, "renamedclient-lookup", updated.DiscoveredTools["renamedclient-lookup"].Function.Name)
	assert.Equal(t, "verifiedclient-lookup", lookup.Function.Name, "the tool the old config still points at is left as it was")

	// And the restore that reads it installs the tool under the current name.
	require.NoError(t, m.DisableClient(config.ID))
	require.NoError(t, m.EnableClient(config.ID))
	m.mu.RLock()
	toolMap := m.clientMap[config.ID].ToolMap
	m.mu.RUnlock()
	assert.Contains(t, toolMap, "renamedclient-lookup")
	assert.NotContains(t, toolMap, "verifiedclient-lookup")
}

// TestSetClientTools_RecordsTheVerificationOnTheConfig pins that a client
// verified while the process is running reads as verified while the process is
// running.
//
// SetClientTools is what the admin verification handlers call once the admin's
// credential has been checked against the upstream: it installs the tools,
// marks the client healthy, and fires the callback that persists those same
// tools to the client's row. That row is what makes the client verified after
// the next restart. Until then awaitsAdminVerification reads the manager's
// in-memory config, which SetClientTools left untouched, so a per_user_headers
// or token_exchange server verified a moment ago went on reading as never
// verified: healthy, serving tools, refused by every path that consults the
// predicate, and parked back in pending_verification by a disable and enable.
//
// Nothing automatic reaches SetClientTools for a client still awaiting its
// admin (the checker stays quiet for one and a refresh refuses it), so the only
// callers that ever move a pending client here are the admin's own.
func TestSetClientTools_RecordsTheVerificationOnTheConfig(t *testing.T) {
	cases := []struct {
		name     string
		authType schemas.MCPAuthType
		tools    map[string]schemas.ChatTool
		// wantNames is compared rather than the tools themselves: an installed tool
		// carries its cached serialization, which a literal never has.
		wantNames []string
	}{
		{
			name:      "per_user_headers",
			authType:  schemas.MCPAuthTypePerUserHeaders,
			tools:     map[string]schemas.ChatTool{"verifiedclient-lookup": {}},
			wantNames: []string{"verifiedclient-lookup"},
		},
		{
			name:      "token_exchange",
			authType:  schemas.MCPAuthTypeTokenExchange,
			tools:     map[string]schemas.ChatTool{"verifiedclient-lookup": {}},
			wantNames: []string{"verifiedclient-lookup"},
		},
		{
			// A verification that found nothing is still a verification. The
			// discriminator is nil-ness, so what is recorded has to be an
			// empty map, never nil.
			name:      "verified with zero tools",
			authType:  schemas.MCPAuthTypePerUserHeaders,
			tools:     nil,
			wantNames: []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
			defer m.checkerManager.StopAll()

			config := newVerifiedClientConfig("client", tc.authType, nil)
			config.DiscoveredToolNameMapping = nil
			require.True(t, awaitsAdminVerification(config), "fixture check: the client starts out awaiting its admin")
			m.mu.Lock()
			m.clientMap[config.ID] = &schemas.MCPClientState{
				Name:            config.Name,
				ExecutionConfig: config,
				State:           schemas.MCPConnectionStatePendingVerification,
				ToolMap:         map[string]schemas.ChatTool{},
				ToolNameMapping: map[string]string{},
				ConnectionInfo:  &schemas.MCPClientConnectionInfo{Type: config.ConnectionType},
			}
			m.mu.Unlock()

			// What the verify handlers do once the admin's credential checks out.
			m.SetClientTools(config.ID, tc.tools, map[string]string{"lookup": "lookup"}, "")

			m.mu.RLock()
			recorded := m.clientMap[config.ID].ExecutionConfig
			state := m.clientMap[config.ID].State
			m.mu.RUnlock()
			require.Equal(t, schemas.MCPConnectionStateHealthy, state, "fixture check: verification marks the client healthy")
			require.NotNil(t, recorded.DiscoveredTools, "a verification that just ran must be on the config the predicate reads")
			assert.Equal(t, tc.wantNames, slices.AppendSeq([]string{}, maps.Keys(recorded.DiscoveredTools)))
			assert.Equal(t, map[string]string{"lookup": "lookup"}, recorded.DiscoveredToolNameMapping)
			assert.False(t, awaitsAdminVerification(recorded), "a client verified in this process is verified in this process")

			// Recorded by swapping the config, never by writing into it: readers
			// may still hold the old pointer.
			assert.NotSame(t, config, recorded)
			assert.Nil(t, config.DiscoveredTools, "the config a concurrent reader may still hold is left as it was")

			// The symptom on this path: disable it, enable it, and it must come
			// back as the working client it was.
			require.NoError(t, m.DisableClient(config.ID))
			require.NoError(t, m.EnableClient(config.ID))
			m.mu.RLock()
			state = m.clientMap[config.ID].State
			m.mu.RUnlock()
			assert.NotEqual(t, schemas.MCPConnectionStatePendingVerification, state,
				"enabling a client verified moments ago must not send it back through admin verification")
		})
	}
}

// refreshToolMapKeys snapshots the prefixed tool names currently cached on a
// client, so a test can watch the manager's view of an upstream tool set
// without reaching into the connection.
func refreshToolMapKeys(m *MCPManager, id string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cs, ok := m.clientMap[id]
	if !ok || cs.ToolMap == nil {
		return nil
	}
	keys := make([]string, 0, len(cs.ToolMap))
	for k := range cs.ToolMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// buildRefreshMCPServer starts a streamable-HTTP MCP server serving a single
// "echo" tool. The returned addTool adds another one after the fact, standing
// in for an upstream server whose tool set changed while Bifrost was connected.
func buildRefreshMCPServer(t *testing.T) (ts *httptest.Server, addTool func(name, description string)) {
	t.Helper()

	s := server.NewMCPServer("refresh-tools", "1.0.0", server.WithToolCapabilities(true))
	echoTool := mcpgo.NewTool("echo",
		mcpgo.WithDescription("Echo tool"),
		mcpgo.WithString("message", mcpgo.Required(), mcpgo.Description("message")),
	)
	s.AddTool(echoTool, func(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		msg, _ := req.GetArguments()["message"].(string)
		return mcpgo.NewToolResultText(msg), nil
	})

	streamable := server.NewStreamableHTTPServer(s)
	ts = httptest.NewServer(http.HandlerFunc(streamable.ServeHTTP))
	t.Cleanup(func() {
		// A listening streamable-HTTP transport holds a GET open, and
		// httptest.Server.Close waits for outstanding requests.
		ts.CloseClientConnections()
		ts.Close()
	})

	return ts, func(name, description string) {
		s.AddTool(
			mcpgo.NewTool(name, mcpgo.WithDescription(description)),
			func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				return mcpgo.NewToolResultText("ok"), nil
			},
		)
	}
}

// newRefreshConfig builds an http client config. Leaving stickiness unset makes
// it per-call (the default for newly created clients); setting it true gives it
// a persistent connection.
func newRefreshConfig(id, name, url string, sticky bool) *schemas.MCPClientConfig {
	config := &schemas.MCPClientConfig{
		ID:               id,
		Name:             name,
		ConnectionType:   schemas.MCPConnectionTypeHTTP,
		AuthType:         schemas.MCPAuthTypeNone,
		ConnectionString: schemas.NewSecretVar(url),
		ToolsToExecute:   []string{"*"},
	}
	if sticky {
		config.NeedsSessionStickiness = schemas.Ptr(true)
	}
	return config
}

// TestRefreshClientTools_PerCallClient_RediscoversOnDemand is the regression
// test for #6885's third gap: a per-call client — which covers every per-user
// auth type as well as any shared http client running with
// needs_session_stickiness nil/false — had no way at all to pick up an upstream
// tool-set change short of the periodic checker's tick (up to 10 minutes) or a
// gateway restart. ReconnectClient does not apply to these clients by design:
// there is no persistent connection to re-establish.
func TestRefreshClientTools_PerCallClient_RediscoversOnDemand(t *testing.T) {
	ts, addTool := buildRefreshMCPServer(t)

	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
	t.Cleanup(func() { _ = m.Cleanup() })

	config := newRefreshConfig("refresh-percall", "percall", ts.URL, false)
	require.NoError(t, m.AddClient(context.Background(), config))
	require.True(t, m.RequiresPerCallConnection(config),
		"precondition: this client must be per-call for the test to exercise the gap")
	require.Equal(t, []string{"percall-echo"}, refreshToolMapKeys(m, config.ID),
		"precondition: the initial tool must be cached after AddClient")

	addTool("ping", "Ping tool added after the client was registered")

	count, err := m.RefreshClientTools(context.Background(), config.ID)
	require.NoError(t, err)
	require.Equal(t, 2, count, "refresh should report the freshly discovered tool count")
	require.Equal(t, []string{"percall-echo", "percall-ping"}, refreshToolMapKeys(m, config.ID),
		"refresh should have picked up the tool added upstream")
}

// TestRefreshClientTools_StickyClient_RelistsOverLiveConnection covers the
// sticky branch: the client already holds a live connection, so the refresh is
// a plain tools/list over it rather than an ephemeral connect-discover-close.
func TestRefreshClientTools_StickyClient_RelistsOverLiveConnection(t *testing.T) {
	ts, addTool := buildRefreshMCPServer(t)

	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
	t.Cleanup(func() { _ = m.Cleanup() })

	config := newRefreshConfig("refresh-sticky", "sticky", ts.URL, true)
	require.NoError(t, m.AddClient(context.Background(), config))
	require.False(t, m.RequiresPerCallConnection(config),
		"precondition: this client must be sticky for the test to exercise the live-connection branch")
	require.Equal(t, []string{"sticky-echo"}, refreshToolMapKeys(m, config.ID),
		"precondition: the initial tool must be cached after AddClient")

	addTool("ping", "Ping tool added after the client already connected")

	count, err := m.RefreshClientTools(context.Background(), config.ID)
	require.NoError(t, err)
	require.Equal(t, 2, count, "refresh should report the freshly discovered tool count")
	require.Equal(t, []string{"sticky-echo", "sticky-ping"}, refreshToolMapKeys(m, config.ID),
		"refresh should have picked up the tool added upstream")
}

// TestRefreshClientTools_FiresToolsChangeCallback pins the seam the transport
// layer persists through: a refresh that genuinely changes the tool set must
// reach the tools-change callback, which is what writes the new set to the DB
// and re-syncs the hosted /mcp surface. Gated on real change, exactly like
// every other discovery path.
func TestRefreshClientTools_FiresToolsChangeCallback(t *testing.T) {
	// Both discovery branches, because they reach the callback by different
	// routes: the per-call one through writeBackDiscoveredTools after an
	// ephemeral discovery, the sticky one through the same write-back after a
	// tools/list over the live connection.
	for _, sticky := range []bool{false, true} {
		name := "per_call"
		if sticky {
			name = "sticky"
		}
		t.Run(name, func(t *testing.T) {
			ts, addTool := buildRefreshMCPServer(t)

			m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
			t.Cleanup(func() { _ = m.Cleanup() })

			fired := make(chan map[string]schemas.ChatTool, 4)
			m.SetToolsChangeCallback(func(_, _ string, tools map[string]schemas.ChatTool, _ map[string]string, _ string) {
				fired <- tools
			})

			config := newRefreshConfig("refresh-callback-"+name, "cb", ts.URL, sticky)
			require.NoError(t, m.AddClient(context.Background(), config))
			drainToolsChange(fired)

			// A refresh that rediscovers the identical set is not a change,
			// so it must not churn the DB or the hosted /mcp surface.
			_, err := m.RefreshClientTools(context.Background(), config.ID)
			require.NoError(t, err)
			require.Empty(t, fired, "an unchanged rediscovery must not fire the tools-change callback")

			addTool("ping", "Ping tool added after the client was registered")
			_, err = m.RefreshClientTools(context.Background(), config.ID)
			require.NoError(t, err)

			require.Len(t, fired, 1, "a genuine tool-set change must fire the tools-change callback exactly once")
			require.Contains(t, <-fired, "cb-ping")
		})
	}
}

// TestRefreshClientTools_UnknownClient_Errors keeps the handler's 404 mapping honest.
func TestRefreshClientTools_UnknownClient_Errors(t *testing.T) {
	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
	t.Cleanup(func() { _ = m.Cleanup() })

	_, err := m.RefreshClientTools(context.Background(), "no-such-client")
	require.ErrorIs(t, err, schemas.ErrMCPClientNotFound)
}

// drainToolsChange empties any callback firings from client setup so a test
// only observes what its own refresh produced.
func drainToolsChange(ch chan map[string]schemas.ChatTool) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// TestRefreshClientTools_AwaitingAdminVerification_Refuses covers the case an
// on-demand refresh must not quietly resolve. A client parked in
// pending_verification is waiting on a human, and for token_exchange the
// refresh would not merely fail — it would succeed, because that auth type
// resolves its own client-credentials token with no admin involved. Since
// awaitsAdminVerification reads DiscoveredTools == nil as the pending signal
// there, persisting a discovered set would promote the client out of
// pending_verification permanently and drop the Verify CTA with it.
func TestRefreshClientTools_AwaitingAdminVerification_Refuses(t *testing.T) {
	cases := []struct {
		name     string
		authType schemas.MCPAuthType
		pending  func(*schemas.MCPClientConfig)
	}{
		{
			name:     "oauth_with_unauthorized_inline_config",
			authType: schemas.MCPAuthTypeOauth,
			pending:  func(c *schemas.MCPClientConfig) { c.PendingOAuthConfig = &schemas.OAuth2Config{} },
		},
		{
			name:     "per_user_oauth_with_unauthorized_inline_config",
			authType: schemas.MCPAuthTypePerUserOauth,
			pending:  func(c *schemas.MCPClientConfig) { c.PendingOAuthConfig = &schemas.OAuth2Config{} },
		},
		{
			name:     "token_exchange_never_verified",
			authType: schemas.MCPAuthTypeTokenExchange,
			pending:  func(c *schemas.MCPClientConfig) { c.DiscoveredTools = nil },
		},
		{
			name:     "per_user_headers_never_verified",
			authType: schemas.MCPAuthTypePerUserHeaders,
			pending:  func(c *schemas.MCPClientConfig) { c.DiscoveredTools = nil },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
			t.Cleanup(func() { m.checkerManager.StopAll() })

			config := &schemas.MCPClientConfig{
				ID:               "refresh-pending",
				Name:             "pending",
				AuthType:         tc.authType,
				ConnectionType:   schemas.MCPConnectionTypeHTTP,
				ConnectionString: schemas.NewSecretVar("http://127.0.0.1:0/mcp"),
			}
			tc.pending(config)
			require.True(t, awaitsAdminVerification(config),
				"precondition: this config must be one AddClient parks in pending_verification")

			m.mu.Lock()
			m.clientMap[config.ID] = &schemas.MCPClientState{
				Name:            config.Name,
				ExecutionConfig: config,
				State:           schemas.MCPConnectionStatePendingVerification,
				ToolMap:         map[string]schemas.ChatTool{},
				ToolNameMapping: map[string]string{},
				ConnectionInfo:  &schemas.MCPClientConnectionInfo{Type: config.ConnectionType},
			}
			m.mu.Unlock()

			_, err := m.RefreshClientTools(context.Background(), config.ID)
			require.ErrorIs(t, err, schemas.ErrMCPRefreshNotApplicable)

			m.mu.RLock()
			state := m.clientMap[config.ID].State
			tools := len(m.clientMap[config.ID].ToolMap)
			m.mu.RUnlock()
			assert.Equal(t, schemas.MCPConnectionStatePendingVerification, state,
				"a refused refresh must leave the client awaiting its admin")
			assert.Zero(t, tools, "nothing may be discovered onto a client still awaiting verification")
		})
	}
}

// TestRefreshClientTools_DisabledAndNeedsReauth_Refuse pins the other two
// states where discovery is meaningless, and that both are reported as the
// same not-applicable class so the handler can map them to 400 rather than
// surfacing them as discovery failures.
func TestRefreshClientTools_DisabledAndNeedsReauth_Refuse(t *testing.T) {
	for _, state := range []schemas.MCPConnectionState{
		schemas.MCPConnectionStateDisabled,
		schemas.MCPConnectionStateNeedsReauth,
	} {
		t.Run(string(state), func(t *testing.T) {
			m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
			t.Cleanup(func() { m.checkerManager.StopAll() })

			config := newRefreshConfig("refresh-"+string(state), "svc", "http://127.0.0.1:0/mcp", false)
			m.mu.Lock()
			m.clientMap[config.ID] = &schemas.MCPClientState{
				Name:            config.Name,
				ExecutionConfig: config,
				State:           state,
				ToolMap:         map[string]schemas.ChatTool{},
				ToolNameMapping: map[string]string{},
				ConnectionInfo:  &schemas.MCPClientConnectionInfo{Type: config.ConnectionType},
			}
			m.mu.Unlock()

			_, err := m.RefreshClientTools(context.Background(), config.ID)
			require.ErrorIs(t, err, schemas.ErrMCPRefreshNotApplicable)
		})
	}
}

// buildRefreshMCPServerWithHook is buildRefreshMCPServer plus a hook fired on
// every inbound tools/list, which lets a test mutate manager state at the exact
// moment a discovery is in flight.
func buildRefreshMCPServerWithHook(t *testing.T, onToolsList func()) (ts *httptest.Server, addTool func(name, description string)) {
	t.Helper()

	s := server.NewMCPServer("refresh-tools", "1.0.0", server.WithToolCapabilities(true))
	echoTool := mcpgo.NewTool("echo",
		mcpgo.WithDescription("Echo tool"),
		mcpgo.WithString("message", mcpgo.Required(), mcpgo.Description("message")),
	)
	s.AddTool(echoTool, func(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		msg, _ := req.GetArguments()["message"].(string)
		return mcpgo.NewToolResultText(msg), nil
	})

	streamable := server.NewStreamableHTTPServer(s)
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && onToolsList != nil {
			body, err := io.ReadAll(r.Body)
			if err == nil {
				r.Body = io.NopCloser(bytes.NewReader(body))
				if bytes.Contains(body, []byte(`"tools/list"`)) {
					onToolsList()
				}
			}
		}
		streamable.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})

	return ts, func(name, description string) {
		s.AddTool(
			mcpgo.NewTool(name, mcpgo.WithDescription(description)),
			func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				return mcpgo.NewToolResultText("ok"), nil
			},
		)
	}
}

// TestRefreshClientTools_StaleWriteBack_ReportsInstalledCount pins tool_count to
// what the client actually serves. writeBackDiscoveredTools drops a discovery
// whose ConnGeneration no longer matches — a reconnect swapped the connection
// while the tools/list was in flight — and leaves the existing ToolMap in place.
// Reporting the discarded discovery's length would then describe a tool set the
// client is not serving, contradicting the endpoint's documented contract.
func TestRefreshClientTools_StaleWriteBack_ReportsInstalledCount(t *testing.T) {
	var m *MCPManager
	const clientID = "refresh-stale-writeback"

	// Bump the generation from inside the in-flight tools/list, which is
	// exactly the window writeBackDiscoveredTools guards against.
	bumped := false
	ts, addTool := buildRefreshMCPServerWithHook(t, func() {
		if m == nil || bumped {
			return
		}
		m.mu.Lock()
		if cs, ok := m.clientMap[clientID]; ok {
			cs.ConnGeneration++
			bumped = true
		}
		m.mu.Unlock()
	})

	m = NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
	t.Cleanup(func() { _ = m.Cleanup() })

	config := newRefreshConfig(clientID, "stale", ts.URL, false)
	require.NoError(t, m.AddClient(context.Background(), config))
	require.Equal(t, []string{"stale-echo"}, refreshToolMapKeys(m, clientID),
		"precondition: the initial tool must be cached after AddClient")

	// The upstream now serves two tools, so a write-back that landed would
	// install 2 and a discarded one leaves the original 1 in place.
	addTool("ping", "Ping tool added after the client was registered")
	bumped = false

	count, err := m.RefreshClientTools(context.Background(), clientID)
	require.NoError(t, err)
	require.True(t, bumped, "precondition: the generation must have been bumped mid-discovery")

	installed := refreshToolMapKeys(m, clientID)
	require.Equal(t, []string{"stale-echo"}, installed,
		"precondition: a stale write-back must leave the existing tool map untouched")
	assert.Equal(t, len(installed), count,
		"tool_count must describe the tools the client actually serves, not a discarded discovery")
}

// TestRefreshClientTools_EditedVerifiedClient_StillRefreshes reproduces a
// reported refusal: "mcp client ... is awaiting admin verification, complete
// that instead of refreshing its tools", returned for a per_user_headers server
// the UI showed as healthy. The sequence that produced it was refresh (worked),
// edit the server, refresh again (refused, and refused from then on).
//
// The refusal reads awaitsAdminVerification off the manager's in-memory config,
// which for this auth type is "DiscoveredTools is nil". UpdateClient rebuilt
// that config without DiscoveredTools, so an edit left a verified client
// reading as never verified, while State and the tool map, which the edit does
// not rebuild, went on saying healthy. The fix lives in UpdateClient; this pins
// what it bought on the route that reported it.
func TestRefreshClientTools_EditedVerifiedClient_StillRefreshes(t *testing.T) {
	ts, addTool := buildRefreshMCPServer(t)

	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
	t.Cleanup(func() { _ = m.Cleanup() })
	// The admin's stored sample values, which per_user_headers discovery runs on.
	m.credStore = &fakeAdminCredStore{headers: http.Header{"X-Api-Key": []string{"admin-sample"}}}

	config := newRefreshConfig("refresh-edited", "edited", ts.URL, false)
	config.AuthType = schemas.MCPAuthTypePerUserHeaders
	config.PerUserHeaderKeys = []string{"X-Api-Key"}
	// Already verified: exactly what a restart loads for a server whose admin
	// verification ran in an earlier process.
	config.DiscoveredTools = map[string]schemas.ChatTool{"edited-echo": {}}
	config.DiscoveredToolNameMapping = map[string]string{}
	require.NoError(t, m.AddClient(context.Background(), config))

	_, err := m.RefreshClientTools(context.Background(), config.ID)
	require.NoError(t, err, "precondition: a verified client refreshes before it is edited")

	// An ordinary edit, shaped the way the update handler shapes it: the
	// editable fields, and no DiscoveredTools.
	edit := *config
	edit.DiscoveredTools = nil
	edit.DiscoveredToolNameMapping = nil
	edit.ToolsToAutoExecute = []string{"echo"}
	require.NoError(t, m.UpdateClient(config.ID, &edit))

	addTool("ping", "Ping tool added after the client was edited")

	count, err := m.RefreshClientTools(context.Background(), config.ID)
	require.NoError(t, err, "editing a verified client must not make it read as awaiting admin verification")
	require.Equal(t, 2, count)
	require.Equal(t, []string{"edited-echo", "edited-ping"}, refreshToolMapKeys(m, config.ID),
		"and the refresh picks up what changed upstream")
}

// mcpDialContext returns the DialContext buildTLSHTTPClient installed for
// config, so a test can exercise the dial-time policy without a transport.
func mcpDialContext(t *testing.T, config *schemas.MCPClientConfig) func(context.Context, string, string) (net.Conn, error) {
	t.Helper()
	httpClient, err := (&MCPManager{logger: &MockLogger{}}).buildTLSHTTPClient(config)
	require.NoError(t, err)
	switch tr := httpClient.Transport.(type) {
	case *http.Transport:
		return tr.DialContext
	case *network.ProxyAwareTransport:
		return tr.Direct.DialContext
	}
	t.Fatalf("unexpected transport %T", httpClient.Transport)
	return nil
}

// TestBuildTLSHTTPClientRequirePublicTargetRefusesPrivateAddresses pins the
// dial-time half of the unauthenticated-registration restriction: a client
// flagged RequirePublicTarget must refuse loopback and RFC1918 destinations on
// every dial, so a name that resolved publicly at registration and privately
// afterwards is still never connected to. An unflagged client keeps the
// documented ability to reach a local MCP server.
func TestBuildTLSHTTPClientRequirePublicTargetRefusesPrivateAddresses(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer local.Close()
	localAddr := local.Listener.Addr().String()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	strict := mcpDialContext(t, &schemas.MCPClientConfig{Name: "unauthenticated", RequirePublicTarget: true})
	for _, addr := range []string{localAddr, "10.0.0.1:443"} {
		conn, err := strict(ctx, "tcp", addr)
		if conn != nil {
			require.NoError(t, conn.Close())
		}
		require.Error(t, err, "flagged client dialed %s", addr)
		require.Contains(t, err.Error(), "blocked connection to non-public address", "dial to %s", addr)
	}

	permissive := mcpDialContext(t, &schemas.MCPClientConfig{Name: "admin"})
	conn, err := permissive(ctx, "tcp", localAddr)
	require.NoError(t, err, "an unflagged client must still reach a loopback MCP server")
	require.NoError(t, conn.Close())
}

// TestMCPProxySelectorRequirePublicTargetRefusesPrivateLiteral pins the
// proxied path for a flagged client: the dialer only sees the proxy there, so
// a private IP-literal destination has to be refused by the selector.
func TestMCPProxySelectorRequirePublicTargetRefusesPrivateLiteral(t *testing.T) {
	proxyURL, err := url.Parse("http://proxy.example:3128")
	require.NoError(t, err)
	next := func(*http.Request) (*url.URL, error) { return proxyURL, nil }

	req, err := http.NewRequest(http.MethodGet, "http://10.0.0.1/mcp", nil)
	require.NoError(t, err)

	_, err = mcpProxySelector(next, true, net.DefaultResolver)(req)
	require.Error(t, err)
	require.Contains(t, err.Error(), "blocked connection to non-public address")

	got, err := mcpProxySelector(next, false, net.DefaultResolver)(req)
	require.NoError(t, err, "an unflagged client may reach a private literal through the proxy")
	require.Equal(t, proxyURL, got)
}

// SetClientTools carries the instructions from the same handshake that found the tools,
// and records them on the config so a per-call client can restore them after a restart.
func TestSetClientToolsRecordsInstructionsForRestart(t *testing.T) {
	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
	config := &schemas.MCPClientConfig{ID: "per-call", Name: "per-call", ToolsToExecute: []string{"*"}}
	m.mu.Lock()
	m.clientMap[config.ID] = &schemas.MCPClientState{Name: config.Name, ExecutionConfig: config, ToolMap: map[string]schemas.ChatTool{}}
	m.mu.Unlock()

	m.SetClientTools(config.ID, map[string]schemas.ChatTool{"t": {Type: "function"}}, map[string]string{"t": "t"}, "Use me carefully.")

	m.mu.RLock()
	defer m.mu.RUnlock()
	assert.Equal(t, "Use me carefully.", m.clientMap[config.ID].ServerInstructions)
	assert.Equal(t, "Use me carefully.", m.clientMap[config.ID].ExecutionConfig.DiscoveredInstructions)
}

// Instructions ride the change hash, so a server that rewrote only its instructions still
// counts as a change. Without this the callback never fires and the new text is never
// persisted or re-served.
func TestInstructionsOnlyChangeStillFiresToolsChangeCallback(t *testing.T) {
	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
	config := &schemas.MCPClientConfig{ID: "c", Name: "c"}
	m.mu.Lock()
	m.clientMap[config.ID] = &schemas.MCPClientState{Name: config.Name, ExecutionConfig: config, ToolMap: map[string]schemas.ChatTool{}}
	m.mu.Unlock()

	var seen []string
	m.SetToolsChangeCallback(func(_, _ string, _ map[string]schemas.ChatTool, _ map[string]string, instructions string) {
		seen = append(seen, instructions)
	})

	tools := map[string]schemas.ChatTool{"echo": {Type: "function"}}
	mapping := map[string]string{"echo": "echo"}
	m.SetClientTools(config.ID, tools, mapping, "v1")
	m.SetClientTools(config.ID, map[string]schemas.ChatTool{"echo": {Type: "function"}}, map[string]string{"echo": "echo"}, "v2")

	assert.Equal(t, []string{"v1", "v2"}, seen)
}

// A tools-only refresh (tools/list over a live connection) learns nothing about
// instructions and must not clear the installed value.
func TestWriteBackDiscoveredToolsKeepsInstructionsWhenNotRediscovered(t *testing.T) {
	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
	config := &schemas.MCPClientConfig{ID: "c", Name: "c"}
	m.mu.Lock()
	m.clientMap[config.ID] = &schemas.MCPClientState{
		Name: config.Name, ExecutionConfig: config,
		ToolMap: map[string]schemas.ChatTool{}, ServerInstructions: "keep me",
	}
	m.mu.Unlock()

	require.True(t, m.writeBackDiscoveredTools(config.ID, 0, map[string]schemas.ChatTool{"a": {Type: "function"}}, map[string]string{"a": "a"}, nil))

	m.mu.RLock()
	defer m.mu.RUnlock()
	assert.Equal(t, "keep me", m.clientMap[config.ID].ServerInstructions)
}

// stubResolver answers every lookup with a fixed IP set or error.
type stubResolver struct {
	ips []net.IP
	err error
}

func (r stubResolver) LookupIP(context.Context, string, string) ([]net.IP, error) {
	return r.ips, r.err
}

// TestBuildTLSHTTPClientRequirePublicTargetReachesPrivateProxy: the strict
// policy is about the MCP destination, not the proxy that carries the
// connection. A flagged client in a proxy-only deployment must still be able
// to dial the operator's proxy on a private or loopback address, while a dial
// to the same kind of address that is NOT the selected proxy stays refused.
func TestBuildTLSHTTPClientRequirePublicTargetReachesPrivateProxy(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer proxy.Close()
	proxyURL, err := url.Parse(proxy.URL)
	require.NoError(t, err)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer other.Close()

	// mcp.example resolves to a public address through the stub, which is what lets the
	// strict proxied-path check admit it; the proxy itself is dialed by IP.
	httpClient, err := (&MCPManager{logger: &MockLogger{}}).buildTLSHTTPClientWithProxyAndResolver(
		&schemas.MCPClientConfig{Name: "unauthenticated", RequirePublicTarget: true},
		func(*http.Request) (*url.URL, error) { return proxyURL, nil },
		stubResolver{ips: []net.IP{net.ParseIP("93.184.216.34")}},
	)
	require.NoError(t, err)
	tr, ok := httpClient.Transport.(*network.ProxyAwareTransport)
	require.True(t, ok, "a flagged client routes per request between a direct and a via-proxy transport")

	req, err := http.NewRequest(http.MethodGet, "https://mcp.example/mcp", nil)
	require.NoError(t, err)
	got, err := tr.ViaProxy.Proxy(req)
	require.NoError(t, err)
	require.Equal(t, proxyURL, got)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := tr.ViaProxy.DialContext(ctx, "tcp", proxy.Listener.Addr().String())
	require.NoError(t, err, "the selected proxy must be dialable even on a loopback address")
	require.NoError(t, conn.Close())

	conn, err = tr.Direct.DialContext(ctx, "tcp", other.Listener.Addr().String())
	if conn != nil {
		require.NoError(t, conn.Close())
	}
	require.Error(t, err, "a loopback address that is not the selected proxy stays refused")
	require.Contains(t, err.Error(), "blocked connection to non-public address")
}

// TestMCPProxySelectorAppliesPolicyToHostnames: on the proxied path the
// selector is the only place that sees the MCP destination, so a hostname is
// held to the same policy as a literal, against what the local resolver
// answers. A name only the proxy can resolve is refused (TXE-5086).
func TestMCPProxySelectorAppliesPolicyToHostnames(t *testing.T) {
	proxyURL, err := url.Parse("http://proxy.example:3128")
	require.NoError(t, err)
	next := func(*http.Request) (*url.URL, error) { return proxyURL, nil }
	cases := []struct {
		name     string
		strict   bool
		resolver stubResolver
		wantErr  string
	}{
		{"strict refuses a hostname resolving to a private address", true, stubResolver{ips: []net.IP{net.ParseIP("10.0.0.5")}}, "blocked connection to non-public address"},
		{"strict refuses a hostname resolving to loopback", true, stubResolver{ips: []net.IP{net.ParseIP("127.0.0.1")}}, "blocked connection to non-public address"},
		{"strict proxies a hostname resolving to a public address", true, stubResolver{ips: []net.IP{net.ParseIP("93.184.216.34")}}, ""},
		{"strict refuses an unresolvable hostname rather than trusting the proxy", true, stubResolver{err: errors.New("no such host")}, "DNS lookup failed"},
		{"unflagged refuses an unresolvable hostname rather than trusting the proxy (TXE-5086)", false, stubResolver{err: errors.New("no such host")}, "DNS lookup failed"},
		{"unflagged proxies a hostname resolving to a private address", false, stubResolver{ips: []net.IP{net.ParseIP("10.0.0.5")}}, ""},
		{"unflagged refuses a hostname resolving to metadata", false, stubResolver{ips: []net.IP{net.ParseIP("169.254.169.254")}}, "link-local"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, "http://mcp.corp/mcp", nil)
			require.NoError(t, err)
			got, err := mcpProxySelector(next, tc.strict, tc.resolver)(req)
			if tc.wantErr == "" {
				require.NoError(t, err)
				require.Equal(t, proxyURL, got)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestBuildTLSHTTPClientRequirePublicTargetProxyClassificationIsPerRequest: which policy a dial
// gets must follow the request's own routing decision, not an address seen earlier. After a
// proxied request has used a loopback proxy, a direct request (no proxy selected for it) whose
// destination is that very address is still a destination dial and must be refused by the
// strict policy.
func TestBuildTLSHTTPClientRequirePublicTargetProxyClassificationIsPerRequest(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Served-By", "proxy")
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()
	proxyURL, err := url.Parse(proxy.URL)
	require.NoError(t, err)

	// Only requests for mcp.example go through the proxy; everything else is direct.
	selector := func(req *http.Request) (*url.URL, error) {
		if req.URL.Hostname() == "mcp.example" {
			return proxyURL, nil
		}
		return nil, nil
	}
	httpClient, err := (&MCPManager{logger: &MockLogger{}}).buildTLSHTTPClientWithProxyAndResolver(
		&schemas.MCPClientConfig{Name: "unauthenticated", RequirePublicTarget: true}, selector,
		stubResolver{ips: []net.IP{net.ParseIP("93.184.216.34")}})
	require.NoError(t, err)

	resp, err := httpClient.Get("http://mcp.example/mcp")
	require.NoError(t, err, "the proxied request must reach the loopback proxy")
	require.Equal(t, "proxy", resp.Header.Get("X-Served-By"))
	resp.Body.Close()

	resp, err = httpClient.Get(proxy.URL + "/mcp")
	if resp != nil {
		resp.Body.Close()
	}
	require.Error(t, err, "a direct request to the proxy's own address is a loopback destination and must be refused")
	require.Contains(t, err.Error(), "blocked connection to non-public address")
}

// TestBuildTLSHTTPClientUsesGlobalProxy pins that MCP client transports honour the
// global proxy when it is enabled for API traffic, while the destination guard still
// refuses instance metadata without anything reaching the proxy. They used to follow
// only HTTP_PROXY / HTTPS_PROXY.
func TestBuildTLSHTTPClientUsesGlobalProxy(t *testing.T) {
	set := proxytest.NewSet(t)
	network.SetDefaultHTTPClientFactory(network.NewHTTPClientFactory(&network.GlobalProxyConfig{
		Enabled: true, Type: network.GlobalProxyTypeHTTP, URL: "http://127.0.0.1:" + set.Config.Port(), EnableForAPI: true,
	}, nil))
	t.Cleanup(func() { network.SetDefaultHTTPClientFactory(nil) })

	// A proxied name must resolve on the gateway (TXE-5086), and .test never does in real
	// DNS, so the name is answered here; the proxy selector is the production global one.
	httpClient, err := (&MCPManager{logger: defaultLogger}).buildTLSHTTPClientWithProxyAndResolver(
		&schemas.MCPClientConfig{Name: "proxied"},
		network.DefaultProxyFunc(network.ClientPurposeAPI),
		stubResolver{ips: []net.IP{net.ParseIP("93.184.216.34")}},
	)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://mcp.bifrost.test/sse", nil)
	if resp, err := httpClient.Do(req); err == nil {
		resp.Body.Close()
	}
	proxytest.AssertRoute(t, set, proxytest.Route{Proxy: "config"}, "mcp.bifrost.test:443", nil)

	// MCP allows private networks (local MCP servers) but never instance metadata.
	set.Reset()
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, "https://169.254.169.254/sse", nil)
	_, err = httpClient.Do(req)
	require.Error(t, err, "a metadata address must be refused")
	proxytest.AssertRoute(t, set, proxytest.Direct, "", nil)
}

// ---------------------------------------------------------------------------
// OpenAPI connection type: synthesized in-process servers via the factory hook
// ---------------------------------------------------------------------------

// stubOpenAPIFactory stands in for framework/openapimcp.Factory: it builds one
// tool per comma-separated name found in openapi_config.spec and records what
// core handed it. Each tool echoes the headers the resolver produced so the
// layering core owns (static config headers, credentials, allow-listed extras)
// is observable from the tool result.
type stubOpenAPIFactory struct {
	calls         atomic.Int32
	sawClient     atomic.Bool
	toolsOverride atomic.Pointer[string] // when set, used instead of openapi_config.spec (a changed remote document)
}

func (f *stubOpenAPIFactory) factory() schemas.InProcessServerFactory {
	return func(_ context.Context, config *schemas.MCPClientConfig, deps schemas.InProcessServerDeps) (*server.MCPServer, error) {
		f.calls.Add(1)
		f.sawClient.Store(deps.HTTPClient != nil)
		if strings.Contains(config.OpenAPIConfig.Spec, "fail") {
			return nil, fmt.Errorf("%w: stub factory refused spec %q", schemas.ErrMCPOpenAPIConfigInvalid, config.OpenAPIConfig.Spec)
		}
		spec := config.OpenAPIConfig.Spec
		if override := f.toolsOverride.Load(); override != nil {
			spec = *override
		}
		s := server.NewMCPServer("stub-openapi", "1.0.0", server.WithToolCapabilities(false))
		for _, name := range strings.Split(spec, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			s.AddTool(
				mcpgo.NewToolWithRawSchema(name, "stub "+name, json.RawMessage(`{"type":"object","properties":{}}`)),
				func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
					headers, err := deps.Headers(ctx)
					if err != nil {
						return mcpgo.NewToolResultError(err.Error()), nil
					}
					keys := make([]string, 0, len(headers))
					for k := range headers {
						keys = append(keys, k)
					}
					sort.Strings(keys)
					var b strings.Builder
					for _, k := range keys {
						fmt.Fprintf(&b, "%s=%s;", k, strings.Join(headers[k], ","))
					}
					return mcpgo.NewToolResultText(b.String()), nil
				},
			)
		}
		return s, nil
	}
}

func newOpenAPITestConfig(name, spec string) *schemas.MCPClientConfig {
	return &schemas.MCPClientConfig{
		ID:                  name + "-id",
		Name:                name,
		ConnectionType:      schemas.MCPConnectionTypeOpenAPI,
		AuthType:            schemas.MCPAuthTypeHeaders,
		Headers:             map[string]schemas.SecretVar{"X-Static": *schemas.NewSecretVar("static-value"), "Authorization": *schemas.NewSecretVar("Bearer shared-token")},
		AllowedExtraHeaders: schemas.WhiteList{"X-Trace"},
		ToolsToExecute:      schemas.WhiteList{"*"},
		OpenAPIConfig:       &schemas.MCPOpenAPIConfig{Spec: spec},
	}
}

func openAPIToolNames(t *testing.T, m *MCPManager, id string) []string {
	t.Helper()
	m.mu.RLock()
	defer m.mu.RUnlock()
	state, ok := m.clientMap[id]
	require.True(t, ok, "client %s not registered", id)
	names := make([]string, 0, len(state.ToolMap))
	for name := range state.ToolMap {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestCreateOpenAPIConnection_RequiresFactory: without a registered factory
// the connect must fail with an error that names the missing hook, so a Go SDK
// user who declares connection_type openapi learns what to wire rather than
// seeing a generic connect failure.
func TestCreateOpenAPIConnection_RequiresFactory(t *testing.T) {
	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
	_, _, err := m.createOpenAPIConnection(context.Background(), newOpenAPITestConfig("nofactory", "echo"), nil)
	require.ErrorContains(t, err, "InProcessServerFactory")

	_, _, err = m.createOpenAPIConnection(context.Background(), &schemas.MCPClientConfig{Name: "noconfig", ConnectionType: schemas.MCPConnectionTypeOpenAPI}, nil)
	require.ErrorContains(t, err, "openapi_config")
}

// TestAddClient_OpenAPI_SynthesizesServerAndLayersHeaders drives the full path
// an openapi client takes: AddClient -> connectToMCPClient -> factory ->
// in-process client -> tool discovery with the client-name prefix -> tool
// execution with the header resolver core built. The factory must receive a
// guarded HTTP client, and the resolver must layer static config headers, the
// shared credential and the caller's allow-listed extras (and nothing else).
func TestAddClient_OpenAPI_SynthesizesServerAndLayersHeaders(t *testing.T) {
	stub := &stubOpenAPIFactory{}
	m := NewMCPManager(context.Background(), schemas.MCPConfig{InProcessServerFactory: stub.factory()}, nil, &MockLogger{}, nil)
	config := newOpenAPITestConfig("petstore", "listPets, getPet")

	require.NoError(t, m.AddClient(context.Background(), config))
	t.Cleanup(func() { _ = m.RemoveClient(config.ID) })

	assert.Equal(t, int32(1), stub.calls.Load(), "factory builds the server exactly once per connect")
	assert.True(t, stub.sawClient.Load(), "core must hand the factory a configured HTTP client")
	assert.Equal(t, []string{"petstore-getPet", "petstore-listPets"}, openAPIToolNames(t, m, config.ID))

	m.mu.RLock()
	state := m.clientMap[config.ID]
	m.mu.RUnlock()
	assert.Equal(t, schemas.MCPConnectionStateHealthy, state.State)
	assert.NotNil(t, state.Conn, "openapi clients hold one sticky in-process connection")

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyMCPExtraHeaders, map[string][]string{
		"X-Trace":  {"trace-123"},
		"X-Denied": {"must-not-pass"},
	})
	name := "petstore-listPets"
	callID := "call-1"
	msg, bifrostErr := m.ExecuteChatTool(ctx, &schemas.ChatAssistantMessageToolCall{
		ID:       &callID,
		Function: schemas.ChatAssistantMessageToolCallFunction{Name: &name, Arguments: "{}"},
	})
	require.Nil(t, bifrostErr, "tool call failed: %+v", bifrostErr)
	require.NotNil(t, msg)
	require.NotNil(t, msg.Content)
	require.NotNil(t, msg.Content.ContentStr)
	got := *msg.Content.ContentStr
	assert.Contains(t, got, "X-Static=static-value;", "static config headers reach the upstream")
	assert.Contains(t, got, "Authorization=Bearer shared-token;", "the shared credential is resolved through the credential store")
	assert.Contains(t, got, "X-Trace=trace-123;", "allow-listed per-request extras are forwarded")
	assert.NotContains(t, got, "X-Denied", "extras outside allowed_extra_headers never reach the upstream")
}

// TestUpdateClient_OpenAPI_SpecChangeRebuildsServer: a changed openapi_config
// must rebuild the synthesized server (factory called again, connection
// generation bumped, new tools discovered), while an update that leaves it
// untouched (or omits it, PATCH-style) must not.
func TestUpdateClient_OpenAPI_SpecChangeRebuildsServer(t *testing.T) {
	stub := &stubOpenAPIFactory{}
	m := NewMCPManager(context.Background(), schemas.MCPConfig{InProcessServerFactory: stub.factory()}, nil, &MockLogger{}, nil)
	config := newOpenAPITestConfig("petstore", "listPets")
	require.NoError(t, m.AddClient(context.Background(), config))
	t.Cleanup(func() { _ = m.RemoveClient(config.ID) })

	var toolsChanges atomic.Int32
	m.SetToolsChangeCallback(func(string, string, map[string]schemas.ChatTool, map[string]string, string) { toolsChanges.Add(1) })

	m.mu.RLock()
	genBefore := m.clientMap[config.ID].ConnGeneration
	m.mu.RUnlock()

	// Omitting openapi_config keeps the stored one: no rebuild.
	unchanged := newOpenAPITestConfig("petstore", "")
	unchanged.OpenAPIConfig = nil
	unchanged.ToolExecutionTimeout = 5 * time.Second
	require.NoError(t, m.UpdateClient(config.ID, unchanged))
	assert.Equal(t, int32(1), stub.calls.Load(), "an update without openapi_config must not rebuild")
	m.mu.RLock()
	assert.Equal(t, "listPets", m.clientMap[config.ID].ExecutionConfig.OpenAPIConfig.Spec, "stored openapi_config carried forward")
	assert.Equal(t, 5*time.Second, m.clientMap[config.ID].ExecutionConfig.ToolExecutionTimeout)
	m.mu.RUnlock()

	// Same spec sent again: still no rebuild.
	require.NoError(t, m.UpdateClient(config.ID, newOpenAPITestConfig("petstore", "listPets")))
	assert.Equal(t, int32(1), stub.calls.Load(), "an identical openapi_config must not rebuild")

	// Changed spec: rebuild, fresh generation, new tool set.
	require.NoError(t, m.UpdateClient(config.ID, newOpenAPITestConfig("petstore", "listPets, createPet")))
	assert.Equal(t, int32(2), stub.calls.Load(), "a changed spec rebuilds the server")
	assert.Equal(t, []string{"petstore-createPet", "petstore-listPets"}, openAPIToolNames(t, m, config.ID))
	m.mu.RLock()
	assert.Greater(t, m.clientMap[config.ID].ConnGeneration, genBefore, "the rebuild is a genuinely new connection")
	assert.Equal(t, schemas.MCPConnectionStateHealthy, m.clientMap[config.ID].State)
	m.mu.RUnlock()
	assert.GreaterOrEqual(t, toolsChanges.Load(), int32(1), "rediscovered tools must be offered for persistence")

	// A rebuild that fails leaves the client Unstable with the new config, and
	// reports the failure to the caller. The spec error is permanent, so it must
	// not sit through the connect retry schedule (~31s) before surfacing.
	start := time.Now()
	err := m.UpdateClient(config.ID, newOpenAPITestConfig("petstore", "fail"))
	require.ErrorIs(t, err, ErrMCPSharedConnectFailedAfterUpdate)
	// The connect plugin gate flattens the dial error into a BifrostError, so the
	// sentinel is not in the chain here; the message and the timing are the pins.
	require.ErrorContains(t, err, "cannot produce a server")
	assert.Less(t, time.Since(start), 5*time.Second, "permanent synthesis errors must fail fast, not retry")
	m.mu.RLock()
	assert.Equal(t, schemas.MCPConnectionStateUnstable, m.clientMap[config.ID].State)
	m.mu.RUnlock()
}

// TestUpdateClient_OpenAPI_RejectsInvalidReplacementConfig: a replacement
// openapi_config is validated before it is stored. Otherwise a bad edit from a
// Go SDK caller closes the working connection first and the factory rejects it
// afterwards, leaving the client disconnected with the bad config saved.
func TestUpdateClient_OpenAPI_RejectsInvalidReplacementConfig(t *testing.T) {
	stub := &stubOpenAPIFactory{}
	m := NewMCPManager(context.Background(), schemas.MCPConfig{InProcessServerFactory: stub.factory()}, nil, &MockLogger{}, nil)
	config := newOpenAPITestConfig("petstore", "listPets")
	require.NoError(t, m.AddClient(context.Background(), config))
	t.Cleanup(func() { _ = m.RemoveClient(config.ID) })

	relative := "/v1"
	for name, cfg := range map[string]*schemas.MCPOpenAPIConfig{
		"no spec source":     {MaxResponseBytes: 1},
		"relative base url":  {Spec: "listPets, createPet", BaseURL: &relative},
		"negative max bytes": {Spec: "listPets, createPet", MaxResponseBytes: -1},
	} {
		update := newOpenAPITestConfig("petstore", "")
		update.OpenAPIConfig = cfg
		require.Error(t, m.UpdateClient(config.ID, update), name)
	}
	assert.Equal(t, int32(1), stub.calls.Load(), "an invalid replacement must not rebuild the server")
	m.mu.RLock()
	assert.Equal(t, schemas.MCPConnectionStateHealthy, m.clientMap[config.ID].State)
	assert.Equal(t, "listPets", m.clientMap[config.ID].ExecutionConfig.OpenAPIConfig.Spec, "the stored config is kept")
	m.mu.RUnlock()
}

// TestUpdateClient_OpenAPI_ConfigMutatedInPlaceStillRebuilds: the manager keeps
// its own copy of openapi_config, so a caller that edits the struct it passed
// earlier and sends it again still triggers a rebuild.
func TestUpdateClient_OpenAPI_ConfigMutatedInPlaceStillRebuilds(t *testing.T) {
	stub := &stubOpenAPIFactory{}
	m := NewMCPManager(context.Background(), schemas.MCPConfig{InProcessServerFactory: stub.factory()}, nil, &MockLogger{}, nil)
	config := newOpenAPITestConfig("petstore", "listPets")
	require.NoError(t, m.AddClient(context.Background(), config))
	t.Cleanup(func() { _ = m.RemoveClient(config.ID) })

	config.OpenAPIConfig.Spec = "listPets, createPet"
	require.NoError(t, m.UpdateClient(config.ID, config))
	assert.Equal(t, int32(2), stub.calls.Load(), "a spec edited on the struct passed to AddClient must rebuild")

	config.OpenAPIConfig.Spec = "listPets, createPet, deletePet"
	require.NoError(t, m.UpdateClient(config.ID, config))
	assert.Equal(t, int32(3), stub.calls.Load(), "a spec edited on the struct passed to UpdateClient must rebuild")
	assert.Equal(t, []string{"petstore-createPet", "petstore-deletePet", "petstore-listPets"}, openAPIToolNames(t, m, config.ID))
}

// TestUpdateClient_OpenAPI_ConnectionInputsMutatedInPlaceRebuild: headers,
// the extras allow-list and TLS settings are baked into the synthesized
// server, so the manager keeps its own copies; editing the caller's values in
// place and sending them again must still rebuild.
func TestUpdateClient_OpenAPI_ConnectionInputsMutatedInPlaceRebuild(t *testing.T) {
	stub := &stubOpenAPIFactory{}
	m := NewMCPManager(context.Background(), schemas.MCPConfig{InProcessServerFactory: stub.factory()}, nil, &MockLogger{}, nil)
	config := newOpenAPITestConfig("petstore", "listPets")
	require.NoError(t, m.AddClient(context.Background(), config))
	t.Cleanup(func() { _ = m.RemoveClient(config.ID) })

	config.Headers["X-Static"] = *schemas.NewSecretVar("edited")
	require.NoError(t, m.UpdateClient(config.ID, config))
	assert.Equal(t, int32(2), stub.calls.Load(), "a header edited on the map passed to AddClient must rebuild")

	config.AllowedExtraHeaders[0] = "X-Other"
	require.NoError(t, m.UpdateClient(config.ID, config))
	assert.Equal(t, int32(3), stub.calls.Load(), "an allow-list edited in place must rebuild")

	config.TLSConfig = &schemas.MCPTLSConfig{}
	require.NoError(t, m.UpdateClient(config.ID, config))
	assert.Equal(t, int32(4), stub.calls.Load(), "adding TLS settings rebuilds")
	config.TLSConfig.InsecureSkipVerify = true
	require.NoError(t, m.UpdateClient(config.ID, config))
	assert.Equal(t, int32(5), stub.calls.Load(), "TLS settings edited on the struct passed to UpdateClient must rebuild")
}

// TestUpdateClient_RequirePublicTargetOnlyTightens: an update can turn the
// public-target restriction on (an unauthenticated caller changed where the
// client connects) but never off.
func TestUpdateClient_RequirePublicTargetOnlyTightens(t *testing.T) {
	stub := &stubOpenAPIFactory{}
	m := NewMCPManager(context.Background(), schemas.MCPConfig{InProcessServerFactory: stub.factory()}, nil, &MockLogger{}, nil)
	config := newOpenAPITestConfig("petstore", "listPets")
	require.NoError(t, m.AddClient(context.Background(), config))
	t.Cleanup(func() { _ = m.RemoveClient(config.ID) })

	tightened := newOpenAPITestConfig("petstore", "listPets")
	tightened.RequirePublicTarget = true
	require.NoError(t, m.UpdateClient(config.ID, tightened))
	m.mu.RLock()
	assert.True(t, m.clientMap[config.ID].ExecutionConfig.RequirePublicTarget, "an update may turn the restriction on")
	m.mu.RUnlock()
	assert.Equal(t, int32(2), stub.calls.Load(), "turning it on rebuilds the server, so its HTTP client gets the strict dialer")

	require.NoError(t, m.UpdateClient(config.ID, newOpenAPITestConfig("petstore", "listPets")))
	m.mu.RLock()
	assert.True(t, m.clientMap[config.ID].ExecutionConfig.RequirePublicTarget, "an update never turns it off")
	m.mu.RUnlock()
}

// TestUpdateClient_OpenAPI_RenameRebuildsServer: synthesized tool names are
// shortened to fit beside the client name, so a rename rebuilds the server
// rather than only swapping the prefix on the old names.
func TestUpdateClient_OpenAPI_RenameRebuildsServer(t *testing.T) {
	stub := &stubOpenAPIFactory{}
	m := NewMCPManager(context.Background(), schemas.MCPConfig{InProcessServerFactory: stub.factory()}, nil, &MockLogger{}, nil)
	config := newOpenAPITestConfig("petstore", "listPets")
	require.NoError(t, m.AddClient(context.Background(), config))
	t.Cleanup(func() { _ = m.RemoveClient(config.ID) })

	require.NoError(t, m.UpdateClient(config.ID, newOpenAPITestConfig("zoo", "listPets")))
	assert.Equal(t, int32(2), stub.calls.Load(), "a rename must rebuild the synthesized server")
	assert.Equal(t, []string{"zoo-listPets"}, openAPIToolNames(t, m, config.ID))
}

// callOpenAPIStubTool runs one tool call through the manager and returns the
// header echo the stub factory's handler produced.
func callOpenAPIStubTool(t *testing.T, m *MCPManager, toolName string) string {
	t.Helper()
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	callID := "call-" + toolName
	msg, bifrostErr := m.ExecuteChatTool(ctx, &schemas.ChatAssistantMessageToolCall{
		ID:       &callID,
		Function: schemas.ChatAssistantMessageToolCallFunction{Name: &toolName, Arguments: "{}"},
	})
	require.Nil(t, bifrostErr, "tool call failed: %+v", bifrostErr)
	require.NotNil(t, msg)
	require.NotNil(t, msg.Content)
	require.NotNil(t, msg.Content.ContentStr)
	return *msg.Content.ContentStr
}

// TestUpdateClient_OpenAPI_HeaderEditRebuildsServer: the synthesized server's
// header resolver and guarded HTTP client are built from the config at connect
// time, so an edit to the static headers, the extras allow-list, the per-user
// keys or TLS must rebuild it even when openapi_config itself is untouched.
// Otherwise the saved edit never reaches the upstream.
func TestUpdateClient_OpenAPI_HeaderEditRebuildsServer(t *testing.T) {
	stub := &stubOpenAPIFactory{}
	m := NewMCPManager(context.Background(), schemas.MCPConfig{InProcessServerFactory: stub.factory()}, nil, &MockLogger{}, nil)
	config := newOpenAPITestConfig("petstore", "listPets")
	require.NoError(t, m.AddClient(context.Background(), config))
	t.Cleanup(func() { _ = m.RemoveClient(config.ID) })
	require.Contains(t, callOpenAPIStubTool(t, m, "petstore-listPets"), "X-Static=static-value;")

	edited := newOpenAPITestConfig("petstore", "listPets")
	edited.Headers["X-Static"] = *schemas.NewSecretVar("edited-value")
	require.NoError(t, m.UpdateClient(config.ID, edited))
	assert.Equal(t, int32(2), stub.calls.Load(), "a static header edit rebuilds the server")
	assert.Contains(t, callOpenAPIStubTool(t, m, "petstore-listPets"), "X-Static=edited-value;", "the edited header reaches the upstream")

	allowList := newOpenAPITestConfig("petstore", "listPets")
	allowList.Headers["X-Static"] = *schemas.NewSecretVar("edited-value")
	allowList.AllowedExtraHeaders = schemas.WhiteList{"X-Trace", "X-Other"}
	require.NoError(t, m.UpdateClient(config.ID, allowList))
	assert.Equal(t, int32(3), stub.calls.Load(), "an allowed_extra_headers edit rebuilds the server")

	// Same headers again: no rebuild.
	require.NoError(t, m.UpdateClient(config.ID, allowList))
	assert.Equal(t, int32(3), stub.calls.Load(), "an update with unchanged headers must not rebuild")
}

// TestUpdateClient_OpenAPI_DisabledClientDefersRebuild: connectToMCPClient
// refuses a disabled client, so a spec edit saved while the client is disabled
// must not dial. The new config is stored and EnableClient builds from it.
func TestUpdateClient_OpenAPI_DisabledClientDefersRebuild(t *testing.T) {
	stub := &stubOpenAPIFactory{}
	m := NewMCPManager(context.Background(), schemas.MCPConfig{InProcessServerFactory: stub.factory()}, nil, &MockLogger{}, nil)
	config := newOpenAPITestConfig("petstore", "listPets")
	require.NoError(t, m.AddClient(context.Background(), config))
	t.Cleanup(func() { _ = m.RemoveClient(config.ID) })
	require.NoError(t, m.DisableClient(config.ID))

	edited := newOpenAPITestConfig("petstore", "listPets, createPet")
	edited.Disabled = true
	require.NoError(t, m.UpdateClient(config.ID, edited), "editing a disabled client's spec must save without dialing")
	assert.Equal(t, int32(1), stub.calls.Load(), "no rebuild while disabled")
	m.mu.RLock()
	assert.Equal(t, schemas.MCPConnectionStateDisabled, m.clientMap[config.ID].State)
	assert.Equal(t, "listPets, createPet", m.clientMap[config.ID].ExecutionConfig.OpenAPIConfig.Spec, "the new spec is stored")
	m.mu.RUnlock()

	require.NoError(t, m.EnableClient(config.ID))
	assert.Equal(t, int32(2), stub.calls.Load(), "enable builds the server from the stored spec")
	assert.Equal(t, []string{"petstore-createPet", "petstore-listPets"}, openAPIToolNames(t, m, config.ID))
}

// headerRewritingPipeline is a plugin pipeline whose PreMCPConnectionHook
// replaces the connect request's static headers, as a header-injecting plugin
// would.
type headerRewritingPipeline struct {
	headers map[string]string
}

func (p *headerRewritingPipeline) RunMCPPreHooks(_ *schemas.BifrostContext, req *schemas.BifrostMCPRequest) (*schemas.BifrostMCPRequest, *schemas.MCPPluginShortCircuit, int) {
	return req, nil, 0
}

func (p *headerRewritingPipeline) RunMCPPostHooks(_ *schemas.BifrostContext, resp *schemas.BifrostMCPResponse, bifrostErr *schemas.BifrostError, _ int) (*schemas.BifrostMCPResponse, *schemas.BifrostError) {
	return resp, bifrostErr
}

func (p *headerRewritingPipeline) RunMCPPreConnectionHooks(_ *schemas.BifrostContext, req *schemas.BifrostMCPConnectRequest) (*schemas.BifrostMCPConnectRequest, *schemas.MCPConnectionShortCircuit, int) {
	clone := *req
	clone.Headers = maps.Clone(p.headers)
	return &clone, nil, 0
}

func (p *headerRewritingPipeline) RunMCPPostConnectionHooks(_ *schemas.BifrostContext, resp *schemas.BifrostMCPConnectResponse, bifrostErr *schemas.BifrostError, _ int) (*schemas.BifrostMCPConnectResponse, *schemas.BifrostError) {
	return resp, bifrostErr
}

// TestConnect_OpenAPI_PluginAdjustedHeadersReachUpstream: as for HTTP and SSE,
// the static headers a PreMCPConnectionHook leaves on the connect request are
// what the upstream receives. A header the plugin adds is sent, one it drops is
// not, and the credential-store header is still layered on top.
func TestConnect_OpenAPI_PluginAdjustedHeadersReachUpstream(t *testing.T) {
	stub := &stubOpenAPIFactory{}
	pipeline := &headerRewritingPipeline{headers: map[string]string{"X-Plugin": "added"}}
	m := NewMCPManager(context.Background(), schemas.MCPConfig{
		InProcessServerFactory: stub.factory(),
		PluginPipelineProvider: func() interface{} { return pipeline },
		ReleasePluginPipeline:  func(interface{}) {},
	}, nil, &MockLogger{}, nil)
	config := newOpenAPITestConfig("petstore", "listPets")
	require.NoError(t, m.AddClient(context.Background(), config))
	t.Cleanup(func() { _ = m.RemoveClient(config.ID) })

	got := callOpenAPIStubTool(t, m, "petstore-listPets")
	assert.Contains(t, got, "X-Plugin=added;", "a plugin-added header reaches the upstream")
	assert.NotContains(t, got, "X-Static", "a header the plugin dropped is not sent")
	assert.Contains(t, got, "Authorization=Bearer shared-token;", "the credential is layered on the plugin-adjusted headers")
}

// TestRefreshClientTools_OpenAPI_RebuildsServer: refresh on an openapi client
// re-resolves the document and re-synthesizes the server, so a spec_url whose
// remote content changed shows its new operations without a reconnect.
func TestRefreshClientTools_OpenAPI_RebuildsServer(t *testing.T) {
	stub := &stubOpenAPIFactory{}
	m := NewMCPManager(context.Background(), schemas.MCPConfig{InProcessServerFactory: stub.factory()}, nil, &MockLogger{}, nil)
	config := newOpenAPITestConfig("petstore", "listPets")
	require.NoError(t, m.AddClient(context.Background(), config))
	t.Cleanup(func() { _ = m.RemoveClient(config.ID) })

	changed := "listPets, createPet"
	stub.toolsOverride.Store(&changed)
	count, err := m.RefreshClientTools(context.Background(), config.ID)
	require.NoError(t, err)
	assert.Equal(t, int32(2), stub.calls.Load(), "refresh rebuilds the synthesized server")
	assert.Equal(t, 2, count)
	assert.Equal(t, []string{"petstore-createPet", "petstore-listPets"}, openAPIToolNames(t, m, config.ID))
}

// TestRefreshClientTools_OpenAPI_RespectsExclusiveOp: the rebuild a refresh
// performs is a reconnect, so it takes the same per-client guard as UpdateClient
// and ReconnectClient. While another operation holds it, refresh must refuse
// rather than dial with a config that operation is about to replace.
func TestRefreshClientTools_OpenAPI_RespectsExclusiveOp(t *testing.T) {
	stub := &stubOpenAPIFactory{}
	m := NewMCPManager(context.Background(), schemas.MCPConfig{InProcessServerFactory: stub.factory()}, nil, &MockLogger{}, nil)
	config := newOpenAPITestConfig("petstore", "listPets")
	require.NoError(t, m.AddClient(context.Background(), config))
	t.Cleanup(func() { _ = m.RemoveClient(config.ID) })

	finish, ok := m.beginExclusiveClientOp(config.ID) // an in-flight update
	require.True(t, ok)
	_, err := m.RefreshClientTools(context.Background(), config.ID)
	require.ErrorContains(t, err, "in progress")
	assert.Equal(t, int32(1), stub.calls.Load(), "no rebuild while another operation holds the client")
	finish(nil)

	_, err = m.RefreshClientTools(context.Background(), config.ID)
	require.NoError(t, err)
	assert.Equal(t, int32(2), stub.calls.Load(), "refresh rebuilds once the guard is free")
}

// TestAcquireClientConn_OpenAPIPerUserHeaders_PreflightsCredential: an openapi
// client is sticky even with per_user_headers, so AcquireClientConn hands out
// the shared in-process connection — but only after resolving the caller's
// credential, so the typed auth error reaches the caller instead of being
// flattened inside the synthesized handler.
func TestAcquireClientConn_OpenAPIPerUserHeaders_PreflightsCredential(t *testing.T) {
	stub := &stubOpenAPIFactory{}
	m := NewMCPManager(context.Background(), schemas.MCPConfig{InProcessServerFactory: stub.factory()}, nil, &MockLogger{}, nil)
	config := newOpenAPITestConfig("peruser", "listPets")
	config.AuthType = schemas.MCPAuthTypePerUserHeaders
	config.PerUserHeaderKeys = []string{"X-User-Token"}
	require.NoError(t, m.AddClient(context.Background(), config))
	t.Cleanup(func() { _ = m.RemoveClient(config.ID) })

	m.mu.RLock()
	state := m.clientMap[config.ID]
	m.mu.RUnlock()
	require.Equal(t, schemas.MCPConnectionStateHealthy, state.State, "openapi clients never park in pending_verification")
	require.NotNil(t, state.Conn)

	// No headers provider is configured on this manager, so the per-user
	// resolver fails: that failure must surface here, before any CallTool.
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	conn, release, err := m.AcquireClientConn(ctx, state)
	require.Error(t, err)
	assert.ErrorContains(t, err, "per-user headers")
	assert.Nil(t, conn)
	assert.Nil(t, release)
}

func TestAwaitsAdminVerification_OpenAPINeverWaits(t *testing.T) {
	cfg := newOpenAPITestConfig("peruser", "listPets")
	cfg.AuthType = schemas.MCPAuthTypePerUserHeaders
	cfg.PerUserHeaderKeys = []string{"X-User-Token"}
	assert.False(t, awaitsAdminVerification(cfg), "nothing upstream to verify for a synthesized server")

	httpCfg := *cfg
	httpCfg.ConnectionType = schemas.MCPConnectionTypeHTTP
	assert.True(t, awaitsAdminVerification(&httpCfg), "control: the same auth type over HTTP still awaits verification")
}

func TestOpenAPIConfigChanged(t *testing.T) {
	str := func(s string) *string { return &s }
	base := func() *schemas.MCPOpenAPIConfig {
		return &schemas.MCPOpenAPIConfig{
			Spec:    "spec-v1",
			BaseURL: str("https://api.example.com"),
			SecurityCredentials: map[string]schemas.MCPOpenAPICredential{
				"ApiKey": {Value: schemas.NewSecretVar("k1")},
			},
			SpecHash: "h1",
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(c *schemas.MCPOpenAPIConfig)
		want   bool
	}{
		{name: "identical", mutate: func(*schemas.MCPOpenAPIConfig) {}},
		{name: "metadata only", mutate: func(c *schemas.MCPOpenAPIConfig) { c.SpecHash = "h2"; c.OperationCount = 9 }},
		{name: "spec", mutate: func(c *schemas.MCPOpenAPIConfig) { c.Spec = "spec-v2" }, want: true},
		{name: "base url", mutate: func(c *schemas.MCPOpenAPIConfig) { c.BaseURL = str("https://other.example.com") }, want: true},
		{name: "base url cleared", mutate: func(c *schemas.MCPOpenAPIConfig) { c.BaseURL = nil }, want: true},
		{name: "spec url", mutate: func(c *schemas.MCPOpenAPIConfig) { c.SpecURL = str("https://x/openapi.json") }, want: true},
		{name: "credential value", mutate: func(c *schemas.MCPOpenAPIConfig) {
			c.SecurityCredentials["ApiKey"] = schemas.MCPOpenAPICredential{Value: schemas.NewSecretVar("k2")}
		}, want: true},
		{name: "credential added", mutate: func(c *schemas.MCPOpenAPIConfig) {
			c.SecurityCredentials["Basic"] = schemas.MCPOpenAPICredential{Username: schemas.NewSecretVar("u")}
		}, want: true},
		{name: "credential removed", mutate: func(c *schemas.MCPOpenAPIConfig) { delete(c.SecurityCredentials, "ApiKey") }, want: true},
		{name: "include deprecated", mutate: func(c *schemas.MCPOpenAPIConfig) { c.IncludeDeprecated = true }, want: true},
		{name: "response cap", mutate: func(c *schemas.MCPOpenAPIConfig) { c.MaxResponseBytes = 10 }, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := base()
			tc.mutate(next)
			assert.Equal(t, tc.want, openAPIConfigChanged(base(), next))
		})
	}
	assert.True(t, openAPIConfigChanged(nil, base()))
	assert.True(t, openAPIConfigChanged(base(), nil))
	assert.False(t, openAPIConfigChanged(nil, nil))
}

// TestOpenAPIHeaderResolver_NoRequestContextLeaksNoGoroutines: header
// resolution without a request BifrostContext (connect-time probes) builds a
// fallback context, and repeated calls must not accumulate goroutines.
func TestOpenAPIHeaderResolver_NoRequestContextLeaksNoGoroutines(t *testing.T) {
	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
	resolve := m.openAPIHeaderResolver(newOpenAPITestConfig("petstore", "listPets"), map[string]string{"X-Static": "v"})
	before := runtime.NumGoroutine()
	for i := 0; i < 1000; i++ {
		_, err := resolve(context.Background())
		require.NoError(t, err)
	}
	time.Sleep(50 * time.Millisecond)
	assert.LessOrEqual(t, runtime.NumGoroutine(), before+5, "fallback contexts must not leave watcher goroutines behind")
}

// TestUnmatchedToolSelections: after an OpenAPI rebuild (a rename can change
// shortened tool names), selections that no longer name a tool are reported so
// a Go SDK caller learns which tools went unavailable.
func TestUnmatchedToolSelections(t *testing.T) {
	tools := map[string]schemas.ChatTool{"zoo-listPets": {}, "zoo-createPet": {}}
	assert.Equal(t, []string{"a_very_long_operation_name_cut"}, unmatchedToolSelections(schemas.WhiteList{"listPets", "a_very_long_operation_name_cut"}, tools, "zoo"))
	assert.Empty(t, unmatchedToolSelections(schemas.WhiteList{"*"}, tools, "zoo"))
	assert.Empty(t, unmatchedToolSelections(schemas.WhiteList{"listPets", "createPet"}, tools, "zoo"))
}
