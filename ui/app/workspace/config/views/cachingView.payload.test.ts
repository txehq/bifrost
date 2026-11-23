import { describe, expect, it } from "vitest";
import type { EditorCacheConfig } from "@/lib/types/config";
import { buildPayload } from "./cachingView.payload";

const saved: EditorCacheConfig = {
	ttl: 300,
	threshold: 0.8,
	dimension: 1,
	conversation_history_threshold: 3,
	exclude_system_prompt: false,
	cache_by_model: true,
	cache_by_provider: true,
	disable_identity_scoping: false,
	vector_store_namespace: "tenant-a",
	default_cache_key: "feature-x",
};

// PUT /api/plugins/{name} merges the body over the stored config, so a field left out of the
// JSON keeps its saved value. These pin that clearing a field sends it, as "".
describe("local cache payload", () => {
	for (const mode of ["direct", "semantic"] as const) {
		it(`sends a cleared default cache key and namespace as "" in ${mode} mode`, () => {
			for (const blank of ["", "   ", undefined]) {
				const config = { ...saved, provider: "openai", embedding_model: "m", dimension: 1536, default_cache_key: blank, vector_store_namespace: blank } as EditorCacheConfig;
				const wire = JSON.parse(JSON.stringify(buildPayload(config, mode)));
				expect(wire).toHaveProperty("default_cache_key", "");
				expect(wire).toHaveProperty("vector_store_namespace", "");
			}
		});
	}

	it("keeps set values, trimmed", () => {
		const payload = buildPayload({ ...saved, default_cache_key: "  feature-x ", vector_store_namespace: " tenant-a" }, "direct");
		expect(payload.default_cache_key).toBe("feature-x");
		expect(payload.vector_store_namespace).toBe("tenant-a");
	});

	it("clears the semantic-only fields in direct mode", () => {
		const payload = buildPayload({ ...saved, provider: "openai" as EditorCacheConfig["provider"], embedding_model: "m", dimension: 1536 }, "direct");
		expect(payload).toMatchObject({ provider: "", embedding_model: "", dimension: 1 });
	});
});
