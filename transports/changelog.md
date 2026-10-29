## 🔒 Security

- **Go 1.27.3** - Security release for [CVE-2026-111943](https://groups.google.com/g/golang-announce/c/66pojueaQek) in `crypto/x509`: certificate chains through a small set of intermediates from a specific Certificate Authority are no longer accepted by `Verify`, which covers every TLS connection Bifrost makes to providers, MCP servers and other upstreams. Bifrost now builds with Go 1.27.3, and the Docker images (`Dockerfile`, `Dockerfile.local`, `Dockerfile.redhat`) move to `golang:1.27.3-alpine3.24`. Custom Go plugins must be rebuilt with Go 1.27.3 to load.

## 🗄️ Database Migrations

- No new database migrations in this release.

## 🍴 Fork changes (txehq/bifrost)

- chore: v2.3.2 is the fork's release of upstream transports/v2.3.2 (`a05ea20e6`) and the docs and changelog commits after it through `1123ea210`, merged onto v2.3.1. It supersedes v2.3.1 as the stable release. Upstream changed no Go source in this range
- note: module versions. Upstream's v2.3.2 bumps core to v1.13.1 and framework to v1.10.1 across every module. This fork publishes no core, framework or plugin module versions: their module paths stay `github.com/capsohq/bifrost/...`, their version files and the transport's requires are unchanged (core v1.11.2, framework v1.9.0 and the plugin versions v2.3.1 shipped), the workspace builds them from source, and their changelogs stay empty. Every module, `go.work` and the release jobs now declare Go 1.27.3
- preserved: everything v2.3.1 listed under Fork changes, including the non-dropping `drop_agent_registration_options` migration (`tenant` and `extension_uris` keep their values), the OpenAPI MCP hardening, the xAI native Messages byte-exact passthrough, and every TXE fix through v2.3.1
- chore: the Helm chart is 2.1.48, the stable chart for this release; `appVersion` is 2.3.2 and `image.tag` stays required
