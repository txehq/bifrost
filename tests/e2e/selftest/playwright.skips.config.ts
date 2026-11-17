import { defineConfig } from "@playwright/test";

// Runs selftest/skips with the offline skip reporter, for scripts/skips.test.mjs (TXE-7733).
export default defineConfig({
	testDir: "./skips",
	retries: 0,
	workers: 1,
	reporter: [["json", { outputFile: process.env.SKIPS_SELFTEST_JSON || "skips-selftest.json" }], ["../reporters/skips.ts"]],
});
