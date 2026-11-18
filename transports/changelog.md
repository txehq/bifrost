## 🍴 Fork changes (txehq/bifrost)

- chore: v2.4.0-sync.13 is a prerelease on top of v2.4.0-sync.12 with no upstream commits taken. It carries the fork PRs merged since v2.4.0-sync.12 (`git log --first-parent transports/v2.4.0-sync.12..<release commit>`). v2.3.5 stays the stable release and `latest`. The Helm chart is 2.1.52-sync.13.

### Security
- fork: **TXE-7705** (#381) on Vertex and Bedrock, and on a custom provider built on either, a virtual key with no `allowed_storage_prefixes` names no storage and one with no `allowed_pubsub_topics` names no Pub/Sub topic. The cloud identity behind a shared provider key is no longer reachable through a key that has no list.

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
- breaking: **TXE-7705** on Vertex and Bedrock, a virtual key with no `allowed_storage_prefixes` may no longer name storage (`gs://`/`s3://` inputs and their https forms, output locations, batch input files and output folders, file operations' `storage_config`), and one with no `allowed_pubsub_topics` may no longer name a Pub/Sub topic: the request is refused with 400 `caller_unsupported_parameter` (`error.param` `storage_location` or `pubsub_topic`). A key with a list works as before. Give each key that names storage or topics on those providers a list. (From `plugins/governance/changelog.md`.)
- migrations: none. `framework/configstore/migrations.go` and `framework/logstore/migrations.go` are unchanged since v2.4.0-sync.12; the only configstore change is the comment on `TableVirtualKey`'s two list fields. v2.4.0-sync.12 runs on this release's schema and a rollback needs no schema step.

### CI and tests (no runtime change)
- ci: **TXE-7762** (#385) the release load test runs the previous release on the same runner, compares the two, and records `load-performance.json`; the comparison warns and does not gate.
- test: **TXE-7736** (#374), **TXE-7735** (#375), **TXE-7737** (#377): the LLM logs, MCP logs and six more UI e2e tests run offline and leave the skip list.
- test: each ticket ships unit tests in the packages it changes. No live provider run is part of this release (`--skip-core-test`).
- preserved: everything v2.4.0-sync.12 and earlier releases listed under Fork changes
- note: module versions. This fork publishes no core, framework or plugin module versions: their module paths stay `github.com/capsohq/bifrost/...`, their version files and the transport's requires are unchanged, and the workspace builds them from source
- chore: the Helm chart is 2.1.52-sync.13, a prerelease; `appVersion` is 2.4.0-sync.13, so an empty `image.tag` selects `v2.4.0-sync.13`. Its templates and values are unchanged from 2.1.52-sync.12
- note: the CLI and Migration CLI are unchanged: v1.0.1 and v0.1.1 remain their stable releases
- note: the image and the OCI chart are private packages, so pulling them needs a token with `read:packages`
