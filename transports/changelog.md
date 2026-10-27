## ✨ Features

- **Virtual key metadata** - `governance.virtual_keys[].metadata` (config.json, API and dashboard) accepts string key/value pairs such as `cost_center` or `owner`; each log entry snapshots its key's metadata, logs and spend filter on `metadata_<key>=<value>`, and the key's values override `x-bf-lh-*` headers and `x-bf-dim-*` dimensions so callers cannot spoof attribution (#7934, #7935, #7936, #7937)
- **Gateway tool search** - deferred tools on models with no tool search of their own (Gemini, older GPT, Bedrock non-Claude) are held back behind a `bifrost_tool_search` function that Bifrost resolves in the gateway, on non-streaming and streaming Responses requests; OpenAI gpt-5.4+ (and Azure/Bedrock GPT) get the `tool_search` OpenAI requires, and OpenAI tool search items render for `/anthropic` clients (#8502, #8503, #8504, #8505)
- **Extra fields as headers** - `client.extra_fields_placement: "headers"` removes `extra_fields` from inference bodies and sends its scalar fields as `x-bifrost-*` response headers; the default stays `body` (#8476, #8477, #8478)
- **Parsed passthrough logs** - passthrough calls are logged as parsed conversations under their inferred request type, including streamed responses, non-text bodies and multipart uploads (form summary) (#8516 to #8525)
- **Parquet log payload offload** - S3 log payload offload can write Parquet (#8475)
- **Databricks `ai_decide`** - the Databricks provider serves `/v1/decisions` natively (#8498)
- **`cache_control` on chat tool_calls** - reaches the Anthropic and Bedrock `tool_use` block (#8494); OpenRouter clamps markers to four for Claude (#8512)
- Antigravity user agent recognised in logs (#8543)

## 🐞 Fixed

- Anthropic-compatible requests keep a client `tool_use` named `web_search`/`web_fetch` as a function call (#8370); Claude tool search replays its server blocks on `/v1/responses` and Bedrock InvokeModel (#8555)
- Bedrock Claude keeps `disable_parallel_tool_use` (#8471); Responses `instructions` merge ahead of developer input (#8515); chat-to-Responses tool results use Responses input block types (#8514)
- Responses events no longer carry an invalid `item`, and `output` is always an array (#8563); a Gemini blocked prompt reports failed instead of an empty success (#8564)
- Model catalog per-1M token prices no longer lose decimals (#8326); keyless model discovery runs despite leftover keys, which can now be deleted (#8530); config.json MCP clients keep their derived `endpoint_slug` on re-sync (#8535)
- OAuth refresh rejections are classified by error code, so Slack (200 `ok:false`) and Atlassian (403 `unauthorized_client`) failures ask for re-auth instead of retrying (#8559)
- Postgres startup index builds on small tables use a plain build that does not wait on an unrelated long transaction's snapshot; cross-cluster materialized-view refresh coordination (#8501)

## 🔒 Security

- Agent Gateway outbound requests (card discovery, OAuth, JSON-RPC, REST, gRPC) enforce destination permissions: link-local and cloud metadata destinations are always refused, private destinations need `client.a2a_private_allowlist`, and credentials only go to the card origin or `agents[].credential_origins` (#8552)
- Agent Gateway endpoints and Skills Repository marketplaces require an explicitly configured public URL (`client.a2a_external_client_url`, new `client.skills_external_client_url`); request headers no longer build agent-card endpoints or marketplace clone URLs, and URLs with userinfo, query or fragment are refused (#8526)

## 🗄️ Migrations

- configstore: `add_client_config_extra_fields_placement_column`, `add_skills_external_client_url_column`, `backfill_skills_external_client_url` (copies a credential-free `mcp_external_client_url`), `add_virtual_key_metadata_column` (nullable `metadata` on `governance_virtual_keys`) and `add_agent_destination_policy_columns` (`credential_origins`, `require_public_target` on `config_agent_registrations`). All are additive; existing rows are kept, and the metadata and destination-policy migrations refuse to roll back.
- logstore: `logs_add_matview_refresh_state_table` and `logs_add_is_passthrough_column`, both additive.

## 🍴 Fork changes (txehq/bifrost)

- chore: v2.3.1-sync.3 is a prerelease: v2.3.1-sync.2 merged with upstream's v2.3.1 development line up to `14e3b5653`, not a release of upstream v2.3.1. It is not tagged `latest`; v2.3.0 stays the stable release
- chore: the core, framework and plugin module paths and versions are unchanged, and no new core, framework or plugin version is published; upstream's module changelog entries are folded into this file and the module changelogs stay empty. Go is 1.27.2 across the go.mod files, go.work, the release prep guard and the builder images
- preserved: the fork's module paths (`github.com/capsohq/bifrost/transports/v2`), custom providers, native Anthropic and xAI APIs (the xAI native Messages passthrough still forwards the body byte-for-byte apart from the provider-prefix model rewrite, TXE-5193; the shared native Messages usage parser still serves Anthropic passthrough usage), the OpenAPI MCP hardening of sync.2, streaming and unary clients, setup-token lock, release and migration guards, the fork's `from_memory`-ignoring virtual key list, and every TXE fix through v2.3.1-sync.2
- fix: release tests the merged range left red: two upstream handler/server tests (nil lib logger; a protocol-route fixture without a2a_external_client_url), a data race in the marketplace git test, and the migration fixtures now cover `config_mcp_clients.openapi_spec`/`openapi_config_json`, which v2.3.1-sync.2 added
- test: the logstore and configstore Postgres tests run in their own databases; upstream's migration pause holds the database-scoped matview refresh lock, so packages sharing a database stalled each other's self-heal, refresh-record and upgrade tests. A new test pins self-heal under a held lock: reads stay correct through the raw fallback and the view is rebuilt after the pause
- chore: the Helm chart is the prerelease 2.1.47-sync.3 with `appVersion` 2.3.1-sync.3; 2.1.46 stays the stable chart and `image.tag` stays required
