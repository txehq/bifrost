#!/usr/bin/env bash
set -euo pipefail

# helm-publish-oci.sh - publish the released chart to the repository owner's
# GHCR namespace, idempotently and fail-closed.
#
# On every attempt the registry is asked whether the requested version's
# manifest exists, through the same authenticated, retrying, fail-closed
# requests PR #6 introduced for image tags (ghcr-tags.sh):
#   - 200 with a manifest body: present. The chart is pulled and compared
#     byte for byte with the release asset; identical means nothing to do,
#     different is a conflict, and a content fetch that fails is a failure,
#     never a reason to push. Absence is decided here only, not from a failed
#     download: `helm pull` both resolves and downloads, and a missing blob
#     under an existing manifest must not be pushed over.
#   - 404 MANIFEST_UNKNOWN or NAME_UNKNOWN: absent only if the repository's
#     authenticated tag listing answers 200 for this very repository. GHCR
#     answers a manifest lookup the same way for a version that is absent and
#     for a repository the token cannot see (measured in TXE-4819), so without
#     that confirmation nothing is pushed. Then the canonical release asset is
#     pushed, pulled back and compared.
#   - a tag listing of 404 NAME_UNKNOWN: the repository is invisible to the
#     token or does not exist yet. Both fail, visibly. A package that does not
#     exist yet is bootstrapped deliberately, not by this script: an operator
#     with write:packages pushes the asset once with `helm push`, links the
#     package to this repository with read and write access, and re-runs.
#   - anything else (refused token, 401/403, 5xx, network, a 404 without a
#     registry error code, a listing for another name): failure. An
#     unanswered lookup is never absence.
# `helm push` overwrites an existing tag, which is why the lookup comes first
# and a confirmed-present version is never pushed.
#
# Why not ghcr_tag_states from ghcr-tags.sh directly: it accepts only
# owner/name repositories, while a chart lives at owner/<path>/<chart>. Its
# request, retry, JSON and error helpers are reused and its decision rule is
# kept; only the lookup itself is restated here for the nested name.
#
# Usage:
#   helm-publish-oci.sh --chart-name bifrost --chart-version 2.1.45 \
#     --canonical-chart "$RUNNER_TEMP/helm-release-asset/bifrost-2.1.45.tgz" \
#     --registry ghcr.io --owner "$GITHUB_REPOSITORY_OWNER" --repository-path helm-charts
#
# Publishes to oci://ghcr.io/<owner, lowercased>/<repository-path>/<chart-name>:<version>.
# Needs helm (already logged in to ghcr.io), curl, jq, GITHUB_TOKEN (a token
# that can read and write the package) and sha256sum or shasum.

CHART_NAME=""
CHART_VERSION=""
CANONICAL=""
REGISTRY="ghcr.io"
OWNER=""
REPOSITORY_PATH="helm-charts"

die() {
  echo "ERROR: helm-publish-oci: $*" >&2
  exit 1
}

log() {
  echo "helm-publish-oci: $*"
}

while [ $# -gt 0 ]; do
  case "$1" in
    --chart-name) CHART_NAME="${2:-}"; shift 2 ;;
    --chart-version) CHART_VERSION="${2:-}"; shift 2 ;;
    --canonical-chart) CANONICAL="${2:-}"; shift 2 ;;
    --registry) REGISTRY="${2:-}"; shift 2 ;;
    --owner) OWNER="${2:-}"; shift 2 ;;
    --repository-path) REPOSITORY_PATH="${2:-}"; shift 2 ;;
    *) die "unknown argument: $1" ;;
  esac
done
for required in CHART_NAME CHART_VERSION CANONICAL REGISTRY OWNER REPOSITORY_PATH; do
  if [ -z "${!required}" ]; then
    die "--$(echo "$required" | tr '[:upper:]_' '[:lower:]-') is required"
  fi
done
[ -f "$CANONICAL" ] || die "release asset not found: $CANONICAL"
[ "$REGISTRY" = "ghcr.io" ] || die "only ghcr.io is supported: the manifest lookup is GHCR-specific (got '$REGISTRY')"
for tool in helm curl jq; do
  command -v "$tool" >/dev/null 2>&1 || die "required tool missing: $tool"
done

# shellcheck source=ghcr-tags.sh
source "$(dirname "${BASH_SOURCE[0]}")/ghcr-tags.sh"

OWNER_LC="$(printf '%s' "$OWNER" | tr '[:upper:]' '[:lower:]')"
if ! printf '%s' "$OWNER_LC" | grep -E '^[a-z0-9][a-z0-9-]*$' >/dev/null; then
  die "--owner '$OWNER' is not a registry namespace"
