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

import { createHash } from "node:crypto";

import { GUARD_MARKER } from "./keyless-providers.mjs";

function walk(items, out = []) {
  for (const item of items || []) {
    if (Array.isArray(item.item)) walk(item.item, out);
    else if (item.request) out.push(item);
  }
  return out;
}

// Gives every request that has none an id that is the same in every newman process.
//
// The source collection carries no item ids, and newman then makes a fresh one each time it loads
// a collection. A parallel run loads the same request more than once: a 429 retry replays it in a
// new process, and a chained row's producer rides along in every shard that needs it. With fresh
// ids each of those is a different request to the report, so a recovered failure stays failed, the
// run counts more requests than were selected, and the surplus hides real skips in the
// selected-minus-run subtraction. The id is a hash of the folder path, the request name and its
// position among same-named siblings, so it is unique, and it is written into the augmented
// collection, which every later step (filter, shards, retries) copies from. An id already present
// is kept.
export function assignStableIds(collection) {
  const seen = new Map();
  const taken = new Set();
  let assigned = 0;
  const visit = (items, folders) => {
    for (const item of items || []) {
      if (Array.isArray(item.item)) {
        visit(item.item, [...folders, item.name || ""]);
        continue;
      }
      if (!item.request) continue;
      if (item.id) {
        taken.add(item.id);
        continue;
      }
      const key = JSON.stringify([...folders, item.name || ""]);
      let id;
      do {
        const ordinal = seen.get(key) || 0;
        seen.set(key, ordinal + 1);
        id = `h-${createHash("sha1").update(`${key}#${ordinal}`).digest("hex").slice(0, 20)}`;
      } while (taken.has(id));
      taken.add(id);
      item.id = id;
      assigned++;
    }
  };
  // Ids already present are reserved first, so a generated one never collides with a later item's.
  for (const item of walk(collection?.item)) if (item.id) taken.add(item.id);
  visit(collection?.item, []);
  return assigned;
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

// What ran, from a newman report (single or merged). A request that ran in more than one process
// (a retry, a chain producer shared by shards) appears once per run under the same id, which
// assignStableIds gives it; the last one is the request's result, as in lib/newman-merge.jq.
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
