// Unit tests for the keyless-provider rule (lib/keyless-providers.mjs): a provider in the harness
// profile whose key env vars are all unset is skipped with a visible "skipped: no key" assertion
// instead of failing the live run on credentials.
//
// Run directly: `node keyless-providers.test.mjs`.
import assert from "node:assert";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import {
  bodyModelProvider,
  describeKeylessSkips,
  findKeylessProviders,
  injectKeylessProviderSkips,
  keyEnvRefs,
  skipLabel,
} from "./keyless-providers.mjs";

let passed = 0;
function test(name, fn) {
  fn();
  passed++;
  console.log(`  ok - ${name}`);
}

const profile = {
  providers: {
    openai: { keys: [{ value: "env.OPENAI_API_KEY" }] },
    deepinfra: { keys: [{ value: "env.DEEPINFRA_API_KEY" }], custom_provider_config: { base_provider_type: "openai" } },
    bedrock: { keys: [{ value: "", bedrock_key_config: { access_key: "env.AWS_ACCESS_KEY_ID", secret_key: "env.AWS_SECRET_ACCESS_KEY" } }] },
    literal: { keys: [{ value: "sk-literal" }] },
    keyless_by_design: { keys: [{ value: "env.NEVER_SET" }], custom_provider_config: { is_key_less: true } },
    nokeys: {},
  },
};

const request = (name, model, extra = {}) => ({
  name,
  request: { method: "POST", body: { mode: "raw", raw: JSON.stringify({ model, input: "hi" }) }, url: "{{baseUrl}}/v1/embeddings" },
  ...extra,
});

test("a provider with every key env var unset is keyless, naming the variables", () => {
  const keyless = findKeylessProviders(profile, { OPENAI_API_KEY: "sk-1", AWS_ACCESS_KEY_ID: "a" });
  assert.deepEqual([...keyless.entries()], [["deepinfra", ["DEEPINFRA_API_KEY"]]]);
});

test("an empty or whitespace value counts as unset", () => {
  const keyless = findKeylessProviders(profile, { OPENAI_API_KEY: "  ", DEEPINFRA_API_KEY: "", AWS_ACCESS_KEY_ID: "a" });
  assert.deepEqual([...keyless.keys()].sort(), ["deepinfra", "openai"]);
});

test("a partly configured provider is not keyless, so it still fails loudly", () => {
  const keyless = findKeylessProviders(profile, { OPENAI_API_KEY: "sk", DEEPINFRA_API_KEY: "d", AWS_SECRET_ACCESS_KEY: "s" });
  assert.equal(keyless.has("bedrock"), false);
  assert.deepEqual(keyEnvRefs(profile.providers.bedrock), ["AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"]);
});

test("literal keys, is_key_less providers and providers without keys are never keyless", () => {
  const keyless = findKeylessProviders(profile, {});
  for (const name of ["literal", "keyless_by_design", "nokeys"]) assert.equal(keyless.has(name), false, name);
});

test("a provider with a literal key alongside an unset env key keeps running", () => {
  const mixed = { providers: { openai: { keys: [{ value: "env.OPENAI_API_KEY" }, { value: "sk-valid-literal-key" }] } } };
  assert.equal(findKeylessProviders(mixed, {}).has("openai"), false);
  const vaultOnly = { providers: { openai: { keys: [{ value: "env.OPENAI_API_KEY" }, { value: "vault.secret/openai" }] } } };
  assert.equal(findKeylessProviders(vaultOnly, {}).has("openai"), true, "a vault reference is not a literal credential");
});

test("the request's provider comes from the body's provider/model, or is empty", () => {
  assert.equal(bodyModelProvider(request("a", "deepinfra/BAAI/bge-m3")), "deepinfra");
  assert.equal(bodyModelProvider(request("b", "text-embedding-3-small")), "");
  assert.equal(bodyModelProvider({ request: { body: { raw: '{"model": "deepinfra/x", "n": {{count}}}' } } }), "deepinfra");
  assert.equal(bodyModelProvider({ request: { method: "GET" } }), "");
  // A multipart request (transcription, image edit) names its model in a form field.
  const form = (fields) => ({ request: { method: "POST", body: { mode: "formdata", formdata: fields } } });
  assert.equal(bodyModelProvider(form([{ key: "model", value: "elevenlabs/scribe_v2", type: "text" }, { key: "file", src: "a.mp3", type: "file" }])), "elevenlabs");
  assert.equal(bodyModelProvider(form([{ key: "model", value: "whisper-1", type: "text" }])), "");
  assert.equal(bodyModelProvider(form([{ key: "model", value: "elevenlabs/scribe_v2", type: "text", disabled: true }])), "");
  assert.equal(bodyModelProvider(form([{ key: "file", src: "a.mp3", type: "file" }])), "");
});

