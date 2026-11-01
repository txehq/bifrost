package governance

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGovernancePlugin_StartResetWorkers(t *testing.T) {
	logger := NewMockLogger()
	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{}, nil, nil)
	require.NoError(t, err)

	resolver := NewBudgetResolver(store, nil, logger, nil)
	tracker := NewUsageTracker(context.Background(), store, resolver, nil, logger)
	pluginCtx, cancel := context.WithCancel(context.Background())
	plugin := &GovernancePlugin{
		ctx:        pluginCtx,
		cancelFunc: cancel,
		store:      store,
		resolver:   resolver,
		tracker:    tracker,
		logger:     logger,
	}
	t.Cleanup(func() { require.NoError(t, plugin.Cleanup()) })

	require.Nil(t, tracker.resetTicker, "reset worker must remain dormant during hydration")

	plugin.StartResetWorkers(context.Background())
	firstTicker := tracker.resetTicker
	require.NotNil(t, firstTicker, "reset worker must start after hydration")

	plugin.StartResetWorkers(context.Background())
	require.Same(t, firstTicker, tracker.resetTicker, "reset worker activation must be idempotent")
}

// a request that failed (Success=false) but still consumed provider tokens
// (Cost/TokensUsed > 0, e.g. a cancelled mid-stream or a 5xx after input
// processing) MUST update the budget. Anthropic bills for tokens it processed
// regardless of whether Bifrost classified the request as successful.
func TestUsageTracker_FailedRequestWithUsage_IsBilled(t *testing.T) {
	logger := NewMockLogger()

	budget := buildBudgetWithUsage("budget1", 1000.0, 0.0, "1d")
	vk := buildVirtualKeyWithBudget("vk1", "sk-bf-test", "Test VK", budget)

	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
		Budgets:     []configstoreTables.TableBudget{*budget},
	}, nil, nil)
	require.NoError(t, err)

	resolver := NewBudgetResolver(store, nil, logger, nil)
	tracker := NewUsageTracker(context.Background(), store, resolver, nil, logger)
	defer tracker.Cleanup()

	update := &UsageUpdate{
		Success:      false, // Failed/cancelled request...
		TokensUsed:   100,
		Cost:         25.5, // ...that nonetheless consumed provider tokens.
		RequestID:    "req-123",
		HasUsageData: true,
	}

	tracker.UpdateUsage(context.Background(), settleLimits(store, "sk-bf-test", schemas.OpenAI, "gpt-4", update))

	// Give time for async processing
	time.Sleep(200 * time.Millisecond)

	// Verify budget WAS updated - retrieve from store
	budgets := store.GetGovernanceData(context.Background()).Budgets
	updatedBudget, exists := budgets["budget1"]
	require.True(t, exists)
	require.NotNil(t, updatedBudget)

	assert.Equal(t, 25.5, updatedBudget.CurrentUsage,
		"Failed request that consumed tokens should still bill the budget")
}

// TestUsageTracker_ToolCallDoesNotClaimItsInferencesBilling: a tool an injected loop runs shares
// its inference's request id, nonce and attempt. Settling the tool first must leave the inference
// billable, each tool call settles once of its own, and a replay of the inference is still refused.
func TestUsageTracker_ToolCallDoesNotClaimItsInferencesBilling(t *testing.T) {
	logger := NewMockLogger()

	budget := buildBudgetWithUsage("budget1", 1000.0, 0.0, "1d")
	vk := buildVirtualKeyWithBudget("vk1", "sk-bf-test", "Test VK", budget)

	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
		Budgets:     []configstoreTables.TableBudget{*budget},
	}, nil, nil)
	require.NoError(t, err)

	resolver := NewBudgetResolver(store, nil, logger, nil)
	tracker := NewUsageTracker(context.Background(), store, resolver, nil, logger)
	defer tracker.Cleanup()

	charge := func(cost float64, toolCall string) {
		tracker.UpdateUsage(context.Background(), settleLimits(store, "sk-bf-test", schemas.OpenAI, "gpt-4", &UsageUpdate{
			Success: true, Cost: cost, RequestID: "req-tool", BillingNonce: "nonce", ToolCallID: toolCall, IsFinalChunk: true, HasUsageData: true,
		}))
	}
	charge(1, "tool-a")
	charge(2, "tool-b")
	charge(1, "tool-a")
	charge(10, "")
	charge(10, "")

	assert.Eventually(t, func() bool {
		return store.GetGovernanceData(context.Background()).Budgets["budget1"].CurrentUsage == 13
	}, 2*time.Second, 20*time.Millisecond, "each tool call and the inference bill once")
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 13.0, store.GetGovernanceData(context.Background()).Budgets["budget1"].CurrentUsage, "the inference billed once, with both tools")
}

