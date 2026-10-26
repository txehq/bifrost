// Tests for newman-exit-when-done.cjs (TXE-5180). Run directly:
// `node newman-exit-when-done.test.mjs`; needs `newman` on PATH, nothing else (a local
// HTTP server stands in for the gateway). test-api-integrations.sh runs it before the
// API collections.
import assert from "node:assert";
import { spawn, execFileSync } from "node:child_process";
import fs from "node:fs";
import http from "node:http";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const HOOK = path.join(path.dirname(fileURLToPath(import.meta.url)), "newman-exit-when-done.cjs");
const NEWMAN = execFileSync("sh", ["-c", "command -v newman"], { encoding: "utf8" }).trim();
const GRACE_MS = 1500;
const dir = fs.mkdtempSync(path.join(os.tmpdir(), "newman-exit-"));

// /ok answers 200; /hang accepts the request and never answers.
const server = http.createServer((req, res) => {
  if (req.url === "/hang") return;
  res.writeHead(200, { "content-type": "application/json" }).end("{}");
});
await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
const base = `http://127.0.0.1:${server.address().port}`;

function collection(name, urlPath, expectedStatus) {
  const file = path.join(dir, `${name}.json`);
  fs.writeFileSync(file, JSON.stringify({
    info: { name, schema: "https://schema.getpostman.com/json/collection/v2.1.0/collection.json" },
    item: [{
      name: "request",
      request: { method: "GET", url: `${base}${urlPath}` },
      event: [{ listen: "test", script: { exec: [`pm.test("status", () => pm.expect(pm.response.code).to.eql(${expectedStatus}));`] } }],
    }],
  }));
  return file;
}

// A preload that holds the process open after the run, as the leaked handle did on CI.
function preload(name, source) {
  const file = path.join(dir, `${name}.cjs`);
  fs.writeFileSync(file, source);
  return file;
}
const leakTimer = preload("leak-timer", "setInterval(() => {}, 1000);");
// A peer that accepts and never reads: a large write to it stays pending.
const sink = net.createServer((socket) => socket.pause());
await new Promise((resolve) => sink.listen(0, "127.0.0.1", resolve));
const pendingWrite = preload("pending-write",
  `const s = require("net").connect(${sink.address().port}, "127.0.0.1", () => s.write(Buffer.alloc(64 * 1024 * 1024)));`);

const passing = collection("passing", "/ok", 200);
const failing = collection("failing", "/ok", 500);
const neverCompletes = collection("never-completes", "/hang", 200);

// Runs the newman CLI with the hook; resolves {code, signal, ms, stderr}. killAfterMs
// bounds a run expected never to finish.
function runNewman({ collectionFile, preloads = [], extraArgs = [], killAfterMs = 30000 }) {
  const args = [...preloads.flatMap((p) => ["--require", p]), "--require", HOOK, NEWMAN, "run", collectionFile, "-r", "cli", ...extraArgs];
  return new Promise((resolve) => {
    const started = Date.now();
    const child = spawn(process.execPath, ["--no-deprecation", ...args], {
      env: { ...process.env, NEWMAN_EXIT_GRACE_MS: String(GRACE_MS) },
      stdio: ["ignore", "ignore", "pipe"],
    });
    let stderr = "";
    child.stderr.on("data", (d) => { stderr += d; });
    const killer = setTimeout(() => child.kill("SIGKILL"), killAfterMs);
    child.on("exit", (code, signal) => {
      clearTimeout(killer);
      resolve({ code, signal, ms: Date.now() - started, stderr });
    });
  });
}

const HOOK_LINE = "[newman-exit-when-done]";
let passed = 0;
async function test(name, fn) {
  await fn();
  passed++;
  console.log(`  ok - ${name}`);
}

try {
  await test("a passing run that exits by itself is untouched and exits 0", async () => {
    const r = await runNewman({ collectionFile: passing });
    assert.strictEqual(r.code, 0);
    assert.ok(!r.stderr.includes(HOOK_LINE), r.stderr);
  });

  await test("a failing assertion that exits by itself is untouched and exits 1", async () => {
    const r = await runNewman({ collectionFile: failing });
    assert.strictEqual(r.code, 1);
    assert.ok(!r.stderr.includes(HOOK_LINE), r.stderr);
  });

  await test("a passing run held open by a leaked handle exits 0 after the grace period and names the handle", async () => {
    const r = await runNewman({ collectionFile: passing, preloads: [leakTimer] });
    assert.strictEqual(r.code, 0);
    assert.ok(r.ms >= GRACE_MS, `exited after ${r.ms}ms, before the ${GRACE_MS}ms grace`);
    assert.match(r.stderr, /\[newman-exit-when-done\] .*held open by: .*Timeout.*exiting with 0/);
  });

  await test("a failing run held open by a leaked handle still exits 1", async () => {
    const r = await runNewman({ collectionFile: failing, preloads: [leakTimer] });
    assert.strictEqual(r.code, 1);
    assert.match(r.stderr, /exiting with 1/);
  });

  await test("a passing run held open by an unfinished write exits 1, never 0", async () => {
    const r = await runNewman({ collectionFile: passing, preloads: [pendingWrite] });
    assert.strictEqual(r.code, 1);
    assert.match(r.stderr, /held open by: .*WriteWrap.*exiting with 1 \(file or write I\/O still pending\)/);
  });

  await test("a report export that fails makes the run exit 1", async () => {
    const unwritable = path.join(passing, "not-a-dir", "report.json"); // parent is a file
    const r = await runNewman({ collectionFile: passing, extraArgs: ["-r", "cli,json", "--reporter-json-export", unwritable] });
    assert.strictEqual(r.code, 1, r.stderr);
  });

  await test("a run that never completes is never turned into an exit, let alone a success", async () => {
    const r = await runNewman({ collectionFile: neverCompletes, killAfterMs: GRACE_MS * 3 });
    assert.strictEqual(r.signal, "SIGKILL", `run ended by itself with code ${r.code}`);
    assert.ok(!r.stderr.includes(HOOK_LINE), r.stderr);
  });
} finally {
  server.closeAllConnections();
  server.close();
  sink.close();
  fs.rmSync(dir, { recursive: true, force: true });
}

console.log(`\n${passed} passed`);
