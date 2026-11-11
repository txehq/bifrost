import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import http from "node:http";
import https from "node:https";
import net from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";

const port = Number(process.env.PROVIDER_ERROR_FIXTURE_PORT || "8791");
// The stall proxy: an HTTP proxy that accepts the TCP connection and never answers the CONNECT, so a
// gateway routed through it stalls inside connection acquisition (the proxy handshake), before any
// request reaches an upstream. Each CONNECT it reads is counted under __stall_proxy_connects in /__hits.
const stallPort = Number(process.env.PROVIDER_ERROR_FIXTURE_STALL_PORT || "8799");
// The TLS listener (TXE-7083): the same routes over https, for providers that always dial https:// and take a
// host override rather than network_config.base_url. Bedrock is the case: bedrock_key_config.endpoints.runtime
// set to 127.0.0.1:<this port> plus network_config.insecure_skip_verify reaches it. The certificate is a
// throwaway self-signed one made with openssl at start-up; without openssl the listener is skipped.
const tlsPort = Number(process.env.PROVIDER_ERROR_FIXTURE_TLS_PORT || "8793");
// How long a stalled connection is held before the fixture drops it, longer than any first-token deadline
// a case arms and shorter than a proxy handshake timeout, so a gateway that waits for the proxy still fails.
const STALL_HOLD_MS = 15000;
// The media tunnel (TXE-7036): an HTTP proxy that answers every CONNECT and joins the tunnel to this
// fixture's own port, whatever address the CONNECT names. The SSRF-hardened media fetcher refuses a
// loopback URL, so a vendor answer names its file at a public-looking address (203.0.113.0/24, RFC 5737)
// and the provider's proxy_config points here: the gateway checks the address, asks this proxy for a tunnel
// to it, and the request arrives at the fixture with that address in its Host header. Each tunnel is
// counted under "__media_tunnel_connects@<host:port>" in /__hits, so a case can prove the download left
// through the provider's proxy, to the address the vendor named.
const mediaTunnelPort = Number(process.env.PROVIDER_ERROR_FIXTURE_MEDIA_TUNNEL_PORT || "8788");
// The address a fixture task names its file at: TEST-NET-3 (RFC 5737), public to the gateway's SSRF check and
// routed nowhere, so the file is reachable through the media tunnel only.
const MEDIA_PUBLIC_HOST = "203.0.113.10";

// Per-model behaviour for POST /v1/chat/completions. The model name in the request
// body selects the response, so one fixture serves every case. A model may carry a
// "~tag" suffix ("upstream-500~ps02-a-run-1"): the part before "~" selects the
// behaviour and hits are counted under the full name, so a case that sends its own
// tag (through a key alias) counts its own attempts without resetting anyone else's.
const errorBody = (message, type, code) => JSON.stringify({ error: { message, type, code } });

// How long upstream-slow waits before it answers, long enough that a 500ms first-token
// deadline always fires first and short enough to stay inside any request timeout.
const SLOW_MS = 2000;

const behaviours = {
	"upstream-500": () => ({ status: 500, headers: {}, body: errorBody("internal server error", "server_error", "internal_error") }),
	"upstream-503": () => ({ status: 503, headers: {}, body: errorBody("service unavailable", "server_error", "service_unavailable") }),
	"upstream-429": () => ({ status: 429, headers: { "Retry-After": "7" }, body: errorBody("rate limit exceeded", "rate_limit_error", "rate_limit_exceeded") }),
	"upstream-429-ms": () => ({ status: 429, headers: { "retry-after-ms": "2500" }, body: errorBody("rate limit exceeded", "rate_limit_error", "rate_limit_exceeded") }),
	// A content block under a 403, OpenAI's shape: the caller's content, not the key (contract C6).
	// A speech 200 with an empty body and no error object: the vendor's fault (contract C6).
	"upstream-speech-empty": () => ({ status: 200, headers: { "Content-Type": "audio/mpeg", "x-request-id": "fixture-speech-empty" }, body: "" }),
	"upstream-403-content-block": () => ({ status: 403, headers: {}, body: errorBody("Your request was rejected as a result of our safety system.", "invalid_request_error", "content_policy_violation") }),
	"upstream-ok": () => ({
		status: 200,
		headers: {},
		body: JSON.stringify({
			id: "chatcmpl-upstream-ok",
			object: "chat.completion",
			created: 1,
			model: "upstream-ok",
			choices: [{ index: 0, message: { role: "assistant", content: "hello" }, finish_reason: "stop" }],
			usage: { prompt_tokens: 10, completion_tokens: 1, total_tokens: 11 },
		}),
	}),
	// upstream-ok with a cached part of the input, for the rows that read token totals back.
	"upstream-cached-tokens": () => ({
		status: 200,
		headers: {},
		body: JSON.stringify({
			id: "chatcmpl-upstream-cached-tokens",
			object: "chat.completion",
			created: 1,
			model: "upstream-cached-tokens",
			choices: [{ index: 0, message: { role: "assistant", content: "hello" }, finish_reason: "stop" }],
			usage: { prompt_tokens: 20, completion_tokens: 2, total_tokens: 22, prompt_tokens_details: { cached_tokens: 7 } },
		}),
	}),
};
// Azure OpenAI's documented answer to a prompt its content filter blocked, TXE-6527 (contract C6): HTTP 400 with
// error.code "content_filter" and the per-category verdicts under innererror.
behaviours["azure-content-filter"] = () => ({
	status: 400,
	headers: {},
	body: JSON.stringify({
		error: {
			message: "The response was filtered due to the prompt triggering Azure OpenAI's content management policy. Please modify your prompt and retry. To learn more about our content filtering policies please read our documentation: https://go.microsoft.com/fwlink/?linkid=2198766",
			type: null,
			param: "prompt",
			code: "content_filter",
			status: 400,
			innererror: {
				code: "ResponsibleAIPolicyViolation",
				content_filter_result: {
					hate: { filtered: false, severity: "safe" },
					jailbreak: { filtered: true, detected: true },
					self_harm: { filtered: false, severity: "safe" },
					sexual: { filtered: false, severity: "safe" },
					violence: { filtered: false, severity: "safe" },
				},
			},
		},
	}),
});
// upstream-slow answers as upstream-ok does, after SLOW_MS.
behaviours["upstream-slow"] = () => ({ ...behaviours["upstream-ok"](), delayMs: SLOW_MS });
// upstream-ok with every usage field a vendor reports beyond tokens, TXE-6522 (contract C10 BF21): web search calls,
// a reported cost and xAI's cost_in_usd_ticks. Streamed, they arrive once, on the trailing usage frame, and must reach
// the gateway's final frame as they reach its non-streamed response.
const extendedUsage = {
	prompt_tokens: 120, completion_tokens: 48, total_tokens: 168,
	completion_tokens_details: { reasoning_tokens: 12, num_search_queries: 2 },
	tool_usage: { web_search: { num_requests: 2 } },
	cost: { total_cost: 0.0123 },
	cost_in_usd_ticks: 123000000,
};
behaviours["upstream-usage-extended"] = () => {
	const r = behaviours["upstream-ok"]();
	const completion = JSON.parse(r.body);
	return { ...r, body: JSON.stringify({ ...completion, id: "chatcmpl-upstream-usage-extended", model: "upstream-usage-extended", usage: extendedUsage }) };
};
// Text completions for upstream-usage-extended: one text frame with the finish reason, then the usage frame.
const textCompletionAnswer = (stream) => {
	const base = { id: "cmpl-upstream-usage-extended", object: "text_completion", created: 1, model: "upstream-usage-extended" };
	if (!stream) return { ...base, choices: [{ index: 0, text: "hello", finish_reason: "stop" }], usage: extendedUsage };
	return [
		{ ...base, choices: [{ index: 0, text: "hello", finish_reason: "stop" }] },
		{ ...base, choices: [], usage: extendedUsage },
	].map((c) => `data: ${JSON.stringify(c)}\n\n`).join("") + "data: [DONE]\n\n";
};
// upstream-cut starts answering as upstream-ok does and drops the connection partway: a stream gets its first
// content chunk and then a closed socket, a non-streamed request a closed socket.
behaviours["upstream-cut"] = () => ({ ...behaviours["upstream-ok"](), cut: true });
// upstream-idle-stream answers a streamed request in two writes with a pause between them longer than the
// gateway's SSE heartbeat interval, so a passthrough case can tell the vendor's bytes from a heartbeat the
// gateway added; the bytes it sent are kept under /__sent (folder 329, TXE-6519).
behaviours["upstream-idle-stream"] = () => ({ ...behaviours["upstream-ok"](), idleMs: 2500 });

// A streamed chat request (stream: true) that a behaviour answers with a completion gets the same
// answer as server-sent chunks: the role, the content, the finish reason, the usage when
// stream_options.include_usage asks for it, then [DONE].
const streamChunks = (completion, includeUsage) => {
	const base = { id: completion.id, object: "chat.completion.chunk", created: completion.created, model: completion.model };
	const choice = completion.choices[0];
	const chunks = [
		{ ...base, choices: [{ index: 0, delta: { role: "assistant", content: "" }, finish_reason: null }] },
		{ ...base, choices: [{ index: 0, delta: { content: choice.message.content }, finish_reason: null }] },
		{ ...base, choices: [{ index: 0, delta: {}, finish_reason: choice.finish_reason }] },
	];
	if (includeUsage) chunks.push({ ...base, choices: [], usage: completion.usage });
	return chunks.map((c) => `data: ${JSON.stringify(c)}\n\n`).join("") + "data: [DONE]\n\n";
};

// Hit counts per model let a case prove a failover really tried the primary once and the
// fallback once. The count endpoints answer with {data: [...]} so the harness's generic
// response-shape check accepts them.
// Azure serves chat under /openai/v1 and decorates the response with content-filter
// annotations: a prompt_filter_results array and a per-choice content_filter_results object.
const filterOk = { hate: { filtered: false, severity: "safe" }, violence: { filtered: false, severity: "safe" } };
const azureFiltered = () => ({
	status: 200,
	headers: {},
	body: JSON.stringify({
		id: "chatcmpl-azure-filtered",
		object: "chat.completion",
		created: 1,
		model: "azure-filtered",
		prompt_filter_results: [{ prompt_index: 0, content_filter_results: filterOk }],
		choices: [{ index: 0, finish_reason: "stop", message: { role: "assistant", content: "hello" }, content_filter_results: filterOk }],
		usage: { prompt_tokens: 10, completion_tokens: 1, total_tokens: 11 },
	}),
});

// OpenAI pairs a reasoning item's id with its encrypted_content and rejects a replay whose id does not
// match the token. The fixture issues one reasoning item and enforces that pairing on replay, which is
// how a gateway that mints a fresh id on replay shows up as a 400.
const REASONING_ID = "rs_fixture_original";
const REASONING_TOKEN = "enc-token-fixture";
const responsesFor = (body, count) => {
	const input = Array.isArray(body.input) ? body.input : [];
	for (const item of input) {
		if (item && item.type === "reasoning" && item.encrypted_content === REASONING_TOKEN) {
			if (item.id !== REASONING_ID) {
				count("replay-id-mismatch");
				return { status: 400, headers: {}, body: errorBody(`Item '${item.id}' of type 'reasoning' was provided without its required encrypted_content pairing`, "invalid_request_error", "invalid_encrypted_content") };
			}
			count("replay-id-ok");
		}
	}
	return {
		status: 200,
		headers: {},
		body: JSON.stringify({
			id: "resp_fixture_1",
			object: "response",
			created_at: 1,
			status: "completed",
			model: "gpt-5-pro",
			output: [
				{ id: REASONING_ID, type: "reasoning", summary: [{ type: "summary_text", text: "thought it through" }], encrypted_content: REASONING_TOKEN },
				{ id: "msg_fixture_1", type: "message", role: "assistant", status: "completed", content: [{ type: "output_text", text: "hello", annotations: [] }] },
			],
			usage: { input_tokens: 10, output_tokens: 5, total_tokens: 15 },
		}),
	};
};

// Injected-tool turns, TXE-5518. Each model answers its first request with a call to the injected web_search tool
// and the request that carries the tool's result with a final answer, so one client request is two provider calls.
// Both report 150k input tokens: priced one at a time they stay under the fixture's 272k threshold, priced as a sum
// they cross it. turns-tiered answers its first turn at priority and its second at default. turns-fail calls the
// tool twice and fails its third turn. The row names its behaviour by model, so every shape shares one script.
const TURN_USAGE = { input: 150000, output: 1000 };
const isTurnsModel = (name) => name === "turns-priced" || name === "turns-tiered" || name === "turns-fail";
const chatTurnsDone = (body) => (Array.isArray(body.messages) ? body.messages : []).filter((m) => m && m.role === "tool").length;
const responsesTurnsDone = (body) => (Array.isArray(body.input) ? body.input : []).filter((i) => i && i.type === "function_call_output").length;
const turnTier = (name, second) => (name === "turns-tiered" && !second ? "priority" : "default");
const chatUsage = () => ({ prompt_tokens: TURN_USAGE.input, completion_tokens: TURN_USAGE.output, total_tokens: TURN_USAGE.input + TURN_USAGE.output });
const responsesUsage = () => ({ input_tokens: TURN_USAGE.input, output_tokens: TURN_USAGE.output, total_tokens: TURN_USAGE.input + TURN_USAGE.output });
const chatTurn = (name, second) => {
	const message = second
		? { role: "assistant", content: "It is sunny." }
		: { role: "assistant", content: null, tool_calls: [{ id: "call_turn_1", type: "function", function: { name: "web_search", arguments: "{\"query\":\"weather\"}" } }] };
	return {
		id: `chatcmpl-${name}`, object: "chat.completion", created: 1, model: name, service_tier: turnTier(name, second),
		choices: [{ index: 0, message, finish_reason: second ? "stop" : "tool_calls" }],
		usage: chatUsage(),
	};
};
const chatTurnStream = (completion) => {
	const base = { id: completion.id, object: "chat.completion.chunk", created: 1, model: completion.model, service_tier: completion.service_tier };
	const { message, finish_reason: finish } = completion.choices[0];
	const delta = message.tool_calls
		? { role: "assistant", tool_calls: [{ index: 0, ...message.tool_calls[0] }] }
		: { role: "assistant", content: message.content };
	const chunks = [
		{ ...base, choices: [{ index: 0, delta, finish_reason: null }] },
		{ ...base, choices: [{ index: 0, delta: {}, finish_reason: finish }] },
		{ ...base, choices: [], usage: completion.usage },
	];
	return chunks.map((c) => `data: ${JSON.stringify(c)}\n\n`).join("") + "data: [DONE]\n\n";
};
const responsesTurn = (name, second) => {
	const item = second
		? { id: "msg_turn_2", type: "message", role: "assistant", status: "completed", content: [{ type: "output_text", text: "It is sunny.", annotations: [] }] }
		: { id: "fc_turn_1", type: "function_call", status: "completed", call_id: "call_turn_1", name: "web_search", arguments: "{\"query\":\"weather\"}" };
	return { id: `resp_${name}_${second ? 2 : 1}`, object: "response", created_at: 1, status: "completed", model: name, service_tier: turnTier(name, second), output: [item], usage: responsesUsage() };
};
const responsesTurnStream = (response) => {
	const pending = { ...response, status: "in_progress", output: [], usage: null };
	const item = response.output[0];
	const events = [
		{ type: "response.created", sequence_number: 0, response: pending },
		{ type: "response.output_item.added", sequence_number: 1, output_index: 0, item },
		{ type: "response.output_item.done", sequence_number: 2, output_index: 0, item },
		{ type: "response.completed", sequence_number: 3, response },
	];
	return events.map((e) => `event: ${e.type}\ndata: ${JSON.stringify(e)}\n\n`).join("");
};

// The injected tool behind the turns models, as a Streamable HTTP MCP server answering plain JSON. It exists so the
// loop runs in the gateway: the fixture's one tool returns a fixed text.
const mcpAnswer = (message) => {
	const reply = (result) => ({ jsonrpc: "2.0", id: message.id, result });
	switch (message.method) {
		case "initialize":
			return reply({ protocolVersion: message.params?.protocolVersion || "2025-03-26", capabilities: { tools: {} }, serverInfo: { name: "provider-error-fixture", version: "1.0.0" } });
		case "ping":
			return reply({});
		case "tools/list":
			return reply({ tools: [{ name: "web_search", description: "Search the web.", inputSchema: { type: "object", properties: { query: { type: "string" } }, required: ["query"] } }] });
		case "tools/call":
			hits.__mcp_tool_calls = (hits.__mcp_tool_calls || 0) + 1;
			return reply({ content: [{ type: "text", text: "Sunny." }], isError: false });
		default:
			return { jsonrpc: "2.0", id: message.id, error: { code: -32601, message: "method not found" } };
	}
};

