#!/usr/bin/env bash
set -euo pipefail

# helm-publish-pages.sh - publish a directory to the Pages branch as one commit
# on the exact revision the index was read from, by compare-and-swap.
#
# Why not a deploy action: clone-and-replace actions clone whatever the branch
# holds at deploy time, clear the destination directory and copy the checkout
# in, so an index built from an older revision silently replaces anything
# another writer published in between. This script builds the publication
# commit on the recorded base revision instead and pushes it with that
# revision as the expected current value of the branch (a lease on exactly
# that commit, or on the branch not existing). If the branch moved, the push
# is rejected, nothing is overwritten, and a re-run rebuilds from the new
# revision. That also makes a workflow concurrency group unnecessary: two
# simultaneous releases both run, and the one that loses the swap fails
# visibly instead of being cancelled while queued.
#
# What the commit contains: the base revision's tree with everything under
# --destination-dir replaced by the contents of --publish-dir, and nothing
# else touched (what keep_files: false did, confined to the destination).
# When there is no branch yet (base record `none`) the commit is a root
# commit holding the destination plus a root .nojekyll.
#
# Usage:
#   helm-publish-pages.sh --remote origin --publish-branch gh-pages \
#     --base-sha-file "$RUNNER_TEMP/helm-index-base-sha" \
#     --publish-dir helm-charts --destination-dir helm-charts \
#     --message "deploy: $GITHUB_SHA"
#
# The base record is what helm-publish-index.sh --base-sha-output wrote.
# Author and committer come from the GIT_AUTHOR_* / GIT_COMMITTER_* environment.
# Exit status is non-zero, with nothing pushed, on a moved branch, a bad base
# record, an unreachable remote or any git failure. Identical content is a
# no-op: nothing is pushed and the exit status is 0.

REMOTE="origin"
BRANCH="gh-pages"
BASE_FILE=""
PUBLISH_DIR=""
DEST=""
MESSAGE=""

die() {
  echo "ERROR: helm-publish-pages: $*" >&2
  exit 1
}

log() {
  echo "helm-publish-pages: $*"
}

while [ $# -gt 0 ]; do
  case "$1" in
    --remote) REMOTE="${2:-}"; shift 2 ;;
    --publish-branch) BRANCH="${2:-}"; shift 2 ;;
    --base-sha-file) BASE_FILE="${2:-}"; shift 2 ;;
    --publish-dir) PUBLISH_DIR="${2:-}"; shift 2 ;;
    --destination-dir) DEST="${2:-}"; shift 2 ;;
    --message) MESSAGE="${2:-}"; shift 2 ;;
    *) die "unknown argument: $1" ;;
  esac
done
for required in REMOTE BRANCH BASE_FILE PUBLISH_DIR DEST MESSAGE; do
  if [ -z "${!required}" ]; then
    die "--$(echo "$required" | tr '[:upper:]_' '[:lower:]-') is required"
  fi