test("requests aimed at a keyless provider get a passing skip assertion and skipRequest; others are untouched", () => {
  const collection = {
    item: [
      { name: "folder", item: [request("di case", "deepinfra/BAAI/bge-m3"), request("oa case", "openai/text-embedding-3-small")] },
      request("di case 2", "deepinfra/Qwen/Qwen3-Embedding-8B", { event: [{ listen: "prerequest", script: { exec: ["setup();"] } }] }),
    ],
  };
  const keyless = new Map([["deepinfra", ["DEEPINFRA_API_KEY"]]]);
  const counts = injectKeylessProviderSkips(collection, keyless);
  assert.deepEqual([...counts.entries()], [["deepinfra", 2]]);

  const guarded = collection.item[0].item[0].event[0].script.exec.join("\n");
  assert.ok(guarded.includes(JSON.stringify(skipLabel("di case", ["DEEPINFRA_API_KEY"]))));
  assert.ok(guarded.includes("skipped: no key (DEEPINFRA_API_KEY unset)"));
  assert.ok(guarded.includes("pm.execution.skipRequest()"));
  assert.equal(collection.item[0].item[1].event, undefined, "a provider with a key must not be guarded");

  const merged = collection.item[1].event.filter((e) => e.listen === "prerequest");
  assert.equal(merged.length, 1, "the guard joins the existing prerequest instead of adding a second");
  assert.ok(merged[0].script.exec.indexOf("setup();") > 0, "the guard runs before the item's own setup");

  assert.deepEqual(describeKeylessSkips(keyless, counts), ["skipped: no key - deepinfra (DEEPINFRA_API_KEY unset): 2 requests"]);
});

test("re-running the injection replaces the guard instead of stacking it", () => {
  const collection = { item: [request("di", "deepinfra/x")] };
  const keyless = new Map([["deepinfra", ["DEEPINFRA_API_KEY"]]]);
  injectKeylessProviderSkips(collection, keyless);
  injectKeylessProviderSkips(collection, keyless);
  const exec = collection.item[0].event[0].script.exec;
  assert.equal(exec.filter((l) => l.includes("[keyless-provider-skip]")).length, 1);
});

test("no keyless providers means no change", () => {
  const collection = { item: [request("di", "deepinfra/x")] };
  assert.equal(injectKeylessProviderSkips(collection, new Map()).size, 0);
  assert.equal(collection.item[0].event, undefined);
});

// The shared profile itself: the custom providers this rule exists for are in it, and with their
// secrets absent they are keyless while the providers CI does have keys for are not.
test("the shared harness profile's DeepInfra and Vercel AI Gateway providers are keyless without their secrets", () => {
  const HERE = dirname(fileURLToPath(import.meta.url));
  const shared = JSON.parse(readFileSync(resolve(HERE, "../../../../integrations/python/config.json"), "utf8"));
  const keyless = findKeylessProviders(shared, { OPENAI_API_KEY: "sk" });
  assert.deepEqual(keyless.get("deepinfra"), ["DEEPINFRA_API_KEY"]);
  assert.deepEqual(keyless.get("vercel_ai_gateway"), ["VERCEL_AI_GATEWAY_API_KEY"]);
  assert.equal(keyless.has("openai"), false);
  const all = findKeylessProviders(shared, { OPENAI_API_KEY: "sk", DEEPINFRA_API_KEY: "d", VERCEL_AI_GATEWAY_API_KEY: "v" });
  assert.equal(all.has("deepinfra") || all.has("vercel_ai_gateway"), false);
});

console.log(`\n${passed} passed`);
