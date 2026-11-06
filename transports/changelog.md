## ✨ Upstream

- feat: **Codex with ChatGPT sign-in** - `/chatgpt_passthrough` now forwards `GET /backend-api/codex/models` alongside `POST /backend-api/codex/responses`, so Codex signed in with ChatGPT can run through Bifrost with the virtual key in `x-bf-vk`, following OpenAI's gateway guide. The model list skips the virtual key's model allowlist because it runs no model; each responses turn is still checked, and budgets and rate limits still count the usage ChatGPT reports (upstream 43e55b2a6, #9196)
- fix: OpenAI passthrough with a caller ChatGPT/Codex token no longer swaps in a stored OpenAI API key, which the ChatGPT backend refused with `api_key_not_supported`. Key selection is skipped only for OpenAI passthrough and passthrough-stream requests, the way it already was for Claude Code on Anthropic; OpenAI inference requests still select their own key (upstream 43e55b2a6)
- security: only a token ChatGPT issued (a JWT whose `iss` is `https://auth.openai.com`) is forwarded, and only to `https://chatgpt.com`; any other bearer on that route keeps the stored OpenAI key, and `/openai_passthrough` never forwards a caller JWT (upstream 43e55b2a6)
- fix: an OpenAI passthrough Responses stream finishes on `response.completed`, `response.failed`, `response.incomplete` or `error` and abandons the upstream body instead of draining it, so Codex hanging up on the end event no longer logs the request as a 499 without usage. Chat Completions chunks carry no top-level `type` and still end on EOF (upstream 43e55b2a6, PR #9177)
- docs: the Codex CLI guide covers sign-in with ChatGPT through Bifrost and the Okta access-token claims for gateway sign-in (upstream 43e55b2a6, 2c54dd14b #9207)
- test: provider-harness folders 257 (ChatGPT model list and stored-key skip; they skip unless `CHATGPT_ACCESS_TOKEN` is set) and 280 (passthrough Responses end event)

## 🍴 Fork changes (txehq/bifrost)

- chore: v2.4.0-sync.2 is a prerelease on top of v2.4.0-sync.1, the second batch toward upstream transports/v2.4.0 (TXE-5868). It merges upstream through 2c54dd14b; v2.3.5 stays the stable release and `latest`
- fork: the end-event check reads the event type in place instead of copying it, so it adds no allocation per Responses event; as merged it added one per event (a 50-delta stream went from 257 to 309 allocations)
- test: offline regression tests drive the real OpenAI passthrough stream against an httptest upstream: one completion with the terminal usage per end event while the upstream stays open, bytes forwarded in order before the final chunk, RawRequest only on the final chunk, the Chat Completions usage chunk after `finish_reason` kept, one 499 on a hangup before the end event, one terminal call on an upstream abort
- test: the OpenAI passthrough key skip reaches no OpenAI inference stream and no other provider (Gemini, ModelArk), and `/openai_passthrough` never forwards a ChatGPT-issued token, never skips key selection and never skips the model allowlist
- note: module versions. This fork publishes no core, framework or plugin module versions: their module paths stay `github.com/capsohq/bifrost/...`, their version files and the transport's requires are unchanged, and the workspace builds them from source. Upstream's core changelog entry is folded in above
- preserved: everything v2.4.0-sync.1, v2.3.5 and earlier releases listed under Fork changes
- chore: the Helm chart is 2.1.52-sync.2, a prerelease; `appVersion` is 2.4.0-sync.2, so an empty `image.tag` selects `v2.4.0-sync.2`
- note: the CLI and Migration CLI are unchanged: v1.0.1 and v0.1.1 remain their stable releases
- note: the image and the OCI chart are private packages, so pulling them needs a token with `read:packages`
