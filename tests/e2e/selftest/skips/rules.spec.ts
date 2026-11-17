import { expect, test } from "../../core/fixtures/base.fixture";

// The sample suite scripts/skips.test.mjs runs against generated offline skip lists, to pin the
// offline skip rules (skips.ts, TXE-7733). It lives outside features/, so the UI suite never runs it.
// It uses the suite's base fixture, as every spec does, so it needs Playwright's Chromium.
test.describe("Skip rules", () => {
	test("a test that skips at run time", () => {
		test.skip(true, "the sample skips on purpose"); // skip-ok: the selftest's subject
	});

	test("a test that runs", () => {
		expect(1).toBe(1);
	});
});

// A test that serial mode skips after an earlier failure is not a skip decision: the failure
// already fails the run, so the skip gate must not report it as an unlisted skip.
test.describe("Serial knock-on", () => {
	test.describe.configure({ mode: "serial" });

	test("a serial test that fails", () => {
		expect(1, "this test fails on purpose").toBe(2);
	});

	test("a serial test after the failure", () => {
		expect(1).toBe(1);
	});
});
