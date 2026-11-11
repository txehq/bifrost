// The base_url support table (core/providers/base_url_support.json, TXE-7083) and the checks
// that keep it true: every registered provider is in it, a provider whose package never reads
// BaseURL is listed as "ignores", and no fixture config points an "ignores" provider at a
// fixture through network_config.base_url (Bedrock sent two requests to AWS that way, TXE-7080).
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";

export const SUPPORT_VALUES = new Set(["honours", "fallback", "ignores"]);

// registeredProviders reads schemas.StandardProviders from core/schemas/bifrost.go and maps each
// constant to its string value. Throws when the source shape changes, so the check never passes
// on an empty list.
export function registeredProviders(bifrostGoSource) {
  const values = new Map();
  for (const [, name, value] of bifrostGoSource.matchAll(/^\s+(\w+)\s+ModelProvider\s*=\s*"([^"]+)"/gm)) {
    values.set(name, value);
  }
  const block = bifrostGoSource.match(/var StandardProviders = \[\]ModelProvider\{([\s\S]*?)\n\}/);
  if (!block) throw new Error("StandardProviders not found in core/schemas/bifrost.go");
  const names = [...block[1].matchAll(/\b(\w+),/g)].map((m) => m[1]);
  const unknown = names.filter((name) => !values.has(name));
  if (unknown.length) throw new Error(`StandardProviders names constants with no value: ${unknown.join(", ")}`);
  if (names.length < 10) throw new Error(`only ${names.length} StandardProviders parsed; the source shape changed`);
  return names.map((name) => values.get(name));
}

// Providers whose constructor lives in a package not named after them (core/bifrost.go,
// createBaseProvider).
const PACKAGE_OVERRIDES = { modelark: "volcengine", "opencode-go": "opencode", "opencode-zen": "opencode" };

// providerPackage is the core/providers directory a provider value lives in.
export function providerPackage(provider) {
  return PACKAGE_OVERRIDES[provider] || provider.replace(/[-_]/g, "");
}

// tableProblems compares the table with the registered providers and, through readsBaseURL
// (provider -> whether its package's non-test Go code mentions BaseURL), with the code.
export function tableProblems(table, registered, readsBaseURL) {
  const problems = [];
  const entries = table.providers || {};
  for (const provider of registered) {
    if (!(provider in entries)) problems.push(`${provider}: registered but missing from the table; add it as honours, fallback or ignores`);
  }
  for (const [provider, entry] of Object.entries(entries)) {
    if (!registered.includes(provider)) problems.push(`${provider}: in the table but not registered (stale entry)`);
    if (!SUPPORT_VALUES.has(entry.base_url)) problems.push(`${provider}: base_url is "${entry.base_url}", want honours, fallback or ignores`);
    if (entry.base_url !== "honours" && (!entry.host_from || !entry.fixture)) {
      problems.push(`${provider}: a provider that does not honour base_url needs host_from and fixture`);
    }
    if (entry.base_url !== "ignores" && readsBaseURL(provider) === false) {
      problems.push(`${provider}: listed as ${entry.base_url}, but core/providers/${providerPackage(provider)} never reads BaseURL`);
    }
  }
  return problems;
}

// fixtureConfigFiles lists the JSON files under dir (recursively) that carry a providers object.
export function fixtureConfigFiles(dir) {
  const out = [];
  for (const name of readdirSync(dir)) {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) {
      out.push(...fixtureConfigFiles(path));
    } else if (name.endsWith(".json")) {
      try {
        const parsed = JSON.parse(readFileSync(path, "utf8"));
        if (parsed && typeof parsed.providers === "object" && !Array.isArray(parsed.providers)) out.push(path);
      } catch {
        // not a config file
      }
    }
  }
  return out.sort();
}

// configProblems finds providers in a Bifrost config that are pointed at a host through
// network_config.base_url although their type ignores it. A custom provider is judged by its
// base_provider_type.
export function configProblems(config, table, label) {
  const problems = [];
  for (const [name, provider] of Object.entries(config.providers || {})) {
    const type = provider?.custom_provider_config?.base_provider_type || name;
    const entry = table.providers?.[type];
    if (entry?.base_url === "ignores" && provider?.network_config?.base_url) {
      problems.push(`${label}: providers.${name} (${type}) sets network_config.base_url, which ${type} ignores (host from ${entry.host_from}); instead ${entry.fixture}`);
    }
  }
  return problems;
}

export function relativeTo(root, path) {
  return relative(root, path);
}