// TestUsageTracker_ToolAndInferenceKeysStayIndependent walks the settlements one request can deliver
// and proves each key bills once: a duplicate hook delivery, a retry, a fallback, parallel tools, a
// stream's terminal racing its cancellation, and a failed turn after a tool ran.
func TestUsageTracker_ToolAndInferenceKeysStayIndependent(t *testing.T) {
	type settle struct {
		cost            float64
		tool            string
		attempt, chain  int
		streaming, fail bool
	}
	for name, tc := range map[string]struct {
		settlements []settle
		want        float64
	}{
		"duplicate tool hook":              {[]settle{{cost: 1, tool: "t1"}, {cost: 1, tool: "t1"}, {cost: 10}}, 11},
		"duplicate inference hook":         {[]settle{{cost: 10}, {cost: 10}, {cost: 1, tool: "t1"}}, 11},
		"parallel tools":                   {[]settle{{cost: 1, tool: "t1"}, {cost: 2, tool: "t2"}, {cost: 4, tool: "t3"}, {cost: 10}}, 17},
		"retry bills its own attempt":      {[]settle{{cost: 1, tool: "t1"}, {cost: 10, fail: true}, {cost: 20, attempt: 1}}, 31},
		"fallback bills its own chain":     {[]settle{{cost: 10, fail: true}, {cost: 20, chain: 1}, {cost: 20, chain: 1}}, 30},
		"same tool id on another fallback": {[]settle{{cost: 1, tool: "t1"}, {cost: 1, tool: "t1", chain: 1}}, 2},
		"stream terminal races cancel":     {[]settle{{cost: 1, tool: "t1"}, {cost: 10, streaming: true}, {cost: 10, streaming: true, fail: true}}, 11},
		"failed turn after a tool":         {[]settle{{cost: 1, tool: "t1"}, {cost: 10, fail: true}}, 11},
	} {
		t.Run(name, func(t *testing.T) {
			logger := NewMockLogger()
			budget := buildBudgetWithUsage("budget1", 1000.0, 0.0, "1d")
			vk := buildVirtualKeyWithBudget("vk1", "sk-bf-test", "Test VK", budget)
			store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
				VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
				Budgets:     []configstoreTables.TableBudget{*budget},
			}, nil, nil)
			require.NoError(t, err)
			tracker := NewUsageTracker(context.Background(), store, NewBudgetResolver(store, nil, logger, nil), nil, logger)
			defer tracker.Cleanup()

			for _, s := range tc.settlements {
				tracker.UpdateUsage(context.Background(), settleLimits(store, "sk-bf-test", schemas.OpenAI, "gpt-4", &UsageUpdate{
					Success: !s.fail, Cost: s.cost, RequestID: "req-tool", BillingNonce: "nonce", ToolCallID: s.tool,
					AttemptNumber: s.attempt, FallbackIndex: s.chain, IsStreaming: s.streaming, IsFinalChunk: true, HasUsageData: true,
				}))
			}
			usage := func() float64 { return store.GetGovernanceData(context.Background()).Budgets["budget1"].CurrentUsage }
			assert.Eventually(t, func() bool { return usage() == tc.want }, 2*time.Second, 20*time.Millisecond, "each key bills once")
			time.Sleep(50 * time.Millisecond)
			assert.Equal(t, tc.want, usage())
		})
	}
}

// TestUsageTracker_FailedRequestNoUsage_IsSkipped verifies the inverse: a
// request that failed WITHOUT consuming any tokens (e.g. 401/403/429 before the
// model ran) carries no usage and must NOT bill anything.
func TestUsageTracker_FailedRequestNoUsage_IsSkipped(t *testing.T) {
	logger := NewMockLogger()

	budget := buildBudgetWithUsage("budget1", 1000.0, 0.0, "1d")
	vk := buildVirtualKeyWithBudget("vk1", "sk-bf-test", "Test VK", budget)

	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
		Budgets:     []configstoreTables.TableBudget{*budget},
	}, nil, nil)
	require.NoError(t, err)

	resolver := NewBudgetResolver(store, nil, logger, nil)
	tracker := NewUsageTracker(context.Background(), store, resolver, nil, logger)
	defer tracker.Cleanup()

	update := &UsageUpdate{
		Success:    false, // Failed before the model ran...
		TokensUsed: 0,
		Cost:       0.0, // ...so no tokens were consumed.
		RequestID:  "req-456",
	}

	tracker.UpdateUsage(context.Background(), settleLimits(store, "sk-bf-test", schemas.OpenAI, "gpt-4", update))

	// Give time for async processing
	time.Sleep(200 * time.Millisecond)

	budgets := store.GetGovernanceData(context.Background()).Budgets
	updatedBudget, exists := budgets["budget1"]
	require.True(t, exists)
	require.NotNil(t, updatedBudget)

	assert.Equal(t, 0.0, updatedBudget.CurrentUsage,
		"Failed request with no usage should not bill anything")
}

