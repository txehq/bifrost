// Counts what a provider-harness run selected, what it will skip for want of a key, and what it
// ran (TXE-7099).
//
// augment-provider-harness gives every request aimed at a keyless provider a guard that records a
// passing "skipped: no key" assertion and skips the request (lib/keyless-providers.mjs). That is
// right for a live run: a secret nobody has provisioned must not turn the run red. It is wrong
// for a fixture run, which needs no vendor key at all: there the only way a row is keyless is
// that augment read the wrong profile (the default one, whose keys are env vars the offline
// runner removed), and the run then exits 0 having sent almost nothing. One such run skipped 12
// of 14 selected rows and read as a pass.
//
// newman's report cannot answer "how many were skipped": a skipped request has no entry under
// run.executions, and the merged report recomputes stats.items from the executions it kept. So
// the count comes from the collection that was selected: the rows carrying the guard are the
// rows that will be skipped, before anything runs.

import { GUARD_MARKER } from "./keyless-providers.mjs";

function walk(items, out = []) {
  for (const item of items || []) {
    if (Array.isArray(item.item)) walk(item.item, out);
    else if (item.request) out.push(item);
  }
  return out;
}

const SKIP_REASON_RE = /skipped: no key \(([^)]*) unset\)/;

// The env var names a guarded request waits for, or null when the request carries no guard.
export function keylessGuardRefs(item) {
  for (const event of item.event || []) {
    if (event.listen !== "prerequest") continue;
    const exec = event.script?.exec || [];
    if (!exec.some((line) => line.includes(GUARD_MARKER))) continue;
    const reason = exec.map((line) => SKIP_REASON_RE.exec(line)?.[1]).find(Boolean);
    return reason || "a key";
  }
  return null;
}

// What the selected collection holds: every request, and the ones a keyless guard will skip,
// grouped by the variables they wait for.
export function countSelection(collection) {
  const requests = walk(collection?.item);
  const keyless = new Map();
  const ids = new Set();
  let skippedNoKey = 0;
  for (const item of requests) {
    if (item.id) ids.add(item.id);
    const refs = keylessGuardRefs(item);
    if (refs === null) continue;
    skippedNoKey++;
    keyless.set(refs, (keyless.get(refs) || 0) + 1);
  }
  return { selected: requests.length, skippedNoKey, keyless, ids };
}

// An execution failed when an assertion failed or the request got no response at all. A 4xx or
// 5xx answer is not a failure by itself: [EXPECT-400] rows assert on exactly that.
function executionFailed(execution) {
  return (execution.assertions || []).some((a) => a.error) || Boolean(execution.requestError) || !execution.response;
}

// What ran, from a newman report (single or merged). A retried request appears once per attempt;
// the last attempt is the request's result, as in lib/newman-merge.jq.
export function countRun(report) {
  const last = new Map();
  let anonymous = 0;
  let anonymousFailed = 0;
  for (const execution of report?.run?.executions || []) {
    const id = execution.item?.id;
    if (id) last.set(id, execution);
    else {
      anonymous++;
      if (executionFailed(execution)) anonymousFailed++;
    }
  }
  let failed = anonymousFailed;
  for (const execution of last.values()) if (executionFailed(execution)) failed++;
  return { run: last.size + anonymous, failed };
}

// The four counts a PR body quotes. skippedOther is what was selected, carries no keyless guard
// and still did not run: a row that skipped itself (its fixture variable is not set, a chained
// variable is missing) or one a request budget refused.
export function summarize(selection, runCounts) {
  const skipped = Math.max(0, selection.selected - runCounts.run);
  const skippedNoKey = Math.min(selection.skippedNoKey, skipped);
  return {
    selected: selection.selected,
    run: runCounts.run,
    failed: runCounts.failed,
    skipped,
    skippedNoKey,
    skippedOther: skipped - skippedNoKey,
  };
}

export function summaryLine(s) {
  return `Harness summary: ${s.selected} selected, ${s.run} run, ${s.skipped} skipped (${s.skippedNoKey} for a missing key, ${s.skippedOther} other), ${s.failed} failed`;
}

// Why a fixture run must not proceed (before the run) or must not read as a pass (after it), or
// "" when it may. A live run is never refused: its skips are the keyless rule working as meant.
export function refusal({ fixture, allowSkipped, selection, summary }) {
  if (!fixture || allowSkipped) return "";
  if (selection.skippedNoKey > 0) {
    const reasons = [...selection.keyless].map(([refs, n]) => `${n} waiting for ${refs}`).join("; ");
    return (
      `${selection.skippedNoKey} of ${selection.selected} selected requests would be skipped for a missing key (${reasons}). ` +
      "A fixture run needs no vendor key, so the augment step read the wrong profile: pass the config the fixture gateway runs with " +
      "(--profile <app-dir>/config.json; make passes APP_DIR's). Set ALLOW_SKIPPED=1 (--allow-skipped) only when the skips are meant."
    );
  }
  if (summary && summary.selected > 0 && summary.run === 0) {
    return (
      `none of the ${summary.selected} selected requests ran: every one skipped itself. ` +
      "A fixture folder skips its rows until its own variable is set (for example vertexEndpointFixture=1); pass it in ENV_FILE or --env-var."
    );
  }
  return "";
}
