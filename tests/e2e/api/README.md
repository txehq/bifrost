# E2E API tests (Newman / Postman)

End-to-end API tests for the Bifrost API using Postman collections and [Newman](https://www.npmjs.com/package/newman) (CLI).

## Provider secret redaction

The API management collection's `Provider Secret Redaction` folder covers every
standard provider, including each provider's credential and alias fields. It uses
disabled keys and a synthetic canary, with no inference requests. The script tests
also compare the fixtures against the Go provider registry and secret-field definitions.

The gateway process must have this exact environment variable set before startup:

```bash
export BIFROST_SECRET_REDACTION_CANARY=synthetic-secretvar-harness-canary-0123456789
```

Run the focused checks against your test gateway:

```bash
node tests/e2e/api/collections/collection-scripts.test.mjs
newman run tests/e2e/api/collections/bifrost-api-management.postman_collection.json \
  --folder "Provider Secret Redaction" \
  --env-var base_url=http://localhost:8080 \
  --timeout-script 120000
```

For an authenticated gateway, also pass `--env-var "admin_auth_header=$ADMIN_AUTH_HEADER"`.
The folder forwards this header to its setup, assertion, and cleanup requests.
The normal API runner runs both auth modes; the release API integration job and
its local wrapper supply the canary before starting the gateway.

The script tests above and `runners/newman-exit-when-done.test.mjs` need no gateway:
`.github/workflows/scripts/test-api-collections-offline.sh` runs both on every pull
request (`.github/workflows/api-collections-offline.yml`) and first in the release's
API integration job, so a provider added to `StandardProviders` without its row fails
the PR, not the release run (TXE-6055).

Each case creates a uniquely named disabled key, checks create/get/list/update/delete
responses, and verifies cleanup. Existing keys and providers are preserved; providers
created by a case are deleted afterward. Missing fields, missing references, unset or
mismatched canaries, unexpected HTTP statuses, and failed cleanup fail the test.
Use only the documented synthetic canary, never a real credential.

## Azure streaming preamble fallback

These two deterministic cases use a local SSE fixture and an isolated gateway.
They make no live provider calls. Run the following commands from the repository root.

Start the fixture in one terminal:

```bash
node tests/e2e/api/runners/azure-stream-preamble-fixture.mjs
```

Start a separate gateway in another terminal:

```bash
azure_fixture_dir=$(mktemp -d)
cp tests/e2e/api/provider_config/azure-stream-preamble.config.json "$azure_fixture_dir/config.json"
go run ./transports/bifrost-http -app-dir "$azure_fixture_dir" -host 127.0.0.1 -port 8790
```

Run the cases in a third terminal. The harness is stored one file per folder in
`collections/provider-harness/` (see `AGENTS.md`, "Every wire-visible change ships with a
provider-harness case"); `build` writes the one file newman takes, to `tmp/provider-harness.json`,
which git ignores. Run it again after editing a folder file. Every `newman run` in this
README reads that copy.

```bash
node tests/e2e/api/runners/provider-harness-folders.mjs build
newman run tmp/provider-harness.json \
  --folder "74. Azure streaming preamble fallback" \
  --env-var baseUrl=http://127.0.0.1:8790 \
  --env-var azureStreamPreambleFixture=1
```

The fixture emits metadata followed by an error for `preamble-error`, then returns
`hello` for the configured fallback, `preamble-success`. Azure paths (`/openai/v1/*`)
fail with a rate limit; direct OpenAI paths (`/v1/*`) fail with `server_is_overloaded`
and no HTTP status, as a real overloaded OpenAI stream does. For both providers, Chat
Completions and Responses must return only the successful attempt's events and a
terminal result.
The cases are skipped unless `azureStreamPreambleFixture=1`.

Stop the fixture and isolated gateway with Ctrl+C after testing.

## Fixed-length response truncation

Folder 196 pins Content-Length validation through both native and OpenAI SDK chat streaming
routes. The local fixture sends valid SSE with `finish_reason: "stop"`, declares 100 extra
bytes, then closes without a `Connection: close` header. The gateway must emit a structured
`unexpected EOF` error. A control case with the exact body length must complete normally.
The connection-disposal regression is covered by the Go transport tests. These cases make
no live provider calls and are skipped unless `providerErrorFixture=1`.

Run from the repository root. Check that ports 8788 (the fixture's media tunnel), 8790, 8791 and 8799 (the fixture's stall proxy) are free before starting:

```bash
lsof -nP -iTCP:8788 -iTCP:8790 -iTCP:8791 -iTCP:8799 -sTCP:LISTEN
```

To run the fixture elsewhere, start it with `PROVIDER_ERROR_FIXTURE_PORT` and
`PROVIDER_ERROR_FIXTURE_STALL_PORT`, point the gateway config's fixture URLs at those ports, and pass
`--env-var providerErrorFixtureUrl=http://127.0.0.1:<port>` and
`--env-var providerErrorFixtureStallProxyUrl=http://127.0.0.1:<stall port>`. The harness declares
`http://127.0.0.1:8791` and `:8799` once, as those two collection variables' defaults. A row reaches the
fixture only through them (`{{providerErrorFixtureUrl}}` or `pm.variables.get('providerErrorFixtureUrl')`).
`collection-scripts.test.mjs` fails on a literal fixture address in any collection's scripts, request URLs,
bodies or headers.

Start the fixture in one terminal:

```bash
node tests/e2e/api/runners/provider-error-fixture.mjs
```

Prepare a fresh isolated profile and start the gateway in another terminal:

```bash
mkdir -p tmp
TASK_CONTENT_LENGTH_APP_DIR=$(mktemp -d "$PWD/tmp/content-length-truncation.XXXXXX")
cp tests/e2e/api/provider_config/provider-error-fixture.config.json "$TASK_CONTENT_LENGTH_APP_DIR/config.json"
printf '%s\n' "$TASK_CONTENT_LENGTH_APP_DIR" > tmp/content-length-truncation-app-dir
printf '%s\n' '{"values":[{"key":"providerErrorFixture","value":"1","enabled":true}]}' > tmp/content-length-truncation.env.json
make dev PORT=8790 APP_DIR="$TASK_CONTENT_LENGTH_APP_DIR"
```

The isolated profile obtains pricing metadata from the fixture and uses dummy provider
keys. It does not require the backing services used by the normal integration profile.
After `/health` responds, run the three cases in a third terminal:

```bash
make run-provider-harness-test PROVIDER=openai FEATURE="content-length truncation" BASE_URL=http://localhost:8790 ENV_FILE=tmp/content-length-truncation.env.json COMPAT=off DB_VERIFY=0 SKIP_STREAM_CANCEL=1
```

Stop the fixture and isolated gateway with Ctrl+C after testing.

## Video list filters and streamed content

Folder 405 pins contract C7 BF15 and C9 BF20 (TXE-6587) on `GET /v1/videos`, `GET /v1/videos/{id}/content`
and `GET /openai/v1/videos/{id}/content`. It needs the fixture and an isolated gateway on
`provider_config/provider-error-fixture.config.json`, whose `fixture_video_keys` provider has two keys, and
makes no live provider call. The fixture's `GET /v1/videos` is an OpenAI-shaped list recorded in two pages;
its `GET /v1/videos/video_gapped_*/content` sends a 512 KiB file in two halves 1.5 seconds apart; and it
counts each call under the caller's bearer token (`video.list@<token>?<query>`, `video.content@<token>`).
The list rows name key b, send a repeated `status` filter and follow `last_id` to the last page. The content
rows call the fixture's `POST /__probe/download`, which fetches the file from the gateway itself and reports
when the first byte arrived against when the fixture wrote the second half: a gateway that holds the file
fails with both times in the message. The last row reads `GET /__hits`: one vendor call per request, with the
named key and the filters unchanged. Run it as the content-length truncation folder above is run, with
`FEATURE="video content stream"`.

## Vendor-named video files through the media fetcher

Folder 482 pins TXE-7036 on `GET /v1/videos/{id}/content` for BytePlus and MiniMax, whose finished task
names its file by URL. The gateway fetches such a URL through the SSRF-hardened media fetcher, which refuses
a loopback address, so the fixture cannot name the file at its own address. It runs a media tunnel on port
8788 (`PROVIDER_ERROR_FIXTURE_MEDIA_TUNNEL_PORT`): an HTTP proxy that answers every CONNECT and joins the
tunnel to the fixture itself, counting each one as `__media_tunnel_connects@<host:port>` in `/__hits`. The
`byteplus` and `minimax` providers of `provider_config/provider-error-fixture.config.json` have it as their
`proxy_config`, so their API calls cross it too. The fixture's task `fixture-media-gapped` names its file at
`http://203.0.113.10` (TEST-NET-3: public to the check, routed nowhere) and the file arrives in two halves
1.5 seconds apart; the content rows use the download probe of folder 405. The task `fixture-media-loopback`
names a file on the fixture's loopback address, which the gateway must refuse without asking for it. The
last row reads `GET /__hits`. Run it as the content-length truncation folder above is run, with
`FEATURE="media fetcher stream"`; when the fixture is not on its default ports, point the two providers'
`proxy_config` at the tunnel's port as well.

## Addresses a vendor's response names

Folder 490 pins TXE-7090. A vendor's answer names addresses the gateway then requests: Replicate's `urls.get`
(the poll) and `urls.stream`, the `next` link of a deployments page, and the URL of an output image that the
GenAI route fetches to inline. The gateway uses the provider's client only for an address on the origin of the
provider's configured base URL; any other goes out on the SSRF-hardened client, which refuses a loopback or
private address and reaches a public one through the provider's proxy.

The fixture's `replicate` provider therefore has the media tunnel as `proxy_config` and two extra keys,
`fixture-deploy-origin` and `fixture-deploy-loopback`, that use the deployments endpoint. A model's last path
segment picks the case: `poll-origin`, `stream-origin` and `picture-public` name the address on the fixture's
own origin (the picture at `http://203.0.113.10`, reached through the tunnel); `poll-loopback`,
`stream-loopback` and `picture-loopback` name it on another loopback origin (the fixture by the host name
`localhost`, or its loopback address for the picture). The rows expect the answer for the first kind and a
refusal naming a non-public address for the second, and the last row reads `GET /__hits`: no request arrived
at `localhost` or for the loopback picture. Run it as the content-length truncation folder above is run, with
`FEATURE="vendor named address"`.

HuggingFace speech has no row. Its model lookup goes to a fixed `https://huggingface.co`, not to the
provider's base URL, so an offline gateway cannot get as far as the audio URL.

## Streamed frames timing

Folder 308 pins contract C10 BF22 (TXE-6522) on chat (`stream: true`) and speech (`stream_format: "sse"`),
each on `/v1/...` and `/openai/v1/...`. The error rows send `upstream-503` and `upstream-429`, which the
fixture answers with that status before any frame; the gateway must answer with the same status and a JSON
error, never a 200 stream carrying an error frame. The gap rows send `upstream-gapped`, which writes three
marker frames 600ms apart. A timing check needs a client that sees each frame arrive, and newman only sees
the finished body, so those rows call the fixture's `POST /__probe/stream-gaps`: the fixture sends the
request to the gateway itself and reports when each SSE event arrived, measured on the same clock it used to
write the frames. Each marker must reach the client before the fixture writes the next one, and the gaps
must survive. A gateway that buffers fails with the send and arrival times in the message.

Rows 9-13 pin BF21: `upstream-usage-extended` reports web search calls (`tool_usage`), a `cost` and xAI's
`cost_in_usd_ticks`, and the non-streamed chat response, the final frame of a chat stream (both routes), of a
text-completion stream (`/v1/completions`) and `response.completed` of a Responses stream served by the chat
fallback (the config's `parasail` entry points at the fixture) must all carry them. Against the accumulator
before TXE-6522 the four streamed rows fail and the non-streamed one passes.

Start the fixture and the isolated gateway as for content-length truncation above (ports 8790, 8791 and
8799), then:

```bash
make run-provider-harness-test PROVIDER=openai FEATURE="streamed frames timing" BASE_URL=http://localhost:8790 ENV_FILE=tmp/content-length-truncation.env.json COMPAT=off DB_VERIFY=0 SKIP_STREAM_CANCEL=1
```

Set `providerErrorFixtureUrl` when the fixture is not on `http://127.0.0.1:8791`. The probe passes the
harness's `admin_auth_header` to the gateway when one is set.

## A GET after pooled connections went stale

Folder 471 pins TXE-7071: a GET the gateway sends to a provider must not fail because the provider closed
the connections the gateway kept in its pool. A GET may be resent, but the resend gives up after three dead
connections, and a pool holds more than that after any burst (startup model discovery leaves four for a
provider with two keys). The row makes no live provider call.

A harness request cannot put a known number of dead connections in the gateway's pool, so the row calls the
fixture's `POST /__probe/stale-get`. The fixture sends six concurrent `GET /v1/videos/video_stale_hold` task
queries through the gateway and holds them until all six have arrived, so the gateway opens six connections.
It answers them, closes each connection 50 ms later without a `Connection: close` header, and then sends one
`GET /v1/videos/video_stale_after`. That last query must answer 200 and reach the fixture once.

Start the fixture and the isolated gateway as for content-length truncation above (ports 8790, 8791 and
8799), then:

```bash
make run-provider-harness-test PROVIDER=openai FEATURE="stale connection get" BASE_URL=http://localhost:8790 ENV_FILE=tmp/content-length-truncation.env.json COMPAT=off DB_VERIFY=0 SKIP_STREAM_CANCEL=1
```

Set `providerErrorFixtureUrl` when the fixture is not on `http://127.0.0.1:8791`. The probe passes the
harness's `admin_auth_header` to the gateway when one is set.

## Perplexity search usage

Folder 333 pins TXE-6549 (contract C5, C10, C6) for Perplexity chat. The config's `perplexity` entry points at
the fixture, which answers `/chat/completions` (Perplexity's path has no `/v1`) in Perplexity's own usage
shape: `search_context_size`, top-level `citation_tokens` and `num_search_queries`, a flat `cost`, and
`tool_calls_details.<tool>.invocation`. Streamed, every frame carries a usage and the frame with the finish
reason carries the full one. `sonar-search-usage` gets that answer; rows 1-3 assert the unified usage
(`search_context_size`, `tool_usage.web_search`, `tool_usage.per_tool`, the token details and the cost) on the
non-streamed response and on the final frame of a stream on `/v1/chat/completions` and
`/openai/v1/chat/completions`; rows 4-5 assert it on `/v1/responses`, which serves Sonar models from chat.
`sonar-invalid-model` gets Perplexity's 400, whose `error.code` is a number; rows 6-7 assert the status is
kept, streamed or not, with `extra_fields.error_type` `caller_invalid_request`.

Folder 548 pins TXE-6987 (contract C5, C10) for Perplexity's native `/v1/responses` (the Agent API), which
serves every model outside the Sonar family. The fixture answers `/v1/responses` for `agent-usage` in the
published `ResponsesUsage` shape: `tool_calls_details.<tool>.invocation` and a `cost` object in US dollars.
Rows 1-3 assert `tool_usage.per_tool`, `tool_usage.web_search`, the cost total and
`vendor_cost {amount: total_cost, unit: usd}` on the non-streamed response and on `response.completed` of a
stream on `/v1/responses` and `/openai/v1/responses`. Rows 4-6 assert `vendor_cost` on the chat routes for
`sonar-search-usage`. The usage shape is from Perplexity's published schema; no recorded reply was available.

```bash
make run-provider-harness-test PROVIDER=perplexity FEATURE="perplexity vendor cost" BASE_URL=http://localhost:8790 ENV_FILE=tmp/content-length-truncation.env.json COMPAT=off DB_VERIFY=0 SKIP_STREAM_CANCEL=1
```

Start the fixture and the isolated gateway as for content-length truncation above, then:

```bash
make run-provider-harness-test PROVIDER=perplexity FEATURE="perplexity search usage" BASE_URL=http://localhost:8790 ENV_FILE=tmp/content-length-truncation.env.json COMPAT=off DB_VERIFY=0 SKIP_STREAM_CANCEL=1
```

## xAI vendor cost

Folder 401 pins contract C5/C10 for TXE-6967. The config's `xai` provider points at the fixture's
`/pp/xai`, where `upstream-usage-extended` reports `cost_in_usd_ticks: 123000000` on chat and Responses.
Usage must carry `vendor_cost: {amount: 123000000, unit: "usd_ticks"}` next to the unchanged
`cost_in_usd_ticks`: non-streamed, on the final usage frame of a chat stream, and on `response.completed` of
a Responses stream, each on `/v1/...` and `/openai/v1/...`. Against a gateway without TXE-6967 only the
`vendor_cost` assertion fails, on all six rows. Start the fixture and the isolated gateway as for content-length
truncation above, then:

```bash
make run-provider-harness-test PROVIDER=xai FEATURE="xai vendor cost" BASE_URL=http://localhost:8790 ENV_FILE=tmp/content-length-truncation.env.json COMPAT=off DB_VERIFY=0 SKIP_STREAM_CANCEL=1
```

## Offline runs and egress

A fixture run must not reach a real vendor (TXE-7083). Dummy keys are not enough: a provider that
ignores `network_config.base_url` still dials its vendor. Bedrock builds its host from the region, so a
TXE-7080 fixture run sent two requests to `bedrock-runtime.us-east-1.amazonaws.com`. So a fixture run
starts all three processes (the fixture, the gateway and the harness) under `scripts/test-offline.sh`:

- Bifrost's dial guard refuses a TCP connection to an address that is not loopback before it is
  attempted, naming it: `offline run (BIFROST_TEST_OFFLINE=1): egress to <host> (<ip:port>) refused`.
  It covers Bifrost's own dialers (provider, proxy, SSRF, fasthttp, WebSocket, gRPC), `http.DefaultTransport`
  and the AWS SDK client. Libraries that dial on their own are held only by the macOS sandbox
  (`AGENTS.md`, "Run local tests through `scripts/test-offline.sh`").
- The runner removes the proxy variables, and on macOS it runs the command under `sandbox-exec` with a
  loopback-only profile, so any process it starts (node, newman, curl) is held to loopback too.
- `make run-provider-harness-test` refuses a fixture run (`providerErrorFixture=1`) outside the runner. It
  also fails closed unless the gateway's `egress_probe` provider (base_url on TEST-NET-3) comes back with
  the guard's refusal. So a gateway not started under the runner is refused, and so is one whose config
  lacks the probe or that asks for authentication (set `BIFROST_E2E_AUTH_HEADER`). Every fixture config
  with providers carries `egress_probe`, and `runners/lib/base-url-support.test.mjs` checks it.
- The AWS SDK's own HTTP client (Bedrock and Vertex credential loading: STS, IMDS, SSO) carries the
  guard too. `core/providers/utils` fails on a `config.LoadDefaultConfig` call without it.

`core/providers/base_url_support.json` says how to point each provider at a fixture. Most honour
`network_config.base_url`. Azure takes `azure_key_config.endpoint`, and Databricks takes
`databricks_key_config.workspace_url`. Bedrock and Bedrock Mantle take `bedrock_key_config.endpoints` and
`bedrock_mantle_key_config.endpoints`. Vertex has no override, so it stays out of fixture configs.
`runners/lib/base-url-support.test.mjs` (offline suites) fails when a registered provider is missing from
the table, or when a fixture config sets base_url on a provider that ignores it.

Bedrock always dials `https://`, so the fixture also serves its routes on a TLS listener
(`PROVIDER_ERROR_FIXTURE_TLS_PORT`, default 8793). The certificate is a throwaway self-signed one made with
`openssl` at start-up. The fixture config's `bedrock` provider sets
`bedrock_key_config.endpoints.runtime: "127.0.0.1:8793"` and `network_config.insecure_skip_verify: true`.
The fixture answers `POST /model/<id>/converse`, and it does not check SigV4 signatures.

Folder 476 pins this. Row 1 is Bedrock through the endpoint override. Rows 2 and 3 point Bedrock and a
base_url provider at TEST-NET-3 (`203.0.113.10`, never routed), and an offline gateway must refuse both,
naming the address. With random ports (substitute them into a copy of the config, including 8793):

```bash
PROVIDER_ERROR_FIXTURE_PORT=$FX PROVIDER_ERROR_FIXTURE_STALL_PORT=$ST PROVIDER_ERROR_FIXTURE_TLS_PORT=$TLS \
  scripts/test-offline.sh node tests/e2e/api/runners/provider-error-fixture.mjs
scripts/test-offline.sh make dev PORT=$GW APP_DIR="$APP_DIR"     # config.json from provider-error-fixture.config.json
scripts/test-offline.sh make run-provider-harness-test FEATURE="offline egress" BASE_URL=http://127.0.0.1:$GW \
  APP_DIR="$APP_DIR" ENV_FILE=tmp/content-length-truncation.env.json COMPAT=off DB_VERIFY=0 SKIP_STREAM_CANCEL=1 PARALLEL=0
```

## Client config round trip

Folder 457 pins TXE-7070: the `client_config` that `GET /api/config` returns saves back unchanged through
`PUT /api/config`. The fixture config's `client` section does not set `log_retention_days`, so the gateway
prunes logs at the documented default of 365 days. Rows 1 and 3 read the config from memory and from the store
(`?from_db=true`) and expect `log_retention_days: 365`, where the read used to return 0. Rows 2 and 4 send each
object back as it was read and expect 200, where the save used to refuse it with 400
`log_retention_days must be at least 1`. The rows call only the gateway's management API.

Start the fixture and the isolated gateway as for content-length truncation above, then:

```bash
node tests/e2e/api/runners/provider-harness-folders.mjs build
newman run tmp/provider-harness.json \
  --folder "457. A client config read from /api/config saves back unchanged, and log_retention_days reads as the retention applied (config round trip)" \
  --env-var baseUrl=http://127.0.0.1:8790 \
  --env-var providerErrorFixture=1
```

## Provider 5xx failover and 429 retry-hint propagation

These deterministic cases use a local HTTP fixture and an isolated gateway. They make no live
provider calls. Run the following commands from the repository root.

Start the fixture in one terminal:

```bash
node tests/e2e/api/runners/provider-error-fixture.mjs
```

Start a separate gateway in another terminal:

```bash
error_fixture_dir=$(mktemp -d)
cp tests/e2e/api/provider_config/provider-error-fixture.config.json "$error_fixture_dir/config.json"
go run ./transports/bifrost-http -app-dir "$error_fixture_dir" -host 127.0.0.1 -port 8790
```

Run the cases in a third terminal:

```bash
newman run tmp/provider-harness.json \
  --folder "154. Provider 5xx failover and 429 retry-hint propagation (provider error fixture)" \
  --env-var baseUrl=http://127.0.0.1:8790 \
  --env-var providerErrorFixture=1
```

Folder 336 (OpenAI-compatible chat fields and the encrypted-reasoning retry switch, TXE-6527) runs on the
same fixture and gateway:

```bash
newman run tmp/provider-harness.json \
  --folder "336. OpenAI-compatible chat sends price-affecting fields as sent or refuses them, and the encrypted-reasoning retry has a switch (compatible chat fields)" \
  --env-var baseUrl=http://127.0.0.1:8790 \
  --env-var providerErrorFixture=1
```

Folder 487 (a custom provider is served with its own keys, TXE-7097) runs on the same gateway. The config has
one custom provider on each of the deepseek, glm, moonshot and qwen bases (`own_deepseek`, `own_glm`,
`own_moonshot`, `own_qwen`), each with its own key and `base_url` on the fixture's `/pp/ownkey`. That route
keeps the bearer of each call in `GET /__wire`, so a row asserts that the key that reached the custom
provider's `base_url` is the custom provider's own, not the `deepseek` or `moonshot` provider's.

Its `moonshot` and `deepseek` providers point at the fixture's `/pp/oc`, which records each request body
(`GET /__wire`), so a row reads back the `max_completion_tokens`, prompt-cache fields and `service_tier` the
vendor received. `oc-tiered` has a capability record listing `default` and `priority`, so `flex` is refused
before any upstream call. `azure-content-filter` answers with Azure OpenAI's documented blocked-prompt 400.
Rows 336.9 to 336.12 save `client.disable_encrypted_reasoning_retry` through `PUT /api/config` with the
config's setup token and turn it off again; if the run stops between them, restart the gateway.

Folder 427 (the compat plugin never drops or rewrites a price-affecting field, TXE-7026) runs on the same
fixture and gateway:

```bash
newman run tmp/provider-harness.json \
  --folder "427. The compat plugin never drops or rewrites a price-affecting field (compat price fields)" \
  --env-var baseUrl=http://127.0.0.1:8790 \
  --env-var providerErrorFixture=1
```

The gateway config sets no compat option, so the plugin's parameter drop is on. `oc-capped` (moonshot chat)
and `cp-capped` (the openai-based custom provider `cp_plain`, Responses) have capability records with a
64-token output limit and no parameter list, so a row reads back that the output cap, `service_tier`,
`prompt_cache_key` and reasoning reached the vendor as sent while `seed` or `top_p` was dropped.

Folder 493 (a price-affecting field nested inside an extra param is refused, TXE-7124) runs on the same
fixture and gateway. Extra params are merged into the converted body object by object, so an extra param
named after a body object could add a price field inside it: `parameters.durationSeconds` on a Veo video,
`voice_setting.speed` on MiniMax speech. `schemas.NestedPriceFields` lists those paths per provider, and the
request worker refuses one with 400 `caller_unsupported_parameter` before a key is chosen. Seven rows are
refusals on Gemini, MiniMax, Replicate and BytePlus, each reading `GET /__wire` to show that nothing reached
the vendor; the eighth sends a nested field that is not a price field and reads it back from the wire.
Bedrock, ElevenLabs, Vertex and Google Cloud Text-to-Speech have no row: core's
`TestNestedPriceFieldsAreDeclaredAndRefused` covers every provider.

Folder 432 (service tier and output cap on Responses, Perplexity, Hugging Face and Bedrock Converse,
TXE-6994) runs on the same fixture and gateway, except its last two rows:

```bash
newman run tmp/provider-harness.json \
  --folder "432. Responses, Perplexity and Hugging Face refuse a service_tier they cannot deliver; Responses and Bedrock Converse send the output cap as sent (responses price fields)" \
  --env-var baseUrl=http://127.0.0.1:8790 \
  --env-var providerErrorFixture=1
```

`cp-tiered` on the openai-based custom provider `cp_plain` has a capability record listing `default` and
`priority`, so `flex` is refused before any upstream call, streamed and non-streamed, and a row reads back
the `service_tier` and `max_output_tokens: 5` the vendor received on `/pp/cp/v1/responses`. Rows 432.6 to
432.11 send `service_tier` on Perplexity and Hugging Face chat (`huggingface` points at the fixture's
`/pp/hf`): `priority` is refused before any upstream call and `default` is served. Rows 432.12 and 432.13
run on the Bedrock endpoint fixture and its gateway instead (see below, `--env-var bedrockEndpointFixture=1`
and `baseUrl` on that gateway): the fixture answers Converse, and its `GET /__connects` shows
`inferenceConfig.maxTokens: 5` as sent for an OpenAI-family model.

The fixture answers by model name: `upstream-500` returns HTTP 500, `upstream-503` returns HTTP 503,
`upstream-429` returns 429 with `Retry-After: 7`, `upstream-429-ms` returns 429 with
`retry-after-ms: 2500`, `upstream-ok` returns a chat completion, and `upstream-slow` returns the
same completion after two seconds. A request with `"stream": true` that a model answers with a
completion gets it as server-sent chunks ending in `data: [DONE]`. Bifrost does not emit a
`Retry-After` HTTP header; it carries the provider hint in the error body as
`extra_fields.retry_after_ms`, which the cases assert. The fixture also counts hits per model
(`GET /__hits`, `POST /__reset`), so a failover case can prove the primary was tried once and the
fallback once. A model may carry a `~tag` suffix (`upstream-500~run-1-a1`): the part before `~`
picks the behaviour and the hit is counted under the full name, so a case counts its own attempts.
`test-key` is the only credential it accepts: a request whose `Authorization` header carries any
other bearer is refused with 401, as a provider refuses a caller's direct key.
The same gateway config also registers an Azure key; the fixture answers model `azure-filtered` on
`/openai/v1/chat/completions` with Azure `prompt_filter_results` and per-choice `content_filter_results`,
which is how the dropped-annotation defect was measured.
The cases are skipped unless `providerErrorFixture=1`.

The same fixture stands in for AssemblyAI's Sync API (folder 404): the config's `assemblyai` provider points at
`/pp/assemblyai`, and `POST /pp/assemblyai/v1/transcribe` answers by the `X-AAI-Model` header, a transcript for
`universal-3-5-pro` and an RFC 9457 problem body for `aai-upstream-429` and `aai-upstream-400`. Each call is
counted under `assemblyai:<model>`, and `GET /__assemblyai` returns what each call carried (the model header, the
`Authorization` value and the multipart parts), which is how the cases prove one request made one upstream call.

The same fixture stands in for a provider that records uploads under a storage URI (folder 483, TXE-7032): the
config's `storage_uri_files` provider (OpenAI-shaped) points at `/pp/storage`, whose `POST /v1/files` answers with a
`gs://bifrost-fixture-bucket/uploads/<n>.jsonl` id, unique per upload, and whose chat completes. Vertex and Bedrock
record uploads in that form and neither host can be pointed at a fixture, so the folder pins the gateway's ownership
check for the id form (two virtual keys, the second refused with 404 on the first's URI on chat, Responses and image
edit), not those providers' own converters. The same folder covers the other names of a stored object and the places
a request writes (TXE-7105): the upload named by its https URL, and a video task's `output_storage_uri` and a batch's
`output_folder.url` that name it, each refused with 404 before the fixture is called. Folder 494 (TXE-7119) uses the
same provider for the task rule: `/pp/storage` also serves a batch create, retrieve and list, and its list always holds
one batch no gateway request created (`batch_storage_made_elsewhere`). A batch created under one virtual key is reached
by that key only; the batch made elsewhere is neither retrieved, cancelled nor listed for a virtual key, while a request
with no virtual key is forwarded for it. Folder 496 (TXE-7088) uses it for a virtual key's storage and Pub/Sub topic
allowlists: a key limited to one folder is served inside it and refused outside it, as an input, by an https URL, as
a task's or a batch's output location and for another topic, while a key with no lists is not restricted. Its rows use the management API, so run the augmented collection with
`--env-var setupToken=<the config's setup_token>`.
Folder 497 (TXE-7121) uses it for the folder rule: a key's first batch into an output folder claims the folder, and a
second key is refused with 404 when it names a result file there as an input, or writes a batch or a task's output into
that folder, a folder inside it or the folder that holds it, while a sibling folder is free. The folder names carry a
per-run suffix, because a claim outlives the key that made it. It needs the same `setupToken`.
Folder 498 (TXE-7259) covers what an operator does with those claims: a key with no storage list is refused with 400
when it names a bucket root as a batch's output folder, `GET /api/governance/storage-folders` lists a claim, `DELETE
/api/governance/storage-folders?folder=<uri>` releases it so the next key's batch claims the folder, and a deleted
key's claim is kept until released. A batch created with `metadata.fixture_output_dir` retrieves as completed with that
directory as its output, which the retrieve claims for the batch's creator. It needs the same `setupToken`, and it
releases the two claims it leaves.

The same fixture and gateway serve folder 416 (Kling video on the contract). The config's built-in
`kling` provider points at the fixture with two keys, `kling-key-a` and `kling-key-b`
(`kling-secret-a`, `kling-secret-b`), and the fixture answers Kling's API on its new design standard:
`POST /text-to-video/{model}`, `/image-to-video/{model}` and `/omni-video/{model}`,
`GET /tasks?task_ids=` and `?external_task_ids=`, and `POST /tasks`. A prompt that says `blocked` is
answered HTTP 400 with service code 1301, task `893605946402811986` is a task Kling's risk control
failed, and an `external_task_ids` value that starts `missing` has no task. Every request is kept
with its key and JSON body (`GET /__kling-calls`) and counted under
`kling <METHOD> <path> <bearer token>` in `/__hits`, which is how the folder proves the body sent to
Kling, one upstream call per create and per query, and none for a refused request:

```bash
newman run tmp/provider-harness.json \
  --folder "416. Kling video on the contract: create by model path, query by task id or client_reference_id, Units as vendor_cost, refusals (kling video contract, provider error fixture)" \
  --env-var baseUrl=http://127.0.0.1:8790 \
  --env-var providerErrorFixture=1
```

Its fixture rows read `providerErrorFixtureUrl`, so add `--env-var providerErrorFixtureUrl=http://127.0.0.1:<port>`
when the fixture runs on another port (`PROVIDER_ERROR_FIXTURE_PORT`). Rows 416.20 to 416.22 are `[PREVIEW]`
rows against the real Kling API and are checked structurally only.

The fixture also serves the two datasheets a fresh gateway downloads on first start (the config's `framework.pricing` URLs point at it), including the model-parameters row that makes `gpt-5-pro` Responses-only, which is what makes folder 160's chat request convert. The run therefore needs no network. Starting the gateway with `HTTPS_PROXY=http://127.0.0.1:9` is a cheap proof: loopback is exempt from proxying, so only an accidental external call would fail.

Stop the fixture and isolated gateway with Ctrl+C after testing.

Folder 185 (`routing-error-fixture`) uses the same fixture for routing cases that need an upstream to
fail on demand: a 500 retried three times, a first token held past a 500ms deadline, a caller key
the upstream refuses. Its rows create their own run-scoped providers whose base URL is the fixture,
and need the management API, governance and the logs, but no vendor: the provider that accepts a
caller's own key (row 8) is the fixture's `/any-key/` upstream, which takes any key and counts its hits
as `any-key:<model>`. The rows assert what the model catalog picks among the run's providers, so the
gateway must serve no model of its own: run them on
`tests/e2e/api/provider_config/routing-error-fixture.config.json`, which has no providers, turns
inference auth off and allows direct keys. The fixture-folder runner does that in its `routing-error`
group (TXE-7289):

```bash
scripts/test-offline.sh node tests/e2e/api/runners/fixture-folders.mjs run --gateway <bifrost-http binary> --group routing-error
```

When the gateway needs a setup token for its management API, run the folder through
`make run-provider-harness-test FEATURE=routing-error-fixture ENV_FILE=<an environment setting
providerErrorFixture=1>` instead, which adds the token to every management call.

### Hybrid log store: hold or fail an upload (TXE-7326)

The hybrid log store keeps a summary row in the database and offloads each log's payload to object
storage. Its bugs live in the order of those uploads, so the fixture also serves an S3-compatible
object store on its main port (`runners/lib/s3-object-fixture.mjs`). It works path-style, under the
bucket `harness-objects`. The fixture-folder runner's `hybrid-logstore` group runs the
provider-error gateway with `logs_store.object_storage` pointing at it, and its sqlite log store in
the group's app dir. Only the folders the group pins run there, since offloading changes what every
other folder's log reads see. Folder 547 proves the setup: a landed upload, a held one, and a
failed one, each read back through `GET /api/logs/{id}`.

A row drives the store through its control API at `{{providerErrorFixtureUrl}}/__s3/control`:

| Call | Effect |
| --- | --- |
| `POST /reset` | Forget every object, rule and PUT; held PUTs fail. |
| `POST /rules` `{"rules": [{"match": "<key substring>", "nth": 1, "action": "hold" \| "fail"}]}` | From now on, the nth PUT whose key contains `match` is held until released, or answered 403 (not retried). |
| `POST /release` `{"match": "<key substring>"}` | Held PUTs whose key contains `match` land. |
| `GET /state?match=&held=N&settled=N&timeoutMs=T` | Objects, PUTs and held keys for `match`. It waits until N PUTs are held, or N have finished, or the timeout passes (then `timedOut: true`). |
| `GET /object?key=` | One stored body, decompressed. |

Log keys are `harness/logs/YYYY/MM/DD/HH/<log id>.json.gz`, and a caller's `x-request-id` is the log
id, so a row matches its own uploads by the id it sends. A held PUT lands on its own after 20 s,
inside the gateway's 30 s upload deadline. A new folder that needs this store joins the group's
`files` list in `fixture-folders.mjs` and reads `hybridLogstoreFixture`.

```bash
scripts/test-offline.sh node tests/e2e/api/runners/fixture-folders.mjs run --gateway <bifrost-http binary> --group hybrid-logstore
```

The fixture also runs a stall proxy on port 8799 (`PROVIDER_ERROR_FIXTURE_STALL_PORT`): an
HTTP proxy that accepts the connection and never answers the CONNECT, holding it for 15 seconds, and
counts each CONNECT it reads as `__stall_proxy_connects` in `/__hits`. The `acquisition-first-token`
folder routes a streamed request to a provider whose `proxy_config` points at it, with a 500ms
first-token deadline and a fallback on the fixture, and asserts the fallback serves at the deadline
although the attempt never got a connection. Run it like folder 185, with
`providerErrorFixtureStallProxyUrl` when the stall proxy is not on `http://127.0.0.1:8799`.

## Tencent, StepFun and Xiaomi chat on openai-based custom providers

Folder 408 (TXE-6439) uses the same fixture and the same isolated gateway as folder 154. The
fixture's gateway config holds one openai-based custom provider per vendor site, as Tokun's
reconciler writes them (`tencent_tokenhub`, `stepfun`, `stepfun_cn`, `xiaomi`), each with its base
on the fixture's `/pp/<site>`. The fixture answers `/pp/<site>/v1/chat/completions` with the usage
shape the vendor documents: StepFun's international site puts the cache-hit count at the top of
`usage` (`cached_tokens`) and sends usage on every streamed chunk, its domestic site nests the count
under `prompt_tokens_details`, and Tencent and Xiaomi report the three token counts. The folder pins
that the counts reach the unified `usage` (StepFun's cached count at
`prompt_tokens_details.cached_read_tokens`, streamed or not) and that a request type outside a
provider's `allowed_requests` is refused with nothing sent to the vendor. Seven cases prove the
shared change of TXE-6527 on these providers. On a model with no capability record,
`max_completion_tokens: 8` and a `service_tier` reach the vendor as sent. The fixture's capability
sheet also lists service tiers for one model per vendor provider (`hy-tiered`, `step-tiered`,
`mimo-tiered`): a tier outside the list is refused with 400 `caller_unsupported_parameter` and
nothing is sent, and a listed tier reaches the vendor.
No answer is recorded from
a real request yet; replace them when the staging requests are made.

Start the fixture and the isolated gateway as in the previous section, then:

```bash
newman run tmp/provider-harness.json \
  --folder "408. Tencent, StepFun and Xiaomi chat on openai-based custom providers: usage, cached tokens and allowed request types (vendor chat channels) [SERIAL]" \
  --env-var baseUrl=http://127.0.0.1:8790 \
  --env-var providerErrorFixture=1 \
  --env-var providerErrorFixtureUrl=http://127.0.0.1:8791
```

## Deepgram

Folder 403 pins the `deepgram` provider against the same local fixture, with no live provider calls in
any row but 403.9: with `x-bf-api-key` one request is exactly one call to the vendor under the named
key, for a transcript and for a vendor error; the options sent as form fields arrive as query parameters
under their own names; a file arrives as the request body and a `url` with no `file` as the JSON body
`{"url": ...}`; `usage.seconds` is `metadata.duration` times `metadata.channels`; a vendor 402 keeps its
status and code; a 200 without results is `provider_empty_result`; and the `caller_unsupported_parameter`
refusals make no upstream call. The fixture serves Deepgram's pre-recorded API on `/v1/listen`. The
`model` query parameter picks the behaviour: `nova-3` answers the reference response (two channels when
`multichannel=true`), `deepgram-402` the out-of-credits error body, and `deepgram-no-results` a 200 with a
request id and no results. It accepts `test-key-a` and `test-key-b` in the `Token` scheme, the two keys the
config's `deepgram` provider carries, and refuses any other with 401. Each call is kept in `GET /__wire`
with its query, its `Content-Type` and which key it carried; an audio body is kept as its size.

Start the fixture and the isolated gateway as in the provider error fixture section above, then run:

```bash
newman run tmp/provider-harness.json \
  --folder "403. Deepgram provider: Nova-3 transcription by file or URL, options as query parameters, duration usage (deepgram provider) [SERIAL]" \
  --working-dir . \
  --env-var baseUrl=http://127.0.0.1:8790 \
  --env-var providerErrorFixture=1 \
  --env-var providerErrorFixtureUrl=http://127.0.0.1:8791
```

Rows 403.1 to 403.8, 403.10 and 403.11 are skipped unless `providerErrorFixture=1`. Row 403.9 is the live
row: one Nova-3 transcription of Deepgram's public sample by URL. In the live run it needs
`DEEPGRAM_API_KEY`; while that variable is unset the row is skipped by the keyless-provider rule, which
reads the `model` field of a multipart form as it reads the `model` of a JSON body. Against the fixture the
row passes too.

Rows 403.10 and 403.11 are the async route: 403.10 submits a url-only job to
`POST /v1/async/audio/transcriptions` and keeps its id, and 403.11 reads
`GET /v1/async/audio/transcriptions/{job_id}` three seconds later and expects the job completed, with the
transcript, the duration usage and exactly one call in `/__wire`. Run them together and in order, as the
folder does.
## DashScope sync image generation and edit

Folder 425 (TXE-6607) uses the same fixture and the same isolated gateway as folder 154. The
fixture's gateway config points the `dashscope` provider at the fixture, which answers DashScope's
`multimodal-generation` endpoint by the model in the body (`qwen-image-2.0`, `z-image-turbo`,
`qwen-image-moderated`, `qwen-image-empty`). Start the fixture and the isolated gateway as in the
previous section, then:

```bash
node tests/e2e/api/runners/provider-harness-folders.mjs build
newman run tmp/provider-harness.json \
  --folder "425. DashScope Qwen-Image and Z-Image sync generation and edit meet the contract (dashscope images) [SERIAL]" \
  --env-var baseUrl=http://127.0.0.1:8790 \
  --env-var providerErrorFixture=1 \
  --env-var providerErrorFixtureUrl=http://127.0.0.1:8791
```

## Fork deployment switches: provider job accounting off and passthrough heartbeat off

Folder 329 (TXE-6519) uses the same fixture and the same isolated gateway as folder 154. The
fixture's gateway config sets the fork's two deployment switches,
`client.disable_provider_job_accounting` and `client.disable_passthrough_sse_heartbeat`, so the
folder shows what a deployment that sets them gets: no vendor call of Bifrost's own between
requests, and a passthrough stream relayed byte for byte.

Start the fixture and the isolated gateway as in the previous section, then:

```bash
newman run tmp/provider-harness.json \
  --folder "329. Fork deployment switches: provider job accounting off and passthrough heartbeat off (provider error fixture, job accounting switch)" \
  --env-var baseUrl=http://127.0.0.1:8790 \
  --env-var providerErrorFixture=1
```

The fixture answers `POST /v1/videos`, `GET /v1/videos/{id}`, `GET /v1/videos/{id}/content`,
`DELETE /v1/videos/{id}`, `POST /v1/files`, `GET /v1/files/{id}/content`, `POST /v1/batches` and
`GET /v1/batches/{id}` in OpenAI shape: the video is complete as soon as it is retrieved, the batch
stays in progress, and every call is counted in `/__hits` under `video.create`, `video.retrieve`,
`video.content`, `video.delete`, `file.upload`, `file.content`, `batch.create` and `batch.retrieve`.
Case 329.5 reads the counters right after the creates and again 70 seconds later, longer than the
batch sweeper's one-minute tick and the video sweeper's 30-second tick, so the folder takes a little
over a minute. Model `upstream-idle-stream` answers a streamed chat request in two writes 2.5
seconds apart, longer than the gateway's one-second heartbeat interval, and `GET /__sent?model=`
returns the exact bytes it wrote; case 329.11 compares them with what the caller read through
`/openai_passthrough`.

To see the folder red, start the gateway on a copy of the config with both switches removed: 329.1
reports the switches off, the video sweeper polls `GET /v1/videos/{id}` within a minute, and the
passthrough body carries `: heartbeat` lines.

## Azure AI Speech

Folder 400 pins the `azurespeech` provider against the same local fixture, with no live provider calls:
the SSML and `X-Microsoft-OutputFormat` of a speech request, the vendor's audio `Content-Type` and the
`x-bifrost-usage` header on its response, audio chunks forwarded on arrival for a
streamed one, the multipart `audio` + `definition` of a transcription and the `usage.seconds` read from
`durationMilliseconds`, the `caller_unsupported_parameter` refusals, and vendor errors that keep their
status. The fixture serves the text to speech API on `/tts/cognitiveservices/v1` and fast transcription on
`/speechtotext/transcriptions:transcribe`, the paths a resource endpoint serves them on; the config's
`azurespeech` key names the fixture in `azure_key_config.endpoint`. The voice name in the SSML picks the
speech behaviour (`azure-speech-429`, `azure-speech-empty`, `azure-speech-gapped`; any other voice answers
audio) and `definition.enhancedMode.model` picks the transcription behaviour (`MAI-Transcribe-2` answers
the quickstart response, `azure-speech-422` a 422 error body). Each call is kept in `GET /__wire` with its
headers; a transcription call is kept as its definition and the size of its audio part.

Start the fixture and the isolated gateway as in the provider error fixture section above, then run:

```bash
newman run tmp/provider-harness.json \
  --folder "400. Azure AI Speech: SSML speech, streamed speech and fast transcription (azure speech provider) [SERIAL]" \
  --working-dir . \
  --env-var baseUrl=http://127.0.0.1:8790 \
  --env-var providerErrorFixture=1 \
  --env-var providerErrorFixtureUrl=http://127.0.0.1:8791
```

The cases are skipped unless `providerErrorFixture=1`.

## Vertex endpoint selection and header forwarding

These deterministic cases use a local TLS-intercepting proxy and an isolated gateway. No traffic
leaves the machine and no Google credentials are used. Vertex hardcodes `https://<host>/...`, so the
fixture is the gateway's HTTP proxy: it mints a throwaway CA, terminates TLS for whatever host the
gateway CONNECTs to, records the decrypted request, and answers with a canned Gemini or Claude body.

Start the fixture; it writes the gateway config (one Vertex key per region, a service account whose
`token_uri` points back at the fixture, and the proxy plus the CA the gateway must trust). Like every
endpoint fixture it writes that config through `writeFixtureGatewayConfig`
(`runners/lib/tls-intercept-proxy.mjs`), which turns inference auth off
(`client.enforce_auth_on_inference: false`): the cases send no virtual key, so with the default the
gateway would answer each of them with 401 before reaching the provider:

```bash
vertex_fixture_dir=${TMPDIR:-/tmp}/bifrost-vertex-fixture
mkdir -p "$vertex_fixture_dir"
node tests/e2e/api/runners/vertex-endpoint-fixture.mjs --app-dir "$vertex_fixture_dir" --port 8792
```

Start an isolated gateway on that directory in another terminal (a shell variable does not carry across
terminals, so set the same path there first):

```bash
vertex_fixture_dir=${TMPDIR:-/tmp}/bifrost-vertex-fixture
go run ./transports/bifrost-http -app-dir "$vertex_fixture_dir" -host 127.0.0.1 -port 8793
```

Run the cases in a third terminal:

```bash
newman run tmp/provider-harness.json \
  --folder "161. Vertex endpoint selection and header forwarding (vertex endpoint fixture)" \
  --env-var baseUrl=http://127.0.0.1:8793 \
  --env-var vertexEndpointFixture=1
```

Keys are picked with `x-bf-api-key`. The cases assert the host, the `locations/<region>` path segment
and the headers Vertex would have received: global, `us`/`eu` multi-region, `us-east5`, a regional key
that stays single-region for Gemini, Claude promoted to the `us` pool from an unpinned
`us-central1` key and kept on `us-central1` with `force_single_region`, the OAuth token path, the
service-tier shared request type header (global endpoint only; `service_tier: priority` on a regional key is
refused with 400 `caller_unsupported_parameter` before any call, TXE-6535), and extra-header forwarding, which
is how Provisioned Throughput spend-limit ids (`x-goog-spend-limit-id`) are sent.
The fixture also serves the pricing and model-parameters datasheets a fresh gateway downloads on first start. The model-parameters row `claude-opus-4-7: { provider: vertex, vertex_multi_region_only: true }` is load-bearing: the promotion from an unpinned `us-central1` key to the `us` pool (161.7) is decided by that catalog flag, so without it exactly that case fails. As with the Bedrock fixture, start the gateway with `HTTPS_PROXY=http://127.0.0.1:9` to prove nothing leaves the machine.

Folder 433 runs against the same fixture and gateway: Gemini text-to-speech on Vertex (TXE-6540). The fixture
answers the models `gemini-tts-ok`, `gemini-tts-blocked` and `gemini-tts-truncated` on `:generateContent` and
`:streamGenerateContent`. The config the fixture writes turns `client.enforce_auth_on_inference` off, because a fresh
gateway otherwise refuses these requests for lacking a virtual key.

The cases are skipped unless `vertexEndpointFixture=1`. Every pull request runs them, with every other
fixture folder, in the `Harness fixture folders (offline)` job (`runners/fixture-folders.mjs`, TXE-7285).

The same fixture stands in for a Vertex batch (TXE-6971). It keeps what the gateway uploads to Cloud Storage
(`POST /upload/storage/v1/b/<bucket>/o`), finishes a `batchPredictionJobs` job the moment it is created, and
writes the job's `predictions.jsonl` from the uploaded lines: each line's request as it was sent, labels
included, beside a canned response. Object list and download (with `Range`) are served from the same store,
and `GET /__gcs` on the control port returns every object. Folder 344 rows 1 and 2 use it: row 1 reads the
`bifrost_custom_id` and `bifrost_custom_id_b32_<n>` labels on the uploaded lines, row 2 reads each
`custom_id` back from `GET /v1/batches/<id>/results`. Every fixture key has `use_for_batch_api` set.

```bash
newman run tmp/provider-harness.json \
  --folder "344. A Vertex batch line's custom_id that cannot be a request label is encoded, or refused when too long (vertex batch custom_id)" \
  --env-var baseUrl=http://127.0.0.1:8793 \
  --env-var vertexEndpointFixture=1 \
  --env-var vertexGcsBucket=fixture-bucket
```

Stop the fixture and isolated gateway with Ctrl+C after testing.

## Bedrock passthrough for operations without native routes

These deterministic cases use a local TLS fixture and an isolated gateway. No traffic leaves the
machine and no AWS credentials are used. The Bedrock provider honours per-service endpoint overrides and
a trusted CA but not `proxy_config`, so the fixture is a TLS server the gateway is pointed at directly.
It overrides every Bedrock service endpoint, and serves empty pricing datasheets, so the gateway
starts offline.

Start the fixture; it writes the gateway config (one Bedrock key with throwaway credentials in
`us-west-2`, the endpoint overrides, the fixture CA as `network_config.ca_cert_pem`, and inference
auth turned off by the shared `writeFixtureGatewayConfig`, as for the Vertex fixture):

```bash
bedrock_fixture_dir=${TMPDIR:-/tmp}/bifrost-bedrock-fixture
mkdir -p "$bedrock_fixture_dir"
node tests/e2e/api/runners/bedrock-endpoint-fixture.mjs --app-dir "$bedrock_fixture_dir" --port 8794
```

Start an isolated gateway on that directory in another terminal, setting `bedrock_fixture_dir` to the same
path there first. Set `HTTPS_PROXY` to a dead port as a
tripwire: the Bedrock client honours it and loopback is exempt, so only an accidental external call
would fail, and it would fail closed:

```bash
bedrock_fixture_dir=${TMPDIR:-/tmp}/bifrost-bedrock-fixture
HTTPS_PROXY=http://127.0.0.1:9 HTTP_PROXY=http://127.0.0.1:9 \
  go run ./transports/bifrost-http -app-dir "$bedrock_fixture_dir" -host 127.0.0.1 -port 8797
```

Run the cases in a third terminal:

```bash
newman run tmp/provider-harness.json \
  --folder "162. Bedrock passthrough for operations without native routes (bedrock passthrough fixture)" \
  --env-var baseUrl=http://127.0.0.1:8797 \
  --env-var bedrockEndpointFixture=1
```

`/bedrock_passthrough` forwards exactly InvokeAgent, knowledge-base Retrieve (both
bedrock-agent-runtime, fixture port 8796) and ApplyGuardrail (bedrock-runtime, fixture port 8798). The
cases assert the request is SigV4 signed by the gateway for the key region, that caller credentials are
not forwarded, that the upstream body, status, `Content-Type` and `X-Amzn-*` headers return verbatim,
and that every other path, a traversal and a non-POST method are refused with 400 by the Bedrock passthrough's own operation allow-list before any request is built (the router's 403 route policy is switched off, so the refusal no longer comes from there). A traversal path is normalized by the HTTP server to a path outside the allow-list, so 162.6 asserts the allow-list refusal; the identifier validators for encoded dots are pinned by the Go tests.
InvokeAgent is streamed: the fixture's `AGENTSLOW001` agent sends five event-stream pieces one second apart, and the fixture gateway sets `default_request_timeout_in_seconds` to 3 seconds. For a streaming request that setting bounds the connection, the request write and the wait for response headers; once headers arrive, `stream_idle_timeout_in_seconds` governs the body. So 162.12 shows that a run longer than 3 seconds is not cut off by a total-duration cap and that every piece arrives. It does not by itself show incremental delivery; the Go tests in `core/providers/bedrock/passthrough_test.go` pin that.
162.13 pins the other half of that rule: the fixture's `STREAMSLOWKB` knowledge base takes 4 seconds to answer a Retrieve, and because Retrieve is a plain request/response call (even with the word `stream` in its id) the 3 second request timeout cuts it off with a 500 whose error says the deadline was exceeded, instead of streaming the answer through.
162.14 pins the stream idle timeout on a native Bedrock stream. The fixture answers ConverseStream for a model id containing `stall` with headers and then nothing for 8 seconds, and the fixture gateway sets `stream_idle_timeout_in_seconds` to 2 (the slow agent's 1 second gaps stay under it). The caller must get a 500 whose error says `stream idle timeout` after about 2 seconds. Before TXE-7264, a read that the idle timer's close ended with `io.EOF` ended the stream as if it were complete; darwin's net/http returns an error there instead, so the row is red before that fix only where the closed body reads as EOF, as on the Linux runners. The Go tests in `core/providers/utils/idle_timeout_reader_test.go` pin the rule on every OS.
The cases are skipped unless `bedrockEndpointFixture=1`.

Stop the fixture and isolated gateway with Ctrl+C after testing.

## Contents

### V1 Endpoint Tests

| Path | Description |
|------|-------------|
| `bifrost-v1-complete.postman_collection.json` | Postman collection: all `/v1` endpoints (models, chat, completions, responses, embeddings, audio, images, count tokens, batches, files, containers, MCP) |
| `bifrost-v1.postman_environment.json` | Optional/legacy Postman environment (OpenAI). `run-newman-inference-tests.sh` uses **BIFROST_*** environment variables as the fallback when no provider-specific env file is passed (see script and `--help`). |
| `run-newman-inference-tests.sh` | Script to run the V1 collection with Newman (single provider or all providers). |

### Integration Endpoint Tests

| Path | Description |
|------|-------------|
| `bifrost-openai-integration.postman_collection.json` | OpenAI integration endpoints: `/openai/v1/*`, `/openai/*`, `/openai/deployments/*` (38 requests) |
| `bifrost-anthropic-integration.postman_collection.json` | Anthropic integration endpoints: `/anthropic/v1/*` (13 requests) |
| `bifrost-bedrock-integration.postman_collection.json` | Bedrock integration endpoints: `/bedrock/model/*`, `/bedrock/files/*`, `/bedrock/model-invocation-*` (13 requests, including List Objects S3 ListObjectsV2). Auth via request headers set from collection/environment variables. |
| `bifrost-composite-integrations.postman_collection.json` | Composite integrations: GenAI, Cohere, LiteLLM, LangChain, PydanticAI, Health (21 requests) |
| `run-newman-openai-integration.sh` | Script to run OpenAI integration tests |
| `run-newman-anthropic-integration.sh` | Script to run Anthropic integration tests |
| `run-newman-bedrock-integration.sh` | Script to run Bedrock integration tests |
| `run-newman-composite-integration.sh` | Script to run composite integration tests |
| `run-all-integration-tests.sh` | Master script to run all integration test suites |

### Model Catalog Wiring Tests

| Path | Description |
|------|-------------|
| `collections/bifrost-model-catalog-wiring.postman_collection.json` | Generated collection asserting that management-API mutations (add/update/delete provider and key, toggle key, alias) propagate into the model catalog read endpoints. **Generated — do not hand-edit.** |
| `runners/build-model-catalog-wiring.mjs` | Generator for the collection above. Holds the scenario spec (the source of truth) and emits the JSON. |
| `runners/individual/run-newman-model-catalog-wiring-tests.sh` | Script to run the model-catalog wiring collection; `--binary` boots a clean server for the run. |

### Routing Wiring Tests

| Path | Description |
|------|-------------|
| `collections/bifrost-routing-wiring.postman_collection.json` | Generated collection asserting how virtual-key gates and per-key catalog gates compose on the routing path: the route each request takes, the error on a refusal, and the stored log. **Generated — do not hand-edit.** |
| `runners/build-routing-wiring.mjs` | Generator for the collection above; its scenario table is the source of truth. |
| `runners/individual/run-newman-routing-wiring-tests.sh` | Script to run the routing wiring collection; `--binary` boots a clean server for the run. |
| `runners/lib/clean-bifrost.sh` | Boots that clean server for both wiring runners: no providers, sqlite stores in a throwaway app dir. |

### Semantic Cache Wiring Tests

| Path | Description |
|------|-------------|
| `collections/bifrost-semantic-cache-wiring.postman_collection.json` | Generated collection covering the `semantic_cache` plugin end to end on one vector store: plugin management, direct and semantic caching on every cacheable request type, streams, per-request headers, the clear endpoints, `x-bifrost-cache-*` response headers, identity scoping and the logs. **Generated — do not hand-edit.** |
| `runners/build-semantic-cache-wiring.mjs` | Generator for the collection above. Its folders are the scenario table and the source of truth. |
| `runners/lib/semantic-cache-wiring.mjs` | Assertion library embedded in the collection: cache_debug outcomes, SSE parsing, header and log checks, polled reads. Unit tests: `runners/lib/semantic-cache-wiring.test.mjs`. |
| `runners/individual/run-newman-semantic-cache-wiring-tests.sh` | Boots one clean server per vector store (Redis, Weaviate, or both concurrently) and runs the collection against each. |

### Shared Resources

| Path | Description |
|------|-------------|
| `provider_config/` | Per-provider Postman env `.json` files (`bifrost-v1-openai.postman_environment.json`, etc.). Reused across all collections. |
| `provider-capabilities.json` | Provider capability matrix: per-provider map of booleans (e.g. `chat_completions: true`, `embedding: false`) for batch, file, container, embedding, speech, transcription, image. Derived from `core/providers/*/provider.go` NewUnsupportedOperationError. Used by integration collections to skip unsupported requests when run with all providers. |
| `collections/smoke-manifest.json` | Curated ~100-request smoke selection for `make smoke-provider-harness-test` (see [Provider Harness Smoke Set](#provider-harness-smoke-set)). Hand-maintained; guarded offline by `runners/lib/smoke-manifest.test.mjs`. |
| `fixtures/` | Sample files for multipart requests: `sample.mp3`, `sample.jsonl`, `sample.txt` |
| `setup-plugin.sh` | Builds the hello-world plugin for API Management plugin tests. Run automatically by API Management and all-integration runners. |
| `setup-mcp.sh` | Starts the test MCP server (`examples/mcps/http-no-ping-server`) on http://localhost:3001/ so Add/Update/Delete MCP Client tests can pass. Run automatically by API Management and all-integration runners. |
| `newman-reports/` | Test reports organized by collection type (e.g., `openai-integration/`, `anthropic-integration/`). HTML/JSON reports when using `--html` / `--json`. |

## Prerequisites

- [Newman](https://www.npmjs.com/package/newman): `npm install -g newman`
- Bifrost server running (e.g. `http://localhost:8080`) with at least one provider configured (API keys, etc.)

## Test infrastructure setup

Before running **API Management** or **all integration** tests, the runners optionally run:

- **`setup-plugin.sh`** – Builds `examples/plugins/hello-world` into `build/hello-world.so` (native OS/arch). If the plugin fails to build, plugin tests may fail with "plugin not found" / "failed to load"; those failures are treated as expected when the plugin is missing.
- **`setup-mcp.sh`** – Builds and starts the test MCP server (`examples/mcps/http-no-ping-server`) on **http://localhost:3001/** so the collection’s test MCP client (connection string `http://localhost:3001/`) can connect. If the server is already listening on 3001 or the script is skipped, MCP client tests accept 404/500 as fallback.

Both are called automatically by `runners/run-newman-api-tests.sh` and `runners/run-all-integration-tests.sh`.

To run setup manually (from this directory):

```bash
./setup-plugin.sh
./setup-mcp.sh
```

No Weaviate/cache setup is required: tests accept 405 for unimplemented cache endpoints.

## Run tests

From this directory (`tests/e2e/api`):

### V1 Endpoint Tests

```bash
# Run for all providers in parallel (each provider_config/bifrost-v1-*.postman_environment.json except sgl and ollama)
./runners/run-newman-inference-tests.sh

# Run for a single provider (by name or path to .json env)
./runners/run-newman-inference-tests.sh --env openai
./runners/run-newman-inference-tests.sh --env provider_config/bifrost-v1-openai.postman_environment.json

# Options
./runners/run-newman-inference-tests.sh --help
./runners/run-newman-inference-tests.sh --folder "Chat Completions"
./runners/run-newman-inference-tests.sh --html --verbose
```

### Provider Harness Smoke Set

The full provider harness runs ~1900 requests after augmentation. For a
pre-release or pre-merge check, run the curated smoke set instead:

```bash
make smoke-provider-harness-test                       # ~100 requests, all 8 providers
make run-provider-harness-test SMOKE=1                 # identical
make run-provider-harness-test SMOKE=1 PROVIDER=bedrock  # smoke set, one fork
make run-provider-harness-test SMOKE=path/to/other.json  # a different manifest
```

The selection lives in `collections/smoke-manifest.json`, grouped into five
pillars, each with an `intent` saying what it buys:

| Pillar | Picks | Covers |
|---|---:|---|
| `cache-parity` | 32 | Round 34 direct-vs-Bifrost cache anchor, Round 35 cross-provider matrix, content-block `cache_control` / `cachePoint`, OpenRouter pass-through |
| `reasoning-tools-interleave` | 24 | Folder 46's three-turn reasoning × tool-call chains, plus chat→responses tool replay |
| `reasoning` | 19 | Redacted thinking, encrypted reasoning item-id round-trips, thought-signature replay, fail-soft |
| `cross-cut-baseline` | 16 | One cell per endpoint shape and modality, including the azure and passthrough forks |
| `costing-guards` | 9 | Cost/usage recording (dbverify) and provider egress streaming/truncation guards |

Three things about the manifest are load-bearing:

- **Rows are matched by `(folder, name)` against the *augmented* collection**,
  not against the source folders in `collections/provider-harness/`. The cache-parity rows are generated at
  augment time by `runners/lib/midconv-system-cache-parity.mjs` and
  `crossprovider-cache-matrix.mjs`, so they exist nowhere in the source
  collection and cannot be tagged in place the way `[PREVIEW]` / `[SKIP]` rows
  are. The folder is required, not decorative: criss-cross rows are named after
  their model, so a bare `gemini/gemini-2.5-flash` matches 25 requests.
- **`SMOKE` forces the deferred cache-parity pass on.** That pass normally runs
  only for a completely unfiltered sweep, and `SMOKE` is a filter — without the
  override the cache rows would be carved out of the parallel pass and never
  replayed. `RERUN_FAILED=1` still wins, because its selection comes from the
  main pass's report.
- **`make test-harness-runner-lib` guards the manifest offline.**
  `runners/lib/smoke-manifest.test.mjs` runs the real augment step and resolves
  every entry, so a renamed or moved request fails there instead of silently
  shrinking the smoke set. It also asserts cache write/read pairs stay balanced,
  every provider fork has something to run, and each three-turn interleave chain
  keeps all three turns (turn 1 sets the collection variables; turns 2 and 3
  replay them).

Selecting a request pulls in its chained-variable producers automatically, and a
row claimed by two provider partitions runs once per fork — both the same as in a
full sweep, so the effective request count can exceed the manifest's 100.

### Routing Harness Ledger

Harness days are journaled in `routing/ledger-YYYY-MM-DD.md` (gitignored, one
file per day the routing/catalog suites run). Each ledger holds: an
open-divergences snapshot, per-suite scenario tables (setup / expected /
actual, with ✅ / ⚠️ recalibrated / 🐞 bug-found markers), the day's run
results, and day notes. Append to the current day's file during a session;
never rewrite past days.

### Management API coverage

`bifrost-api-management.postman_collection.json` is the OSS management surface:
every `/api/*` route the OSS binary serves, plus `/health` and `/metrics`.
Enterprise-only routes (RBAC, access profiles, audit logs, business units,
circuit breaker, MCP tool groups, vault) are **not** here — they 404 on an OSS
server and live in `bifrost-enterprise/tests/e2e/api/enterprise-management.postman_collection.json`,
which the enterprise runner merges on top of this base.

Two request styles coexist, and the difference is load-bearing:

- **Lifecycle requests** create a resource, assert on it, and tear it down. The
  collection-level script requires a 2xx, so these only belong to routes that
  succeed deterministically against a local gateway.
- **Requests suffixed `(Coverage Probe)`** pass on a 2xx or on any 4xx/5xx — but
  not on a 3xx, and not on a response the gate rejects for other reasons. Use
  that suffix only when the outcome depends on deployment shape rather than on
  correctness — an unconfigured vault, a missing background job runner, a webhook
  receiver that is deliberately not listening.

The collection-level gate also whitelists a handful of named requests that carry
no `(Coverage Probe)` suffix, because their non-2xx outcome is itself the
expected result. Keep this list in sync when adding to it:

- `Get … (Before Create)` requests may answer 404 — the resource does not exist yet.
- `Add / Reconnect / Update / Delete MCP Client` may answer 404 when the client
  was never added.
- Plugin requests may answer 404 with a "plugin not found"/"failed to load"
  message (the `.so` is not built), or 403 with "requires genuine admin
  authentication" in the unauthenticated pass.
- `Clear Cache by Cache ID / by Key (Coverage Probe)` may answer 405 — the routes
  are not implemented yet.
- `Add Provider` and `Update Proxy Config` may answer 403 with "requires an
  authenticated admin session" in the unauthenticated pass only: a provider
  base URL or the global proxy URL chooses where Bifrost dials out, so the
  server refuses to store it without a genuine admin session. Each request's
  own test asserts the 403 there and the 2xx in the authenticated pass; the
  dependent `Get / Update / Delete Provider` requests skip when the create was
  refused.
- `Add MCP Client`, `Create MCP Client (vMCP setup)` and
  `Add MCP Client (unresolvable host)` may answer 403 in the unauthenticated
  pass only: the e2e MCP server is on loopback (`http://localhost:3001/`) and an
  unresolvable name cannot be classified, and neither may be registered without
  an admin session. `Reconnect / Update / Delete MCP Client` and the Virtual MCP
  requests that need the setup client skip when its registration was refused.
  `Add MCP Client (unsupported connection_type)` answers 400 in both passes, and
  `Add MCP Client (unresolvable host)` answers 500 ("failed to connect") in the
  authenticated pass, where the registration is allowed but cannot connect.
- `Test Webhook Endpoint` may answer 403 in the unauthenticated pass only: the
  endpoint is created with `allow_private_network: true`, and a test delivery
  to such an endpoint needs an admin session. Authenticated it answers 200 with
  the delivery outcome (`delivered: false`, since nothing listens on the
  receiver port).

Resource names are stamped with `Date.now()` so the collection can run twice in
one invocation (the runner replays it with dashboard auth enabled).

### API Management Extensions

The API management runner can merge additional Postman folders maintained
outside this repo:

```bash
./runners/run-newman-api-tests.sh --extra-collection /path/to/extra.postman_collection.json
```

You can also pass extensions via environment variable:

```bash
BIFROST_API_EXTRA_COLLECTION=/path/to/extra.postman_collection.json \
  ./runners/run-newman-api-tests.sh
```

The default run loads no extra collections. Downstream repos pass their own
collections at run time, so the shared management requests live here while
assertions specific to those repos stay with them.

### Request guard tests

`collections/bifrost-v1-request-guards.postman_collection.json` pins what the
management and gateway surface refuses over the API, and that nothing was
persisted as a side effect: `file://` and unreachable catalog URLs on
`PUT /api/config`, MCP client registrations with an unsupported
`connection_type` or a loopback/unresolvable target, proxy / provider /
provider-key endpoint changes that need an admin session, the OAuth2 issuance
endpoints while `mcp_server_auth_mode` is `headers`, malformed passthrough
paths and unknown `x-model-provider` values, a zstd body declaring an oversized
window (`fixtures/zstd-window-512mib.zst`), the on-demand test delivery of a
private-network webhook, and `auth_config` updates that must prove the stored
admin password. It is hermetic (no provider is contacted) and has no
collection-level 2xx gate: every request asserts its exact status.

`runners/run-newman-api-tests.sh` runs it as its own newman invocation after
auth is restored to disabled and before the governance suites; set
`BIFROST_E2E_SKIP_REQUEST_GUARDS=1` to skip it. To run it standalone against a
gateway with dashboard auth disabled (from this directory, so newman finds the
zstd fixture):

```bash
BIFROST_BASE_URL=http://localhost:8080 ./runners/individual/run-newman-request-guards-tests.sh

# With a stored admin account whose password you know: also runs the
# "Dashboard auth update" folder, which re-enables auth with that password and
# disables it again. Create the account first if needed:
#   BIFROST_E2E_SETUP_TOKEN=<token> node runners/set-auth-config.mjs enable
#   BIFROST_E2E_AUTH_HEADER="Bearer $(printf 'admin:<password>' | base64)" node runners/set-auth-config.mjs disable
BIFROST_BASE_URL=http://localhost:8080 BIFROST_E2E_ADMIN_EXISTS=1 \
  BIFROST_E2E_ADMIN_USERNAME=admin BIFROST_E2E_ADMIN_PASSWORD='<password>' \
  ./runners/individual/run-newman-request-guards-tests.sh --json
```

Without `BIFROST_E2E_ADMIN_EXISTS=1` that folder reports a single named
"skipped" test and sends nothing. The "Provider endpoint changes" folder's
key-endpoint case targets the `azure` provider (part of the shared
`tests/config.json` profile); on a gateway without it the request asserts the
404 from the provider lookup instead, as recorded by the preceding check.

**Retry logic (CI)**
When `CI=1` or `CI=true` is set (case-insensitive), each failing request in the V1 collection is retried up to 3 times before moving to the next request. This helps with flaky tests in CI. The runner passes the value through to Newman when the environment variable is set (e.g. `CI=1 ./runners/run-newman-inference-tests.sh --env openai` or `CI=true ./runners/run-newman-inference-tests.sh --env openai`). Retry attempts are logged to the console as `[RETRY] Request "..." failed (attempt n/3). Retrying...`.

### Extra fields placement tests

`collections/bifrost-v1-extra-fields-placement.postman_collection.json` pins
`client.extra_fields_placement`, streaming and non-streaming. Body mode (the
default) keeps `extra_fields` in JSON bodies and SSE chunks and adds no new
headers. Headers mode removes `extra_fields` from every body, chunk and error
body and carries the routed identity and scalar fields as `x-bifrost-*` headers
on `/v1/chat/completions`, `/v1/responses` and `/openai/v1/chat/completions`,
each unary and streaming, plus a provider 4xx. Streams carry only what is known
before the first chunk, so the stream checks assert the identity headers and not
`x-bifrost-latency-ms`. A request with an `Origin` checks that
`Access-Control-Expose-Headers` lists the `x-bifrost-*` headers. The routing
identity headers that existed before this setting (`x-bifrost-provider`, original
and resolved model, request type, `x-bifrost-fallback-index` and every
`x-bifrost-routing-info-*`) are pinned against regression: in body mode each must
equal its `extra_fields` value, and the same unary, streaming and fallback
requests must return the same values in headers mode. Setup snapshots
`client_config` and Teardown restores it.

It makes about ten small live completions against the configured provider
(`openai` by default). `runners/run-newman-api-tests.sh` runs it after the
request guard tests; set `BIFROST_E2E_SKIP_EXTRA_FIELDS_PLACEMENT=1` to skip it.
Standalone, against a gateway with dashboard auth disabled:

```bash
BIFROST_BASE_URL=http://localhost:8080 ./runners/individual/run-newman-extra-fields-placement-tests.sh
```

### Integration Endpoint Tests

```bash
# Run all integration test suites for all providers
./run-all-integration-tests.sh

# Run all integration test suites for a single provider
./run-all-integration-tests.sh --env openai

# Run a specific integration test suite
./run-newman-openai-integration.sh           # OpenAI integration endpoints
./run-newman-anthropic-integration.sh        # Anthropic integration endpoints
./run-newman-bedrock-integration.sh          # Bedrock integration endpoints
./run-newman-composite-integration.sh        # Composite integrations + Health

# Run with options
./run-newman-openai-integration.sh --html --verbose
./run-newman-openai-integration.sh --env azure   # Test Azure-specific paths
```

### Model Catalog Wiring Tests

These tests cover the path **HTTP mutation → config write → server-side catalog
hook → read endpoint**: the wiring that keeps the model catalog (`/api/models`,
`/api/models/details`) in sync with provider and key changes made through the
management API. Each scenario stands up an isolated custom provider backed by a
real upstream (the per-provider scenarios run once each for OpenAI, Anthropic and
Gemini), drives a sequence of mutations, and asserts the catalog reflects each one.

What it covers (one scenario per contract):

- **Add provider + key** — a gated key surfaces its allowed model.
- **Update key model set** — changing a key's allow-list re-gates the catalog.
- **Disable / re-enable key** — a disabled key drops its models; re-enabling restores them.
- **Delete one of two keys** — only the deleted key's models drop; the sibling's survive.
- **Delete provider** — the provider and its models disappear from the catalog.
- **Alias resolution** — an inference call via a key alias routes to the underlying model.

Run locally (from this directory):

```bash
./runners/individual/run-newman-model-catalog-wiring-tests.sh                                 # against a running server
./runners/individual/run-newman-model-catalog-wiring-tests.sh --binary /path/to/bifrost-http  # boots a clean one (--port, default 8095)
```

CI runs it with `--binary` from `.github/workflows/scripts/test-api-integrations.sh`.

Requirements:

- With `--binary`, a built `bifrost-http`. Without it, Bifrost running at
  `$BIFROST_BASE_URL`, else `{{base_url}}` (default `http://localhost:8080`), ideally
  against a clean config store so no pre-existing `catwiring-*` providers linger.
- The server's environment needs `OPENAI_API_KEY`, `ANTHROPIC_API_KEY` and
  `GEMINI_API_KEY`: the collection's keys use `env.<NAME>` values, which Bifrost
  resolves from its own process environment, so the runner forwards no credentials.

Notes:

- Every resource is named `catwiring-openai-<scenario>-<run-id>`, where the
  run-id is built once per run from `e2e_seed_prefix` plus a timestamp nonce, so
  parallel runs never collide and a failed run leaves no blocking state.
- The catalog's live-model cache is populated asynchronously by the key hooks, so
  every post-mutation catalog read polls with exponential backoff (up to 8 attempts)
  instead of asserting immediately. The alias inference request is sent once, after
  a read has seen the alias listed, so an alias that resolves only on a retry fails.
- Each scenario has a cleanup folder that deletes its provider (cascading to its
  keys); it runs even when a mid-scenario step fails, and accepts 200/204/404.
- To change or extend the scenarios, edit
  `runners/build-model-catalog-wiring.mjs` and re-run it, then commit
  both the script and the regenerated collection:

  ```bash
  node runners/build-model-catalog-wiring.mjs
  ```

The only seed-env var it reads is `e2e_seed_prefix`, for run-id namespacing.

### Routing Wiring Tests

These tests cover how governance and the model catalog compose on the routing
path. Each scenario creates providers and keys, gates them with a virtual key,
sends inference, and asserts the route taken (`extra_fields.routing_info`), the
error on a refusal, and the stored log. Before a scenario's first inference
request, one step polls until the model catalog lists every provider's models and
asserts nothing about routing; each routing request after it is sent once and
judged once, so a route that only shows up on a retry fails.

Some scenarios create the global standard providers (openai, anthropic, gemini,
azure, bedrock, vertex), so run it against a server nothing else has configured. `--binary` boots
one (no providers, sqlite stores in a throwaway app dir) and stops it afterwards;
CI runs it this way from `.github/workflows/scripts/test-api-integrations.sh`.

```bash
./runners/individual/run-newman-routing-wiring-tests.sh --binary /path/to/bifrost-http  # --port, default 8094
node runners/build-routing-wiring.mjs                                                  # regenerate after editing scenarios
```

It calls real providers through `env.<NAME>` keys, so the server's environment
needs `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, `AZURE_API_KEY` and
`AZURE_ENDPOINT`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` and `AWS_REGION`,
and `VERTEX_PROJECT_ID` and `VERTEX_CREDENTIALS`. Both wiring collections send the
setup token on `/api` calls (`BIFROST_SETUP_TOKEN`, default
`bifrost-e2e-setup-token`), which a server with a setup token and no admin requires.

### Auth Matrix Tests

| Path | Description |
|------|-------------|
| `collections/bifrost-v1-auth-matrix.postman_collection.json` | Asserts the separation between inference auth (governance / virtual key) and dashboard auth (admin password on `/api/*`). |
| `runners/individual/run-newman-auth-matrix-tests.sh` | Boots a fresh server per config combination and runs the collection against each. |

Unlike the other runners, this one **boots its own servers** — each of the four
combinations needs a different boot config, so it cannot reuse a shared running
server. It requires a built `bifrost-http` binary.

It sweeps the 2x2 of `client.enforce_auth_on_inference` x
`governance.auth_config.is_enabled` (admin password) with a pre-seeded virtual key
and an unreachable dummy provider (so "auth passed" surfaces as a non-401 upstream
error). Per combination it asserts:

- **VK-authenticated inference is never blocked by the auth layer** (never 401),
  in every combination — including admin-password-on. This is the core regression
  guard: the admin middleware must not reject virtual-key inference.
- **No-VK inference** is rejected by governance with `virtual_key_required` only
  when `enforce_auth_on_inference` is on; otherwise it passes the auth layer.
- **Admin Basic auth is not a substitute for a VK** on inference (same governance
  rejection when enforce is on).
- **`/api/config`** requires admin creds (admin-middleware `Unauthorized`) only
  when admin password auth is on; it is open otherwise.

Run locally (from this directory):

```bash
./runners/individual/run-newman-auth-matrix-tests.sh --binary /path/to/bifrost-http
# options: --port <port> (default 8090), --html, --json, --verbose, --bail
```

### MCP Auth Tests

| Path | Description |
|------|-------------|
| `collections/bifrost-v1-mcp-auth.postman_collection.json` | Asserts inbound `/mcp` authentication across the three server auth modes (`headers` / `both` / `oauth`): discovery gating, the credential connect matrix, the full issuance flow, refresh rotation + family revocation, the revocation window, and the runtime `headers`→`both` upgrade. |
| `runners/individual/run-newman-mcp-auth-tests.sh` | Builds + starts the upstream MCP server, then boots a fresh server per `client.mcp_server_auth_mode` and runs the collection against each. |

Like the auth-matrix runner, this one **boots its own servers** — each mode needs a
different boot config. It also builds and starts the upstream MCP server
(`examples/mcps/http-no-ping-server`) so `/mcp` exposes real tools, and pre-seeds it
as an MCP client plus two virtual keys (one active, one inactive). It requires a
built `bifrost-http` binary.

The collection's test scripts branch on the `auth_mode` env-var, so a single
collection encodes the full matrix. Per mode it asserts:

- **`headers` (default):** discovery endpoints 404; every virtual-key credential
  (`x-bf-vk`, `Authorization: Bearer <vk>`, `x-api-key`) connects exactly as before;
  anonymous connects when auth is not enforced; an inactive key never connects. The
  OAuth surface is invisible.
- **`both`:** every header-credential outcome is identical to `headers`, and only
  *adds* JWT acceptance + live discovery; an invalid JWT is rejected with
  `WWW-Authenticate`.
- **`oauth`:** header credentials and anonymous are rejected (401 +
  `WWW-Authenticate`); only issued JWTs connect.

In `both`/`oauth` it also runs the end-to-end issuance flow (dynamic client
registration, PKCE-S256 authorize, consent bound to a virtual key or to a
server-minted session, token exchange, JWT connect), then refresh rotation with
stolen-token family revocation, the revocation window (a revoked grant stops refresh
while its already-issued access token keeps working until expiry), and — from the
`headers` boot — the runtime `headers`→`both` upgrade.

Run locally (from this directory):

```bash
./runners/individual/run-newman-mcp-auth-tests.sh --binary /path/to/bifrost-http
# options: --port <port> (default 8090), --mcp-port <port> (default 3001), --html, --json, --verbose, --bail
```

### Warp Tests

| Path | Description |
|------|-------------|
| `collections/bifrost-v1-warp.postman_collection.json` | Asks Warp (the dashboard log-analysis agent) standard questions — incident RCA, errors, spend, latency, org breakdowns, conversation search, calendar windows, drill-down, follow-up chains — checks it declines what it cannot do (charts, files, and every write action) with the feature-request link instead of chart code or tool loops, and probes its guardrails: out-of-scope refusals, instruction overrides, a prompt injection planted in log content, and the two-questions-in-a-row cap. Also pins request validation (400/413) and SSE framing. **Generated — do not hand-edit.** |
| `runners/build-warp-collection.mjs` | Generator for the collection above. The case table (question + expectations) is the source of truth. |
| `runners/lib/warp-case.mjs` | Case driver embedded in the collection: answers Warp's `ask_user` questions the way the dashboard does, carries follow-up chains, retries a missed case once. Unit tests: `runners/lib/warp-case.test.mjs`. |
| `runners/individual/run-newman-warp-tests.sh` | Recreates a throwaway Postgres database, boots Bifrost with the `warp` flag on, seeds it with `tests/cmd/seed/warpseed`, and runs the collection. |

This runner **boots its own server on its own database** (`bifrost_warp_e2e` by
default): Warp answers questions about every row in the logs table, so any other
traffic would change the answers. `warpseed -reset-db` recreates the database
before boot; a second `warpseed` run after boot inserts a fixed-seed week of logs
(an 18-minute anthropic `overloaded_error` incident, 20 scattered failures, a
quieter prior week, lopsided team/user/app/customer splits, and pinned slowest,
most-expensive and prompt-injection rows) and writes the facts the checks need
(incident window, pinned row ids) to an env file handed to newman.

It is **live and paid**: Warp's agent loop and its embeddings call OpenAI through
`env.OPENAI_API_KEY` (default `gpt-5.6-luna` and `text-embedding-3-small`; override with `WARP_MODEL`). Locally you
can skip the key and set `WARP_UPSTREAM_BIFROST=http://localhost:8080` instead: the test
server then sends its OpenAI calls to that Bifrost's `/openai` route with a placeholder
key, and that instance uses its own configured OpenAI key (it only forwards a caller's
key when `allow_direct_keys` is on and the request sends `x-bf-direct-key`). Answers
come from a model, so checks are structural (no agent error, the expected tools ran,
the expected filter was applied) plus keyword checks against seeded facts, and each
question case is retried once before it fails. Request validation and SSE framing
make no model call and never retry.

It needs Postgres and Weaviate (both in `tests/docker-compose.yml`) and a built
`bifrost-http` binary. CI runs it from `.github/workflows/scripts/test-api-integrations.sh`.

```bash
make run-warp-test                                  # builds tmp/bifrost-http first
make run-warp-test BINARY=tmp/bifrost-http FOLDER="Guardrails"
# or directly, from this directory:
./runners/individual/run-newman-warp-tests.sh --binary /path/to/bifrost-http
# options: --port <port> (default 8093), --folder <name> (repeatable; Setup always runs), --html, --json, --verbose, --bail
# env: OPENAI_API_KEY or WARP_UPSTREAM_BIFROST, POSTGRES_HOST/PORT/USER/PASSWORD, WARP_DB, WEAVIATE_HOST (default localhost:9000), WARP_MODEL, WARP_SEED
```

To change a question or its checks, edit `runners/build-warp-collection.mjs` and
regenerate:

```bash
node runners/build-warp-collection.mjs
node runners/lib/warp-case.test.mjs
```

### Semantic Cache Wiring Tests

The collection drives the `semantic_cache` plugin through the HTTP API and checks
what a client sees: `extra_fields.cache_debug` on JSON bodies, the final chunk of
every stream, the `x-bifrost-cache-*` response headers on routes whose bodies carry
no cache_debug (Anthropic, GenAI, Cohere, Bedrock, speech), and the log row found
through `x-bifrost-request-id`. Folders run in order: P (preconditions and providers),
M (plugin management), D (direct cache on chat completions), X (every other request
type), T (streams), H (per-request headers), C (clear endpoints), K (response
headers), G (identity scoping by virtual key), S (semantic mode), O (logs).

The runner **boots its own servers**, one per vector store, each on a temp app dir
with SQLite config and logs stores, so the provider list and the logs table hold
only this run's traffic. With `--store both` (the default) the Redis and Weaviate
runs go concurrently on `--port` and `--port`+1, and the runner fails if either
does. Every cache key carries the run id and every vector store namespace its
alphanumeric form, so repeated runs never read each other's entries; the runner
drops the run's Weaviate classes on exit.

Reads that must hit are polled with backoff and send `x-bf-cache-no-store: true`, so
a read that misses never writes the entry it is waiting for. A read that must miss
is only issued after the entry it is isolated from was seen to hit. The only fixed
waits are the TTL rows (TTL plus two seconds) and the "nothing was written" rows
(two seconds).

It is **live and paid**: OpenAI (chat, responses, embeddings, tts-1, whisper-1,
gpt-image-1), Anthropic, Gemini, Cohere, OpenRouter (text completions, since OpenAI
retired every `/v1/completions` model) and Bedrock. Every key is `env.<NAME>`, read
from the environment of the server the runner boots; the runner refuses to start
when one is missing. It needs Redis Stack and Weaviate (both in
`tests/docker-compose.yml`) and a built `bifrost-http` binary. CI runs it from
`.github/workflows/scripts/test-api-integrations.sh`.

Run locally (from this directory):

```bash
docker compose -f ../../docker-compose.yml up -d redis-stack weaviate
./runners/individual/run-newman-semantic-cache-wiring-tests.sh --binary /path/to/bifrost-http
# options: --port <port> (default 8096), --store redis|weaviate|both, --folder <name> (repeatable; Setup and P always run, O also runs D, X and S), --html, --json, --verbose
# env: OPENAI_API_KEY ANTHROPIC_API_KEY GEMINI_API_KEY COHERE_API_KEY OPENROUTER_API_KEY AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_REGION, REDIS_ADDR (default localhost:6379), WEAVIATE_HOST (default localhost:9000)
```

Reports land in `newman-reports/semantic-cache-wiring-<store>/`, with the server
log next to a failed run's report. To change a scenario, edit
`runners/build-semantic-cache-wiring.mjs` and regenerate:

```bash
node runners/build-semantic-cache-wiring.mjs
node runners/lib/semantic-cache-wiring.test.mjs
```

### Test Success Criteria

A request **passes** if either:
- The response status is 2xx, or
- The response is 4xx/5xx but the error indicates the operation is not supported by the provider (e.g. `error.code === "unsupported_operation"` or message like "operation is not supported" / "not supported by X provider").

Any other non-2xx (e.g. 401 with a wrong API key) fails the test.

**V1 collection ("documented unsupported" assertion)**  
The **"Or documented unsupported (allowed request types)"** test passes only when the request’s operation category is marked as unsupported for the current provider in **`provider-capabilities.json`** (`providers.<name>.<operation> === false`). The request name is mapped to one of: `chat_completions`, `chat_completions_with_tools`, `text_completion`, `responses`, `responses_with_tools`, `count_tokens`, `batch_create`, `batch_create_file`, `batch_list`, `batch_retrieve`, `batch_cancel`, `batch_results`, `file_upload`, `file_batch_input`, `file_list`, `file_retrieve`, `file_delete`, `file_content`, `container_create`, `container_list`, `container_retrieve`, `container_delete`, `container_file_create`, `container_file_create_reference`, `container_file_list`, `container_file_retrieve`, `container_file_content`, `container_file_delete`, `embedding`, `speech`, `transcription`, `list_models`, `image_generation`, `image_variation`, `image_edit`, `video_generation`, `video_retrieve`, `video_download`, `video_delete`, `video_list`, `video_remix`, `rerank`. These match the operation types in `core/schemas/bifrost.go` (e.g. `FileUploadRequest`, `ContainerFileContentRequest`). **`provider-capabilities.json` is the single source of truth:** the V1 run script (`run-newman-tests.sh`) loads it at run time and passes it to Newman as globals; the collection does not define or embed `provider_capabilities`.

### Expected failures (known limitations)

Some failures are expected and do not indicate bugs:

- **Authentication (401)** – Provider envs may use placeholder or invalid API keys; 401 is then expected. Some OpenAI integration endpoints may show 401 even with valid keys if keys are not configured for all endpoint types.

- **Batch API config (500)** – **"no batch-enabled keys found"** / **"no config found for batch APIs"** when batch endpoints are not configured for that provider.

  **To fix:** In Bifrost's config (or provider config), enable "Use for Batch APIs" on at least one API key for the provider, e.g. in config JSON:
  ```json
  {
    "providers": [
      {
        "name": "openai",
        "api_keys": [
          {
            "key": "sk-...",
            "use_for_batch_apis": true
          }
        ]
      }
    ]
  }
  ```

- **Model incompatibility** – Some models do not support certain operations (e.g. Azure gpt-4o does not support text completions, OpenAI chat models cannot be used for text completions); these may return 400 errors.

- **Responses API with tools** – The V1 "Create Response with Tools" test uses only a function tool (no `web_search`). Using `web_search` or other tool types can trigger 500 errors from the provider (OpenAI has had known 500s with web search on the Responses API).

- **Bedrock** – Model (converse/invoke) or S3 file operations may fail with 403/500 if AWS keys or S3 are not configured. The Bedrock **integration** collection (`bifrost-bedrock-integration.postman_collection.json`) tests `/bedrock/*` and supports auth via **request headers** (set from collection or environment variables by the collection’s pre-request script). Credentials can be provided via env vars (e.g. `BIFROST_BEDROCK_API_KEY`, `BIFROST_BEDROCK_ACCESS_KEY`, `BIFROST_BEDROCK_SECRET_KEY`, `BIFROST_BEDROCK_REGION`) when using the runner with `--env bedrock`; the runner passes these into Postman variables, which the pre-request script forwards as `x-bf-bedrock-*` headers. Set authentication in `provider_config/bifrost-v1-bedrock.postman_environment.json` or collection variables:
  - **Option A:** `bedrock_api_key` (API key authentication) and optionally `bedrock_region` (default: us-east-1)
  - **Option B:** `bedrock_access_key`, `bedrock_secret_key`, `bedrock_region` (required), and optionally `bedrock_session_token` (for temporary credentials)
  - For S3 operations: set `s3_bucket` and `s3_key` to a bucket you have access to; List Objects (GET `/bedrock/files/{bucket}`) is included and supports optional query params (`prefix`, `max-keys`, `continuation-token`)
  - For batch operations: set `role_arn` to an IAM role with Bedrock batch permissions; ensure `inputDataConfig` and `outputDataConfig` S3 URIs exist
  - The V1 collection skips file/batch requests when `file_id` / `batch_id` are placeholders

- **Composite integrations** – Cohere/OpenAI/Nebius etc. can show 401/402/500 due to keys, billing, or provider limits (e.g. tool calling, embeddings).

- **Plugin tests** – If `setup-plugin.sh` did not build the hello-world plugin, Create/Get/Update Plugin tests may fail with "plugin not found" / "failed to load"; the test suite treats these as acceptable when the plugin is missing.

## Integration Endpoint Testing Strategy

### Native Integration Endpoints

Each major provider has its own integration test collection that tests provider-specific endpoint patterns:

- **OpenAI Integration** (`/openai/*`): Tests standard paths (`/openai/v1/chat/completions`), no-v1 paths (`/openai/chat/completions`), and Azure deployment paths (`/openai/deployments/{deployment-id}/chat/completions`). Covers 38 endpoints including chat, completions, embeddings, audio, images, batches, files, and containers.

- **Anthropic Integration** (`/anthropic/*`): Tests Anthropic-specific paths with different batch result endpoint pattern (`/anthropic/v1/messages/batches/{batch_id}/results` vs OpenAI's pattern). Covers 13 endpoints including messages, complete, count tokens, batches, and files.

- **Bedrock Integration** (`/bedrock/*`): Tests AWS Bedrock patterns with ARN-based batch job identifiers and S3 file operations. Covers 13 endpoints including converse, invoke, batch jobs, S3 operations (PUT/GET/HEAD/DELETE Object and List Objects), and List Batch Jobs with optional query params.

### Composite Integration Testing (Delegation)

The composite integrations collection tests **routing** for frameworks that delegate to other integrations:

- **LiteLLM, LangChain, PydanticAI**: These are pass-through routers that prefix requests with their framework name, then delegate to the underlying integration. For example:
  - `POST /litellm/v1/chat/completions` → delegates to OpenAI integration logic
  - `POST /litellm/anthropic/v1/messages` → delegates to Anthropic integration logic
  - `POST /langchain/bedrock/model/{model}/converse` → delegates to Bedrock integration logic

Rather than duplicating 100+ tests for each composite integration, we test **representative routes** (5 per composite) to validate routing works correctly. Comprehensive endpoint coverage is provided by the base integration tests.

- **GenAI**: Tests Google Gemini API format endpoints (2 requests)
- **Cohere**: Tests Cohere API format endpoints (3 requests)
- **Health**: Tests the `/health` endpoint (1 request)

### Skipping unsupported operations (integration collections)

When integration tests are run for **all providers** (e.g. `./run-newman-openai-integration.sh` without `--env`), each collection is executed once per provider environment. Some providers do not support batch, file, container, embedding, audio, or image operations. To avoid failing on those requests, each integration collection has:

- **Collection-level prerequest**: Runs before every request. Reads the current **provider** from the environment and the **request name**. If the request maps to an operation category for which the provider has `false` in `provider_capabilities` (e.g. `providers.anthropic.embedding === false`), the request is **skipped** via `postman.setNextRequest(nextRequestName)` so the next request in execution order runs instead.
- **Embedded variables**: `execution_order` (JSON array of request names in depth-first order) and `request_to_operation` (JSON map of request name → operation category). For the **V1** collection, `provider_capabilities` is **not** embedded: it is loaded from `provider-capabilities.json` at run time by `run-newman-inference-tests.sh` and passed to Newman as globals.

**Config file**: `provider-capabilities.json` in this directory is a map per provider of capability flags (e.g. `chat_completions`, `embedding`, `batch`) to booleans. It is the single source of truth for which operations each provider supports (aligned with `core/providers/*/provider.go` returning `NewUnsupportedOperationError`).

**Updating capabilities or request mappings**:

1. **Change provider support**: Edit `provider-capabilities.json` and set each capability to `true` or `false` under `providers.<name>` (e.g. `providers.anthropic.embedding: false`). It is the only source of truth; the V1 inference run script loads it at run time (no embedded copy in the collection).
2. **Change which requests are skippable**: Edit `scripts/update-collection-capabilities.js` (function `getRequestToOperationMap`) to adjust the request-name → operation map for each collection.
3. **Re-inject variables into a collection**: From this directory run:
   ```bash
   node scripts/update-collection-capabilities.js bifrost-openai-integration.postman_collection.json --inject
   ```
   This re-extracts execution order, re-reads `provider-capabilities.json`, and overwrites the collection variables and the prerequest script. Run for each integration collection you changed.

### Batches, Files, Containers (mirror core tests)

Execution order and request shapes match the core Go tests (`core/internal/llmtests/batch.go`, `containers.go`):

- **Files** run first: Upload File (sets `file_id`), List, Retrieve, Get Content. No delete yet so the file can be used by Batches.
- **Batches**: Create Batch (Inline) sets `batch_id`; Create Batch (File-based) uses `file_id`; List (with `limit=10`), Retrieve, Cancel, Results use `batch_id`; then Delete File.
- **Containers**: Create Container sets `container_id`; List, Retrieve; Create Container File (Upload) sets `container_file_id`; List/Retrieve/Content/Delete container file use `container_file_id`; Delete Container last.

Request bodies match core (e.g. batch inline with `custom_id`/`body`/`Say hello`, container create with `name: "bifrost-test-container"`).

## Syncing from OpenAPI docs

The collection and supporting files are maintained under `docs/openapi/`. To refresh this e2e copy:

- `bifrost-v1-complete.postman_collection.json` ← `docs/openapi/bifrost-v1-complete.postman_collection.json`
- `bifrost-v1.postman_environment.json` ← `docs/openapi/bifrost-v1.postman_environment.json`
- `runners/run-newman-inference-tests.sh` ← `docs/openapi/run-newman-inference-tests.sh`
- `provider_config/*.postman_environment.json` and `provider_config/README.md` ← `docs/openapi/provider_config/` (if syncing from docs)
- `fixtures/*` ← `docs/openapi/fixtures/`

## Video costing checks

Video is priced at **settlement**, not at submission. `POST /v1/videos` returns a queued job with no
cost, and minutes later a settler writes a *second* log row carrying the real figure. Neither newman
nor a Go unit test can verify that — one has no way to wait minutes for an out-of-band row, the other
has no provider. `runners/run-video-costing.mjs` does the whole loop:

```
submit -> poll to terminal -> wait for the settlement row -> assert the billed cost
```

The expected figures in `fixtures/video-costing-cases.json` are the providers' **own published
rates**, so a red case means Bifrost disagrees with a price list.

| Path | Description |
|------|-------------|
| `fixtures/video-costing-cases.json` | One case per line of the video-costing checklist: request body, expected dimensions, expected rate. |
| `runners/run-video-costing.mjs` | The runner: submit, poll, wait for settlement, assert, report. |
| `runners/lib/video-costing.mjs` | Pure verdict logic — band resolution, expected cost, which row is the cost row. |
| `runners/lib/video-costing.test.mjs` | Unit tests for the above. No network, no Bifrost, no credentials. Runs under `make test-harness-runner-lib`. |

### Running

```bash
# What each case covers. No network.
make list-video-costing-cases

# Everything, against a local Bifrost.
make run-video-costing-test

# One provider group, against hosted Bifrost with a virtual key.
make run-video-costing-test ARGS="--group Runware --base-url https://<host> --vk sk-bf-..."

# Print what would run without spending money on generations.
make run-video-costing-test ARGS="--dry-run"
```

Exit code is 1 if any case fails, so it drops into CI unchanged once the rates are filled in.

### Flags

| Flag | Meaning |
|------|---------|
| `--base-url` | Target Bifrost. Defaults to `http://localhost:8080`. |
| `--vk` | Virtual key for the inference calls. Required against hosted. |
| `--api-key` | Management API key, sent as `x-bf-api-key`, for reading `/api/logs`. |
| `--header "X-Foo: bar"` | Extra headers on every call; comma-separate for several. |
| `--group` / `--provider` / `--case` | Run a subset. |
| `--concurrency` | Cases in flight at once (default 3). Every case is a real paid generation. |
| `--seed-pricing` | Write each case's expected rate as a global pricing override before running. |
| `--include-optional` | Also run cases marked `optional` (they depend on provoking a provider failure). |
| `--config-db-url` | Postgres URL for the configstore, which unlocks the job-row checks. |
| `--dry-run` / `--list` | Show what would run, and stop. |
| `--out` | Report path. Defaults to `newman-reports/video-costing-report.json`. |

### Why `--seed-pricing` exists

The resolution-band pricing keys (`output_cost_per_video_per_second_{480p,720p,1024p,1080p,4k}`) are
wired in Bifrost but **not yet published to the upstream datasheet feed**. Without a rate, every
banded case settles *unpriceable* (cost `NULL`) and proves nothing. `--seed-pricing` writes each
case's own expected rate as a global pricing override first, so the run measures Bifrost's band
selection and arithmetic rather than the feed's contents. Drop the flag once the feed publishes them.

### Cases marked `needs_rate`

Runway and Replicate cases ship with `rate_per_second: null` and are reported as **skipped**, not
passed. Their official rates have not been sourced from provider docs yet (Runway prices in credits,
Replicate ids are version-pinned), and a made-up number in a billing test is worse than no test. Fill
in `expect.cost.rate_per_second` and delete `needs_rate` to activate them.

### Reading a failure

Each case reports independent checks, so the report names the property that broke rather than one
opaque boolean:

```
OpenAI
  ✘ sora-2-pro at 1080p (3 of 3 rates) $2.4
      cost: billed $2.4, wanted $5.6 (0.7/s x 8s x 1)
```

The checks, in the order they run: `submitted`, `terminal`, `expected-status`, `one-settlement-row`,
`seconds`, `size`, `output-count`, `band`, `incomplete-flag`, `priced`, `cost`, plus
`settlement-path-parity` and `job-params.*` where a case asks for them.

Two are worth calling out:

- **`one-settlement-row`** fails on 0 *or* 2+. Two rows means the deterministic-id idempotency key
  broke, which would double-bill and double-report to governance — a different bug from a wrong rate.
- **`seconds` / `size`** are asserted separately from `cost` so a bad dimension merge (provider
  returned 5s for a requested 4s and we priced the request) fails on its own check instead of
  silently moving the cost target.

### Notes on specific cases

- **`settle_via: "sweeper"`** deliberately does not poll. Calling retrieve settles the job inline, so
  polling would make the sweeper path untestable. That case waits for the row to appear on its own,
  and `compare_cost_with` cross-checks that both paths bill the identical figure.
- **`runway-retrieve-404-stops-polling`** asserts on *latency*, not just the status code — an
  internally retried 404 blows through the `expect_fast_ms` ceiling.
- **`runway-seedance25-input-seconds`** is expected to fail on price even once its rate is filled in.
  Runway bills seedance for input seconds *plus* output seconds and Bifrost has an output-only
  per-second model, so video-to-video under-bills. Run it to measure the gap.
- **`expect_job_params`** (the Gemini cases) needs `--config-db-url`; without it those checks report
  as skipped rather than failing, so every other check still works against hosted Bifrost.
