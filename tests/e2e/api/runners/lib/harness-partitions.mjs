// Which provider fork of the harness sweep runs a collection row (fork, TXE-7308).
//
// The sweep forks one newman per partition. A partition is a registered provider
// (schemas.StandardProviders in core/schemas/bifrost.go), a reseller custom provider of the
// harness profile, or a route family (passthrough). The roster is never kept by hand: it is the
// registry plus the two lists below. A top-level folder holding a row no partition claims (a
// setup, reset or read-back step) runs whole, in order, in the serial pass instead
// (filter-collection.mjs serialFolderKey), so every row runs somewhere. Before TXE-7308 the
// Makefile kept HARNESS_PROVIDERS by hand, so rows for bfl, replicate, runware, voyage and
// typesafe matched no fork and never ran in a parallel sweep.
// tests/e2e/api/runners/lib/harness-partitions.test.mjs fails when a harness row is claimed by
// no named partition outside a serial-pass folder.
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { buildHaystack } from "./haystack.mjs";
import { registeredProviders } from "./base-url-support.mjs";

export const PROVIDER_KEYWORDS = {
  openai: ["openai", "/openai", "gpt-", "o3", "o1"],
  anthropic: ["anthropic", "claude-"],
  bedrock: ["bedrock", "/bedrock"],
  bedrock_mantle: ["bedrock_mantle", "bedrock-mantle"],
  gemini: ["gemini", "/genai", "googlesearch"],
  vertex: ["vertex", "/genai/v1beta/models/{{vertexModel}}"],
  azure: ["azure", "deployments"],
  passthrough: ["_passthrough"],
  openrouter: ["openrouter"],
  xai: ["xai", "grok"],
  replicate: ["replicate", "/replicate", "flux", "black-forest-labs"],
  runware: ["runware", "runware/"],
  voyage: ["voyage", "voyage-"],
  typesafe: ["typesafe", "/typesafe", "jev-"],
};

// Custom providers in the harness profile that resell other vendors' models; see
// itemMatchesProvider. Each is also a HARNESS_PROVIDERS partition in the Makefile.
export const RESELLER_PROVIDERS = ["deepinfra", "vercel_ai_gateway"];

// itemMatchesProvider reports whether the named partition claims the row. Taking the partition as
// a parameter, not a global, keeps the planner's cells identical to the shards' own selection.
// A row's haystack is the same for every partition asked about it, and a roster asks about 50 or
// more, so it is built once per row. Rows are the collection's own objects, keyed weakly.
const haystacks = new WeakMap();
const haystackOf = (item, ancestorNames) => {
  let haystack = haystacks.get(item);
  if (haystack === undefined) {
    haystack = buildHaystack(item, ancestorNames);
    haystacks.set(item, haystack);
  }
  return haystack;
};

export const itemMatchesProvider = (item, ancestorNames, provider) => {
  if (!provider) return true;
  const keywords = PROVIDER_KEYWORDS[provider] || [provider];
  const haystack = haystackOf(item, ancestorNames);
  // OpenRouter rows (model "openrouter/<vendor>/<model>") embed vendor substrings like
  // gpt-/claude-/gemini, so they'd otherwise be claimed by the openai/anthropic/gemini
  // partitions too. Route them exclusively to the openrouter partition.
  const isOpenRouter = haystack.includes("openrouter");
  if (provider === "openrouter") return isOpenRouter;
  if (isOpenRouter) return false;
  // Bedrock Mantle rows (model "bedrock_mantle/...") contain the substring "bedrock", so they'd
  // otherwise be claimed by the bedrock partition too. Route them exclusively to bedrock_mantle.
  const isMantle = haystack.includes("bedrock_mantle") || haystack.includes("bedrock-mantle");
  if (provider === "bedrock_mantle") return isMantle;
  if (isMantle) return false;
  // Vertex rows run Gemini models (model "vertex/gemini-..."), so they'd otherwise be claimed
  // by the gemini partition too - same collision class as openrouter/bedrock_mantle above.
  // Route them exclusively to vertex. Vertex Model Garden rows ("vertex/minimaxai/minimax-m2-maas")
  // name the model vendor the same way, so minimax is excluded for the same reason.
  const isVertex = PROVIDER_KEYWORDS.vertex.some((k) => haystack.includes(k));
  if (provider === "vertex") return isVertex;
  if (isVertex && (provider === "gemini" || provider === "anthropic" || provider === "minimax")) return false;
  // Rows that send a Gemini, OpenAI or Azure model through the Anthropic Messages ingress
  // (POST /anthropic/v1/messages with model "gemini/...", "openai/..." or "azure/...") contain
  // "anthropic" only because of the route or folder name, yet the upstream call goes to the
  // model's own provider - same collision class as vertex above. Keep them out of the anthropic
  // partition; they still match their own provider's keywords through the model prefix.
  if (provider === "anthropic" && /"model"\s*:\s*"(gemini|openai|azure)\//.test(item.request?.body?.raw || "")) return false;
  // Runware rows name the upstream vendor inside the AIR model id ("runware/anthropic:claude@...",
  // "runware/google:gemini@...", "runware/minimax:..."), so they'd otherwise be claimed by those
  // partitions too - same collision class as openrouter/bedrock_mantle/vertex above. Route them
  // exclusively to runware, so an anthropic- or gemini-filtered run does not try to reach Runware.
  const isRunware = haystack.includes("runware");
  if (provider === "runware") return isRunware;
  if (isRunware) return false;
  // OpenAI-compatible custom providers that resell other vendors' models name that vendor in the
  // model id ("vercel_ai_gateway/cohere/embed-v4.0", "deepinfra/BAAI/bge-m3"), so a cohere- or
  // openai-filtered run would otherwise claim their rows - same collision class as openrouter.
  // Route each exclusively to its own partition. Matched on the body's model prefix, not a haystack
  // substring: one folder can hold rows for several resellers, and folder names are in the haystack.
  const model = /"model"\s*:\s*"([^"/]+)\//.exec(item.request?.body?.raw || "")?.[1];
  for (const reseller of RESELLER_PROVIDERS) {
    const isReseller = model === reseller;
    if (provider === reseller) return isReseller;
    if (isReseller) return false;
  }
  // bedrock_openai rows (token-parity-matrix.mjs's "one more model per provider" addition -
  // gpt-oss-family models on Bedrock) contain "openai" in the backend key/model, so they'd
  // otherwise be claimed by the openai partition too - same collision class as above. Route
  // them exclusively to bedrock (they already match PROVIDER_KEYWORDS.bedrock's "bedrock"
  // keyword with no extra logic needed for that direction).
  // BFL rows name FLUX models ("bfl/flux-2-pro"), and "flux" is one of replicate's keywords for the
  // same models hosted on Replicate, so replicate would claim every BFL row too. A row whose model
  // is BFL's runs in the bfl partition only (TXE-7308).
  if (provider === "replicate" && model === "bfl") return false;
  const isBedrockOpenai = haystack.includes("bedrock_openai");
  if (isBedrockOpenai && provider === "openai") return false;
  // A row whose model names the provider ("fireworks/accounts/fireworks/models/...") is that
  // provider's row even when the haystack lost the name: stripBase64Blobs drops any 40+ character
  // run of base64-alphabet characters, and a long model path is one (TXE-7308).
  return model === provider || keywords.some((k) => haystack.includes(k));
};