// An OpenAPI document with no servers, TXE-5533: its operations are served here, so the gateway's default server has
// to be this origin. fixture_openapi in provider-error-fixture.config.json reads it. The description is long and
// multibyte so the client's instruction cap cuts inside a character unless the cut respects them.
const openAPIDocument = {
	openapi: "3.0.3",
	info: { title: "Fixture API", version: "1.0.0", description: "déjà vu ".repeat(40).trim() },
	paths: {
		"/fixture-api/items/{id}": {
			put: {
				operationId: "putItem",
				parameters: [{ name: "id", in: "path", required: true, schema: { type: "string" } }],
				requestBody: { required: true, content: { "application/json": { schema: { type: "object", properties: { name: { type: "string" } } } } } },
				responses: { 200: { description: "ok" } },
			},
		},
		"/fixture-api/profiles": {
			post: {
				operationId: "createProfile",
				requestBody: {
					required: true,
					content: {
						"application/json": {
							schema: {
								type: "object",
								required: ["id", "email"],
								properties: { id: { type: "string", minLength: 3 } },
								allOf: [
									{
										oneOf: [
											{ type: "object", properties: { email: { type: "string", format: "email" } } },
											{ type: "object", properties: { phone: { type: "string", pattern: "^[0-9]+$" } } },
										],
									},
								],
							},
						},
					},
				},
				responses: { 200: { description: "ok" } },
			},
		},
	},
};

// What reached the document's operations, in order; POST /__reset clears it.
const openAPICalls = [];

// Decision answers the conversion rejects, TXE-5518: no answers for the question asked, though the model ran and
// reported usage. OpenRouter also reports what it billed.
const decisionUsage = { input_tokens: 1000, output_tokens: 10, total_tokens: 1010 };
const nativeDecisionAnswer = (body) => {
	if (body.model === "gpt-6-luna") {
		return { model: "gpt-6-luna-2026", answers: [], usage: decisionUsage };
	}
	return undefined;
};
const openRouterDecisionAnswer = () => ({ model: "typesafe/jev-1.13.0", answers: {}, usage: { input_tokens: 1000, output_tokens: 10, cost: 0.25 } });

// The rows the per-call pricing folder is judged against. Every other fixture model stays unpriced.
const turnRates = { mode: "chat", input_cost_per_token: 0.000001, output_cost_per_token: 0.000002, input_cost_per_token_above_272k_tokens: 0.00001, output_cost_per_token_above_272k_tokens: 0.00002, input_cost_per_token_priority: 0.000002, output_cost_per_token_priority: 0.000004 };
const fixturePricing = {
	"turns-priced": { provider: "openai_turns", ...turnRates },
	"turns-tiered": { provider: "openai_turns", ...turnRates },
	"turns-fail": { provider: "openai_turns", ...turnRates },
	"gpt-6-luna": { provider: "openai", mode: "chat", input_cost_per_token: 0.000001, output_cost_per_token: 0.000002, input_cost_per_token_decisions: 0.000003, output_cost_per_token_decisions: 0.000004, supports_decisions: true },
	"typesafe/jev-1.13": { provider: "openrouter", mode: "chat", input_cost_per_token: 0.000001, output_cost_per_token: 0.000002 },
	"decision-emulated": { provider: "openai", mode: "chat", input_cost_per_token: 0.000001, output_cost_per_token: 0.000002, cache_read_input_token_cost: 0.0000001 },
	"pp-custom-legacy": { provider: "openai", mode: "responses", input_cost_per_token: 0.000001, output_cost_per_token: 0.000002 },
	// TXE-6526: a token-priced Gemini image model, for the row that reads back the cost of a response whose usage
	// carries thinking tokens and no modality breakdown.
	"fixture-image-thoughts-only": { provider: "gemini", mode: "image_generation", input_cost_per_token: 0.000001, output_cost_per_token: 0.00003 },
	// A per-image-priced Gemini image model whose answer is a safety block: nothing delivered, nothing charged per image.
	"fixture-image-safety": { provider: "gemini", mode: "image_generation", input_cost_per_token: 0.000001, output_cost_per_image: 0.04 },
	// A provider the datasheet knows and list-models cannot enumerate, for run-bounds-wire.mjs.
	"fireworks_ai/accounts/fixture/models/known": { provider: "fireworks_ai", mode: "chat", input_cost_per_token: 0.000001, output_cost_per_token: 0.000002 },
};

// Provider wire capture, TXE-5539. The config points each provider these rows use at /pp/<provider>, so one fixture
// plays them all; the bytes that arrive are kept for the row to compare, and GET /__wire returns them. Nothing here
// keeps a header, so no key reaches a report.
const wireCalls = [];
const anthropicMessage = (model) => ({
	id: "msg_fixture", type: "message", role: "assistant", model,
	content: [{ type: "text", text: "hello" }],
	stop_reason: "end_turn", stop_sequence: null, usage: { input_tokens: 12, output_tokens: 3 },
});
const sse = (events) => events.map((e) => `event: ${e.type}\ndata: ${JSON.stringify(e)}\n\n`).join("");
const anthropicMessageStream = (model) => sse([
	{ type: "message_start", message: { ...anthropicMessage(model), content: [], stop_reason: null } },
	{ type: "content_block_start", index: 0, content_block: { type: "text", text: "" } },
	{ type: "content_block_delta", index: 0, delta: { type: "text_delta", text: "hello" } },
	{ type: "content_block_stop", index: 0 },
	{ type: "message_delta", delta: { stop_reason: "end_turn", stop_sequence: null }, usage: { output_tokens: 3 } },
	{ type: "message_stop" },
]);
// Code execution with a thoughtSignature on both of its parts: dropping one breaks the next turn.
const geminiCodeExecution = {
	candidates: [{
		index: 0, finishReason: "STOP",
		content: { role: "model", parts: [
			{ executableCode: { language: "PYTHON", code: "print(1)" }, thoughtSignature: "c2lnLWNvZGU=" },
			{ codeExecutionResult: { outcome: "OUTCOME_OK", output: "1\n" }, thoughtSignature: "c2lnLXJlc3VsdA==" },
			{ text: "done" },
		] },
	}],
	usageMetadata: { promptTokenCount: 5, candidatesTokenCount: 2, totalTokenCount: 7 },
	modelVersion: "pp-gemini",
};
const clefAnswer = { result: { model: "clef-flash", answers: { is_red: { type: "noul", noul: 0.97 } }, usage: { input_tokens: 210, output_tokens: 0 } }, success: true, errors: [], messages: [] };
const embeddingAnswer = { object: "list", model: "pp-embedding", data: [{ object: "embedding", index: 0, embedding: [0.1, 0.2] }], usage: { prompt_tokens: 1, total_tokens: 1 } };
// A function_call answer for a custom tool the gateway sent as a function tool, for a model the datasheet marks as
// rejecting custom tools. x_fixture_raw exists only in these bytes, so a relay of them is recognisable.
const legacyCustomToolResponse = (model) => ({
	id: "resp_pp", object: "response", created_at: 1, status: "completed", model, x_fixture_raw: true,
	output: [{ id: "fc_pp", type: "function_call", status: "completed", call_id: "call_pp", name: "apply_patch", arguments: JSON.stringify({ input: "*** patch" }) }],
	usage: { input_tokens: 5, output_tokens: 2, total_tokens: 7 },
});
const legacyCustomToolStream = (model) => {
	const response = legacyCustomToolResponse(model);
	const item = response.output[0];
	let sequence_number = 0;
	const event = (e) => ({ ...e, sequence_number: sequence_number++ });
	return sse([
		event({ type: "response.created", response: { ...response, status: "in_progress", output: [] } }),
		event({ type: "response.output_item.added", output_index: 0, item: { ...item, status: "in_progress", arguments: "" } }),
		event({ type: "response.function_call_arguments.delta", output_index: 0, item_id: item.id, delta: item.arguments }),
		event({ type: "response.function_call_arguments.done", output_index: 0, item_id: item.id, arguments: item.arguments }),
		event({ type: "response.output_item.done", output_index: 0, item }),
		event({ type: "response.completed", response }),
	]);
};
// Core preservation, TXE-5540. The cp_injected provider injects the fixture's web_search; cp_plain injects nothing and
// is the fallback target. A model answers by how many tool results its request already carries, so a replayed turn is
// visible on the wire. cp-fail* fail with 503: cp-fail after a tool ran, cp-fail-first before any did.
const webSearchCall = { id: "fc_cp", type: "function_call", status: "completed", call_id: "call_cp", name: "web_search", arguments: "{\"query\":\"weather\"}" };
const clientCustomCall = { id: "ctc_cp", type: "custom_tool_call", status: "completed", call_id: "call_client", name: "code_exec", input: "print(1)" };
const cpMessage = (text) => ({ id: "msg_cp", type: "message", role: "assistant", status: "completed", content: [{ type: "output_text", text, annotations: [] }] });
const cpResponses = {
	"cp-custom": (done) => (done ? [cpMessage("Done.")] : [clientCustomCall, webSearchCall]),
	"cp-custom-final": (done) => (done ? [clientCustomCall] : [webSearchCall]),
	"cp-fail": () => [webSearchCall],
	"cp-fallback-target": () => [cpMessage("Served by the fallback.")],
};
const cpResponse = (model, output) => ({ id: `resp_${model}`, object: "response", created_at: 1, status: "completed", model, output, usage: { input_tokens: 5, output_tokens: 2, total_tokens: 7 } });
const cpResponseStream = (response) => {
	let sequence_number = 0;
	const event = (e) => ({ ...e, sequence_number: sequence_number++ });
	const items = response.output.flatMap((item, output_index) => [
		event({ type: "response.output_item.added", output_index, item }),
		event({ type: "response.output_item.done", output_index, item }),
	]);
	return sse([event({ type: "response.created", response: { ...response, status: "in_progress", output: [], usage: null } }), ...items, event({ type: "response.completed", response })]);
};
const cpChatCompletion = (model, choices) => ({ id: `chatcmpl-${model}`, object: "chat.completion", created: 1, model, choices, usage: { prompt_tokens: 5, completion_tokens: 2, total_tokens: 7 } });
const cpChatToolCall = { id: "call_cp", type: "function", function: { name: "web_search", arguments: "{\"query\":\"weather\"}" } };
const cpChatAnswer = (text) => ({ role: "assistant", content: text });
const cpChatToolTurn = { role: "assistant", content: null, tool_calls: [cpChatToolCall] };
// Each entry is one upstream chunk's choices, so a chunk can carry several choices or only another one. The OpenAI
// stream reader passes a chunk only on its first choice's content, so another choice's finish reason rides with text.
const cpChunks = (model, usage, ...chunkChoices) => {
	const base = { id: `chatcmpl-${model}`, object: "chat.completion.chunk", created: 1, model };
	const chunks = [...chunkChoices.map((choices) => ({ ...base, choices })), { ...base, choices: [], usage }];
	return chunks.map((c) => `data: ${JSON.stringify(c)}\n\n`).join("") + "data: [DONE]\n\n";
};
const cpUsage = { prompt_tokens: 5, completion_tokens: 2, total_tokens: 7 };
const cpDelta = (index, delta, finish_reason = null) => ({ index, delta, finish_reason });
const cpChatAnswerFor = (body) => {
	const done = chatTurnsDone(body) > 0;
	const stream = body.stream === true;
	const reply = (choices) => ({ status: 200, body: JSON.stringify(cpChatCompletion(body.model, choices)) });
	const failed = { status: 503, body: errorBody("service unavailable", "server_error", "unavailable") };
	switch (body.model) {
		case "cp-fail":
			if (done) return failed;
			return stream
				? { status: 200, stream: cpChunks(body.model, cpUsage, [cpDelta(0, { role: "assistant", tool_calls: [{ index: 0, ...cpChatToolCall }] })], [cpDelta(0, {}, "tool_calls")]) }
				: reply([{ index: 0, message: cpChatToolTurn, finish_reason: "tool_calls" }]);
		case "cp-fail-first":
			return failed;
		case "cp-other-choice":
			return done
				? { status: 200, stream: cpChunks(body.model, cpUsage, [cpDelta(0, { role: "assistant", content: "Sunny." }), cpDelta(1, { role: "assistant", content: "Cloudy." }, "length")], [cpDelta(0, {}, "stop")]) }
				: { status: 200, stream: cpChunks(body.model, cpUsage, [cpDelta(1, { role: "assistant", content: "Discarded draft." })], [cpDelta(0, { role: "assistant", tool_calls: [{ index: 0, ...cpChatToolCall }] })], [cpDelta(0, {}, "tool_calls")]) };
		case "cp-two-choices":
			if (stream) return { status: 200, stream: cpChunks(body.model, cpUsage, [cpDelta(0, { role: "assistant", content: "First." }), cpDelta(1, { role: "assistant", content: "Second." })], [cpDelta(0, {}, "stop"), cpDelta(1, {}, "length")]) };
			return reply([{ index: 0, message: cpChatAnswer("First."), finish_reason: "stop" }, { index: 1, message: cpChatAnswer("Second."), finish_reason: "length" }]);
		case "cp-injected-on-other":
			return reply([{ index: 0, message: cpChatAnswer("First."), finish_reason: "stop" }, { index: 1, message: cpChatToolTurn, finish_reason: "tool_calls" }]);
		default:
			return stream
				? { status: 200, stream: cpChunks(body.model, cpUsage, [cpDelta(0, { role: "assistant", content: "Served by the fallback." })], [cpDelta(0, {}, "stop")]) }
				: reply([{ index: 0, message: cpChatAnswer("Served by the fallback."), finish_reason: "stop" }]);
	}
};
const cpResponsesAnswerFor = (body) => {
	const done = responsesTurnsDone(body) > 0;
	if (body.model === "cp-fail-first" || (body.model === "cp-fail" && done)) return { status: 503, body: errorBody("service unavailable", "server_error", "unavailable") };
	if (body.model === "cp-search-collision") {
		const response = cpResponse(body.model, [{ id: "fc_search", type: "function_call", status: "completed", call_id: "call_search", name: "bifrost_tool_search", arguments: "{\"topic\":\"weather\"}" }]);
		return body.stream === true ? { status: 200, stream: cpResponseStream(response) } : { status: 200, body: JSON.stringify(response) };
	}
	const script = Object.hasOwn(cpResponses, body.model) ? cpResponses[body.model] : cpResponses["cp-fallback-target"];
	const response = cpResponse(body.model, script(done));
	return body.stream === true ? { status: 200, stream: cpResponseStream(response) } : { status: 200, body: JSON.stringify(response) };
};
// Text after a tool call on a provider that answers Responses through chat completions.
const cpCloudflareChat = (body) => ({ status: 200, stream: cpChunks(body.model, cpUsage,
	[cpDelta(0, { role: "assistant", content: "Before." })],
	[cpDelta(0, { tool_calls: [{ index: 0, ...cpChatToolCall }] })],
	[cpDelta(0, { content: "\n" })],
	[cpDelta(0, { content: "After." })],
	[cpDelta(0, {}, "tool_calls")]) });

// Vercel AI Gateway image channel, TXE-6438. pp_vercel is an openai-based custom provider configured as Tokun's
// reconciler writes it (contract C12). The answer is the OpenAI images shape with no usage block, one image per
// requested n: no Vercel image response has been recorded yet and its usage fields were not found, so this stands in
// for the shape that leaves the gateway nothing to read but data[].
const vercelImageAnswer = (body) => {
	const n = Number.isInteger(body.n) && body.n > 0 ? body.n : 1;
	return { created: 1, data: Array.from({ length: n }, () => ({ b64_json: "iVBORw0KGgo=" })) };
};

// Imagen answers three predictions with the middle one blocked, TXE-6536 (contract C6).
const imagenPartiallyBlocked = {
	predictions: [
		{ bytesBase64Encoded: "aW1hZ2Ux", mimeType: "image/png" },
		{ raiFilteredReason: "Unable to show generated images. Support codes: 58061214" },
		{ bytesBase64Encoded: "aW1hZ2Uy", mimeType: "image/png" },
	],
};
// Gemini API audio, TXE-6539 (contract C4, C5, C6, C10). The model in the path selects the answer, in the
// generateContent shape a Gemini TTS or transcription model gives: pp-tts-ok is six bytes of PCM that end on
// finishReason STOP with every usage detail the vendor reports (thought tokens, the candidates' modality
// breakdown); pp-tts-blocked is a prompt refused before generation (promptFeedback.blockReason, no candidates);
// pp-tts-truncated streams one audio chunk and ends with no finishReason; pp-stt-ok is a transcript. No Gemini
// TTS response has been recorded for these rows: the shapes are the ones the fork's Gemini types decode.
const geminiPCMPart = { inlineData: { mimeType: "audio/L16;codec=pcm;rate=24000", data: "AAECAwQF" } };
const geminiSpeechUsage = {
	promptTokenCount: 7, candidatesTokenCount: 50, totalTokenCount: 69, thoughtsTokenCount: 12,
	promptTokensDetails: [{ modality: "TEXT", tokenCount: 7 }],
	candidatesTokensDetails: [{ modality: "AUDIO", tokenCount: 50 }],
};
const geminiSpeechAudioChunk = { candidates: [{ index: 0, content: { role: "model", parts: [geminiPCMPart] } }] };
const geminiSpeechFinal = { candidates: [{ index: 0, finishReason: "STOP", content: { role: "model", parts: [geminiPCMPart] } }], usageMetadata: geminiSpeechUsage };
const geminiPromptBlocked = {
	promptFeedback: { blockReason: "SAFETY", blockReasonMessage: "The prompt is blocked due to safety" },
	usageMetadata: { promptTokenCount: 7, totalTokenCount: 7 },
};
const geminiTranscript = {
	candidates: [{ index: 0, finishReason: "STOP", content: { role: "model", parts: [{ text: "hello from the fixture" }] } }],
	usageMetadata: { promptTokenCount: 40, candidatesTokenCount: 5, totalTokenCount: 45, promptTokensDetails: [{ modality: "AUDIO", tokenCount: 32 }, { modality: "TEXT", tokenCount: 8 }] },
};
const geminiAudioFrames = {
	"pp-tts-ok": [geminiSpeechAudioChunk, geminiSpeechFinal],
	"pp-tts-blocked": [geminiPromptBlocked],
	"pp-tts-truncated": [geminiSpeechAudioChunk],
	"pp-stt-ok": [geminiTranscript],
};
// geminiAudioAnswer answers /pp/gemini/models/<model>:generateContent with the model's last frame and
// :streamGenerateContent with all of them as SSE; undefined for any other path or model.
const geminiAudioAnswer = (path) => {
	const m = /^\/pp\/gemini\/models\/([^:/]+):(generateContent|streamGenerateContent)$/.exec(path);
	const frames = m && geminiAudioFrames[m[1]];
	if (!frames) return undefined;
	if (m[2] === "streamGenerateContent") return { status: 200, stream: frames.map((f) => `data: ${JSON.stringify(f)}\n\n`).join("") };
	return { status: 200, body: JSON.stringify(frames[frames.length - 1]) };
};