// TestUsageTracker_UpdateUsage_VirtualKeyNotFound tests handling of missing VK
func TestUsageTracker_UpdateUsage_VirtualKeyNotFound(t *testing.T) {
	logger := NewMockLogger()

	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{}, nil, nil)
	require.NoError(t, err)

	resolver := NewBudgetResolver(store, nil, logger, nil)
	tracker := NewUsageTracker(context.Background(), store, resolver, nil, logger)
	defer tracker.Cleanup()

	update := &UsageUpdate{
		Success:    true,
		TokensUsed: 100,
		Cost:       25.5,
	}

	// Should not panic or error
	tracker.UpdateUsage(context.Background(), settleLimits(store, "sk-bf-test", schemas.OpenAI, "gpt-4", update))

	time.Sleep(100 * time.Millisecond)
	// Just verify it doesn't crash
	assert.True(t, true)
}

// TestUsageTracker_UpdateUsage_StreamingOptimization tests streaming request handling
func TestUsageTracker_UpdateUsage_StreamingOptimization(t *testing.T) {
	logger := NewMockLogger()

	rateLimit := buildRateLimitWithUsage("rl1", 10000, 0, 1000, 0)
	vk := buildVirtualKeyWithRateLimit("vk1", "sk-bf-test", "Test VK", rateLimit)

	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
		RateLimits:  []configstoreTables.TableRateLimit{*rateLimit},
	}, nil, nil)
	require.NoError(t, err)

	resolver := NewBudgetResolver(store, nil, logger, nil)
	tracker := NewUsageTracker(context.Background(), store, resolver, nil, logger)
	defer tracker.Cleanup()

	// First streaming chunk (not final, has usage data)
	update1 := &UsageUpdate{
		Success:      true,
		TokensUsed:   50,
		Cost:         0.0, // No cost on non-final chunks
		RequestID:    "req-123",
		IsStreaming:  true,
		IsFinalChunk: false,
		HasUsageData: true,
	}

	tracker.UpdateUsage(context.Background(), settleLimits(store, "sk-bf-test", schemas.OpenAI, "gpt-4", update1))
	time.Sleep(200 * time.Millisecond)

	// Retrieve the updated rate limit from the main RateLimits map
	governanceData := store.GetGovernanceData(context.Background())
	updatedRateLimit, exists := governanceData.RateLimits["rl1"]
	require.True(t, exists, "Rate limit should exist")
	require.NotNil(t, updatedRateLimit)

	// Tokens should be updated but not requests (not final chunk)
	assert.Equal(t, int64(50), updatedRateLimit.TokenCurrentUsage, "Tokens should be updated on non-final chunk")

	// Final chunk
	update2 := &UsageUpdate{
		Success:      true,
		TokensUsed:   0, // Already counted
		Cost:         12.5,
		RequestID:    "req-123",
		IsStreaming:  true,
		IsFinalChunk: true,
		HasUsageData: true,
	}

	tracker.UpdateUsage(context.Background(), settleLimits(store, "sk-bf-test", schemas.OpenAI, "gpt-4", update2))
	time.Sleep(200 * time.Millisecond)

	// Retrieve the updated rate limit again
	governanceData = store.GetGovernanceData(context.Background())
	updatedRateLimit, exists = governanceData.RateLimits["rl1"]
	require.True(t, exists, "Rate limit should exist")
	require.NotNil(t, updatedRateLimit)

	// Request counter should be updated on final chunk
	assert.Equal(t, int64(1), updatedRateLimit.RequestCurrentUsage, "Request should be incremented on final chunk")
}

