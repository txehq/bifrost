# tokun routing plugin

The tokun gateway sends each request to Bifrost with a signed route plan:
`tokun.routing.v1.RoutePlan` in the `tk-route-plan` header and its `RoutePlanSignature`
in `tk-route-plan-sig` (contracts in `txeproto/`, copied from txe's `tokun/proto`). The
plugin verifies the plan in `HTTPTransportPreAuthHook`, before authentication, and answers
401 when it does not hold. It then dispatches the request only to the upstreams and keys
the plan lists: the primary and the Bifrost fallbacks are the plan's entries in the order the
selection curve draws, each with its own key pinned. The design is in *Tokun Hot Path 重构*
(sections Bifrost, 路由计划, 签名路由计划, 选择曲线); the plan items are New Tokun B1-D4 to
B1-D8.

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
| Not one of tokun's endpoint types | WebSocket Responses (the GET upgrades of `/v1/responses`, `/openai/responses`, `/openai/v1/responses`, `/openai/openai/responses`); live sessions and live control (`/v1/live/sessions`, `.../{session_id}/attach`, `.../{session_id}/content`, each also under `/openai` and `/openai/v1`); MCP (`/v1/mcp/tool/execute`, `/mcp`, `/mcp/{slug}`); the Agent Gateway (`/agents/a2a/{name}/json-rpc`, `/rest/*`, `/.well-known/agent-card.json` and `/push/callback`); the Anthropic routers' wildcard `POST /v1/messages/{path:*}` under `/anthropic`, `/cursor`, `/langchain`, `/litellm` and `/pydanticai` | 404, code `tokun_route_not_offered` |
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
- **The `/v1/messages` wildcard** of the Anthropic routers handles any path under
  `/v1/messages` that has no route of its own as a message to create. Tokun's Messages endpoint
  type has two paths, `/v1/messages` and `/v1/messages/count_tokens`, each a route of its own
  under every Anthropic router, so the wildcard serves no endpoint type of tokun.
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

## The order of a send

`PreRequestHook` draws the order of the plan's `{provider, upstream, key}` entries by the
selection curve of *Tokun Hot Path 重构* (`drawOrder` in `route.go`, the curve in `curve.go`),
and the first entry is the primary, the rest Bifrost's fallbacks:

- **Groups**: the upstreams of the request's format (`same_format`) form the first group and the
  converted ones the second; each group is drawn on its own, and every entry of the first comes
  before any of the second. A provider with upstreams in both groups is drawn in each, with its
  upstreams of that group.
- **Layers**: in a group, the providers are drawn at the plan's `request_rate`; under each
  provider, its upstreams of the group are drawn at the rate the provider took (its final share
  of `request_rate`); under each upstream, its keys are drawn by the planner's shares.
- **A provider or upstream layer**: the candidates that score at least the layer's threshold
  (`threshold`, or the provider's `upstream_threshold`) are drawn and the others follow by
  score; when none does, all are ranked by score. The weight is `erfc(z / √2)` of how far a
  candidate trails the top in their uncertainty, the share `w / Σw`, the final share
  `φ + (1 − n × φ) × s` with `φ = min(1 / (λ × T), 1 / n)` and `T = stale_after_seconds`, then cut
  to `capacity / λ` with the excess going to the others in proportion to their shares, until
  none is over; when the capacities together cannot take λ, the shares are their proportions.
  A capacity of 0 is no limit. A provider's capacity is that of its upstreams in the group that
  take its requests: those at or above its upstream threshold, or the top-scored when none is;
  0 when one of those has no limit.
- **Keys**: the keys of a share above 0 are drawn by their part of the upstream's total; keys of
  share 0 follow in the plan's order.
- **The draw**: without replacement, by `u^(1/p)` for a uniform `u`, largest first (kept as
  `ln(u)/p`, which ranks the same). The numbers come from `math/rand/v2`'s global source, so
  every request draws its own; `SeedDraws` fixes a seed for tests.
- **What is recorded**: each entry keeps its final share in each layer and whether its provider
  or its upstream scored below its threshold, the fields of `RouteAttempts.order`. A share is the
  probability that the candidate comes first in its layer and group (1 for the first of a layer
  ranked by score, 0 for the others ranked by score), so in the first group the product of the
  three is the probability that the entry was drawn as the primary.

The curve has no coefficients of its own. `BenchmarkDrawOrder` measures it on a plan of 10
providers, 50 upstreams and 150 keys.

## Each attempt's request

The design is *Tokun Hot Path 重构*, section 请求和响应的改写. Each request enters the
integration of its format (`/openai/v1/chat/completions`, `/openai/v1/responses`,
`/anthropic/v1/messages` and its `count_tokens`, `/bedrock/...`, `/cohere/...`), which parses
it into Bifrost's request; the plan's entries may be of that format (`same_format`) or of any
other, which the target provider converts to. A verified plan asks the integration to keep the
caller's body on the request (`BifrostContextKeyKeepRawRequestBody`) and to skip a native
passthrough, which would send the request to one provider in its own format. Before each
attempt, `PreLLMHook` rewrites the request for the attempt's entry, always from the caller's
body as Bifrost received it:

- **`strip_params`**: the entry's parameter paths present in the body are removed, and listed in
  the response header `tokun-stripped-params` when that attempt answers.
- **An entry in the request's format** sends the body as it is
  (`BifrostContextKeyUseRawRequestBody`), changed in place only where it has to be: the
  top-level `model` becomes the entry's `forward_model`, and a streamed OpenAI chat or
  completions body gets `stream_options.include_usage`. Fields no format registers reach the
  upstream unchanged. The provider's own raw-body handling still applies (for example
  Anthropic's removes `max_tokens` from a token count).
- **An entry of another format** goes out from the typed request, which the provider converts;
  core sets its provider, model and key from the entry. When parameters were removed, the
  request is built again from the stripped body by the route's own converter
  (`BifrostContextKeyRequestBodyConverter`).
- **Output limits**: every attempt carries its entry's `interface_output_tokens`
  (`BifrostContextKeyAttemptOutputTokens`). The Anthropic Messages builders then send the
  caller's `max_tokens` and thinking budget as written, also above the model's known output
  ceiling, and a request that names no `max_tokens` gets `interface_output_tokens`; with none
  in the plan such an attempt fails. Without the key (no plan) they lower `max_tokens` to the
  ceiling, refit the thinking budget below it, and fill a missing one with the ceiling.
- **The usage the plugin asked for**: on a streamed OpenAI chat or completions request whose
  caller did not ask for usage, `HTTPTransportStreamChunkHook` drops the chunk that carries
  only usage and takes the usage off a chunk that carries output too.

A route that keeps no body (no body at all, a multipart upload, the large-payload path, the
inference handler outside the SDK integrations) sends every attempt from the typed request.

## Each attempt's request type

The design is *Tokun Hot Path 重构*, section tokun 现有的格式转换和去处 (the
`translateResponsesRequest` and `responsesEgress` rows). Every upstream of a plan carries
`request_type`, the Bifrost RequestType its interface takes for the request's operation and
interaction mode, which the gateway takes from the catalog (the interface's
`bifrost_request_type_override`, or its endpoint type's `bifrost_request_types`). Bifrost has no list
of its own:

- `PreRequestHook` sends each entry as its `request_type` and puts the types of the attempts it
  sends on the context, by attempt (`BifrostContextKeyRoutedRequestTypes`, indexed like
  `BifrostContextKeyFallbackIndex`). It leaves out an entry whose `request_type` is empty, its
  interface having no operation for the request (`operation_not_offered`), and one core cannot send
  the request as (`request_type_unconvertible`, `schemas.RequestTypeChange`): a type of another
  operation, a streamed request to a type that does not stream or the reverse, or another type on
  an entry in the request's format, whose body goes out as it is.
- The compat plugin, which runs before this one on every attempt, sets
  `BifrostContextKeyChangeRequestType` from the attempt's type when it differs from the request's
  own and clears it when it does not (`markRoutedConversion` in `plugins/compat/routed.go`), and
  makes none of the conversions it otherwise derives from this Bifrost's model catalog. Core then
  converts the attempt: a Responses request (`/openai/v1/responses`, and `/anthropic/v1/messages`,
  which Bifrost handles as Responses) to an upstream that only has chat completions goes out as a
  chat completion (`ToChatRequest`), and its answer comes back as a Responses response
  (`ToBifrostResponsesResponse`), or, streamed, as Responses events made chunk by chunk
  (`ToBifrostResponsesStreamResponse`); a chat completion to a Responses upstream goes the other way.
  Each attempt has its own conversion, so a plan may mix both kinds in either order.
- The usage: every field of the chat usage reaches the Responses usage (`ToResponsesResponseUsage`
  in `core/schemas/mux.go`): input, output and total tokens, their text, audio and image parts, cache
  reads, cache writes with their 5-minute and 1-hour parts, reasoning, predictions, citations, search
  queries, server-side tool calls, duration-based audio and the cost. A stream reports it in
  `response.completed`, from the usage chunks the OpenAI-compatible stream handler gathers.
- A provider whose chat stream does not make Responses events (Anthropic, Bedrock, Gemini, Cohere,
  whose own Responses translation needs no conversion) fails a streamed Responses attempt sent as a
  chat completion on its first chunk, `tokun_attempt_stream_unconverted`, before the caller
  receives anything, and core moves to the next entry.

## count_tokens

The design is the `CountTokens` row of the same section. A count_tokens request is sent to the
plan's entries whose `request_type` is `count_tokens`, through Bifrost's `count_tokens` of each
provider, and the upstream's count is the answer. The entries without the operation are left out
(`operation_not_offered`). When no entry of the plan has it, `PreLLMHook` answers with the local
count (`estimate.go`) and no upstream is called; when one has it but none can be sent from this
Bifrost, the request fails (503 `tokun_route_plan_unroutable`).

The local count reads the caller's body once, in the request's own format, without converting it,
with the settings of `RoutePlan.token_estimate` (txe's runtime settings `estimate.*`; there are no
coefficients in Bifrost):

- **Text**, by its UTF-8 bytes at `input_bytes_per_token`, rounded up: the system prompt, the text
  of every message, thinking and redacted thinking, the name and input of a tool call, the content of
  a tool result, a text document, a search result, every tool definition as its whole JSON, and a
  block of a type the count does not know as its whole JSON.
- **An image** given as base64 (JPEG, PNG, GIF, WebP), by its size: scaled down, keeping its aspect
  ratio, until its longer edge is at most `image_max_edge_pixels`, then one token for each square
  patch of `image_patch_pixels`, and at most `image_max_tokens`. The initial values (28, 2576, 4784)
  are Claude's high-resolution tier, and reproduce the examples of Anthropic's vision guide.
- **A PDF** given as base64, by its pages (its page tree, read with pdfcpu), `document_page_tokens`
  each: the page's text and the image of the page.
- `input_overhead_tokens` once, for what the upstream adds: role markers, the tool-use system
  prompt.

Like Anthropic's own count, it refuses with 400 `invalid_request_error` an image or a document
given by URL or file id, and one it cannot read. A plan without `token_estimate`, or with a setting
that is not positive, fails with 500 `tokun_token_estimate_invalid`. Only the Messages format is
counted locally; the token counts of the other formats (Responses `input_tokens`, Gemini
`countTokens`, Bedrock `count-tokens`, Cohere `tokenize`) answer 501
`tokun_token_estimate_not_implemented` until batch 3 (`TODO(tokun-B3)` in `estimate.go`).

`TestEstimateTextAgainstARealTokenizer` gives the count's distance from a real tokenizer,
OpenAI's o200k_base: at 3 bytes per token it is 0.83 to 1.65 times the tokenizer's count on the
kinds of text a coding agent sends, and 1.28 times on all of them together. A token count is not
billed: its answer, the upstream's or the local one, is `input_tokens` with no usage object.

## Each attempt, and its record

The design is *Tokun Hot Path 重构*, section 执行和记录 and its 尝试记录合同. Core tries the
entries in the drawn order, one attempt each (a provider entry's `max_retries` is 0), until one
answers, the entries sent run out, or the plan's `max_attempts` entries have been sent.

- **Any error moves to the next entry**: the next key of the same upstream, the next upstream,
  the next provider, whatever the error's scope. When the plan's `stop_scopes`
  (`routing.stop_scopes`, empty by default) lists the scope of an attempt's error, `PostLLMHook`
  sets that error's `AllowFallbacks` to false and core stops there (`shouldTryFallbacks` and
  `shouldContinueWithFallbacks` in `core/bifrost.go`).
- **Only before the first chunk**: core hands a stream to the transport once its first valid
  chunk has passed (`CheckFirstStreamChunkForError`, or `CheckStreamPreambleForError` for a
  stream that opens with events Bifrost holds back, in `executeRequestWithRetries`), and tries no
  other entry after that; a stream that breaks later ends with an error event to the caller.
- **The error scope**, by the upstream's status, and by Bifrost's failure classification
  (`ClassifyFailure` in `core/failureclass.go`, which reads the provider's error code, type and
  message) where the status does not decide:

  | Scope | Errors |
  | --- | --- |
  | `key` | 401, 403, 429; a rate limit, a rejected key, a model this key cannot reach, a location this key's account is refused |
  | `upstream` | 402 and a quota exhausted (also under a 429 or a 400); 5xx; a connection error; a timeout; a retired model; a failure inside Bifrost; a status Bifrost does not recognise (a 404 for the path) |
  | `request` | the upstream refused the request itself: invalid, over the context length, refused by moderation (400, 409, 413, 415, 422 with a provider error body) |
  | none | the caller cancelled |

  A status nothing recognises is `upstream`, not `request`: `request` is claimed only on the
  upstream's own refusal, since `stop_scopes` may stop on it.
- **The class** (`error_class`, healthobs' failure classes): `transport` for an attempt no
  response came back to, `stream_broken` for a stream that failed after its first chunk or event
  passed, `auth` for 401, 403 and a rejected key, `throttled` for 429 and a rate limit,
  `upstream_5xx` for 5xx, none for a `request` error (healthobs counts it as a rejected request,
  not a failure), `upstream_4xx` for the other 4xx.
- **The record**: `PreLLMHook` notes when each attempt starts and the send's upstream time so
  far; `PostLLMHook` notes how it ended: the upstream's status (200 for an answer, 0 when no
  response came back), the scope and class, the upstream's error code (its `code`, or its error
  type when it has none) and message, the duration, and the upstream time it added. An error
  Bifrost made itself (a connection error, a timeout, a failure inside Bifrost) carries no
  upstream code or message. A streamed attempt ends with its first chunk; its later chunks are
  not recorded.
- **`tk-route-attempts`**: `HTTPTransportResponseHeadersHook`, which runs just before the
  response headers are sent, so before a stream's first chunk reaches the caller, writes
  `tokun.routing.v1.RouteAttempts` as standard base64: `request_id`, `send_id`, `order` (every
  entry of the drawn order with its three shares, `below_threshold` and `same_format`, as the
  selection curve left them), and `attempts` in the order of their entries. Besides the attempts
  made, `attempts` lists the entries the plugin left out (with their `skip_reason`), and the
  entries core passed over without an attempt before a later one, which have no provider entry
  on this Bifrost (`provider_not_synced`). An attempt begun and not ended by then is written as
  one no response came back to. Nothing is recorded after the header is written.

The tests: `TestTokunPluginRoutesInsideTheSignedPlan` in `transports/bifrost-http/server`
(the `tk-route-attempts` subtests) sends signed plans through the whole Bifrost against upstream
stand-ins and decodes the header; `TestClassifyAttempt` and the `TestRouteAttempts*` tests here
cover each error and the record's encoding.

## What the plugin keeps

- **Request state**: the verified plan, the drawn order, the attempt chain with the request type
  of each attempt (also on the context for the compat plugin) and the record of each attempt, on
  the request's context (`requestStateKey`). It is bounded by the plan, which
  fits in one request header: one record of 136 bytes for each attempt made, at most one for
  each entry of the plan, the first kept in the state itself. It is freed with the request.
  It refers to the caller's body, the transport's own buffer, without copying it; an attempt in
  the request's format that changes the body makes its own copy, freed with the attempt.
- **Encoders of `tk-route-attempts`**, in a `sync.Pool`: the messages, the timestamps and the
  buffer one send's header is built in, so writing it allocates only the header's string. Each
  keeps the capacity of the largest send it encoded: 136 bytes for each entry of the order, 400
  for each attempt, and a buffer of 2.4 times the encoded size (the bytes and their base64); on
  a plan of 150 keys with three attempts, about 50 KiB, for a header of 16 KiB. It holds no
  string of a request once the header is written, and is freed when the garbage collector
  empties the pool; the pool holds about as many as headers are written at once.
- **Accepted `send_id`s**, so a signed plan is used once per pod. Keyed by `send_id`; each
  held send costs about 110 bytes of map and heap entry (measured over 100,000 sends,
  `BenchmarkSendIDLedgerClaim`), plus the 30-byte `send_id` string it keeps alive (a 32-byte
  allocation). An entry is removed by the first verification after its `expires_at`, so the
  ledger holds the sends accepted within the longest signed lifetime: a request's deadline,
  or an async task's. The map and the heap keep the capacity of the most sends they ever held
  at once.
