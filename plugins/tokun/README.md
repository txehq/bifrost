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

## Routes, once it is on

The plugin is an HTTP transport plugin, and its pre-auth hook runs on every route registered
with the inference middlewares (`RegisterInferenceRoutes` in
`transports/bifrost-http/server/server.go`). The hook decides by the route the request matched
(its method and route template, `HTTPRequest.Route`) before it reads the plan, so a refused
route never uses up the request's `send_id`. Every route has a class in `routes.go`:

| Class | Routes | Answer |
| --- | --- | --- |
| Needs a signed plan | The inference handler (`/v1/chat/completions`, `/v1/completions`, the HTTP `/v1/responses` routes, `/v1/embeddings`, `/v1/audio/*`, `/v1/images/*`, `/v1/videos/*`, `/v1/rerank`, `/v1/ocr`, `/v1/decisions`, `/v1/models`, `/v1/batches/*`, `/v1/files/*`, `/v1/containers/*`); `/v1/async/*` submissions and polls; the SDK routers under `/openai`, `/anthropic`, `/genai`, `/bedrock`, `/cohere`, `/litellm`, `/langchain`, `/pydanticai`, `/cursor`, `/typesafe`; the passthrough routers (`/*_passthrough/*`) | 401 when the plan does not hold; otherwise dispatched inside the plan |
| Not one of tokun's endpoint types | WebSocket Responses (the GET upgrades of `/v1/responses`, `/openai/responses`, `/openai/v1/responses`, `/openai/openai/responses`); live sessions and live control (`/v1/live/sessions`, `.../{session_id}/attach`, `.../{session_id}/content`, each also under `/openai` and `/openai/v1`); MCP (`/v1/mcp/tool/execute`, `/mcp`, `/mcp/{slug}`); the Agent Gateway (`/agents/a2a/{name}/json-rpc`, `/rest/*`, `/.well-known/agent-card.json` and `/push/callback`) | 404, code `tokun_route_not_offered` |
| The realtime conversation | `/v1/realtime` (WebSocket), `/v1/realtime/calls` (WebRTC), `/v1/realtime/client_secrets`, `/v1/realtime/sessions`, each also under `/openai` (and `/openai/openai` where Bifrost registers it) | 501, code `tokun_route_not_implemented` |
| Not in the table | A route Bifrost registers that `routes.go` does not list | 500, code `tokun_route_unclassified` |

The 404, 501 and 500 bodies are `{"error": {"type": "tokun_route_rejected", "code": "<code>",
"message": "<detail>"}}`; a refused plan's is the same with type `tokun_route_plan_rejected`.

Why each route is refused or not:

- **The routes that need a plan** are dispatched through Bifrost core on the request's context
  (`handleRequest`, `handleStreamRequest`; an async job's context carries the submission's
  context values), so `PreRequestHook` applies the plan: the request reaches only the upstreams
  and keys the plan lists, at most once per pod before the plan expires. Requests that call no
  upstream (a models listing, a file, batch or container operation, an async poll) need a plan
  too.
- **WebSocket Responses, live sessions, MCP and the Agent Gateway** are not endpoint types of
  tokun (the table of endpoint types in *Tokun 平台功能设计*), so no plan opens them. The
  WebSocket and live handlers also build the context of each request on the connection from the
  headers captured at the upgrade, which do not carry the plan, so a plan could not bind what
  such a connection dispatches.
- **The realtime conversation** is the endpoint type `audio.realtime.openai`, which batch 3
  implements (B3-10): a plan will be verified on the request that opens the connection, creates
  the call or creates the client secret, will bind what the connection dispatches, and the
  connection will not outlive the plan. Until then these routes answer 501, as the request
  types batch 3 adds do. The 501 is a placeholder (`TODO(tokun-B3)` in `routes.go`).
- **A route not in the table** is refused, so a route added to Bifrost is closed until it is
  classified. The server's `TestTokunRouteClassesCoverEveryInferenceRoute` registers the routes
  as the server does and fails for every registered route without a class and every listed
  route Bifrost no longer registers.

The management API (`/api/*`), `/health`, `/metrics` and the web UI are registered with the
API middlewares; the plugin does not see them.

A plan lists at least one key, and the body length it signs is the bytes actually sent (0 for
a GET).

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