const miniMaxBaseRespRoutes = new Set([
	"/pp/minimax/v1/chat/completions",
	"/pp/minimax/v1/responses",
	"/pp/minimax/v1/t2a_v2",
	"/pp/minimax/v1/image_generation",
	"/pp/minimax/v2/video_generation",
]);
const miniMaxBaseRespAnswer = (selector) => {
	const m = /^base-resp-(\d+)$/.exec(selector || "");
	if (!m) return undefined;
	return { status: 200, body: JSON.stringify({ id: "", choices: null, base_resp: { status_code: Number(m[1]), status_msg: `fixture base_resp ${m[1]}` } }) };
};

// Google Cloud Text-to-Speech, TXE-6541. text:synthesize answers by the voice name in the request: a WaveNet voice
// gets Google's documented sample response (audioContent is base64, shortened as in Google's quickstart), a voice name
// ending in -ZZ gets Google's INVALID_ARGUMENT error envelope with status 400, one ending in -Quota a 429
// RESOURCE_EXHAUSTED, and one ending in -Silent a 200 with no audioContent.
const cloudTTSAudioContent = "//NExAASCCIIAAhEAGAAEMW4kAYPnwwIKw/BBTpwTvB+IAxIfghU";
const cloudTTSAnswer = (body) => {
	const voice = body.voice && typeof body.voice.name === "string" ? body.voice.name : "";
	if (voice.endsWith("-ZZ")) return { status: 400, body: JSON.stringify({ error: { code: 400, message: `Voice '${voice}' does not exist. Is it misspelled?`, status: "INVALID_ARGUMENT" } }) };
	if (voice.endsWith("-Quota")) return { status: 429, body: JSON.stringify({ error: { code: 429, message: "Quota exceeded for quota metric 'All requests' and limit 'All requests per minute'.", status: "RESOURCE_EXHAUSTED" } }) };
	if (voice.endsWith("-Silent")) return { status: 200, body: "{}" };
	return { status: 200, body: JSON.stringify({ audioContent: cloudTTSAudioContent }) };
};

// OpenAI's own images API, TXE-6528 (contract C4, C5, C6): the openai provider's base URL is this fixture's root, so
// an image request to it arrives on /v1/images/generations or /v1/images/edits. gpt-image-blocked answers with
// OpenAI's documented moderation block (HTTP 400, code moderation_blocked); every other model answers in the
// gpt-image shape, n images with the token usage block OpenAI reports and no image count. A streamed generation
// or edit is answered as server-sent events. An edit arrives as a
// multipart form, or as OpenAI's JSON edit body when its input images are links, so the model and n are read from
// whichever body came.
const openAIImageModerationBlocked = JSON.stringify({ error: { message: "Your request was rejected by the safety system. If you believe this is an error, contact us at help.openai.com and include the request ID req_fixture.", type: "image_generation_user_error", param: null, code: "moderation_blocked" } });
const formField = (bytes, name) => {
	const match = new RegExp(`name="${name}"\\r\\n\\r\\n([^\\r]*)\\r\\n`).exec(bytes);
	return match ? match[1] : undefined;
};
const openAIImageAnswer = (body, bytes, isEdit) => {
	const model = typeof body.model === "string" ? body.model : formField(bytes, "model");
	if (model === "gpt-image-blocked") return { status: 400, body: openAIImageModerationBlocked };
	// gpt-image-drop is read in full and then loses its connection without a response byte: an upstream that
	// has the request and never answers, which a write-once request must not be sent to twice (folder 429, TXE-7044).
	if (model === "gpt-image-drop") return { drop: true };
	const requested = Number.isInteger(body.n) ? body.n : Number(formField(bytes, "n"));
	const n = Number.isInteger(requested) && requested > 0 ? requested : 1;
	// A streamed request (stream: true in either body) is answered as OpenAI streams it: partial_images previews of
	// the one image (default 1), numbered by partial_image_index, then the completed event carrying the image and
	// the token usage, and no image count (TXE-6984).
	if (body.stream === true || formField(bytes, "stream") === "true") {
		const kind = isEdit ? "image_edit" : "image_generation";
		const asked = Number.isInteger(body.partial_images) ? body.partial_images : Number(formField(bytes, "partial_images"));
		const previews = Number.isInteger(asked) && asked >= 0 ? asked : 1;
		const frame = { created_at: 1, size: "1024x1024", quality: "low", background: "opaque", output_format: "png" };
		const events = [
			...Array.from({ length: previews }, (_, i) => ({ type: `${kind}.partial_image`, b64_json: "iVBORw0KGgo=", partial_image_index: i, sequence_number: i + 1, ...frame })),
			{ type: `${kind}.completed`, b64_json: "iVBORw0KGgo=", sequence_number: previews + 1, ...frame, usage: { input_tokens: 12, input_tokens_details: { image_tokens: 0, text_tokens: 12 }, output_tokens: 272, output_tokens_details: { image_tokens: 272, text_tokens: 0 }, total_tokens: 284 } },
		];
		return { status: 200, stream: events.map((e) => `event: ${e.type}\ndata: ${JSON.stringify(e)}\n\n`).join("") };
	}
	return {
		status: 200,
		body: JSON.stringify({
			created: 1, background: "opaque", output_format: "png", quality: "low", size: "1024x1024",
			data: Array.from({ length: n }, () => ({ b64_json: "iVBORw0KGgo=" })),
			usage: { input_tokens: 12, input_tokens_details: { image_tokens: 0, text_tokens: 12 }, output_tokens: 272 * n, output_tokens_details: { image_tokens: 272 * n, text_tokens: 0 }, total_tokens: 12 + 272 * n },
		}),
	};
};
// Azure runs the same images API on its endpoint under /openai/v1 (the azure key's endpoint is this fixture).
const isOpenAIImagePath = (path) => path === "/v1/images/generations" || path === "/v1/images/edits" || path === "/openai/v1/images/generations" || path === "/openai/v1/images/edits";

// Gemini API image generation and edit (Nano Banana), TXE-6526, contract C4, C5, C6. The gemini provider's
// generateContent path names the model, and the model picks the answer: one delivered image with thinking tokens,
// the three ways the vendor answers 200 with no image and a block marker (a safety finishReason, promptFeedback
// with no candidates, text only), and a 200 with no image and no marker. The bodies are in the shape the Gemini
// API documents for generateContent on an image model.
const geminiImageAnswers = {
	"fixture-image-delivered": { candidates: [{ content: { role: "model", parts: [{ text: "Here is your image." }, { inlineData: { mimeType: "image/png", data: "iVBORw0KGgo=" } }] }, finishReason: "STOP", index: 0 }], usageMetadata: { promptTokenCount: 12, candidatesTokenCount: 1300, totalTokenCount: 1512, thoughtsTokenCount: 200, promptTokensDetails: [{ modality: "TEXT", tokenCount: 12 }], candidatesTokensDetails: [{ modality: "IMAGE", tokenCount: 1290 }, { modality: "TEXT", tokenCount: 10 }] }, modelVersion: "fixture-image-delivered", responseId: "fixture-image-delivered" },
	"fixture-image-safety": { candidates: [{ finishReason: "IMAGE_SAFETY", finishMessage: "Unable to show the generated image because it may violate our policies.", index: 0 }], usageMetadata: { promptTokenCount: 9, totalTokenCount: 9, promptTokensDetails: [{ modality: "TEXT", tokenCount: 9 }] }, modelVersion: "fixture-image-safety", responseId: "fixture-image-safety" },
	"fixture-image-prompt-blocked": { promptFeedback: { blockReason: "PROHIBITED_CONTENT" }, usageMetadata: { promptTokenCount: 7, totalTokenCount: 7 }, modelVersion: "fixture-image-prompt-blocked", responseId: "fixture-image-prompt-blocked" },
	"fixture-image-text-only": { candidates: [{ content: { role: "model", parts: [{ text: "I can't create that image." }] }, finishReason: "STOP", index: 0 }], usageMetadata: { promptTokenCount: 8, candidatesTokenCount: 7, totalTokenCount: 15, candidatesTokensDetails: [{ modality: "TEXT", tokenCount: 7 }] }, modelVersion: "fixture-image-text-only", responseId: "fixture-image-text-only" },
	// Thinking reported with no modality breakdown: candidatesTokenCount is the only output count. Priced in
	// fixturePricing, so the row can read the cost the gateway computed from the aggregate.
	"fixture-image-thoughts-only": { candidates: [{ content: { role: "model", parts: [{ inlineData: { mimeType: "image/png", data: "iVBORw0KGgo=" } }] }, finishReason: "STOP", index: 0 }], usageMetadata: { promptTokenCount: 10, candidatesTokenCount: 1300, totalTokenCount: 1510, thoughtsTokenCount: 200 }, modelVersion: "fixture-image-thoughts-only", responseId: "fixture-image-thoughts-only" },
	"fixture-image-empty": { usageMetadata: { promptTokenCount: 7, totalTokenCount: 7 }, modelVersion: "fixture-image-empty", responseId: "fixture-image-empty" },
};
// geminiImageAnswerFor returns the answer for a generateContent path whose model is one of the above, else undefined.
const geminiImageAnswerFor = (path) => {
	const match = /\/models\/([^/:]+):generateContent$/.exec(path);
	if (!match || !Object.hasOwn(geminiImageAnswers, match[1])) return undefined;
	return { status: 200, body: JSON.stringify(geminiImageAnswers[match[1]]) };
};

// A Gemini file upload, TXE-6526: each upload gets its own file name, so the gateway records one owner per file and
// a row can name another key's file. The gemini provider's base URL here has no /v1beta, so the upload arrives at
// /pp/gemini/files.
let geminiUploads = 0;
const geminiFileUpload = () => {
	const name = `files/fixture-upload-${Date.now()}-${++geminiUploads}`;
	return { status: 200, body: JSON.stringify({ file: { name, displayName: "sample.png", mimeType: "image/png", sizeBytes: "70", createTime: "2026-11-10T00:00:00Z", state: "ACTIVE", uri: `https://generativelanguage.googleapis.com/v1beta/${name}` } }) };
};

// MiniMax speech usage, TXE-6629. A recorded /v1/t2a_v2 extra_info: usage_characters is what MiniMax bills and
// audio_length is milliseconds. A non-streamed answer carries hex audio; a streamed one sends status-1 audio chunks and
// a status-2 chunk with extra_info and, under exclude_aggregated_audio, no audio.
const miniMaxSpeechExtraInfo = { audio_length: 2500, audio_sample_rate: 32000, audio_size: 40960, bitrate: 128000, word_count: 30, invisible_character_ratio: 0, usage_characters: 37, audio_format: "mp3", audio_channel: 1, usage_voice_count: 1 };
const miniMaxOk = { status_code: 0, status_msg: "success" };
const miniMaxSpeechAnswer = (body) => {
	if (body.stream === true) {
		const chunks = [
			{ data: { audio: "aabb", status: 1 }, trace_id: "t_pp", base_resp: miniMaxOk },
			{ data: { audio: "", status: 2 }, extra_info: miniMaxSpeechExtraInfo, trace_id: "t_pp", base_resp: miniMaxOk },
		];
		return { status: 200, stream: chunks.map((c) => `data: ${JSON.stringify(c)}\n\n`).join("") };
	}
	return { status: 200, body: JSON.stringify({ data: { audio: "4944330000", status: 2 }, extra_info: miniMaxSpeechExtraInfo, trace_id: "t_pp", base_resp: miniMaxOk }) };
};

// MiniMax content-safety flags, native Responses and Messages, TXE-6595 (vendor sheet B1, B3, B4; contract C6).
// Chat for the model "sensitive-flags" answers with MiniMax's four top-level flags; input_sensitive_type 5 is
// "abusive" (platform.minimax.io/docs/api-reference/text-chat-openai). Streamed, the flags ride the content chunk and
// the finish chunk. /v1/responses answers MiniMax's native Responses object, whose reasoning item has an empty summary
// and carries the thinking as reasoning_text (api-reference/responses-create). /anthropic/v1/messages is MiniMax's
// Anthropic-compatible endpoint at the API origin (api-reference/text-anthropic-api), outside the /pp/minimax base.
const miniMaxSensitivity = { input_sensitive: true, input_sensitive_type: 5, output_sensitive: false, output_sensitive_type: 0 };
const miniMaxSensitiveChatAnswer = (body) => {
	const usage = { prompt_tokens: 3, completion_tokens: 1, total_tokens: 4 };
	if (body.stream === true) {
		const base = { id: "chatcmpl_mm_flags", object: "chat.completion.chunk", created: 1, model: body.model };
		const frames = [
			{ ...base, choices: [{ index: 0, delta: { role: "assistant", content: "hello" } }], ...miniMaxSensitivity },
			{ ...base, choices: [{ index: 0, delta: {}, finish_reason: "stop" }], usage, ...miniMaxSensitivity, base_resp: miniMaxOk },
		];
		return { status: 200, stream: frames.map((f) => `data: ${JSON.stringify(f)}\n\n`).join("") + "data: [DONE]\n\n" };
	}
	return { status: 200, body: JSON.stringify({ id: "chatcmpl_mm_flags", object: "chat.completion", created: 1, model: body.model, choices: [{ index: 0, finish_reason: "stop", message: { role: "assistant", content: "hello" } }], usage, ...miniMaxSensitivity, base_resp: miniMaxOk }) };
};
const miniMaxResponsesAnswer = (body) => ({
	status: 200,
	body: JSON.stringify({
		id: "resp_mm_1",
		object: "response",
		created_at: 1,
		status: "completed",
		model: body.model,
		output: [
			{ id: "rs_mm_1", type: "reasoning", summary: [], content: [{ type: "reasoning_text", text: "Compare the tenths: 9 is more than 1." }] },
			{ id: "msg_mm_1", type: "message", role: "assistant", status: "completed", content: [{ type: "output_text", text: "9.9", annotations: [] }] },
		],
		usage: { input_tokens: 12, output_tokens: 4, total_tokens: 16 },
	}),
});

// BFL image tasks, TXE-6431 (folder 421). The config points bfl at /pp/bfl, so a submit (POST /pp/bfl/v1/{model})
// arrives through the wire capture and answers BFL's AsyncResponse, and GET /pp/bfl/v1/get_result answers BFL's
// ResultResponse for the task id it names: bfl-ready is Ready with a sample link and its settled cost, bfl-moderated
// is Content Moderated with the vendor's reasons, bfl-pending is Pending, anything else is "Task not found". The
// polling_url is plain http, so the gateway polls its configured base, which is this fixture.
const BFL_FIXTURE_BASE = "http://127.0.0.1:8791/pp/bfl";
const bflSubmitAnswer = () => ({ id: "bfl-ready", polling_url: `${BFL_FIXTURE_BASE}/v1/get_result?id=bfl-ready`, cost: 4, input_mp: 0, output_mp: 1.05 });
const bflResult = (id) => {
	switch (id) {
	case "bfl-ready":
		return { id, status: "Ready", result: { sample: "https://delivery.example.test/bfl/sample.jpeg", end_time: 1762700000, seed: 7 }, progress: null, details: null, cost: 4 };
	case "bfl-moderated":
		return { id, status: "Content Moderated", result: null, progress: null, details: { "Moderation Reasons": ["Derivative Works Filter"] }, cost: 0 };
	case "bfl-pending":
		return { id, status: "Pending", result: null, progress: 0.2, details: null };
	default:
		return { id, status: "Task not found", result: null, progress: null, details: null };
	}
};

