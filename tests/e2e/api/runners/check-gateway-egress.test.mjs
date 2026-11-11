// Tests for the harness egress preflight (check-gateway-egress.mjs, TXE-7083): a fixture run
// outside scripts/test-offline.sh is refused, and a gateway is judged guarded only when the
// egress probe comes back with the dial guard's refusal.
//
// Run directly: `node check-gateway-egress.test.mjs`.
import assert from "node:assert";
import { spawn, spawnSync } from "node:child_process";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { classifyProbe, isFixtureRun, MARKER } from "./check-gateway-egress.mjs";

const script = join(dirname(fileURLToPath(import.meta.url)), "check-gateway-egress.mjs");
const fixtureEnv = JSON.stringify({ values: [{ key: "providerErrorFixture", value: "1", enabled: true }] });

test("a fixture run is one whose environment enables providerErrorFixture", () => {
  assert.strictEqual(isFixtureRun(fixtureEnv), true);
  assert.strictEqual(isFixtureRun(JSON.stringify({ values: [{ key: "providerErrorFixture", value: "1", enabled: false }] })), false);
  assert.strictEqual(isFixtureRun(JSON.stringify({ values: [{ key: "baseUrl", value: "x" }] })), false);
  assert.strictEqual(isFixtureRun("not json"), false);
  assert.strictEqual(isFixtureRun(""), false);
});

test("only the egress_probe provider's guard refusal counts as a guarded gateway", () => {
  const servedBy = (provider) => `"extra_fields":{"routing_info":{"provider":"${provider}"}}`;
  const refused = `{"error":{"error":"dial tcp 203.0.113.10:443: ${MARKER} 203.0.113.10:443 refused"},${servedBy("egress_probe")}}`;
  assert.strictEqual(classifyProbe(502, refused).verdict, "guarded");
  assert.strictEqual(classifyProbe(502, `{"error":{"error":"dial tcp 203.0.113.10:443: i/o timeout"},${servedBy("egress_probe")}}`).verdict, "unguarded");
  assert.strictEqual(classifyProbe(200, `{"choices":[],${servedBy("egress_probe")}}`).verdict, "unguarded");
  // A gateway without the provider routes the model to another one (a real fixture gateway did).
  assert.strictEqual(classifyProbe(200, `{"choices":[],${servedBy("anthropic")}}`).verdict, "unverified");
  assert.strictEqual(classifyProbe(400, '{"error":{"message":"unknown provider"}}').verdict, "unverified");
  // The marker quoted by something else is not the guard.
  assert.strictEqual(classifyProbe(200, `{"echo":"${MARKER}",${servedBy("anthropic")}}`).verdict, "unverified");
  const auth = classifyProbe(401, '{"error":{"message":"unauthorized"}}');
  assert.strictEqual(auth.verdict, "unverified");
  assert.match(auth.detail, /BIFROST_E2E_AUTH_HEADER/);
});

function run(env, envFile) {
  const dir = mkdtempSync(join(tmpdir(), "egress-preflight-"));
  const args = [script, "http://127.0.0.1:1"];
  if (envFile !== undefined) {
    writeFileSync(join(dir, "env.json"), envFile);
    args.push(join(dir, "env.json"));
  }
  const childEnv = { ...process.env, ...env };
  for (const [k, v] of Object.entries(env)) if (v === undefined) delete childEnv[k];
  return spawnSync(process.execPath, args, { env: childEnv, encoding: "utf8" });
}

test("a fixture run outside scripts/test-offline.sh is refused before any request", () => {
  const result = run({ BIFROST_TEST_OFFLINE: undefined }, fixtureEnv);
  assert.strictEqual(result.status, 1, result.stderr);
  assert.match(result.stderr, /outside scripts\/test-offline\.sh/);
});

test("a live run (no fixture, not offline) is not checked", () => {
  const result = run({ BIFROST_TEST_OFFLINE: undefined }, JSON.stringify({ values: [] }));
  assert.strictEqual(result.status, 0, result.stderr);
});

test("an offline run against a gateway that does not answer is refused as unguarded", () => {
  const result = run({ BIFROST_TEST_OFFLINE: "1" }, fixtureEnv);
  assert.strictEqual(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stderr, /is not guarded/);
});

test("a gateway that asks for authentication, or has no egress_probe provider, fails the run", async () => {
  const { createServer } = await import("node:http");
  for (const [status, body] of [[401, '{"error":{"message":"unauthorized"}}'], [200, '{"choices":[],"extra_fields":{"routing_info":{"provider":"anthropic"}}}']]) {
    const server = createServer((req, res) => {
      res.writeHead(status, { "Content-Type": "application/json" });
      res.end(body);
    });
    await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
    const dir = mkdtempSync(join(tmpdir(), "egress-preflight-"));
    writeFileSync(join(dir, "env.json"), fixtureEnv);
    const result = await new Promise((resolve) => {
      const child = spawn(process.execPath, [script, `http://127.0.0.1:${server.address().port}`, join(dir, "env.json")], {
        env: { ...process.env, BIFROST_TEST_OFFLINE: "1" },
      });
      let stderr = "";
      child.stderr.on("data", (c) => (stderr += c));
      child.on("close", (code) => resolve({ code, stderr }));
    });
    server.close();
    assert.strictEqual(result.code, 1, `HTTP ${status}: ${result.stderr}`);
    assert.match(result.stderr, /could not be shown to refuse egress/);
  }
});

test("the configured auth header reaches the gateway", async () => {
  const { createServer } = await import("node:http");
  let seen;
  const server = createServer((req, res) => {
    seen = req.headers.authorization;
    res.writeHead(502, { "Content-Type": "application/json" });
    res.end(`{"error":{"error":"${MARKER} 203.0.113.10:443 refused"},"extra_fields":{"routing_info":{"provider":"egress_probe"}}}`);
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const dir = mkdtempSync(join(tmpdir(), "egress-preflight-"));
  writeFileSync(join(dir, "env.json"), fixtureEnv);
  const code = await new Promise((resolve) => {
    spawn(process.execPath, [script, `http://127.0.0.1:${server.address().port}`, join(dir, "env.json")], {
      env: { ...process.env, BIFROST_TEST_OFFLINE: "1", BIFROST_E2E_AUTH_HEADER: "Bearer probe-token" },
      stdio: "ignore",
    }).on("close", resolve);
  });
  server.close();
  assert.strictEqual(code, 0);
  assert.strictEqual(seen, "Bearer probe-token");
});
