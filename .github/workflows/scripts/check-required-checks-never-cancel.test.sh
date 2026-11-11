#!/usr/bin/env bash
set -euo pipefail

# Tests check-required-checks-never-cancel.py against the fixture workflows in
# testdata/required-checks-never-cancel (TXE-7104).
#
# A pass-*.yml fixture must pass. A fail-*.yml fixture must fail, and its first line,
# `# expect: <text>`, names text the failure must contain, so each case pins the rule
# that catches it rather than any failure at all. The fixtures live below scripts/, so
# GitHub never runs them as workflows. No network, no dependencies beyond PyYAML.
#
# Usage: ./check-required-checks-never-cancel.test.sh

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK="$SCRIPT_DIR/check-required-checks-never-cancel.py"
FIXTURES="$SCRIPT_DIR/testdata/required-checks-never-cancel"

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

# With no arguments the check discovers the workflows itself, and a required check with no
# job (a renamed job) fails: here only codex-review exists.
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/.github/workflows/scripts"
cp "$CHECK" "$WORK/.github/workflows/scripts/"
cp "$FIXTURES/pass-no-concurrency.yml" "$WORK/.github/workflows/codex-review.yaml"
cases=$((cases + 1))
out="" status=0
out="$(python3 "$WORK/.github/workflows/scripts/check-required-checks-never-cancel.py" 2>&1)" || status=$?
if [[ $status -eq 0 || "$out" != *"required check 'API collection offline suites' has no job"* || "$out" == *"'codex-review' has no job"* ]]; then
  echo "FAIL default discovery: expected only the missing offline-suites job to fail:"; echo "$out" | sed 's/^/    /'
  failures=$((failures + 1))
fi

if [[ $cases -lt 2 ]]; then
  echo "FAIL: found $cases fixtures in $FIXTURES"
  exit 1
fi
if [[ $failures -ne 0 ]]; then
  echo "$failures of $cases required-check cancellation cases failed."
  exit 1
fi
echo "All $cases required-check cancellation cases passed."
