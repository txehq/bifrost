import { defineConfig } from "@playwright/test";

// Runs selftest/availability, for scripts/availability.test.mjs (TXE-7733).
export default defineConfig({
	testDir: "./availability",
	retries: 0,
	workers: 1,
	reporter: [["json", { outputFile: process.env.AVAILABILITY_SELFTEST_JSON || "availability-selftest.json" }]],
});
