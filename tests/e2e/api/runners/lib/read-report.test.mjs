// Unit tests for the size-safe newman report reader. Run directly:
// `node read-report.test.mjs`. No test framework needed (the tests/e2e/api dir has
// no test runner configured). Requires jq, exactly like the code under test, and newman on PATH
// for the merge cases (TXE-7103), which run real newman against a loopback server.
import assert from "node:assert";
import { mkdtempSync, writeFileSync, readFileSync, statSync, rmSync } from "node:fs";
import { execFileSync, spawn } from "node:child_process";
import http from "node:http";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { sanitizeTo, readReport } from "./read-report.mjs";
import { countRun } from "./harness-skips.mjs";

const MERGE_PROGRAM = join(dirname(fileURLToPath(import.meta.url)), "newman-merge.jq");

let passed = 0;
function test(name, fn) {
  fn();
  passed++;
  console.log(`  ok - ${name}`);
}

// A single execution whose response is an image-sized Buffer: 30000 elements, past the
// merge program's 20000-element cap. This is the shape that blew a full sweep's merged
// report to 686MB - not because of the byte count, but because jq pretty-prints each
// element on its own indented line ("        255,\n", ~13 bytes) instead of "255," (4).
const STREAM_LEN = 30000;
const fixture = {
  collection: { info: { name: "fixture" }, item: [] },
  environment: {},
  run: {
    executions: [
      {
        id: "exec-1",
        item: { id: "item-1", name: "image gen" },
        request: { method: "POST", url: { path: ["v1", "images"] }, body: { mode: "raw", raw: "{}" } },
        response: {
          code: 200,
          status: "OK",
          stream: { type: "Buffer", data: Array.from({ length: STREAM_LEN }, (_, i) => i % 256) },
        },
        assertions: [{ assertion: "status 200" }],
      },
    ],
    failures: [],
    stats: { requests: { total: 1, failed: 0 } },
    timings: {},
  },
};

const dir = mkdtempSync(join(tmpdir(), "read-report-test-"));
const src = join(dir, "newman-report.json");
const dest = join(dir, ".newman-report.slim.json");
writeFileSync(src, JSON.stringify(fixture));

try {
  test("sanitizeTo writes the slimmed report as compact JSON (one line)", () => {
    sanitizeTo(src, dest);
    const out = readFileSync(dest, "utf8");
    // jq's compact mode ends the document with a single trailing newline; anything
    // beyond that means pretty-printing crept back in.
    const lines = out.trimEnd().split("\n").length;
    assert.strictEqual(lines, 1, `expected one line of compact JSON, got ${lines} lines`);
  });

  test("sanitizeTo keeps the existing stream cap (head + tail, truncated flag)", () => {
    const parsed = JSON.parse(readFileSync(dest, "utf8"));
    const stream = parsed.run.executions[0].response.stream;
    assert.strictEqual(stream.type, "Buffer");
    assert.strictEqual(stream.data.length, 20000);
    assert.strictEqual(stream.truncated, true);
    assert.strictEqual(stream.truncatedMiddle, true);
    // Head is the first 12000 elements, tail the last 8000 - both ends must survive.
    assert.strictEqual(stream.data[0], 0);
    assert.strictEqual(stream.data[11999], 11999 % 256);
    assert.strictEqual(stream.data[12000], (STREAM_LEN - 8000) % 256);
    assert.strictEqual(stream.data[19999], (STREAM_LEN - 1) % 256);
  });

  test("compact output is materially smaller than the same program pretty-printed", () => {
    // The same merge program without -c: this is what every writer used to do, and what
    // pushed a 235MB report to 686MB on disk.
    const pretty = execFileSync("jq", ["-s", "-f", MERGE_PROGRAM, src], { maxBuffer: 64 * 1024 * 1024 });
    const compactSize = statSync(dest).size;
    assert.ok(
      compactSize * 2 < pretty.length,
      `compact ${compactSize}B should be well under half of pretty ${pretty.length}B`
    );
  });

  test("readReport parses a report that fits in a string without slimming", () => {
    const report = readReport(src);
    assert.strictEqual(report.run.executions.length, 1);
    assert.strictEqual(report.run.executions[0].response.stream.data.length, STREAM_LEN);
  });
  await mergeKeepsScriptErrors();
} finally {
  rmSync(dir, { recursive: true, force: true });
}

