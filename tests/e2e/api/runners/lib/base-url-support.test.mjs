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