// TestUsageTracker_Idempotency_SameAttemptBilledOnce verifies the billing dedup:
// the same physical provider call (RequestID + AttemptNumber) settling twice —
// e.g. both the core ctx.Done() return path and the provider goroutine's
// terminal post-hook — bills the budget only once.
func TestUsageTracker_Idempotency_SameAttemptBilledOnce(t *testing.T) {
	logger := NewMockLogger()

	budget := buildBudgetWithUsage("budget1", 1000.0, 0.0, "1d")
	vk := buildVirtualKeyWithBudget("vk1", "sk-bf-test", "Test VK", budget)

	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
		Budgets:     []configstoreTables.TableBudget{*budget},
	}, nil, nil)
	require.NoError(t, err)

	resolver := NewBudgetResolver(store, nil, logger, nil)
	tracker := NewUsageTracker(context.Background(), store, resolver, nil, logger)
	defer tracker.Cleanup()

	mk := func() *UsageUpdate {
		return &UsageUpdate{
			Success:       false,
			TokensUsed:    100,
			Cost:          10.0,
			RequestID:     "req-dup",
			AttemptNumber: 0,
			HasUsageData:  true,
		}
	}

	tracker.UpdateUsage(context.Background(), settleLimits(store, "sk-bf-test", schemas.OpenAI, "gpt-4", mk()))
	tracker.UpdateUsage(context.Background(), settleLimits(store, "sk-bf-test", schemas.OpenAI, "gpt-4", mk())) // duplicate settlement
	time.Sleep(200 * time.Millisecond)

	budgets := store.GetGovernanceData(context.Background()).Budgets
	updatedBudget := budgets["budget1"]
	require.NotNil(t, updatedBudget)
	assert.Equal(t, 10.0, updatedBudget.CurrentUsage,
		"Same RequestID+attempt must bill exactly once")
}

// TestUsageTracker_Idempotency_DifferentAttemptsBothBilled verifies that two
// distinct physical provider calls under one logical RequestID (e.g. a failed
// attempt that consumed partial tokens, then a successful retry) each bill —
// the dedup key includes the attempt number so legitimate per-attempt charges
// are not suppressed.
func TestUsageTracker_Idempotency_DifferentAttemptsBothBilled(t *testing.T) {
	logger := NewMockLogger()

	budget := buildBudgetWithUsage("budget1", 1000.0, 0.0, "1d")
	vk := buildVirtualKeyWithBudget("vk1", "sk-bf-test", "Test VK", budget)

	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
		Budgets:     []configstoreTables.TableBudget{*budget},
	}, nil, nil)
	require.NoError(t, err)

	resolver := NewBudgetResolver(store, nil, logger, nil)
	tracker := NewUsageTracker(context.Background(), store, resolver, nil, logger)
	defer tracker.Cleanup()

	mk := func(attempt int, success bool, cost float64) *UsageUpdate {
		return &UsageUpdate{
			Success:       success,
			TokensUsed:    100,
			Cost:          cost,
			RequestID:     "req-retry",
			AttemptNumber: attempt,
			HasUsageData:  true,
		}
	}

	tracker.UpdateUsage(context.Background(), settleLimits(store, "sk-bf-test", schemas.OpenAI, "gpt-4", mk(0, false, 4.0))) // failed attempt, partial usage
	tracker.UpdateUsage(context.Background(), settleLimits(store, "sk-bf-test", schemas.OpenAI, "gpt-4", mk(1, true, 6.0)))  // successful retry
	time.Sleep(200 * time.Millisecond)

	budgets := store.GetGovernanceData(context.Background()).Budgets
	updatedBudget := budgets["budget1"]
	require.NotNil(t, updatedBudget)
	assert.Equal(t, 10.0, updatedBudget.CurrentUsage,
		"Distinct attempts under one RequestID must each bill")
}

// TestUsageTracker_Idempotency_ForgedRequestIDsBothBilled verifies that two
// INDEPENDENT physical calls sharing a caller-chosen request ID (x-request-id
// is caller-supplied) each bill: the transport mints a distinct BillingNonce
// per HTTP request, and the nonce is part of the billing key precisely so a
// forged duplicate ID cannot claim another request's settlement.
func TestUsageTracker_Idempotency_ForgedRequestIDsBothBilled(t *testing.T) {
	logger := NewMockLogger()

	budget := buildBudgetWithUsage("budget1", 1000.0, 0.0, "1d")
	vk := buildVirtualKeyWithBudget("vk1", "sk-bf-test", "Test VK", budget)

	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
		Budgets:     []configstoreTables.TableBudget{*budget},
	}, nil, nil)
	require.NoError(t, err)

	resolver := NewBudgetResolver(store, nil, logger, nil)
	tracker := NewUsageTracker(context.Background(), store, resolver, nil, logger)
	defer tracker.Cleanup()

	mk := func(nonce string) *UsageUpdate {
		return &UsageUpdate{
			Success:       true,
			TokensUsed:    100,
			Cost:          10.0,
			RequestID:     "attacker-chosen-id",
			BillingNonce:  nonce,
			AttemptNumber: 0,
			HasUsageData:  true,
		}
	}

	// Two physically distinct HTTP requests: same forged request ID, both at
	// attempt 0, but each carrying its own transport-minted nonce.
	tracker.UpdateUsage(context.Background(), settleLimits(store, "sk-bf-test", schemas.OpenAI, "gpt-4", mk("nonce-a")))
	tracker.UpdateUsage(context.Background(), settleLimits(store, "sk-bf-test", schemas.OpenAI, "gpt-4", mk("nonce-b")))
	time.Sleep(200 * time.Millisecond)

	budgets := store.GetGovernanceData(context.Background()).Budgets
	updatedBudget := budgets["budget1"]
	require.NotNil(t, updatedBudget)
	assert.Equal(t, 20.0, updatedBudget.CurrentUsage,
		"Independent physical calls sharing a caller-chosen RequestID must each bill")
}

