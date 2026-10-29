# Review: the Codex connector and the `codex-review` check

This page is for txehq/bifrost, the fork; upstream maximhq/bifrost does not run it.

Every PR to `main` is reviewed by the Codex GitHub connector
(`chatgpt-codex-connector[bot]`) on open and on every push. The check `codex-review`
(`.github/workflows/codex-review.yml`, `scripts/codex-review-check.sh`, TXE-4892) passes
only when the connector has finished reviewing the PR's current head and left no P0 or P1
finding there, or a person has added the `review-override` label. The script and its
fixture tests (`tests/codex_review_check_test.sh`) are ported unchanged from
txehq/action-runner-ops; its header lists the seven rules it decides by. The `rpr` and
`rprf` skills post nothing that counts here.

## How the connector is configured

The connector is the GitHub App `chatgpt-codex-connector`, installed on the `txehq`
organisation for selected repositories; txehq/bifrost was added to it on 2026-10-22. Its
review settings (`chatgpt.com/settings/code-review`, repository `txehq/bifrost`) are
automatic review on for all PRs, on every push, security review on, reporting floor
Medium. When the plan behind it is rate-limited, reviews stop and the check fails after
its 25-minute wait; `@codex review` on the PR asks for another attempt.
`security_review` is printed, never gated on.

## What the connector posts

- **One summary issue comment**, rewritten on every review, carrying a state block:
  `<!-- codex-review-state:v1 {"code_review":{"commit_sha":…,"has_any_findings":…,"status":…},…} -->`.
- **A PR review object only when it has findings**, each an inline comment whose first
  line starts with a badge, `P0 Badge` to `P3 Badge`. P2 and P3 do not block.
- **A clean pass posts no review**: it rewrites the summary comment, and may post a
  `<!-- codex-review-receipt:v1 -->` comment naming the reviewed commit.

## The procedure an author follows

1. **Open the PR ready for review**, not as a draft. The connector does not review
   drafts, and the check fails on one.
2. **Wait for the state block on your head.** Poll the summary comment, not the workflow:

   ```bash
   gh api repos/txehq/bifrost/issues/<n>/comments --paginate \
     --jq '.[] | select(.user.login == "chatgpt-codex-connector[bot]") | .body' \
     | sed -n 's/.*<!-- codex-review-state:v1 \(.*\) -->.*/\1/p' | jq .code_review
   ```

   Done when `commit_sha` is your head (`gh pr view <n> -R txehq/bifrost --json headRefOid`)
   and `status` is `completed`. Until it is, the check polls every 30 seconds for up to 25
   minutes, long enough for the nudge job's re-request of a silent review and the review
   that follows to land (TXE-5348), then fails naming what differed. A `has_any_findings`
   that is neither true nor false (`null` on txehq/txe PR 4047, TXE-5126) passes only on
   the connector's receipt for the head and otherwise fails naming the value: comment
   `@codex review` once, which posts a receipt on a clean head, and re-run the check.
3. **Fix every P0 and P1 and push.** The push starts a new review; go back to 2. Fix a P2
   or P3 when it is right and cheap.
4. **When a finding is wrong, reply in its thread with the evidence and stop.** Never add
   `review-override` yourself. A person decides; if they agree, they add the label and
   re-run the latest `Codex review` run on the PR (labels do not start the workflow).
5. **Resolve the connector threads you have fixed or refuted before the next push.** A P0
   or P1 you refuted is resolved by a person, never by you: an open, live P0/P1 thread is
   what the check fails on when a re-review posts no review of its own. Resolving a thread
   starts no run; re-run the latest `Codex review` run after it.
6. **If the connector is silent, leave the re-request to the nudge job.** A review silent
   for 10 minutes is re-requested once by the nudge job (txehq/action-runner-ops
   `jobs/codex-review-nudge/`, an `@codex review` comment from `connorwang` carrying
   `<!-- codex-nudge:v1 <sha> -->`); one still silent 10 minutes after that gets a second
   comment asking for a new head: push one empty commit. Do not re-request by hand inside
   those windows.
7. **After a retarget, rebase onto the new base and force-push**; the check holds until
   the connector has reviewed a push made after the base changed. After a change to
   `codex-review.yml` lands on `main`, an open PR picks it up only through a new
   `pull_request` event (edit the PR description, or push); re-running an existing
   `codex-review` run reuses the workflow file it was created with. A change to the script
   needs no new event: every run, a re-run included, checks it out from the tip of `main`.
8. **Merge** with a squash once `codex-review` and the other required checks are green.

## Limits

The workflow checks out `main` and runs the script from there, so a PR cannot pass its own
check by editing the script; it does run the PR's own copy of the workflow file, so a PR
that edits `.github/workflows/codex-review.yml` changes its own check. Agents act through
the same GitHub account as the people who add the override, so the check can name who
added the label but not tell a person from an agent. `review-override` is human-only.