// Tencent, StepFun and Xiaomi chat on openai-based custom providers, TXE-6439 (contract C5, C12). The fixture gateway
// config points one custom provider per vendor site at /pp/<site>, written as Tokun's reconciler writes them, and each
// site answers with the usage shape its vendor documents:
//   stepfun     international site (api.stepfun.ai): the cache hits are a top-level usage.cached_tokens with no
//               prompt_tokens_details, and a stream carries usage on every chunk. The numbers are the ones in
//               StepFun's prompt-cache guide (512 of 591 prompt tokens from the cache).
//   stepfun-cn  domestic site (api.stepfun.com): the same count nested as prompt_tokens_details.cached_tokens.
//   tencent     TokenHub: the usage block of TokenHub's response reference.
//   xiaomi      the three token counts only; Xiaomi's full usage page was not found.
// No response recorded from a real request exists for any of them yet, so these are the documented shapes.
const vendorChatUsage = {
	stepfun: { cached_tokens: 512, prompt_tokens: 591, completion_tokens: 120, total_tokens: 711 },
	"stepfun-cn": { prompt_tokens: 591, completion_tokens: 120, total_tokens: 711, prompt_tokens_details: { cached_tokens: 512 } },
	tencent: { prompt_tokens: 25, completion_tokens: 120, total_tokens: 145, completion_tokens_details: { reasoning_tokens: 0 }, prompt_tokens_details: { cached_tokens: 0 } },
	xiaomi: { prompt_tokens: 25, completion_tokens: 120, total_tokens: 145 },
};
const vendorChatAnswer = (site, body) => {
	const usage = vendorChatUsage[site];
	const base = { id: `chatcmpl_${site}`, created: 1, model: body.model };
	if (body.stream !== true) {
		return { status: 200, body: JSON.stringify({ ...base, object: "chat.completion", choices: [{ index: 0, finish_reason: "stop", message: { role: "assistant", content: "hello" } }], usage }) };
	}
	// StepFun sends usage on every chunk; the others send it once, on a trailing frame with no choices, and only
	// when stream_options.include_usage asks for it.
	const everyChunk = site === "stepfun";
	const chunk = (choices, withUsage) => ({ ...base, object: "chat.completion.chunk", choices, ...(withUsage ? { usage } : {}) });
	const chunks = [
		chunk([{ index: 0, delta: { role: "assistant", content: "" }, finish_reason: null }], everyChunk),
		chunk([{ index: 0, delta: { content: "hello" }, finish_reason: null }], everyChunk),
		chunk([{ index: 0, delta: {}, finish_reason: "stop" }], everyChunk),
	];
	if (!everyChunk && body.stream_options && body.stream_options.include_usage) chunks.push(chunk([], true));
	return { status: 200, stream: chunks.map((c) => `data: ${JSON.stringify(c)}\n\n`).join("") + "data: [DONE]\n\n" };
};

// Bedrock Converse (TXE-7083), reached over the TLS listener through bedrock_key_config.endpoints.runtime.
const bedrockConverseAnswer = (path) => ({
	status: 200,
	body: JSON.stringify({
		output: { message: { role: "assistant", content: [{ text: `hello from the bedrock fixture (${decodeURIComponent(path.split("/")[2])})` }] } },
		stopReason: "end_turn",
		usage: { inputTokens: 3, outputTokens: 7, totalTokens: 10 },
	}),
});
const isBedrockConversePath = (path) => /^\/model\/[^/]+\/converse$/.test(path);

// BytePlus Seedance video tasks, TXE-6426 (contract C7), served on /pp/byteplus/api/v3/contents/generations/tasks.
// A create is answered by the route above wireAnswer already has (task id cgt-fixture-video-task) and is kept in /__wire. A query answers the task whose
// state its id names (cgt-fixture-queued, -running, -succeeded, -failed, -cancelled, -expired), in the shape
// core/providers/byteplus/videos_test.go pins, and DELETE answers 200 with no body, as Ark does. Every query and
// DELETE is counted as "seedance.retrieve <id>" or "seedance.delete <id>", so a cancel row can prove that a
// queued task was queried once and deleted once, and that a finished one was never deleted.
const SEEDANCE_TASKS = "/pp/byteplus/api/v3/contents/generations/tasks";
const seedanceTask = (id) => {
	const status = id.replace(/^cgt-fixture-/, "");
	const task = { id, model: "dreamina-seedance-2-0-fast-260128", status, created_at: 1762700000, updated_at: 1762700090 };
	if (status === "succeeded") {
		Object.assign(task, { content: { video_url: "https://fixture.invalid/seedance/video.mp4" }, usage: { completion_tokens: 108900, total_tokens: 108900 }, resolution: "720p", ratio: "16:9", duration: 5 });
	}
	if (status === "failed") task.error = { code: "OutputVideoSensitiveContentDetected", message: "The request failed because the output video may contain sensitive information." };
	return task;
};

// A provider that records an upload under a storage URI (folder 483, TXE-7032). Vertex's file upload returns
// "gs://<bucket>/<object>" as the file id and Bedrock's "s3://<bucket>/<key>"; neither host can be pointed at this
// fixture, so an OpenAI-shaped provider under /pp/storage stands in for the id form: its upload answers with a gs://
// id, unique per upload so a row kept from an earlier run never names this run's file, and its chat completes. The
// gateway's ownership check is the same code whatever provider recorded the id.
//
// Its batches stand in for a provider's tasks (folder 494, TXE-7119): a create answers with an id, a retrieve
// answers for a batch created here, and the list holds every batch created here and one this fixture names that no
// gateway request ever created (STORAGE_BATCH_MADE_ELSEWHERE), as a batch made outside the gateway would appear.
let storageUploads = 0;
let storageBatches = 0;
const storageBatchIds = new Set();
const STORAGE_BATCH_MADE_ELSEWHERE = "batch_storage_made_elsewhere";
const storageBatchObject = (id) => ({
	id, object: "batch", endpoint: "/v1/chat/completions", input_file_id: "gs://bifrost-fixture-bucket/uploads/input.jsonl", completion_window: "24h",
	status: "validating", created_at: 1, request_counts: { total: 1, completed: 0, failed: 0 },
});
const storageBatchList = () => {
	const data = [...storageBatchIds, STORAGE_BATCH_MADE_ELSEWHERE].map(storageBatchObject);
	return { status: 200, body: JSON.stringify({ object: "list", data, first_id: data[0].id, last_id: data[data.length - 1].id, has_more: false }) };
};
const storageBatchRetrieve = (id) => {
	if (!storageBatchIds.has(id)) return { status: 404, body: JSON.stringify({ error: { message: `no storage fixture batch ${id}` } }) };
	return { status: 200, body: JSON.stringify(storageBatchObject(id)) };
};
const storageAnswer = (path, body) => {
	if (path === "/pp/storage/v1/batches") {
		storageBatches += 1;
		const id = `batch_storage_${Date.now()}_${storageBatches}`;
		storageBatchIds.add(id);
		return { status: 200, body: JSON.stringify(storageBatchObject(id)) };
	}
	if (path === "/pp/storage/v1/files") {
		storageUploads += 1;
		const id = `gs://bifrost-fixture-bucket/uploads/${Date.now()}-${storageUploads}.jsonl`;
		return { status: 200, body: JSON.stringify({ id, object: "file", bytes: 64, created_at: 1, filename: "sample.jsonl", purpose: "user_data", status: "processed" }) };
	}
	if (path === "/pp/storage/v1/chat/completions") {
		return { status: 200, body: JSON.stringify({ id: "chatcmpl_storage", object: "chat.completion", created: 1, model: body.model, choices: [{ index: 0, finish_reason: "stop", message: { role: "assistant", content: "ok" } }], usage: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 } }) };
	}
	return { status: 404, body: JSON.stringify({ error: { message: `no storage fixture route for ${path}` } }) };
};

const wireAnswer = (path, body, bytes) => {
	if (isBedrockConversePath(path)) return bedrockConverseAnswer(path);
	if (path.startsWith("/pp/storage/")) return storageAnswer(path, body);
	if (path.startsWith("/pp/bfl/v1/")) return { status: 200, body: JSON.stringify(bflSubmitAnswer()) };
	if (isOpenAIImagePath(path)) return openAIImageAnswer(body, bytes, path.endsWith("/images/edits"));
	const geminiImage = geminiImageAnswerFor(path);
	if (geminiImage) return geminiImage;
	if (path === "/pp/gemini/files") return geminiFileUpload();
	const vendorChat = /^\/pp\/(tencent|stepfun|stepfun-cn|xiaomi)\/v1\/chat\/completions$/.exec(path);
	if (vendorChat) return vendorChatAnswer(vendorChat[1], body);
	if (path === "/pp/bounds/v1/chat/completions") {
		return { status: 200, body: JSON.stringify({ id: "chatcmpl_bounds", object: "chat.completion", created: 1, model: body.model, choices: [{ index: 0, finish_reason: "stop", message: { role: "assistant", content: "hello" } }], usage: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 } }) };
	}
	// OpenAI-compatible chat as the vendor sees it, TXE-6527 (contract C4): moonshot posts to /v1/chat/completions and
	// deepseek to /chat/completions under the same base. The recorded body is the assertion; the answer is a plain
	// completion that echoes the tier it was asked for.
	if (path === "/pp/oc/v1/chat/completions" || path === "/pp/oc/chat/completions") {
		return { status: 200, body: JSON.stringify({ id: "chatcmpl_oc", object: "chat.completion", created: 1, model: body.model, service_tier: body.service_tier, choices: [{ index: 0, finish_reason: "stop", message: { role: "assistant", content: "hello" } }], usage: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 } }) };
	}
	if (path === "/pp/cp/v1/chat/completions") return cpChatAnswerFor(body);
	// A custom provider is served with its own keys (folder 487, TXE-7097): one custom provider on each of the deepseek,
	// glm, moonshot and qwen bases points at /pp/ownkey, where each base posts chat to its own path. The call is kept in
	// /__wire with the bearer that arrived, which is what the row reads.
	if (path.startsWith("/pp/ownkey/") && path.endsWith("/chat/completions")) {
		return { status: 200, body: JSON.stringify({ id: "chatcmpl_ownkey", object: "chat.completion", created: 1, model: body.model, choices: [{ index: 0, finish_reason: "stop", message: { role: "assistant", content: "hello" } }], usage: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 } }) };
	}
	// MiniMax answers its failures inside a 200 as base_resp, with no result (TXE-6588, contract C6). The model
	// "base-resp-<code>" selects the code, so a case asks for exactly the MiniMax error it pins. Chat, Responses
	// (MiniMax's native /v1/responses, TXE-6595), speech, image generation and video create each read base_resp from
	// their own route.
	if (miniMaxBaseRespRoutes.has(path)) {
		const answer = miniMaxBaseRespAnswer(body.model);
		if (answer) return answer;
	}
	if (path === "/pp/minimax/v1/t2a_v2") return miniMaxSpeechAnswer(body);
	// Video create, accepted (folder 434, TXE-7065): each vendor's own task answer, so a row can read what the
	// converter sent from /__wire without creating a billed task. MiniMax's base-resp models are answered above.
	if (path === "/pp/minimax/v2/video_generation") return { status: 200, body: JSON.stringify({ task_id: "fixture-video-task", base_resp: { status_code: 0, status_msg: "success" } }) };
	if (path === "/pp/byteplus/api/v3/contents/generations/tasks") return { status: 200, body: JSON.stringify({ id: "cgt-fixture-video-task" }) };
	if (path.startsWith("/pp/gemini/models/") && path.endsWith(":predictLongRunning")) return { status: 200, body: JSON.stringify({ name: `${path.slice("/pp/gemini/".length, -":predictLongRunning".length)}/operations/fixture-video-op` }) };
	if (path === "/pp/minimax/v1/chat/completions" && body.model === "sensitive-flags") return miniMaxSensitiveChatAnswer(body);
	if (path === "/pp/minimax/v1/responses") return miniMaxResponsesAnswer(body);
	if (path === "/anthropic/v1/messages") return { status: 200, body: JSON.stringify(anthropicMessage(body.model)) };
	if (path === "/pp/vercel/v1/images/generations") return { status: 200, body: JSON.stringify(vercelImageAnswer(body)) };
	if (path === "/pp/google-cloud-tts/v1/text:synthesize") return cloudTTSAnswer(body);
	if (path === "/pp/cp/v1/responses") return cpResponsesAnswerFor(body);
	if (path === "/pp/cloudflare/accounts/acct-1/ai/v1/chat/completions") return cpCloudflareChat(body);
	// TXE-6967: xAI reports its charge as cost_in_usd_ticks; upstream-usage-extended answers with it on chat and
	// Responses, streamed and not, for the rows that read it back as vendor_cost.
	if (path === "/pp/xai/v1/chat/completions" && body.model === "upstream-usage-extended") {
		const completion = JSON.parse(behaviours["upstream-usage-extended"]().body);
		return body.stream === true ? { status: 200, stream: streamChunks(completion, true) } : { status: 200, body: JSON.stringify(completion) };
	}
	if (path === "/pp/xai/v1/responses" && body.model === "upstream-usage-extended") {
		const response = {
			id: "resp_upstream_usage_extended", object: "response", created_at: 1, status: "completed", model: body.model,
			output: [{ id: "msg_upstream_usage_extended", type: "message", role: "assistant", status: "completed", content: [{ type: "output_text", text: "hello", annotations: [] }] }],
			usage: { input_tokens: 120, output_tokens: 48, total_tokens: 168, output_tokens_details: { reasoning_tokens: 12 }, cost_in_usd_ticks: extendedUsage.cost_in_usd_ticks },
		};
		return body.stream === true ? { status: 200, stream: sse([{ type: "response.completed", sequence_number: 0, response }]) } : { status: 200, body: JSON.stringify(response) };
	}
	// DeepSeek's Anthropic-compatible endpoint is {base}/anthropic/v1/messages (TXE-6548), under the same /pp/oc base its chat uses.
	if (path === "/pp/xai/v1/messages" || path === "/pp/anthropic/v1/messages" || path === "/pp/oc/anthropic/v1/messages") {
		if (body.model === "pp-fail") return { status: 400, body: JSON.stringify({ type: "error", error: { type: "invalid_request_error", message: "fixture refused" } }) };
		if (body.stream === true) return { status: 200, stream: anthropicMessageStream(body.model) };
		return { status: 200, body: JSON.stringify(anthropicMessage(body.model)) };
	}
	// Folder 490 (TXE-7090): predictions whose addresses are the vendor's to name. The model's last path
	// segment is "<what>-<where>". poll-origin and poll-loopback name urls.get on the provider's own origin
	// and on another loopback origin (the same fixture by the name localhost). stream-origin and
	// stream-loopback do the same with urls.stream. picture-public and picture-loopback are polled on the
	// provider's origin and answer with an output image at a public-looking address (reached through the
	// media tunnel) and at the fixture's loopback address. A gateway follows the origin and public ones and
	// must refuse the loopback ones without asking for them.
	const replicateNamed = /^\/pp\/replicate\/.*\/(poll|stream|picture)-(origin|loopback|public)\/predictions$/.exec(path);
	if (replicateNamed) {
		const [, what, where] = replicateNamed;
		const id = `pred_${what}_${where}`;
		const named = `http://${where === "loopback" ? "localhost" : "127.0.0.1"}:${port}/pp/replicate/v1`;
		const own = `http://127.0.0.1:${port}/pp/replicate/v1`;
		const urls = { get: `${what === "poll" ? named : own}/predictions/${id}` };
		if (what === "stream") urls.stream = `${named}/streams/${id}`;
		return { status: 201, body: JSON.stringify({ id, status: "starting", urls }) };
	}
	// Replicate accepts a prediction (201) but the answer names no urls.get to poll it at (folder 456, TXE-7056).
	if (path.startsWith("/pp/replicate/") && path.endsWith("/predictions")) return { status: 201, body: JSON.stringify({ id: "pred_fixture_no_urls", status: "starting" }) };
	if (path.endsWith(":predict")) return { status: 200, body: JSON.stringify(imagenPartiallyBlocked) };
	const geminiAudio = geminiAudioAnswer(path);
	if (geminiAudio) return geminiAudio;
	if (path.endsWith(":generateContent")) return { status: 200, body: JSON.stringify(geminiCodeExecution) };
	if (path.endsWith(":streamGenerateContent")) return { status: 200, stream: `data: ${JSON.stringify(geminiCodeExecution)}\n\n` };
	if (path === "/pp/cloudflare/accounts/acct-1/ai/run/@cf/cloudflare/clef-flash") return { status: 200, body: JSON.stringify(clefAnswer) };
	if (path === "/v1/embeddings" || path === "/pp/volcengine/embeddings") return { status: 200, body: JSON.stringify(embeddingAnswer) };
	if (path === "/pp/volcengine/embeddings/multimodal") return { status: 200, body: JSON.stringify({ id: "emb_pp", object: "list", model: body.model, data: { embedding: [0.1, 0.2] }, usage: { prompt_tokens: 1, total_tokens: 1 } }) };
	return { status: 500, body: errorBody("the fixture has no answer here", "server_error", "internal_error") };
};

