#!/usr/bin/env bash
# The codex-review check (TXE-4661): a PR passes only when the Codex GitHub connector
# (chatgpt-codex-connector[bot]) has finished reviewing its current head and left no
# P0 or P1 finding there, or when a person has added the review-override label.
# docs/review.md is the procedure an agent follows around it.
#
#   scripts/codex-review-check.sh OWNER/REPO PR_NUMBER
#
# The decision, from what the connector posts (recorded on txehq/sub2api PR 290):
# 1. Label review-override on the PR: pass, and name who added it and when. A label
#    added by a bot does not count.
# 2. STATE is the JSON in `<!-- codex-review-state:v1 {...} -->`, in the newest summary
#    issue comment by the bot. code_review.commit_sha must be the PR head and
#    code_review.status must be "completed".
# 3. And the review saw the PR's current base (TXE-4801, txe's TXE-4666: the connector's P1s
#    on txehq/txe PR 4022). A retarget, or a force-push of a stacked PR's base
#    (`base_ref_changed`, `base_ref_force_pushed` issue events), changes the diff without
#    moving the head. The connector records neither the base it reviewed against nor when a
#    review started, so after the latest such event T the check is not yet satisfied until
#    ALL of:
#      a. the PR's latest `head_ref_force_pushed` event F is later than T;
#      b. the head is the commit F set, or descends from it (the compare API); and
#      c. the summary's Code Review row says the review completed after F, so the state
#         block is not one written before F about a SHA that F then restored.
#    After a retarget the author rebases onto the new base and force-pushes, and the
#    connector reviews the pushed head. The evidence is events GitHub and the connector
#    write on this PR, naming a commit and a time, with no timing margin. The review is
#    dated by its own row, never by the comment's updated_at, which a security review also
#    advances; a row with no readable time, or a time in the same second as F, is not yet.
# 4. A clean review posts no PR review object; it only rewrites that summary comment.
#    So no bot review on the head and has_any_findings false is a pass.
# 5. Otherwise every bot review whose commit_id is the head (a re-requested review can
#    leave out an earlier finding) fails on any of its inline comments, or its body,
#    carrying a "P0 Badge" or "P1 Badge"; P2 and P3 do not block.
# 6. No bot review on the head and has_any_findings true, and the connector's clean-pass
#    receipt names the head (TXE-4801, txe's TXE-4666): an issue comment by the bot carrying
#    `<!-- codex-review-receipt:v1 -->`, "major issues", and a "Reviewed commit" the head
#    starts with, posted within RECEIPT_WINDOW_S of the Code Review row's completion (so it
#    belongs to the review the state block records at the head, not to an earlier pass of
#    the same SHA): pass, printing the receipt's id and time. A clean re-review after a fix
#    loop keeps has_any_findings true while earlier findings' threads stay open (measured on
#    txehq/txe PR 4022 at 83822e7eb), so the receipt is what a clean pass reads then.
#    A has_any_findings that is neither true nor false is decided by that receipt alone (TXE-5125,
#    txe's TXE-5123: txehq/txe PR 4047 at 4505be9c5 completed with null, no review and a receipt
#    only on the manual re-request), so without one it fails until an `@codex review` posts it.
#    Rule 7 is never read for it: null does not claim findings, so earlier threads say nothing.
# 7. No bot review on the head, has_any_findings true and no receipt naming it (TXE-4787,
#    txe's TXE-4786): a re-review after a fix loop completes at the head with
#    has_any_findings still true from earlier pushes and posts neither a review nor a
#    receipt, because receipts are not posted per head (txehq/txe PRs 4024 and 4030). The
#    connector's live findings decide. A live P0/P1 is a review
#    thread whose first comment is the bot's, carries "P0 Badge" or "P1 Badge" and is not
#    outdated: GitHub keeps every thread no push has outdated on the head, so the commit it
#    names is not read. None live: pass. Each live one resolved by an actor whose GraphQL
#    type is User and whose login does not end in [bot]: pass, printing thread, path and
#    resolver. resolvedBy is typed User in GitHub's schema, so a thread an App resolves comes
#    back with resolvedBy null and blocks; the type is read as well as the [bot] suffix in
#    case the field ever widens to Actor, where a Bot's login has no suffix (TXE-4892, the
#    connector's P1 on txehq/bifrost PR 10). Otherwise fail. The threads come from one
#    paginated GraphQL read; a failed read fails.
# security_review is printed, not gated on.
#
# Everything is read from the API at run time, never from the triggering event's text.
#
# Environment:
#   CODEX_REVIEW_FIXTURE_DIR   read pull.json, reviews.json, review_comments.json,
#                              issue_comments.json, events.json, review_threads.json
#                              (the GraphQL pages) and compare.json (rule 3b) from this
#                              directory instead of calling gh
#                              (tests/codex_review_check_test.sh)
#   CODEX_REVIEW_WAIT_SECONDS  while the head has no completed review yet, poll for up to
#                              this long before failing (default 0: decide at once). A
#                              clean review emits no pull_request_review event, so the
#                              run started by the push has to wait for it.
#   CODEX_REVIEW_POLL_SECONDS  poll interval (default 30)
#   GITHUB_STEP_SUMMARY        the verdict is appended here when set
set -Eeuo pipefail

