## 🍴 Fork changes (txehq/bifrost)

- chore: v2.4.0-sync.14 is a prerelease on top of v2.4.0-sync.12 with no upstream commits taken. It carries the fork PRs merged since v2.4.0-sync.12 (`git log --first-parent transports/v2.4.0-sync.12..<release commit>`): the eleven that v2.4.0-sync.13 was cut for, and the six merged after it. v2.4.0-sync.13 has no tag and no image: its release run (44223574926) failed in `plugins-release` before anything was tagged, so this release is the first to carry those eleven. v2.3.5 stays the stable release and `latest`. The Helm chart is 2.1.52-sync.14.

### Security
- fork: **TXE-7705** (#381) on Vertex and Bedrock, and on a custom provider built on either, a virtual key with no `allowed_storage_prefixes` names no storage and one with no `allowed_pubsub_topics` names no Pub/Sub topic. The cloud identity behind a shared provider key is no longer reachable through a key that has no list.
- fork: **TXE-7783** (#388) management reads show every address with a credential in its userinfo masked as `redacted:redacted`: provider `base_url` and `workspace_base_url`, the Azure key alias `endpoint`, `oauth_key_config.token_url`, and the MCP OAuth and token-exchange URLs. A save that echoes the masked URL keeps the stored one; a masked URL that matches nothing stored is refused with 400 naming the field.
- test: **TXE-7780** (#387) a test walks every management read path and fails on any secret field it returns unredacted, including a field nobody has listed; a second test fails on a new `GET /api/...` route that is neither covered nor classified.

### Providers and video
- fork: **TXE-7809** (#389) a BytePlus video create's `seconds` follows the model's Seedance series: the 2.5 series (2.5 and 2.5 Flash) takes 4 to 30, the 2.0 series 4 to 15, and a model in no listed series (an Ark endpoint ID) 2 to 30; `-1` is always taken. A Seedance 2.5 video edit takes only `-1`. `omni_reference_task_type` is a task-type hint and never a price field.
- fork: **TXE 7827** (#390) a BytePlus or Volcengine video create may leave out the prompt when it carries a first or last frame, reference images, a reference video, reference audio or a `draft_task_id`; it reaches Ark with no text item. Audio with no reference image or video on a Seedance 2.0 series model, and a create with none of these inputs, are refused with 400 `caller_invalid_request` before Ark. Every other provider refuses a create with no prompt, `input_reference` or `video_uri` with 400 `caller_invalid_request` (`error.param` `prompt`) before a key is chosen.

### Gateway stability
- fork: **TXE-7696** (#380) a batch create with more inline lines than its vendor's per-batch maximum is refused with 400 before any line is converted, on Anthropic, OpenAI, Azure, Gemini, Vertex, xAI and Bedrock (custom providers over them included). File-based creates are unchanged.

### Billing and pricing
- fork: **TXE-7763** (#384) known vendor promotions on rates the fork bills are recorded with their last day, and the standard rate is billed from that day. The first switch is on 21 Nov 2026 (UTC): `databricks/system.ai.gpt-5-6-sol` and the priority and long-context rates of `databricks/databricks-gpt-5-6-sol` move to OpenAI's standard rates. Argon follows on 30 Nov, and the Gemini 3.6, 3.7 and 3.8 Flash rows on 31 Dec. A gateway bills the switch only when it runs a build that contains it.
- fork: **TXE-7688** (#382) `databricks/databricks-gemini-3-pro` input and output are billed at the Databricks list rate (2.49998e-06 and 1.500002e-05), not the promotional rate the upstream datasheet carries.
- test: **TXE-7687** (#370) a test fails when an upstream Databricks DBU rate has no matching USD rate, which would bill it at $0.

### UI
- fork: **TXE-7746** (#383) a configured OTel or Prometheus connector can be deleted from the observability page, after a confirmation that says what stops; deleting the Prometheus connector stops `/metrics` and the Push Gateway push.
- fork: **TXE-7744** (#379) the OTel and Prometheus views lose the delete props and tests nothing passed (replaced by #383's wiring).

### Breaking changes
- breaking: **TXE-7705** on Vertex and Bedrock, a virtual key with no `allowed_storage_prefixes` may no longer name storage (`gs://`/`s3://` inputs and their https forms, output locations, batch input files and output folders, file operations' `storage_config`), and one with no `allowed_pubsub_topics` may no longer name a Pub/Sub topic: the request is refused with 400 `caller_unsupported_parameter` (`error.param` `storage_location` or `pubsub_topic`). A key with a list works as before. Give each key that names storage or topics on those providers a list. (This entry is the governance plugin's; the fork keeps module changelogs empty, see CI below.)
- migrations: none. `framework/configstore/migrations.go` and `framework/logstore/migrations.go` are unchanged since v2.4.0-sync.12; the only configstore change is the comment on `TableVirtualKey`'s two list fields. v2.4.0-sync.12 runs on this release's schema and a rollback needs no schema step.

### CI and tests (no runtime change)
- ci: **TXE-7895** (#391) `bifrost-http-binary` is kept 7 days, the approval window `docs/release.md` states, and `test-release-gates.py` fails when an artifact downloaded behind an environment gate is kept for less.
- ci: **TXE-7907** (#392) a plugin is released only when the core, framework and plugin modules its release pins are tagged or released by the same run; any other plugin is left out with a notice, and the gateway still ships its code from the workspace.
- ci: this release PR empties `plugins/governance/changelog.md`, which made v2.4.0-sync.13's run try a governance plugin release that the fork cannot publish (its module versions are not on proxy.golang.org), and adds `check-module-changelogs-empty.py` to Workflow Lint: a pull request that leaves an entry in core's, framework's or a plugin's changelog fails, since nothing in the fork publishes those changelogs and a core or framework entry starts a module release.
- ci: **TXE-7762** (#385) the release load test runs the previous release on the same runner, compares the two, and records `load-performance.json`; the comparison warns and does not gate.
- test: **TXE-7736** (#374), **TXE-7735** (#375), **TXE-7737** (#377): the LLM logs, MCP logs and six more UI e2e tests run offline and leave the skip list.
- test: each ticket ships unit tests in the packages it changes, and provider-harness folders validated offline (641 to 644). No live provider run is part of this release (`--skip-core-test`).
- preserved: everything v2.4.0-sync.12 and earlier releases listed under Fork changes
- note: module versions. This fork publishes no core, framework or plugin module versions: their module paths stay `github.com/capsohq/bifrost/...`, their version files and the transport's requires are unchanged, and the workspace builds them from source
- chore: the Helm chart is 2.1.52-sync.14, a prerelease; `appVersion` is 2.4.0-sync.14, so an empty `image.tag` selects `v2.4.0-sync.14`. Its templates and values are unchanged from 2.1.52-sync.12. Chart 2.1.52-sync.13 was published, but the image it selects, `v2.4.0-sync.13`, does not exist; install 2.1.52-sync.14
- note: the CLI and Migration CLI are unchanged: v1.0.1 and v0.1.1 remain their stable releases
- note: the image and the OCI chart are private packages, so pulling them needs a token with `read:packages`
