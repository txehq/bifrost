#!/usr/bin/env bash
set -euo pipefail

# Tests the provider-harness rule of check-egress-allowlist.sh against small
# collections and provider profiles of its own (TXE-6978, TXE-6981).
#
# The rule reads every external host in the harness collection as a host the runner
# dials, except in text that is documentation by construction: Postman description
# fields and whole-line script comments. A vendor documentation link in a folder
# description (platform.kimi.ai, after the Moonshot folder) failed the check on every
# pull request until this exception existed. These cases pin both halves: the
# documentation link passes, and a host in anything newman sends or runs still fails.
#
# Each case builds a throwaway repository root holding the script, one workflow with a
# harness job under egress-policy: block, a provider config and a collection, and runs
# the script there. No network, no dependencies beyond the PyYAML the check needs.
#
# Usage: ./check-egress-allowlist.test.sh

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

failures=0

# run_case <name> <pass|fail> <text expected in the failure, or ""> <collection json>
#          [provider config json] [provider default hosts json]
run_case() {
  local name="$1" want="$2" host="$3" collection="$4"
  local config="${5:-{\"providers\":{\}\}}"
  local default_hosts="${6:-{\}}"
  local root="$WORK/$name"
  mkdir -p "$root/.github/workflows/scripts" "$root/tests/e2e/api/collections" \
    "$root/tests/integrations/python" "$root/core/providers"
  cp "$SCRIPT_DIR/check-egress-allowlist.sh" "$root/.github/workflows/scripts/"
  printf '%s\n' "$collection" > "$root/tests/e2e/api/collections/provider-harness.json"
  printf '%s\n' "$config" > "$root/tests/integrations/python/config.json"
  printf '%s\n' "$default_hosts" > "$root/.github/workflows/provider-default-hosts.json"
  cat > "$root/.github/workflows/harness.yml" <<'YAML'
jobs:
  test-core:
    runs-on: ubuntu-24.04
    steps:
      - uses: step-security/harden-runner@v2
        with:
          egress-policy: block
          allowed-endpoints: >
            api.allowed.test:443
      - run: ./.github/workflows/scripts/test-core.sh
YAML

  local out status=0
  out="$(bash "$root/.github/workflows/scripts/check-egress-allowlist.sh" 2>&1)" || status=$?
  if [[ "$want" == "pass" && $status -ne 0 ]]; then
    echo "FAIL $name: expected the check to pass, it failed:"; echo "$out" | sed 's/^/    /'
    failures=$((failures + 1)); return
  fi
  if [[ "$want" == "fail" ]]; then
    if [[ $status -eq 0 ]]; then
      echo "FAIL $name: expected the check to fail on $host, it passed"
      failures=$((failures + 1)); return
    fi
    if ! grep -q -- "$host" <<<"$out"; then
      echo "FAIL $name: the check failed, but not on $host:"; echo "$out" | sed 's/^/    /'
      failures=$((failures + 1)); return
    fi
  fi
  echo "ok   $name"
}

# A collection with one request to an allowlisted host; $1 is spliced into the folder
# object and $2 into the request's test script lines.
collection() {
  cat <<JSON
{
  "info": { "name": "fixture", "description": "Top-level notes: https://docs.collection.test/intro" },
  "item": [
    {
      "name": "folder",
      $1
      "item": [
        {
          "name": "case",
          "event": [ { "listen": "test", "script": { "exec": [ $2 "pm.test('ok', function () {});" ] } } ],
          "request": {
            "method": "POST",
            "description": "See https://docs.request.test/reference",
            "header": [ { "key": "X-Doc", "value": "1", "description": "https://docs.header.test/x-doc" } ],
            "body": { "mode": "raw", "raw": "{\"model\": \"m\"}" },
            "url": { "raw": "https://api.allowed.test/v1/chat", "host": ["api.allowed.test"], "path": ["v1", "chat"] }
          }
        }
      ]
    }
  ]
}
JSON
}

