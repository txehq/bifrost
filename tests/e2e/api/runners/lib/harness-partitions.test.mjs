// Tests the harness sweep's partitions (fork, TXE-7308). Run directly: `node harness-partitions.test.mjs`.
//
// Before TXE-7308 the Makefile kept HARNESS_PROVIDERS by hand. A row no listed provider claimed
// (every bfl, replicate, runware, voyage and typesafe row, 428 rows in all) matched no fork, and a
// parallel sweep ran it nowhere, silently. These tests pin the derived roster, the serial pass
// that runs a folder holding a row no partition claims, and the Makefile's use of both.
import assert from "node:assert";
import { execFileSync } from "node:child_process";
import { mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { walkRequests } from "./chained-vars.mjs";
import { registeredProviders } from "./base-url-support.mjs";
import {
  Partitioner,
  RESELLER_PROVIDERS,
  ROUTE_PARTITIONS,
  harnessPartitions,
  isUnclaimed,
  itemMatchesProvider,
} from "./harness-partitions.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, "..", "..", "..", "..", "..");
const harnessDir = join(repoRoot, "tests", "e2e", "api", "collections", "provider-harness");

let passed = 0;
function test(name, fn) {
  fn();
  passed++;
  console.log(`  ok - ${name}`);
}

const chat = (name, model) => ({
  name,
  request: { method: "POST", url: { raw: "{{baseUrl}}/v1/chat/completions" }, body: { mode: "raw", raw: JSON.stringify({ model, messages: [] }) } },
});
const get = (name, url) => ({ name, request: { method: "GET", url: { raw: url } } });
const folder = (name, item) => ({ name, item });

const harnessFolders = () =>
  readdirSync(harnessDir)
    .filter((f) => /^\d+-.*\.json$/.test(f))
    .map((f) => JSON.parse(readFileSync(join(harnessDir, f), "utf8")));

test("the roster is every registered provider, the resellers and the route families, once each", () => {
  const roster = harnessPartitions(repoRoot);
  const registry = registeredProviders(readFileSync(join(repoRoot, "core", "schemas", "bifrost.go"), "utf8"));
  for (const p of [...registry, ...RESELLER_PROVIDERS, ...ROUTE_PARTITIONS]) assert.ok(roster.includes(p), `${p} missing from the roster`);
  assert.strictEqual(new Set(roster).size, roster.length, "a partition is listed twice");
  // The providers the hand-kept list left out.
  for (const p of ["bfl", "replicate", "runware", "voyage", "typesafe"]) assert.ok(roster.includes(p), `${p} missing`);
});

test("a row whose model names the provider is that provider's, even when the haystack lost the name", () => {
  const row = chat("long model path", "fireworks/accounts/fireworks/models/deepseek-v3-0324-long-name");
  assert.ok(itemMatchesProvider(row, ["Some folder"], "fireworks"));
  assert.ok(!itemMatchesProvider(row, ["Some folder"], "openai"));
});

test("a row whose model names a provider runs in that provider's fork only, never once per name in it", () => {
  const roster = harnessPartitions(repoRoot);
  const claimants = (row, ancestors) => roster.filter((p) => itemMatchesProvider(row, ancestors, p));
  // Folder 108's shape: before, qwen claimed the Groq, Cerebras and Parasail rows too, and the
  // sweep sent each paid request twice.
  assert.deepStrictEqual(claimants(chat("groq qwen", "groq/qwen-2.5-32b"), ["108. Open-weight chat"]), ["groq"]);
  assert.deepStrictEqual(claimants(chat("cerebras qwen", "cerebras/qwen-3-32b"), ["108. Open-weight chat"]), ["cerebras"]);
  // A folder name no longer routes a row the model names elsewhere.
  assert.deepStrictEqual(claimants(chat("gemini usage", "gemini/gemini-2.5-flash"), ["337. Gemini and Vertex chat usage"]), ["gemini"]);
  // A route family still claims by route, beside the provider.
  const passthrough = { name: "pt", request: { method: "POST", url: { raw: "{{baseUrl}}/openai_passthrough/v1/chat/completions" }, body: { mode: "raw", raw: JSON.stringify({ model: "openai/gpt-4o-mini" }) } } };
  assert.deepStrictEqual(claimants(passthrough, ["Passthrough"]), ["openai", "passthrough"]);
});

test("the serial pass takes a folder with a row no partition claims, or one that touches fixture state", () => {
  const coll = [
    folder("154. Provider 5xx failover", [get("counters reset", "{{fixtureUrl}}/__reset"), chat("503", "openai/upstream-503"), get("hits", "{{fixtureUrl}}/__hits")]),
    // Every row claimed by bfl, but the read-back reads what the fixture received: a class shard
    // would run it beside, not after, the create (image-gen vs other).
    folder("472. BFL webhook secret", [chat("create", "bfl/flux-2"), get("bfl received the secret", "{{providerErrorFixtureUrl}}/__sent?model=bfl-flux")]),
    folder("139. MCP OAuth2 server guards", [get("metadata", "{{baseUrl}}/.well-known/oauth-authorization-server")]),
    folder("12. Chat", [chat("plain", "openai/gpt-4o-mini"), chat("claude", "anthropic/claude-sonnet-4-5")]),
  ];
  const entries = walkRequests(coll);
  const P = new Partitioner(entries, ["openai", "anthropic", "bfl"]);
  assert.deepStrictEqual([...P.serialPassFolders()], ["154. Provider 5xx failover", "472. BFL webhook secret", "139. MCP OAuth2 server guards"]);
  assert.deepStrictEqual(P.roster(entries), ["openai", "anthropic", "bfl"], "the roster names the partitions that claim a row");
});

// runFilter runs filter-collection.mjs over source and returns the names of the rows it selects.
const runFilter = (source, out, ...args) => {
  execFileSync("node", [join(here, "..", "filter-collection.mjs"), "--source", source, "--out", out, ...args], { stdio: ["ignore", "ignore", "ignore"] });
  return walkRequests(JSON.parse(readFileSync(out, "utf8")).item).map(({ item, ancestors }) => `${ancestors[0]} > ${item.name}`);
};

test("the serial pass runs such a folder whole and in order; no provider fork runs any of it", () => {
  const dir = mkdtempSync(join(tmpdir(), "harness-partitions-"));
  try {
    const source = join(dir, "c.json");
    writeFileSync(source, JSON.stringify({ info: { name: "t" }, item: [
      folder("154. Provider 5xx failover", [get("counters reset", "{{fixtureUrl}}/__reset"), chat("503", "openai/upstream-503"), chat("429", "openai/upstream-429"), get("hits", "{{fixtureUrl}}/__hits")]),
      folder("12. Chat", [chat("plain", "openai/gpt-4o-mini")]),
    ] }));
    const out = join(dir, "out.json");
    assert.deepStrictEqual(runFilter(source, out, "--only-serial"), [
      "154. Provider 5xx failover > counters reset",
      "154. Provider 5xx failover > 503",
      "154. Provider 5xx failover > 429",
      "154. Provider 5xx failover > hits",
    ]);
    // The class axis cannot split the folder either: a provider fork and its chat class shard
    // see none of it (before, the reset and the hit check landed in "other", the calls in "chat").
    assert.deepStrictEqual(runFilter(source, out, "--exclude-serial", "--provider", "openai"), ["12. Chat > plain"]);
    assert.deepStrictEqual(runFilter(source, out, "--provider", "openai", "--class", "other"), [
      "154. Provider 5xx failover > counters reset",
      "154. Provider 5xx failover > 503",
      "154. Provider 5xx failover > 429",
      "154. Provider 5xx failover > hits",
    ]);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("every harness row runs: in a provider fork, or with its folder in the serial pass", () => {
  const entries = walkRequests(harnessFolders());
  assert.ok(entries.length > 1000, `only ${entries.length} harness rows read`);
  const roster = harnessPartitions(repoRoot);
  const P = new Partitioner(entries, roster);
  const serialFolders = P.serialPassFolders();
  const nowhere = entries.filter(({ item, ancestors }) =>
    !ancestors.some((a) => /\[serial\]/i.test(a)) && !serialFolders.has(ancestors[0]) && !roster.some((p) => P.runs(item, ancestors, p)));
  assert.deepStrictEqual(nowhere.map(({ item, ancestors }) => `${ancestors[0]} > ${item.name}`), []);
});

// The custom providers the harness profiles declare: the fixture configs and the live profile.
const declaredProviders = () => {
  const files = [
    ...readdirSync(join(repoRoot, "tests", "e2e", "api", "provider_config")).filter((f) => f.endsWith(".json")).map((f) => join(repoRoot, "tests", "e2e", "api", "provider_config", f)),
    join(repoRoot, "tests", "integrations", "python", "config.json"),
  ];
  const names = new Set();
  for (const file of files) {
    let config;
    try {
      config = JSON.parse(readFileSync(file, "utf8"));
    } catch {
      continue;
    }
    for (const name of Object.keys(config.providers || {})) names.add(name);
  }
  return names;
};

// Google resource names ("models/gemini-2.5-pro", "projects/p/locations/l/...") are model ids,
// not provider prefixes.
const RESOURCE_NAME_PREFIXES = new Set(["models", "projects"]);

test("every provider a harness row calls is in the roster, or is a custom provider a harness profile declares", () => {
  const roster = new Set(harnessPartitions(repoRoot));
  const declared = declaredProviders();
  const unknown = [];
  const unclaimed = [];
  for (const { item, ancestors } of walkRequests(harnessFolders())) {
    const prefix = /"model"\s*:\s*"([^"/]+)\//.exec(item.request?.body?.raw || "")?.[1];
    if (!prefix || prefix.startsWith("{{") || RESOURCE_NAME_PREFIXES.has(prefix)) continue;
    if (roster.has(prefix)) {
      // A registered provider's row is claimed by a named partition: its own, or the one a
      // collision rule routes it to (a gemini model in a Vertex folder runs in vertex). It never
      // makes its folder serial by being claimed by none.
      if (isUnclaimed(item, ancestors, [...roster])) unclaimed.push(`${ancestors[0]} > ${item.name} (${prefix})`);
    } else if (!declared.has(prefix)) {
      unknown.push(`${ancestors[0]} > ${item.name} calls "${prefix}", which is neither registered (schemas.StandardProviders) nor declared by a harness profile`);
    }
  }
  assert.deepStrictEqual(unknown, []);
  assert.deepStrictEqual(unclaimed, []);
});

test("filter-collection --roster and --provider fork the partitions a selection reaches", () => {
  const dir = mkdtempSync(join(tmpdir(), "harness-partitions-"));
  try {
    const source = join(dir, "c.json");
    writeFileSync(source, JSON.stringify({ info: { name: "t" }, item: [
      folder("472. BFL webhook secret", [chat("create", "bfl/flux-2"), get("read key", "{{baseUrl}}/api/providers/bfl")]),
      folder("139. MCP OAuth2 server guards", [get("metadata", "{{baseUrl}}/.well-known/oauth-authorization-server")]),
    ] }));
    const run = (...args) => execFileSync("node", [join(here, "..", "filter-collection.mjs"), "--source", source, ...args], { encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] });
    // The OAuth folder's row is claimed by none: it runs in the serial pass, not a fork.
    assert.strictEqual(run("--roster").trim(), "bfl");
    const out = join(dir, "bfl.json");
    run("--provider", "bfl", "--out", out);
    const names = walkRequests(JSON.parse(readFileSync(out, "utf8")).item).map(({ item }) => item.name);
    assert.deepStrictEqual(names, ["create", "read key"]);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("the Makefile forks the derived roster, never a hand-kept list", () => {
  const makefile = readFileSync(join(repoRoot, "Makefile"), "utf8");
  const definition = makefile.match(/^HARNESS_PROVIDERS\s*:?=\s*(.*)$/m);
  assert.ok(definition, "HARNESS_PROVIDERS is not defined in the Makefile");
  assert.strictEqual(definition[1].trim(), "$(shell node tests/e2e/api/runners/harness-partitions.mjs)", "HARNESS_PROVIDERS must be derived");
  assert.match(makefile, /filter-collection\.mjs --source "\$\$COLLECTION_FILE" --roster/, "the parallel sweep must fork the selection's roster");
});

console.log(`${passed} passed`);