// Streamed frames with gaps, TXE-6522 (contract C10 BF22). upstream-gapped streams GAPPED_FRAMES marker frames
// GAP_MS apart, on chat and on speech with stream_format "sse"; the fixture keeps the moment it wrote each marker.
// POST /__probe/stream-gaps is the client: it sends the request it is given to the gateway, whose upstream is this
// fixture, and reports when each SSE event arrived, on the same clock as those writes. A gateway that forwards each
// frame on arrival delivers marker i before the fixture writes marker i+1; one that buffers does not.
const GAP_MS = 600;
const GAPPED_FRAMES = 3;
const gapMarker = (i) => `gap-${i}`;
// The writes of the latest upstream-gapped stream per route, as performance.now() values.
const gappedSends = { chat: [], speech: [] };
const writeGapped = (res, route, frames, tail) => {
	res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
	gappedSends[route] = [];
	let i = 0;
	const next = () => {
		if (res.destroyed) return;
		if (i === frames.length) return res.end(tail);
		gappedSends[route].push(performance.now());
		res.write(frames[i++]);
		setTimeout(next, GAP_MS);
	};
	next();
};
const gappedChat = (res, includeUsage) => {
	const base = { id: "chatcmpl-upstream-gapped", object: "chat.completion.chunk", created: 1, model: "upstream-gapped" };
	const frame = (choices, extra) => `data: ${JSON.stringify({ ...base, choices, ...extra })}\n\n`;
	const frames = [];
	for (let i = 0; i < GAPPED_FRAMES; i++) {
		frames.push(frame([{ index: 0, delta: i === 0 ? { role: "assistant", content: gapMarker(i) } : { content: gapMarker(i) }, finish_reason: null }]));
	}
	let tail = frame([{ index: 0, delta: {}, finish_reason: "stop" }]);
	if (includeUsage) tail += frame([], { usage: { prompt_tokens: 5, completion_tokens: GAPPED_FRAMES, total_tokens: 5 + GAPPED_FRAMES } });
	writeGapped(res, "chat", frames, tail + "data: [DONE]\n\n");
};
// Speech deltas carry base64 audio; each marker's base64 is what a client finds in the forwarded delta.
const gappedSpeech = (res) => {
	const frames = [];
	for (let i = 0; i < GAPPED_FRAMES; i++) {
		frames.push(`data: ${JSON.stringify({ type: "speech.audio.delta", audio: Buffer.from(gapMarker(i)).toString("base64") })}\n\n`);
	}
	const done = `data: ${JSON.stringify({ type: "speech.audio.done", usage: { input_tokens: 5, output_tokens: 30, total_tokens: 35 } })}\n\n`;
	writeGapped(res, "speech", frames, done);
};
// The probe: {base_url, path, body, headers?}. Answers {data: [{status, content_type, events, sends, gap_ms}]}, where
// events are the SSE events (or the whole non-SSE body) with the ms each arrived, and sends are the ms the fixture
// wrote each marker of the route's latest upstream-gapped stream, both measured from the moment the probe sent.
const probeStreamGaps = (spec, reply) => {
	const route = String(spec.path || "").includes("/audio/speech") ? "speech" : "chat";
	const payload = JSON.stringify(spec.body || {});
	const t0 = performance.now();
	const ms = (t) => Math.round((t - t0) * 10) / 10;
	const target = new URL(String(spec.path || ""), String(spec.base_url || ""));
	const upstream = http.request(target, {
		method: "POST",
		headers: { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(payload), ...(spec.headers || {}) },
	}, (gw) => {
		const events = [];
		let pending = "";
		gw.setEncoding("utf8");
		gw.on("data", (c) => {
			const at = ms(performance.now());
			pending += c;
			let cut;
			while ((cut = pending.indexOf("\n\n")) !== -1) {
				events.push({ at, text: pending.slice(0, cut) });
				pending = pending.slice(cut + 2);
			}
		});
		gw.on("end", () => {
			if (pending !== "") events.push({ at: ms(performance.now()), text: pending });
			reply(200, { data: [{ status: gw.statusCode, content_type: gw.headers["content-type"] || "", events, sends: gappedSends[route].map(ms), gap_ms: GAP_MS }] });
		});
	});
	upstream.on("error", (err) => reply(502, { error: { message: `probe could not reach the gateway: ${err.message}`, type: "probe_error" } }));
	upstream.end(payload);
};
// An OpenAI video task as GET /v1/videos/{id} returns it while the task waits in the queue.
const videoTask = (id) => ({ id, object: "video", model: "sora-2", status: "queued", progress: 0, created_at: 1762700000, seconds: "4", size: "720x1280" });

// Perplexity chat, TXE-6549 (contract C5, C10, C6). Perplexity serves chat on /chat/completions (no /v1) and
// reports its billing quantities in its own usage shape: the search context tier, top-level citation_tokens and
// num_search_queries, a flat cost, and a per-tool call report. Streamed, every frame carries a usage and the
// frame with the finish reason carries the full one. sonar-search-usage answers with that usage;
// sonar-invalid-model with Perplexity's 400, whose error.code is a number.
const perplexityUsage = {
	prompt_tokens: 12, completion_tokens: 40, total_tokens: 52,
	search_context_size: "high", citation_tokens: 300, num_search_queries: 3, reasoning_tokens: 8,
	cost: { input_tokens_cost: 0.000036, output_tokens_cost: 0.0006, reasoning_tokens_cost: 0.000024, citation_tokens_cost: 0.0006, search_queries_cost: 0.015, request_cost: 0.014, total_cost: 0.03026 },
	tool_calls_details: { web_search: { invocation: 3 }, fetch_url: { invocation: 1 } },
};
const perplexityChat = (model, stream) => {
	if (model === "sonar-invalid-model") {
		return { status: 400, body: JSON.stringify({ error: { message: "Invalid model 'sonar-invalid-model'. Permitted models can be found in the documentation at https://docs.perplexity.ai/guides/model-cards.", type: "invalid_model", code: 400 } }) };
	}
	if (model !== "sonar-search-usage") {
		return { status: 404, body: errorBody(`unknown fixture model ${model}`, "invalid_request_error", "model_not_found") };
	}
	const base = { id: "pplx-sonar-search-usage", created: 1, model, citations: ["https://example.com/a"] };
	if (!stream) {
		return { status: 200, body: JSON.stringify({ ...base, object: "chat.completion", choices: [{ index: 0, message: { role: "assistant", content: "hello" }, finish_reason: "stop" }], usage: perplexityUsage }) };
	}
	const frames = [
		{ ...base, object: "chat.completion.chunk", choices: [{ index: 0, delta: { role: "assistant", content: "hel" }, finish_reason: null }], usage: { prompt_tokens: 12, completion_tokens: 1, total_tokens: 13, search_context_size: "high" } },
		{ ...base, object: "chat.completion.chunk", choices: [{ index: 0, delta: { content: "lo" }, finish_reason: "stop" }], usage: perplexityUsage },
	];
	return { status: 200, stream: frames.map((c) => `data: ${JSON.stringify(c)}\n\n`).join("") + "data: [DONE]\n\n" };
};

// Azure AI Speech, TXE-6542: the two APIs the azurespeech provider calls, on the paths a resource endpoint (custom
// domain) serves them. Text to speech takes an SSML body and answers audio bytes; the voice name in the SSML selects
// the behaviour. Fast transcription takes a multipart audio + definition and answers JSON; definition.enhancedMode.model
// selects the behaviour. Every call is kept in /__wire with the headers a case asserts on, and the transcription call is
// kept as its definition and the size of its audio part, not as the multipart bytes. The resource key is "test-key" in
// Ocp-Apim-Subscription-Key; any other is refused with 401 and no body, as the text to speech API answers.
const AZURE_SPEECH_TTS_PATH = "/tts/cognitiveservices/v1";
const AZURE_SPEECH_STT_PATH = "/speechtotext/transcriptions:transcribe";
const azureSpeechAudio = Buffer.from("ID3-azure-speech-fixture-audio");
// The response of the fast transcription quickstart, cut to its first phrase.
const azureSpeechTranscription = {
	durationMilliseconds: 182439,
	combinedPhrases: [{ text: "Good afternoon. This is Sam." }],
	phrases: [{
		offsetMilliseconds: 960, durationMilliseconds: 640, text: "Good afternoon.",
		words: [{ text: "Good", offsetMilliseconds: 960, durationMilliseconds: 240 }, { text: "afternoon.", offsetMilliseconds: 1200, durationMilliseconds: 400 }],
		locale: "en-US", confidence: 0.93554276,
	}],
};
const azureSpeechAnswer = (req, res, path, raw) => {
	const headers = {
		content_type: req.headers["content-type"] || "",
		output_format: req.headers["x-microsoft-outputformat"] || "",
		user_agent: req.headers["user-agent"] || "",
		authorization: req.headers.authorization || "",
		subscription_key: req.headers["ocp-apim-subscription-key"] === "test-key" ? "test-key" : "other",
	};
	const query = new URL(req.url, "http://localhost").search;
	const unauthorized = headers.subscription_key !== "test-key";
	if (path === AZURE_SPEECH_TTS_PATH) {
		const ssml = raw.toString("utf8");
		wireCalls.push({ path, query, body: ssml, headers });
		if (unauthorized) { res.writeHead(401); return res.end(); }
		const voice = (ssml.match(/<voice name="([^"]*)"/) || [])[1] || "";
		if (voice === "azure-speech-429") { res.writeHead(429); return res.end(); }
		if (voice === "azure-speech-empty") { res.writeHead(200, { "Content-Type": "audio/mpeg" }); return res.end(); }
		if (voice === "azure-speech-gapped") {
			// Raw audio written GAP_MS apart: each marker is one chunk of the vendor's response body.
			res.writeHead(200, { "Content-Type": "audio/mpeg" });
			gappedSends.speech = [];
			let i = 0;
			const next = () => {
				if (res.destroyed) return undefined;
				if (i === GAPPED_FRAMES) return res.end();
				gappedSends.speech.push(performance.now());
				res.write(Buffer.from(gapMarker(i++)));
				return setTimeout(next, GAP_MS);
			};
			return next();
		}
		// Azure AI Speech labels the audio itself: a riff-* output format comes back as audio/x-wav.
		res.writeHead(200, { "Content-Type": headers.output_format.startsWith("riff-") ? "audio/x-wav" : "audio/mpeg" });
		return res.end(azureSpeechAudio);
	}
	const form = raw.toString("latin1");
	const definition = (form.match(/name="definition"\r\n\r\n([\s\S]*?)\r\n--/) || [])[1] || "";
	const audio = (form.match(/name="audio"[^\r\n]*\r\n(?:[^\r\n]+\r\n)*\r\n([\s\S]*?)\r\n--/) || [])[1];
	wireCalls.push({ path, query, body: definition, headers, audio_bytes: audio === undefined ? -1 : audio.length });
	if (unauthorized) { res.writeHead(401); return res.end(); }
	let model = "";
	try { model = ((JSON.parse(definition) || {}).enhancedMode || {}).model || ""; } catch {}
	if (model === "azure-speech-422") {
		res.writeHead(422, { "Content-Type": "application/json" });
		return res.end(JSON.stringify({ code: "InvalidRequest", message: "The request is invalid.", innerError: { code: "InvalidAudio", message: "The audio format is not supported." } }));
	}
	if (model !== "MAI-Transcribe-2") {
		res.writeHead(404, { "Content-Type": "application/json" });
		return res.end(JSON.stringify({ code: "NotFound", message: `unknown fixture model ${model}` }));
	}
	res.writeHead(200, { "Content-Type": "application/json" });
	return res.end(JSON.stringify(azureSpeechTranscription));
};

// Deepgram, TXE-6434: the pre-recorded transcription API the deepgram provider calls, POST /v1/listen. The options
// are query parameters and the audio is the body: the bytes of a file, or {"url": ...} as application/json. The model
// query parameter selects the behaviour: nova-3 answers the reference response (two channels when multichannel=true),
// deepgram-402 the out-of-credits error body, deepgram-no-results a 200 that carries a request id and no results. Every
// call is kept in /__wire with its query and the headers a case asserts on; an audio body is kept as its size, a JSON
// body as its text. The keys are "test-key-a" and "test-key-b" in the Token scheme; any other is refused with 401.
const DEEPGRAM_LISTEN_PATH = "/v1/listen";
const deepgramMetadata = (duration, channels) => ({
	transaction_key: "deprecated", request_id: "a847f427-4ad5-4d67-9b95-db801e58251c",
	sha256: "154e291ecfa8be6ab8343560bcc109008fa7853eb5372533e8efdefc9b504c33", created: "2024-05-12T18:57:13.426Z",
	duration, channels, models: ["30089e05-99d1-4376-b32e-c263170674af"],
	model_info: { "30089e05-99d1-4376-b32e-c263170674af": { name: "2-general-nova", version: "2024-01-09.29447", arch: "nova-3" } },
});
const deepgramChannel = (transcript) => ({
	alternatives: [{ transcript, confidence: 0.99902344, words: [{ word: "yeah", start: 0.08, end: 0.32, confidence: 0.9975586, punctuated_word: "Yeah." }] }],
});
const deepgramAnswer = (req, res, raw) => {
	const url = new URL(req.url, "http://localhost");
	const contentType = req.headers["content-type"] || "";
	const isJSON = contentType.startsWith("application/json");
	const authorization = req.headers.authorization || "";
	const key = { "Token test-key-a": "test-key-a", "Token test-key-b": "test-key-b" }[authorization] || "other";
	wireCalls.push({
		path: DEEPGRAM_LISTEN_PATH, query: url.search, body: isJSON ? raw.toString("utf8") : "",
		headers: { content_type: contentType, key }, audio_bytes: isJSON ? -1 : raw.length,
	});
	const answer = (status, body) => {
		res.writeHead(status, { "Content-Type": "application/json", "dg-request-id": "a847f427-4ad5-4d67-9b95-db801e58251c" });
		return res.end(JSON.stringify(body));
	};
	if (key === "other") return answer(401, { err_code: "INVALID_AUTH", err_msg: "Invalid credentials.", request_id: "a847f427-4ad5-4d67-9b95-db801e58251c" });
	const model = url.searchParams.get("model") || "";
	if (model === "deepgram-402") {
		return answer(402, { err_code: "ASR_PAYMENT_REQUIRED", err_msg: "Project does not have enough credits for an ASR request and does not have an overage agreement.", request_id: "a847f427-4ad5-4d67-9b95-db801e58251c" });
	}
	if (model === "deepgram-no-results") return answer(200, { request_id: "a847f427-4ad5-4d67-9b95-db801e58251c" });
	if (model !== "nova-3") return answer(400, { err_code: "Bad Request", err_msg: `unknown fixture model ${model}`, request_id: "a847f427-4ad5-4d67-9b95-db801e58251c" });
	if (url.searchParams.get("multichannel") === "true") {
		return answer(200, { metadata: deepgramMetadata(30.5, 2), results: { channels: [deepgramChannel("thank you for calling"), deepgramChannel("hello i'd like to order")] } });
	}
	return answer(200, { metadata: deepgramMetadata(25.933313, 1), results: { channels: [deepgramChannel("Yeah. As as much as, it's worth celebrating, the first, spacewalk.")] } });
};

// Folder 405 (TXE-6587, contract C7 / BF15 and C9 / BF20). GET /v1/videos is an OpenAI-shaped list recorded in
// two pages (the page after video_b is the last), and GET /v1/videos/video_gapped_*/content sends a file in two
// halves VIDEO_GAP_MS apart. Both are counted per caller key and, for the list, per query
// ("video.list@<bearer token>?<sorted query>", "video.content@<bearer token>"), so a case can prove which key
// reached the vendor, how often, and with which filters.
const VIDEO_GAP_MS = 1500;
const VIDEO_HALF = Buffer.alloc(256 * 1024, 0x1a);
const videoGapSends = [];
const bearerOf = (req) => String(req.headers.authorization || "").replace(/^Bearer\s+/i, "");
const listedVideo = (id, status, created_at) => ({ id, object: "video", model: "sora-fixture", status, created_at });
const videoListPage = (after) => after === "video_b"
	? { object: "list", data: [listedVideo("video_c", "failed", 1762700200)], first_id: "video_c", last_id: "video_c", has_more: false }
	: { object: "list", data: [listedVideo("video_a", "completed", 1762700000), listedVideo("video_b", "in_progress", 1762700100)], first_id: "video_a", last_id: "video_b", has_more: true };
