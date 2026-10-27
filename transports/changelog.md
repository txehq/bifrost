## ✨ Features

- **MCP servers from OpenAPI specs** - A new `openapi` MCP connection type synthesizes an in-process MCP server from an OpenAPI or Swagger document (Swagger 2.0, OpenAPI 3.0 to 3.2, JSON or YAML). Each supported operation becomes a prefixed MCP tool that proxies an HTTP call to the described API. The spec comes from an inline `spec`, a config.json-only `spec_file` or a `spec_url`; `openapi_config` also takes `base_url`, `security_credentials` per security scheme, `include_deprecated` and `max_response_bytes`. New endpoints `POST /api/mcp/openapi/preview` and `GET /api/mcp/client/{id}/openapi-spec`, and an OpenAPI connection type in the dashboard's New MCP Server sheet (#8307-#8315, #8481)
- **Azure Microsoft-Decision-1** - `/v1/decisions` works against Azure: the Azure provider posts to the Foundry resource's `/providers/microsoft/v1/systemone` endpoint with the Typesafe System One shapes, so the Complexity Router can use Azure as its decision classifier (#8485)
- **Bare `*` OAuth redirect URI** - `oauth2_server_config.allowed_redirect_uris` accepts a bare `"*"` to approve any remote https callback, with a startup warning; partial wildcards are rejected with 400. The dashboard gains an "Allowed redirect URIs" field (#8480)

## 🐞 Fixed

- **config.json OAuth redirect policy** - `allowed_redirect_uris` and `disable_vk_identity` now count toward the client config hash, so adding them to config.json on a deployment whose stored row already held the rest of `oauth2_server_config` takes effect instead of being masked by a matching hash (#8480)
- **MCP OAuth issuer** - The dashboard requires an issuer URL in OAuth and Both modes; the issuer is never derived from the `Host` header (#8484)
- **Concurrent index builds during migrations** - A Postgres migration that finds an INVALID index waits for a build another backend is still running instead of dropping it, which failed one side with "tuple concurrently updated" when a pod was killed mid-migration (#8479)
- **Anthropic tool search** - `tool_reference` blocks inside a client `tool_result` survive the Responses conversion on the Anthropic and Vertex routes, and are dropped before an OpenAI `function_call_output` (#7813)

## 🔒 Security

- OpenAPI tool calls use the MCP client's network policy: link-local and instance-metadata destinations are always refused, and a client registered by a caller that passed no credential check may only reach public addresses. A spec download by such a caller goes through the SSRF-safe client. A tool call does not follow a redirect to another host (fork: for every call, not only credentialed ones) or from https to plain http (#8307, #8308, #8310)
- `openapi_config_json` holds the upstream API credentials and is encrypted at rest when encryption is enabled; fork: so is the stored document (`openapi_spec`), which can carry a credential of its own such as a token in a server URL. Management API reads mask those credentials, leave out the spec document, and mask credentials embedded in `spec_url` and `base_url` (#8309, #8310)
- fork: an OpenAPI tool call is refused before any request when its operation-level server is at another origin (scheme, host, effective port) than the base URL, with or without credentials. An imported document can no longer point a call, or the credentials it carries, at a server the administrator never reviewed, private networks included. Operation servers no longer stand in for a missing base URL either: a document that declares servers only on its operations needs `openapi_config.base_url`, which pins every operation to one server. Same-origin operation servers are unchanged (TXE-5296)
- fork: a spec whose shared `$ref`s expand past 1,000,000 copied entries or 64 MiB of handed-out string data across the document is refused with 400 instead of being expanded in memory (a large shared default or enum would otherwise be copied into every tool's input schema); so is a spec whose server variables would expand a server URL past 8 KiB, or all its server URLs past 1 MiB, which is checked before the URL is built. Every per-operation copy is bounded: tool descriptions, preview summaries, security warnings and unsupported-operation reasons are capped where they are composed; each use of a $ref'd path item is charged against the document budget; path keys over 2 KiB, method names over 64 bytes, operationIds over 1 KiB and more than 5,000 operations refuse the document (identifiers are never truncated); the text all tools retain together is bounded too, and transport errors no longer carry URL userinfo to the model or the logs

## 🗄️ Migrations

- configstore `add_mcp_client_openapi_spec_column` and `add_mcp_client_openapi_config_json_column` add two nullable columns to `config_mcp_clients`. Both are additive and refuse to roll back, since the columns hold the only stored copy of a client's spec and credentials. No existing row is changed

## 🍴 Fork changes (txehq/bifrost)

- chore: v2.3.1-sync.2 is a prerelease: v2.3.1-sync.1 merged with upstream's v2.3.1 development line up to `72457fe69` (OpenAPI MCP and decisions), not a release of upstream v2.3.1. It is not tagged `latest`; v2.3.0 stays the stable release
- chore: the core, framework and plugin module paths and versions are unchanged, and no new core, framework or plugin version is published. The CLI version file follows upstream to 1.0.0 with an empty changelog; the fork publishes no CLI release (TXE-5293)
- preserved: the fork's module paths (`github.com/capsohq/bifrost/transports/v2`), custom providers, native Anthropic and xAI APIs (the xAI native Messages passthrough still forwards the body byte-for-byte apart from the provider-prefix model rewrite, TXE-5193), streaming and unary clients, setup-token lock, release and migration guards, and every TXE fix through v2.3.1-sync.1
- chore: the Helm chart is the prerelease 2.1.47-sync.2 with `appVersion` 2.3.1-sync.2; 2.1.46 stays the stable chart and `image.tag` stays required