# Documentation: a link in a description at any level, or in a whole-line comment.
run_case "docs-link-in-folder-description" pass "" \
  "$(collection '"description": "Kimi serves Responses (https://platform.kimi.ai/docs/api/responses.md).",' '')"
run_case "docs-link-in-script-comment" pass "" \
  "$(collection '' '"// see https://learn.vendor.test/files-api", "   // and https://learn.vendor.test/indented",')"

# Anything newman sends or runs still counts.
run_case "host-in-request-url" fail "api.unlisted.test" \
  "$(collection '' '' | sed 's#https://api.allowed.test/v1/chat#https://api.unlisted.test/v1/chat#')"
run_case "host-in-request-body" fail "files.unlisted.test" \
  "$(collection '' '' | sed 's#{\\"model\\": \\"m\\"}#{\\"image_url\\": \\"https://files.unlisted.test/a.png\\"}#')"
run_case "host-in-script-code" fail "callback.unlisted.test" \
  "$(collection '' '"pm.sendRequest({ url: \"https://callback.unlisted.test/hook\" }, function () {});",')"
# One exec entry can hold a whole multi-line script. Only its comment lines are
# documentation; a script that begins with a comment keeps the statements after it.
run_case "host-in-code-after-a-comment-in-a-multiline-entry" fail "multiline.unlisted.test" \
  "$(collection '' '"// initialize\npm.sendRequest(\"https://multiline.unlisted.test/hook\", function () {});",')"
run_case "docs-link-in-a-comment-line-of-a-multiline-entry" pass "" \
  "$(collection '' '"var a = 1;\n  // see https://learn.vendor.test/multiline\nvar b = 2;",')"
run_case "host-in-trailing-comment-of-a-code-line" fail "trailing.unlisted.test" \
  "$(collection '' '"var x = 1; // https://trailing.unlisted.test/still-counted",')"
run_case "host-in-a-key-that-only-resembles-description" fail "tool.unlisted.test" \
  "$(collection '"description_url": "https://tool.unlisted.test/spec",' '')"

# A provider in the harness profile without a base_url dials the default its Go
# constructor sets, which is in neither the collection nor the config. The check reads
# it from provider-default-hosts.json (TXE-6981).
DEFAULT_HOSTS='{"acme": "api.acme.test", "allowed": "api.allowed.test", "nodefault": ""}'
run_case "profile-provider-default-host-not-allowlisted" fail "omits api.acme.test" "$(collection '' '')" \
  '{"providers": {"acme": {"keys": []}}}' "$DEFAULT_HOSTS"
run_case "profile-provider-default-host-allowlisted" pass "" "$(collection '' '')" \
  '{"providers": {"allowed": {"keys": []}}}' "$DEFAULT_HOSTS"
run_case "profile-provider-without-a-default-host" pass "" "$(collection '' '')" \
  '{"providers": {"nodefault": {"keys": []}}}' "$DEFAULT_HOSTS"
run_case "custom-provider-dials-its-base-providers-host" fail "omits api.acme.test" "$(collection '' '')" \
  '{"providers": {"acme_custom": {"custom_provider_config": {"base_provider_type": "acme"}}}}' "$DEFAULT_HOSTS"
run_case "profile-provider-with-its-own-base-url" pass "" "$(collection '' '')" \
  '{"providers": {"acme": {"network_config": {"base_url": "https://api.allowed.test/v1"}}}}' "$DEFAULT_HOSTS"
# A provider the list does not know means the list is stale; that is an error, not a skip.
run_case "profile-provider-missing-from-the-list" fail "'newcomer' (base provider 'newcomer') has no entry" "$(collection '' '')" \
  '{"providers": {"newcomer": {"keys": []}}}' "$DEFAULT_HOSTS"

if [[ $failures -gt 0 ]]; then
  echo ""
  echo "$failures case(s) failed."
  exit 1
fi
echo ""
echo "All egress allowlist cases passed."
