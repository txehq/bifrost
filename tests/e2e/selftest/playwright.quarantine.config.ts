import { defineConfig } from "@playwright/test";

// Runs selftest/quarantine with the quarantine reporter, for scripts/quarantine.test.mjs (TXE-7385).
export default defineConfig({
	testDir: "./quarantine",
	retries: 0,
	workers: 1,
	reporter: [["json", { outputFile: process.env.QUARANTINE_SELFTEST_JSON || "quarantine-selftest.json" }], ["../reporters/quarantine.ts"]],
});
