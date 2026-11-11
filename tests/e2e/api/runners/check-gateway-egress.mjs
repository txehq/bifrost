// Preflight for make run-provider-harness-test (TXE-7083): a fixture run must not reach a real
// vendor. A run is a fixture run when its environment file sets providerErrorFixture=1, or when
// it already runs under scripts/test-offline.sh (BIFROST_TEST_OFFLINE=1). For such a run:
//   1. the harness itself must run under scripts/test-offline.sh, and
//   2. the gateway must refuse egress: a chat request to the egress_probe provider (base_url on
//      TEST-NET-3, in provider-error-fixture.config.json) must come back with the dial guard's
//      refusal, which only a gateway started under scripts/test-offline.sh gives.
// A gateway with no egress_probe provider cannot be checked; that is printed, not failed.
//
//   node check-gateway-egress.mjs <base-url> [env-file]
// Exit 0: live run, or a verified fixture run. Exit 1: refuse to run, with the reason.
import { readFileSync } from "node:fs";

export const MARKER = "offline run (BIFROST_TEST_OFFLINE=1): egress to";

export function isFixtureRun(envFileText) {
  if (!envFileText) return false;
  try {
    const values = JSON.parse(envFileText).values || [];
    return values.some((v) => v.key === "providerErrorFixture" && String(v.value) === "1" && v.enabled !== false);
  } catch {
    return false;
  }
}

// classifyProbe reads the gateway's answer to the egress probe. Only an answer that the
// egress_probe provider itself produced (extra_fields.routing_info.provider) says anything about
// the guard: a gateway without that provider routes the model elsewhere, or refuses it.
export function classifyProbe(status, body) {
  if (body.includes(MARKER)) return { verdict: "guarded" };
  let provider;
  try {
    provider = JSON.parse(body)?.extra_fields?.routing_info?.provider;
  } catch {
    provider = undefined;
  }
  if (provider !== "egress_probe") return { verdict: "unknown", detail: `HTTP ${status}, served by ${provider || "no provider"}` };
  return { verdict: "unguarded", detail: `HTTP ${status}: ${body.slice(0, 300)}` };
}

async function main() {
  const [baseURL, envFile] = process.argv.slice(2);
  const fixture = envFile ? isFixtureRun(readFileSync(envFile, "utf8")) : false;
  const offline = process.env.BIFROST_TEST_OFFLINE === "1";
  if (!fixture && !offline) return 0;
  if (!offline) {
    console.error("[egress] refusing a fixture run (providerErrorFixture=1) outside scripts/test-offline.sh: a provider that ignores base_url would reach the real vendor (TXE-7083). Run: scripts/test-offline.sh make run-provider-harness-test ...");
    return 1;
  }
  let status = 0;
  let body = "";
  try {
    const res = await fetch(`${baseURL.replace(/\/$/, "")}/v1/chat/completions`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ model: "egress_probe/egress-probe", messages: [{ role: "user", content: "egress probe" }] }),
      signal: AbortSignal.timeout(20000),
    });
    status = res.status;
    body = await res.text();
  } catch (err) {
    body = String(err);
  }
  const { verdict, detail } = classifyProbe(status, body);
  if (verdict === "guarded") {
    console.log("[egress] the gateway refuses non-loopback egress (started under scripts/test-offline.sh)");
    return 0;
  }
  if (verdict === "unknown" && status !== 0) {
    console.log(`[egress] the gateway has no egress_probe provider (${detail}), so its egress guard cannot be checked; start it under scripts/test-offline.sh`);
    return 0;
  }
  console.error(`[egress] the gateway at ${baseURL} is not guarded or did not answer: the egress probe did not get the dial guard's refusal (${detail || body.slice(0, 300)}). Start the gateway under scripts/test-offline.sh (TXE-7083).`);
  return 1;
}

if (import.meta.url === `file://${process.argv[1]}`) {
  main().then((code) => process.exit(code));
}
