// Providers in the harness profile whose credentials are not in this run's environment.
//
// The profile (tests/integrations/python/config.json) reads every key from an env var
// (`"value": "env.DEEPINFRA_API_KEY"`). An unset variable does not stop Bifrost from starting:
// the key resolves to an empty string, and every case aimed at that provider then fails on
// credentials rather than on the behaviour it pins. That turns a missing secret into a red live
// run (the release pipeline's test-api-integrations job gates on it), so a provider added to the
// profile before its secret exists would break the release.
//
// The rule, applied once here so every later provider is covered without a per-provider step:
// a provider whose key env vars are ALL unset or empty is "keyless", and augment-provider-harness
// gives each request aimed at it a pre-request guard that records a passing
// "skipped: no key (<VAR> unset)" assertion and skips the request. A provider with at least one
// of its variables set runs as before, so a partly configured provider still fails loudly.
// Providers marked is_key_less, and providers whose keys carry literal values, are never keyless.
//
// A request is aimed at a provider when its raw JSON body's "model" is "<provider>/<model>",
// the notation the native /v1 routes take. Drop-in routes that carry the provider elsewhere
// (in the URL, or a bare model name) are not matched and run as before.

const ENV_REF_RE = /^env\.([A-Za-z0-9_]+)$/;

// Every env var a provider's keys reference, in first-seen order. Walks the whole key object, so
// credentials nested in a provider-specific config (bedrock_key_config, vertex_key_config, ...)
// count the same as a top-level value.
export function keyEnvRefs(provider) {
  const refs = [];
  const visit = (node) => {
    if (typeof node === "string") {
      const m = ENV_REF_RE.exec(node.trim());
      if (m && !refs.includes(m[1])) refs.push(m[1]);
    } else if (Array.isArray(node)) {
      node.forEach(visit);
    } else if (node && typeof node === "object") {
      Object.values(node).forEach(visit);
    }
  };
  visit(provider?.keys || []);
  return refs;
}

// A key is usable when its value is a literal credential, or when any env var it references is set.
// Only `value` is checked for a literal: provider-specific configs also carry literal non-secret
// fields (a region, a deployment name) that are not credentials on their own.
function keyIsUsable(key, env) {
  const value = typeof key?.value === "string" ? key.value.trim() : "";
  if (value !== "" && !value.startsWith("env.") && !value.startsWith("vault.")) return true;
  return keyEnvRefs({ keys: [key] }).some((ref) => typeof env[ref] === "string" && env[ref].trim() !== "");
}

// Map of provider name -> the env var names it needs, for every keyless provider in the profile:
// one that references at least one env var and has no usable key.
export function findKeylessProviders(profile, env = process.env) {
  const keyless = new Map();
  for (const [name, provider] of Object.entries(profile?.providers || {})) {
    if (provider?.custom_provider_config?.is_key_less) continue;
    const refs = keyEnvRefs(provider);
    if (refs.length === 0) continue;
    if (!(provider.keys || []).some((key) => keyIsUsable(key, env))) keyless.set(name, refs);
  }
  return keyless;
}

// The provider half of a request body's "model", or "" when the body has none.
export function bodyModelProvider(item) {
  const raw = item?.request?.body?.raw;
  if (typeof raw !== "string" || raw === "") return "";
  let model;
  try {
    model = JSON.parse(raw)?.model;
  } catch {
    // Bodies with Postman templates are not JSON until newman substitutes them.
    model = /"model"\s*:\s*"([^"]+)"/.exec(raw)?.[1];
  }
  if (typeof model !== "string") return "";
  const slash = model.indexOf("/");
  return slash > 0 ? model.slice(0, slash) : "";
}

// Marker on the injected pre-request script, so re-running augment replaces it.
const GUARD_MARKER = "// [keyless-provider-skip]";

export function skipLabel(itemName, refs) {
  return `${itemName}: skipped: no key (${refs.join(", ")} unset)`;
}

function guardScript(itemName, refs) {
  return [
    GUARD_MARKER,
    `pm.test(${JSON.stringify(skipLabel(itemName, refs))}, function () {});`,
    'if (pm.execution && typeof pm.execution.skipRequest === "function") pm.execution.skipRequest();',
  ];
}

function walk(items, out = []) {
  for (const item of items || []) {
    if (Array.isArray(item.item)) walk(item.item, out);
    else if (item.request) out.push(item);
  }
  return out;
}

// Guards every request aimed at a keyless provider. Returns provider name -> requests guarded.
export function injectKeylessProviderSkips(collection, keyless) {
  const counts = new Map();
  if (!keyless || keyless.size === 0) return counts;
  for (const item of walk(collection.item)) {
    const provider = bodyModelProvider(item);
    const refs = keyless.get(provider);
    if (!refs) continue;
    const events = (item.event || []).filter(
      (e) => !(e.listen === "prerequest" && (e.script?.exec || []).some((l) => l.includes(GUARD_MARKER)))
    );
    const existing = events.find((e) => e.listen === "prerequest");
    const lines = guardScript(item.name, refs);
    if (existing) {
      existing.script = { ...existing.script, type: existing.script?.type || "text/javascript", exec: [...lines, ...(existing.script?.exec || [])] };
      item.event = events;
    } else {
      item.event = [{ listen: "prerequest", script: { type: "text/javascript", exec: lines } }, ...events];
    }
    counts.set(provider, (counts.get(provider) || 0) + 1);
  }
  return counts;
}

// One line per skipped provider, for the run log.
export function describeKeylessSkips(keyless, counts) {
  return [...counts.entries()].map(
    ([provider, n]) => `skipped: no key - ${provider} (${keyless.get(provider).join(", ")} unset): ${n} request${n === 1 ? "" : "s"}`
  );
}