fi
CHART_REPO="$OWNER_LC/$REPOSITORY_PATH/$CHART_NAME"
REPO_URL="oci://$REGISTRY/$OWNER_LC/$REPOSITORY_PATH"
CHART_URL="$REPO_URL/$CHART_NAME"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/helm-publish-oci.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

# Top-level keys only: helm sorts the output, so a nested key with the same
# name (a maintainer's `name:`) comes first and must not be taken for it.
chart_meta() {
  helm show chart "$1" | awk -v k="$2" '$0 ~ "^" k ": " { v = substr($0, length(k) + 3); gsub(/^"|"$/, "", v); print v; exit }'
}

# ghcr_chart_manifest_state <owner/path/name> <tag>: prints "present" or
# "absent" when the registry answered definitively; otherwise explains on
# stderr and returns 1. "absent" requires the repository's own tag listing to
# answer 200 under the same token. Runs in a subshell so its temporary
# directory is removed however it returns.
ghcr_chart_manifest_state() (
  repository="$1"
  tag="$2"
  if [[ ! "$repository" =~ ^[a-z0-9][a-z0-9._-]*(/[a-z0-9][a-z0-9._-]*)+$ ]]; then
    echo "::error::refusing malformed GHCR repository name: '${repository}'" >&2
    return 1
  fi
  if [[ ! "$tag" =~ ^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$ ]]; then
    echo "::error::refusing malformed GHCR tag: '${tag}'" >&2
    return 1
  fi
  if [ -z "${GITHUB_TOKEN:-}" ]; then
    echo "::error::GITHUB_TOKEN is not set, so ghcr.io/${repository} cannot be queried and the state of ${tag} is unknown." >&2
    return 1
  fi
  if [[ ! "$GITHUB_TOKEN" =~ ^[A-Za-z0-9_.-]+$ ]]; then
    echo "::error::GITHUB_TOKEN contains characters a GitHub token never has; refusing to use it." >&2
    return 1
  fi

  tmp=$(mktemp -d) || return 1
  trap 'rm -rf "$tmp"' EXIT

  ghcr_request "token exchange" "user = \"token:${GITHUB_TOKEN}\"" "$tmp/token" \
    "https://ghcr.io/token?service=ghcr.io&scope=repository:${repository}:pull"
  if [ "$GHCR_STATUS" != "200" ]; then
    ghcr_fail "token exchange for ${repository}" "$tmp/token"
    return 1
  fi
  bearer=$(jq -e -r -s '
      (if length == 1 then .[0] | (.token // .access_token) else null end)
      | if type == "string" and length > 0 and gsub("[A-Za-z0-9._~+/=-]"; "") == ""
        then . else error("no usable token") end
    ' "$tmp/token" 2>/dev/null) || bearer=""
  if [[ ! "$bearer" =~ ^[A-Za-z0-9._~+/=-]+$ ]]; then
    echo "::error::GHCR token exchange for ${repository} answered HTTP 200 with no usable token." >&2
    return 1
  fi

  auth="header = \"Authorization: Bearer ${bearer}\""
  ghcr_request "manifest lookup" "$auth" "$tmp/manifest" \
    "https://ghcr.io/v2/${repository}/manifests/${tag}" -H "Accept: ${GHCR_ACCEPT}"
  case "$GHCR_STATUS" in
    200)
      if ! ghcr_json_is "$tmp/manifest" '.schemaVersion == 2'; then
        echo "::error::GHCR manifest lookup for ${repository}:${tag} answered HTTP 200 without a manifest body; refusing to guess." >&2
        return 1
      fi
      echo present
      return 0 ;;
    404)
      if ! ghcr_json_is "$tmp/manifest" '.errors[0].code == "MANIFEST_UNKNOWN" or .errors[0].code == "NAME_UNKNOWN"'; then
        ghcr_fail "manifest lookup for ${repository}:${tag}" "$tmp/manifest"
        return 1
      fi ;;
    *)
      ghcr_fail "manifest lookup for ${repository}:${tag}" "$tmp/manifest"
      return 1 ;;
  esac

  # The manifest is not there, or the whole repository is invisible to this
  # token: only its tag listing tells those apart.
  ghcr_request "tag listing" "$auth" "$tmp/tags" "https://ghcr.io/v2/${repository}/tags/list?n=1"
  case "$GHCR_STATUS" in
    200)
      # shellcheck disable=SC2016 # $repo is a jq variable, bound with --arg
      if ! ghcr_json_is "$tmp/tags" '.name == $repo' --arg repo "$repository"; then
        echo "::error::GHCR tag listing for ${repository} answered HTTP 200 for a different or malformed repository name; refusing to read the manifest 404 as absence." >&2
        return 1
      fi
      echo absent ;;
    404)
      if ghcr_json_is "$tmp/tags" '.errors[0].code == "NAME_UNKNOWN"'; then
        echo "::error::ghcr.io/${repository} is not visible to the job token (tag listing: HTTP 404 NAME_UNKNOWN), so the manifest 404 for ${tag} cannot be read as absence. If the package exists, grant this repository read and write access to it. If it does not exist yet, bootstrap it deliberately: an operator with write:packages runs 'helm push <release asset> oci://ghcr.io/${repository%/*}' once, links the package to this repository with read and write access, then re-runs this job." >&2
        return 1
      fi
      ghcr_fail "tag listing for ${repository}" "$tmp/tags"
      return 1 ;;
    *)
      ghcr_fail "tag listing for ${repository}" "$tmp/tags"
      return 1 ;;
  esac
)

