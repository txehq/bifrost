# Releasing the fork

This page is for txehq/bifrost, the fork. The upstream release process does not apply here.

## How a release happens

1. **A release PR bumps the version files.** It changes `transports/version` (and
   `core/version`, `framework/version` or `plugins/<name>/version` when those publish), the
   chart's `version` and `appVersion`, and the changelogs. Its squash subject carries
   `--skip-core-test` when the release should not run the live provider harness.
2. **Merging it starts the Release Pipeline** (`.github/workflows/release.yml`) on that merge
   commit. Its `gate` job decides whether the run may release, and its `release` job then
   runs the release jobs in `.github/workflows/release-pipeline.yml`. The jobs that read
   provider credentials run in the `live-providers` environment, so the run waits there until
   a required reviewer approves it. The run tags `transports/v<version>` on the merge commit
   and publishes the image.
3. **The Helm Release** (`.github/workflows/helm-release.yml`) runs on the same merge,
   because the release PR changes `helm-charts/bifrost/`, and publishes
   `helm-chart-v<chart version>`.

## Approving a release

A release run asks for a `live-providers` approval on each job that runs in that environment
(the test suites, then `core-release`, `framework-release` and `plugins-release`). A required
reviewer approves it from the run page (**Review deployments**).
`gh api repos/txehq/bifrost/actions/runs/<run-id>/pending_deployments` lists what is waiting.

**Approval window: 7 days.** Approve every pending gate of a release run within 7 days of the
run starting. Every artifact that a job behind an approval gate downloads is kept at least that
long, and `test-release-gates.py` (`GatedConsumersOutliveTheApprovalWindow`) fails when one
is not: it reads the window from the line above, finds every job that downloads an artifact
after a job in an environment, and checks the retention the producing upload sets. An
environment counts as a gate unless the test lists it as ungated, so a new environment or a
new consumer is checked without anyone remembering to.

Past the window, the build artifacts may be gone, and re-running only the failed jobs fails
again with "Artifact not found", because the jobs that built them passed and are not re-run.
Re-run the whole run instead (`gh run rerun <run-id> -R txehq/bifrost`); it asks for the
approvals again.

The window exists because the sync.13 release (run 44223574926) waited about 32 hours for its
approval while `bifrost-http-binary` was kept for 1 day. Every job that downloaded it failed
with "Artifact not found for name: bifrost-http-binary", and nothing was released (TXE-7895).

## Only the version bump releases

A push to `main` may release only when the push itself changes a version file. The `gate`
job reads that from the git range (`release-trigger.sh`, TXE-7511). Every other push runs the
gate and stops: the `release` job is skipped, so nothing is built or tagged and nobody is asked
for a `live-providers` approval, even while a version's tag is still missing.

Only the `release` job is in the `release-pipeline` concurrency group (TXE-7566). It covers
the whole release, so two releases never publish at once. A push that may not release never
joins it, because a job skipped by its `if:` does not enter a group. GitHub keeps one pending
run per group and cancels the older one when another joins, so only another release (a version
bump or a dispatch) can replace a release that is waiting for the group. Before TXE-7566 every
push joined the group, and that is how the first retry of sync.11 was lost.

`test-release-gates.py` fails when the release jobs can start other than through the gated
`release` job, when the group moves off it, or when the workflows read a `github.event` field
that a real push payload does not carry (captured payloads in `scripts/testdata/`).
The Actions push payload has no `added`, `modified` or `removed` file lists.

Before TXE-7511, every merge after a version bump re-attempted that release from its own
commit, and each attempt waited for an approval. On sync.11 five such runs queued behind the
release, and two had to be cancelled by hand.

## For operators: where releases run now

Since TXE-7566 the Release Pipeline's workflow file is `release.yml`, and
`release-pipeline.yml` only holds the release jobs it calls.

- Dispatch a release with `gh workflow run release.yml -R txehq/bifrost --ref main`. A
  dispatch of `release-pipeline.yml` is refused, because that file no longer has a dispatch
  trigger.
- List runs with `gh run list -R txehq/bifrost --workflow release.yml`. Runs from before the
  change stay listed under `release-pipeline.yml`, so a watcher or script that filters on
  that name sees no new runs.
- A release run's jobs appear as `release / <job>`, for example
  `release / build-binaries-x86`. The approval for `live-providers` is asked on those jobs as
  before.
- Every push still starts a run. A push that may not release shows `gate` succeeded and
  `release` skipped.

The provider keys are `live-providers` environment secrets that the called jobs read
themselves. No run has yet shown them reaching a called workflow, because the environment held
no keys when this changed. TXE-7574 is the check to run at the first release that has keys.

## Retrying a release that failed

Retry it on the same commit, by re-running the failed jobs of the release run:

```bash
gh run rerun <run-id> --failed -R txehq/bifrost
```

A re-run uses the workflow file as it was at the run's commit, and keeps the outputs of the
jobs that passed, so it releases the commit that was chosen. If the run was cancelled before
it started (another release replaced it in the concurrency group), re-run all of it:
`gh run rerun <run-id> -R txehq/bifrost`.

When the fix for the failure is in a workflow file, a re-run cannot pick it up. Dispatch the
pipeline on `main` instead:

```bash
gh workflow run release.yml -R txehq/bifrost --ref main
```

A dispatch always may release. It releases `main`'s head, which carries every merge since the
version bump, so list those merges in the GitHub release body. sync.11 was this case: its
release failed on an egress rule (TXE-7509), the fix was a workflow change, and the tag was cut
at the fix's merge, so the release notes name the seven PRs merged after the release PR.
