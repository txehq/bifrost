#!/usr/bin/env bash
set -euo pipefail

# Three guards on harden-runner egress allowlists:
#
#   1. every job that runs apt-get under `egress-policy: block` allowlists the
#      Ubuntu APT hosts it will need;
#   2. every job that runs the provider harness allowlists each external host
#      the harness will dial - see the second python block for why that is not
#      the same set as "the hosts the collection sends requests to";
#   3. every job that builds the release binaries allowlists the Go module proxy
#      and checksum database the workspace build verifies modules against.
#
# harden-runner's `egress-policy: block` drops every destination absent from
# `allowed-endpoints`, so a job that shells out to `apt-get update` fails at the
# mirror fetch unless those hosts are listed. The failure only surfaces when the
# workflow actually runs - release-pipeline.yml triggers on push to main, not on
# pull requests - so a job added with a copied-but-incomplete allowlist reaches
# production untested and takes the release with it.
#
# Usage: ./check-egress-allowlist.sh [workflow.yml ...]
#   With no arguments, checks every workflow in .github/workflows.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKFLOW_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
# Exported for the python blocks below. They run from stdin (`python3 - ...`),
# where __file__ is "<stdin>" rather than a path - deriving the repo root there
# silently resolves somewhere above the checkout, every input file reads as
# absent, and the check passes having examined nothing.
REPO_ROOT="$(cd "${WORKFLOW_DIR}/../.." && pwd)"
export REPO_ROOT

