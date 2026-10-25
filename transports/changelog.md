## ✨ Features

- **Bifrost CLI Management and Inference Commands** - The `bifrost` CLI gains an authenticated gateway transport, named contexts, SSO login in the interactive launcher, OpenAPI-driven management commands with structured output, and inference commands (#7279, #7280, #7281, #7282, #7283, #7284)
- **MCP Connection Recovery** - A failed make-before-break re-dial keeps the live connection, a dead session triggers a reconnect instead of a re-probe of the same session, and tool calls recover from dead sessions. Clients in `pending_verification` keep that state through enable and health checks (#6921, #6922, #6923, #6924, #6926)
- **MCP Tool Refresh and Client Re-registration** - `POST /api/mcp/client/{id}/refresh-tools` rediscovers a client's tools on demand, and `POST /api/mcp/client/{id}/reregister` re-registers an OAuth client whose dynamic registration the provider evicted. The dashboard has matching actions (#6927, #6928, #7228, #7229)
- **Business-Unit Owned Virtual Keys** - A virtual key can be owned by a business unit (`business_unit_id`), alongside a team or a customer. Access profiles can be held by teams, business units and customers (#7251, #7253, #7255)
- **Virtual Key Content Logging Control** - `disable_content_logging` on a virtual key turns off content logging and strips exported content for that key's requests (#7476, #7477, #7478, #7479, #7480, #7481)
- **Virtual Key Auto-Deletion After Expiry** - `client.delete_expired_virtual_keys` and a per-key `delete_after_expire` let a daily job delete expired keys (#7525, #7526)
- **Connector Exports** - Observability connectors receive a typed LLM span payload, the cost breakdown, the app, raw payloads (opt-in) and `x-bf-dim-*` metric dimensions. Trace data is only produced when a connector reads it (#7367, #7368, #7371, #7389, #7390, #7391)
- **Pinned Keys on Routing Fallbacks** - A routing-rule fallback can pin a provider key (`key_id`, or `provider_key_name` in config.json). Unpinned fallbacks keep the legacy `provider/model` string (#7379, #7380, #7381)
- **Per-Attempt TTFT Cutoff** - A stream attempt that sends no first token within the cutoff is abandoned and handed to the next fallback. Routing rules take `ttft_timeout_ms` (#7503, #7504, #7505, #7506)
- **Failure Classes and Retry Hints** - Provider failures are classified so the key pool skips keys that failed permanently, at any `max_retries`, and provider retry hints reach callers as `retry_after_ms` (#7211, #7212)
- **Session-Grouped Logs** - The logs view can collapse sessions to their earliest chain root with per-session rollups (`roots_only`, `group_sessions`) (#7298, #7299, #7300)
- **Typesafe SDK Fidelity** - The native `/typesafe` routes accept SDK-valid `state: null`, forward native request extensions such as `images`, relay native success and error bodies, forward upstream response headers and serve the endpoint's live model catalog (#7652)
- **Warp (off by default)** - Warp, the natural-language assistant over logs, ships behind the `warp` feature flag, which stays off unless an operator enables it (#6212-#6221, #6847-#6852, #7418)
- **Model and Provider Selectors** - Unified provider and model selectors replace the ad-hoc selects across the dashboard; deprecated models sink below live ones in search (#7247, #7258, #7259, #7408, #7410)

## 🐞 Fixed

- **OAuth Client Columns Widened** - `state`, `scope` and `client_name` on OAuth2 client rows are `text`, so long provider values no longer fail to save (#7405)
- **Temp-Token 401 Codes** - A rejected temp token now answers `401` with `temp_token_auth_disabled` or `temp_token_rejected`. Scope is still checked against the raw request path (#7664)
- **Gemini and Vertex** - `n` maps to `candidateCount`, signed inline images survive Responses streams, and `$ref` tool schemas are accepted (#7394, #7357, #7713)
- **Bedrock** - Extended-thinking token counts and Anthropic extra params survive conversion, and cache-control blocks are no longer dropped (#7653, #7395, #7646)
- **Provider Key Partial Updates** - A partial PUT keeps the fields it omits (#7679)
- **Semantic Cache Families** - Cache entries are isolated by request family (#7561)

## 🗄️ Database Migrations

All are additive, idempotent by ID and safe for a rolling deploy; none rewrites rows except where noted.

- Config store: `add_virtual_key_delete_after_expire_column`, `add_client_config_delete_expired_virtual_keys_column`, `widen_oauth2_client_controlled_columns` (varchar to text on three OAuth2 client columns), `add_virtual_key_business_unit_column`, `add_warp_config_table`, `add_warp_api_key_id_column`, `add_warp_history_retention_days_column`, `add_warp_log_embedding_columns` (backfills NULL Warp strings to ''), `add_warp_temperature_reasoning_columns`, `add_virtual_key_disable_content_logging_column`, `add_ttft_timeout_ms_column_to_routing_targets`
- Logs store: `logs_add_warp_conversation_tables`, `logs_add_warp_conversations_updated_at_index`, `logs_add_warp_message_outcome_columns`

## 🍴 Fork changes (txehq/bifrost)

- chore: v2.3.0-sync.1 is a prerelease: the fork's v2.2.6 merged with upstream's v2.3.0 development line up to `ec081ef5b` (the first 173 first-parent commits after v2.2.6's base), not a release of upstream v2.3.0. Upstream v2.2.4 to v2.2.6 fixes, the fork's module paths, custom providers, native Anthropic and xAI APIs, streaming and unary clients, release guards and every TXE fix through v2.2.6 are kept. It is not tagged `latest`
- breaking (unchanged from v2.2.6): with a config store and dashboard auth off or no admin yet, every non-public `/api` call needs the setup token in `X-Bifrost-Setup-Token`, and `enforce_auth_on_inference` defaults to `true` for a fresh deployment unless set explicitly
- fix: the Typesafe systemone route relays the raw or native provider body only while its answers match the processed response, so a post-hook's change still reaches the client
- fix: test-only corrections of three upstream tests that fail or race on `ec081ef5b` (realtime retry-hint status, schema sync for the response-only `business_unit`, the Warp handler tests' fake job store)