done
[ -f "$BASE_FILE" ] || die "base revision record not found: $BASE_FILE"
[ -d "$PUBLISH_DIR" ] || die "publish directory not found: $PUBLISH_DIR"
DEST="${DEST%/}"
case "$DEST" in
  ""|.|..|/*|./*|../*|*/..|*/../*) die "--destination-dir must be a relative path inside the branch, got '$DEST'" ;;
esac
ABS_PUBLISH="$(cd "$PUBLISH_DIR" && pwd)"

BASE="$(tr -d '[:space:]' <"$BASE_FILE")"
case "$BASE" in
  none) ;;
  *)
    if ! printf '%s' "$BASE" | grep -E '^[0-9a-f]{40}$' >/dev/null; then
      die "base revision record '$BASE' is neither 'none' nor a commit id"
    fi ;;
esac

WORK="$(mktemp -d "${TMPDIR:-/tmp}/helm-publish-pages.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

moved() {
  die "$1 Nothing was overwritten. Re-run the job to rebuild the index from the current revision of '$BRANCH'."
}

# --- 1. The branch must still be where the index was read from ------------
# An early, explicit answer; the push below is the authoritative check.

set +e
git ls-remote --exit-code "$REMOTE" "refs/heads/$BRANCH" >"$WORK/ls-remote.out" 2>"$WORK/ls-remote.err"
rc=$?
set -e
case "$rc" in
  0) CURRENT="$(cut -f1 <"$WORK/ls-remote.out" | head -n 1)" ;;
  2) CURRENT="none" ;;
  *)
    cat "$WORK/ls-remote.err" >&2
    die "cannot query '$REMOTE' for refs/heads/$BRANCH (git ls-remote exit $rc); refusing to publish without knowing the branch's revision"
    ;;
esac
if [ "$CURRENT" != "$BASE" ]; then
  case "$BASE" in
    none) moved "publish branch '$BRANCH' appeared on '$REMOTE' (now at $CURRENT) after the index was built without it." ;;
    *)
      case "$CURRENT" in
        none) moved "publish branch '$BRANCH' disappeared from '$REMOTE' after the index was read from $BASE." ;;
        *) moved "publish branch '$BRANCH' on '$REMOTE' moved from $BASE to $CURRENT after the index was read." ;;
      esac ;;
  esac
fi

# --- 2. Build the publication tree on the base revision -------------------

if [ "$BASE" != "none" ]; then
  git fetch --no-tags --quiet "$REMOTE" "refs/heads/$BRANCH" || die "cannot fetch $BRANCH from $REMOTE"
  FETCHED="$(git rev-parse --verify --quiet FETCH_HEAD)" || die "fetched $BRANCH but FETCH_HEAD is unreadable"
  if [ "$FETCHED" != "$BASE" ]; then
    moved "publish branch '$BRANCH' on '$REMOTE' moved from $BASE to $FETCHED while publishing."
  fi
fi

export GIT_INDEX_FILE="$WORK/index"
if [ "$BASE" != "none" ]; then
  git read-tree "$BASE" || die "cannot read the tree of $BASE"
else
  git read-tree --empty
fi
git ls-files -z -- "$DEST" | git update-index --force-remove -z --stdin
find "$ABS_PUBLISH" -type f -print0 | while IFS= read -r -d '' file; do
  rel="${file#"$ABS_PUBLISH"/}"
  mode=100644
  [ -x "$file" ] && mode=100755
  blob="$(git hash-object -w -- "$file")" || exit 1
  printf '%s %s\t%s\0' "$mode" "$blob" "$DEST/$rel"
done | git update-index --add -z --index-info
if [ "$BASE" = "none" ]; then
  empty="$(git hash-object -w --stdin </dev/null)"
  printf '100644 %s\t.nojekyll\0' "$empty" | git update-index --add -z --index-info
fi
TREE="$(git write-tree)"
if [ "$BASE" != "none" ] && [ "$TREE" = "$(git rev-parse "$BASE^{tree}")" ]; then
  log "'$DEST' on '$BRANCH' already holds exactly this content; nothing to publish"
  exit 0
fi
if [ "$BASE" != "none" ]; then
  COMMIT="$(git commit-tree "$TREE" -p "$BASE" -m "$MESSAGE")"
else
  COMMIT="$(git commit-tree "$TREE" -m "$MESSAGE")"
fi
unset GIT_INDEX_FILE

# --- 3. Compare-and-swap push --------------------------------------------
# The lease names the exact revision (or absence) the branch must still have.
# This is never a plain force: a branch at any other revision rejects the push.

if [ "$BASE" != "none" ]; then
  lease="refs/heads/$BRANCH:$BASE"
else
  lease="refs/heads/$BRANCH:"
fi
if ! git push "$REMOTE" "${COMMIT}:refs/heads/$BRANCH" "--force-with-lease=$lease" >"$WORK/push.out" 2>"$WORK/push.err"; then
  cat "$WORK/push.err" >&2
  if grep -qE 'stale info|rejected|fetch first|non-fast-forward|already exists' "$WORK/push.err"; then
    moved "publish branch '$BRANCH' on '$REMOTE' changed while publishing (expected $BASE)."
  fi
  die "git push of $COMMIT to $REMOTE/$BRANCH failed"
fi

NOW="$(git ls-remote "$REMOTE" "refs/heads/$BRANCH" | cut -f1 | head -n 1)"
[ "$NOW" = "$COMMIT" ] || die "pushed $COMMIT but '$BRANCH' on '$REMOTE' now reads '$NOW'"
log "published $COMMIT to $REMOTE/$BRANCH on top of $BASE"