// The download probe: {base_url, path, headers?}. It GETs the gateway and answers {data: [{status, content_type,
// bytes, first_byte_ms, end_ms, sends, gap_ms}]}: when the first byte and the last reached the probe, and when
// the fixture wrote each half of the latest gapped file, all in ms from the moment the probe sent.
const probeDownload = (spec, reply) => {
	const t0 = performance.now();
	const ms = (t) => Math.round((t - t0) * 10) / 10;
	const target = new URL(String(spec.path || ""), String(spec.base_url || ""));
	const upstream = http.request(target, { method: "GET", headers: { ...(spec.headers || {}) } }, (gw) => {
		let bytes = 0;
		let firstByte = null;
		gw.on("data", (c) => {
			if (firstByte === null) firstByte = ms(performance.now());
			bytes += c.length;
		});
		gw.on("end", () => reply(200, { data: [{ status: gw.statusCode, content_type: gw.headers["content-type"] || "", bytes, first_byte_ms: firstByte, end_ms: ms(performance.now()), sends: videoGapSends.map(ms), gap_ms: VIDEO_GAP_MS }] }));
	});
	upstream.on("error", (err) => reply(502, { error: { message: `probe could not reach the gateway: ${err.message}`, type: "probe_error" } }));
	upstream.end();
};

// Hit counters are keyed by the request body's model, so they must not inherit Object.prototype names.
const hits = Object.create(null);
// The exact bytes the last streamed answer for a model wrote, keyed by the request's model; GET /__sent?model=
// returns them and POST /__reset clears them (folder 329, TXE-6519).
const sentBodies = Object.create(null);

// OpenAI-shaped video, file and batch objects for folder 329 (TXE-6519): what the gateway's accounting
// sweeper would poll on its own after a video task or a batch is created. The video is complete as soon as it
// is asked for and the batch stays in progress, so a sweeper that is running has something to poll, and every
// call is counted under a fixed key (video.create, video.retrieve, video.content, video.delete, file.upload,
// file.content, batch.create, batch.retrieve), so a case can prove that nothing but its own requests arrived.
const FIXTURE_VIDEO_ID = "video_fixture_1";
const FIXTURE_BATCH_ID = "batch_fixture_1";
const FIXTURE_FILE_ID = "file-fixture-batch-input";
// Leading control byte: the collection's generic content check treats a body that is not JSON and starts with a
// control character as binary, which is how a real video's header reads too.
const FIXTURE_MP4 = Buffer.concat([Buffer.from([0x1a]), Buffer.from("FIXTURE-MP4 bytes served by provider-error-fixture")]);
const fixtureVideo = (status, progress) => ({
	id: FIXTURE_VIDEO_ID, object: "video", model: "sora-fixture", status, progress, created_at: 1,
	completed_at: status === "completed" ? 2 : null, expires_at: null, seconds: "4", size: "720x1280",
	remixed_from_video_id: null, error: null,
});
const fixtureBatch = (status) => ({
	id: FIXTURE_BATCH_ID, object: "batch", endpoint: "/v1/chat/completions", input_file_id: FIXTURE_FILE_ID,
	completion_window: "24h", status, created_at: 1, request_counts: { total: 1, completed: 0, failed: 0 },
});
const countHit = (key) => { hits[key] = (hits[key] || 0) + 1; };

// Zhipu bigmodel batch (folder 407, TXE-6665). bigmodel is the z.ai domestic site: OpenAI-shaped files and
// batches rooted at /api/paas/v4, not /v1. tkp_bigmodel in the fixture config is an openai-based custom provider
// written as Tokun's reconciler writes it (contract C12), its request_path_overrides naming those paths, and this
// is its only upstream: every call under /pp/bigmodel is kept for GET /__wire with its method and the name of the
// key it carried (never the key), and anything outside /api/paas/v4 is a 404, so a call the gateway built on the
// default /v1 path fails and shows in the wire. The output lines are the shape bigmodel documents
// (docs.bigmodel.cn/cn/guide/tools/batch.md, read 2026-11-10); the error line is OpenAI's shape, which is all
// the documentation says about the error file. No real bigmodel batch has been recorded yet.
const BIGMODEL_ROOT = "/pp/bigmodel/api/paas/v4";
const BIGMODEL_BATCH_ID = "batch_1791490810192076800";
const BIGMODEL_KEY_NAMES = { "Bearer bm-fixture-key-a": "tk-key-bigmodel-a", "Bearer bm-fixture-key-b": "tk-key-bigmodel-b" };
const bigmodelLine = (n, requestId, usage) => JSON.stringify({
	response: { status_code: 200, body: { created: 1715959701, usage, model: "glm-5.3-flash", id: `866835753385032054${n}`, choices: [{ finish_reason: "stop", index: 0, message: { role: "assistant", content: "ok" } }], request_id: requestId } },
	custom_id: `request-${n}`, id: BIGMODEL_BATCH_ID,
});
const BIGMODEL_FILES = {
	"file-bm-output": [
		bigmodelLine(1, "615-request-1", { completion_tokens: 26, prompt_tokens: 89, total_tokens: 115 }),
		bigmodelLine(2, "616-request-2", { completion_tokens: 22, prompt_tokens: 94, total_tokens: 116, prompt_tokens_details: { cached_tokens: 64 } }),
	].join("\n") + "\n",
	"file-bm-error": JSON.stringify({ response: { status_code: 400, body: { error: { code: "1210", message: "API 调用参数有误，请检查文档。" } } }, custom_id: "request-3", id: BIGMODEL_BATCH_ID }) + "\n",
};
const bigmodelBatch = (status, extra) => ({
	id: BIGMODEL_BATCH_ID, object: "batch", endpoint: "/v4/chat/completions", input_file_id: "file-bm-input", completion_window: "24h",
	status, created_at: 1715959000, request_counts: { total: 3, completed: 0, failed: 0 }, ...extra,
});
const bigmodelAnswer = (method, path, body) => {
	const notFound = { status: 404, body: errorBody(`bigmodel serves nothing at ${method} ${path}`, "invalid_request_error", "not_found") };
	if (!path.startsWith(BIGMODEL_ROOT + "/")) return notFound;
	const rest = path.slice(BIGMODEL_ROOT.length);
	if (method === "POST" && rest === "/files") {
		return { status: 200, body: JSON.stringify({ id: "file-bm-input", object: "file", bytes: 64, created_at: 1715959000, filename: "batch_requests.jsonl", purpose: "batch" }) };
	}
	if (method === "POST" && rest === "/batches") {
		return { status: 200, body: JSON.stringify(bigmodelBatch("validating", { endpoint: body.endpoint, input_file_id: body.input_file_id, metadata: body.metadata })) };
	}
	if (method === "GET" && rest === `/batches/${BIGMODEL_BATCH_ID}`) {
		return { status: 200, body: JSON.stringify(bigmodelBatch("completed", { output_file_id: "file-bm-output", error_file_id: "file-bm-error", request_counts: { total: 3, completed: 2, failed: 1 } })) };
	}
	if (method === "POST" && rest === `/batches/${BIGMODEL_BATCH_ID}/cancel`) {
		return { status: 200, body: JSON.stringify(bigmodelBatch("cancelling", { cancelling_at: 1715959100 })) };
	}
	const content = rest.match(/^\/files\/([^/]+)\/content$/);
	if (method === "GET" && content && Object.hasOwn(BIGMODEL_FILES, content[1])) {
		return { status: 200, contentType: "application/octet-stream", body: BIGMODEL_FILES[content[1]] };
	}
	return notFound;
};
// Reads and discards a request body (JSON or multipart) before answering, so the gateway's upload completes.
const drain = (req, then) => {
	req.on("data", () => {});
	req.on("end", then);
};

// AssemblyAI's Sync API (folder 404, TXE-6435): POST /pp/assemblyai/v1/transcribe answers in the vendor's
// shape, a transcript for universal-3-5-pro and an RFC 9457 problem body for the aai-upstream-* models. The
// model is the X-AAI-Model header, as on the real API. Every call is counted under "assemblyai:<model>" and
// the last one is kept for GET /__assemblyai (its headers and multipart parts), so a case can prove one
// request made one upstream call and what that call carried. POST /__reset clears it.
const assemblyAICalls = [];
const assemblyAITranscript = {
	text: "The quick brown fox.",
	words: [
		{ text: "The", confidence: 0.99, start: 140, end: 260 },
		{ text: "quick", confidence: 0.98, start: 260, end: 600 },
		{ text: "brown", confidence: 0.97, start: 600, end: 900 },
		{ text: "fox.", confidence: 0.96, start: 900, end: 1500 },
	],
	confidence: 0.975,
	audio_duration_ms: 3312,
	session_id: "0d6c1f7e-6a3b-4a55-9f0e-2a4b8f1c9d10",
	request_time_ms: 412.6,
};
const assemblyAIProblems = {
	"aai-upstream-429": { status: 429, headers: { "Retry-After": "7" }, title: "Too Many Requests", detail: "Rate limit exceeded." },
	"aai-upstream-400": { status: 400, headers: {}, title: "Audio Too Short", detail: "Audio must be at least 80 ms long." },
};
// The parts of a multipart/form-data body: name, filename, Content-Type, byte length, and the text of a JSON part.
const multipartParts = (contentType, body) => {
	const boundary = /boundary=(?:"([^"]+)"|([^;]+))/i.exec(contentType || "");
	if (!boundary) return [];
	return body.toString("latin1").split(`--${boundary[1] || boundary[2]}`).slice(1, -1).map((chunk) => {
		const split = chunk.indexOf("\r\n\r\n");
		const head = chunk.slice(0, split);
		const content = chunk.slice(split + 4, chunk.endsWith("\r\n") ? -2 : undefined);
		const type = /^content-type:\s*(.+)$/im.exec(head)?.[1].trim() || "";
		const part = { name: /name="([^"]*)"/i.exec(head)?.[1] || "", filename: /filename="([^"]*)"/i.exec(head)?.[1] || "", contentType: type, bytes: content.length };
		if (type === "application/json") part.text = Buffer.from(content, "latin1").toString("utf8");
		return part;
	});
};


// Stale pooled connections, TXE-7071. GET /v1/videos/video_stale_hold is held until staleHold.want requests
// are waiting, so the gateway needs one upstream connection for each. Then every one is answered and, 50 ms
// later, its socket is closed with no Connection: close header, which leaves the gateway's pool for this
// provider holding that many dead connections. GET /v1/videos/video_stale_after is answered at once and counted.
// POST /__probe/stale-get is the client: {base_url, hold_path, path, headers?, count?}. It sends count
// concurrent GETs of hold_path to the gateway, waits for the closes to land, then sends one GET of path.
// Answers {data: [{burst: [status...], connections, after: {status, body}, after_upstream_requests}]}:
// connections is how many upstream sockets carried the burst, after_upstream_requests how many times the last
// GET reached this fixture. A gateway that fails a GET on a pool of dead connections answers after.status 502.
const STALE_HOLD_ID = "video_stale_hold";
const STALE_AFTER_ID = "video_stale_after";
const staleHold = { want: 0, waiting: [], timer: null, afterHits: 0 };
const flushStaleHold = () => {
	clearTimeout(staleHold.timer);
	staleHold.want = 0;
	const body = JSON.stringify(fixtureVideo("completed", 100));
	for (const { req, res } of staleHold.waiting.splice(0)) {
		res.writeHead(200, { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(body) });
		res.end(body, () => setTimeout(() => req.socket.destroy(), 50));
	}
};
const probeStaleGet = (spec, reply) => {
	const count = Math.min(Math.max(Number(spec.count) || 6, 1), 32);
	const get = (path) => new Promise((resolve) => {
		const request = http.request(new URL(String(path || ""), String(spec.base_url || "")), { method: "GET", headers: spec.headers || {}, agent: false }, (gw) => {
			const chunks = [];
			gw.on("data", (c) => chunks.push(c));
			gw.on("end", () => resolve({ status: gw.statusCode, body: Buffer.concat(chunks).toString("utf8").slice(0, 600) }));
		});
		request.on("error", (err) => resolve({ status: 0, body: `probe could not reach the gateway: ${err.message}` }));
		request.end();
	});
	flushStaleHold();
	staleHold.want = count;
	staleHold.afterHits = 0;
	const sockets = new Set();
	staleHold.sockets = sockets;
	// A gateway that never opens count connections must not hang the probe.
	staleHold.timer = setTimeout(flushStaleHold, 8000);
	Promise.all(Array.from({ length: count }, () => get(spec.hold_path))).then((burst) => {
		// The closes land 50 ms after each answer; give the gateway's sockets time to see them.
		setTimeout(() => {
			get(spec.path).then((after) => reply(200, { data: [{ burst: burst.map((b) => b.status), connections: sockets.size, after, after_upstream_requests: staleHold.afterHits }] }));
		}, 500);
	});
};

