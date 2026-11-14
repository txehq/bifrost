import { expect, test } from "../../core/fixtures/base.fixture";

// The sample suite scripts/quarantine.test.mjs runs against generated quarantine lists, to pin the
// quarantine rules (TXE-7385). It lives outside features/, so the UI suite never runs it.
test.describe("Quarantine rules", () => {
	test("a listed test that fails", () => {
		expect(1, "this test fails on purpose").toBe(2);
	});

	test("a listed test that passes", () => {
		expect(1).toBe(1);
	});

	test("an unlisted test that passes", () => {
		expect(1).toBe(1);
	});
});
