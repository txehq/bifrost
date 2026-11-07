import { CacheConfig, EditorCacheConfig, ModelProviderName } from "@/lib/types/config";

// The local cache plugin runs in one of two modes. Direct-only is purely
// hash-based, no embedding provider needed; perfect for exact-replay
// caching. Semantic adds vector similarity on top, requiring an
// embedding-capable provider and the model's real dimension.
export type CacheMode = "direct" | "semantic";

// Clear semantic-only fields when persisting a direct-only payload so the
// server validator doesn't reject a stale provider choice. An update merges
// over the saved config, so they are sent empty rather than left out.
export const buildPayload = (config: EditorCacheConfig, mode: CacheMode): CacheConfig => {
	const base = {
		ttl: config.ttl ?? 0,
		threshold: config.threshold ?? 0,
		conversation_history_threshold: config.conversation_history_threshold,
		exclude_system_prompt: config.exclude_system_prompt,
		cache_by_model: config.cache_by_model,
		cache_by_provider: config.cache_by_provider,
		disable_identity_scoping: config.disable_identity_scoping ?? false,
		// Sent as "" (not undefined) when blank so the server-side merge clears
		// a previously saved value instead of keeping it.
		vector_store_namespace: config.vector_store_namespace?.trim() ?? "",
		default_cache_key: config.default_cache_key?.trim() ?? "",
	};
	if (mode === "direct") {
		return { ...base, provider: "", embedding_model: "", dimension: 1 } as CacheConfig;
	}
	return {
		...base,
		provider: config.provider as ModelProviderName,
		embedding_model: config.embedding_model ?? "",
		dimension: config.dimension ?? 0,
	} as CacheConfig;
};
