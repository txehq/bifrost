import type { AgentRegistrationView, UpdateAgentRequest } from "@/lib/types/agents";

/**
 * The enable/disable toggle's request. PUT is a full replacement, so it
 * resubmits every field with only `enabled` changed, including the approved
 * credential origins: omitting them clears the allowlist, and re-enabling then
 * rejects a runtime endpoint that needed one. Masked credential secrets
 * round-trip verbatim and the backend keeps the stored values.
 */
export function agentToggleRequest(agent: AgentRegistrationView, enabled: boolean): UpdateAgentRequest {
	return {
		agent_card_url: agent.agent_card_url,
		enabled,
		allow_by_default: agent.allow_by_default,
		forward_accepted_credential: agent.forward_accepted_credential,
		forward_accepted_credential_overrides_auth: agent.forward_accepted_credential_overrides_auth,
		discovery_auth: agent.discovery_auth,
		runtime_auth: agent.runtime_auth,
		virtual_key_ids: agent.virtual_key_ids ?? [],
		credential_origins: agent.credential_origins ?? [],
	};
}