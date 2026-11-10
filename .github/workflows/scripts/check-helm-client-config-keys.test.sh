#!/usr/bin/env bash
set -euo pipefail

# Tests check-helm-client-config-keys.py against edited copies of the real chart files
# (TXE-7038): the repository as it stands must pass, and each way of leaving a client setting
# out of the chart must fail with the message that names it. No network, no helm.
#
# Usage: ./check-helm-client-config-keys.test.sh

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CHECK="$SCRIPT_DIR/check-helm-client-config-keys.py"
HELPERS="$REPO_ROOT/helm-charts/bifrost/templates/_helpers.tpl"
SCHEMA="$REPO_ROOT/transports/config.schema.json"
VALUES_SCHEMA="$REPO_ROOT/helm-charts/bifrost/values.schema.json"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

failures=0

# expect_fail <case name> <text the failure must contain> <check arguments...>
expect_fail() {
  local name=$1 expect=$2 out="" status=0
  shift 2
  out="$(python3 "$CHECK" "$@" 2>&1)" || status=$?
  if [[ $status -eq 0 ]]; then
    echo "FAIL $name: expected the check to fail with '$expect', it passed"
    failures=$((failures + 1))
  elif [[ "$out" != *"$expect"* ]]; then
    echo "FAIL $name: the failure does not contain '$expect':"; echo "$out" | sed 's/^/    /'
    failures=$((failures + 1))
  else
    echo "ok   $name"
  fi
}

if out="$(python3 "$CHECK" 2>&1)"; then
  echo "ok   the repository's chart passes"
else
  echo "FAIL the repository's chart: expected the check to pass:"; echo "$out" | sed 's/^/    /'
  failures=$((failures + 1))
fi

# A mapping removed from _helpers.tpl: the setting is in the schema and nothing renders it.
for key in disable_provider_job_accounting disable_passthrough_sse_heartbeat enforce_vk_object_ownership; do
  grep -v "set \$client \"$key\"" "$HELPERS" > "$WORK/helpers-without-$key.tpl"
  expect_fail "mapping of $key removed" \
    "client.$key is in config.schema.json but _helpers.tpl never sets it" \
    --helpers "$WORK/helpers-without-$key.tpl"
done

# A new client setting in the schema with no chart value.
python3 - "$SCHEMA" "$WORK/schema-new-setting.json" <<'PY'
import json, sys
schema = json.load(open(sys.argv[1]))
schema["properties"]["client"]["properties"]["a_new_client_switch"] = {"type": "boolean"}
json.dump(schema, open(sys.argv[2], "w"))
PY
expect_fail "new schema setting without a chart value" \
  "client.a_new_client_switch is in config.schema.json but _helpers.tpl never sets it" \
  --schema "$WORK/schema-new-setting.json"

# A mapping whose value the chart's values schema does not declare.
python3 - "$VALUES_SCHEMA" "$WORK/values-schema-undeclared.json" <<'PY'
import json, sys
schema = json.load(open(sys.argv[1]))
del schema["properties"]["bifrost"]["properties"]["client"]["properties"]["disableProviderJobAccounting"]
json.dump(schema, open(sys.argv[2], "w"))
PY
expect_fail "value missing from values.schema.json" \
  "rendered from bifrost.client.disableProviderJobAccounting, which values.schema.json does not declare" \
  --values-schema "$WORK/values-schema-undeclared.json"

# A value the chart's values schema accepts and no template reads.
python3 - "$VALUES_SCHEMA" "$WORK/values-schema-unread.json" <<'PY'
import json, sys
schema = json.load(open(sys.argv[1]))
schema["properties"]["bifrost"]["properties"]["client"]["properties"]["aValueNothingReads"] = {"type": "boolean"}
json.dump(schema, open(sys.argv[2], "w"))
PY
expect_fail "declared value no template reads" \
  "bifrost.client.aValueNothingReads is declared in values.schema.json but _helpers.tpl never reads it" \
  --values-schema "$WORK/values-schema-unread.json"

# A mapping of a key the config schema does not have.
{ cat "$HELPERS"; echo '{{- $_ := set $client "not_a_schema_key" .Values.bifrost.client.enableLogging }}'; } > "$WORK/helpers-extra-key.tpl"
expect_fail "mapping of a key the schema lacks" \
  "_helpers.tpl sets client.not_a_schema_key, which config.schema.json does not have" \
  --helpers "$WORK/helpers-extra-key.tpl"

# A NOT_IN_CHART entry that has since been mapped must be removed from the list.
{ cat "$HELPERS"; echo '{{- $_ := set $client "enforce_scim_auth" .Values.bifrost.client.enableLogging }}'; } > "$WORK/helpers-listed-key.tpl"
expect_fail "listed key that is now mapped" \
  "client.enforce_scim_auth is mapped in _helpers.tpl: remove it from NOT_IN_CHART" \
  --helpers "$WORK/helpers-listed-key.tpl"

if [[ $failures -ne 0 ]]; then
  echo "❌ $failures case(s) failed"
  exit 1
fi
echo "✅ check-helm-client-config-keys.py: every case behaves as expected"
