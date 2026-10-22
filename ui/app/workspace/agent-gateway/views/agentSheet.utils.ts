import type { AgentUpstreamAuth } from "@/lib/types/agents";

/**
 * mergeInspectedAuth applies the auth an inspected card requires over what the operator
 * has entered. The card decides the shape (type, headers, schemes, token URL, scopes);
 * values only the operator can supply survive: header values, OAuth client credentials
 * and OAuth settings the card does not describe (such as resource), and TLS material.
 */
export function mergeInspectedAuth(current: AgentUpstreamAuth, next: AgentUpstreamAuth): AgentUpstreamAuth {
	return {
		...next,
		headers: Object.fromEntries(
			Object.entries(next.headers ?? {}).map(([header, empty]) => [header, current.headers?.[header] ?? empty]),
		),
		oauth:
			next.oauth && current.type === "oauth"
				? {
						...current.oauth,
						...next.oauth,
						client_id: current.oauth?.client_id ?? next.oauth.client_id,
						client_secret: current.oauth?.client_secret ?? next.oauth.client_secret,
						token_url: next.oauth.token_url ?? current.oauth?.token_url,
					}
				: next.oauth,
		tls: current.tls ?? next.tls,
	};
}
