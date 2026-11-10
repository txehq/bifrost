package governance

import (
	"context"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every refusal decision has a policy (or, for a wiring fault, bifrost) error type. A new
// Decision constant must be added to policyErrorTypes; this lists every one by hand so
// that adding a decision without a type fails here.
func TestEveryRefusalDecisionHasAnErrorType(t *testing.T) {
	for _, d := range []Decision{
		DecisionAccessNotFound, DecisionAccessBlocked, DecisionRateLimited, DecisionBudgetExceeded,
		DecisionTokenLimited, DecisionRequestLimited, DecisionModelBlocked, DecisionProviderBlocked,
		DecisionMCPToolBlocked, DecisionModelLimitsUnattributable, DecisionAccessUnresolved, DecisionAgentBlocked,
	} {
		errorType, ok := policyErrorTypes[string(d)]
		require.True(t, ok, "decision %q has no error type", d)
		assert.Contains(t, schemas.ErrorTypes, errorType)
	}
}

func TestDeclarePolicyErrorType(t *testing.T) {
	cases := []struct {
		name string
		err  *schemas.BifrostError
		want schemas.ErrorType
	}{
		{"virtual key required, a 401", &schemas.BifrostError{Type: new(virtualKeyRequiredType), StatusCode: new(401)}, schemas.ErrorTypePolicyAccessDenied},
		{"missing required headers", &schemas.BifrostError{Type: new(missingRequiredHeadersType), StatusCode: new(400)}, schemas.ErrorTypePolicyAccessDenied},
		{"mcp tool blocked", &schemas.BifrostError{Type: new(string(DecisionMCPToolBlocked)), StatusCode: new(403)}, schemas.ErrorTypePolicyToolBlocked},
		{"budget", &schemas.BifrostError{Type: new(string(DecisionBudgetExceeded)), StatusCode: new(402)}, schemas.ErrorTypePolicyBudgetExceeded},
		{"untyped 403 refusal", &schemas.BifrostError{StatusCode: new(403)}, schemas.ErrorTypePolicyAccessDenied},
		{"governance failing, a 500", &schemas.BifrostError{StatusCode: new(500)}, schemas.ErrorTypeBifrostInternal},
		{"a declaration is kept", &schemas.BifrostError{Type: new(string(DecisionModelBlocked)), ExtraFields: schemas.BifrostErrorExtraFields{ErrorType: schemas.ErrorTypePolicyRateLimited}}, schemas.ErrorTypePolicyRateLimited},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			declarePolicyErrorType(tc.err)
			assert.Equal(t, tc.want, tc.err.ExtraFields.ErrorType)
			// What the client is sent: the declaration wins over the status, so a 401/403
			// refusal is never read as provider_auth_failed.
			assert.Equal(t, tc.want, schemas.ClassifyErrorType(tc.err, schemas.ChatCompletionRequest))
		})
	}
	declarePolicyErrorType(nil) // nil-safe
}

// Through the hook a deployment calls: refusals that used to carry no error type (the
// mandatory virtual key's 401) and ones that always did both reach the caller as policy_*.
func TestPreLLMHookRefusalsCarryPolicyErrorTypes(t *testing.T) {
	t.Run("virtual key required", func(t *testing.T) {
		logger := NewMockLogger()
		store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{}, nil, nil)
		require.NoError(t, err)
		plugin, err := InitFromStore(context.Background(), &Config{IsVkMandatory: boolPtr(true)}, logger, store, nil, nil, nil, nil)
		require.NoError(t, err)
		ctx := grantedCtx(context.WithValue(context.Background(), schemas.BifrostContextKeyRequestID, "req-1"))
		_, shortCircuit, err := plugin.PreLLMHook(ctx, &schemas.BifrostRequest{
			RequestType: schemas.ChatCompletionRequest,
			ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "gpt-4"},
		})
		require.NoError(t, err)
		require.NotNil(t, shortCircuit)
		require.NotNil(t, shortCircuit.Error.StatusCode)
		assert.Equal(t, 401, *shortCircuit.Error.StatusCode)
		assert.Equal(t, schemas.ErrorTypePolicyAccessDenied, shortCircuit.Error.ExtraFields.ErrorType)
	})
	t.Run("inactive key", func(t *testing.T) {
		plugin := refusalTestPlugin(t, func(vk *configstoreTables.TableVirtualKey) {
			inactive := false
			vk.IsActive = &inactive
		})
		bifrostErr := refusalTestRefusalFor(t, plugin, newPreRequestCtx(nil, nil))
		assert.Equal(t, schemas.ErrorTypePolicyAccessDenied, bifrostErr.ExtraFields.ErrorType)
	})
}
