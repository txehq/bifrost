import type { FullConfig, FullResult, Reporter, Suite } from "@playwright/test/reporter";
import { existsSync, readFileSync } from "fs";
import { relative } from "path";
import { E2E_ROOT } from "../plan";
import { isOfflineRun, loadQuarantine, quarantineKey } from "../quarantine";

/**
 * Keeps the offline quarantine (quarantine.ts) honest, in an offline run only (TXE-7385):
 *   - an entry whose file is in this run but whose test is not fails the run: the test was renamed
 *     or removed, so the entry quarantines nothing;
 *   - an entry whose file does not exist fails every run, whichever shard it is;
 *   - a spec file in this run that does not import the base fixture fails the run: the fixture is
 *     what marks a quarantined test as expected to fail, so such a spec would never be quarantined.
 * A listed test that passes is failed by Playwright itself (testInfo.fail in the base fixture).
 */
export default class QuarantineReporter implements Reporter {
	private problems: string[] = [];

	onBegin(_config: FullConfig, suite: Suite) {
		if (!isOfflineRun()) return;
		const quarantine = loadQuarantine();
		const present = new Set<string>();
		const files = new Set<string>();
		for (const test of suite.allTests()) {
			const file = relative(E2E_ROOT, test.location.file);
			// core/ holds the login setup, which no entry names and no quarantine applies to.
			if (file.startsWith("core/")) continue;
			files.add(file);
			// titlePath is [root, project, file, ...describes, title]; the project's title may be empty.
			present.add(quarantineKey(file, test.titlePath().slice(3).join(" › ")));
		}
		for (const [key, ticket] of quarantine) {
			const file = key.split(" :: ")[0];
			// A file missing from the checkout fails in every shard; one in another shard is that shard's to check.
			if (!existsSync(`${E2E_ROOT}/${file}`)) this.problems.push(`quarantine.json (${ticket}) lists ${key}, but ${file} does not exist; remove the entry`);
			else if (files.has(file) && !present.has(key)) this.problems.push(`quarantine.json (${ticket}) lists ${key}, which this run does not have; remove the entry`);
		}
		for (const file of files) {
			if (!/from ["'][./]*core\/fixtures\/base\.fixture["']/.test(readFileSync(`${E2E_ROOT}/${file}`, "utf8"))) {
				this.problems.push(`${file} does not import core/fixtures/base.fixture, so the offline quarantine cannot apply to it`);
			}
		}
		for (const problem of this.problems) console.error(`[quarantine] ${problem}`);
	}

	async onEnd(_result: FullResult): Promise<{ status?: FullResult["status"] } | undefined> {
		return this.problems.length > 0 ? { status: "failed" } : undefined;
	}
}