const handleRequest = (req, res) => {
	const path = new URL(req.url, "http://localhost").pathname;
	const send = (status, headers, body) => {
		res.writeHead(status, { "Content-Type": "application/json", ...headers });
		res.end(body);
	};
	// A fresh gateway downloads a pricing datasheet and a model-parameters datasheet on first start, and the model
	// catalog decides which request types a model supports (gpt-5-pro is Responses-only, so a chat request to it is
	// converted). Serving both from here keeps the run offline and the result independent of a remote file.
	if (req.method === "GET" && path === "/pricing.json") {
		return send(200, {}, JSON.stringify(fixturePricing));
	}
	if (req.method === "GET" && path === "/model-parameters.json") {
		// supported_endpoints/mode feed the catalog's per-model request types: gpt-5-pro answers on /v1/responses only.
		return send(200, {}, JSON.stringify({
			"gpt-5-pro": { provider: "openai", mode: "responses", supported_endpoints: ["/v1/responses"] },
			"pp-custom-legacy": { provider: "openai", mode: "responses", supported_endpoints: ["/v1/responses"], supports_custom_tools: false, supports_function_calling: true },
			// TXE-6527: a model whose capability record lists its service tiers, so a tier outside the list is refused.
			"oc-tiered": { provider: "moonshot", mode: "chat", service_tiers: ["default", "priority"] },
			// TXE-7026: two models with a record that lists no parameters and a low output limit, so anything the compat
			// plugin would drop or lower for them shows in the request the fixture records.
			"cp-capped": { provider: "cp_plain", mode: "chat", max_output_tokens: 64, supported_endpoints: ["/v1/chat/completions", "/v1/responses"] },
			"oc-capped": { provider: "moonshot", mode: "chat", max_output_tokens: 64 },
			// TXE-6439: a capability record on each vendor custom provider that lists its service tiers, so a tier
			// outside the list is one the model cannot take (folder 408).
			"hy-tiered": { provider: "tencent_tokenhub", mode: "chat", service_tiers: ["default", "priority"] },
			"step-tiered": { provider: "stepfun", mode: "chat", service_tiers: ["default", "priority"] },
			"mimo-tiered": { provider: "xiaomi", mode: "chat", service_tiers: ["default", "priority"] },
			// TXE-6548: a DeepSeek model served under another id, as DeepSeek serves deepseek-v4-flash as deepseek-flash, so the
			// native Messages rows can read the served model the gateway stamps from server_side_model.
			"fx-deepseek-flash": { provider: "deepseek", mode: "chat", server_side_model: "fx-deepseek-served" },
		}));
	}
	if (req.method === "POST" && path === "/__reset") {
		for (const k of Object.keys(hits)) delete hits[k];
		openAPICalls.length = 0;
		wireCalls.length = 0;
		for (const k of Object.keys(sentBodies)) delete sentBodies[k];
		assemblyAICalls.length = 0;
		return send(200, {}, JSON.stringify({ data: [{ reset: true }] }));
	}
	if (req.method === "POST" && path === "/pp/assemblyai/v1/transcribe") {
		const body = [];
		req.on("data", (c) => body.push(c));
		req.on("end", () => {
			const model = req.headers["x-aai-model"] || "";
			countHit(`assemblyai:${model}`);
			assemblyAICalls.push({
				model,
				authorization: req.headers.authorization || "",
				parts: multipartParts(req.headers["content-type"], Buffer.concat(body)),
			});
			const problem = assemblyAIProblems[model];
			if (problem) {
				res.writeHead(problem.status, { "Content-Type": "application/problem+json", ...problem.headers });
				return res.end(JSON.stringify({ status: problem.status, title: problem.title, detail: problem.detail }));
			}
			send(200, { "x-aai-fixture": "sync" }, JSON.stringify(assemblyAITranscript));
		});
		return undefined;
	}
	if (req.method === "GET" && path === "/__assemblyai") {
		return send(200, {}, JSON.stringify({ data: [{ calls: assemblyAICalls }] }));
	}
	if (req.method === "GET" && path === "/openapi-fixture/docs/no-servers.json") {
		return send(200, {}, JSON.stringify(openAPIDocument));
	}
	if (req.method === "GET" && path === "/__openapi-calls") {
		return send(200, {}, JSON.stringify({ data: [{ calls: openAPICalls }] }));
	}
	if (path.startsWith("/fixture-api/")) {
		const body = [];
		req.on("data", (c) => body.push(c));
		req.on("end", () => {
			openAPICalls.push({ method: req.method, path, body: Buffer.concat(body).toString("utf8") });
			send(200, {}, JSON.stringify({ ok: true }));
		});
		return undefined;
	}
	if (req.method === "GET" && path === "/__wire") {
		return send(200, {}, JSON.stringify({ data: [{ calls: wireCalls }] }));
	}
	if (path.startsWith("/pp/bigmodel/")) {
		const body = [];
		req.on("data", (c) => body.push(c));
		req.on("end", () => {
			const bytes = Buffer.concat(body).toString("utf8");
			wireCalls.push({ method: req.method, path, query: new URL(req.url, "http://localhost").search, body: bytes, key: BIGMODEL_KEY_NAMES[req.headers.authorization] || "unknown" });
			let parsed = {};
			try { parsed = JSON.parse(bytes) || {}; } catch {}
			const answer = bigmodelAnswer(req.method, path, parsed);
			send(answer.status, answer.contentType ? { "Content-Type": answer.contentType } : {}, answer.body);
		});
		return undefined;
	}
	if (req.method === "POST" && path === DEEPGRAM_LISTEN_PATH) {
		const body = [];
		req.on("data", (c) => body.push(c));
		req.on("end", () => deepgramAnswer(req, res, Buffer.concat(body)));
		return undefined;
	}
	// Folder 482 (TXE-7036): a finished BytePlus or MiniMax task names its file by URL, and the gateway fetches
	// that URL through the SSRF-hardened media fetcher. Task ids ending in "media-gapped" name the gapped file at
	// a public-looking address, which the gateway reaches through the media tunnel (the providers' proxy_config);
	// ids ending in "media-loopback" name a file on this fixture's own loopback address, which the gateway must
	// refuse without asking for it. Each file request is counted as "media.<file>@<Host header>".
	const mediaTask = /^\/pp\/(byteplus\/api\/v3\/contents\/generations\/tasks|minimax\/v2\/query\/video_generation)\/[^/]*media-(gapped|loopback)$/.exec(path);
	if (req.method === "GET" && mediaTask) {
		const id = decodeURIComponent(path.slice(path.lastIndexOf("/") + 1));
		const fileURL = mediaTask[2] === "gapped"
			? `http://${MEDIA_PUBLIC_HOST}/pp/media/gapped.mp4?X-Fixture-Signature=signed`
			: `http://127.0.0.1:${port}/pp/media/internal.mp4?X-Fixture-Signature=signed`;
		countHit(`media.task@${mediaTask[1].split("/")[0]}`);
		if (mediaTask[1].startsWith("byteplus")) {
			return send(200, {}, JSON.stringify({ id, model: "seedance-1-0-pro-fast-251015", status: "succeeded", content: { video_url: fileURL }, resolution: "720p", ratio: "16:9", duration: 5 }));
		}
		return send(200, {}, JSON.stringify({ task: { id, model: "MiniMax-H3", status: "succeeded", content: { url: fileURL }, resolution: "768P", ratio: "16:9", duration: 6 } }));
	}
	if (req.method === "GET" && path === "/pp/media/gapped.mp4") {
		countHit(`media.gapped@${req.headers.host}`);
		videoGapSends.length = 0;
		res.writeHead(200, { "Content-Type": "video/mp4", "Content-Length": 2 * VIDEO_HALF.length });
		videoGapSends.push(performance.now());
		res.write(VIDEO_HALF);
		setTimeout(() => {
			videoGapSends.push(performance.now());
			res.end(VIDEO_HALF);
		}, VIDEO_GAP_MS);
		return undefined;
	}
	// The poll of a folder 490 prediction, counted under the Host it was asked at. A picture prediction's
	// output is at the address its name says; any other prediction's is a delivery URL nobody fetches.
	if (req.method === "GET" && path.startsWith("/pp/replicate/v1/predictions/pred_")) {
		const id = path.slice(path.lastIndexOf("/") + 1);
		countHit(`replicate.poll@${String(req.headers.host).split(":")[0]}`);
		const output = id === "pred_picture_public" ? `http://${MEDIA_PUBLIC_HOST}/pp/media/image.png?X-Fixture-Signature=signed`
			: id === "pred_picture_loopback" ? `http://127.0.0.1:${port}/pp/media/internal.png?X-Fixture-Signature=signed`
				: "https://replicate-delivery.invalid/fixture/out.png";
		return send(200, {}, JSON.stringify({ id, status: "succeeded", output }));
	}
	// The stream of a folder 490 prediction: one output event and the done event, in Replicate's SSE shape.
	if (req.method === "GET" && path.startsWith("/pp/replicate/v1/streams/pred_")) {
		countHit(`replicate.stream@${String(req.headers.host).split(":")[0]}`);
		res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
		return res.end("event: output\nid: 1\ndata: fixture stream text\n\nevent: done\nid: 2\ndata: {}\n\n");
	}
	// Folder 490: Replicate's deployments list for a key that uses the deployments endpoint. The first page
	// names the next one: on the provider's own origin for the key "fixture-deploy-origin", on another
	// loopback origin for "fixture-deploy-loopback". Each page is counted by key, page and Host.
	if (req.method === "GET" && path === "/pp/replicate/v1/deployments") {
		const key = bearerOf(req);
		const page = new URL(req.url, "http://localhost").searchParams.get("page") || "1";
		countHit(`replicate.deployments@${key}?page=${page}@${String(req.headers.host).split(":")[0]}`);
		const deployment = (name) => ({ owner: "fixture", name, current_release: null });
		if (page === "2") return send(200, {}, JSON.stringify({ next: null, previous: null, results: [deployment(`${key}-page-2`)] }));
		const host = key === "fixture-deploy-loopback" ? "localhost" : "127.0.0.1";
		return send(200, {}, JSON.stringify({ next: `http://${host}:${port}/pp/replicate/v1/deployments?page=2`, previous: null, results: [deployment(`${key}-page-1`)] }));
	}
	// The image files a folder 490 prediction names, counted under the Host they were asked at.
	if (req.method === "GET" && (path === "/pp/media/image.png" || path === "/pp/media/internal.png")) {
		countHit(`media.${path === "/pp/media/image.png" ? "image" : "internal"}@${String(req.headers.host).split(":")[0]}`);
		res.writeHead(200, { "Content-Type": "image/png" });
		return res.end(Buffer.from("iVBORw0KGgo=", "base64"));
	}
	if (req.method === "GET" && path === "/pp/media/internal.mp4") {
		countHit(`media.internal@${req.headers.host}`);
		return send(200, { "Content-Type": "video/mp4" }, "internal");
	}
	// MiniMax video query is a GET; the task id "base-resp-<code>" selects the base_resp code (TXE-6588).
	if (req.method === "GET" && path === "/pp/storage/v1/batches") {
		const answer = storageBatchList();
		return send(answer.status, {}, answer.body);
	}
	const storageBatch = /^\/pp\/storage\/v1\/batches\/([^/]+)$/.exec(path);
	if (req.method === "GET" && storageBatch) {
		const answer = storageBatchRetrieve(decodeURIComponent(storageBatch[1]));
		return send(answer.status, {}, answer.body);
	}
	if (req.method === "GET" && path.startsWith("/pp/minimax/v2/query/video_generation/")) {
		wireCalls.push({ path, query: new URL(req.url, "http://localhost").search, body: "" });
		const answer = miniMaxBaseRespAnswer(decodeURIComponent(path.slice("/pp/minimax/v2/query/video_generation/".length)));
		if (answer) return send(answer.status, {}, answer.body);
		return send(404, {}, JSON.stringify({ error: "unknown fixture task" }));
	}
	if ((req.method === "GET" || req.method === "DELETE") && path.startsWith(SEEDANCE_TASKS + "/")) {
		const id = decodeURIComponent(path.slice(SEEDANCE_TASKS.length + 1));
		req.resume();
		if (req.method === "DELETE") {
			countHit(`seedance.delete ${id}`);
			return send(200, {}, "");
		}
		countHit(`seedance.retrieve ${id}`);
		return send(200, {}, JSON.stringify(seedanceTask(id)));
	}
	if (req.method === "POST" && (path.startsWith("/pp/") || path === "/v1/embeddings" || path === "/anthropic/v1/messages" || isOpenAIImagePath(path) || isBedrockConversePath(path))) {
		const body = [];
		req.on("data", (c) => body.push(c));
		req.on("end", () => {
			const bytes = Buffer.concat(body).toString("utf8");
			const call = { path, query: new URL(req.url, "http://localhost").search, body: bytes };
			// An image edit is a form or a JSON body (TXE-6528), so its row also says which one arrived.
			if (isOpenAIImagePath(path)) call.contentType = req.headers["content-type"] || "";
			// Folder 487 reads which key the gateway sent (TXE-7097).
			if (path.startsWith("/pp/ownkey/")) call.bearer = bearerOf(req);
			wireCalls.push(call);
			let parsed = {};
			try { parsed = JSON.parse(bytes) || {}; } catch {}
			const answer = wireAnswer(path, parsed, bytes);
			// The request was read in full; the connection dies before any response byte.
			if (answer.drop) return req.socket.destroy();
			if (answer.stream !== undefined) {
				res.writeHead(answer.status, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
				return res.end(answer.stream);
			}
			send(answer.status, {}, answer.body);
		});
		return undefined;
	}
	if (req.method === "POST" && path === "/__probe/stale-get") {
		const body = [];
		req.on("data", (c) => body.push(c));
		req.on("end", () => {
			let spec;
			try { spec = JSON.parse(Buffer.concat(body).toString("utf8")); }
			catch { return send(400, {}, errorBody("invalid JSON", "invalid_request_error", "invalid_json")); }
			probeStaleGet(spec, (status, answer) => send(status, {}, JSON.stringify(answer)));
		});
		return undefined;
	}
	if (req.method === "POST" && (path === AZURE_SPEECH_TTS_PATH || path === AZURE_SPEECH_STT_PATH)) {
		const body = [];
		req.on("data", (c) => body.push(c));
		req.on("end", () => azureSpeechAnswer(req, res, path, Buffer.concat(body)));
		return undefined;
	}
	if (req.method === "POST" && path === "/__probe/stream-gaps") {
		const body = [];
		req.on("data", (c) => body.push(c));
		req.on("end", () => {
			let spec;
			try { spec = JSON.parse(Buffer.concat(body).toString("utf8")); }
			catch { return send(400, {}, errorBody("invalid JSON", "invalid_request_error", "invalid_json")); }
			probeStreamGaps(spec, (status, answer) => send(status, {}, JSON.stringify(answer)));
		});
		return undefined;
	}
	if (req.method === "POST" && path === "/__probe/download") {
		const body = [];
		req.on("data", (c) => body.push(c));
		req.on("end", () => {
			let spec;
			try { spec = JSON.parse(Buffer.concat(body).toString("utf8")); }
			catch { return send(400, {}, errorBody("invalid JSON", "invalid_request_error", "invalid_json")); }
			probeDownload(spec, (status, answer) => send(status, {}, JSON.stringify(answer)));
		});
		return undefined;
	}
	if (req.method === "GET" && path === "/__hits") {
		return send(200, {}, JSON.stringify({ data: [{ hits: { ...hits } }] }));
	}
	if (req.method === "GET" && path === "/__sent") {
		const model = new URL(req.url, "http://localhost").searchParams.get("model") || "";
		return send(200, {}, JSON.stringify({ data: [{ model, body: Object.hasOwn(sentBodies, model) ? sentBodies[model] : null }] }));
	}
	// BFL's task query (TXE-6431), counted under "bfl get_result <id>".
	if (req.method === "GET" && path === "/pp/bfl/v1/get_result") {
		const id = new URL(req.url, "http://localhost").searchParams.get("id") || "";
		countHit(`bfl get_result ${id}`);
		return send(200, {}, JSON.stringify(bflResult(id)));
	}
	// Sora 2 on two Azure resources (folder 419, TXE-6532): each key's endpoint is its own path
	// prefix, so a counter names the resource a request reached and the key it carried. Every
	// create's form fields are kept for /__sent?model=sora-form. A prompt containing "blocked" gets
	// the content filter error of Azure's v1-preview schema.
	const soraMatch = path.match(/^\/sora-(a|b)\/openai\/v1\/videos(?:\/([^/]+)(\/content)?)?$/);
	if (soraMatch) {
		const [, resource, soraId, soraContent] = soraMatch;
		const chunks = [];
		req.on("data", (c) => chunks.push(c));
		req.on("end", () => {
			countHit(`sora ${req.method} ${resource} ${req.headers["api-key"] || ""}${soraContent ? " content" : ""}`);
			if (req.method === "POST" && !soraId) {
				const form = {};
				const raw = Buffer.concat(chunks).toString("latin1");
				for (const m of raw.matchAll(/name="([^"]+)"[^\r\n]*\r\n(?:[^\r\n]+\r\n)*\r\n([\s\S]*?)\r\n--/g)) form[m[1]] = m[2];
				sentBodies["sora-form"] = JSON.stringify(form);
				if ((form.prompt || "").includes("blocked")) {
					return send(400, {}, JSON.stringify({ error: { code: "invalid_prompt", message: "Your request was rejected by the safety system.", param: "prompt", type: "invalid_request_error", inner_error: { code: "ResponsibleAIPolicyViolation", error_details: { sexual: { filtered: true, severity: "high" } } } } }));
				}
				return send(200, {}, JSON.stringify({ id: "video_fixture_sora", object: "video", status: "queued", created_at: 1, model: form.model, seconds: form.seconds || "4", size: form.size }));
			}
			if (req.method === "GET" && soraContent) {
				res.writeHead(200, { "Content-Type": "video/mp4", "Content-Length": FIXTURE_MP4.length });
				return res.end(FIXTURE_MP4);
			}
			if (req.method === "GET" && soraId) {
				return send(200, {}, JSON.stringify({ id: decodeURIComponent(soraId), object: "video", status: "completed", created_at: 1, completed_at: 2, model: "sora-2", seconds: "8", size: "1280x720" }));
			}
			if (req.method === "DELETE" && soraId) {
				return send(200, {}, JSON.stringify({ id: decodeURIComponent(soraId), object: "video.deleted", deleted: true }));
			}
			return send(404, {}, errorBody("not found", "invalid_request_error", "not_found"));
		});
		return undefined;
	}
	// The OpenAI video task routes (TXE-6521). Every request is counted under
	// "videos <METHOD> <bearer token>", so a case can prove which key a task query used and that a
	// request the gateway refuses never got here. A query answers a queued task; a create answers the
	// same task. Nothing else exists upstream: OpenAI has no cancel API. Only the keys of openai_video_keys
	// (video-secret-a, video-secret-b) are served here; every other key reaches the folder 329 routes below.
	const bearer = (req.headers.authorization || "").replace(/^Bearer /, "");
	if (bearer.startsWith("video-secret-") && (path === "/v1/videos" || path.startsWith("/v1/videos/"))) {
		const counter = `videos ${req.method} ${bearer}`;
		hits[counter] = (hits[counter] || 0) + 1;
		req.resume();
		const id = decodeURIComponent(path.slice("/v1/videos/".length));
		if (req.method === "GET" && id && !id.includes("/")) {
			return send(200, {}, JSON.stringify(videoTask(id)));
		}
		if (req.method === "POST" && path === "/v1/videos") {
			return send(200, {}, JSON.stringify(videoTask("video_fixture_created")));
		}
		return send(404, {}, errorBody("not found", "invalid_request_error", "not_found"));
	}
	if (req.method === "GET" && path === "/mcp") {
		return send(405, {}, errorBody("no server stream", "invalid_request_error", "method_not_allowed"));
	}
	// Video, file and batch endpoints in OpenAI shape (folder 329, TXE-6519); see fixtureVideo above.
	const videoMatch = path.match(/^\/v1\/videos\/([^/]+)(\/content)?$/);
	// The stale-connection probe's two tasks (TXE-7071); see staleHold above.
	if (req.method === "GET" && videoMatch && !videoMatch[2] && videoMatch[1] === STALE_HOLD_ID) {
		if (staleHold.want === 0) {
			return send(200, {}, JSON.stringify(fixtureVideo("completed", 100)));
		}
		staleHold.sockets.add(req.socket);
		staleHold.waiting.push({ req, res });
		if (staleHold.waiting.length >= staleHold.want) flushStaleHold();
		return undefined;
	}
	if (req.method === "GET" && videoMatch && !videoMatch[2] && videoMatch[1] === STALE_AFTER_ID) {
		staleHold.afterHits++;
		return send(200, {}, JSON.stringify(fixtureVideo("completed", 100)));
	}
	if (req.method === "POST" && path === "/v1/videos") {
		return drain(req, () => {
			countHit("video.create");
			send(200, {}, JSON.stringify(fixtureVideo("queued", 0)));
		});
	}
	if (req.method === "GET" && path === "/v1/videos") {
		const query = new URL(req.url, "http://localhost").searchParams;
		query.sort();
		countHit(`video.list@${bearerOf(req)}?${query.toString()}`);
		return send(200, {}, JSON.stringify(videoListPage(query.get("after"))));
	}
	if (req.method === "GET" && videoMatch && videoMatch[2] && videoMatch[1].startsWith("video_gapped_")) {
		countHit(`video.content@${bearerOf(req)}`);
		videoGapSends.length = 0;
		res.writeHead(200, { "Content-Type": "video/mp4", "Content-Length": 2 * VIDEO_HALF.length });
		videoGapSends.push(performance.now());
		res.write(VIDEO_HALF);
		setTimeout(() => {
			videoGapSends.push(performance.now());
			res.end(VIDEO_HALF);
		}, VIDEO_GAP_MS);
		return undefined;
	}
	if (req.method === "GET" && videoMatch && videoMatch[2]) {
		countHit("video.content");
		res.writeHead(200, { "Content-Type": "video/mp4", "Content-Length": FIXTURE_MP4.length });
		return res.end(FIXTURE_MP4);
	}
	if (req.method === "GET" && videoMatch) {
		countHit("video.retrieve");
		return send(200, {}, JSON.stringify(fixtureVideo("completed", 100)));
	}
	if (req.method === "DELETE" && videoMatch) {
		countHit("video.delete");
		return send(200, {}, JSON.stringify({ id: videoMatch[1], object: "video.deleted", deleted: true }));
	}
	if (req.method === "POST" && path === "/v1/files") {
		return drain(req, () => {
			countHit("file.upload");
			send(200, {}, JSON.stringify({ id: FIXTURE_FILE_ID, object: "file", bytes: 64, created_at: 1, filename: "batch_requests.jsonl", purpose: "batch", status: "processed" }));
		});
	}
	if (req.method === "GET" && /^\/v1\/files\/[^/]+\/content$/.test(path)) {
		countHit("file.content");
		res.writeHead(200, { "Content-Type": "application/jsonl" });
		return res.end("");
	}
	if (req.method === "POST" && path === "/v1/batches") {
		return drain(req, () => {
			countHit("batch.create");
			send(200, {}, JSON.stringify(fixtureBatch("validating")));
		});
	}
	if (req.method === "GET" && /^\/v1\/batches\/[^/]+$/.test(path)) {
		countHit("batch.retrieve");
		return send(200, {}, JSON.stringify(fixtureBatch("in_progress")));
	}
	if (req.method !== "POST" || (path !== "/v1/chat/completions" && path !== "/openai/v1/chat/completions" && path !== "/v1/responses" && path !== "/v1/live/sessions" && path !== "/mcp" && path !== "/v1/decisions" && path !== "/alpha/decisions" && path !== "/v1/audio/speech" && path !== "/v1/completions" && path !== "/chat/completions")) {
		return send(404, {}, errorBody("not found", "invalid_request_error", "not_found"));
	}
	const chunks = [];
	req.on("data", (c) => chunks.push(c));
	req.on("end", () => {
		if (path === "/mcp") {
			let message = {};
			try { message = JSON.parse(Buffer.concat(chunks).toString("utf8")) || {}; } catch {}
			if (message.id === undefined) {
				res.writeHead(202);
				return res.end();
			}
			return send(200, {}, JSON.stringify(mcpAnswer(message)));
		}
		// Creating a session incurs the WebRTC minimum before the relay validates this answer.
		if (path === "/v1/live/sessions") {
			let body;
			try { body = JSON.parse(Buffer.concat(chunks).toString("utf8")); }
			catch { return send(400, {}, errorBody("invalid JSON", "invalid_request_error", "invalid_json")); }
			if (!body?.session?.model || typeof body.session.instructions !== "string" || body.transport?.type !== "webrtc") {
				return send(400, {}, errorBody("session and WebRTC transport are required", "invalid_request_error", "invalid_session"));
			}
			if (body.session.instructions.startsWith("filtered-live-create-")) {
				const permissions = body.session.client?.data_channel;
				const selectors = permissions?.allowed_server_events;
				const topLevel = ["session.started", "session.updated", "session.closed", "session.usage.updated", "session.delegation.created", "session.input_transcript.delta", "session.output_transcript.delta", "error"];
				const responses = ["response.output_item.done", "response.completed", "response.incomplete", "response.failed"];
				const has = (type, nested) => Array.isArray(selectors) && selectors.some((event) => event?.type === type && event.response_event === nested);
				if (!topLevel.every((type) => has(type, undefined)) || !responses.every((type) => has("response.event", type))) {
					return send(400, {}, errorBody("gateway accounting events missing from upstream permissions", "invalid_request_error", "missing_gateway_events"));
				}
				if (JSON.stringify(permissions.allowed_client_events) !== '["session.close"]') {
					return send(400, {}, errorBody("frontend command permissions changed", "invalid_request_error", "changed_client_permissions"));
				}
			}
			if (body.session?.instructions === "refuse-live-create") {
				return send(403, {}, errorBody("fixture refused live session creation", "permission_error", "session_refused"));
			}
			// A created session without an answer has still incurred the minimum.
			if (body.session?.instructions === "answerless-live-create") {
				return send(201, {}, JSON.stringify({ session: { id: "live_" + body.session.instructions, model: body.session.model } }));
			}
			return send(201, {}, JSON.stringify({
				session: { id: "live_" + body.session.instructions, model: body.session.model },
				transport: { type: "webrtc", sdp: "invalid SDP answer" },
			}));
		}
		let parsed = {};
		try {
			parsed = JSON.parse(Buffer.concat(chunks).toString("utf8")) || {};
		} catch {}
		// A model that is not a string names no behaviour, so it is answered as an unknown model rather
		// than reaching the string handling below, which would throw and stop the fixture.
		const model = typeof parsed.model === "string" ? parsed.model : "";
		hits[model] = (hits[model] || 0) + 1;
		// test-key is the only credential the fixture accepts. A request bearing any other key is refused
		// with 401 before its model is looked at, which is how a provider refuses a caller's direct key.
		const auth = req.headers.authorization;
		if (auth !== undefined && auth !== "Bearer test-key") {
			return send(401, {}, errorBody("Incorrect API key provided", "invalid_request_error", "invalid_api_key"));
		}
		if (path === "/chat/completions") {
			const answer = perplexityChat(model, parsed.stream === true);
			if (answer.stream !== undefined) {
				res.writeHead(answer.status, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
				return res.end(answer.stream);
			}
			return send(answer.status, {}, answer.body);
		}
		// Speech, TXE-6522: an error behaviour answers with its status before any frame; upstream-gapped streams its
		// deltas GAP_MS apart when stream_format is "sse" and otherwise answers with audio bytes.
		if (path === "/v1/audio/speech") {
			const speechName = model.split("~")[0];
			if (Object.hasOwn(behaviours, speechName) && behaviours[speechName]().status !== 200) {
				const failing = behaviours[speechName]();
				return send(failing.status, failing.headers, failing.body);
			}
			// TXE-6517: a 200 with no audio and no error object, which Bifrost turns into 502 provider_empty_result.
			if (speechName === "upstream-speech-empty") {
				const empty = behaviours[speechName]();
				return send(empty.status, empty.headers, empty.body);
			}
			if (speechName !== "upstream-gapped") {
				return send(404, {}, errorBody(`unknown fixture model ${model}`, "invalid_request_error", "model_not_found"));
			}
			if (parsed.stream_format === "sse") return gappedSpeech(res);
			res.writeHead(200, { "Content-Type": "audio/mpeg" });
			return res.end(Buffer.from("fixture-audio"));
		}
		if (model.split("~")[0] === "upstream-gapped" && path !== "/v1/responses") {
			if (parsed.stream === true) return gappedChat(res, !!(parsed.stream_options && parsed.stream_options.include_usage));
			return send(200, {}, JSON.stringify({
				id: "chatcmpl-upstream-gapped", object: "chat.completion", created: 1, model: "upstream-gapped",
				choices: [{ index: 0, message: { role: "assistant", content: Array.from({ length: GAPPED_FRAMES }, (_, i) => gapMarker(i)).join("") }, finish_reason: "stop" }],
				usage: { prompt_tokens: 5, completion_tokens: GAPPED_FRAMES, total_tokens: 5 + GAPPED_FRAMES },
			}));
		}
		if (path === "/v1/completions") {
			if (model !== "upstream-usage-extended") {
				return send(404, {}, errorBody(`unknown fixture model ${model}`, "invalid_request_error", "model_not_found"));
			}
			if (parsed.stream !== true) return send(200, {}, JSON.stringify(textCompletionAnswer(false)));
			res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
			return res.end(textCompletionAnswer(true));
		}
		if (model === "upstream-fixed-length-short" || model === "upstream-fixed-length-ok") {
			// finish_reason makes this semantically complete even without [DONE].
			// Only the transport's Content-Length check can reject the short body.
			const payload = [
				{ id: "chatcmpl-fixed-length", object: "chat.completion.chunk", model, choices: [{ index: 0, delta: { role: "assistant", content: "hello" } }] },
				{ id: "chatcmpl-fixed-length", object: "chat.completion.chunk", model, choices: [{ index: 0, delta: {}, finish_reason: "stop" }], usage: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 } },
			].map((chunk) => `data: ${JSON.stringify(chunk)}\n\n`).join("");
			const truncated = model === "upstream-fixed-length-short";
			const socket = res.socket;
			res.writeHead(200, {
				"Content-Type": "text/event-stream",
				"Content-Length": Buffer.byteLength(payload) + (truncated ? 100 : 0),
			});
			return res.end(payload, () => {
				if (truncated) socket.end(); // Graceful EOF without a Connection: close header.
			});
		}
		if (path === "/alpha/decisions") {
			return send(200, {}, JSON.stringify(openRouterDecisionAnswer()));
		}
		if (path === "/v1/responses" && model === "pp-custom-legacy") {
			wireCalls.push({ path, query: "", body: Buffer.concat(chunks).toString("utf8") });
			if (parsed.stream !== true) return send(200, {}, JSON.stringify(legacyCustomToolResponse(model)));
			res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
			return res.end(legacyCustomToolStream(model));
		}
		if (path === "/v1/responses" && model === "gpt-6-pp-search") {
			wireCalls.push({ path, query: "", body: Buffer.concat(chunks).toString("utf8") });
		}
		if (path === "/v1/responses" && model === "decision-emulated") {
			return send(200, {}, JSON.stringify({
				id: "resp_decision_emulated", object: "response", created_at: 1, status: "completed", model: "decision-emulated-2026",
				output: [{ id: "msg_decision_emulated", type: "message", role: "assistant", status: "completed", content: [{ type: "output_text", text: "no decision", annotations: [] }] }],
				usage: { input_tokens: 1000, input_tokens_details: { cached_tokens: 800 }, output_tokens: 10, total_tokens: 1010 },
			}));
		}
		if (path === "/v1/decisions") {
			const answer = nativeDecisionAnswer(parsed);
			return answer ? send(200, {}, JSON.stringify(answer)) : send(404, {}, errorBody(`unknown fixture model ${model}`, "invalid_request_error", "model_not_found"));
		}
		if (isTurnsModel(model)) {
			const done = path === "/v1/responses" ? responsesTurnsDone(parsed) : chatTurnsDone(parsed);
			if (model === "turns-fail" && done === 2) {
				return send(500, {}, errorBody("internal server error", "server_error", "internal_error"));
			}
			const second = model !== "turns-fail" && done > 0;
			if (path === "/v1/responses") {
				const response = responsesTurn(model, second);
				if (parsed.stream !== true) return send(200, {}, JSON.stringify(response));
				res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
				return res.end(responsesTurnStream(response));
			}
			const completion = chatTurn(model, second);
			if (parsed.stream !== true) return send(200, {}, JSON.stringify(completion));
			res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
			return res.end(chatTurnStream(completion));
		}
		// The model's behaviour picks the response, so only an own entry may be looked up: "constructor" or
		// "toString" must be an unknown model, not an inherited function.
		const name = model.split("~")[0];
		if (path === "/v1/responses") {
			// A Responses request whose model names an error behaviour gets that error; any other model gets the
			// fixed reasoning response below.
			const failing = Object.hasOwn(behaviours, name) ? behaviours[name]() : undefined;
			if (failing && failing.status !== 200) {
				return send(failing.status, failing.headers, failing.body);
			}
			let body = {};
			try {
				body = JSON.parse(Buffer.concat(chunks).toString("utf8"));
			} catch {}
			const r = responsesFor(body, (k) => { hits[k] = (hits[k] || 0) + 1; });
			return send(r.status, r.headers, r.body);
		}
		const behaviour = name === "azure-filtered" ? azureFiltered : Object.hasOwn(behaviours, name) ? behaviours[name] : undefined;
		if (!behaviour) {
			return send(404, {}, errorBody(`unknown fixture model ${model}`, "invalid_request_error", "model_not_found"));
		}
		const r = behaviour();
		const answer = () => {
			if (r.cut) {
				if (parsed.stream !== true) {
					return req.socket.destroy();
				}
				const first = streamChunks(JSON.parse(r.body), false).split("\n\n").slice(0, 2).join("\n\n") + "\n\n";
				res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
				res.write(first);
				return setTimeout(() => req.socket.destroy(), 100);
			}
			if (r.idleMs && parsed.stream === true) {
				// Two writes with a pause between them longer than the gateway's heartbeat interval; the
				// whole body is kept under /__sent so a passthrough case can compare it byte for byte.
				const events = streamChunks(JSON.parse(r.body), false).split("\n\n").filter(Boolean).map((e) => e + "\n\n");
				const head = events[0];
				const tail = events.slice(1).join("");
				sentBodies[model] = head + tail;
				res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
				res.write(head);
				return setTimeout(() => res.end(tail), r.idleMs);
			}
			if (parsed.stream === true && r.status === 200 && path !== "/v1/responses") {
				const completion = JSON.parse(r.body);
				res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", ...r.headers });
				return res.end(streamChunks(completion, !!(parsed.stream_options && parsed.stream_options.include_usage)));
			}
			send(r.status, r.headers, r.body);
		};
		if (r.delayMs) {
			return setTimeout(answer, r.delayMs);
		}
		answer();
	});
};
const server = http.createServer(handleRequest);

const stallProxy = net.createServer((socket) => {
	let seen = "";
	socket.on("data", (c) => {
		seen += c.toString("latin1");
		if (seen.includes("\r\n\r\n")) {
			if (seen.startsWith("CONNECT ")) hits.__stall_proxy_connects = (hits.__stall_proxy_connects || 0) + 1;
			seen = "";
		}
	});
	socket.on("error", () => {});
	setTimeout(() => socket.destroy(), STALL_HOLD_MS).unref();
});

// throwawayCertificate makes a self-signed certificate for 127.0.0.1 and localhost with openssl, or returns
// null when openssl is missing or fails. The files live in a temporary directory removed right away.
const throwawayCertificate = () => {
	const dir = mkdtempSync(join(tmpdir(), "provider-error-fixture-tls-"));
	try {
		execFileSync("openssl", ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2", "-subj", "/CN=provider-error-fixture",
			"-addext", "subjectAltName=IP:127.0.0.1,DNS:localhost", "-keyout", join(dir, "key.pem"), "-out", join(dir, "cert.pem")], { stdio: "ignore" });
		return { key: readFileSync(join(dir, "key.pem")), cert: readFileSync(join(dir, "cert.pem")) };
	} catch {
		return null;
	} finally {
		rmSync(dir, { recursive: true, force: true });
	}
};

