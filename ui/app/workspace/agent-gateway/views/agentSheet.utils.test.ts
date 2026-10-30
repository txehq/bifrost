import { describe, expect, it } from "vitest";
import type { AgentUpstreamAuth } from "@/lib/types/agents";
import { mergeInspectedAuth } from "./agentSheet.utils";

describe("mergeInspectedAuth", () => {
	it("keeps the operator's OAuth settings and credentials when a card is refetched", () => {
		const current: AgentUpstreamAuth = {
			type: "oauth",
			oauth: {
				token_url: "https://old.example/token",
				client_id: { value: "id", ref: "" },
				client_secret: { value: "secret", ref: "" },
				scopes: ["old"],
				resource: "https://api.example/agents",
				discovery_url: "https://idp.example/.well-known",
			},
		};
		const next: AgentUpstreamAuth = {
			type: "oauth",
			oauth: { token_url: "https://new.example/token", client_id: { value: "", ref: "" }, scopes: ["agent.read"] },
		};
		const merged = mergeInspectedAuth(current, next);
		expect(merged.oauth?.resource).toBe("https://api.example/agents");
		expect(merged.oauth?.discovery_url).toBe("https://idp.example/.well-known");
		expect(merged.oauth?.client_id?.value).toBe("id");
		expect(merged.oauth?.client_secret?.value).toBe("secret");
		expect(merged.oauth?.token_url).toBe("https://new.example/token");
		expect(merged.oauth?.scopes).toEqual(["agent.read"]);
	});
});
