# txehq/txe PR 4047 at 4505be9c5, a completed review with `has_any_findings: null`

The connector behaviour TXE-5123 handles, first measurement. The state block on the head read
`code_review: {commit_sha: 4505be9c574eec2221a853465ce04d8070926f49, status: completed,
has_any_findings: null, has_unresolved_findings: null}`, and no review object was posted for the
head. The manual pass (`@codex review` at 20:07:10Z) posted a clean receipt, issue comment
6373791844 at 20:11:33Z ("Didn't find any major issues", Reviewed commit `4505be9c57`); its Code
Review row says completed at 20:11:35.258285Z. Run 40010514355, re-run after the receipt, printed:

```text
FAIL: the connector's state block gives has_any_findings as null on 4505be9c574eec2221a853465ce04d8070926f49, not false, and no connector review of 4505be9c574eec2221a853465ce04d8070926f49 was found — re-run this check
```

The coordinator's count, from TXE-5123's brief: in the 155 completed state records of the
2026-10-19..23 window the field was never null (125 false, 30 true).

## Recorded on 2026-10-23 at 20:18:30Z, head still 4505be9c5

```bash
gh api repos/txehq/txe/pulls/4047                                      # pull.json, trimmed
gh api --paginate 'repos/txehq/txe/issues/4047/comments?per_page=100'  # issue-comments.json
gh api --paginate 'repos/txehq/txe/issues/4047/events?per_page=100'    # issue-events.json
gh api --paginate 'repos/txehq/txe/pulls/4047/reviews?per_page=100'    # reviews.json
gh api --paginate 'repos/txehq/txe/pulls/4047/comments?per_page=100'   # review-comments.json
gh api graphql --paginate -F owner=txehq -F name=txe -F pr=4047 -f query="$THREADS_QUERY" | jq -s .  # review-threads.json
```

`THREADS_QUERY` is the one in `ops/ci/codex-review-check.sh` as it read that day. Each list is
joined into one array. Three files are trimmed to the fields the check reads, and nothing else
is edited: `pull.json` keeps `number`, `state`, `draft`, `head.sha` and the label names;
`reviews.json` each review's `id`, `commit_id`, `state`, `submitted_at`, `user` login and type and
`body`; `review-comments.json` each comment's `id`, `pull_request_review_id`, `commit_id`, `user`
login and type, `path`, `line`, `original_line` and `body` (as read it was 1.6 MB, mostly
`diff_hunk`). Nothing is reconstructed. Every live (not
outdated) connector P0/P1 thread in `review-threads.json` was resolved by a User.