// A gateway keeps connections open between rows; closing an idle one (Node's default after 5s) makes
// the next non-idempotent request fail with "the server closed connection before returning the first
// response byte", which is the fixture's doing, not the gateway's.
server.keepAliveTimeout = 0;

const mediaTunnel = net.createServer((socket) => {
	let head = Buffer.alloc(0);
	const onData = (c) => {
		head = Buffer.concat([head, c]);
		const end = head.indexOf("\r\n\r\n");
		if (end === -1) return;
		socket.off("data", onData);
		const match = /^CONNECT (\S+) HTTP\/1\.[01]\r\n/.exec(head.subarray(0, end + 2).toString("latin1"));
		if (!match) return socket.end("HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\n\r\n");
		countHit(`__media_tunnel_connects@${match[1]}`);
		const upstream = net.connect(port, "127.0.0.1", () => {
			socket.write("HTTP/1.1 200 Connection established\r\n\r\n");
			const rest = head.subarray(end + 4);
			if (rest.length) upstream.write(rest);
			socket.pipe(upstream);
			upstream.pipe(socket);
		});
		upstream.on("error", () => socket.destroy());
		socket.on("close", () => upstream.destroy());
		upstream.on("close", () => socket.destroy());
	};
	socket.on("data", onData);
	socket.on("error", () => {});
});

// The media tunnel listens before the fixture announces itself, so it is up by the "listening" line.
mediaTunnel.listen(mediaTunnelPort, "127.0.0.1", () => {
	console.log(`provider-error-fixture media tunnel on http://127.0.0.1:${mediaTunnelPort}`);
});

stallProxy.listen(stallPort, "127.0.0.1", () => {
	server.listen(port, "127.0.0.1", () => {
		console.log(`provider-error-fixture stall proxy on http://127.0.0.1:${stallPort}`);
		const certificate = throwawayCertificate();
		if (!certificate) {
			console.log("provider-error-fixture: openssl unavailable, no TLS listener (Bedrock rows will fail to connect)");
			console.log(`provider-error-fixture listening on http://127.0.0.1:${port}`);
			return;
		}
		https.createServer(certificate, handleRequest).listen(tlsPort, "127.0.0.1", () => {
			console.log(`provider-error-fixture TLS listener on https://127.0.0.1:${tlsPort}`);
			console.log(`provider-error-fixture listening on http://127.0.0.1:${port}`);
		});
	});
});
