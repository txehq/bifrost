// Counts a provider-harness run's selected, run, skipped and failed requests, and refuses a
// fixture run that would skip selected rows for a missing key (TXE-7099). See lib/harness-skips.mjs.
//
//   node check-harness-skips.mjs --collection <selected.json> [--env-file <env.json>] [--allow-skipped]
//       Before the run. Prints what is selected and how much of it a keyless guard will skip.
//   node check-harness-skips.mjs --collection <selected.json> --report <newman-report.json> [...]
//       After the run. Prints "Harness summary: N selected, R run, S skipped (...), F failed".
//
// <selected.json> is the augmented collection after the run's own filters (FOLDER, FEATURE,
// PROVIDER, SMOKE), before it is split into passes. A run is a fixture run when its environment
// file sets providerErrorFixture=1 or it runs under scripts/test-offline.sh, the same rule as
// check-gateway-egress.mjs.
// Exit 0: a live run, or a fixture run that skips nothing for a key and ran something.
// Exit 1: a fixture run that must not read as a pass, with the reason. Exit 2: bad usage.
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { isFixtureRun } from "./check-gateway-egress.mjs";
import { countRun, countSelection, refusal, summarize, summaryLine } from "./lib/harness-skips.mjs";
import { readReport } from "./lib/read-report.mjs";

function parseArgs(argv) {
  const args = {};
  for (let i = 0; i < argv.length; i++) {
    if (!argv[i].startsWith("--")) continue;
    const next = argv[i + 1];
    args[argv[i].slice(2)] = next && !next.startsWith("--") ? next : "true";
  }
  return args;
}

export function main(argv, env = process.env, out = console) {
  const args = parseArgs(argv);
  if (!args.collection || args.collection === "true") {
    out.error("usage: node check-harness-skips.mjs --collection <selected.json> [--report <newman-report.json>] [--env-file <env.json>] [--allow-skipped]");
    return 2;
  }
  const selection = countSelection(JSON.parse(readFileSync(args.collection, "utf8")));
  const envFile = args["env-file"] && args["env-file"] !== "true" ? args["env-file"] : "";
  const fixture = env.BIFROST_TEST_OFFLINE === "1" || (envFile ? isFixtureRun(readFileSync(envFile, "utf8")) : false);
  const allowSkipped = args["allow-skipped"] === "true" || env.ALLOW_SKIPPED === "1";

  let summary = null;
  if (args.report && args.report !== "true") {
    summary = summarize(selection, countRun(readReport(args.report)));
    out.log(summaryLine(summary));
  } else {
    out.log(`Harness selection: ${selection.selected} requests selected, ${selection.skippedNoKey} will be skipped for a missing key`);
  }
  const reason = refusal({ fixture, allowSkipped, selection, summary });
  if (reason) {
    out.error(`[harness-skips] fixture run refused: ${reason}`);
    return 1;
  }
  return 0;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  process.exit(main(process.argv.slice(2)));
}