BOT='chatgpt-codex-connector[bot]'
OVERRIDE_LABEL='review-override'
STATE_MARKER='<!-- codex-review-state:v1 '
GRAPHQL_BOT='chatgpt-codex-connector' # GraphQL names a Bot without the [bot] suffix
RECEIPT_MARKER='<!-- codex-review-receipt:v1 -->'
# The receipt is posted 1 to 3 s before the Code Review row's completion time (txehq/txe PR
# 3953: 17:23:33Z against 17:23:35.8Z; PR 4022: 21:09:56Z against 21:09:57.6Z); a pass takes
# minutes, so 120 s cannot reach a receipt of the previous pass.
RECEIPT_WINDOW_S=120
# A finding's first line, badge markup reduced to "P1".
HEADLINE='def headline: split("\n")[0]
  | gsub("!\\[(?<p>P[0-9]) Badge\\]\\([^)]*\\)"; .p)
  | gsub("\\*\\*|</?sub>"; "") | gsub("\\s+"; " ") | ltrimstr(" ");'
# Every review thread of the PR with its first comment: what rule 7 reads.
# shellcheck disable=SC2016 # GraphQL variables, not shell ones
THREADS_QUERY='query($owner: String!, $name: String!, $pr: Int!, $endCursor: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $pr) {
      reviewThreads(first: 100, after: $endCursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          isResolved isOutdated line originalLine
          resolvedBy { __typename login }
          comments(first: 1) { nodes { databaseId author { __typename login } path body } }
        }
      }
    }
  }
}'

trap 'echo "codex-review: the check itself failed at line $LINENO" >&2' ERR

