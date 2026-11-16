# Releasing the fork

This page is for txehq/bifrost, the fork. The upstream release process does not apply here.

## How a release happens

1. **A release PR bumps the version files.** It changes `transports/version` (and
   `core/version`, `framework/version` or `plugins/<name>/version` when those publish), the
   chart's `version` and `appVersion`, and the changelogs. Its squash subject carries
   `--skip-core-test` when the release should not run the live provider harness.
2. **Merging it starts the Release Pipeline** (`.github/workflows/release-pipeline.yml`) on
   that merge commit. The jobs that read provider credentials run in the `live-providers`
   environment, so the run waits there until a required reviewer approves it. The run tags
   `transports/v<version>` on the merge commit and publishes the image.
3. **The Helm Release** (`.github/workflows/helm-release.yml`) runs on the same merge,
   because the release PR changes `helm-charts/bifrost/`, and publishes
   `helm-chart-v<chart version>`.

## Only the version bump releases

A push to `main` may release only when the push itself changes a version file
(`release-trigger.sh`, TXE-7511). Every other push runs the gate jobs and stops: it builds
nothing, tags nothing and asks nobody for a `live-providers` approval, even while a version's
tag is still missing. `test-release-gates.py` fails if a release job can run on such a push.

Before TXE-7511, every merge after a version bump re-attempted that release from its own
commit, and each attempt waited for an approval. On sync.11 five such runs queued behind the
release, and two had to be cancelled by hand.

## Retrying a release that failed

Retry it on the same commit, by re-running the failed jobs of the release run:

```bash
gh run rerun <run-id> --failed -R txehq/bifrost
```

A re-run uses the workflow file as it was at the run's commit, and keeps the outputs of the
jobs that passed, so it releases the commit that was chosen. If the run was cancelled before
it started, for example replaced in the `release-pipeline` concurrency group by a later push's
run, re-run all of it: `gh run rerun <run-id> -R txehq/bifrost`.

When the fix for the failure is in a workflow file, a re-run cannot pick it up. Dispatch the
pipeline on `main` instead:

```bash
gh workflow run release-pipeline.yml -R txehq/bifrost --ref main
```

A dispatch always may release. It releases `main`'s head, which carries every merge since the
version bump, so list those merges in the GitHub release body. sync.11 was this case: its
release failed on an egress rule (TXE-7509), the fix was a workflow change, and the tag was cut
at the fix's merge, so the release notes name the seven PRs merged after the release PR.
