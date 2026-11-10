package governance

import "github.com/capsohq/bifrost/core/schemas"

// policyErrorTypes is the error type each governance refusal carries, keyed by the
// decision string the refusal writes as its top-level type. A governance refusal is
// Tokun's own policy, never the provider's: classified by status alone, a 401 or 403
// refusal would read as provider_auth_failed (contract C6, BF8).
var policyErrorTypes = map[string]schemas.ErrorType{
	string(DecisionAccessNotFound):            schemas.ErrorTypePolicyAccessDenied,
	string(DecisionAccessBlocked):             schemas.ErrorTypePolicyAccessDenied,
	string(DecisionAgentBlocked):              schemas.ErrorTypePolicyAccessDenied,
	string(DecisionModelLimitsUnattributable): schemas.ErrorTypePolicyAccessDenied,
	virtualKeyRequiredType:                    schemas.ErrorTypePolicyAccessDenied,
	missingRequiredHeadersType:                schemas.ErrorTypePolicyAccessDenied,
	string(DecisionModelBlocked):              schemas.ErrorTypePolicyModelBlocked,
	string(DecisionProviderBlocked):           schemas.ErrorTypePolicyProviderBlocked,
	string(DecisionMCPToolBlocked):            schemas.ErrorTypePolicyToolBlocked,
	string(DecisionRateLimited):               schemas.ErrorTypePolicyRateLimited,
	string(DecisionTokenLimited):              schemas.ErrorTypePolicyRateLimited,
	string(DecisionRequestLimited):            schemas.ErrorTypePolicyRateLimited,
	string(DecisionBudgetExceeded):            schemas.ErrorTypePolicyBudgetExceeded,
	string(DecisionAccessUnresolved):          schemas.ErrorTypeBifrostInternal,
}

// The refusal types governance writes that are not decisions.
const (
	virtualKeyRequiredType     = "virtual_key_required"
	missingRequiredHeadersType = "missing_required_headers"
)

// declarePolicyErrorType gives a governance refusal its error type when the site that
// built it declared none. Every hook that can refuse (PreLLMHook, PreMCPHook, PreA2AHook)
// and Evaluate pass their refusal through here, so a new refusal site cannot reach the
// client unclassified. A refusal whose decision is not in policyErrorTypes is a policy
// refusal all the same (policy_access_denied), unless it is a 5xx, which is governance
// itself failing (bifrost_internal).
func declarePolicyErrorType(err *schemas.BifrostError) {
	if err == nil || err.ExtraFields.ErrorType != "" {
		return
	}
	if err.Type != nil {
		if errorType, ok := policyErrorTypes[*err.Type]; ok {
			err.ExtraFields.ErrorType = errorType
			return
		}
	}
	if err.StatusCode != nil && *err.StatusCode >= 500 {
		err.ExtraFields.ErrorType = schemas.ErrorTypeBifrostInternal
		return
	}
	err.ExtraFields.ErrorType = schemas.ErrorTypePolicyAccessDenied
}
