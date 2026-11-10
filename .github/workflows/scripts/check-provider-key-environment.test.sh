#!/usr/bin/env bash
set -euo pipefail

# Tests check-provider-key-environment.py against the fixture workflows in
# testdata/provider-key-environment (TXE-6975).
#
# A pass-*.yml fixture must pass. A fail-*.yml fixture must fail, and its first line,
# `# expect: <text>`, names text the failure must contain, so each case pins the rule
# that catches it rather than any failure at all. The fixtures live below scripts/, so
# GitHub never runs them as workflows. No network, no dependencies beyond PyYAML.
#
# Usage: ./check-provider-key-environment.test.sh

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK="$SCRIPT_DIR/check-provider-key-environment.py"
FIXTURES="$SCRIPT_DIR/testdata/provider-key-environment"

failures=0
cases=0

for fixture in "$FIXTURES"/pass-*.yml "$FIXTURES"/fail-*.yml; do
  name="$(basename "$fixture")"
  cases=$((cases + 1))
  out="" status=0
  out="$(python3 "$CHECK" "$fixture" 2>&1)" || status=$?
  case "$name" in
    pass-*)
      if [[ $status -ne 0 ]]; then
        echo "FAIL $name: expected the check to pass, it failed:"; echo "$out" | sed 's/^/    /'
        failures=$((failures + 1))
      fi
      ;;
    fail-*)
      expect="$(sed -n '1s/^# expect: //p' "$fixture")"
      if [[ -z "$expect" ]]; then
        echo "FAIL $name: the first line must be '# expect: <text>'"
        failures=$((failures + 1))
      elif [[ $status -eq 0 ]]; then
        echo "FAIL $name: expected the check to fail with '$expect', it passed"
        failures=$((failures + 1))
      elif [[ "$out" != *"$expect"* ]]; then
        echo "FAIL $name: the failure does not say '$expect':"; echo "$out" | sed 's/^/    /'
        failures=$((failures + 1))
      fi
      ;;
  esac
done

# With no arguments the check discovers workflows itself: a .yaml workflow is a workflow
# too, so an ungated one there must fail exactly as it does as a .yml.
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/.github/workflows/scripts"
cp "$CHECK" "$WORK/.github/workflows/scripts/"
cp "$FIXTURES/pass-gated-job-env.yml" "$WORK/.github/workflows/gated.yml"
cp "$FIXTURES/fail-step-env.yml" "$WORK/.github/workflows/ungated.yaml"
cases=$((cases + 1))
out="" status=0
out="$(python3 "$WORK/.github/workflows/scripts/check-provider-key-environment.py" 2>&1)" || status=$?
if [[ $status -eq 0 || "$out" != *"ungated.yaml: job test-core: reads ANTHROPIC_API_KEY"* || "$out" != *"2 workflows checked"* ]]; then
  echo "FAIL default discovery: expected both workflows checked and ungated.yaml to fail:"; echo "$out" | sed 's/^/    /'
  failures=$((failures + 1))
fi

if [[ $cases -lt 2 ]]; then
  echo "FAIL: found $cases fixtures in $FIXTURES"
  exit 1
fi
if [[ $failures -ne 0 ]]; then
  echo "$failures of $cases provider-key environment cases failed."
  exit 1
fi
echo "All $cases provider-key environment cases passed."