// TestUsageTracker_Idempotency_ForgedRequestIDsCrossVKBothBilled verifies the
// cross-principal case: a duplicate request ID sent under a DIFFERENT virtual
// key must not suppress the second key's charges — the billing map is
// process-global, so without the nonce vk2's settlement would be claimed by
// vk1's earlier request.
func TestUsageTracker_Idempotency_ForgedRequestIDsCrossVKBothBilled(t *testing.T) {
	logger := NewMockLogger()

	budget1 := buildBudgetWithUsage("budget1", 1000.0, 0.0, "1d")
	budget2 := buildBudgetWithUsage("budget2", 1000.0, 0.0, "1d")
	vk1 := buildVirtualKeyWithBudget("vk1", "sk-bf-one", "VK One", budget1)
	vk2 := buildVirtualKeyWithBudget("vk2", "sk-bf-two", "VK Two", budget2)

	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk1, *vk2},
		Budgets:     []configstoreTables.TableBudget{*budget1, *budget2},
	}, nil, nil)
	require.NoError(t, err)

	resolver := NewBudgetResolver(store, nil, logger, nil)
	tracker := NewUsageTracker(context.Background(), store, resolver, nil, logger)
	defer tracker.Cleanup()

	mk := func(nonce string) *UsageUpdate {
		return &UsageUpdate{
			Success:       true,
			TokensUsed:    100,
			Cost:          10.0,
			RequestID:     "shared-id",
			BillingNonce:  nonce,
			AttemptNumber: 0,
			HasUsageData:  true,
		}
	}

	tracker.UpdateUsage(context.Background(), settleLimits(store, "sk-bf-one", schemas.OpenAI, "gpt-4", mk("nonce-vk1")))
	tracker.UpdateUsage(context.Background(), settleLimits(store, "sk-bf-two", schemas.OpenAI, "gpt-4", mk("nonce-vk2")))
	time.Sleep(200 * time.Millisecond)

	budgets := store.GetGovernanceData(context.Background()).Budgets
	require.NotNil(t, budgets["budget1"])
	require.NotNil(t, budgets["budget2"])
	assert.Equal(t, 10.0, budgets["budget1"].CurrentUsage,
		"vk1's request must bill its own budget")
	assert.Equal(t, 10.0, budgets["budget2"].CurrentUsage,
		"vk2's request must bill despite sharing a request ID with vk1's")
}

// TestUsageTracker_Idempotency_SameNonceSameAttemptBilledOnce verifies the race
// the dedup was built for still holds with the nonce in the key: the
// success-terminal and cancellation-terminal paths of ONE physical call share
// nonce, request ID, and attempt, and must settle at most once.
func TestUsageTracker_Idempotency_SameNonceSameAttemptBilledOnce(t *testing.T) {
	logger := NewMockLogger()

	budget := buildBudgetWithUsage("budget1", 1000.0, 0.0, "1d")
	vk := buildVirtualKeyWithBudget("vk1", "sk-bf-test", "Test VK", budget)

	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
		Budgets:     []configstoreTables.TableBudget{*budget},
	}, nil, nil)
	require.NoError(t, err)

	resolver := NewBudgetResolver(store, nil, logger, nil)
	tracker := NewUsageTracker(context.Background(), store, resolver, nil, logger)
	defer tracker.Cleanup()

	mk := func(success bool) *UsageUpdate {
		return &UsageUpdate{
			Success:       success,
			TokensUsed:    100,
			Cost:          10.0,
			RequestID:     "req-one-call",
			BillingNonce:  "nonce-one-call",
			AttemptNumber: 0,
			HasUsageData:  true,
		}
	}

	tracker.UpdateUsage(context.Background(), settleLimits(store, "sk-bf-test", schemas.OpenAI, "gpt-4", mk(true)))
	tracker.UpdateUsage(context.Background(), settleLimits(store, "sk-bf-test", schemas.OpenAI, "gpt-4", mk(false))) // competing cancellation-terminal
	time.Sleep(200 * time.Millisecond)

	budgets := store.GetGovernanceData(context.Background()).Budgets
	updatedBudget := budgets["budget1"]
	require.NotNil(t, updatedBudget)
	assert.Equal(t, 10.0, updatedBudget.CurrentUsage,
		"Competing terminal paths of one physical call must settle at most once")
}