[[ $# -eq 2 && $1 == */* && $2 =~ ^[0-9]+$ ]] || {
  echo "usage: $0 OWNER/REPO PR_NUMBER" >&2
  exit 2
}
REPO=$1
PR=$2
WAIT=${CODEX_REVIEW_WAIT_SECONDS:-0}
POLL=${CODEX_REVIEW_POLL_SECONDS:-30}

# fetch NAME API_PATH: one object for the pull and the compare, every page of a list otherwise.
fetch() {
  if [[ -n ${CODEX_REVIEW_FIXTURE_DIR:-} ]]; then
    cat "$CODEX_REVIEW_FIXTURE_DIR/$1.json"
  elif [[ $1 == pull || $1 == compare ]]; then
    gh api "$2"
  else
    gh api --paginate "$2" --jq '.[]' | jq -s .
  fi
}

# live_threads sets VERDICT and REASON from the connector's live P0/P1 threads (rule 7).
live_threads() {
  local pages errors rows flag thread who title named='' blocking=''
  if [[ -n ${CODEX_REVIEW_FIXTURE_DIR:-} ]]; then
    pages=$(cat "$CODEX_REVIEW_FIXTURE_DIR/review_threads.json")
  else
    pages=$(gh api graphql --paginate -F owner="${REPO%%/*}" -F name="${REPO#*/}" -F pr="$PR" \
      -f query="$THREADS_QUERY" | jq -s .) || {
      VERDICT=fail REASON="could not read the PR's review threads (the GraphQL error is above); re-run this check"
      return
    }
  fi
  errors=$(jq -r '[.[]? | .errors[]? | .message // tostring] | join("; ")' <<<"$pages" 2>&1) \
    || errors="not JSON: $errors"
  if [[ -n $errors ]]; then
    VERDICT=fail REASON="could not read the PR's review threads (GraphQL: $errors); re-run this check"
    return
  fi
  # One row per live P0/P1 of the connector: ok|block, thread, resolution, title.
  rows=$(jq -er --arg bot "$GRAPHQL_BOT" "$HEADLINE"'
    if type != "array" or length == 0 then error("no pages") else . end
    | [.[] | .data.repository.pullRequest.reviewThreads.nodes
           | if type == "array" then .[] else error("a page without reviewThreads.nodes") end]
    | map(if (.isResolved | type) == "boolean" and (.isOutdated | type) == "boolean" then .
          else error("a thread without isResolved or isOutdated") end)
    | map((.comments.nodes[0] // {}) as $c
          | select($c.author.login == $bot and $c.author.__typename == "Bot"
                   and .isOutdated == false and ($c.body // "" | test("P[01] Badge")))
          | (.resolvedBy.login // "") as $who
          | (.resolvedBy.__typename // "") as $kind
          | [(if .isResolved and $who != "" and $kind == "User" and ($who | endswith("[bot]") | not)
              then "ok" else "block" end),
             "thread \($c.databaseId) \($c.path):\(.line // .originalLine // "?")",
             (if .isResolved | not then "unresolved"
              elif $who == "" then "resolved by no named user"
              elif $kind == "Bot" or ($who | endswith("[bot]")) then "resolved by a bot, \($who)"
              elif $kind != "User" then "resolved by \($who), not a User: \(if $kind == "" then "no actor type" else $kind end)"
              else "resolved by \($who)" end),
             ($c.body | headline)] | @tsv)
    | join("\n")' <<<"$pages" 2>&1) || {
    VERDICT=fail REASON="could not read the PR's review threads (not what GraphQL returns: $rows); re-run this check"
    return
  }
  if [[ -z $rows ]]; then
    VERDICT=pass REASON="connector completed its review of $HEAD_SHA with no review or receipt on it, and none of its live findings there is P0/P1 (has_any_findings carries over from earlier pushes)"
    return
  fi
  while IFS=$'\t' read -r flag thread who title; do
    DETAILS+=("$thread, $who: $title")
    named+="${named:+; }$thread $who"
    [[ $flag == ok ]] || blocking+="${blocking:+; }$thread ($who)"
  done <<<"$rows"
  if [[ -z $blocking ]]; then
    VERDICT=pass REASON="connector completed its review of $HEAD_SHA with no review or receipt on it, and every live P0/P1 thread of the connector there was resolved by a person: $named"
    return
  fi
  VERDICT=fail REASON="the connector's state block reports findings on $HEAD_SHA with no review of it and no receipt for it, and its live P0/P1 findings there are not resolved by a person: $blocking. Fix them and push; or reply in the thread with evidence and have a person resolve it (the codex-review-nudge job then re-runs this check within about 5 minutes); or comment @codex review once, then re-run this check"
}

# base_seen sets VERDICT=pending and REASON when the PR's base changed after the review of
# the head could have started (rule 3), and VERDICT=fail when the events cannot be read.
# REVIEWED is the Code Review row's completion time, to the second, or empty.
base_seen() {
  local events based pushed pushed_at pushed_sha cmp
  events=$(fetch events "repos/$REPO/issues/$PR/events")
  based=$(jq -er '
    [.[] | select(.event == "base_ref_changed" or .event == "base_ref_force_pushed") | .created_at]
    | if any(.[]; type != "string") then error("an event without a created_at") else (max // "") end' <<<"$events" 2>&1) || {
    VERDICT=fail REASON="could not read the PR's base-change events ($based); re-run this check"
    return
  }
  [[ -n $based ]] || return 0
  # The latest force-push of the PR's head branch: "<created_at> <commit>", or "".
  pushed=$(jq -er '
    [.[] | select(.event == "head_ref_force_pushed")] | max_by(.created_at)
    | if . == null then ""
      elif (.created_at | type) != "string" or (.commit_id | type) != "string"
           or (.commit_id | test("^[0-9a-f]{40}$") | not) then error("a force-push event without its time or commit")
      else "\(.created_at) \(.commit_id)" end' <<<"$events" 2>&1) || {
    VERDICT=fail REASON="could not read the PR's force-push events ($pushed); re-run this check"
    return
  }
  pushed_at=${pushed%% *} pushed_sha=${pushed#* }
  if [[ -z $pushed || ! $pushed_at > $based ]]; then
    VERDICT=pending REASON="the PR's base changed at $based and the head branch has not been force-pushed since (latest: ${pushed_at:-none}), so the review of $HEAD_SHA may be of the old diff — rebase onto the new base and force-push"
    return
  fi
  if [[ $pushed_sha != "$HEAD_SHA" ]]; then
    cmp=$(fetch compare "repos/$REPO/compare/$pushed_sha...$HEAD_SHA")
    # ahead: the head descends from the commit the push after the change set.
    if [[ $(jq -r '.status // "null"' <<<"$cmp") != ahead ]]; then
      VERDICT=pending REASON="the PR's base changed at $based, and the head $HEAD_SHA does not descend from $pushed_sha, the commit its force-push at $pushed_at set — rebase onto the new base and force-push"
      return
    fi
  fi
  if [[ -z $REVIEWED || ! $REVIEWED > $pushed_at ]]; then
    VERDICT=pending REASON="the PR's base changed at $based and its head was force-pushed at $pushed_at, but the connector's code review is dated ${REVIEWED:-nowhere} (its Code Review row), not after that push — comment @codex review for a review of the pushed head"
  fi
}

# receipt prints "<id> at <created_at>" of the bot's clean-pass receipt for the head posted
# within RECEIPT_WINDOW_S of REVIEWED (rule 6), or nothing.
receipt() {
  [[ -n $REVIEWED ]] || return 0
  jq -r --arg bot "$BOT" --arg m "$RECEIPT_MARKER" --arg h "$HEAD_SHA" --arg r "$REVIEWED" \
    --argjson w "$RECEIPT_WINDOW_S" '
    ($r | fromdateiso8601) as $rt
    | [.[] | select(.user.login == $bot and .user.type == "Bot" and (.body | type) == "string"
                    and (.body | contains($m)) and (.body | test("major issues")))
           | . as $c
           | ($c.body | capture("Reviewed commit:[*]* *`(?<s>[0-9a-f]{7,40})`") | .s) as $s
           | select($h | startswith($s))
           | (($c.created_at | fromdateiso8601) - $rt) as $d
           | select((if $d < 0 then -$d else $d end) <= $w)
           | "\($c.id) at \($c.created_at)"] | last // ""' <<<"$ISSUE_COMMENTS"
}

# evaluate sets VERDICT (pass, fail, or pending: worth polling again) and REASON.
evaluate() {
  local pull ev actor actor_type at summary state sha status reviews ids comments hits line found findings why
  DETAILS=() SECURITY='' REVIEWED=''
  pull=$(fetch pull "repos/$REPO/pulls/$PR")
  HEAD_SHA=$(jq -er '.head.sha' <<<"$pull")

  if jq -e --arg l "$OVERRIDE_LABEL" 'any(.labels[]; .name == $l)' <<<"$pull" >/dev/null; then
    ev=$(fetch events "repos/$REPO/issues/$PR/events" | jq -c --arg l "$OVERRIDE_LABEL" \
      '[.[] | select(.event == "labeled" and .label.name == $l)] | sort_by(.created_at) | last // empty')
    if [[ -z $ev ]]; then
      VERDICT=fail REASON="label $OVERRIDE_LABEL is on the PR, but the events API has no labeled event for it"
      return
    fi
    actor=$(jq -r '.actor.login' <<<"$ev")
    actor_type=$(jq -r '.actor.type' <<<"$ev")
    at=$(jq -r '.created_at' <<<"$ev")
    if [[ $actor_type == Bot ]]; then
      VERDICT=fail REASON="label $OVERRIDE_LABEL was added by $actor at $at, a bot; the override is human-only"
      return
    fi
    VERDICT=pass REASON="overridden: label $OVERRIDE_LABEL added by $actor at $at"
    return
  fi

  if [[ $(jq -r '.draft' <<<"$pull") == true ]]; then
    VERDICT=fail REASON="the PR is a draft; the connector reviews it once it is marked ready for review"
    return
  fi

  ISSUE_COMMENTS=$(fetch issue_comments "repos/$REPO/issues/$PR/comments")
  summary=$(jq -c --arg bot "$BOT" --arg m "$STATE_MARKER" '
    [.[] | select(.user.login == $bot and .user.type == "Bot" and (.body | contains($m)))]
    | sort_by(.updated_at) | last // empty' <<<"$ISSUE_COMMENTS")
  if [[ -z $summary ]]; then
    VERDICT=pending REASON="no connector review yet: no codex-review-state:v1 summary comment by $BOT"
    return
  fi
  state=$(jq -c '.body | capture("<!-- codex-review-state:v1 (?<j>\\{.*\\}) -->").j | fromjson' <<<"$summary" 2>/dev/null) \
    || { VERDICT=fail REASON="the connector's codex-review-state:v1 block is not valid JSON"; return; }
  sha=$(jq -r '.code_review.commit_sha // "none"' <<<"$state")
  status=$(jq -r '.code_review.status // "none"' <<<"$state")
  SECURITY=$(jq -r '"security_review (not gated): status \(.security_review.status // "none"), commit \(.security_review.commit_sha // "none")"' <<<"$state")
  if [[ $sha != "$HEAD_SHA" ]]; then
    VERDICT=pending REASON="connector has not reviewed the current head: its state block names $sha, the head is $HEAD_SHA"
    return
  fi
  if [[ $status != completed ]]; then
    VERDICT=pending REASON="connector review of $HEAD_SHA is $status, not completed"
    return
  fi
  # The Code Review row's completion time, to the second; empty when the row carries none.
  REVIEWED=$(jq -r '(.body | capture("[*][*]Code Review[*][*][^\n]*?datetime=\"(?<t>[^\"]+)\"") | .t) // ""
    | select(test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}([.][0-9]+)?Z$")) | .[0:19] + "Z"' <<<"$summary")

  VERDICT=
  base_seen
  [[ -z $VERDICT ]] || return 0

  reviews=$(fetch reviews "repos/$REPO/pulls/$PR/reviews")
  reviews=$(jq -c --arg bot "$BOT" --arg head "$HEAD_SHA" '
    [.[] | select(.user.login == $bot and .user.type == "Bot" and .commit_id == $head)]' <<<"$reviews")
  if [[ $reviews == "[]" ]]; then
    findings=$(jq -c '.code_review.has_any_findings' <<<"$state")
    case $findings in
      false) VERDICT=pass REASON="connector completed its review of $HEAD_SHA with no findings" ;;
      *)
        found=$(receipt)
        if [[ $findings == true ]]; then why="stays true from earlier pushes"; else why="is $findings"; fi
        if [[ -n $found ]]; then
          VERDICT=pass REASON="connector completed its review of $HEAD_SHA with no review on it, and its receipt $found says no major issues there (has_any_findings $why)"
        elif [[ $findings == true ]]; then
          live_threads # null claims nothing the threads could answer
        else
          VERDICT=fail REASON="the connector's state block gives has_any_findings as $findings on $HEAD_SHA, and neither a connector review of $HEAD_SHA nor a receipt for it was found — comment @codex review once (a clean pass posts a receipt), then re-run this check; or a person decides on $OVERRIDE_LABEL"
        fi
        ;;
    esac
    return
  fi

  ids=$(jq -r 'map(.id) | join(", ")' <<<"$reviews")
  comments=$(fetch review_comments "repos/$REPO/pulls/$PR/comments")
  # Each failing finding as path:line and its first line, badge markup reduced to "P1".
  hits=$(jq -r --argjson reviews "$reviews" "$HEADLINE"'
    ($reviews | map(.id)) as $ids
    | (.[] | select(.pull_request_review_id as $r | $ids | index($r)) | select(.body | test("P[01] Badge"))
      | "\(.path):\(.line // .original_line // "?") \(.body | headline)"),
    ($reviews[] | select(.body // "" | test("P[01] Badge")) | "review \(.id) body: \(.body | headline)")
    ' <<<"$comments")
  if [[ -n $hits ]]; then
    while IFS= read -r line; do DETAILS+=("$line"); done <<<"$hits"
    VERDICT=fail REASON="connector review(s) $ids of $HEAD_SHA have $(wc -l <<<"$hits" | tr -d ' ') P0/P1 finding(s)"
    return
  fi
  VERDICT=pass REASON="connector review(s) $ids of $HEAD_SHA have no P0/P1 finding"
}

deadline=$((SECONDS + WAIT))
while :; do
  evaluate
  [[ $VERDICT == pending ]] || break
  ((SECONDS + POLL <= deadline)) || break
  echo "codex-review: waiting ${POLL}s: $REASON"
  sleep "$POLL"
done

[[ $VERDICT == pass ]] && word=PASS || word=FAIL
report="codex-review: $word: $REASON
head: $HEAD_SHA"
for d in "${DETAILS[@]}"; do report+=$'\n'"  $d"; done
[[ -z $SECURITY ]] || report+=$'\n'"$SECURITY"
echo "$report"
if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then
  printf '### codex-review: %s\n\n```\n%s\n```\n' "$word" "$report" >>"$GITHUB_STEP_SUMMARY"
fi
[[ $word == PASS ]] || exit 1
