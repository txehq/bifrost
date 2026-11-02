#!/usr/bin/env bash
set -euo pipefail

# The image the chart renders: by default, this fork's GHCR image at the tag the
# release pipeline publishes for the chart's appVersion, on both workloads, with
# explicit values winning. Runs helm template only; nothing is contacted.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
CHART="$ROOT/helm-charts/bifrost"
FAILURES=0

fail() { echo "FAIL: $*" >&2; FAILURES=$((FAILURES + 1)); }

meta() { awk -v k="$1" '$0 ~ "^" k ": " { v = substr($0, length(k) + 3); gsub(/^"|"$/, "", v); print v; exit }' "$CHART/Chart.yaml"; }

APP_VERSION="$(meta appVersion)"
TRANSPORT_VERSION="$(tr -d '[:space:]' <"$ROOT/transports/version")"
[ "$APP_VERSION" = "$TRANSPORT_VERSION" ] || fail "Chart appVersion '$APP_VERSION' is not transports/version '$TRANSPORT_VERSION'"
case "$APP_VERSION" in
  v*) fail "Chart appVersion '$APP_VERSION' must not carry the v prefix" ;;
esac
[ "$(awk '/^sources:/ { getline; print $2 }' "$CHART/Chart.yaml")" = "https://github.com/txehq/bifrost" ] ||
  fail "the first Chart source must be this repository: helm publishes it as org.opencontainers.image.source"

DEFAULT="ghcr.io/txehq/bifrost:v$APP_VERSION"

# check LABEL KIND EXPECTED [helm args]: exactly the expected Bifrost image on KIND.
check() {
  local label=$1 kind=$2 want=$3 out images
  shift 3
  if ! out="$(helm template bifrost "$CHART" "$@" 2>&1)"; then
    fail "$label: helm template failed: $(printf '%s' "$out" | head -n 3)"
    return
  fi
  printf '%s\n' "$out" | grep -q "^kind: $kind\$" || fail "$label: renders no $kind"
  images="$(printf '%s\n' "$out" | sed -n 's/^ *image: "\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' | sort -u)"
  [ "$images" = "$want" ] || fail "$label: images are '$images', not '$want'"
}

POSTGRES=(--set storage.mode=postgres --set postgresql.enabled=false
  --set postgresql.external.enabled=true --set postgresql.external.host=db.example.com
  --set postgresql.external.password=fixture)
SQLITE=(--set storage.mode=sqlite --set storage.persistence.enabled=true)

check "default Deployment" Deployment "$DEFAULT" "${POSTGRES[@]}"
check "default StatefulSet" StatefulSet "$DEFAULT" "${SQLITE[@]}"
check "tag override" Deployment "ghcr.io/txehq/bifrost:v2.3.2" "${POSTGRES[@]}" --set image.tag=v2.3.2
check "ubi9 tag override" StatefulSet "ghcr.io/txehq/bifrost:v$APP_VERSION-ubi9" "${SQLITE[@]}" --set "image.tag=v$APP_VERSION-ubi9"
check "repository override" StatefulSet "registry.example.com/team/bifrost:v$APP_VERSION" "${SQLITE[@]}" --set image.repository=registry.example.com/team/bifrost
check "both overrides" Deployment "registry.example.com/team/bifrost:custom" "${POSTGRES[@]}" --set image.repository=registry.example.com/team/bifrost --set image.tag=custom

secrets="$(helm template bifrost "$CHART" "${SQLITE[@]}" --set 'imagePullSecrets[0].name=ghcr-pull')"
printf '%s\n' "$secrets" | grep -q 'name: ghcr-pull' || fail "imagePullSecrets do not reach the workload"

if [ "$FAILURES" -ne 0 ]; then
  echo "$FAILURES chart image check(s) failed" >&2
  exit 1
fi
echo "chart image checks passed: $DEFAULT"
