import { describe, expect, it } from "vitest";
import { ALL_PROVIDERS_VALUE, normalizeProviderFilter } from "./providerFilterSelect.utils";

describe("normalizeProviderFilter", () => {
	it("stores a deselection as the all-providers sentinel the charts aggregate on", () => {
		expect(normalizeProviderFilter("")).toBe(ALL_PROVIDERS_VALUE);
		expect(ALL_PROVIDERS_VALUE).toBe("all");
	});

	it("keeps a picked provider", () => {
		expect(normalizeProviderFilter("openai")).toBe("openai");
		expect(normalizeProviderFilter(ALL_PROVIDERS_VALUE)).toBe(ALL_PROVIDERS_VALUE);
	});
});