if [[ $# -gt 0 ]]; then
  FILES=("$@")
else
  FILES=()
  while IFS= read -r f; do FILES+=("$f"); done < <(find "${WORKFLOW_DIR}" -maxdepth 1 -name '*.yml' -o -maxdepth 1 -name '*.yaml' | sort)
fi

python3 - "${FILES[@]}" <<'PY'
import sys
import yaml

# Hosts apt-get needs on a GitHub-hosted ubuntu runner. azure.archive.ubuntu.com
# is the Azure-local mirror the runner images point at (plain HTTP), and
# esm.ubuntu.com serves the Expanded Security Maintenance lists apt refreshes
# alongside it.
REQUIRED_APT_HOSTS = ("azure.archive.ubuntu.com:80", "esm.ubuntu.com:443")

failures = []
checked = 0

for path in sys.argv[1:]:
    with open(path) as fh:
        doc = yaml.safe_load(fh) or {}

    for job_name, job in (doc.get("jobs") or {}).items():
        if not isinstance(job, dict):
            continue
        steps = job.get("steps") or []

        uses_apt = any(
            "apt-get" in (step.get("run") or "")
            for step in steps
            if isinstance(step, dict)
        )
        if not uses_apt:
            continue

        harden = next(
            (
                step
                for step in steps
                if isinstance(step, dict)
                and "step-security/harden-runner" in (step.get("uses") or "")
            ),
            None,
        )
        if harden is None:
            continue

        with_block = harden.get("with") or {}
        if str(with_block.get("egress-policy", "")).strip() != "block":
            continue

        checked += 1
        allowed = set((with_block.get("allowed-endpoints") or "").split())
        missing = [host for host in REQUIRED_APT_HOSTS if host not in allowed]
        if missing:
            failures.append((path, job_name, missing))

for path, job_name, missing in failures:
    print(f"FAIL {path} :: job '{job_name}' runs apt-get under egress-policy: block "
          f"but omits {', '.join(missing)}", file=sys.stderr)

if failures:
    print(f"\n{len(failures)} of {checked} apt-using blocked-egress job(s) have an "
          f"incomplete allowlist.", file=sys.stderr)
    sys.exit(1)

print(f"OK: all {checked} apt-using blocked-egress job(s) allowlist the Ubuntu APT hosts.")
PY

# ---------------------------------------------------------------------------
# 2. Provider-harness hosts
# ---------------------------------------------------------------------------
#
# The harness sends nearly every request to 127.0.0.1:8080, so its collection
# looks like it needs no egress at all. It does: bifrost-http runs on the SAME
# runner and makes the upstream provider calls, and it downloads any URL passed
# as a document/image source for providers whose APIs cannot take a URL
# directly. Those fetches leave the runner and are governed by the same
# allowlist.
#
# That combination is why this check exists. A blocked host surfaces as a
# connection error attributed to the provider, so it reads as a flaky provider
# rather than a firewall - and release-pipeline.yml only runs on push to main,
# so the first time anyone sees it is during a real release.
#
# The rule enforced here is deliberately blunt: every external host appearing in
# the harness collection or its seeded provider config must be either
# allowlisted or listed in NOT_DIALLED_HOSTS with a reason. The one exception is
# text that is documentation by construction - Postman description fields and
# whole-line script comments - which is not scanned at all. Classifying
# automatically would mean deciding which URLs Bifrost fetches and which the
# provider fetches, which depends on per-provider API shape and would silently
# get it wrong. Forcing a human to say which one it is cannot.

python3 - "${FILES[@]}" <<'PY'
import json
import os
import re
import sys
import yaml

REPO_ROOT = os.environ["REPO_ROOT"]
COLLECTION = os.path.join(REPO_ROOT, "tests/e2e/api/collections/provider-harness.json")
PROVIDER_CONFIG = os.path.join(REPO_ROOT, "tests/integrations/python/config.json")
PROVIDER_DEFAULT_HOSTS = os.path.join(REPO_ROOT, ".github/workflows/provider-default-hosts.json")

# A missing input means this check verified nothing, which must be an error and
# not a silent pass - that is the exact failure mode the REPO_ROOT export fixes.
for required_input in (COLLECTION, PROVIDER_CONFIG, PROVIDER_DEFAULT_HOSTS):
    if not os.path.exists(required_input):
        print(f"::error::egress check input not found: {required_input}", file=sys.stderr)
        sys.exit(1)

# Scripts that mean "this job runs the provider harness".
HARNESS_MARKERS = ("test-core.sh", "test-provider-harness.sh")

# Hosts that appear in the sources scanned above but that the runner never
# dials, each with the reason it is exempt. Anything not listed here and not
# allowlisted fails this check, so a new host forces an explicit decision rather
# than being quietly assumed harmless.
NOT_DIALLED_HOSTS = {
    # Fetched by the provider's own server-side tooling, never by Bifrost.
    "example.com": "MCP server_url and web_fetch targets - dereferenced by the provider",
    "mcp.deepwiki.com": "Anthropic mcp_servers url (12.B17 beta-header row) - Anthropic's MCP connector dials it",
    "www.youtube.com": "Gemini video input - Google fetches the URL server-side",
    "www.wikipedia.org": "Gemini urlContext target in a prompt - Google fetches it server-side",
    "does-not-exist-h146.search.windows.net": "Azure On Your Data azure_search endpoint in a request body; Azure dereferences it",
    "replace-me.search.windows.net": "Azure AI Search placeholder collection variable; never resolved",
    "does-not-exist-h150.invalid": "OpenAI Responses MCP tool server_url; OpenAI dials it, and .invalid never resolves",
    "en.wikipedia.org": "googleSearch excludeDomains filter value, not a URL anyone fetches",
    "petstore3.swagger.io": "servers/base_url inside the inline specs of the 229 mcp-openapi-spec rows; no tool is called, and the rows carry the setup token, so the auth-bypassed public-target DNS check never runs",
    "upload.wikimedia.org": "Volcengine multimodal embedding image_url in the 277 rows - Volcengine fetches it server-side",
    "raw.githubusercontent.com": "Groq transcription audio url in the 310 rows - Groq fetches it server-side; Bifrost forwards the url and never downloads it, and the openai row is refused before any upstream call",
    "placeholder.search.windows.net": "Azure AI Search placeholder in a request body; never resolved",
    "client.example": "push callback URL the gateway stores in the 266 a2a-push-tenancy rows; the fixture agent never pushes, so no delivery dials it",
    "intruder.example": "another tenant's attempted push callback URL in the 266 a2a-push-tenancy rows; refused before anything is stored or dialled",
    # Deliberately unresolvable - negative tests assert the failure path.
    "bifrost.invalid": "reserved .invalid TLD; negative-path tests assert it fails to resolve",
    "does-not-resolve.invalid": "reserved .invalid TLD; the unresolvable MCP client probe asserts the connect fails",
    "harness-endpoint-guard.example.invalid": "reserved .invalid TLD; the endpoint-guard row asserts the key endpoint change is refused",
    "harness-lib-installed.invalid": "reserved .invalid TLD; connection_url of the MCP library entry in the 248 installed-flag rows, stored only - the client those rows install is an inline-spec OpenAPI client",
    "unlisted.example": "reserved .example TLD; a redirect_uri value in the OAuth registration redirect-check row, never fetched",
    "harness.example": "reserved .example TLD; an OAuth redirect_uri value in the 114 raw-path-auth rows, which do not follow redirects",
    "dbx-fixture-workspace.invalid": "reserved .invalid TLD; the Databricks OAuth row asserts the token request fails",
    "harness-credential-guard.example.invalid": "reserved .invalid TLD; a token_url inside the credential-guard row's refused external_account body",
    "harness-plugin-guard.example.invalid": "reserved .invalid TLD; the plugin-guard row asserts the refused collector_url is never stored",
    "docs.openapi.example": "reserved .example TLD; a spec_url beside the inline documents of the 272 openapi-preservation previews, which are never fetched",
    "self.openapi.example": "reserved .example TLD; $self in a 272 openapi-preservation inline document, previewed and never dialled",
    "pinned.openapi.example": "reserved .example TLD; a base_url override in a 272 openapi-preservation preview, never dialled",
    "api.openapi.example": "reserved .example TLD; an operation server in a 272 openapi-preservation inline document, previewed and never dialled",
    "server.openapi.example": "reserved .example TLD; a server in the 272 openapi-preservation inline documents, previewed and never dialled",
    "idp.example": "reserved .example TLD; oauth_key_config token_url in the OAuth-minted key rows, never resolves",
    "anthropic.com": "Gemini urlContext target in a prompt (12 backlog row) - Google fetches it server-side",
    # Documentation links in a folder or request description, or in a whole-line
    # test-script comment, need no entry here: hosts_in_collection() does not
    # scan that text (TXE-6978).
    "schema.getpostman.com": "Postman collection schema declaration, not fetched at run time",
    # Identifiers that merely look like endpoints.
    "s3.amazonaws.com": "XML namespace URI in a ListBucketResult document; namespaces are names, not fetches",
    "www.googleapis.com": "the cloud-platform OAuth SCOPE string; the token exchange itself goes to oauth2.googleapis.com",
    "www.ietf.org": "an RTP header-extension URI in a WebRTC SDP offer; extmap URIs are identifiers, not fetches",
}

HOST_RE = re.compile(r"https?://([A-Za-z0-9._-]+\.[A-Za-z]{2,})")
# {{var}} placeholders resolve at run time from secrets; the concrete hosts they
# expand to (regional Bedrock/Vertex endpoints) cannot be known statically, so
# they are out of scope here and stay a manual review item.
TEMPLATED = re.compile(r"\{\{")


def hosts_in(path):
    if not os.path.exists(path):
        return set()
    with open(path, encoding="utf-8") as fh:
        return hosts_in_text(fh.read())


def hosts_in_text(raw):
    found = set()
    for line in raw.splitlines():
        if TEMPLATED.search(line):
            # Strip only URLs whose AUTHORITY is templated -- those expand to hosts that
            # cannot be known statically. A literal host with a templated PATH
            # ("https://files.example.com/{{file_id}}") still dials a knowable host and
            # must stay in the census, or the validator passes a blocked-egress job that
            # harden-runner will then kill at the request. Excluding "/" from the span
            # before the placeholder is what draws that line.
            line = re.sub(r"https?://[^\s\"'/]*\{\{[^\s\"']*", " ", line)
        for m in HOST_RE.finditer(line):
            found.add(m.group(1))
    return found


def collection_without_documentation(node, parent_key=None):
    """Drop the parts of a Postman collection that are documentation, not requests.

    A `description` (collection, folder, request, header, variable) is prose that
    newman never sends, and a whole-line `//` comment in a test or pre-request
    script never runs. A vendor documentation link belongs in exactly those two
    places, and read as a dialled host it failed this check on every pull request
    until someone listed the docs site by hand: platform.kimi.ai after the Moonshot
    harness folder (TXE-6978), and docs.x.ai, docs.aws.amazon.com, ai.google.dev
    and five more before it. Request URLs, headers, bodies and script code are
    all still scanned, so a host the harness can dial is not hidden by this.
    """
    if isinstance(node, dict):
        return {
            key: collection_without_documentation(value, key)
            for key, value in node.items()
            if key != "description"
        }
    if isinstance(node, list):
        if parent_key == "exec":
            # An exec entry is usually one line of the script, but some entries hold
            # a whole multi-line script. Comments are dropped per physical line, so a
            # script that merely begins with a comment keeps every statement after it.
            return [
                "\n".join(
                    line for line in entry.split("\n") if not line.lstrip().startswith("//")
                )
                if isinstance(entry, str)
                else entry
                for entry in node
            ]
        return [collection_without_documentation(item, parent_key) for item in node]
    return node


def hosts_in_collection(path):
    with open(path, encoding="utf-8") as fh:
        collection = json.load(fh)
    # Re-serialised one value per line so the line-based scan below still sees a
    # templated URL and a literal one on separate lines.
    text = json.dumps(collection_without_documentation(collection), indent=1, ensure_ascii=False)
    return hosts_in_text(text)


# Provider endpoints Bifrost builds in Go beyond a provider's one default host
# (those come from default_hosts_of_profile_providers below), which
# is why scanning the seeded config alone is not enough: discoveryengine
# (the Vertex reranker backend) is assembled in core/providers/vertex and appears
# in no config file, so it was missing from two workflows' allowlists at once.
# Restricted to the cloud-provider domains whose hostnames are constructed this
# way; anything else in that source tree is a doc link, not a dial.
GO_HOST_SUFFIXES = (".googleapis.com", ".amazonaws.com", ".api.aws")


def hosts_in_go(root):
    found = set()
    providers_dir = os.path.join(root, "core", "providers")
    for dirpath, _, filenames in os.walk(providers_dir):
        for name in filenames:
            if not name.endswith(".go") or name.endswith("_test.go"):
                continue
            with open(os.path.join(dirpath, name), encoding="utf-8", errors="ignore") as fh:
                text = fh.read()
            for m in HOST_RE.finditer(text):
                host = m.group(1)
                if host.endswith(GO_HOST_SUFFIXES):
                    found.add(host)
    return found


def default_hosts_of_profile_providers(config_path, hosts_path):
    """The host each provider in the harness profile dials when it sets no base_url.

    A provider configured without network_config.base_url uses the default its Go
    constructor sets, and that default is in neither the collection nor the config:
    moonshot was added to the profile and api.moonshot.ai was dialled from jobs whose
    allowlist nobody had been told to extend (TXE-6547, TXE-6981). The defaults are
    listed in provider-default-hosts.json, which a core test regenerates from the
    constructors themselves (TestProviderDefaultHostsFileIsCurrent), so this needs no
    parsing of Go and a new provider is covered without a change here.

    A custom provider dials its base provider's host. A provider the list does not
    know is an error, not a skip: it means the list is stale, and skipping would pass
    the provider this check exists to catch.
    """
    with open(config_path, encoding="utf-8") as fh:
        providers = (json.load(fh).get("providers") or {})
    with open(hosts_path, encoding="utf-8") as fh:
        default_hosts = json.load(fh)

    hosts, unknown = set(), []
    for name, provider in sorted(providers.items()):
        provider = provider or {}
        if (provider.get("network_config") or {}).get("base_url"):
            continue  # an explicit base_url is in the config, which hosts_in() scans
        base = (provider.get("custom_provider_config") or {}).get("base_provider_type") or name
        if base not in default_hosts:
            unknown.append((name, base))
            continue
        if default_hosts[base]:
            hosts.add(default_hosts[base])
    return hosts, unknown


profile_default_hosts, unknown_providers = default_hosts_of_profile_providers(
    PROVIDER_CONFIG, PROVIDER_DEFAULT_HOSTS
)
if unknown_providers:
    for name, base in unknown_providers:
        print(
            f"FAIL harness profile provider '{name}' (base provider '{base}') has no entry in "
            f".github/workflows/provider-default-hosts.json",
            file=sys.stderr,
        )
    print(
        "\nThat file lists the host each provider dials by default. Regenerate it from core:\n"
        "  BIFROST_UPDATE_PROVIDER_DEFAULT_HOSTS=1 go test . -run TestProviderDefaultHostsFileIsCurrent",
        file=sys.stderr,
    )
    sys.exit(1)

required = (
    hosts_in_collection(COLLECTION)
    | hosts_in(PROVIDER_CONFIG)
    | hosts_in_go(REPO_ROOT)
    | profile_default_hosts
)
required -= set(NOT_DIALLED_HOSTS)
required = {h for h in required if h not in ("localhost",)}

failures = []
checked = 0

for path in sys.argv[1:]:
    with open(path, encoding="utf-8") as fh:
        doc = yaml.safe_load(fh) or {}

    for job_name, job in (doc.get("jobs") or {}).items():
        if not isinstance(job, dict):
            continue
        steps = job.get("steps") or []

        runs_harness = any(
            any(marker in (step.get("run") or "") for marker in HARNESS_MARKERS)
            for step in steps
            if isinstance(step, dict)
        )
        if not runs_harness:
            continue

        harden = next(
            (
                step
                for step in steps
                if isinstance(step, dict)
                and "step-security/harden-runner" in (step.get("uses") or "")
            ),
            None,
        )
        if harden is None:
            continue

        with_block = harden.get("with") or {}
        if str(with_block.get("egress-policy", "")).strip() != "block":
            continue

        checked += 1
        allowed = {
            entry.rsplit(":", 1)[0]
            for entry in (with_block.get("allowed-endpoints") or "").split()
        }
        missing = sorted(h for h in required if h not in allowed)
        if missing:
            failures.append((path, job_name, missing))

for path, job_name, missing in failures:
    print(
        f"FAIL {path} :: job '{job_name}' runs the provider harness under "
        f"egress-policy: block but omits {', '.join(missing)}",
        file=sys.stderr,
    )

if failures:
    print(
        "\nEach host above appears in the provider harness collection or its seeded\n"
        "provider config, or is the default host of a provider in that config\n"
        "(.github/workflows/provider-default-hosts.json). Either add it to that job's\n"
        "allowed-endpoints, or - if the provider dereferences it server-side and the\n"
        "runner never dials it - add it to NOT_DIALLED_HOSTS in this script with the reason.",
        file=sys.stderr,
    )
    sys.exit(1)

if checked == 0:
    print("OK: no harness-running blocked-egress jobs found.")
else:
    print(
        f"OK: all {checked} harness-running blocked-egress job(s) allowlist the "
        f"{len(required)} host(s) the harness dials."
    )
PY

# ---------------------------------------------------------------------------
# 3. Go module hosts for the release binary build
# ---------------------------------------------------------------------------
#
# build-executables.sh builds through go.work. In workspace mode go verifies
# every module that no workspace go.sum lists against the checksum database,
# and go.work.sum is not committed, so the build dials sum.golang.org as well
# as proxy.golang.org. With sum.golang.org blocked the build fails with
# "verifying module: ... connection refused" (TXE-4972), and only on push to
# main, after every test has passed.

python3 - "${FILES[@]}" <<'PY'
import sys
import yaml

BUILD_MARKERS = ("build-executables.sh",)
REQUIRED_GO_HOSTS = ("proxy.golang.org:443", "sum.golang.org:443")

failures = []
checked = 0

for path in sys.argv[1:]:
    with open(path, encoding="utf-8") as fh:
        doc = yaml.safe_load(fh) or {}

    for job_name, job in (doc.get("jobs") or {}).items():
        if not isinstance(job, dict):
            continue
        steps = job.get("steps") or []

        builds_binaries = any(
            any(marker in (step.get("run") or "") for marker in BUILD_MARKERS)
            for step in steps
            if isinstance(step, dict)
        )
        if not builds_binaries:
            continue

        harden = next(
            (
                step
                for step in steps
                if isinstance(step, dict)
                and "step-security/harden-runner" in (step.get("uses") or "")
            ),
            None,
        )
        if harden is None:
            continue

        with_block = harden.get("with") or {}
        if str(with_block.get("egress-policy", "")).strip() != "block":
            continue

        checked += 1
        allowed = set((with_block.get("allowed-endpoints") or "").split())
        missing = [host for host in REQUIRED_GO_HOSTS if host not in allowed]
        if missing:
            failures.append((path, job_name, missing))

for path, job_name, missing in failures:
    print(
        f"FAIL {path} :: job '{job_name}' builds the release binaries under "
        f"egress-policy: block but omits {', '.join(missing)}",
        file=sys.stderr,
    )

if failures:
    sys.exit(1)

if checked == 0:
    print("OK: no binary-building blocked-egress jobs found.")
else:
    print(f"OK: all {checked} binary-building blocked-egress job(s) allowlist the Go module hosts.")
PY
