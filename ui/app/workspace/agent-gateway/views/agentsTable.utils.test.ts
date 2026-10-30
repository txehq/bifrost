import type { AgentRegistrationView } from "@/lib/types/agents";
import { describe, expect, it } from "vitest";
import { agentToggleRequest } from "./agentsTable.utils";

const agent: AgentRegistrationView = {
	name: "billing",
	agent_card_url: "https://agents.example.com/.well-known/agent-card.json",
	enabled: true,
	allow_by_default: false,
	forward_accepted_credential: true,
	forward_accepted_credential_overrides_auth: false,
	credential_origins: ["https://runtime.example.net"],
	virtual_key_ids: ["vk-1"],
	created_at: "2026-10-01T00:00:00Z",
	updated_at: "2026-10-01T00:00:00Z",
};

describe("agentToggleRequest", () => {
	it("keeps approved credential origins, so re-enabling still reaches the approved runtime", () => {
		const disable = agentToggleRequest(agent, false);
		expect(disable.enabled).toBe(false);
		expect(disable.credential_origins).toEqual(["https://runtime.example.net"]);
		expect(disable.virtual_key_ids).toEqual(["vk-1"]);
	});

	it("sends empty collections rather than omitting them", () => {
		const bare = agentToggleRequest({ ...agent, credential_origins: undefined, virtual_key_ids: undefined }, true);
		expect(bare.credential_origins).toEqual([]);
		expect(bare.virtual_key_ids).toEqual([]);
	});
});