# pull_chart DIR: fetch the published chart into DIR; stderr lands in DIR.err.
pull_chart() {
  mkdir -p "$1"
  helm pull "$CHART_URL" --version "$CHART_VERSION" --destination "$1" >"$1.out" 2>"$1.err" &&
    [ -f "$1/$CHART_NAME-$CHART_VERSION.tgz" ]
}

# --- 1. The asset is the version it claims to be ---------------------------

asset_name="$(chart_meta "$CANONICAL" name)" || die "cannot read chart metadata from $CANONICAL"
asset_version="$(chart_meta "$CANONICAL" version)" || die "cannot read chart metadata from $CANONICAL"
if [ "$asset_name" != "$CHART_NAME" ] || [ "$asset_version" != "$CHART_VERSION" ]; then
  die "release asset $CANONICAL is $asset_name $asset_version, not $CHART_NAME $CHART_VERSION"
fi
CANON_DIGEST="$(sha256_of "$CANONICAL")"
log "release asset $CANONICAL: $CHART_NAME $CHART_VERSION, sha256 $CANON_DIGEST"

# --- 2. Ask the registry for the requested version's manifest --------------

STATE="$(ghcr_chart_manifest_state "$CHART_REPO" "$CHART_VERSION")" ||
  die "cannot determine whether $CHART_URL:$CHART_VERSION is published; not publishing"
case "$STATE" in
  present)
    log "$CHART_URL:$CHART_VERSION is published; comparing its content with the release asset"
    if ! pull_chart "$WORK/existing"; then
      cat "$WORK/existing.err" >&2
      die "$CHART_URL:$CHART_VERSION is published but its content cannot be fetched; not publishing over it"
    fi
    existing_digest="$(sha256_of "$WORK/existing/$CHART_NAME-$CHART_VERSION.tgz")"
    if [ "$existing_digest" = "$CANON_DIGEST" ]; then
      log "$CHART_URL:$CHART_VERSION already holds the release asset's bytes; leaving it unchanged"
      exit 0
    fi
    die "conflict: $CHART_URL:$CHART_VERSION is published with sha256 $existing_digest but the release asset has $CANON_DIGEST; refusing to overwrite a published chart"
    ;;
  absent)
    log "$CHART_URL:$CHART_VERSION is not published yet"
    ;;
  *)
    die "unexpected registry state '$STATE' for $CHART_URL:$CHART_VERSION"
    ;;
esac

# --- 3. Publish the canonical asset and read it back -----------------------

if ! helm push "$CANONICAL" "$REPO_URL" >"$WORK/push.out" 2>"$WORK/push.err"; then
  cat "$WORK/push.err" >&2
  die "helm push of $CHART_NAME $CHART_VERSION to $REPO_URL failed"
fi
cat "$WORK/push.out"

if ! pull_chart "$WORK/verify"; then
  cat "$WORK/verify.err" >&2
  die "pushed $CHART_NAME $CHART_VERSION to $REPO_URL but cannot read it back; publication unverified"
fi
verify_digest="$(sha256_of "$WORK/verify/$CHART_NAME-$CHART_VERSION.tgz")"
if [ "$verify_digest" != "$CANON_DIGEST" ]; then
  die "pushed $CHART_NAME $CHART_VERSION to $REPO_URL but the registry serves sha256 $verify_digest, not the asset's $CANON_DIGEST"
fi
log "published $CHART_URL:$CHART_VERSION (sha256 $CANON_DIGEST) and verified it reads back identically"
