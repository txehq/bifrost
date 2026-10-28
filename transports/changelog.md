## ✨ Features

- **News feeds** - the notification tray has a News tab fed by RSS and Atom feeds (Bifrost, OpenAI, Anthropic, Gemini and Cursor built in); admins toggle built-in feeds and add custom ones through `/api/rss/feeds`, `/api/rss/items` and `/api/rss/settings` (#8537, #8538, #8539, #8540)
- **Code mode scripting** - the Starlark sandbox adds the `json` and `math` modules, a failed nested tool call reaches the script as an error value, `executeToolCode` replies carry only what the script prints and returns, and runtime errors include a traceback (#8603, #8604, #8605, #8606, #8607, #8692, #8693)
- **Secret references in extra headers** - `network_config.extra_headers` values accept `env.` and `vault.` references; values are masked in API responses and restored on save (#8618, #8619, #8620)
- **Vertex session routing** - Vertex keys accept `use_session_routing: true`, which sends the prompt cache key as `sessionId` on Gemini calls; off by default (#8650)
- **Guardrail output monitor mode** - a rule with `output_mode: "monitor"` delivers the response untouched and records matches in the background; the log row is held until the deferred result lands (#8641, #8644)
- **Observability** - spans and metrics carry the stored prompt a request ran, MCP spans carry the client name and `bifrost.mcp.tool.is_error`, the inbound W3C `tracestate` is captured, and A2A operations export bodies, stream updates and `a2a.client.operation.duration` to OTel, Maxim and telemetry; gRPC ingress keeps caller request and trace IDs (#8575, #8681, #8682, #8683)
- **Datadog** - `otel_llmobs_conversion` and `llmobs_gateway_mode` plugin settings (#8679)
- **Calendar date presets** - Today, This week and This month in the date filter (#8592); Typesafe image assets by reference and box questions (#8571); cluster monitoring view (#8613); Mistral streamed thinking blocks map to reasoning (#8567, #8612)

## 🐞 Fixed

- A2A push relay no longer leaks one goroutine every 3 seconds, and calendar-aligned budgets and rate limits honour the duration multiplier (`7d` no longer resets daily) (#8596)
- Agent Gateway OAuth caches are bounded at 5,000 entries; ClickHouse A2A cleanup and correlation stop issuing needless mutations or loading payload columns (#8608, #8617, #8657)
- Pooled keep-alive connections release request-body buffers above `server.body_buffer_pool_limit_bytes` (default 4 MiB) after each request; async jobs copy the raw body (#8602). A body above `max_request_body_size_mb` returns `413 request_too_large` (#8700)
- Postgres migrations wait for a live concurrent index build, logstore upgrade migrations no longer stall behind old replicas, and the Helm chart adds a `startupProbe` (#8572)
- Bedrock: automatic caching creates a cache point, tool-message `cache_control` emits a standalone `cachePoint`, Converse keeps `stopReason` and `additionalModelResponseFields`, explicit prompt cache mode reaches gpt-5.6, and InvokeModel excludes Converse-only fields (#8573, #8595, #8649, #8704, #8721)
- Responses: `configuration_update` items keep their reasoning effort, assistant messages with an id keep their status, empty tool output is passed as `""`, and converted chat streams keep id, model and created (#8599, #8621, #8634, #8699)
- `reasoning_effort: "xhigh"` reaches vLLM, Ollama, SGL and custom OpenAI-compatible providers (#6244); Gemini and Vertex batch results and SGLang count reasoning tokens (#8600, #8687)
- Azure deployments route forwards unknown chat fields (#8703); decisions gate custom OpenAI-base providers on allowed requests and recover stringified answers (#8626, #8705); zero-key custom providers no longer claim every model in routing (#8706)
- Provider key `enabled` from config.json persists without re-syncing UI-edited keys (#8610, #8647); MCP library "Installed" is computed server-side (#8581); dashboard throughput counts only inference requests (#8579)
- Passthrough log keys no longer show "(deleted)" and passthrough streams without a `Content-Type` are no longer labelled `text/plain` (#8601, #8638, #8689, #8715); SQLite busy/locked log batches retry whole (#8707)

## 🗄️ Migrations

- configstore: `add_vertex_use_session_routing_column`, `add_rss_feeds_table` (seeds the built-in feeds) and `add_rss_feeds_icon_url_column` are additive.
- configstore: `drop_agent_registration_options` is recorded but drops nothing in this fork; see Fork changes.

## 🍴 Fork changes (txehq/bifrost)

- chore: v2.3.1-sync.4 is a prerelease: v2.3.1-sync.3 merged with upstream's v2.3.1 line up to `8f0d3f51f` (the v2.3.1 merge, #8718). It is not tagged `latest`; v2.3.0 stays the stable release
- chore: the core, framework and plugin module paths and versions are unchanged, and no new core, framework or plugin version is published; upstream's module changelog entries are folded into this file and the module changelogs stay empty
- preserved: config data. Upstream's `drop_agent_registration_options` drops `config_agent_registrations.tenant` and `.extension_uris` and cannot roll back. In this fork the migration keeps both columns and their values: the code no longer reads or writes them (Agent Gateway discovers extensions from the agent card, and registration tenant was unused), both are nullable, and a rollback to an earlier fork release still finds them. The migration fixtures keep asserting those values on Postgres and SQLite. Removing the columns later needs a new migration ID
- preserved: the fork's module paths (`github.com/capsohq/bifrost/transports/v2`), custom providers, native Anthropic and xAI APIs (byte-exact xAI passthrough apart from the provider-prefix model rewrite), the OpenAPI MCP hardening, the hosted tool-search queue lifecycle, parsed passthrough visibility, streaming and unary clients, setup-token lock, release and migration guards, and every TXE fix through v2.3.1-sync.3
- chore: the Helm chart is the prerelease 2.1.47-sync.4 with `appVersion` 2.3.1-sync.4; 2.1.46 stays the stable chart and `image.tag` stays required
