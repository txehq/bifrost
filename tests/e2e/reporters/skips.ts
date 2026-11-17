import type { FullConfig, FullResult, Reporter, Suite, TestCase } from "@playwright/test/reporter";
import { existsSync } from "fs";
import { relative } from "path";
import { E2E_ROOT } from "../plan";
import { isOfflineRun } from "../quarantine";
import { loadSkips, skipKey } from "../skips";

/**
 * Keeps the offline run honest about skips (skips.ts, TXE-7733), in an offline run only:
 *   - a test that skips and is not in offline-skips.json fails the run, naming it and its reason;
 *   - a listed test that ran without skipping fails the run: the entry hides nothing, remove it;
 *   - a listed test whose file is in this run but which this run does not have fails it (renamed or
 *     removed), but only in a run of whole files: --grep and --rerun leave tests out on purpose;
 *   - a listed file that does not exist fails every run, whichever shard it is.
 * A skip is a test whose last attempt was skipped with a skip or fixme annotation, from test.skip(),
 * test.fixme() or their runtime forms. A test serial mode skipped after an earlier test failed has
 * no such annotation: that failure already fails the run.
 */
const isTestFilter = (arg: string) => /^(-g|--grep|--grep-invert|--last-failed|--only-changed)(=|$)/.test(arg) || /\.ts:\d+(:\d+)?$/.test(arg);
const SKIP_TYPES = new Set(["skip", "fixme"]);

const keyOf = (test: TestCase) => skipKey(relative(E2E_ROOT, test.location.file), test.titlePath().slice(3).join(" › "));

export default class SkipsReporter implements Reporter {
	private problems: string[] = [];
	private suite: Suite | undefined;
	private skips = new Map<string, string>();

	onBegin(_config: FullConfig, suite: Suite) {
		if (!isOfflineRun()) return;
		this.suite = suite;
		this.skips = loadSkips();
		const wholeFiles = process.env.E2E_WHOLE_FILES !== "0" && !process.argv.some(isTestFilter);
		const present = new Set<string>();
		const files = new Set<string>();
		for (const test of suite.allTests()) {
			const file = relative(E2E_ROOT, test.location.file);
			if (file.startsWith("core/")) continue;
			files.add(file);
			present.add(keyOf(test));
		}
		for (const [key, ticket] of this.skips) {
			const file = key.split(" :: ")[0];
			if (!existsSync(`${E2E_ROOT}/${file}`)) this.problems.push(`offline-skips.json (${ticket}) lists ${key}, but ${file} does not exist; remove the entry`);
			else if (wholeFiles && files.has(file) && !present.has(key)) this.problems.push(`offline-skips.json (${ticket}) lists ${key}, which this run does not have; remove the entry`);
		}
	}

	async onEnd(_result: FullResult): Promise<{ status?: FullResult["status"] } | undefined> {
		if (!this.suite) return undefined;
		for (const test of this.suite.allTests()) {
			const last = test.results.at(-1);
			if (!last) continue; // not run: filtered out, or the run stopped first
			const key = keyOf(test);
			const ticket = this.skips.get(key);
			const skip = [...test.annotations, ...last.annotations].find((a) => SKIP_TYPES.has(a.type));
			if (last.status === "skipped" && skip && !ticket) {
				this.problems.push(`${key} skipped offline (${skip.description || "no reason given"}) and is not in offline-skips.json; make it run, or list it under the ticket that records why it cannot`);
			} else if (last.status !== "skipped" && ticket) {
				this.problems.push(`offline-skips.json (${ticket}) lists ${key}, which ran without skipping; remove the entry`);
			}
		}
		for (const problem of this.problems) console.error(`[skips] ${problem}`);
		return this.problems.length > 0 ? { status: "failed" } : undefined;
	}
}
