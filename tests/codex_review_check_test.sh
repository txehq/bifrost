#!/usr/bin/env bash
# Runs scripts/codex-review-check.sh (TXE-4661) against API payloads recorded from the
# Codex connector, so the check is tested without the connector.
#
# tests/fixtures/codex-review/sub2api-pr290/ holds txehq/sub2api PR 290 as recorded on
# 2026-10-17 (`gh api` of pulls/290, its reviews and comments, and issues/290 comments
# and events; pull.json is cut down to the fields the check reads). Its head c6f314a3
# had a clean connector pass: the summary comment's state block names it, completed,
# with no findings, and there is no review object on it. Its newest connector review,
# 5550906105, is on the previous push, 2bf1311d, with two P2 findings.
# labeled-event.json is a labeled event recorded from txehq/txe.
# txe-pr4024/ and txe-pr4030/ are the two txehq/txe holds the live-thread rule (TXE-4787)
# was measured on; txe-pr4022/ is the clean re-review the receipt rule (TXE-4801) passes, and
# txe-pr3713-base-and-force-push-events.json the retarget the base-change rule is tested on;
# txe-pr4047/ is the completed review with has_any_findings null (TXE-5125).
# tests/fixtures/codex-review/README.md says where every file came from.
#
# Each case below is that recording with the smallest edit that makes it the case.
set -Eeuo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
CHECK="$ROOT/scripts/codex-review-check.sh"
RECORDED="$ROOT/tests/fixtures/codex-review/sub2api-pr290"
LABELED="$ROOT/tests/fixtures/codex-review/labeled-event.json"
PREVIOUS_HEAD=2bf1311d2bb87741c536af0694229f85e885be44
TARGET=(txehq/sub2api 290) # the repository and PR the check is run for
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

failures=0

