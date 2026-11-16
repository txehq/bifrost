// The base_url support table (core/providers/base_url_support.json, TXE-7083): it lists every
// registered provider, agrees with the code, and no fixture config redirects a provider that
// ignores base_url through base_url. Runs in the offline API suites.
//
// Run directly: `node base-url-support.test.mjs`.
import assert from "node:assert";
import { readdirSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import {
  configProblems,
  fixtureConfigFiles,
  providerPackage,
  registeredProviders,
  relativeTo,
  tableProblems,
} from "./base-url-support.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const root = resolve(here, "../../../../..");
const table = JSON.parse(readFileSync(join(root, "core/providers/base_url_support.json"), "utf8"));
const registered = registeredProviders(readFileSync(join(root, "core/schemas/bifrost.go"), "utf8"));

function readsBaseURL(provider) {
  const dir = join(root, "core/providers", providerPackage(provider));
  let files;
  try {
    files = readdirSync(dir).filter((f) => f.endsWith(".go") && !f.endsWith("_test.go"));
  } catch {
    return undefined; // no package of that name: nothing to compare
  }
  return files.some((f) => readFileSync(join(dir, f), "utf8").includes("BaseURL"));
}

test("every registered provider is in the base_url table, and the table agrees with the code", () => {
  assert.ok(registered.includes("bedrock") && registered.includes("openai"), `parsed: ${registered.join(", ")}`);
  assert.deepStrictEqual(tableProblems(table, registered, readsBaseURL), []);
});

test("every provider package exists, so the code check reads real code", () => {
  const missing = registered.filter((p) => readsBaseURL(p) === undefined);
  assert.deepStrictEqual(missing, [], "providers with no core/providers package; teach providerPackage the mapping");
});

test("Bedrock is listed as ignoring base_url, with the fixture route", () => {
  assert.strictEqual(table.providers.bedrock.base_url, "ignores");
  assert.match(table.providers.bedrock.fixture, /endpoints\.runtime/);
});

test("the checks catch a missing provider, a false honours claim and a stale entry", () => {
  const fake = { providers: { openai: { base_url: "honours" }, retired: { base_url: "honours" }, bedrock: { base_url: "honours" } } };
  const problems = tableProblems(fake, ["openai", "bedrock", "newvendor"], (p) => p !== "bedrock");
  assert.ok(problems.some((p) => p.startsWith("newvendor: registered but missing")), problems.join("\n"));
  assert.ok(problems.some((p) => p.startsWith("retired: in the table but not registered")), problems.join("\n"));
  assert.ok(problems.some((p) => p.startsWith("bedrock: listed as honours, but core/providers/bedrock never reads BaseURL")), problems.join("\n"));
});

test("a fixture config that points Bedrock at a fixture through base_url is refused, with the fix", () => {
  const config = {
    providers: {
      bedrock: { network_config: { base_url: "http://127.0.0.1:8791" } },
      my_bedrock: { custom_provider_config: { base_provider_type: "bedrock" }, network_config: { base_url: "http://127.0.0.1:8791" } },
      openai: { network_config: { base_url: "http://127.0.0.1:8791" } },
    },
  };
  const problems = configProblems(config, table, "probe.json");
  assert.strictEqual(problems.length, 2, problems.join("\n"));
  assert.match(problems[0], /providers\.bedrock \(bedrock\) sets network_config\.base_url, which bedrock ignores/);
  assert.match(problems[1], /providers\.my_bedrock \(bedrock\)/);
  assert.match(problems[0], /endpoints\.runtime/);
});

test("no fixture config points a provider that ignores base_url at a fixture through base_url", () => {
  const dir = join(root, "tests/e2e/api/provider_config");
  const files = fixtureConfigFiles(dir);
  assert.ok(files.some((f) => f.endsWith("provider-error-fixture.config.json")), `scanned: ${files.join(", ")}`);
  const problems = files.flatMap((file) =>
    configProblems(JSON.parse(readFileSync(file, "utf8")), table, relativeTo(root, file)),
  );
  assert.deepStrictEqual(problems, []);
});

// make run-provider-harness-test refuses a fixture run unless the gateway's egress_probe provider
// comes back with the dial guard's refusal (runners/check-gateway-egress.mjs), so every fixture
// config that defines providers carries one, aimed at TEST-NET-3 (never routed).
test("every fixture config with providers carries the egress_probe provider on TEST-NET-3", () => {
  const dir = join(root, "tests/e2e/api/provider_config");
  const missing = [];
  for (const file of fixtureConfigFiles(dir)) {
    const config = JSON.parse(readFileSync(file, "utf8"));
    if (Object.keys(config.providers).length === 0) continue;
    const probe = config.providers.egress_probe;
    if (!probe || probe.network_config?.base_url !== "https://203.0.113.10" || probe.custom_provider_config?.base_provider_type !== "openai") {
      missing.push(relativeTo(root, file));
    }
  }
  assert.deepStrictEqual(missing, [], "add the egress_probe provider from provider-error-fixture.config.json");
});

// JSON.parse keeps the last of two equal keys and says nothing, so a provider defined twice in a
// fixture config runs on whichever entry comes last: TXE-7117's huggingface entry on /pp/huggingface
// was shadowed by a later one on /pp/hf, and its rows got 404s with no hint why. No object in a
// fixture config may name a key twice.
export function duplicateKeys(text) {
  const found = [];
  let i = 0;
  const ws = () => { while (/\s/.test(text[i])) i++; };
  const str = () => { const start = i++; while (text[i] !== '"') i += text[i] === "\\" ? 2 : 1; i++; return JSON.parse(text.slice(start, i)); };
  const value = (path) => {
    ws();
    if (text[i] === "{") {
      i++;
      const seen = new Set();
      for (ws(); text[i] !== "}"; ws()) {
        if (text[i] === ",") { i++; continue; }
        const key = str();
        if (seen.has(key)) found.push(`${path}.${key}`);
        seen.add(key);
        ws(); i++; // ':'
        value(`${path}.${key}`);
      }
      i++;
    } else if (text[i] === "[") {
      i++;
      for (let n = 0; (ws(), text[i] !== "]"); ) {
        if (text[i] === ",") { i++; continue; }
        value(`${path}[${n++}]`);
      }
      i++;
    } else if (text[i] === '"') {
      str();
    } else {
      while (i < text.length && !/[\s,}\]]/.test(text[i])) i++;
    }
  };
  value("$");
  return found;
}

test("duplicateKeys finds a key named twice in one object, at any depth", () => {
  assert.deepStrictEqual(duplicateKeys('{"a":1,"b":{"c":[1,{"d":"x\\"y"}],"c":2},"a":[]}'), ["$.b.c", "$.a"]);
  assert.deepStrictEqual(duplicateKeys('{"a":{"x":1},"b":{"x":1}}'), []);
});

test("no fixture config names a key twice in one object", () => {
  const dir = join(root, "tests/e2e/api/provider_config");
  const problems = [];
  for (const file of fixtureConfigFiles(dir)) {
    for (const key of duplicateKeys(readFileSync(file, "utf8"))) problems.push(`${relativeTo(root, file)}: ${key}`);
  }
  assert.deepStrictEqual(problems, [], "JSON.parse keeps only the last of two equal keys; merge or rename the entries");
});
