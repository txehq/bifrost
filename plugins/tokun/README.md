# tokun routing plugin

The tokun gateway sends each request to Bifrost with a signed route plan:
`tokun.routing.v1.RoutePlan` in the `tk-route-plan` header and its `RoutePlanSignature`
in `tk-route-plan-sig` (contracts in `txeproto/`, copied from txe's `tokun/proto`). The
plugin verifies the plan in `HTTPTransportPreAuthHook`, before authentication, and answers
401 when it does not hold. It then dispatches the request only to the upstreams and keys
the plan lists: the primary and the Bifrost fallbacks are the plan's entries in order, each
with its own key pinned. The design is in *Tokun Hot Path 重构* (sections Bifrost, 路由计划,
签名路由计划); the plan items are New Tokun B1-D4 to B1-D8.

## Turning it on

The plugin is built in and loaded only when Bifrost's `config.json` has an enabled entry:

```json
{"name": "tokun", "enabled": true, "config": {"signing_keys": "env.TOKUN_ROUTE_PLAN_SIGNING_KEYS"}}
```

`signing_keys` is a secret reference (`env.<NAME>` or `vault.<path>`) whose value is a JSON
object of signing key id to secret, for example
`{"gw-2026-11": "…", "tw-2026-11": "…", "hp-2026-11": "…"}`. The HMAC key is the UTF-8 bytes
of a secret. Every listed id verifies, so a rotation adds the new id beside the old one in
the Secret and removes the old one once every signer has switched. An enabled entry whose
keys do not resolve stops the server.

Without the entry, or with `"enabled": false`, the plugin is not loaded and no request
reaches it.

## Routes that need a signed plan once it is on

The plugin is an HTTP transport plugin, and the pre-auth phase runs on every route
registered with the inference middlewares (`RegisterInferenceRoutes` in
`transports/bifrost-http/server/server.go`). Once the plugin is loaded, every request on
these routes needs a valid plan, or it is answered 401 with
`{"error": {"type": "tokun_route_plan_rejected", "code": "<reason>", ...}}`:

| Routes | Handler |
| --- | --- |
| `/v1/chat/completions`, `/v1/completions`, `/v1/responses` and its `compact`, `input_tokens`, `{response_id}`, `cancel` and `input_items` routes, `/v1/embeddings`, `/v1/audio/speech`, `/v1/audio/transcriptions`, `/v1/images/*`, `/v1/videos/*`, `/v1/rerank`, `/v1/ocr`, `/v1/decisions`, `/v1/models`, `/v1/models/{model}`, `/v1/batches/*`, `/v1/files/*`, `/v1/containers/*` | inference handler |
| `/v1/async/*`, the submissions and the `{job_id}` polls | async handler |
| `/openai/*`, `/anthropic/*`, `/genai/*`, `/bedrock/*`, `/cohere/*`, `/litellm/*`, `/langchain/*`, `/pydanticai/*`, `/cursor/*`, `/typesafe/*` | SDK integration routers |
| `/openai_passthrough/*`, `/anthropic_passthrough/*`, `/azure_passthrough/*`, `/bedrock_passthrough/*`, `/runware_passthrough/*`, `/genai_passthrough/*`, `/chatgpt_passthrough/*` | passthrough routers |
| WebSocket `/v1/responses` and `/openai/v1/responses` | WebSocket Responses |
| `/v1/realtime`, `/v1/realtime/calls`, `/v1/realtime/client_secrets`, `/v1/realtime/sessions`, each also under `/openai` | realtime (WebSocket, WebRTC, client secrets) |
| `/v1/live/sessions`, `/v1/live/sessions/{session_id}/attach`, `/v1/live/sessions/{session_id}/content`, each also under `/openai` | live sessions and live control |
| `/v1/mcp/tool/execute`, `/mcp`, `/mcp/{slug}` | MCP tool execution and Bifrost's MCP server |
| `/agents/a2a/{name}` with its `/json-rpc`, `/rest/*` and `/.well-known/agent-card.json` routes, and `/agents/a2a/{name}/push/callback` | Agent Gateway |

The management API (`/api/*`), `/health`, `/metrics` and the web UI are registered with the
API middlewares and are not affected.

Every request on these routes needs a plan, including the ones that call no upstream: a
models listing, a file, batch or container operation, an async poll, an MCP call, an Agent
Gateway call. A plan lists at least one key, and the body length it signs is the bytes
actually sent (0 for a GET). MCP clients of `/mcp`, Agent Gateway clients and the upstream
agents that post to the push callback cannot sign a plan, so with the plugin on the Bifrost
deployment serves tokun's gateway, task-worker and health probe only.

Whether the plan then decides where the request goes depends on the route:

| Routes | The plan decides the upstream and key |
| --- | --- |
| Inference handler, SDK integration routers, passthrough routers | Yes. The request runs through `handleRequest` or `handleStreamRequest` on the request's context, and `PreRequestHook` sets the primary and the fallbacks from the plan. |
| `/v1/async/*` submissions | Yes, when the job runs: the job's context carries the request's context values (`AsyncJobExecutor.SubmitJob`), and the job runs through `handleRequest`. The plan's `expires_at` is checked at the submission only. |
| WebSocket `/v1/responses`, `/v1/realtime` over WebSocket, `/v1/live/sessions` (WebSocket and WebRTC) and its `attach` route | No. The plan is verified on the request that opens the connection or session, but the handler builds the context of every request it makes from the captured headers (`createBifrostContextFromAuth`), which do not carry the plugin's state: the caller's provider and model and Bifrost's key selection decide. |
| `/v1/realtime/calls` (WebRTC) | No. `PreRequestHook` runs on the call's context, but the handler keeps the caller's provider and model and does not read the routing back. |
| `/v1/realtime/client_secrets`, `/v1/realtime/sessions` | No. The handler selects the provider key itself (`SelectKeyForProviderRequestType`); no plugin hook runs. |
| MCP, Agent Gateway, live control | No upstream provider is called with a provider key. |

On the routes the plan does not decide, a verified plan opens a connection or makes a call
outside the upstreams and keys it lists, and a connection may outlive the plan's
`expires_at`. Tokun's gateway does not use these routes in batch 1. Realtime routing is part
of plan item B3-10; the WebSocket Responses route and live sessions have no plan item yet.

## What the plugin keeps

- **Request state**: the verified plan, the drawn order and the attempt chain, on the
  request's context (`requestStateKey`). It is bounded by the plan, which fits in one request
  header, and is freed with the request.
- **Accepted `send_id`s**, so a signed plan is used once per pod. Keyed by `send_id`; each
  held send costs about 110 bytes of map and heap entry (measured over 100,000 sends,
  `BenchmarkSendIDLedgerClaim`), plus the 30-byte `send_id` string it keeps alive (a 32-byte
  allocation). An entry is removed by the first verification after its `expires_at`, so the
  ledger holds the sends accepted within the longest signed lifetime: a request's deadline,
  or an async task's. The map and the heap keep the capacity of the most sends they ever held
  at once.
