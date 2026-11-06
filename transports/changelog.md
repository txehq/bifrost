## 📝 Upstream

- docs: the Edge v0.9.4 changelog page (upstream 2c4bd66d7)
- docs: the OpenAPI MCP server guide shows three new screenshots from `docs/media/mcp/`; the images it named before were missing (upstream efe1b351f)
- ci: on `pull_request`, the codspeed, dependency-review and workflow-lint jobs run in an approval environment: upstream sends a pull request from a fork or from an author who is not an owner or member to `pr-testing`, and others to `ci-trusted-branch` (upstream 9c594766d, #8869)

## 🍴 Fork changes (txehq/bifrost)

- chore: v2.4.0-sync.1 is a prerelease on top of v2.3.5 and the first batch toward upstream transports/v2.4.0 (TXE-5856). It merges upstream through efe1b351f, the first three commits after 1123ea210; v2.3.5 stays the stable release and `latest`
- chore: no gateway code changes, only documentation and three workflow files
- fork: the approval environment sends a pull request to `pr-testing` only when its head branch is in another repository (`head.repo.full_name != github.repository`). Upstream also reads `head.repo.fork` and `author_association`, and neither works here: this repository is itself a fork of maximhq/bifrost, so `head.repo.fork` is true for its own branches, and txehq membership is private, so the event reports members as non-members (PR #94's own jobs went to `pr-testing` under that test). A branch in this repository needs write access, which is the trust the boundary checks. The three jobs keep running on `vars.BIFROST_RUNNER_AMD64`
- test: Workflow Lint runs `test-approval-environment.py`, which evaluates each job's `environment:` expression from the workflow files against fork, same-repository, push and manual events, and fails if a fourth job names `pr-testing` without being listed
- note: `pr-testing` has no protection rules in this repository, so an outside pull request is held only once a required reviewer is set on it
- note: module versions. This fork publishes no core, framework or plugin module versions: their module paths stay `github.com/capsohq/bifrost/...`, their version files and the transport's requires are unchanged, and the workspace builds them from source
- preserved: everything v2.3.5 and earlier releases listed under Fork changes
- chore: the Helm chart is 2.1.52-sync.1, a prerelease; `appVersion` is 2.4.0-sync.1, so an empty `image.tag` selects `v2.4.0-sync.1`
- note: the CLI and Migration CLI are unchanged: v1.0.1 and v0.1.1 remain their stable releases
- note: the image and the OCI chart are private packages, so pulling them needs a token with `read:packages`