// TestUsageTracker_Idempotency_NestedCallsUnderOneNonceEachBilled verifies the
// MCP-agent/codemode shape: nested inference calls mint a fresh RequestID each
// (core/mcp/agent.go) but share the parent HTTP request's nonce. The RequestID
// stays in the billing key so those calls each bill rather than colliding on
// the shared nonce.
func TestUsageTracker_Idempotency_NestedCallsUnderOneNonceEachBilled(t *testing.T) {
	logger := NewMockLogger()

	budget := buildBudgetWithUsage("budget1", 1000.0, 0.0, "1d")
	vk := buildVirtualKeyWithBudget("vk1", "sk-bf-test", "Test VK", budget)

	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
		Budgets:     []configstoreTables.TableBudget{*budget},
	}, nil, nil)
	require.NoError(t, err)

	resolver := NewBudgetResolver(store, nil, logger, nil)
	tracker := NewUsageTracker(context.Background(), store, resolver, nil, logger)
	defer tracker.Cleanup()

	mk := func(requestID string) *UsageUpdate {
		return &UsageUpdate{
			Success:       true,
			TokensUsed:    100,
			Cost:          10.0,
			RequestID:     requestID,
			BillingNonce:  "nonce-parent-http-request",
			AttemptNumber: 0,
			HasUsageData:  true,
		}
	}

	tracker.UpdateUsage(context.Background(), settleLimits(store, "sk-bf-test", schemas.OpenAI, "gpt-4", mk("nested-call-1")))
	tracker.UpdateUsage(context.Background(), settleLimits(store, "sk-bf-test", schemas.OpenAI, "gpt-4", mk("nested-call-2")))
	time.Sleep(200 * time.Millisecond)

	budgets := store.GetGovernanceData(context.Background()).Budgets
	updatedBudget := budgets["budget1"]
	require.NotNil(t, updatedBudget)
	assert.Equal(t, 20.0, updatedBudget.CurrentUsage,
		"Nested calls sharing one nonce but distinct RequestIDs must each bill")
}

// TestUsageTracker_Idempotency_ConcurrentForgedRequestIDsAllBilled verifies the
// forged-ID fix under concurrency: N parallel physical calls sharing one
// caller-chosen request ID, each with its own nonce, must all bill.
func TestUsageTracker_Idempotency_ConcurrentForgedRequestIDsAllBilled(t *testing.T) {
	logger := NewMockLogger()

	budget := buildBudgetWithUsage("budget1", 1000.0, 0.0, "1d")
	vk := buildVirtualKeyWithBudget("vk1", "sk-bf-test", "Test VK", budget)

	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
		Budgets:     []configstoreTables.TableBudget{*budget},
	}, nil, nil)
	require.NoError(t, err)

	resolver := NewBudgetResolver(store, nil, logger, nil)
	tracker := NewUsageTracker(context.Background(), store, resolver, nil, logger)
	defer tracker.Cleanup()

	const calls = 10
	var wg sync.WaitGroup
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			update := settleLimits(store, "sk-bf-test", schemas.OpenAI, "gpt-4", &UsageUpdate{
				Success:       true,
				TokensUsed:    100,
				Cost:          1.0,
				RequestID:     "attacker-chosen-id",
				BillingNonce:  fmt.Sprintf("nonce-%d", i),
				AttemptNumber: 0,
				HasUsageData:  true,
			})
			tracker.UpdateUsage(context.Background(), update)
		}(i)
	}
	wg.Wait()
	time.Sleep(200 * time.Millisecond)

	budgets := store.GetGovernanceData(context.Background()).Budgets
	updatedBudget := budgets["budget1"]
	require.NotNil(t, updatedBudget)
	assert.Equal(t, float64(calls), updatedBudget.CurrentUsage,
		"All concurrent physical calls sharing a forged RequestID must bill")
}

