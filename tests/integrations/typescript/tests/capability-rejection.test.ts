/**
 * Offline unit tests for the decision-matrix capability skip: no gateway or
 * provider is contacted.
 */

import { describe, expect, it } from "vitest";
import { isCapabilityRejection } from "../src/utils/capability";

describe("isCapabilityRejection", () => {
	const split = new Set(["perplexity"]);

	it("skips a split provider's 400 schema rejection", () => {
		expect(isCapabilityRejection(split, "perplexity", 400, '{"error":{"message":"Invalid request: unsupported schema"}}')).toBe(true);
		expect(isCapabilityRejection(split, "perplexity", 400, '{"error":{"type":"invalid_request_error"}}')).toBe(true);
	});

	it("fails any other status from a split provider", () => {
		expect(isCapabilityRejection(split, "perplexity", 500, "invalid request")).toBe(false);
		expect(isCapabilityRejection(split, "perplexity", 401, "invalid request")).toBe(false);
		expect(isCapabilityRejection(split, "perplexity", 429, "invalid_request")).toBe(false);
	});

	it("fails a 400 that is not a schema rejection", () => {
		expect(isCapabilityRejection(split, "perplexity", 400, '{"error":{"message":"context length exceeded"}}')).toBe(false);
	});

	it("never skips a provider outside the split list", () => {
		expect(isCapabilityRejection(split, "openai", 400, "invalid request")).toBe(false);
	});
});
