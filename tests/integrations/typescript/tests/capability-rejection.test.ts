/**
 * Offline unit tests for the decision-matrix capability skip: no gateway or
 * provider is contacted.
 */

import { describe, expect, it } from "vitest";
import { isCapabilityRejection } from "../src/utils/capability";

describe("isCapabilityRejection", () => {
	const split = new Set(["perplexity"]);

	it("skips a split provider's 400 rejection of a choice or score question", () => {
		expect(isCapabilityRejection(split, "perplexity", 400, '{"error":{"message":"Invalid request: unsupported schema"}}', "choice")).toBe(true);
		expect(isCapabilityRejection(split, "perplexity", 400, '{"error":{"type":"invalid_request_error"}}', "score")).toBe(true);
	});

	it("fails a generic invalid request that is not the demonstrated rejection", () => {
		// The batched call, or a noul question, is not the documented limit.
		expect(isCapabilityRejection(split, "perplexity", 400, '{"error":{"type":"invalid_request_error"}}')).toBe(false);
		expect(isCapabilityRejection(split, "perplexity", 400, '{"error":{"type":"invalid_request_error"}}', "noul")).toBe(false);
	});

	it("fails any other status from a split provider", () => {
		expect(isCapabilityRejection(split, "perplexity", 500, "invalid request", "choice")).toBe(false);
		expect(isCapabilityRejection(split, "perplexity", 401, "invalid request", "choice")).toBe(false);
		expect(isCapabilityRejection(split, "perplexity", 429, "invalid_request", "choice")).toBe(false);
	});

	it("fails a 400 that is not a schema rejection", () => {
		expect(isCapabilityRejection(split, "perplexity", 400, '{"error":{"message":"context length exceeded"}}', "choice")).toBe(false);
	});

	it("never skips a provider outside the split list", () => {
		expect(isCapabilityRejection(split, "openai", 400, "invalid request", "choice")).toBe(false);
	});
});