// TestUsageTracker_Cleanup tests cleanup of the usage tracker
func TestUsageTracker_Cleanup(t *testing.T) {
	logger := NewMockLogger()
	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{}, nil, nil)
	require.NoError(t, err)

	resolver := NewBudgetResolver(store, nil, logger, nil)
	tracker := NewUsageTracker(context.Background(), store, resolver, nil, logger)

	// Should cleanup without error
	err = tracker.Cleanup()
	assert.NoError(t, err, "Cleanup should succeed")
}

type cleanupOrderingStore struct {
	GovernanceStore
	periodicEntered       chan struct{}
	periodicExited        chan struct{}
	finalDumped           chan struct{}
	finalBeforeWorkerExit atomic.Bool
}

func (s *cleanupOrderingStore) ResetExpiredRateLimitsInMemory(context.Context, bool, ...string) []*configstoreTables.TableRateLimit {
	return nil
}

func (s *cleanupOrderingStore) ResetExpiredBudgetsInMemory(context.Context, bool, ...string) []*configstoreTables.TableBudget {
	return nil
}

func (s *cleanupOrderingStore) ResetExpiredRateLimits(context.Context, []*configstoreTables.TableRateLimit) error {
	return nil
}

func (s *cleanupOrderingStore) ResetExpiredBudgets(context.Context, []*configstoreTables.TableBudget) error {
	return nil
}

func (s *cleanupOrderingStore) DumpBudgets(context.Context, map[string]float64) error {
	return nil
}

func (s *cleanupOrderingStore) DumpRateLimits(ctx context.Context, _ map[string]int64, _ map[string]int64) error {
	if ctx.Done() == nil {
		select {
		case <-s.periodicExited:
		default:
			s.finalBeforeWorkerExit.Store(true)
		}
		close(s.finalDumped)
		return nil
	}

	close(s.periodicEntered)
	<-ctx.Done()
	close(s.periodicExited)
	return fmt.Errorf("failed to dump rate limits to database: failed to dump 4 rate limits: %w", ctx.Err())
}

// Cleanup must first cancel and join an in-flight periodic dump, then take the
// final snapshot. Cancellation is expected during shutdown and must not be
// reported as a database failure.
func TestUsageTracker_CleanupWaitsForPeriodicDumpBeforeFinalFlush(t *testing.T) {
	store := &cleanupOrderingStore{
		periodicEntered: make(chan struct{}),
		periodicExited:  make(chan struct{}),
		finalDumped:     make(chan struct{}),
	}
	logger := NewMockLogger()
	tracker := NewUsageTracker(context.Background(), store, nil, nil, logger)

	// Enter the same reset cycle as resetWorker without waiting for the
	// production ten-second ticker, and account for it in the worker wait group.
	tracker.wg.Add(1)
	go func() {
		defer tracker.wg.Done()
		tracker.resetExpiredCounters(tracker.trackerCtx)
	}()

	select {
	case <-store.periodicEntered:
	case <-time.After(time.Second):
		t.Fatal("periodic dump did not start")
	}

	require.NoError(t, tracker.Cleanup())
	assert.False(t, store.finalBeforeWorkerExit.Load(), "final dump raced the periodic worker")
	select {
	case <-store.finalDumped:
	default:
		t.Fatal("final rate-limit dump was not called")
	}

	logger.mu.Lock()
	defer logger.mu.Unlock()
	assert.NotContains(t, logger.errors, "failed to dump rate limits to database: %v",
		"shutdown cancellation must not be logged as a database failure")
}

