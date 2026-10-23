# Recorded payloads for `scripts/codex-review-check.sh`

Copied byte for byte from txehq/action-runner-ops `tests/fixtures/codex-review/` at 6d06919
(TXE-4892); "here" below is that repository. They were recorded before `THREADS_QUERY` read
`resolvedBy.__typename`; the test cases that resolve a P0/P1 thread set it.

`tests/codex_review_check_test.sh` runs the check over these, one directory per PR. Each
directory holds the files `CODEX_REVIEW_FIXTURE_DIR` reads: `pull.json`, `reviews.json`,
`review_comments.json`, `issue_comments.json`, `events.json` and `review_threads.json` (the
GraphQL `reviewThreads` pages, read with the script's `THREADS_QUERY`, joined with `jq -s .`).

| Path | Where it came from |
| --- | --- |
| `sub2api-pr290/` | txehq/sub2api PR 290, recorded here on 2026-10-17 (TXE-4661) with `gh api`; `pull.json` cut down to the fields the check reads. `review_threads.json` is txehq/txe's `ops/ci/testdata/codex-review/pr290/review-threads.json` at 119fd1fe6 (TXE-4786), recorded 2026-10-21 with the head unchanged; txe's other files for PR 290 are identical to these. |
| `txe-pr4024/` | txehq/txe `ops/ci/testdata/codex-review/pr4024/` at 119fd1fe6: PR 4024 at 3d9840757 as its failed `codex-review` run saw it. One open, live P1 thread (4312476492). |
| `txe-pr4030/` | txehq/txe `ops/ci/testdata/codex-review/pr4030/` at 119fd1fe6: PR 4030 at d3025fd29 as its failed run saw it. Every live thread is a resolved P2. |
| `txe-pr4022/` | txehq/txe `ops/ci/testdata/codex-review/pr4022/` at 689ce5b39 (added in b708112c4, TXE-4666; `review-threads.json` added in a8cb6f054, TXE-4786): PR 4022 at 83822e7eb as the API returned it on 2026-10-18, `pull.json` trimmed, and its `reviewThreads` read on 2026-10-21 after the PR merged. A clean re-review after a fix loop: receipt 6241141339 (Reviewed commit `83822e7ebb`) at 21:09:56Z, 1.6 s before the Code Review row's completion, no review object on the head, `has_any_findings` true, three open P1 threads from earlier passes. The receipt rule passes it. txe ships no README for it. |
| `txe-pr3713-base-and-force-push-events.json` | txehq/txe `ops/ci/testdata/codex-review/pr3713-base-and-force-push-events.json` at 689ce5b39 (added in af8d2b3e0, TXE-4666): PR 3713's `base_ref_changed` and `head_ref_force_pushed` events as `gh api repos/txehq/txe/issues/3713/events` returned them on 2026-10-18, trimmed to the fields read. The force-push at 06:20:36Z, 11 s after the retarget at 06:20:25Z, set the head d470ad54. The test grafts these onto `sub2api-pr290/` for the base-change rule. |
| `txe-pr4047/` | txehq/txe `ops/ci/testdata/codex-review/pr4047/` at 4b14dafd8 (TXE-5123): PR 4047 at 4505be9c5 recorded on 2026-10-23, trimmed by txe to the fields the check reads. A completed review with `has_any_findings` null and no review object on the head; receipt 6373791844 (Reviewed commit `4505be9c57`) at 20:11:33Z, 2 s before the Code Review row's completion. Every live P0/P1 thread was resolved by a User. The null cases (TXE-5125) are this recording with one edit each. |
| `labeled-event.json` | a `labeled` issue event recorded from txehq/txe. |

The txe files are copied byte for byte, renamed to this script's names
(`issue-comments.json` → `issue_comments.json`, `issue-events.json` → `events.json`,
`review-comments.json` → `review_comments.json`, `review-threads.json` → `review_threads.json`).
Each txe directory that has a `README.md` in txe keeps it verbatim: it says what was
recorded and what was reconstructed to cut the PR back to the failed run. Its references to
`ops/ci/` are txe paths.

The other base-change cases (a retarget with no force-push since, a stacked base
force-pushed, a force-push before the retarget, a force-push to an older commit with
`compare.json` written by the test) are `sub2api-pr290/` with the events appended in
`tests/codex_review_check_test.sh`, where each edit is named; txe's test derives the same
cases the same way. The receipt cases outside the window, naming another commit or written
by a user are `txe-pr4022/` with one edit each, as in txe's test.