console.log(`\n${passed} tests passed`);

// TXE-7103: a script that throws outside pm.test on a 200 is recorded by newman only under
// run.failures; the execution has no failed assertion. The merge kept a sourced failure only when
// the surviving execution failed its `failed` predicate, so it dropped this one, and the summary
// read 0 failed. Real newman reports, from a loopback server, go through the real jq program.
async function mergeKeepsScriptErrors() {
  const server = http.createServer((req, res) => {
    res.writeHead(req.url === "/429" ? 429 : 200, { "Content-Type": "application/json" });
    res.end("{}");
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const base = `http://127.0.0.1:${server.address().port}`;
  const newmanBin = execFileSync("sh", ["-c", "command -v newman"], { encoding: "utf8" }).trim();
  const row = (id, path, exec) => ({
    id,
    name: id,
    request: { method: "GET", url: `${base}${path}` },
    event: [{ listen: "test", script: { type: "text/javascript", exec } }],
  });
  const statusOk = "pm.test('status 200', function () { pm.response.to.have.status(200); });";
  const runNewman = (name, items) =>
    new Promise((resolve) => {
      const collection = join(dir, `${name}.collection.json`);
      const report = join(dir, `${name}.report.json`);
      writeFileSync(collection, JSON.stringify({ info: { name, schema: "https://schema.getpostman.com/json/collection/v2.1.0/collection.json" }, item: items }));
      const child = spawn(newmanBin, ["run", collection, "--reporters", "json", "--reporter-json-export", report], { stdio: "ignore" });
      child.on("exit", () => resolve(JSON.parse(readFileSync(report, "utf8"))));
    });
  const merge = (...reports) => {
    const paths = reports.map((r, i) => {
      const p = join(dir, `merge-in-${i}-${Math.random()}.json`);
      writeFileSync(p, JSON.stringify(r));
      return p;
    });
    return JSON.parse(execFileSync("jq", ["-c", "-s", "-f", MERGE_PROGRAM, ...paths], { encoding: "utf8" }));
  };

  try {
    // The main pass: a script error on a 200, a clean row, and a 429.
    const main = await runNewman("main", [
      row("script-error", "/ok", [statusOk, "notDefinedAnywhere();"]),
      row("clean", "/ok", [statusOk]),
      row("rate-limited", "/429", [statusOk]),
    ]);
    // The retry pass: the same ids, every one clean.
    const retry = await runNewman("retry", [row("script-error", "/ok", [statusOk]), row("rate-limited", "/ok", [statusOk])]);

    test("newman records the script error only under run.failures (the shape the merge must keep)", () => {
      const exec = main.run.executions.find((e) => e.item.id === "script-error");
      assert.strictEqual(exec.response.code, 200);
      assert.ok(!(exec.assertions || []).some((a) => a.error), "no failed assertion on the execution");
      assert.ok(main.run.failures.some((f) => f.source?.id === "script-error" && !/Assertion/i.test(f.error?.name || "")), JSON.stringify(main.run.failures.map((f) => f.error?.name)));
    });

    test("the merge keeps a script error on a 200, and the summary counts it failed", () => {
      const merged = merge(main);
      const sources = merged.run.failures.map((f) => f.source?.id);
      assert.ok(sources.includes("script-error"), `failures: ${JSON.stringify(sources)}`);
      assert.deepStrictEqual(countRun(merged), { run: 3, failed: 2 }, "script-error and rate-limited fail");
      assert.strictEqual(merged.run.stats.requests.failed, 2);
    });

    test("an attempt a later retry superseded is dropped, script error or 429 alike", () => {
      const merged = merge(main, retry);
      assert.deepStrictEqual(merged.run.failures.map((f) => f.source?.id), [], "both recovered on retry");
      assert.deepStrictEqual(countRun(merged), { run: 3, failed: 0 });
      assert.strictEqual(merged.run.stats.requests.failed, 0);
      assert.ok(!merged.run.executions.some((e) => "__r" in e), "the report tag is not written out");
    });

    test("a script error in the attempt that survives is kept even after a clean earlier pass", () => {
      const merged = merge(retry, main); // main last: its attempts are the ones that count
      assert.ok(merged.run.failures.some((f) => f.source?.id === "script-error"));
      assert.deepStrictEqual(countRun(merged), { run: 3, failed: 2 });
    });
  } finally {
    server.close();
  }
}
