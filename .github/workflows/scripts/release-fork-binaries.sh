#!/usr/bin/env bash
set -euo pipefail

# Publishes a CLI to the fork's own GitHub release, and verifies what was published.
#
# Usage: release-fork-binaries.sh publish|verify <cli|migration> <version>
#
# The R2 path in release-cli.sh is upstream's: its bucket and download host are not the
# fork's, and the fork has no R2 credentials. A release here is immutable: publish
# refuses a version whose tag or published release exists, and never moves either.
#
# publish builds from the pushed commit, uploads to a draft, reads the assets back from
# the draft, and only then publishes. The tag does not exist until that last step, so
# a failed run leaves no tag and a rerun of the same commit replaces its own draft.
# Every release is --latest=false: GH latest stays the transport's.

FORK_REPO="txehq/bifrost"

fail() {
  echo "❌ $*" >&2
  exit 1
}

[[ $# -eq 3 ]] || fail "Usage: $0 publish|verify <cli|migration> <version>"
ACTION="$1"
COMPONENT="$2"
VERSION="$3"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd -P)"

case "$COMPONENT" in
  cli)
    TAG_PREFIX="cli"
    VERSION_FILE="cli/version"
    CHANGELOG_FILE="cli/changelog.md"
    BUILD_SCRIPT="build-cli-executables.sh"
    BINARY="bifrost"
    ASSET_PREFIX="bifrost-cli"
    TITLE="Bifrost CLI"
    ;;
  migration)
    TAG_PREFIX="bifrost-migration-cli"
    VERSION_FILE="scripts/bifrost-migration-cli/version"
    CHANGELOG_FILE="scripts/bifrost-migration-cli/changelog.md"
    BUILD_SCRIPT="build-bifrost-migration-cli-executables.sh"
    BINARY="bifrost-migration-cli"
    ASSET_PREFIX="bifrost-migration-cli"
    TITLE="Bifrost Migration CLI"
    ;;
  *) fail "unknown component: $COMPONENT" ;;
esac

[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]] || fail "invalid version: $VERSION"
TAG="$TAG_PREFIX/v$VERSION"