# new_case NAME [RECORDING]: a copy of the recording (default sub2api-pr290) to edit.
new_case() {
  mkdir -p "$WORK/$1"
  cp "${2:-$RECORDED}"/*.json "$WORK/$1/"
}

# edit CASE FILE JQ_FILTER [jq args...]
edit() {
  local file="$WORK/$1/$2" filter=$3
  shift 3
  jq "$@" "$filter" "$file" >"$file.new"
  mv "$file.new" "$file"
}

# The state block, rewritten for the head and findings the case needs.
set_state() {
  edit "$1" issue_comments.json '
    map(if .user.login == "chatgpt-codex-connector[bot]" and (.body | contains("codex-review-state:v1"))
        then .body |= sub("\"commit_sha\":\"[0-9a-f]{40}\",\"has_any_findings\":(true|false)";
                          "\"commit_sha\":\"\($sha)\",\"has_any_findings\":\($findings)")
        else . end)' --arg sha "$2" --arg findings "$3"
}

# expect CASE pass|fail TEXT...: the verdict, every TEXT in the output, and the
# verdict line in the step summary. FIXTURE_DIR set and empty runs the check against gh.
expect() {
  local name=$1 want=$2 out rc=0
  shift 2
  out=$(CODEX_REVIEW_FIXTURE_DIR="${FIXTURE_DIR-$WORK/$name}" GITHUB_STEP_SUMMARY="$WORK/$name.summary" \
    "$CHECK" "${TARGET[@]}" 2>&1) || rc=$?
  local got=pass
  ((rc == 0)) || got=fail
  local ok=1
  [[ $got == "$want" ]] || ok=0
  for text in "$@"; do
    [[ $out == *"$text"* ]] || ok=0
  done
  # The step summary carries the verdict and its reason.
  grep -qxF "### codex-review: ${got^^}" "$WORK/$name.summary" || ok=0
  grep -qxF -- "$(grep -m1 "^codex-review: ${got^^}: " <<<"$out")" "$WORK/$name.summary" || ok=0
  if ((ok)); then
    echo "ok   $name ($got)"
  else
    echo "FAIL $name: wanted $want and [$*], got $got (exit $rc):" >&2
    sed 's/^/     /' <<<"$out" >&2
    failures=$((failures + 1))
  fi
}

# 1. reviewed-head-clean: the recording as is. A clean pass leaves no review object.
new_case reviewed-head-clean
expect reviewed-head-clean pass "with no findings" "head: c6f314a3806a7acbadf6b906b2b95c7afbcff82d"

# 2. stale-head: a push after the review; the state block still names c6f314a3.
new_case stale-head
edit stale-head pull.json '.head.sha = "d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0"'
expect stale-head fail "has not reviewed the current head" "names c6f314a3806a7acbadf6b906b2b95c7afbcff82d"

# 3. P1-present: the head is 2bf1311d, which review 5550906105 reviewed, and one of
#    that review's two findings is a P1 instead of a P2.
new_case p1-present
edit p1-present pull.json '.head.sha = $h' --arg h "$PREVIOUS_HEAD"
set_state p1-present "$PREVIOUS_HEAD" true
edit p1-present review_comments.json 'map(if .id == 4297056859 then .body |= sub("P2 Badge"; "P1 Badge") else . end)'
expect p1-present fail "review(s) 5550906105 " "1 P0/P1 finding" \
  "services/api/internal/service/billing_service.go:815 P1 Normalize audio-only output before treating it as text"

# The same review with its findings left at P2 passes: P2 does not block.
new_case p2-only
edit p2-only pull.json '.head.sha = $h' --arg h "$PREVIOUS_HEAD"
set_state p2-only "$PREVIOUS_HEAD" true
expect p2-only pass "review(s) 5550906105 " "no P0/P1 finding"

# A newer review of the same head does not hide an older one's P1.
new_case p1-in-older-review
cp "$WORK/p1-present/"*.json "$WORK/p1-in-older-review/"
edit p1-in-older-review reviews.json \
  '. + [last | .id = 5550999999 | .submitted_at = "2026-10-16T17:30:00Z" | .body = "<!-- codex-review-receipt:v1 -->"]'
expect p1-in-older-review fail "5550906105, 5550999999" "billing_service.go:815 P1"

# 4. override: the P1 case, plus the review-override label added by a person.
new_case override
cp "$WORK/p1-present/"*.json "$WORK/override/"
edit override pull.json '.labels += [{name: "review-override"}]'
edit override events.json '. + [$ev | .label.name = "review-override" | .actor = {login: "a-maintainer", type: "User"}]' \
  --argjson ev "$(cat "$LABELED")"
expect override pass "overridden" "added by a-maintainer at 2026-10-17T10:52:23Z"

# The label added by a bot (the recorded event's own actor) is not an override.
new_case override-by-bot
cp "$WORK/override/"*.json "$WORK/override-by-bot/"
edit override-by-bot events.json 'map(if .event == "labeled" then .actor = {login: "github-actions[bot]", type: "Bot"} else . end)'
expect override-by-bot fail "added by github-actions[bot]" "human-only"

# A review still running on the head is not a pass.
new_case in-progress
edit in-progress issue_comments.json \
  'map(.body |= sub("\"status\":\"completed\"\\}"; "\"status\":\"in_progress\"}"))'
expect in-progress fail "is in_progress, not completed"

# No summary comment at all: the connector has not started.
new_case no-review
edit no-review issue_comments.json 'map(select(.user.login != "chatgpt-codex-connector[bot]"))'
expect no-review fail "no connector review yet"

# A pending state is polled until the wait runs out.
new_case stale-head-waits
cp "$WORK/stale-head/"*.json "$WORK/stale-head-waits/"
export CODEX_REVIEW_WAIT_SECONDS=2 CODEX_REVIEW_POLL_SECONDS=1
expect stale-head-waits fail "waiting 1s: connector has not reviewed the current head"
unset CODEX_REVIEW_WAIT_SECONDS CODEX_REVIEW_POLL_SECONDS

# Rule 3, the base changed: the head is unchanged and the review completed on it, but it may
# be a review of the old diff. The recording's Code Review row says it completed at
# 2026-10-16T17:12:43.617739Z, and its events carry no base change or force-push.
# base_event EVENT CREATED_AT and push_event CREATED_AT COMMIT: jq edits that append one event.
base_event() {
  printf '. + [{event: "%s", actor: {login: "a-maintainer", type: "User"}, created_at: "%s", commit_id: null}]' "$1" "$2"
}
push_event() {
  printf '. + [{event: "head_ref_force_pushed", actor: {login: "a-maintainer", type: "User"}, created_at: "%s", commit_id: "%s"}]' "$1" "$2"
}
RECORDED_HEAD=c6f314a3806a7acbadf6b906b2b95c7afbcff82d
OLDER=0123456789abcdef0123456789abcdef01234567

# A retarget after the review and no force-push since: not yet, so a fail at wait 0.
new_case retarget-not-pushed
edit retarget-not-pushed events.json "$(base_event base_ref_changed 2026-10-17T03:00:00Z)"
expect retarget-not-pushed fail "the PR's base changed at 2026-10-17T03:00:00Z and the head branch has not been force-pushed since (latest: none)" \
  "rebase onto the new base and force-push"

# The same is polled: the wait covers the rebase and the review that follows it.
new_case retarget-waits
cp "$WORK/retarget-not-pushed/"*.json "$WORK/retarget-waits/"
export CODEX_REVIEW_WAIT_SECONDS=2 CODEX_REVIEW_POLL_SECONDS=1
expect retarget-waits fail "waiting 1s: the PR's base changed at 2026-10-17T03:00:00Z"
unset CODEX_REVIEW_WAIT_SECONDS CODEX_REVIEW_POLL_SECONDS

# A force-push of a stacked PR's base is a base change too.
new_case base-force-pushed
edit base-force-pushed events.json "$(base_event base_ref_force_pushed 2026-10-17T03:00:00Z)"
expect base-force-pushed fail "has not been force-pushed since (latest: none)"

# A force-push of the head before the change does not count.
new_case pushed-before-retarget
edit pushed-before-retarget events.json \
  "$(push_event 2026-10-16T12:00:00Z "$RECORDED_HEAD") | $(base_event base_ref_changed 2026-10-16T13:00:00Z)"
expect pushed-before-retarget fail "has not been force-pushed since (latest: 2026-10-16T12:00:00Z)"

# txehq/txe PR 3713's recorded retarget and force-pushes (README.md): its head d470ad54 was
# set by the force-push at 06:20:36Z, 11 s after the retarget at 06:20:25Z. Grafted onto the
# recording: the head and the state block moved to d470ad54 and the Code Review row dated
# REVIEWED_AT.
HEAD_3713=d470ad541ba0caace510acd80c9766e7694d7849
EVENTS_3713="$ROOT/tests/fixtures/codex-review/txe-pr3713-base-and-force-push-events.json"
# retarget_3713 CASE REVIEWED_AT
retarget_3713() {
  new_case "$1"
  edit "$1" pull.json '.head.sha = $h' --arg h "$HEAD_3713"
  set_state "$1" "$HEAD_3713" false
  edit "$1" issue_comments.json 'map(.body |= gsub("datetime=\"[^\"]*\""; "datetime=\"" + $t + "\""))' --arg t "$2"
  edit "$1" events.json '. + $rec[0]' --slurpfile rec "$EVENTS_3713"
}
# Force-pushed after the retarget, and the Code Review row dated after the push: passes.
retarget_3713 retarget-pushed-reviewed-after 2026-10-15T06:25:00.000000Z
expect retarget-pushed-reviewed-after pass "completed its review of $HEAD_3713 with no findings"
# Force-pushed after the retarget, but the Code Review row older than the push: not yet.
retarget_3713 retarget-pushed-reviewed-before 2026-10-15T06:20:00.000000Z
expect retarget-pushed-reviewed-before fail \
  "force-pushed at 2026-10-15T06:20:36Z, but the connector's code review is dated 2026-10-15T06:20:00Z (its Code Review row), not after that push — comment @codex review"

# A force-push after the change to an older commit, then ordinary pushes on top: the head
# descends from it (compare.json, the compare API's answer, says ahead), so it passes; diverged,
# it does not.
new_case pushed-then-commits
edit pushed-then-commits events.json \
  "$(base_event base_ref_changed 2026-10-16T12:00:00Z) | $(push_event 2026-10-16T12:05:00Z "$OLDER")"
echo '{"status": "ahead", "ahead_by": 2, "behind_by": 0}' >"$WORK/pushed-then-commits/compare.json"
expect pushed-then-commits pass "with no findings"
new_case pushed-diverged
cp "$WORK/pushed-then-commits/"*.json "$WORK/pushed-diverged/"
echo '{"status": "diverged", "ahead_by": 1, "behind_by": 1}' >"$WORK/pushed-diverged/compare.json"
expect pushed-diverged fail "does not descend from $OLDER"

# A force-push event that names no commit is not evidence.
new_case pushed-no-commit
edit pushed-no-commit events.json \
  "$(base_event base_ref_changed 2026-10-16T12:00:00Z) | . + [{event: \"head_ref_force_pushed\", created_at: \"2026-10-16T12:05:00Z\", commit_id: null}]"
expect pushed-no-commit fail "could not read the PR's force-push events"

# 5. Findings reported on the head with no review of it: the connector's live threads
#    decide (TXE-4787). The recording with has_any_findings true: its two live threads are
#    resolved P2s, so it passes.
new_case findings-without-review
set_state findings-without-review c6f314a3806a7acbadf6b906b2b95c7afbcff82d true
expect findings-without-review pass "none of its live findings there is P0/P1"

# txehq/txe PR 4024 at 3d984075, as its failed codex-review run saw it: P1 thread
# 4312476492 is open and not outdated; the author had answered it, not resolved it.
TARGET=(txehq/txe 4024)
TXE_4024="$ROOT/tests/fixtures/codex-review/txe-pr4024"
P1_4024="thread 4312476492 okr/web/apps/web/e2e/real/notifications-m2.desktop.real.spec.ts:423"
# on_p1 JQ: the review_threads.json edit that applies JQ to that P1 thread.
on_p1() {
  printf '.[].data.repository.pullRequest.reviewThreads.nodes |= map(if .comments.nodes[0].databaseId == 4312476492 then %s else . end)' "$1"
}
new_case recorded-4024 "$TXE_4024"
expect recorded-4024 fail "not resolved by a person: $P1_4024 (unresolved). Fix them and push" \
  "$P1_4024, unresolved: P1 "

# The same thread resolved by a person: passes, naming the resolver.
new_case p1-resolved-by-user "$TXE_4024"
edit p1-resolved-by-user review_threads.json "$(on_p1 '.isResolved = true | .resolvedBy = {__typename: "User", login: "a-maintainer"}')"
expect p1-resolved-by-user pass "resolved by a person: $P1_4024 resolved by a-maintainer"

# Resolved by a [bot] login: not a person's decision.
new_case p1-resolved-by-bot "$TXE_4024"
edit p1-resolved-by-bot review_threads.json "$(on_p1 '.isResolved = true | .resolvedBy = {login: "github-actions[bot]"}')"
expect p1-resolved-by-bot fail "$P1_4024 (resolved by a bot, github-actions[bot])"

# Resolved by an App: resolvedBy is typed User, so GitHub returns it as null.
new_case p1-resolved-by-app "$TXE_4024"
edit p1-resolved-by-app review_threads.json "$(on_p1 '.isResolved = true | .resolvedBy = null')"
expect p1-resolved-by-app fail "$P1_4024 (resolved by no named user)"

# Were resolvedBy to widen to Actor, a Bot would come back without the [bot] suffix
# (TXE-4892, the connector's P1 on txehq/bifrost PR 10): its type says it is not a person.
new_case p1-resolved-by-graphql-bot "$TXE_4024"
edit p1-resolved-by-graphql-bot review_threads.json "$(on_p1 '.isResolved = true | .resolvedBy = {__typename: "Bot", login: "github-actions"}')"
expect p1-resolved-by-graphql-bot fail "$P1_4024 (resolved by a bot, github-actions)"

# A resolver with no actor type is not shown to be a person either.
new_case p1-resolved-no-type "$TXE_4024"
edit p1-resolved-no-type review_threads.json "$(on_p1 '.isResolved = true | .resolvedBy = {login: "a-maintainer"}')"
expect p1-resolved-no-type fail "$P1_4024 (resolved by a-maintainer, not a User: no actor type)"

# Outdated by a later push: not a finding on the head, so not counted.
new_case p1-outdated "$TXE_4024"
edit p1-outdated review_threads.json "$(on_p1 '.isOutdated = true')"
expect p1-outdated pass "none of its live findings there is P0/P1"

# A GraphQL error is never a pass.
new_case graphql-error "$TXE_4024"
echo '[{"data": null, "errors": [{"type": "RATE_LIMITED", "message": "API rate limit exceeded"}]}]' \
  >"$WORK/graphql-error/review_threads.json"
expect graphql-error fail "could not read the PR's review threads (GraphQL: API rate limit exceeded)"

# Nor is gh failing the GraphQL read: a stand-in gh serves the REST reads from the
# recording and fails the GraphQL one the way gh does.
new_case gh-graphql-fails "$TXE_4024"
mkdir -p "$WORK/bin"
cat >"$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
[[ $2 != graphql ]] || { echo "gh: Something went wrong while executing your query." >&2; exit 1; }
for a; do [[ $a != repos/* ]] || path=$a; done
case $path in
  */pulls/*/reviews) f=reviews ;; */pulls/*/comments) f=review_comments ;;
  */issues/*/comments) f=issue_comments ;; */issues/*/events) f=events ;; */pulls/*) f=pull ;;
esac
if [[ $f == pull ]]; then cat "$STUB_DIR/$f.json"; else jq -c '.[]' "$STUB_DIR/$f.json"; fi
STUB
chmod +x "$WORK/bin/gh"
PATH="$WORK/bin:$PATH" STUB_DIR="$WORK/gh-graphql-fails" FIXTURE_DIR='' \
  expect gh-graphql-fails fail "Something went wrong" "the GraphQL error is above"

# txehq/txe PR 4030 at d3025fd2: every live thread is a resolved P2, so it passes.
TARGET=(txehq/txe 4030)
new_case recorded-4030 "$ROOT/tests/fixtures/codex-review/txe-pr4030"
expect recorded-4030 pass "none of its live findings there is P0/P1"

# Rule 6, the receipt: txehq/txe PR 4022 at 83822e7e, recorded. The connector posted receipt
# 6241141339 ("Didn't find any major issues", Reviewed commit 83822e7ebb) at 21:09:56Z, 1.6 s
# before its Code Review row's 21:09:57.600375Z, and no review object on the head; the state
# block still says has_any_findings true, and three P1 threads of earlier passes are open.
TARGET=(txehq/txe 4022)
TXE_4022="$ROOT/tests/fixtures/codex-review/txe-pr4022"
new_case recorded-4022 "$TXE_4022"
expect recorded-4022 pass "its receipt 6241141339 at 2026-10-17T21:09:56Z says no major issues there"

# The same receipt an hour before the row: an earlier pass of the same SHA, outside the window,
# so the live threads decide and the open P1s fail it.
new_case receipt-outside-window "$TXE_4022"
edit receipt-outside-window issue_comments.json 'map(if .id == 6241141339 then .created_at = "2026-10-17T20:09:56Z" else . end)'
expect receipt-outside-window fail "no review of it and no receipt for it, and its live P0/P1 findings there are not resolved by a person: thread 4304368919 "

# A receipt naming another commit is not a receipt for the head.
new_case receipt-other-commit "$TXE_4022"
edit receipt-other-commit issue_comments.json 'map(if .id == 6241141339 then .body |= sub("83822e7ebb"; "0fd56af346") else . end)'
expect receipt-other-commit fail "no review of it and no receipt for it"

# Nor is one written by someone other than the connector.
new_case receipt-not-connector "$TXE_4022"
edit receipt-not-connector issue_comments.json 'map(if .id == 6241141339 then .user = {login: "a-maintainer", type: "User"} else . end)'
expect receipt-not-connector fail "no review of it and no receipt for it"

# A has_any_findings that is neither true nor false (TXE-5125, txe's TXE-5123): txehq/txe PR 4047
# at 4505be9c, recorded on 2026-10-23. No review object on the head; receipt 6373791844 at
# 20:11:33Z names it, 2 s before the Code Review row's 20:11:35Z. Null is decided by that receipt
# alone. The live threads are irrelevant under null: every live P0/P1 thread in the recording was
# resolved by a user, which would pass the live-thread rule, and null without a receipt still fails.
TARGET=(txehq/txe 4047)
TXE_4047="$ROOT/tests/fixtures/codex-review/txe-pr4047"
HEAD_4047=4505be9c574eec2221a853465ce04d8070926f49
NULL_NO_RECEIPT="gives has_any_findings as null on $HEAD_4047, and neither a connector review of $HEAD_4047 nor a receipt for it was found"
NO_RECEIPT='map(select(.id != 6373791844))'
new_case null-with-receipt "$TXE_4047"
expect null-with-receipt pass "its receipt 6373791844 at 2026-10-23T20:11:33Z says no major issues there (has_any_findings is null)"
new_case null-without-receipt "$TXE_4047"
edit null-without-receipt issue_comments.json "$NO_RECEIPT"
expect null-without-receipt fail "$NULL_NO_RECEIPT"
new_case null-receipt-other-commit "$TXE_4047"
edit null-receipt-other-commit issue_comments.json 'map(if .id == 6373791844 then .body |= sub("4505be9c57"; "0fd56af346") else . end)'
expect null-receipt-other-commit fail "$NULL_NO_RECEIPT"
new_case null-receipt-outside-window "$TXE_4047"
edit null-receipt-outside-window issue_comments.json 'map(if .id == 6373791844 then .created_at = "2026-10-23T20:14:00Z" else . end)'
expect null-receipt-outside-window fail "$NULL_NO_RECEIPT"
# A live P1 thread (4361506470, unresolved) and no receipt: fails, on the null, not the thread.
new_case null-live-p1 "$TXE_4047"
edit null-live-p1 issue_comments.json "$NO_RECEIPT"
edit null-live-p1 review_threads.json '.[].data.repository.pullRequest.reviewThreads.nodes |= map(
  if .comments.nodes[0].databaseId == 4361506470 then .isResolved = false | .resolvedBy = null else . end)'
expect null-live-p1 fail "$NULL_NO_RECEIPT"
# The value is read as JSON, so a string "false" is not false.
TARGET=(txehq/sub2api 290)
new_case findings-malformed
set_state findings-malformed "$RECORDED_HEAD" '"false"'
expect findings-malformed fail "gives has_any_findings as \"false\" on $RECORDED_HEAD"

if ((failures)); then
  echo "$failures case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
