// Tests for check-harness-skips.mjs and lib/harness-skips.mjs (TXE-7099). Run directly:
// `node check-harness-skips.test.mjs`; needs `newman` on PATH, no gateway, no network, no
// credentials. The offline API-collection suites find and run it.
//
// The case it exists for: a fixture run whose augment step read the default profile. Every key in
// that profile is an env var the offline runner removed, so each selected row aimed at a provider
// is skipped with a passing "skipped: no key" assertion, newman exits 0, and the run reads as a
// pass having sent nothing. Reproduced here with the real collection, the real augment and filter
// steps and the real newman, on folder 161 (the Vertex endpoint fixture's folder).
import assert from "node:assert";
import { execFileSync, spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { assignStableIds, countRun, countSelection, keylessGuardRefs, refusal, summarize, summaryLine } from "./lib/harness-skips.mjs";
import { bodyModelProvider } from "./lib/keyless-providers.mjs";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const REPO = path.resolve(HERE, "../../../..");
const SOURCE = path.join(REPO, "tests/e2e/api/collections/provider-harness.json");
const AUGMENT = path.join(HERE, "augment-provider-harness.mjs");
const FILTER = path.join(HERE, "filter-collection.mjs");
const CHECK = path.join(HERE, "check-harness-skips.mjs");
const NEWMAN = execFileSync("sh", ["-c", "command -v newman"], { encoding: "utf8" }).trim();
const FOLDER = "161. Vertex endpoint selection";

const dir = fs.mkdtempSync(path.join(os.tmpdir(), "harness-skips-"));
let passed = 0;
function test(name, fn) {
  fn();
  passed++;
  console.log(`  ok - ${name}`);
}

// No credential and no offline marker: each case adds what it needs.
const bareEnv = { PATH: process.env.PATH, HOME: process.env.HOME || dir };

function node(script, args, env = bareEnv) {
  return spawnSync(process.execPath, [script, ...args], { env, encoding: "utf8", maxBuffer: 64 * 1024 * 1024 });
}

// The real augment step with the given profile ("" for its default), then the run's own filter.
function selectFolder(name, profile) {
  const augmented = path.join(dir, `${name}-augmented.json`);
  const selected = path.join(dir, `${name}-selected.json`);
  const aug = node(AUGMENT, ["--source", SOURCE, "--out", augmented, ...(profile ? ["--profile", profile] : [])]);
  assert.strictEqual(aug.status, 0, `augment failed: ${aug.stderr}`);
  const filtered = node(FILTER, ["--source", augmented, "--out", selected, "--folder", FOLDER]);
  assert.strictEqual(filtered.status, 0, `filter failed: ${filtered.stderr}`);
  return selected;
}

function requests(collectionPath) {
  const out = [];
  const walk = (items) => {
    for (const item of items || []) {
      if (Array.isArray(item.item)) walk(item.item);
      else if (item.request) out.push(item);
    }
  };
  walk(JSON.parse(fs.readFileSync(collectionPath, "utf8")).item);
  return out;
}

function runNewman(collectionPath, name) {
  const report = path.join(dir, `${name}-report.json`);
  // Port 9 (discard) refuses the connection, so a request that is sent, not skipped, shows up
  // as a failed execution.
  const run = spawnSync(NEWMAN, ["run", collectionPath, "--env-var", "baseUrl=http://127.0.0.1:9", "--reporters", "json", "--reporter-json-export", report], {
    env: bareEnv,
    encoding: "utf8",
  });
  return { status: run.status, report };
}

const offline = { ...bareEnv, BIFROST_TEST_OFFLINE: "1" };

// ---- the counting rules, on built reports and collections --------------------------------

const guarded = (name) => ({
  id: name,
  name,
  request: { method: "POST", url: "{{baseUrl}}/v1/chat/completions" },
  event: [{ listen: "prerequest", script: { exec: ["// [keyless-provider-skip]", `pm.test("${name}: skipped: no key (ACME_API_KEY unset)", function () {});`, "pm.execution.skipRequest();"] } }],
});
const plain = (name) => ({ id: name, name, request: { method: "POST", url: "{{baseUrl}}/v1/chat/completions" } });
const execution = (id, { code = 200, assertionError = false, noResponse = false } = {}) => ({
  item: { id, name: id },
  ...(noResponse ? { requestError: { code: "ECONNREFUSED" } } : { response: { code } }),
  assertions: [{ assertion: "a", ...(assertionError ? { error: { name: "AssertionError" } } : {}) }],
});

test("a guarded request is counted with the variables it waits for; an unguarded one is not", () => {
  assert.strictEqual(keylessGuardRefs(guarded("a")), "ACME_API_KEY");
  assert.strictEqual(keylessGuardRefs(plain("b")), null);
  const selection = countSelection({ item: [{ name: "folder", item: [guarded("a"), guarded("b"), plain("c")] }] });
  assert.strictEqual(selection.selected, 3);
  assert.strictEqual(selection.skippedNoKey, 2);
  assert.deepStrictEqual([...selection.keyless], [["ACME_API_KEY", 2]]);
});

test("run counts: a 4xx answer whose assertions pass is not a failure; a failed assertion or no response is", () => {
  const counts = countRun({ run: { executions: [execution("a", { code: 400 }), execution("b", { assertionError: true }), execution("c", { noResponse: true })] } });
  assert.deepStrictEqual(counts, { run: 3, failed: 2 });
});

test("run counts: a retried request counts once, with its last attempt's result", () => {
  const counts = countRun({ run: { executions: [execution("a", { assertionError: true }), execution("b"), execution("a")] } });
  assert.deepStrictEqual(counts, { run: 2, failed: 0 });
});

test("run counts: a request whose script threw outside pm.test is failed, from run.failures", () => {
  const report = { run: { executions: [execution("a"), execution("b")], failures: [{ source: { id: "b", name: "b" }, error: { name: "TypeError" } }, { error: { name: "Error" } }] } };
  assert.deepStrictEqual(countRun(report), { run: 2, failed: 1 });
});

test("the summary splits skipped rows into missing-key and other, and prints all four counts", () => {
  const selection = countSelection({ item: [guarded("a"), guarded("b"), plain("c"), plain("d"), plain("e")] });
  const s = summarize(selection, countRun({ run: { executions: [execution("c"), execution("d", { assertionError: true })] } }));
  assert.deepStrictEqual(s, { selected: 5, run: 2, failed: 1, skipped: 3, skippedNoKey: 2, skippedOther: 1 });
  assert.strictEqual(summaryLine(s), "Harness summary: 5 selected, 2 run, 3 skipped (2 for a missing key, 1 other), 1 failed");
});

test("only a fixture run is refused, and ALLOW_SKIPPED lets it through", () => {
  const selection = countSelection({ item: [guarded("a"), plain("b")] });
  assert.match(refusal({ fixture: true, allowSkipped: false, selection }), /1 of 2 selected requests would be skipped for a missing key \(1 waiting for ACME_API_KEY\)/);
  assert.strictEqual(refusal({ fixture: false, allowSkipped: false, selection }), "");
  assert.strictEqual(refusal({ fixture: true, allowSkipped: true, selection }), "");
});

test("a fixture run in which nothing ran is refused even with no keyless row", () => {
  const selection = countSelection({ item: [plain("a"), plain("b")] });
  const none = summarize(selection, countRun({ run: { executions: [] } }));
  assert.match(refusal({ fixture: true, allowSkipped: false, selection, summary: none }), /none of the 2 selected requests ran/);
  const one = summarize(selection, countRun({ run: { executions: [execution("a")] } }));
  assert.strictEqual(refusal({ fixture: true, allowSkipped: false, selection, summary: one }), "");
});

// ---- the real collection, the real steps ---------------------------------------------------

const wrongProfile = selectFolder("default-profile", "");
const wrongProfileAugmented = path.join(dir, "default-profile-augmented.json");
const all = requests(wrongProfile);
const aimedAtVertex = all.filter((item) => bodyModelProvider(item) === "vertex").length;
assert.ok(all.length > 0 && aimedAtVertex > 0, `folder "${FOLDER}" no longer holds vertex rows: the test needs another fixture folder`);

test("default profile, no credentials: every vertex row of the fixture folder carries the keyless guard", () => {
  const selection = countSelection(JSON.parse(fs.readFileSync(wrongProfile, "utf8")));
  assert.strictEqual(selection.selected, all.length);
  assert.strictEqual(selection.skippedNoKey, aimedAtVertex);
});

test("before the run: a fixture run on that selection is refused, naming the count, the variables and the profile flag", () => {
  const res = node(CHECK, ["--collection", wrongProfile], offline);
  assert.strictEqual(res.status, 1, res.stdout + res.stderr);
  assert.match(res.stdout, new RegExp(`${all.length} requests selected, ${aimedAtVertex} will be skipped for a missing key`));
  assert.match(res.stderr, new RegExp(`${aimedAtVertex} of ${all.length} selected requests would be skipped for a missing key`));
  assert.match(res.stderr, /VERTEX_/);
  assert.match(res.stderr, /--profile <app-dir>\/config\.json/);
});

test("before the run: the same selection passes with --allow-skipped, with ALLOW_SKIPPED=1, and on a live run", () => {
  assert.strictEqual(node(CHECK, ["--collection", wrongProfile, "--allow-skipped"], offline).status, 0);
  assert.strictEqual(node(CHECK, ["--collection", wrongProfile], { ...offline, ALLOW_SKIPPED: "1" }).status, 0);
  const live = node(CHECK, ["--collection", wrongProfile]);
  assert.strictEqual(live.status, 0, live.stderr);
  assert.match(live.stdout, /will be skipped for a missing key/);
});

test("a fixture run named by its environment file, outside the offline runner, is refused the same way", () => {
  const envFile = path.join(dir, "fixture-env.json");
  fs.writeFileSync(envFile, JSON.stringify({ values: [{ key: "providerErrorFixture", value: "1", enabled: true }] }));
  assert.strictEqual(node(CHECK, ["--collection", wrongProfile, "--env-file", envFile]).status, 1);
});

test("stable ids: every request gets a unique one, the same on every call, and an existing id is kept", () => {
  const build = () => ({ item: [{ name: "f", item: [{ name: "same", request: {} }, { name: "same", request: {} }, { id: "kept", name: "c", request: {} }] }, { name: "g", item: [{ name: "same", request: {} }] }] });
  const a = build();
  const b = build();
  assert.strictEqual(assignStableIds(a), 3);
  assignStableIds(b);
  const ids = (c) => c.item.flatMap((f) => f.item.map((i) => i.id));
  assert.deepStrictEqual(ids(a), ids(b));
  assert.strictEqual(new Set(ids(a)).size, 4);
  assert.strictEqual(a.item[0].item[2].id, "kept");
  assert.strictEqual(assignStableIds(a), 0, "a second pass assigns nothing");
});

test("stable ids survive newman: the same request run in two processes is counted once, with its last result", () => {
  const twice = { info: { name: "ids", schema: "https://schema.getpostman.com/json/collection/v2.1.0/collection.json" }, item: [{ name: "f", item: [plain("a"), plain("b")] }] };
  for (const item of twice.item[0].item) delete item.id;
  assignStableIds(twice);
  const file = path.join(dir, "ids.json");
  fs.writeFileSync(file, JSON.stringify(twice));
  const first = JSON.parse(fs.readFileSync(runNewman(file, "ids-first").report, "utf8"));
  const second = JSON.parse(fs.readFileSync(runNewman(file, "ids-second").report, "utf8"));
  assert.deepStrictEqual(first.run.executions.map((e) => e.item.id), twice.item[0].item.map((i) => i.id));
  assert.deepStrictEqual(second.run.executions.map((e) => e.item.id), first.run.executions.map((e) => e.item.id));
  // A shard plus a retry of it: four executions, two requests.
  const merged = { run: { executions: [...first.run.executions, ...second.run.executions] } };
  assert.strictEqual(countRun(merged).run, 2);
  assert.strictEqual(summarize(countSelection(twice), countRun(merged)).skipped, 0);
});

test("the augment step writes an id on every request of the real collection, all different", () => {
  const ids = requests(wrongProfileAugmented).map((item) => item.id);
  assert.ok(ids.length > 1000 && ids.every(Boolean), "a request has no id");
  assert.strictEqual(new Set(ids).size, ids.length);
});

test("a real newman report: a test script that throws before its assertions counts as a failed request", () => {
  const throwing = { info: { name: "throws", schema: "https://schema.getpostman.com/json/collection/v2.1.0/collection.json" }, item: [{ name: "f", item: [
    { name: "throws", request: { method: "GET", url: "{{baseUrl}}/x" }, event: [{ listen: "prerequest", script: { exec: ["undefinedFunctionCall();"] } }] },
  ] }] };
  assignStableIds(throwing);
  const file = path.join(dir, "throws.json");
  fs.writeFileSync(file, JSON.stringify(throwing));
  const run = runNewman(file, "throws");
  const report = JSON.parse(fs.readFileSync(run.report, "utf8"));
  assert.ok(report.run.failures.length > 0, "newman recorded the script error");
  assert.ok(countRun(report).failed >= 1, JSON.stringify(countRun(report)));
});

const wrongRun = runNewman(wrongProfile, "default-profile");

test("newman exits 0 on that selection although it sent nothing: the false pass", () => {
  assert.strictEqual(wrongRun.status, 0);
  assert.deepStrictEqual(JSON.parse(fs.readFileSync(wrongRun.report, "utf8")).run.executions, []);
});

test("after the run: the summary reports every row skipped, and the fixture run fails", () => {
  const res = node(CHECK, ["--collection", wrongProfile, "--report", wrongRun.report], offline);
  assert.strictEqual(res.status, 1, res.stdout + res.stderr);
  const other = all.length - aimedAtVertex;
  assert.match(res.stdout, new RegExp(`Harness summary: ${all.length} selected, 0 run, ${all.length} skipped \\(${aimedAtVertex} for a missing key, ${other} other\\), 0 failed`));
  const live = node(CHECK, ["--collection", wrongProfile, "--report", wrongRun.report]);
  assert.strictEqual(live.status, 0, "a live run prints the same summary and is not refused");
  assert.match(live.stdout, /Harness summary: /);
});

// The profile a fixture gateway runs with carries literal keys, so nothing is keyless.
const fixtureProfile = path.join(dir, "fixture-config.json");
fs.writeFileSync(fixtureProfile, JSON.stringify({ providers: { vertex: { keys: [{ name: "vertex-global", value: "fixture-api-key", models: ["*"], weight: 1 }] } } }));
const rightProfile = selectFolder("fixture-profile", fixtureProfile);

test("with the fixture's own profile nothing is keyless and the fixture run may start", () => {
  const res = node(CHECK, ["--collection", rightProfile], offline);
  assert.strictEqual(res.status, 0, res.stderr);
  assert.match(res.stdout, new RegExp(`${all.length} requests selected, 0 will be skipped for a missing key`));
});

test("with the right profile but the folder's own switch left off, every row skips itself and the run still fails", () => {
  const run = runNewman(rightProfile, "fixture-profile");
  assert.strictEqual(run.status, 0, "newman itself still exits 0");
  const res = node(CHECK, ["--collection", rightProfile, "--report", run.report], offline);
  assert.strictEqual(res.status, 1, res.stdout + res.stderr);
  assert.match(res.stdout, new RegExp(`Harness summary: ${all.length} selected, 0 run, ${all.length} skipped \\(0 for a missing key, ${all.length} other\\), 0 failed`));
  assert.match(res.stderr, /none of the \d+ selected requests ran/);
});

test("bad usage exits 2", () => {
  assert.strictEqual(node(CHECK, []).status, 2);
});

fs.rmSync(dir, { recursive: true, force: true });
console.log(`${passed} passed`);
