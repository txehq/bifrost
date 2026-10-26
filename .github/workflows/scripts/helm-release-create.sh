#!/usr/bin/env bash
set -euo pipefail

# Creates the GitHub release that carries a packaged Helm chart, the step of
# helm-release.yml that every later publication (Pages index, OCI) reads the
# canonical tarball from.
#
# The one decision made here is the release's prerelease flag. A chart version
# with a hyphen (2.1.46-sync.3) is a prerelease, exactly as the transport
# release treats its own versions (release-bifrost-http-finalize.sh): the
# GitHub release is created with --prerelease, so it never becomes the
# repository's "latest" release, and Helm clients skip it unless asked for
# prereleases (--devel). A plain MAJOR.MINOR.PATCH is created as before, with
# no flag. Before this script the workflow inlined `gh release create` with no
# flag at all, and every chart release took "latest".
#
# The version is validated before it reaches gh: the same shape the release
# pipeline's version guard accepts (MAJOR.MINOR.PATCH with an optional -suffix,
# never an empty one), and the package must be the chart it names, read from
# the Chart.yaml inside the tarball. Nothing here touches the published index
# or a registry; helm-publish-index.sh and helm-publish-oci.sh keep their own
# rules.
#
# Usage:
#   helm-release-create.sh --chart-name bifrost --chart-version 2.1.46-sync.3 \
#     --package helm-charts/bifrost-2.1.46-sync.3.tgz --target <sha> \
#     --repository owner/name
#
# Needs GH_TOKEN or GITHUB_TOKEN. Tests: helm-release-create.test.sh.

CHART_NAME=""
CHART_VERSION=""
PACKAGE=""
TARGET=""
REPOSITORY=""

die() { echo "helm-release-create: $*" >&2; exit 1; }
log() { echo "helm-release-create: $*"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --chart-name) CHART_NAME="${2:-}"; shift 2 ;;
    --chart-version) CHART_VERSION="${2:-}"; shift 2 ;;
    --package) PACKAGE="${2:-}"; shift 2 ;;
    --target) TARGET="${2:-}"; shift 2 ;;
    --repository) REPOSITORY="${2:-}"; shift 2 ;;
    *) die "unknown argument: $1" ;;
  esac
done

for required in CHART_NAME CHART_VERSION PACKAGE TARGET REPOSITORY; do
  if [ -z "${!required}" ]; then
    die "--$(printf '%s' "$required" | tr '[:upper:]_' '[:lower:]-') is required"
  fi
done

# Same shape as validate_version in detect-all-changes.sh: three numeric
# components, an optional suffix only after PATCH and only when introduced by
# `-` or `+`, never empty. The value is interpolated into a tag name and the
# release title, so a shell metacharacter here would be a command, not a version.
if [[ ! "$CHART_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][A-Za-z0-9.+_-]+)?$ ]]; then
  die "refusing malformed chart version '$CHART_VERSION' (expected MAJOR.MINOR.PATCH with an optional -suffix)"
fi
if [[ ! "$TARGET" =~ ^[0-9a-f]{7,40}$ ]]; then
  die "refusing malformed target commit '$TARGET'"
fi
if [[ ! "$REPOSITORY" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]]; then
  die "refusing malformed repository '$REPOSITORY' (expected owner/name)"
fi
if [ -z "${GH_TOKEN:-}" ] && [ -z "${GITHUB_TOKEN:-}" ]; then
  die "GH_TOKEN or GITHUB_TOKEN is not set"
fi

[ -f "$PACKAGE" ] || die "package $PACKAGE does not exist"
if [ "$(basename "$PACKAGE")" != "$CHART_NAME-$CHART_VERSION.tgz" ]; then
  die "package $PACKAGE is not named $CHART_NAME-$CHART_VERSION.tgz"
fi

# chart_meta KEY -> the top-level Chart.yaml scalar inside the package.
# `helm package` re-marshals Chart.yaml with its keys sorted, so a nested key
# such as maintainers[].name ("  name: Bifrost Team") lands before the
# top-level name; only a line that starts in column 0 is the chart's own key.
# The whole value is taken, with surrounding quotes removed.
chart_meta() {
  tar -xzOf "$PACKAGE" "$CHART_NAME/Chart.yaml" 2>/dev/null | awk -v key="$1" '
    /^[^[:space:]#]/ && $1 == key ":" {
      sub(/^[^:]*:[[:space:]]*/, "")
      sub(/[[:space:]]+$/, "")
      gsub(/^["'"'"']|["'"'"']$/, "")
      print
      exit
    }'
}
pkg_name="$(chart_meta name)"
pkg_version="$(chart_meta version)"
if [ "$pkg_name" != "$CHART_NAME" ] || [ "$pkg_version" != "$CHART_VERSION" ]; then
  die "package $PACKAGE is chart '$pkg_name' version '$pkg_version', not $CHART_NAME $CHART_VERSION"
fi

TAG="helm-chart-v$CHART_VERSION"
PRERELEASE_FLAG=""
if [[ "$CHART_VERSION" == *-* ]]; then
  PRERELEASE_FLAG="--prerelease"
  log "$CHART_VERSION is a prerelease: $TAG is created with --prerelease and never takes the latest release"
else
  log "$CHART_VERSION is a stable version: $TAG is created as a regular release"
fi

# shellcheck disable=SC2086 # PRERELEASE_FLAG is empty or one flag, never user input
gh release create "$TAG" "$PACKAGE" \
  --repo "$REPOSITORY" \
  --target "$TARGET" \
  --title "Helm Chart v$CHART_VERSION" \
  --notes "Helm chart release for Bifrost v$CHART_VERSION" \
  $PRERELEASE_FLAG

log "created $TAG from $PACKAGE at $TARGET"
