## ✨ Features

[feat]: expose native ElevenLabs custom channels and supported base providers (TXE-5087)
- **Pinned Keys on Routing Fallbacks** - Each routing-rule fallback can pin a provider key via `key_id`, or `provider_key_name` in config.json. The UI rule editor lets you pick or clear a key per fallback. Unpinned fallbacks keep the legacy `provider/model` string, so existing rules keep their config hash (#7470, #7379, #7380, #7381)
- **OpenAI Async Tool Execution** - The `async` flag on Responses tools and tool calls, `output_schema` on function tools and `tunnel_id` on MCP tools are now forwarded to OpenAI. `async` is stripped for models without support, and the datasheet `supports_async_tools` field can override this (#7242)
- **GPT-6 Prompt Cache Breakpoints** - Prompt-cache breakpoints now cover the GPT-6 family on OpenAI, Azure, Bedrock and Bedrock Mantle. The datasheet `supports_prompt_cache_breakpoint` field can override this (#7240)
- **GPT-6 Sol and Luna Reasoning Off** - `reasoning.effort: "none"` is forwarded for `gpt-6-sol` and `gpt-6-luna`. Other GPT-6 models keep reasoning on (#7492)

## 🐞 Fixed

[fix]: preserve key snapshots and bound provider-read database contexts (TXE-5087)
- **OpenAI Sampling Parameters on Reasoning Models** - `temperature`, `top_logprobs` and `logprobs` are now stripped alongside `top_p` on chat and Responses when the model and effort do not support them. An omitted `reasoning.effort` now counts as `none` only for models that default to no reasoning (#7239)
- **Responses API Wire Shapes** - Structured MCP tool-call errors, object-form `conversation`, array-form MCP `allowed_tools`, `approval_request_id` on MCP approval responses, and `in`/`nin` file search filters now decode and re-encode correctly (#7241)
  <Warning>Go SDK callers: `ResponsesMCPApprovalResponse.ApprovalResponseID` is now `ApprovalRequestID`, the message type is now `mcp_approval_response`, and `ResponsesToolMessage.Error` and `ResponsesParameters.Conversation` are now union types, where they used to be `*string`.</Warning>
- **OpenRouter Anthropic Cache Breakpoints** - Anthropic models routed through OpenRouter now keep their `cache_control` breakpoints, based on the model capability (#7521)
- **Session Affinity with Pinned Keys** - When session affinity reorders the chain, a routing rule's key pin now moves with its provider, so the pinned key is never looked up under the wrong provider (#7468)
- **Session Affinity Route Matching** - A session's route is now matched on provider and model together. Bindings the request followed into a failure are dropped (#7473)
- **Databricks Gemini System Prompts** - Multiple system and developer messages are merged into one for Gemini models hosted on Databricks, which reject more than one system prompt (#7461)
- **Bedrock Encrypted Reasoning Replay** - Bedrock's "encrypted reasoning was created for a different account or model" error now triggers the strip-and-retry path for unverifiable reasoning
- **Decisions on Bedrock Mantle** - Decision emulation now sends `tool_choice: "auto"` for gpt-oss models on Bedrock Mantle, which reject `"required"`. Leaked parameter tags with surrounding whitespace are now recovered
- **Gemini Transcription Usage** - Usage is reported even when the transcript is empty
- **Routing Rule Enabled State** - Syncing or updating a routing rule that omits `enabled` keeps the stored value, where it used to write NULL
- **Telemetry User Labels Toggle** - `user_labels_enabled` is now saved with the telemetry config (#7490)

## 🗄️ Database Migrations

- No new database migrations in this release.

## 🍴 Fork changes (txehq/bifrost)

- chore: v2.2.3 is upstream `transports/v2.2.3` (411d62b28) merged onto the fork's v2.2.1, keeping the fork's Go module paths, custom providers (ModelArk, Volcengine), native Anthropic Messages and xAI passthrough, separate streaming and unary clients, the legacy realtime session endpoints and Bifrost-minted realtime secrets, and release guards. Core, framework and plugins are built from the workspace and are not tagged separately
- fix: xAI's native Anthropic Messages route strips Claude Code's `safeguards` and `diagnostics` fields and its per-request `x-anthropic-billing-header` system line before the request leaves for xAI, as upstream #7393, #7243 and #7431 do for every converted non-Anthropic request; the billing line changes on every request and would otherwise defeat xAI's prompt cache. The Anthropic route still forwards them
- fix: a decision answered by emulation (upstream #7361) is billed as the text call it is, from the model's chat or responses pricing row with its cache rates; it previously found no `decisions` row and cost nothing. A decision answer Bifrost rejects, from Typesafe or from emulation, still charges the usage the provider billed
- fix: decision emulation on a custom provider wrapping Bedrock Mantle sends Mantle's `auto` tool choice
- fix: a stream paused for approval that overflows its replay cap delivers only approved content before the overflow error
- note: MCP HTTP and SSE connections honour `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` again (upstream #7437). Through a proxy, an IP-literal link-local, unspecified or metadata destination is refused before the proxy is contacted, and, unlike upstream, a hostname must resolve on the gateway to addresses that pass the same block, or the connection is refused; the proxy is never handed a name the gateway could not check
- note: the release pipeline gains upstream's `test-memory` allocation gate; its break-glass job uses the `memory-regression-override` environment