// TestGovernanceConstructorsStartWorkersUnlessDeferred pins that a plugin from either
// constructor runs its workers without another call, as a Go SDK caller expects, while a
// caller that defers the start gets them only from StartResetWorkers, once.
func TestGovernanceConstructorsStartWorkersUnlessDeferred(t *testing.T) {
	newStore := func(t *testing.T) GovernanceStore {
		store, err := NewLocalGovernanceStore(context.Background(), NewMockLogger(), nil, &configstore.GovernanceConfig{}, nil, nil)
		require.NoError(t, err)
		return store
	}
	constructors := map[string]func(*testing.T, *Config) (*GovernancePlugin, error){
		"Init": func(_ *testing.T, config *Config) (*GovernancePlugin, error) {
			return Init(context.Background(), config, NewMockLogger(), nil, &configstore.GovernanceConfig{}, nil, nil, nil)
		},
		"InitFromStore": func(t *testing.T, config *Config) (*GovernancePlugin, error) {
			return InitFromStore(context.Background(), config, NewMockLogger(), newStore(t), nil, nil, nil, nil)
		},
	}
	for name, construct := range constructors {
		t.Run(name+"/starts", func(t *testing.T) {
			plugin, err := construct(t, &Config{})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, plugin.Cleanup()) })
			ticker := plugin.tracker.resetTicker
			require.NotNil(t, ticker, "the constructor must start the workers")
			plugin.StartResetWorkers(context.Background())
			require.Same(t, ticker, plugin.tracker.resetTicker, "a later StartResetWorkers must not start them again")
		})
		t.Run(name+"/nil config starts", func(t *testing.T) {
			plugin, err := construct(t, nil)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, plugin.Cleanup()) })
			require.NotNil(t, plugin.tracker.resetTicker)
		})
		t.Run(name+"/deferred waits", func(t *testing.T) {
			plugin, err := construct(t, &Config{DeferWorkerStart: true})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, plugin.Cleanup()) })
			require.Nil(t, plugin.tracker.resetTicker, "a deferred start waits for StartResetWorkers")
			plugin.StartResetWorkers(context.Background())
			require.NotNil(t, plugin.tracker.resetTicker)
		})
		t.Run(name+"/cleanup before a deferred start stops it", func(t *testing.T) {
			plugin, err := construct(t, &Config{DeferWorkerStart: true})
			require.NoError(t, err)
			require.NoError(t, plugin.Cleanup())
			plugin.StartResetWorkers(context.Background())
			require.Nil(t, plugin.tracker.resetTicker, "a stopped plugin must not start workers")
		})
	}
}

// TestGovernanceDeferredStartRacesCleanupCleanly: cancelling startup while the server is
// still loading leaves no worker behind, whichever of the start and Cleanup runs first.
func TestGovernanceDeferredStartRacesCleanupCleanly(t *testing.T) {
	for range 50 {
		ctx, cancel := context.WithCancel(context.Background())
		plugin, err := Init(ctx, &Config{DeferWorkerStart: true}, NewMockLogger(), nil, &configstore.GovernanceConfig{}, nil, nil, nil)
		require.NoError(t, err)
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			plugin.StartResetWorkers(ctx)
		}()
		go func() {
			defer wg.Done()
			cancel()
		}()
		go func() {
			defer wg.Done()
			require.NoError(t, plugin.Cleanup())
		}()
		wg.Wait()
		done := make(chan struct{})
		go func() {
			plugin.tracker.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("a reset worker outlived Cleanup")
		}
	}
}

// TestGovernanceConstructorRunsStartupResetsBeforeReturning keeps the StartResetWorkers
// ordering in the constructor path against a real store: the expired counter is reset and
// the startup lock released before the constructor returns, with the workers then running.
// A deferred start leaves the stored counter alone until StartResetWorkers runs.
func TestGovernanceConstructorRunsStartupResetsBeforeReturning(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(fmt.Sprintf("deferred=%t", deferred), func(t *testing.T) {
			ctx := context.Background()
			logger := NewMockLogger()
			configStore, err := configstore.NewConfigStore(ctx, &configstore.Config{
				Enabled: true,
				Type:    configstore.ConfigStoreTypeSQLite,
				Config:  &configstore.SQLiteConfig{Path: t.TempDir() + "/startup.db"},
			}, logger)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, configStore.Close(ctx)) })

			anchor := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
			rateLimit := buildRateLimitWithUsage("startup-rate-limit", 1000, 900, 1000, 900)
			rateLimit.CreatedAt = anchor
			rateLimit.TokenLastReset = anchor
			rateLimit.RequestLastReset = anchor
			require.NoError(t, configStore.CreateRateLimit(ctx, rateLimit))
			store, err := NewLocalGovernanceStore(ctx, logger, configStore, nil, nil, nil)
			require.NoError(t, err)

			plugin, err := InitFromStore(ctx, &Config{DeferWorkerStart: deferred}, logger, store, configStore, nil, nil, nil)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, plugin.Cleanup()) })

			persisted, err := configStore.GetRateLimit(ctx, rateLimit.ID)
			require.NoError(t, err)
			if deferred {
				require.True(t, persisted.TokenLastReset.Equal(anchor), "a deferred start must not reset during construction")
				require.Nil(t, plugin.tracker.resetTicker)
				plugin.StartResetWorkers(ctx)
				persisted, err = configStore.GetRateLimit(ctx, rateLimit.ID)
				require.NoError(t, err)
			}
			require.True(t, persisted.TokenLastReset.After(anchor), "the expired counter must be reset before workers run")
			require.NotNil(t, plugin.tracker.resetTicker)
			lock, err := configStore.GetLock(ctx, "governance_startup_reset")
			require.NoError(t, err)
			require.Nil(t, lock, "the startup lock must be released")
		})
	}
}
