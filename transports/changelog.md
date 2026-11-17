## 🍴 Fork changes (txehq/bifrost)

- chore: v2.4.0-sync.12 is a prerelease on top of v2.4.0-sync.11 with no upstream commits taken. It carries the fork PRs merged since v2.4.0-sync.11 (`git log --first-parent transports/v2.4.0-sync.11..<release commit>`). v2.3.5 stays the stable release and `latest`. The Helm chart is 2.1.52-sync.12.

### Security
- fork: **TXE-7525** (#355) every provider reads an extra-param object through `schemas.ExtraParamObject`; a typed object had bypassed Bedrock guardrails, Vertex batch labels and Gemini safety settings, and google-cloud-tts refuses a typed `audioConfig.audioEncoding`.
- ci: **TXE-7597** (#362) the five pull_request jobs that ran PR code are gated on the pr-testing approval.

### Gateway stability
- fork: **TXE-7743** (#376) a refused Agent Gateway agent-card, OAuth discovery or OAuth token fetch closes its connection; 500 refused token fetches used to leave 500 connections open.
- fork: **TXE-4982** (#363) `Cache.Fill`'s leader reads the cache before it loads, so a late caller does not load twice.
- fork: **TXE-7318** (#268) only an Agent log's latest upload sets `has_object`, so a row is never marked offloaded to an object older than itself.

### Billing and pricing
- fork: **TXE-7302** (#326) an over-limit image refusal charges an estimate to the virtual-key budget.
- fork: **TXE-7492** (#348) an over-limit image refusal on a token-priced model charges the documented tokens per image.
- fork: **TXE-7498** (#349) a cost recalculation of an unread large-response image log prices the requested `n`.
- fork: **TXE-7502** (#356) a raw image request body is priced at its own size and count, or refused.
- fork: **TXE-7499** (#353) the C4 price-field refusal runs on the image task creates, for every image task provider.
- fork: **TXE-7494** (#350) a batch results read is held to `client.max_response_body_size_mb`; the settlement read is the one exception.
- fork: **TXE-7513** (#352) the batch sweeper settles a batch page by page, with totals unchanged.
- fork: **TXE-7526** (#354) a batch result line past the response limit is read from a slim copy and billed, and comes back `truncated`.
- fork: **TXE-7122** (#249) every price-affecting video field reaches the pricing row; input media never does.
- fork: **TXE-7054** (#250) the video settler settles Azure Sora 2 and first-generation sora.
- fork: **TXE-7131** (#351) the video settler settles VolcEngine and ModelArk jobs.
- fork: **TXE-7706** (#372) a Veo create on a custom provider gets its base provider's default duration, so it is priced.
- fork: **TXE-7373** (#300) the cost breakdown gains a video category.
- fork: **TXE-7358** (#285) the older names for the cache-read rate and the per-request fee are read.
- fork: **TXE-7359** (#278) the Databricks DBU keys are recorded as deliberately ignored, with the reason.

### Contract C6 errors
- fork: **TXE-7634** (#365) every error core raises declares a C6 `error_type`; `_OTHER` no longer comes from core.
- fork: **TXE-7651** (#366) refusals core raises before dispatch declare `upstream_sent: false`, call-wide across fallbacks.
- fork: **TXE-6746** (#367) the vendor call reports `upstream_sent` from the shared HTTP client.
- fork: **TXE-7047** (#327) a failed async job's stored error carries `extra_fields.error_type`.

### Providers, video, image and batch
- fork: **TXE-6537** (#170) Vertex Veo 3.1 video generation.
- fork: **TXE-6428** (#171) MiniMax video v1 (Hailuo) and v2 (H3) meet the contract.
- fork: **TXE-7493** (#332) VolcEngine and ModelArk video speak Ark's task shape through byteplus's converters.
- fork: **TXE-7050** (#336) DashScope Wan text-to-image and Wan image edit on the image task routes.
- fork: **TXE-6597** (#154) Anthropic Message Batches take inline OpenAI-shape chat lines and return OpenAI-shape results.
- fork: **TXE-7045** (#337) the video task object carries the vendor's own resolution and aspect ratio.
- fork: **TXE-7239** (#335) the video task object carries input tokens by modality, filled for Gemini Omni on gemini and vertex.
- fork: **TXE-7374** (#288) a video create with a `video_uri` the provider cannot send is refused, naming it.
- fork: **TXE-7096** (#298) model and video calls follow `request_path_overrides` on the other bases; gemini and kling refuse their video ones.
- fork: **TXE-7058** (#333) a Vertex or Gemini batch line sent as it is has its url checked.

### Routing and transport
- fork: **TXE-7042** (#218) a repeated query parameter keeps every value through the transport plugins.
- fork: **TXE-7086** (#227) a repeated vendor query parameter on the list routes reaches the provider with every value.
- fork: **TXE-7087** (#236) passthrough routes forward the query the transport plugins left.
- fork: **TXE-7102** (#237) a boolean vendor query parameter on `GET /v1/models` reaches the vendor.
- fork: **TXE-7117** (#242) the HuggingFace model hub lookup follows the configured base URL.
- fork: **TXE-7222** (#324) a provider's forced extra-params merge stays off for the fallback.
- fork: **TXE-7301** (#334) no provider walks on to another key after an answer past the response limit.

### Breaking changes
- breaking: **TXE-7634** `error_type` is a C6 type on every error core raises, and the status moves from 400 to 500 or 503 for the Bifrost-fault and request-shed errors.
- breaking: **TXE-7502** a Go SDK caller sending a raw body on a priced image operation to a provider without a reader gets `400 raw_request_body_unsupported`; over HTTP, a GenAI Gemini image body naming a pricing field twice is refused.
- breaking: **TXE-6537** on gemini and vertex video create, a `size` outside the table, an `http(s)` frame and (on Vertex) an undeclared unified field return 400 instead of being replaced or dropped; the create response no longer copies `seconds` from the request; a Cloud Storage video is a `content` entry.
- breaking: **TXE-6428** MiniMax video no longer clamps `seconds`, rounds `size` or writes a default `ratio`; those requests get 400 `caller_unsupported_parameter`.
- breaking: **TXE-7493** `volcengine` and `modelark` video use Ark's task shape; on `byteplus` a delete answers the suffixed id.
- breaking: **TXE-7374** a video create with a non-blank `video_uri` on Replicate or the OpenAI video form (OpenAI, Azure v1 videos, Volcengine, a custom OpenAI base) is refused with 400 `caller_unsupported_parameter`.
- breaking: **TXE-7096** a gemini- or kling-based custom provider with a `video_retrieve` or `video_download` override is refused when written; a custom provider's model and video overrides on the other bases are now followed.
- breaking: **TXE-7086** repeating `endpoint` (Cohere), a `by*` filter, `s3_bucket` or `s3_prefix` (Bedrock) on the list routes gets 400.
- breaking: **TXE-7102** `GET /v1/models?provider=cohere&default_only=<x>` with a value other than `true` or `false` gets 400.
- breaking: **TXE-7117** a HuggingFace provider with a `base_url` reads `/api/models` from that host; serve it there or leave `base_url` unset.
- breaking: **TXE-7042** `HTTPRequest` gains a field and the `SetQuery` and `QueryAssigned` methods; a native `.so` plugin is rebuilt against this core.
- breaking: **TXE-7494** an unpaged batch results read past `client.max_response_body_size_mb` answers 400 and asks the caller to page.
- breaking: **TXE-7058** a native batch line with a url other than `/v1/chat/completions` is refused at create.
- breaking: **TXE-7499** a price field sent through extra params on an image task create is refused with 400 `caller_unsupported_parameter`.
- migrations: none. `framework/configstore/migrations.go` and `framework/logstore/migrations.go` are unchanged since v2.4.0-sync.11, so v2.4.0-sync.11 runs on this release's schema and a rollback needs no schema step.

### CI and tests (no runtime change)
- ci: **TXE-7511** (#358) a push releases only when it changes a version file; a dispatch is the retry. **TXE-7566** (#359) only a run that may release joins the release concurrency group; dispatch with `gh workflow run release.yml`.
- ci: **TXE-7508** (#347), **TXE-7530** (#357), **TXE-7592** (#360), **TXE-7621** (#364), **TXE-7684** (#369), **TXE-7704** (#371), **TXE-7106** (#230), **TXE-7113** (#235), **TXE-7137** (#243), **TXE-7066** (#219), **TXE-7103** (#328), **TXE-7733** (#373): E2E UI shard 1 waits on state, live tests run only through `core/livetest`, the docs broken-links check on every PR, Go module proxy retries, CI images from a digest-pinned GHCR mirror with retried pulls, fixture port checks, the plugin release dependency test, harness fixture rows, overhead reset counting, harness report merges, and offline UI skips that must be listed.
- test: **TXE-7392** (#304), **TXE-7394** (#305), **TXE-7393** (#306), **TXE-7396** (#307), **TXE-7397** (#313), **TXE-7398** (#308), **TXE-7399** (#309), **TXE-7400** (#310), **TXE-7404** (#311), **TXE-7412** (#312), **TXE-7395** (#321): UI e2e specs run offline and leave the quarantine list.
- test: **TXE-7643** (#368) the Agent Gateway shared-initialization test holds discovery until all ten callers have joined.
- test: each ticket ships unit tests in the packages it changes and provider-harness cases validated offline. No live provider run is part of this release (`--skip-core-test`).
- preserved: everything v2.4.0-sync.11 and earlier releases listed under Fork changes
- note: module versions. This fork publishes no core, framework or plugin module versions: their module paths stay `github.com/capsohq/bifrost/...`, their version files and the transport's requires are unchanged, and the workspace builds them from source
- chore: the Helm chart is 2.1.52-sync.12, a prerelease; `appVersion` is 2.4.0-sync.12, so an empty `image.tag` selects `v2.4.0-sync.12`. Its templates and values are unchanged from 2.1.52-sync.11
- note: the CLI and Migration CLI are unchanged: v1.0.1 and v0.1.1 remain their stable releases
- note: the image and the OCI chart are private packages, so pulling them needs a token with `read:packages`