PLATFORMS=(darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64)
EXPECTED_ASSETS=$((${#PLATFORMS[@]} + 1))

[[ "${GITHUB_REPOSITORY:-}" == "$FORK_REPO" ]] || fail "refusing to release from ${GITHUB_REPOSITORY:-unset}: only $FORK_REPO"
[[ "${GITHUB_SHA:-}" =~ ^[0-9a-f]{40}$ ]] || fail "GITHUB_SHA is not a commit: ${GITHUB_SHA:-unset}"

cd "$REPO_ROOT"

# Prints "<release id> <draft> <target commitish>", or nothing when absent.
find_release() {
  gh api "repos/$FORK_REPO/releases" --paginate \
    --jq ".[] | select(.tag_name == \"$TAG\") | \"\(.id) \(.draft) \(.target_commitish)\""
}

# Prints the tag's commit, or nothing when absent. Fails on any other git error.
remote_tag_commit() {
  local refs
  refs="$(git ls-remote --tags origin "refs/tags/$TAG" "refs/tags/$TAG^{}")" || return 1
  awk 'END { print $1 }' <<<"$refs"
}

latest_release() {
  gh api "repos/$FORK_REPO/releases/latest" --jq .tag_name
}

asset_name() {
  local goos="$1" goarch="$2" name
  name="$ASSET_PREFIX-v$VERSION-$goos-$goarch"
  [[ "$goos" == "windows" ]] && name+=".exe"
  echo "$name"
}

host_asset() {
  local arch
  case "$(uname -m)" in
    x86_64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) fail "no asset for $(uname -m)" ;;
  esac
  asset_name linux "$arch"
}

# The binary must report this version and commit and, for the CLI, refuse to update
# from upstream. A fork build refuses before any network call, so this needs no egress.
check_binary() {
  local binary="$1" output
  chmod +x "$binary"
  output="$("$binary" version)"
  echo "$output"
  [[ "$output" == *"v$VERSION"* && "$output" == *"$GITHUB_SHA"* ]] || fail "the binary does not report v$VERSION at $GITHUB_SHA"
  [[ "$COMPONENT" == "cli" ]] || return 0
  if output="$(BIFROST_NO_UPDATE_CHECK=1 "$binary" update 2>&1)"; then
    fail "the binary's update command succeeded: $output"
  fi
  [[ "$output" == *"github.com/$FORK_REPO/releases"* ]] || fail "the binary is not a fork build: $output"
}

check_source() {
  [[ "$(git rev-parse HEAD)" == "$GITHUB_SHA" ]] || fail "checkout $(git rev-parse HEAD) is not the pushed commit $GITHUB_SHA"
  [[ -z "$(git status --porcelain=v1 --untracked-files=all)" ]] || fail "the checkout has local changes"
  git fetch --quiet --no-tags origin +refs/heads/main:refs/remotes/origin/main
  git merge-base --is-ancestor "$GITHUB_SHA" refs/remotes/origin/main || fail "$GITHUB_SHA is not on main"
  local file_version
  file_version="$(tr -d '[:space:]' <"$VERSION_FILE")"
  [[ "$file_version" == "$VERSION" ]] || fail "$VERSION_FILE says $file_version, not $VERSION"
}

# Writes the changelog body to stdout. Empty is a refusal.
changelog_body() {
  local body
  body="$(sed '/<!--/,/-->/d' "$CHANGELOG_FILE")"
  [[ -n "$(tr -d '[:space:]' <<<"$body")" ]] || fail "$CHANGELOG_FILE is empty"
  printf '%s\n' "$body"
}

publish() {
  check_source
  local changelog
  changelog="$(changelog_body)"

  local tag_commit
  tag_commit="$(remote_tag_commit)" || fail "could not read tags from origin"
  [[ -z "$tag_commit" ]] || fail "tag $TAG already exists at $tag_commit: versions are immutable"

  local release id draft target
  release="$(find_release)"
  if [[ -n "$release" ]]; then
    read -r id draft target <<<"$release"
    [[ "$draft" == "true" ]] || fail "release $TAG is already published: versions are immutable"
    [[ "$target" == "$GITHUB_SHA" ]] || fail "draft $TAG targets $target, not $GITHUB_SHA"
    echo "🧹 Replacing this commit's unfinished draft $id"
    gh api -X DELETE "repos/$FORK_REPO/releases/$id" >/dev/null
  fi

  local latest_before
  latest_before="$(latest_release)"

  echo "🔨 Building $TITLE v$VERSION from $GITHUB_SHA"
  CLI_RELEASE_CHANNEL=txehq bash "$SCRIPT_DIR/$BUILD_SCRIPT" "$VERSION"

  local work stage back notes platform goos goarch name ext
  work="$(mktemp -d)"
  stage="$work/stage"
  back="$work/back"
  notes="$work/notes.md"
  mkdir -p "$stage" "$back"
  for platform in "${PLATFORMS[@]}"; do
    IFS='/' read -r goos goarch <<<"$platform"
    ext=""
    [[ "$goos" == "windows" ]] && ext=".exe"
    name="$(asset_name "$goos" "$goarch")"
    cp "dist/$goos/$goarch/$BINARY$ext" "$stage/$name"
  done
  check_binary "$stage/$(host_asset)"
  (cd "$stage" && sha256sum -- * >"$work/SHA256SUMS" && mv "$work/SHA256SUMS" .)

  local prerelease=()
  [[ "$VERSION" == *-* ]] && prerelease=(--prerelease)

  {
    printf '## %s v%s\n\n%s\n\n' "$TITLE" "$VERSION" "$changelog"
    printf '### Installation\n\n'
    printf 'Download the asset for your platform, check it against `SHA256SUMS`, and put it on your `PATH` as `%s`.\n\n' "$BINARY"
    printf 'This is a fork build from %s. The `@maximhq` npm packages and upstream download host do not serve it.\n' "$GITHUB_SHA"
  } >"$notes"

  echo "📦 Creating draft $TAG at $GITHUB_SHA"
  gh release create "$TAG" "$stage"/* \
    --repo "$FORK_REPO" \
    --draft \
    --target "$GITHUB_SHA" \
    --latest=false \
    --title "$TITLE v$VERSION" \
    --notes-file "$notes" \
    "${prerelease[@]+"${prerelease[@]}"}"

  echo "🔎 Reading the draft's assets back"
  gh release download "$TAG" --repo "$FORK_REPO" --dir "$back"
  [[ "$(find "$back" -type f | wc -l | tr -d ' ')" == "$EXPECTED_ASSETS" ]] || fail "the draft does not hold $EXPECTED_ASSETS assets"
  diff -r "$stage" "$back" >/dev/null || fail "the draft's assets differ from the build"

  gh release edit "$TAG" --repo "$FORK_REPO" --draft=false --latest=false "${prerelease[@]+"${prerelease[@]}"}"

  tag_commit="$(remote_tag_commit)" || fail "could not read tags from origin"
  [[ "$tag_commit" == "$GITHUB_SHA" ]] || fail "tag $TAG is at ${tag_commit:-nothing}, not $GITHUB_SHA"
  [[ "$(latest_release)" == "$latest_before" ]] || fail "GH latest moved from $latest_before"

  echo "✅ Published $TAG at $GITHUB_SHA"
  if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
    echo "success=true" >>"$GITHUB_OUTPUT"
  fi
}

verify() {
  local release id draft target
  release="$(find_release)"
  [[ -n "$release" ]] || fail "no release $TAG"
  read -r id draft target <<<"$release"
  [[ "$draft" == "false" ]] || fail "release $TAG is still a draft"
  [[ "$target" == "$GITHUB_SHA" ]] || fail "release $TAG targets $target, not $GITHUB_SHA"

  local tag_commit
  tag_commit="$(remote_tag_commit)" || fail "could not read tags from origin"
  [[ "$tag_commit" == "$GITHUB_SHA" ]] || fail "tag $TAG is at ${tag_commit:-nothing}, not $GITHUB_SHA"

  local latest
  latest="$(latest_release)"
  [[ "$latest" != "$TAG" ]] || fail "$TAG took GH latest"

  local work
  work="$(mktemp -d)"
  gh release download "$TAG" --repo "$FORK_REPO" --dir "$work"
  [[ "$(find "$work" -type f | wc -l | tr -d ' ')" == "$EXPECTED_ASSETS" ]] || fail "release $TAG does not hold $EXPECTED_ASSETS assets"
  (cd "$work" && sha256sum -c SHA256SUMS)

  check_binary "$work/$(host_asset)"

  echo "✅ Verified $TAG at $GITHUB_SHA; GH latest is $latest"
}

case "$ACTION" in
  publish) publish ;;
  verify) verify ;;
  *) fail "unknown action: $ACTION" ;;
esac
