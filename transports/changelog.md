## ✨ Features

- **Jev Complexity Routing** - The complexity router's primary classifier is the Typesafe Jev classifier, with the semantic classifier as fallback, decision cost tracking and per-tier criteria overrides in the dashboard (#7485, #7605, #7690)
- **MCP Server Instructions** - Upstream MCP servers' `instructions` are captured, forwarded on the MCP gateway and injected into LLM requests, with global and per-client length caps (#7516, #7517, #7518, #7519, #7766)
- **Multimodal Embeddings** - Embedding inputs are lists of content parts (text, image, audio, video, file, tokens) with optional per-item params, for Cohere, Gemini, Vertex, Bedrock Titan and Nova, and Parasail. Logs record the embedding input (#2559, #7001, #7002, #7096, #7459, #7550)
- **Model Retrieve** - `GET /v1/models/{model}` retrieves one model's metadata from providers that support it (#7419, #7716)
- **Web Search Pricing** - `tool_usage.web_search.num_requests` is priced by `web_search_cost_per_request`, which replaces `search_context_cost_per_query` (#7624, #7687)
- **Connector Raw Payload Export and Attribute Denylist** - Connectors can export raw payloads and drop attributes per connector (#7753, #7754)
- **Global Proxy and HTTP Client Factory** - One proxy configuration (with SCIM, inference and API toggles, also in config.json) routes provider, MCP, OAuth, webhook, catalog sync, plugin download and OpenTelemetry egress; `https://` proxies are reached over TLS and IPv6 proxies dial (#7567-#7576, #7799)
- **Vertex AWS Workload Identity** - Vertex keys can authenticate through AWS workload identity federation (#7705-#7709)
- **Typesafe Custom Providers** - `typesafe` is accepted as a custom provider base type (#7948, #7949)
- **Requested Route Headers** - `routing_info` and response headers report the provider and model the caller asked for before routing (#7683)
- **OpenTelemetry semconv 1.44** (#7810)

## 🐞 Fixed

- **Security Hardening** - Bounded request decompression (oversized zstd windows answer 413), passthrough paths validated and kept on the provider host, provider endpoint changes require authentication and are validated, OAuth discovery and MCP OAuth2 server settings validated, semantic cache entries scoped per virtual key, batch lookups bound to the creating key, MCP OAuth grants revoked on key rotation, code-mode tool discovery scoped to the request's grant, secret references in MCP client fields and private-network webhooks require an admin session, unlisted browser origins refused on the management API while auth is disabled, and code execution resources bounded (#5767-#5782, #7847-#7857, #7886-#7893, #7958-#7962)
- **Postgres at Scale** - Index and query fixes for large log, user and virtual key tables; transient errors retried in chunks; only pending migration steps run on startup (#7606, #7607, #7608, #7882, #7994)
- **Error Statuses** - Error metrics, spans and stream errors carry the effective HTTP status instead of "unknown" (#7451, #7611)
- **Providers** - Anthropic error types normalized and stop sequences preserved, Mistral streamed text arrays kept, Gemini schemaless function tools kept and tool results ordered by call id, Bedrock incomplete Converse streams error out, and a dead provider no longer blocks the aggregate model list (#7916, #7921, #7939, #7927, #7797, #6614)
- **Model Catalog** - Falls back to stored pricing when the datasheet URL hangs (#7737)

## 🗄️ Database Migrations

All are additive and idempotent by ID. `move_pricing_override_search_context_to_web_search` rewrites pricing-override rows that use the old key; `make_mcp_oauth_flows_state_unique` adds a unique index on OAuth flow state.

- Config store: `add_vertex_aws_workload_identity_column`, `add_mcp_discovered_instructions_column`, `add_mcp_instruction_cap_columns`, `add_mcp_client_max_instructions_length_column`, `add_mcp_client_require_public_target_column`, `add_web_search_cost_per_request_column`, `move_pricing_override_search_context_to_web_search`, `add_warp_additional_models_column`, `add_vk_provider_config_virtual_key_id_index`, `add_value_hash_and_token_hash_indexes`, `add_vk_mcp_configs_mcp_client_id_index`, `add_vk_provider_config_provider_and_key_indexes`, `add_virtual_keys_created_at_id_index`, `add_batch_jobs_due_index`, `make_mcp_oauth_flows_state_unique`, `add_mcp_code_mode_limits_client_column`
- Logs store: `logs_add_embedding_input_column`, `logs_add_owner_timestamp_indexes`, `logs_add_ranking_name_timestamp_indexes`

## 🍴 Fork changes (txehq/bifrost)

- chore: v2.3.0-sync.2 is a prerelease: v2.3.0-sync.1 merged with upstream's v2.3.0 development line up to `caf84c288` (168 more first-parent commits after `ec081ef5b`), not a release of upstream v2.3.0. The fork's module paths, custom providers, native Anthropic and xAI APIs, streaming and unary clients, release guards and every TXE fix through v2.3.0-sync.1 are kept. It is not tagged `latest`
- breaking (unchanged from v2.2.6): with a config store and dashboard auth off or no admin yet, every non-public `/api` call needs the setup token in `X-Bifrost-Setup-Token`, and `enforce_auth_on_inference` defaults to `true` for a fresh deployment unless set explicitly
- feat: the flat multimodal embedding list (`[{"type":"text",...},{"type":"image_url",...}]`) is still accepted, as one input item; Volcengine/ModelArk multimodal embeddings, `instructions` and `sparse_embedding` keep working on the new input shape
- fix: a proxied MCP hostname the gateway cannot resolve is still refused for every client (TXE-5086), including on upstream's new per-request proxy transport
- fix: native Anthropic and xAI Messages passthrough validate the path against the provider host like the other providers
- chore: GLM, Qwen, MiniMax, Moonshot and Volcengine/ModelArk report model retrieve as unsupported
- note: the `warp` feature flag now defaults to on (upstream #7757; it was off in v2.3.0-sync.1). Warp stays inert until an admin configures its chat and embedding models: no provider calls and no log indexing before that. Turn the flag off to hide it