// Partitions that are a route family rather than a provider.
export const ROUTE_PARTITIONS = ["passthrough"];

// harnessPartitions is the sweep's roster of named partitions, in a stable order: every
// registered provider, then the reseller custom providers, then the route families.
export function harnessPartitions(repoRoot) {
  const registry = registeredProviders(readFileSync(join(repoRoot, "core", "schemas", "bifrost.go"), "utf8"));
  return [...new Set([...registry, ...RESELLER_PROVIDERS, ...ROUTE_PARTITIONS])];
}

// isUnclaimed reports whether no named partition claims the row.
export const isUnclaimed = (item, ancestorNames, partitions) =>
  !partitions.some((p) => itemMatchesProvider(item, ancestorNames, p));

const topFolderOf = (ancestors) => (ancestors && ancestors[0]) || "";

// A fixture's shared state: its counters, recorded requests and probes (/__reset, /__hits,
// /__sent, /__wire, /__probe, ...). Every fixture endpoint that is not a vendor route starts "/__".
const FIXTURE_STATE_PATH = /\/__[a-z]/i;
const urlOf = (item) => (typeof item.request?.url === "string" ? item.request.url : item.request?.url?.raw) || "";

// Partitioner answers, for one collection, which partition runs a row and which top-level folders
// must run whole in the serial pass: one newman, in collection order, alone after the sweep
// (filter-collection.mjs serialFolderKey). A folder goes there, besides a [SERIAL] tag, when
//
//   - it holds a row no partition claims (a setup, reset or read-back step, a management call),
//     which no provider fork would run, or
//   - it reads or resets a fixture's shared state. Folder 154 resets the counters, calls openai,
//     then asserts the counts; folder 472 creates a BFL task, then reads what the fixture
//     received. The class and sub-shard axes split those steps into concurrent newman processes
//     (the create is image-gen, the read-back other), and two such folders in different forks
//     reset or read each other's counters.
export class Partitioner {
  constructor(entries, partitions) {
    this.entries = entries;
    this.partitions = partitions;
    this.serialFolders = null;
  }

  // runs reports whether the partition's fork runs the row.
  runs(item, ancestors, partition) {
    return itemMatchesProvider(item, ancestors, partition);
  }

  // serialPassFolders is the set of top-level folders that run whole in the serial pass.
  serialPassFolders() {
    if (!this.serialFolders) {
      this.serialFolders = new Set();
      for (const { item, ancestors } of this.entries) {
        const folder = topFolderOf(ancestors);
        if (this.serialFolders.has(folder)) continue;
        if (FIXTURE_STATE_PATH.test(urlOf(item)) || isUnclaimed(item, ancestors, this.partitions)) this.serialFolders.add(folder);
      }
    }
    return this.serialFolders;
  }

  // roster is the partitions that claim at least one of the given rows, in roster order.
  roster(selected) {
    return this.partitions.filter((p) => selected.some(({ item, ancestors }) => this.runs(item, ancestors, p)));
  }
}
