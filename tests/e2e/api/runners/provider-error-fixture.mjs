import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import http from "node:http";
import https from "node:https";
import net from "node:net";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { crc32, gzipSync } from "node:zlib";
import { createS3ObjectFixture } from "./lib/s3-object-fixture.mjs";

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
// The open tunnels. POST /__reset closes them with the hit counts (TXE-7289): a gateway keeps a tunnel it
// opened for one download and reuses it for the next download to the same address, so without this a case
// that resets and then asserts its own CONNECT fails whenever an earlier case downloaded from that address
// (422.15 then 423.26 both fetch 203.0.113.10:80).
const mediaTunnelSockets = new Set();
// The address a fixture task names its file at: TEST-NET-3 (RFC 5737), public to the gateway's SSRF check and
// routed nowhere, so the file is reachable through the media tunnel only.
const MEDIA_PUBLIC_HOST = "203.0.113.10";
// TXE-7329 (folder 520): the output of a Replicate prediction whose hosted model rendered a longer video than
// the create asked for. It is TXE-6774's 12-second ffmpeg MP4, the reader's own test file.
const VIDEODURATION_TESTDATA = join(dirname(fileURLToPath(import.meta.url)), "../../../../framework/videoduration/testdata");
const RENDERED_LONGER_MP4 = readFileSync(join(VIDEODURATION_TESTDATA, "twelve-seconds.mp4"));
// TXE-7334: the same clip with 2 MiB more media in its mdat, past video-settlement.config.json's 1 MB body
// limit, so only a streamed measurement reads it; and a 12-second clip with reordered frames, which the
// reader once read as 14 seconds (twelve-seconds-reordered.mp4, written by TestReorderedFixture).
const inflateMdat = (mp4, extra) => {
	for (let at = 0; at + 8 <= mp4.length; ) {
		const size = mp4.readUInt32BE(at);
		if (mp4.toString("latin1", at + 4, at + 8) === "mdat") {
			const out = Buffer.concat([mp4.subarray(0, at + size), Buffer.alloc(extra), mp4.subarray(at + size)]);
			out.writeUInt32BE(size + extra, at);
			return out;
		}
		at += size;
	}
	throw new Error("no mdat");
};
const RENDERED_VIDEOS = {
	"rendered-longer": RENDERED_LONGER_MP4,
	"rendered-oversized": inflateMdat(RENDERED_LONGER_MP4, 2 << 20),
	"rendered-reordered": readFileSync(join(VIDEODURATION_TESTDATA, "twelve-seconds-reordered.mp4")),
	// TXE-7386: the 12-second clip as an output on a host the caller chose (a model that answers with any
	// URL): the poll names it on localhost, not the provider's configured 127.0.0.1 origin. The settler must
	// never fetch it, and charges it up to the provider's unmeasured bound (TXE-7401), marked incomplete.
	"rendered-foreign": RENDERED_LONGER_MP4,
	// TXE-7401 (folder 582): outputs the settler cannot measure, a page served as video/mp4. One is
	// charged up to the provider's 30-second bound, the other up to an override's.
	"rendered-unreadable": Buffer.from("<html>not a video</html>"),
	"rendered-unreadable-override": Buffer.from("<html>not a video</html>"),
};
// The rendered videos whose output the poll names on another origin (TXE-7386).
const RENDERED_FOREIGN = new Set(["rendered-foreign"]);

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
	// Folder 565 (TXE-7353): answers served on a tier, for the rows that read back the tier's rates. tiers-flex-long
	// is a 250k-token prompt served on flex, past its 200k tier; tiers-priority-audio is an audio prompt served on
	// priority. fixturePricing prices both.
	"tiers-flex-long": () => ({
		status: 200,
		headers: {},
		body: JSON.stringify({
			id: "chatcmpl-tiers-flex-long",
			object: "chat.completion",
			created: 1,
			model: "tiers-flex-long",
			service_tier: "flex",
			choices: [{ index: 0, message: { role: "assistant", content: "hello" }, finish_reason: "stop" }],
			usage: { prompt_tokens: 250000, completion_tokens: 1000, total_tokens: 251000, prompt_tokens_details: { cached_tokens: 50000 } },
		}),
	}),
	// Folder 562 (TXE-7355): long prompts past tiers the fork had none for. tiers-long-512k is a 600k-token
	// prompt (100k of it cached) past the 512k tier; tiers-long-128k-cache is a 150k-token prompt whose cache
	// reads (100k) are past the 128k cache-read tier.
	"tiers-long-512k": () => ({
		status: 200,
		headers: {},
		body: JSON.stringify({
			id: "chatcmpl-tiers-long-512k",
			object: "chat.completion",
			created: 1,
			model: "tiers-long-512k",
			choices: [{ index: 0, message: { role: "assistant", content: "hello" }, finish_reason: "stop" }],
			usage: { prompt_tokens: 600000, completion_tokens: 1000, total_tokens: 601000, prompt_tokens_details: { cached_tokens: 100000 } },
		}),
	}),
	"tiers-long-128k-cache": () => ({
		status: 200,
		headers: {},
		body: JSON.stringify({
			id: "chatcmpl-tiers-long-128k-cache",
			object: "chat.completion",
			created: 1,
			model: "tiers-long-128k-cache",
			choices: [{ index: 0, message: { role: "assistant", content: "hello" }, finish_reason: "stop" }],
			usage: { prompt_tokens: 150000, completion_tokens: 0, total_tokens: 150000, prompt_tokens_details: { cached_tokens: 100000 } },
		}),
	}),
	"tiers-priority-audio": () => ({
		status: 200,
		headers: {},
		body: JSON.stringify({
			id: "chatcmpl-tiers-priority-audio",
			object: "chat.completion",
			created: 1,
			model: "tiers-priority-audio",
			service_tier: "priority",
			choices: [{ index: 0, message: { role: "assistant", content: "hello" }, finish_reason: "stop" }],
			usage: { prompt_tokens: 1000, completion_tokens: 100, total_tokens: 1100, prompt_tokens_details: { audio_tokens: 400, text_tokens: 600 } },
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
// Answers past a 1 MB response limit (folder 513, TXE-7148). upstream-oversize is a 2 MiB chat completion;
// upstream-oversize-gzip-500 is a 500 whose gzip body is a few KiB on the wire and 2 MiB once decompressed.
const OVERSIZE_BYTES = 2 * 1024 * 1024;
behaviours["upstream-oversize"] = () => {
	const r = behaviours["upstream-ok"]();
	const completion = JSON.parse(r.body);
	completion.id = "chatcmpl-upstream-oversize";
	completion.model = "upstream-oversize";
	completion.choices[0].message.content = "x".repeat(OVERSIZE_BYTES);
	return { ...r, body: JSON.stringify(completion) };
};
behaviours["upstream-oversize-gzip-500"] = () => ({
	status: 500,
	headers: { "Content-Encoding": "gzip" },
	body: gzipSync(errorBody("x".repeat(OVERSIZE_BYTES), "server_error", "internal_error")),
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
	// Folder 565 (TXE-7353): a 300k-token decision, 100k of it read from cache, past the 272k decision tier.
	if (body.model === "gpt-6-luna-long") {
		return { model: "gpt-6-luna-long", answers: [{ type: "predicate", name: "is_refund_request", probability: 0.97 }], usage: { input_tokens: 300000, input_tokens_details: { cached_tokens: 100000 }, output_tokens: 10, total_tokens: 300010 } };
	}
	return undefined;
};
const openRouterDecisionAnswer = () => ({ model: "typesafe/jev-1.13.0", answers: {}, usage: { input_tokens: 1000, output_tokens: 10, cost: 0.25 } });

// The rows the per-call pricing folder is judged against. Every other fixture model stays unpriced.
const turnRates = { mode: "chat", input_cost_per_token: 0.000001, output_cost_per_token: 0.000002, input_cost_per_token_above_272k_tokens: 0.00001, output_cost_per_token_above_272k_tokens: 0.00002, input_cost_per_token_priority: 0.000002, output_cost_per_token_priority: 0.000004 };
const fixturePricing = {
	"turns-priced": { provider: "openai_turns", ...turnRates },
	// Folder 581 (TXE-7365): a Bedrock chat model's token rates, and bedrock/guardrails exactly as the upstream
	// datasheet publishes it, one map of Bedrock guardrail unit to price, which the gateway's sync spreads into
	// per-unit rates.
	"bedrock/amazon.nova-lite-v1:0": { provider: "bedrock", mode: "chat", input_cost_per_token: 0.00000006, output_cost_per_token: 0.00000024 },
	"bedrock/guardrails": {
		provider: "bedrock", mode: "guardrail", base_model: "guardrails",
		guardrail_cost_per_unit: { automatedReasoningPolicyUnits: 0.00017, contentPolicyImageUnits: 0.00075, contentPolicyUnits: 0.00015, contextualGroundingPolicyUnits: 0.0001, sensitiveInformationPolicyFreeUnits: 0, sensitiveInformationPolicyUnits: 0.0001, topicPolicyUnits: 0.00015, wordPolicyUnits: 0 },
	},
	// Folder 520 (TXE-7329): $0.10 a second, so the 12 rendered seconds settle at $1.20 and the 5 asked for at $0.50.
	// The sheet's key drops its first segment, as for every provider-prefixed key, leaving the Replicate model name.
	"replicate/fixture/rendered-longer": { provider: "replicate", mode: "video_generation", output_cost_per_video_per_second: 0.1 },
	"replicate/fixture/rendered-oversized": { provider: "replicate", mode: "video_generation", output_cost_per_video_per_second: 0.1 },
	"replicate/fixture/rendered-reordered": { provider: "replicate", mode: "video_generation", output_cost_per_video_per_second: 0.1 },
	"replicate/fixture/rendered-foreign": { provider: "replicate", mode: "video_generation", output_cost_per_video_per_second: 0.1 },
	"replicate/fixture/rendered-unreadable": { provider: "replicate", mode: "video_generation", output_cost_per_video_per_second: 0.1 },
	"replicate/fixture/rendered-unreadable-override": { provider: "replicate", mode: "video_generation", output_cost_per_video_per_second: 0.1 },
	"turns-tiered": { provider: "openai_turns", ...turnRates },
	"turns-fail": { provider: "openai_turns", ...turnRates },
	"gpt-6-luna": { provider: "openai", mode: "chat", input_cost_per_token: 0.000001, output_cost_per_token: 0.000002, input_cost_per_token_decisions: 0.000003, output_cost_per_token_decisions: 0.000004, supports_decisions: true },
	// Folder 565 (TXE-7353): the live datasheet's gpt-6-luna decision rates above 272k and its decision cache rates,
	// gemini-3.1-pro-preview's flex rates above 200k, and gemini-2.5-flash's priority audio rate.
	"gpt-6-luna-long": {
		provider: "openai", mode: "chat", supports_decisions: true,
		input_cost_per_token: 1e-7, output_cost_per_token: 5e-7,
		input_cost_per_token_decisions: 1e-7, output_cost_per_token_decisions: 0,
		input_cost_per_token_above_272k_tokens_decisions: 2e-7, output_cost_per_token_above_272k_tokens_decisions: 0,
		cache_read_input_token_cost_decisions: 1e-8, cache_read_input_token_cost_above_272k_tokens_decisions: 2e-8,
	},
	"tiers-flex-long": {
		provider: "openai", mode: "chat",
		input_cost_per_token: 2e-6, output_cost_per_token: 1.2e-5,
		input_cost_per_token_flex: 1e-6, output_cost_per_token_flex: 6e-6, cache_read_input_token_cost_flex: 2e-7,
		input_cost_per_token_above_200k_tokens_flex: 2e-6, output_cost_per_token_above_200k_tokens_flex: 9e-6, cache_read_input_token_cost_above_200k_tokens_flex: 4e-7,
	},
	// Folder 562 (TXE-7355): minimax/MiniMax-M3's 512k tier and byteplus/seed-2-0-pro-260328's 128k cache-read
	// tier, as the live datasheet publishes them.
	"tiers-long-512k": {
		provider: "openai", mode: "chat",
		input_cost_per_token: 3e-7, output_cost_per_token: 1.2e-6, cache_read_input_token_cost: 6e-8,
		input_cost_per_token_above_512k_tokens: 6e-7, output_cost_per_token_above_512k_tokens: 2.4e-6, cache_read_input_token_cost_above_512k_tokens: 1.2e-7,
	},
	"tiers-long-128k-cache": {
		provider: "openai", mode: "chat",
		input_cost_per_token: 5e-7, output_cost_per_token: 4e-6, input_cost_per_token_above_128k_tokens: 1e-6,
		cache_read_input_token_cost: 1e-7, cache_read_input_token_cost_above_128k_tokens: 2e-7,
	},
	"tiers-priority-audio": {
		provider: "openai", mode: "chat",
		input_cost_per_token: 3e-7, output_cost_per_token: 2.5e-6, input_cost_per_token_priority: 5.4e-7, output_cost_per_token_priority: 4.5e-6,
		input_cost_per_audio_token: 1e-6, input_cost_per_audio_token_priority: 1.8e-6,
	},
	"typesafe/jev-1.13": { provider: "openrouter", mode: "chat", input_cost_per_token: 0.000001, output_cost_per_token: 0.000002 },
	"decision-emulated": { provider: "openai", mode: "chat", input_cost_per_token: 0.000001, output_cost_per_token: 0.000002, cache_read_input_token_cost: 0.0000001 },
	"pp-custom-legacy": { provider: "openai", mode: "responses", input_cost_per_token: 0.000001, output_cost_per_token: 0.000002 },
	// TXE-6526: a token-priced Gemini image model, for the row that reads back the cost of a response whose usage
	// carries thinking tokens and no modality breakdown.
	"fixture-image-thoughts-only": { provider: "gemini", mode: "image_generation", input_cost_per_token: 0.000001, output_cost_per_token: 0.00003 },
	// A per-image-priced Gemini image model whose answer is a safety block: nothing delivered, nothing charged per image.
	"fixture-image-safety": { provider: "gemini", mode: "image_generation", input_cost_per_token: 0.000001, output_cost_per_image: 0.04 },
	// TXE-7372: gemini/gemini-2.5-flash-image's live image token rates, standard and priority, for folder 566.
	"fixture-image-priority": { provider: "gemini", mode: "image_generation", input_cost_per_token: 3e-7, output_cost_per_image_token: 3e-5, output_cost_per_image_token_priority: 5.4e-5 },
	"fixture-image-standard-tier": { provider: "gemini", mode: "image_generation", input_cost_per_token: 3e-7, output_cost_per_image_token: 3e-5, output_cost_per_image_token_priority: 5.4e-5 },
	// A provider the datasheet knows and list-models cannot enumerate, for run-bounds-wire.mjs.
	"fireworks_ai/accounts/fixture/models/known": { provider: "fireworks_ai", mode: "chat", input_cost_per_token: 0.000001, output_cost_per_token: 0.000002 },
	// TXE-7297: a per-second Kling video rate, so folder 519 can read the price a settled job got from the 5.1 second
	// clip the Kling query reports. Nothing settles a video unless a row turns provider job accounting on.
	// TXE-7357: a pro-mode rate too, so folder 571 can read that a job created with mode "pro" settles at it.
	// A job that names no mode (folder 519) keeps the unbanded 0.1.
	"kling-3.0": { provider: "kling", mode: "video_generation", output_cost_per_video_per_second: 0.1, output_cost_per_video_per_second_pro: 0.224 },
	// TXE-7306: a 720p rate with and without generated audio, so folder 549 can read which one a
	// settled job got. The figures are Model Studio's for wan2.6-i2v-flash 720P (0.025 silent, 0.05
	// with audio); the unbanded 0.1 shows a job that matched no band.
	"wan2.5-t2v-preview": {
		provider: "dashscope", mode: "video_generation",
		output_cost_per_video_per_second: 0.1,
		output_cost_per_video_per_second_720p: 0.025,
		output_cost_per_video_per_second_720p_audio: 0.05,
	},
	// TXE-7349: MiniMax-H3's live band rates (getbifrost.ai/datasheet, 14 Nov 2026), $0.08/s unbanded and at 768P,
	// $0.13/s at 2K, under upstream's output_cost_per_second_<band> keys. Folder 521 settles a 2K job at the 2K rate.
	// TXE-7351: the row also carries MiniMax-H3's live input_cost_per_video_per_second, 0.08, which was billed on the
	// output length (6 s x 0.08 more, $1.26). Folder 521 still expects $0.78, so it fails if that charge comes back.
	"fixture-minimax-2k": { provider: "minimax", mode: "video_generation", input_cost_per_video_per_second: 0.08, output_cost_per_second: 0.08, output_cost_per_second_768p: 0.08, output_cost_per_second_2k: 0.13 },
	// TXE-7409: MiniMax-H3's live row with its input-image price, $0.04 per image beyond the first five, whose free
	// allowance upstream publishes only under provider_specific_entry. Folder 598 settles a job with eight reference images.
	"fixture-minimax-images": {
		provider: "minimax",
		mode: "video_generation",
		input_cost_per_image: 0.04,
		input_cost_per_video_per_second: 0.08,
		output_cost_per_second: 0.08,
		output_cost_per_second_768p: 0.08,
		output_cost_per_second_2k: 0.13,
		provider_specific_entry: { minimax_free_input_images: 5 },
	},
	// TXE-7434: grok-imagine-video-1.5's live rates ($0.08/s unbanded, $0.01 per input image) under xai_media_proxy, a
	// custom provider on an xAI base. Folder 600 sends xAI's own first frame, "image", which only xAI's converter
	// sends; the capture judged it by the custom name and billed no image.
	"fixture-xai-images": { provider: "xai_media_proxy", mode: "video_generation", input_cost_per_image: 0.01, output_cost_per_second: 0.08 },
	// TXE-7356: dall-e-3's live upstream row, which prices an image only by its joint size and quality rate, so folder
	// 570 can read the price of an hd and a standard image. The fork dropped these keys at sync and billed $0.
	"fixture-dalle-3": {
		provider: "openai",
		mode: "image_generation",
		output_cost_per_image_above_1024_and_1024_pixels_hd_quality: 0.088,
		output_cost_per_image_above_1024_and_1792_pixels_hd_quality: 0.132,
		output_cost_per_image_above_1792_and_1024_pixels_hd_quality: 0.132,
		output_cost_per_image_above_1024_and_1024_pixels_standard_quality: 0.044,
		output_cost_per_image_above_1024_and_1792_pixels_standard_quality: 0.088,
		output_cost_per_image_above_1792_and_1024_pixels_standard_quality: 0.088,
	},
	// TXE-7356: Replicate image models priced by resolution tier (replicate/google/nano-banana-2's live rates) and by
	// inference steps (Bedrock SDXL 1.0's), on the fixture's poll-origin and stream-origin predictions, so folder 570
	// can read what a tier and a step count were billed at.
	"replicate/fixture/poll-origin": {
		provider: "replicate",
		mode: "image_generation",
		output_cost_per_image: 0.067,
		output_cost_per_image_1K: 0.067,
		output_cost_per_image_2K: 0.101,
		output_cost_per_image_4K: 0.151,
	},
	"replicate/fixture/stream-origin": {
		provider: "replicate",
		mode: "image_generation",
		output_cost_per_image_above_1024_and_1024_pixels_50_steps: 0.04,
		output_cost_per_image_above_1024_and_1024_pixels_max_steps: 0.08,
	},
	// The same tier rates for an edit, which prices from an image_edit row.
	"replicate/fixture/poll-public": {
		provider: "replicate",
		mode: "image_edit",
		output_cost_per_image: 0.067,
		output_cost_per_image_1K: 0.067,
		output_cost_per_image_2K: 0.101,
		output_cost_per_image_4K: 0.151,
	},
};

// TXE-7349: the fixture-minimax-2k task (folder 521) finishes as a 6 second 2K clip and names no ratio, so the
// response carries no size and the job is priced from the resolution tier the request named.
const MINIMAX_BAND_TASK = "fixture-band-2k";
// TXE-7409: the fixture-minimax-images task (folder 598) finishes the same way, a 6 second 2K clip.
const MINIMAX_IMAGES_TASK = "fixture-minimax-images-task";

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
// Cohere /v2/embed, folder 475 (TXE-7082). Cohere reports what it bills under meta.billed_units and sends no usage
// object, so usage.prompt_tokens on the client can only come from Bifrost's conversion. Model embed-large-response
// answers with a vector long enough to put the body above 10 MB, the default large-payload threshold.
const COHERE_LARGE_RESPONSE_FLOATS = 2200000;
const cohereEmbedAnswer = (body) => {
	const floats = body.model === "embed-large-response" ? COHERE_LARGE_RESPONSE_FLOATS : 2;
	return `{"id":"emb_fixture","embeddings":{"float":[[${new Array(floats).fill("0.125").join(",")}]]},"texts":[],"meta":{"api_version":{"version":"2"},"billed_units":{"input_tokens":7}}}`;
};
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
	// fixture-dalle-3 answers as dall-e-3 does: images and a revised prompt, with no usage and no size or quality, so
	// the gateway prices it from the size and quality the request asked for (folder 570, TXE-7356).
	if (model === "fixture-dalle-3") {
		return { status: 200, body: JSON.stringify({ created: 1, data: [{ b64_json: "iVBORw0KGgo=", revised_prompt: "a lighthouse at dusk" }] }) };
	}
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
	// TXE-7372: one delivered image served on the priority tier, and the same answer on the standard tier. The Gemini
	// API reports the served tier in usageMetadata.serviceTier.
	"fixture-image-priority": { candidates: [{ content: { role: "model", parts: [{ inlineData: { mimeType: "image/png", data: "iVBORw0KGgo=" } }] }, finishReason: "STOP", index: 0 }], usageMetadata: { promptTokenCount: 12, candidatesTokenCount: 1290, totalTokenCount: 1302, serviceTier: "priority", promptTokensDetails: [{ modality: "TEXT", tokenCount: 12 }], candidatesTokensDetails: [{ modality: "IMAGE", tokenCount: 1290 }] }, modelVersion: "fixture-image-priority", responseId: "fixture-image-priority" },
	"fixture-image-standard-tier": { candidates: [{ content: { role: "model", parts: [{ inlineData: { mimeType: "image/png", data: "iVBORw0KGgo=" } }] }, finishReason: "STOP", index: 0 }], usageMetadata: { promptTokenCount: 12, candidatesTokenCount: 1290, totalTokenCount: 1302, serviceTier: "standard", promptTokensDetails: [{ modality: "TEXT", tokenCount: 12 }], candidatesTokensDetails: [{ modality: "IMAGE", tokenCount: 1290 }] }, modelVersion: "fixture-image-standard-tier", responseId: "fixture-image-standard-tier" },
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

// DashScope tasks by id, TXE-6432: the output (and usage) of GET /api/v1/tasks/{id} in DashScope's documented
// shapes. Times are UTC+8, as DashScope sends them.
const dashScopeTimes = { submit_time: "2026-11-10 14:18:52.331", scheduled_time: "2026-11-10 14:18:59.290" };
const dashScopeEnded = { ...dashScopeTimes, end_time: "2026-11-10 14:23:39.407" };
const dashScopeTasks = {
	"ds-pending": { output: { task_status: "PENDING", submit_time: dashScopeTimes.submit_time } },
	"ds-running": { output: { task_status: "RUNNING", ...dashScopeTimes } },
	"ds-succeeded": {
		output: { task_status: "SUCCEEDED", ...dashScopeEnded, orig_prompt: "A kitten on a rooftop", video_url: "https://dashscope-result.example.test/fixture/ds-succeeded.mp4?Expires=1762842232&Signature=fixture" },
		usage: { duration: 10, input_video_duration: 0, output_video_duration: 10, video_count: 1, ratio: "16:9", SR: 720 },
	},
	"ds-filtered": { output: { task_status: "FAILED", ...dashScopeEnded, code: "DataInspectionFailed", message: "Output data may contain inappropriate content." } },
	"ds-infringing": { output: { task_status: "FAILED", ...dashScopeEnded, code: "IPInfringementSuspect", message: "Input data is suspected of being involved in IP infringement." } },
	"ds-canceled": { output: { task_status: "CANCELED", ...dashScopeEnded } },
	// Folder 549 (TXE-7306): two finished 5-second 720P clips of wan2.5-t2v-preview, one created with
	// generated audio and one with audio false. The create answers with one or the other by the
	// request's parameters.audio, so their settlements can be told apart.
	"ds-audio-on": {
		output: { task_status: "SUCCEEDED", ...dashScopeEnded, video_url: "https://dashscope-result.example.test/fixture/ds-audio-on.mp4?Expires=1762842232&Signature=fixture" },
		usage: { duration: 5, input_video_duration: 0, output_video_duration: 5, video_count: 1, ratio: "16:9", SR: 720 },
	},
	"ds-audio-off": {
		output: { task_status: "SUCCEEDED", ...dashScopeEnded, video_url: "https://dashscope-result.example.test/fixture/ds-audio-off.mp4?Expires=1762842232&Signature=fixture" },
		usage: { duration: 5, input_video_duration: 0, output_video_duration: 5, video_count: 1, ratio: "16:9", SR: 720 },
	},
	"ds-expired": { output: { task_status: "UNKNOWN" } },
	// The download rows of folder 423. ds-media-gapped names its file at a public-looking address, which the
	// gateway reaches through the media tunnel (dashscope's proxy_config); ds-media-loopback names a file on
	// this fixture's own loopback address, which the gateway must refuse without asking for it.
	"ds-media-gapped": {
		output: { task_status: "SUCCEEDED", ...dashScopeEnded, video_url: `http://${MEDIA_PUBLIC_HOST}/pp/media/gapped.mp4?X-Fixture-Signature=signed` },
		usage: { duration: 5, input_video_duration: 0, output_video_duration: 5, video_count: 1, ratio: "16:9", SR: 720 },
	},
	"ds-media-loopback": {
		output: { task_status: "SUCCEEDED", ...dashScopeEnded, video_url: `http://127.0.0.1:${port}/pp/media/internal.mp4?X-Fixture-Signature=signed` },
		usage: { duration: 5, input_video_duration: 0, output_video_duration: 5, video_count: 1, ratio: "16:9", SR: 720 },
	},
};
// DashScope's task list, GET /api/v1/tasks: three tasks over two pages of two, in DashScope's documented page
// shape (times in Unix milliseconds). page_no=2 is the last page; any other request is the first.
const dashScopeListed = [
	{ api_key_id: "1", caller_parent_id: "fixture", caller_uid: "fixture", gmt_create: 1762755532331, start_time: 1762755539290, end_time: 1762755819407, region: "ap-southeast-1", request_id: "fixture-dashscope-create", status: "SUCCEEDED", task_id: "ds-succeeded", user_api_unique_key: "apikey:v1:video:generation:wan2.6-t2v", model_name: "wan2.6-t2v" },
	{ api_key_id: "1", caller_parent_id: "fixture", caller_uid: "fixture", gmt_create: 1762755600000, start_time: 1762755601000, region: "ap-southeast-1", request_id: "fixture-dashscope-create", status: "RUNNING", task_id: "ds-running", user_api_unique_key: "apikey:v1:video:generation:wan3.0-video", model_name: "wan3.0-video" },
	{ api_key_id: "1", caller_parent_id: "fixture", caller_uid: "fixture", gmt_create: 1762755700000, start_time: 1762755701000, end_time: 1762755710000, region: "ap-southeast-1", request_id: "fixture-dashscope-create", status: "CANCELED", task_id: "ds-canceled", user_api_unique_key: "apikey:v1:video:generation:wan2.7-t2v", model_name: "wan2.7-t2v" },
];
const dashScopeListAnswer = (query) => {
	const last = new URLSearchParams(query).get("page_no") === "2";
	return { request_id: "fixture-dashscope-list", data: last ? dashScopeListed.slice(2) : dashScopeListed.slice(0, 2), total: 3, total_page: 2, page_no: last ? 2 : 1, page_size: 2 };
};

const dashScopeAnswer = (method, path, query, body) => {
	if (method === "GET" && path === "/api/v1/tasks") return { status: 200, body: dashScopeListAnswer(query) };
	if (method === "POST" && path === "/api/v1/services/aigc/video-generation/video-synthesis") {
		// Folder 549: a wan2.5-t2v-preview create gets the finished task matching its audio flag.
		if (body?.model === "wan2.5-t2v-preview") {
			const taskID = body?.parameters?.audio === false ? "ds-audio-off" : "ds-audio-on";
			return { status: 200, body: { output: { task_status: "PENDING", task_id: taskID }, request_id: "fixture-dashscope-create" } };
		}
		return { status: 200, body: { output: { task_status: "PENDING", task_id: "ds-pending" }, request_id: "fixture-dashscope-create" } };
	}
	const task = /^\/api\/v1\/tasks\/([^/]+)(\/cancel)?$/.exec(path);
	if (task && method === "POST" && task[2]) {
		if (task[1] === "ds-pending") return { status: 200, body: { request_id: "fixture-dashscope-cancel" } };
		return { status: 400, body: { code: "UnsupportedOperation", message: "Failed to cancel the task, please confirm if the task is in PENDING status.", request_id: "fixture-dashscope-cancel" } };
	}
	if (task && method === "GET" && !task[2] && Object.hasOwn(dashScopeTasks, task[1])) {
		const known = dashScopeTasks[task[1]];
		return { status: 200, body: { request_id: "fixture-dashscope-query", ...known, output: { task_id: task[1], ...known.output } } };
	}
	return { status: 404, body: { code: "NotFound", message: "the fixture has no DashScope answer here", request_id: "fixture-dashscope" } };
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
// FLUX 3 Video, TXE-6617 (folder 422): a submit to /pp/bfl/v1/flux-3-video gets its own task, bfl-video-ready, whose
// result is an MP4 link.
const bflVideoSubmitAnswer = () => ({ id: "bfl-video-ready", polling_url: `${BFL_FIXTURE_BASE}/v1/get_result?id=bfl-video-ready`, cost: 30 });
const bflSubmitAnswer = () => ({ id: "bfl-ready", polling_url: `${BFL_FIXTURE_BASE}/v1/get_result?id=bfl-ready`, cost: 4, input_mp: 0, output_mp: 1.05 });
const bflResult = (id) => {
	switch (id) {
	case "bfl-ready":
		return { id, status: "Ready", result: { sample: "https://delivery.example.test/bfl/sample.jpeg", end_time: 1762700000, seed: 7 }, progress: null, details: null, cost: 4 };
	case "bfl-video-ready":
		return { id, status: "Ready", result: { sample: "https://delivery.example.test/bfl/sample.mp4", end_time: 1762700000 }, progress: null, details: null, cost: 30 };
	// The download rows of folder 422: a sample link at a public-looking address, which the gateway reaches
	// through the media tunnel (bfl's proxy_config), and one on this fixture's own loopback address, which the
	// gateway must refuse without asking for it.
	case "bfl-video-media-gapped":
		return { id, status: "Ready", result: { sample: `http://${MEDIA_PUBLIC_HOST}/pp/media/gapped.mp4?X-Fixture-Signature=signed`, end_time: 1762700000 }, progress: null, details: null, cost: 30 };
	case "bfl-video-media-loopback":
		return { id, status: "Ready", result: { sample: `http://127.0.0.1:${port}/pp/media/internal.mp4?X-Fixture-Signature=signed`, end_time: 1762700000 }, progress: null, details: null, cost: 30 };
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
// A request that carries a guardrailConfig (folder 581, TXE-7365) gets Bedrock's guardrail trace, with
// the policy units the guardrail consumed in each assessment's invocationMetrics.usage: 4 content,
// 1 topic and 1 sensitive-information units in all.
const bedrockGuardrailTrace = {
	guardrail: {
		inputAssessment: { "fixture-guardrail": { invocationMetrics: { guardrailProcessingLatency: 120, usage: { contentPolicyUnits: 1, topicPolicyUnits: 1, sensitiveInformationPolicyUnits: 1, wordPolicyUnits: 0 } } } },
		outputAssessments: { "fixture-guardrail": [{ invocationMetrics: { guardrailProcessingLatency: 80, usage: { contentPolicyUnits: 3 } } }] },
	},
};
const bedrockConverseAnswer = (path, body) => ({
	status: 200,
	body: JSON.stringify({
		output: { message: { role: "assistant", content: [{ text: `hello from the bedrock fixture (${decodeURIComponent(path.split("/")[2])})` }] } },
		stopReason: "end_turn",
		usage: { inputTokens: 3, outputTokens: 7, totalTokens: 10 },
		...(body && body.guardrailConfig ? { trace: bedrockGuardrailTrace } : {}),
	}),
});
const isBedrockConversePath = (path) => /^\/model\/[^/]+\/converse$/.test(path);

// Bedrock ConverseStream (TXE-7365, folder 581): the same answer as an AWS EventStream, one binary
// frame per event. A frame is a 12-byte prelude (total length, headers length, CRC32 of those 8
// bytes), string headers (name length, name, type 7, value length, value), the JSON payload and a
// CRC32 of everything before it. The guardrail trace rides on the closing metadata event, as on Bedrock.
const eventStreamFrame = (eventType, payload) => {
	const headers = Buffer.concat(
		[[":message-type", "event"], [":event-type", eventType], [":content-type", "application/json"]].map(([name, value]) => {
			const n = Buffer.from(name);
			const v = Buffer.from(value);
			const len = Buffer.alloc(2);
			len.writeUInt16BE(v.length);
			return Buffer.concat([Buffer.from([n.length]), n, Buffer.from([7]), len, v]);
		}),
	);
	const body = Buffer.from(JSON.stringify(payload));
	const prelude = Buffer.alloc(8);
	prelude.writeUInt32BE(12 + headers.length + body.length + 4, 0);
	prelude.writeUInt32BE(headers.length, 4);
	const preludeCrc = Buffer.alloc(4);
	preludeCrc.writeUInt32BE(crc32(prelude));
	const message = Buffer.concat([prelude, preludeCrc, headers, body]);
	const messageCrc = Buffer.alloc(4);
	messageCrc.writeUInt32BE(crc32(message));
	return Buffer.concat([message, messageCrc]);
};
const bedrockConverseStreamAnswer = (path, body) => ({
	status: 200,
	headers: { "Content-Type": "application/vnd.amazon.eventstream" },
	body: Buffer.concat([
		eventStreamFrame("messageStart", { role: "assistant" }),
		eventStreamFrame("contentBlockDelta", { contentBlockIndex: 0, delta: { text: `hello from the bedrock fixture (${decodeURIComponent(path.split("/")[2])})` } }),
		eventStreamFrame("contentBlockStop", { contentBlockIndex: 0 }),
		eventStreamFrame("messageStop", { stopReason: "end_turn" }),
		eventStreamFrame("metadata", {
			usage: { inputTokens: 3, outputTokens: 7, totalTokens: 10 },
			metrics: { latencyMs: 5 },
			...(body && body.guardrailConfig ? { trace: bedrockGuardrailTrace } : {}),
		}),
	]),
});
const isBedrockConverseStreamPath = (path) => /^\/model\/[^/]+\/converse-stream$/.test(path);

// BytePlus Seedance video tasks, TXE-6426 (contract C7), served on /pp/byteplus/api/v3/contents/generations/tasks.
// A create is answered by the route above wireAnswer already has (task id cgt-fixture-video-task) and is kept in /__wire. A query answers the task whose
// state its id names (cgt-fixture-queued, -running, -succeeded, -failed, -cancelled, -expired), in the shape
// core/providers/byteplus/videos_test.go pins, and DELETE answers 200 with no body, as Ark does. Every query and
// DELETE is counted as "seedance.retrieve <id>" or "seedance.delete <id>", so a cancel row can prove that a
// queued task was queried once and deleted once, and that a finished one was never deleted.
const SEEDANCE_TASKS = "/pp/byteplus/api/v3/contents/generations/tasks";

// VolcEngine's task DELETE, TXE-7366, served on /pp/volcengine/contents/generations/tasks (folder 590). Ark documents a
// successful delete as 200 with an empty object (docs.volcengine.com/docs/82379/1521720), and a stand-in may answer 200
// with no body: cgt-volc-empty-object gets {}, any other id gets no body. Each is counted as "volcengine.delete <id>".
const VOLCENGINE_TASKS = "/pp/volcengine/contents/generations/tasks";
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
// A batch created with metadata.fixture_output_dir retrieves as completed, naming that directory as its output
// (folder 498, TXE-7259), as a Vertex or Bedrock retrieve names the directory its results were written to.
const storageBatchOutputs = new Map();
const STORAGE_BATCH_MADE_ELSEWHERE = "batch_storage_made_elsewhere";
const storageBatchObject = (id) => {
	const batch = {
		id, object: "batch", endpoint: "/v1/chat/completions", input_file_id: "gs://bifrost-fixture-bucket/uploads/input.jsonl", completion_window: "24h",
		status: "validating", created_at: 1, request_counts: { total: 1, completed: 0, failed: 0 },
	};
	if (storageBatchOutputs.has(id)) Object.assign(batch, { status: "completed", output_file_id: storageBatchOutputs.get(id), request_counts: { total: 1, completed: 1, failed: 0 } });
	return batch;
};
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
		const outputDir = body && body.metadata && body.metadata.fixture_output_dir;
		if (typeof outputDir === "string" && outputDir) storageBatchOutputs.set(id, outputDir);
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

// xAI's video, text to speech and speech to text endpoints (folder 415, TXE-6429), under the xai provider's
// base /pp/xai, so every call is a /__wire row. A create answers a request id; the model xai-video-policy
// is refused as a usage-policy block under a 403, and xai-video-drop is read in full and then the socket is
// destroyed without a response byte: the failing upstream of a task create that must be written only once.
const xaiVideoCreateAnswer = (body) => {
	if (body.model === "xai-video-drop") return { drop: true };
	if (body.model === "xai-video-policy") return { status: 403, body: JSON.stringify({ code: "content_policy_violation", error: "The prompt violates the usage policy." }) };
	// TXE-7434 (folder 600): a create that sent xAI's own first frame is answered with a task that reports no cost.
	if (body.model === "fixture-xai-images" && body.image && body.image.url) return { status: 200, body: JSON.stringify({ request_id: "xai-fixture-images" }) };
	return { status: 200, body: JSON.stringify({ request_id: "xai-fixture-done" }) };
};
// GET /pp/xai/v1/videos/{request_id}: xai-fixture-pending is still generating, xai-fixture-moderated finished
// with the video withheld (respect_moderation false, no link), anything else finished with a link and usage.
// The link is under a reserved name: nothing fetches it.
const xaiVideoTask = (id) => {
	if (id === "xai-fixture-pending") return { status: "pending", progress: 42, model: "grok-imagine-video-1.5" };
	// TXE-7434 (folder 600): finished, 6 s, with no usage, so settlement prices it from the fixture-xai-images row.
	if (id === "xai-fixture-images") {
		return { status: "done", progress: 100, model: "fixture-xai-images", video: { url: "https://vidgen.fixture.invalid/xai-video-images.mp4", duration: 6, respect_moderation: true } };
	}
	const usage = { cost_in_usd_ticks: 3000000000, input_tokens: 12, output_tokens: 4800, total_tokens: 4812 };
	if (id === "xai-fixture-moderated") {
		return { status: "done", progress: 100, model: "grok-imagine-video-1.5", video: { url: "", duration: 6, respect_moderation: false }, usage };
	}
	// xai-fixture-download names a link the fixture itself serves, as a forward proxy (see XAI_MEDIA_HOST).
	const url = id === "xai-fixture-download" ? `http://${XAI_MEDIA_HOST}/xai-video-${id}.mp4` : `https://vidgen.fixture.invalid/xai-video-${id}.mp4`;
	return { status: "done", progress: 100, model: "grok-imagine-video-1.5", video: { url, duration: 6, respect_moderation: true }, usage };
};
// The host of a downloadable xAI video link. The gateway's media fetcher refuses loopback, so a link it can
// fetch has to name a public address; this one is in TEST-NET-3 (RFC 5737), which is never routed. The
// provider xai_media_proxy sends its traffic through this fixture as an HTTP forward proxy, so the fetch of
// http://203.0.113.10/... arrives here as an absolute-URI request and is answered with FIXTURE_MP4.
const XAI_MEDIA_HOST = "203.0.113.10";
// POST /pp/xai/v1/tts: audio bytes, or the JSON envelope (base64 audio, duration, character timings) when
// with_timestamps is true. The bytes start with an ID3 tag, as an MP3 file does.
const XAI_FIXTURE_AUDIO = Buffer.from("ID3\u0004 FIXTURE-MP3 bytes served by provider-error-fixture");
const xaiSpeechAnswer = (body) => {
	if (body.with_timestamps === true) {
		return {
			status: 200,
			body: JSON.stringify({
				audio: XAI_FIXTURE_AUDIO.toString("base64"), content_type: "audio/mpeg", duration: 2.6,
				audio_timestamps: { graph_chars: ["H", "i"], graph_times: [{ start: 0, end: 0.06 }, { start: 0.06, end: 0.12 }] },
			}),
		};
	}
	return { status: 200, headers: { "Content-Type": "audio/mpeg" }, body: XAI_FIXTURE_AUDIO };
};
// POST /pp/xai/v1/stt: the transcript of xAI's reference, with a speaker per word and one channel.
const xaiTranscript = {
	text: "The balance is $167,983.15.", language: "en", duration: 8.4,
	words: [
		{ text: "The", start: 0, end: 0.24, confidence: 0.33, speaker: 0 },
		{ text: "balance", start: 0.24, end: 0.64, confidence: 0.67, speaker: 1 },
	],
	channels: [{ index: 0, language: "en", text: "The balance is $167,983.15." }],
};

// DashScope's synchronous image endpoint, TXE-6607. The model in the body picks the answer; bodies follow the samples
// of Alibaba's Qwen-Image and Z-Image API references. qwen-image-2.0 returns as many links as parameters.n asks for.
const dashScopeImagePath = "/pp/dashscope/api/v1/services/aigc/multimodal-generation/generation";
const dashScopeImageAnswer = (body) => {
	const link = (i) => ({ image: `https://dashscope-result-sh.oss-cn-shanghai.aliyuncs.com/fixture/${i}.png?Expires=1` });
	const ok = (content, usage) => ({ status: 200, body: JSON.stringify({ output: { choices: [{ finish_reason: "stop", message: { role: "assistant", content } }] }, usage, request_id: "fixture-dashscope-image" }) });
	switch (body.model) {
		case "z-image-turbo":
			return ok([link(1), { text: "a lighthouse at dusk, warm light, 35mm" }, { reasoning_content: "the user wants a lighthouse" }],
				{ width: 1024, height: 1536, image_count: 1, input_tokens: 120, output_tokens: 310, total_tokens: 430, output_tokens_details: { reasoning_tokens: 250 } });
		case "qwen-image-moderated":
			return { status: 400, body: JSON.stringify({ request_id: "fixture-dashscope-moderated", code: "DataInspectionFailed", message: "Input data may contain inappropriate content." }) };
		case "qwen-image-empty":
			return { status: 200, body: JSON.stringify({ output: { choices: [] }, request_id: "fixture-dashscope-empty" }) };
		default: {
			const n = (body.parameters && body.parameters.n) || 1;
			return ok(Array.from({ length: n }, (_, i) => link(i + 1)), { width: 1536, height: 1024, image_count: n });
		}
	}
};

// xAI chat and Responses under the xai provider's base /pp/xai (folder 443, TXE-7043). The chat answer
// reports the per-tool call counts of tools xAI ran itself; a streamed one sends them once, on the
// trailing usage frame. The Responses answer is a finished response: what matters there is the request
// body the fixture recorded.
const xaiToolUsage = {
	prompt_tokens: 220, completion_tokens: 56, total_tokens: 276, cost_in_usd_ticks: 5584000,
	server_side_tool_usage_details: { web_search_calls: 2, x_search_calls: 1, code_interpreter_calls: 0, file_search_calls: 0, mcp_calls: 0, document_search_calls: 0 },
};
const xaiChatAnswer = (body) => {
	const base = { id: "chatcmpl-xai-fixture", created: 1, model: body.model };
	if (body.stream === true) {
		const chunk = (extra) => `data: ${JSON.stringify({ ...base, object: "chat.completion.chunk", ...extra })}\n\n`;
		return {
			status: 200,
			stream: chunk({ choices: [{ index: 0, delta: { role: "assistant", content: "answer" } }] })
				+ chunk({ choices: [{ index: 0, delta: {}, finish_reason: "stop" }] })
				+ chunk({ choices: [], usage: xaiToolUsage })
				+ "data: [DONE]\n\n",
		};
	}
	return { status: 200, body: JSON.stringify({ ...base, object: "chat.completion", choices: [{ index: 0, message: { role: "assistant", content: "answer" }, finish_reason: "stop" }], usage: xaiToolUsage }) };
};
const xaiResponsesAnswer = (body) => {
	const response = { id: "resp_xai_fixture", object: "response", created_at: 1, model: body.model, status: "completed", output: [], usage: { input_tokens: 1, output_tokens: 1, total_tokens: 2 } };
	if (body.stream === true) {
		return { status: 200, stream: `event: response.completed\ndata: ${JSON.stringify({ type: "response.completed", sequence_number: 1, response })}\n\n` };
	}
	return { status: 200, body: JSON.stringify(response) };
};

// BytePlus Seedream image results, TXE-6605 (contract C6), served on /pp/byteplus/api/v3/images/generations. Ark
// answers a sequential generation with a 200 whose data[] mixes delivered images with per-image error objects; the
// codes and shape are the ones core/providers/byteplus/byteplus_test.go pins. The model selects the answer:
// seedream-partly-filtered delivers two images and refuses one, seedream-all-filtered refuses its only image, and
// seedream-empty delivers nothing and refuses nothing. seedream-internal-error, seedream-top-refusal and
// seedream-top-invalid-param are 200s carrying error objects: InternalServiceError as the only image, and a top-level error
// object (a moderation code, a parameter code) in place of data. seedream-moderated and seedream-invalid-param are refused over
// HTTP instead: a 400 carrying Ark's error object, a moderation code and a parameter code, on every request including a
// stream, since Ark refuses before it sends a frame. A streamed request for the other models gets the result as Ark's
// image_generation events (seedreamStream).
const seedreamRefusal = { error: { code: "OutputImageSensitiveContentDetected", message: "The request failed because the output image may contain sensitive information." } };
const seedreamImage = (i) => ({ url: `https://fixture.invalid/seedream/${i}.png`, size: "2048x2048" });
const seedreamUsage = (n) => ({ generated_images: n, output_tokens: 16384 * n, total_tokens: 16384 * n });
const seedreamAnswers = {
	"seedream-partly-filtered": () => ({ data: [seedreamImage(0), seedreamRefusal, seedreamImage(2)], usage: seedreamUsage(2) }),
	"seedream-all-filtered": () => ({ data: [seedreamRefusal], usage: seedreamUsage(0) }),
	"seedream-empty": () => ({ data: [], usage: seedreamUsage(0) }),
	// Error objects inside a 200: Ark's own failure on the only image, and a whole-request error object in place of data.
	"seedream-internal-error": () => ({ data: [{ error: { code: "InternalServiceError", message: "The service encountered an unexpected internal error." } }], usage: seedreamUsage(0) }),
	"seedream-top-refusal": () => ({ error: { code: "InputTextSensitiveContentDetected", message: "The request failed because the input text may contain sensitive information." } }),
	"seedream-top-invalid-param": () => ({ error: { code: "InvalidParameter", message: "The parameter `size` specified in the request is not valid." } }),
};
const seedreamHTTPErrors = {
	"seedream-moderated": { code: "InputTextSensitiveContentDetected", message: "The request failed because the input text may contain sensitive information." },
	"seedream-invalid-param": { code: "InvalidParameter", message: "The parameter `size` specified in the request is not valid." },
};
const seedreamAnswer = (body) => {
	const refused = seedreamHTTPErrors[body.model];
	if (refused) return { status: 400, body: JSON.stringify({ error: { ...refused, param: "", type: "BadRequest" } }) };
	const answer = seedreamAnswers[body.model];
	// Any other model gets one delivered image with Ark's own creation time, which an image link's expiry is
	// counted from (folder 436, TXE-7051).
	if (!answer) return { status: 200, body: JSON.stringify({ model: body.model, created: 1762000000, data: [{ url: "https://delivery.example.test/seedream/1.jpeg?Expires=1", size: "2048x2048" }], usage: { generated_images: 1, output_tokens: 16384, total_tokens: 16384 } }) };
	const result = answer();
	if (body.stream === true) return { status: 200, stream: seedreamStream(body.model, result) };
	return { status: 200, body: JSON.stringify({ model: body.model, created: 1, ...result }) };
};
// A streamed Seedream request gets the same result as Ark streams it: one event per image, image_generation.partial_succeeded
// for a delivered image and image_generation.partial_failed carrying the refusal's error object, then
// image_generation.completed with the usage, then [DONE].
const seedreamStream = (model, result) => {
	// A whole-request error object is Ark's only event on a stream.
	if (result.error) return `data: ${JSON.stringify({ error: result.error })}\n\n`;
	const events = (result.data || []).map((item, i) =>
		item.error
			? { type: "image_generation.partial_failed", model, created: 1, image_index: i, error: item.error }
			: { type: "image_generation.partial_succeeded", model, created: 1, image_index: i, ...item },
	);
	events.push({ type: "image_generation.completed", model, created: 1, usage: result.usage });
	return events.map((e) => `data: ${JSON.stringify(e)}\n\n`).join("") + "data: [DONE]\n\n";
};

const wireAnswer = (path, body, bytes) => {
	if (isBedrockConversePath(path)) return bedrockConverseAnswer(path, body);
	if (isBedrockConverseStreamPath(path)) return bedrockConverseStreamAnswer(path, body);
	if (path.startsWith("/pp/storage/")) return storageAnswer(path, body);
	if (path === "/pp/bfl/v1/flux-3-video") return { status: 200, body: JSON.stringify(bflVideoSubmitAnswer()) };
	if (path.startsWith("/pp/bfl/v1/")) return { status: 200, body: JSON.stringify(bflSubmitAnswer()) };
	// Runway's task create (folder 519, TXE-7297): the answer is the task id only, as Runway's is.
	if (path.startsWith("/pp/runway/v1/") && path.endsWith("_to_video")) return { status: 200, body: JSON.stringify({ id: "runway-fixture-task" }) };
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
	// TXE-6994: /pp/hf is the huggingface provider's base; its chat body has no service_tier field.
	if (path === "/pp/oc/v1/chat/completions" || path === "/pp/oc/chat/completions" || path === "/pp/hf/v1/chat/completions") {
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
	if (path === "/pp/minimax/v2/video_generation" && body.model === "fixture-minimax-2k") return { status: 200, body: JSON.stringify({ task_id: MINIMAX_BAND_TASK, base_resp: { status_code: 0, status_msg: "success" } }) };
	if (path === "/pp/minimax/v2/video_generation" && body.model === "fixture-minimax-images") return { status: 200, body: JSON.stringify({ task_id: MINIMAX_IMAGES_TASK, base_resp: { status_code: 0, status_msg: "success" } }) };
	if (path === "/pp/minimax/v2/video_generation") return { status: 200, body: JSON.stringify({ task_id: "fixture-video-task", base_resp: { status_code: 0, status_msg: "success" } }) };
	if (path === "/pp/byteplus/api/v3/contents/generations/tasks") return { status: 200, body: JSON.stringify({ id: "cgt-fixture-video-task" }) };
	if (path.startsWith("/pp/gemini/models/") && path.endsWith(":predictLongRunning")) return { status: 200, body: JSON.stringify({ name: `${path.slice("/pp/gemini/".length, -":predictLongRunning".length)}/operations/fixture-video-op` }) };
	if (path === "/pp/minimax/v1/chat/completions" && body.model === "sensitive-flags") return miniMaxSensitiveChatAnswer(body);
	if (path === "/pp/minimax/v1/responses") return miniMaxResponsesAnswer(body);
	if (path === "/anthropic/v1/messages") return { status: 200, body: JSON.stringify(anthropicMessage(body.model)) };
	// A delivered MiniMax image, TXE-7051: links by default, bytes when the body asks for base64. MiniMax's image
	// body carries no creation time.
	// Seedream generation and edit share Ark's one endpoint: the named fixture models of folder 312 (TXE-6605), and a
	// delivered image for any other model (TXE-7051).
	if (path === "/pp/byteplus/api/v3/images/generations") return seedreamAnswer(body);
	if (path === "/pp/minimax/v1/image_generation") {
		const data = body.response_format === "base64" ? { image_base64: ["iVBORw0KGgo="] } : { image_urls: ["https://delivery.example.test/minimax/1.png?Expires=1"] };
		return { status: 200, body: JSON.stringify({ id: "fixture-minimax-image", data, metadata: { success_count: "1", failed_count: "0" }, base_resp: { status_code: 0, status_msg: "success" } }) };
	}
	if (path === "/pp/vercel/v1/images/generations") return { status: 200, body: JSON.stringify(vercelImageAnswer(body)) };
	if (path === "/pp/google-cloud-tts/v1/text:synthesize") return cloudTTSAnswer(body);
	// Speech accepted (folder 473, TXE-7077): Sarvam's and ElevenLabs' own answers, so a row can read from /__wire
	// what the converter sent. ElevenLabs answers audio bytes, or JSON on the with-timestamps endpoint.
	if (path === "/pp/sarvam/text-to-speech") return { status: 200, body: JSON.stringify({ request_id: "fixture-sarvam-speech", audios: [cloudTTSAudioContent] }) };
	if (path.startsWith("/pp/elevenlabs/v1/text-to-speech/")) {
		if (path.endsWith("/with-timestamps")) return { status: 200, body: JSON.stringify({ audio_base64: cloudTTSAudioContent, alignment: { characters: ["h", "i"], character_start_times_seconds: [0, 0.1], character_end_times_seconds: [0.1, 0.2] } }) };
		return { status: 200, body: "fixture-elevenlabs-audio" };
	}
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
	if (path === "/pp/xai/v1/chat/completions") return xaiChatAnswer(body);
	if (path === "/pp/xai/v1/responses") return xaiResponsesAnswer(body);
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
	// Folder 520 (TXE-7329, TXE-7334): a prediction of fixture/<name> for each RENDERED_VIDEOS entry, polled on
	// the provider's own origin. Its output is that video, whatever duration the create sent.
	const renderedCreate = /^\/pp\/replicate\/v1\/models\/fixture\/([a-z-]+)\/predictions$/.exec(path);
	if (renderedCreate && RENDERED_VIDEOS[renderedCreate[1]]) {
		const id = `pred_${renderedCreate[1].replace(/-/g, "_")}`;
		return { status: 201, body: JSON.stringify({ id, status: "starting", urls: { get: `http://127.0.0.1:${port}/pp/replicate/v1/predictions/${id}` } }) };
	}
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
	if (path === "/pp/gemini/interactions") return { status: 200, body: JSON.stringify(omniInteraction("omni-running", "")) };
	const omniCancel = path.match(/^\/pp\/gemini\/interactions\/([^/]+)\/cancel$/);
	if (omniCancel) return { status: 200, body: JSON.stringify({ ...omniInteraction(omniCancel[1], ""), status: "cancelled" }) };
	if (path === "/pp/xai/v1/videos/generations") return xaiVideoCreateAnswer(body);
	if (path === "/pp/xai/v1/tts") return xaiSpeechAnswer(body);
	if (path === "/pp/xai/v1/stt") return { status: 200, body: JSON.stringify(xaiTranscript) };
	if (path.endsWith(":predict")) return { status: 200, body: JSON.stringify(imagenPartiallyBlocked) };
	const geminiAudio = geminiAudioAnswer(path);
	if (geminiAudio) return geminiAudio;
	if (path.endsWith(":generateContent")) return { status: 200, body: JSON.stringify(geminiCodeExecution) };
	if (path.endsWith(":streamGenerateContent")) return { status: 200, stream: `data: ${JSON.stringify(geminiCodeExecution)}\n\n` };
	if (path === "/pp/cloudflare/accounts/acct-1/ai/run/@cf/cloudflare/clef-flash") return { status: 200, body: JSON.stringify(clefAnswer) };
	// Gemini :batchEmbedContents answers one vector per requests[] entry (folder 142, TXE-7076).
	if (path.startsWith("/pp/gemini/models/") && path.endsWith(":batchEmbedContents")) return { status: 200, body: JSON.stringify({ embeddings: (Array.isArray(body.requests) ? body.requests : []).map(() => ({ values: [0.1, 0.2] })) }) };
	if (path === "/v1/embeddings" && String(body.model).startsWith("voyage-")) {
        if (body.input.includes("voyage-fixture-fail")) return { status: 500, body: JSON.stringify({ detail: "voyage fixture failure" }) };
        const embedding = body.encoding_format === "base64" ? "AACAPw==" :
            ["int8", "binary"].includes(body.output_dtype) ? [-128, 0, 127] :
            ["uint8", "ubinary"].includes(body.output_dtype) ? [0, 128, 255] :
            [256, 512, 1024, 2048].includes(body.output_dimension) ? Array(body.output_dimension).fill(0.1) : [0.1, 0.2];
        return { status: 200, body: JSON.stringify({ object: "list", data: body.input.map((_, index) => ({ object: "embedding", index, embedding })), usage: body.input.includes("voyage-fixture-no-usage") ? {} : { total_tokens: 7 } }) };
    }
	if (path === "/pp/cohere/v2/embed") return { status: 200, body: cohereEmbedAnswer(body) };
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
// A Gemini Interactions API resource for an Omni video task, folder 418 (TXE-6538). The interaction id picks
// the state: "omni-running" is in_progress and "omni-<status>" is that status ("omni-completed",
// "omni-incomplete", ...). A completed interaction carries a delivery=uri video, a Gemini Files file on this
// fixture (GET /pp/gemini/files/omni-file:download), and usage broken down by modality. POST /pp/gemini/interactions
// answers the running one and POST /pp/gemini/interactions/{id}/cancel a cancelled one; both land in /__wire like
// every POST under /pp/. The GETs are counted under "omni.retrieve <id>" and "omni.file".
const omniInteraction = (id, origin) => {
	const status = id === "omni-running" ? "in_progress" : id.replace(/^omni-/, "");
	const interaction = { id, object: "interaction", status, model: "gemini-omni-1.1-flash", created: "2026-11-10T12:22:47Z", updated: "2026-11-10T12:25:15Z" };
	if (status === "completed") {
		interaction.steps = [
			{ type: "user_input", content: [{ type: "text", text: "a marble on a track" }] },
			{ type: "thought", content: [{ type: "thought", text: "planning the shot" }] },
			{ type: "model_output", content: [{ type: "video", mime_type: "video/mp4", uri: `${origin}/pp/gemini/files/omni-file:download?alt=media` }] },
		];
		interaction.usage = {
			input_tokens_by_modality: [{ modality: "text", tokens: 12 }],
			output_tokens_by_modality: [{ modality: "text", tokens: 7 }, { modality: "video", tokens: 46336 }],
			total_input_tokens: 12, total_output_tokens: 46343, total_tokens: 46355,
		};
	}
	return interaction;
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

// Kling's task object (kling.ai/document-api, "Query Task"), as the fixture answers it. The task id
// decides the state: "893605946402811986" failed on the platform's content risk control, every other id
// succeeded with one video, a duration and a deduction of Units.
const KLING_TASK_ID = "893605946402811985";
const KLING_BLOCKED_TASK_ID = "893605946402811986";
// A create whose prompt says "[pro-mode]" gets its own task id (folder 571, TXE-7357), so its settlement row
// is not confused with folder 519's.
const KLING_PRO_MODE_TASK_ID = "893605946402811987";
const klingTask = (id, externalID) => (id === KLING_BLOCKED_TASK_ID
	? { id, status: "failed", message: "The task failed because it triggered the content risk control of the platform", create_time: 1781080778802, update_time: 1781080794151, external_id: externalID, outputs: [], billing: [] }
	: {
		id, status: "succeeded", message: "", create_time: 1781080778802, update_time: 1781080794151, external_id: externalID,
		outputs: [{ type: "video", id: "vid_fixture_1", url: "https://kling-fixture.example.test/out.mp4", watermark_url: "", duration: "5.1" }],
		billing: [{ charge_type: "unit", amount: "3.5", package_type: "video" }],
	});
const klingEnvelope = (data) => JSON.stringify({ code: 0, message: "SUCCEED", request_id: "kling-fixture-request", data });
// Every request that reached the Kling routes, in order; GET /__kling-calls returns them and POST /__reset
// clears them.
const klingCalls = [];

// Perplexity's native /v1/responses (the Agent API), TXE-6987 (contract C5, C10). Every model outside the Sonar
// family is served there. Its usage is the published ResponsesUsage shape: tool_calls_details.<tool>.invocation,
// and a cost object in US dollars whose total_cost includes the tool calls. agent-usage answers with it; streamed,
// response.completed carries the same usage, after one of Perplexity's own response.reasoning.* events.
const perplexityAgentUsage = {
	input_tokens: 20, output_tokens: 60, total_tokens: 80,
	input_tokens_details: { cache_creation_input_tokens: 0, cache_read_input_tokens: 0 },
	tool_calls_details: { web_search: { invocation: 2, cost_usd: 0.01 }, fetch_url: { invocation: 1, cost_usd: 0.0005 } },
	cost: { currency: "USD", input_cost: 0.00004, output_cost: 0.0006, tool_calls_cost: 0.0105, tool_calls_cost_details: { web_search: 0.01, fetch_url: 0.0005 }, total_cost: 0.01114 },
};
const perplexityAgentResponse = (model, stream) => {
	const response = {
		id: "resp_pplx_agent_usage", object: "response", created_at: 1, status: "completed", model,
		output: [{ id: "msg_pplx_agent_usage", type: "message", role: "assistant", status: "completed", content: [{ type: "output_text", text: "hello", annotations: [] }] }],
		usage: perplexityAgentUsage,
	};
	if (!stream) return { status: 200, body: JSON.stringify(response) };
	const events = [
		{ type: "response.created", sequence_number: 0, response: { ...response, status: "in_progress", output: [], usage: null } },
		{ type: "response.reasoning.search_queries", sequence_number: 1, queries: ["hello"] },
		{ type: "response.output_text.delta", sequence_number: 2, item_id: "msg_pplx_agent_usage", output_index: 0, content_index: 0, delta: "hello" },
		{ type: "response.completed", sequence_number: 3, response },
	];
	return { status: 200, stream: events.map((e) => `event: ${e.type}\ndata: ${JSON.stringify(e)}\n\n`).join("") };
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

// Reads a request body as text before answering.
const collect = (req, then) => {
	const chunks = [];
	req.on("data", (c) => chunks.push(c));
	req.on("end", () => then(Buffer.concat(chunks).toString("utf8")));
};

// Resource ids on an openai-based custom provider (folder 477, TXE-7020). tkp_vidvendor in the fixture config is an
// openai-based custom provider whose request_path_overrides are templates off /v1: {video_id}, {model},
// {container_id} with {file_id}, and {response_id}. This upstream answers only under /pp/vidvendor/api/v3 and only
// for the ids below, so a call on a default /v1 path, or one that lost or misplaced an id, is a 404 and shows in
// GET /__wire.
const VIDVENDOR_ROOT = "/pp/vidvendor/api/v3";
const VIDVENDOR_KEY_NAMES = { "Bearer vv-fixture-key": "tk-key-vidvendor" };
const vidvendorAnswer = (method, path) => {
	const notFound = { status: 404, body: errorBody(`vidvendor serves nothing at ${method} ${path}`, "invalid_request_error", "not_found") };
	if (!path.startsWith(VIDVENDOR_ROOT + "/")) return notFound;
	const rest = path.slice(VIDVENDOR_ROOT.length);
	if (method === "GET" && rest === "/tasks/video_477") {
		return { status: 200, body: JSON.stringify({ id: "video_477", object: "video", model: "glm-video-1", status: "completed", progress: 100, created_at: 1715959000, completed_at: 1715959060, seconds: "4", size: "720x1280" }) };
	}
	if (method === "DELETE" && rest === "/tasks/video_477") {
		return { status: 200, body: JSON.stringify({ id: "video_477", object: "video.deleted", deleted: true }) };
	}
	if (method === "GET" && rest === "/models/glm-video-1") {
		return { status: 200, body: JSON.stringify({ id: "glm-video-1", object: "model", created: 1715959000, owned_by: "vidvendor" }) };
	}
	const video = { id: "video_477", object: "video", model: "glm-video-1", status: "completed", progress: 100, created_at: 1715959000, completed_at: 1715959060, seconds: "4", size: "720x1280" };
	if (method === "GET" && rest === "/tasks/video_477/file") return { status: 200, contentType: "video/mp4", body: "vidvendor video bytes" };
	if (method === "POST" && rest === "/tasks/video_477/remix") return { status: 200, body: JSON.stringify({ ...video, id: "video_478", status: "queued", progress: 0, remixed_from_video_id: "video_477" }) };
	const container = { id: "cntr_477", object: "container", created_at: 1715959000, status: "running", name: "harness" };
	const containerFile = { id: "cfile_477", object: "container.file", created_at: 1715959000, bytes: 5, container_id: "cntr_477", path: "/mnt/data/a.txt", source: "user" };
	if (method === "GET" && rest === "/boxes/cntr_477") return { status: 200, body: JSON.stringify(container) };
	if (method === "DELETE" && rest === "/boxes/cntr_477") return { status: 200, body: JSON.stringify({ id: "cntr_477", object: "container.deleted", deleted: true }) };
	if (method === "POST" && rest === "/boxes/cntr_477/items") return { status: 200, body: JSON.stringify(containerFile) };
	if (method === "GET" && rest === "/boxes/cntr_477/items") return { status: 200, body: JSON.stringify({ object: "list", data: [containerFile], first_id: "cfile_477", last_id: "cfile_477", has_more: false }) };
	// These templates name the file before its container: an id placed by position would swap them.
	if (method === "GET" && rest === "/items/cfile_477/in/cntr_477") return { status: 200, body: JSON.stringify(containerFile) };
	if (method === "GET" && rest === "/items/cfile_477/in/cntr_477/bytes") return { status: 200, contentType: "text/plain", body: "hello" };
	if (method === "DELETE" && rest === "/items/cfile_477/in/cntr_477") return { status: 200, body: JSON.stringify({ id: "cfile_477", object: "container.file.deleted", deleted: true }) };
	const response = { id: "resp_477", object: "response", created_at: 1715959000, status: "completed", model: "glm-video-1", output: [{ id: "msg_477", type: "message", role: "assistant", status: "completed", content: [{ type: "output_text", text: "stored answer", annotations: [] }] }], usage: { input_tokens: 3, output_tokens: 2, total_tokens: 5 } };
	if (method === "GET" && rest === "/answers/resp_477") return { status: 200, body: JSON.stringify(response) };
	// The streamed retrieve has its own template, so it is told apart from the plain one on the wire.
	if (method === "GET" && rest === "/answers/resp_477/events") {
		const events = [{ type: "response.created", sequence_number: 0, response: { ...response, status: "in_progress", output: [] } }, { type: "response.completed", sequence_number: 1, response }];
		return { status: 200, contentType: "text/event-stream", body: events.map((e) => `event: ${e.type}\ndata: ${JSON.stringify(e)}\n\n`).join("") };
	}
	if (method === "DELETE" && rest === "/answers/resp_477") return { status: 200, body: JSON.stringify({ id: "resp_477", object: "response", deleted: true }) };
	if (method === "POST" && rest === "/answers/resp_477/stop") return { status: 200, body: JSON.stringify({ ...response, status: "cancelled", output: [] }) };
	if (method === "GET" && rest === "/answers/resp_477/inputs") return { status: 200, body: JSON.stringify({ object: "list", data: [{ id: "msg_in_477", type: "message", role: "user", content: [{ type: "input_text", text: "question" }] }], first_id: "msg_in_477", last_id: "msg_in_477", has_more: false }) };
	return notFound;
};

// Resource path overrides on every base (folder 485, TXE-7085). tkp_ovr_<base> in the fixture config is a custom
// provider on the anthropic, gemini, modelark, volcengine or replicate base whose request_path_overrides move each
// call that names a batch or a file by id to /moved/<request type>/{batch_id} or /moved/<request type>/{file_id}.
// Every call under /pp/ovr/<base> is kept for GET /__wire with its method, path and the name of the key it carried
// (never the key). Only /pp/ovr/<base>/moved/<request type>/<id> answers, with the shape that base decodes, so a
// call built on the vendor's own path, or one that still carries the literal placeholder, is a 404 that shows in
// the wire.
const OVR_ROOT = "/pp/ovr";
const OVR_BATCH_ID = "ob_485";
const OVR_FILE_ID = "of_485";
const OVR_BASES = ["anthropic", "gemini", "modelark", "volcengine", "replicate"];
const ovrKeyName = (headers) => {
	const sent = [headers.authorization, headers["x-api-key"], headers["x-goog-api-key"]].filter(Boolean).join(" ");
	const base = OVR_BASES.find((b) => sent.includes(`ovr-fixture-key-${b}`));
	return base ? `tk-key-ovr-${base}` : "unknown";
};
const ovrAnthropicBatch = (status) => ({
	id: OVR_BATCH_ID, type: "message_batch", processing_status: status, created_at: "2026-11-11T00:00:00Z", expires_at: "2026-11-12T00:00:00Z",
	ended_at: "2026-11-11T00:01:00Z", request_counts: { processing: 0, succeeded: 1, errored: 0, canceled: 0, expired: 0 },
});
const ovrAnthropicMessage = { id: "msg_485", type: "message", role: "assistant", model: "claude-sonnet-4-5", content: [{ type: "text", text: "ok" }], stop_reason: "end_turn", usage: { input_tokens: 9, output_tokens: 1 } };
const ovrGeminiBatch = {
	name: `batches/${OVR_BATCH_ID}`, done: true,
	metadata: { "@type": "type.googleapis.com/google.ai.generativelanguage.v1main.GenerateContentBatch", model: "models/gemini-2.5-flash", displayName: "bifrost-batch", createTime: "2026-11-11T00:00:00Z", endTime: "2026-11-11T00:01:00Z", updateTime: "2026-11-11T00:01:00Z", batchStats: { requestCount: "1", successfulRequestCount: "1" }, state: "BATCH_STATE_SUCCEEDED", name: `batches/${OVR_BATCH_ID}` },
	response: { inlinedResponses: { inlinedResponses: [{ metadata: { key: "line-1" }, response: { candidates: [{ content: { role: "model", parts: [{ text: "ok" }] }, finishReason: "STOP" }], usageMetadata: { promptTokenCount: 9, candidatesTokenCount: 1, totalTokenCount: 10 } } }] } },
};
const ovrOpenAIFile = { id: OVR_FILE_ID, object: "file", bytes: 64, filename: "input.jsonl", purpose: "user_data", created_at: 1762819200, status: "processed" };
const OVR_FILE_BYTES = '{"custom_id":"line-1"}\n';
// ovrAnswers[base]["METHOD request_type"] is what the vendor of that base answers on the moved path.
const ovrAnswers = {
	anthropic: {
		"GET batch_retrieve": { body: ovrAnthropicBatch("ended") },
		"POST batch_cancel": { body: ovrAnthropicBatch("canceling") },
		"GET batch_results": { contentType: "application/x-jsonl", raw: JSON.stringify({ custom_id: "line-1", result: { type: "succeeded", message: ovrAnthropicMessage } }) + "\n" },
		"GET file_retrieve": { body: { id: OVR_FILE_ID, type: "file", filename: "input.jsonl", mime_type: "application/jsonl", size_bytes: 64, created_at: "2026-11-11T00:00:00Z", downloadable: true } },
		"DELETE file_delete": { body: { id: OVR_FILE_ID, type: "file_deleted" } },
		"GET file_content": { contentType: "application/octet-stream", raw: OVR_FILE_BYTES },
	},
	gemini: {
		"GET batch_retrieve": { body: ovrGeminiBatch },
		"POST batch_cancel": { body: {} },
		"GET batch_results": { body: ovrGeminiBatch },
		"GET file_retrieve": { body: { name: `files/${OVR_FILE_ID}`, displayName: "input.jsonl", mimeType: "application/jsonl", sizeBytes: "64", createTime: "2026-11-11T00:00:00Z", updateTime: "2026-11-11T00:00:00Z", expirationTime: "2026-11-13T00:00:00Z", uri: `https://generativelanguage.googleapis.com/v1beta/files/${OVR_FILE_ID}`, state: "ACTIVE" } },
		"DELETE file_delete": { body: {} },
	},
	modelark: {
		"GET file_retrieve": { body: ovrOpenAIFile },
		"DELETE file_delete": { body: { id: OVR_FILE_ID, object: "file", deleted: true } },
		"GET file_content": { contentType: "application/octet-stream", raw: OVR_FILE_BYTES },
	},
	replicate: {
		"GET file_retrieve": { body: { id: OVR_FILE_ID, name: "input.jsonl", content_type: "application/jsonl", size: 64, created_at: "2026-11-11T00:00:00Z", expires_at: "2026-11-12T00:00:00Z", urls: { get: `https://api.replicate.com/v1/files/${OVR_FILE_ID}` } } },
		"DELETE file_delete": { status: 204, raw: "" },
	},
};
ovrAnswers.volcengine = ovrAnswers.modelark;
// Video on the same custom providers (folder 485 rows 21-28, TXE-7085). A custom provider on these bases names
// itself in the video ids it returns, so a retrieve, download or delete with that id is routed back to it, with its
// own key; before, it returned the base's suffix and those calls went to the built-in provider. The calls take the
// bases' default video paths under each provider's base_url: Replicate's model predictions and prediction by id,
// and Ark's content-generation tasks for volcengine and modelark. Only the one prediction or task named here
// answers, so an id that lost or kept its suffix is a 404 that shows in GET /__wire.
const OVR_VIDEO_PREDICTION = "pred_ovr_video";
const OVR_VIDEO_TASK = "cgt-ovr-video";
const ovrVideoAnswer = (base, method, rest) => {
	const json = (body, status = 200) => ({ status, body: JSON.stringify(body) });
	if (base === "replicate") {
		if (method === "POST" && rest === "/v1/models/fixture/ovr-video/predictions") return json({ id: OVR_VIDEO_PREDICTION, model: "fixture/ovr-video", status: "starting", created_at: "2026-11-14T00:00:00Z" }, 201);
		if (method === "GET" && rest === `/v1/predictions/${OVR_VIDEO_PREDICTION}`) {
			return json({ id: OVR_VIDEO_PREDICTION, model: "fixture/ovr-video", status: "succeeded", output: "https://fixture.invalid/ovr/video.mp4", created_at: "2026-11-14T00:00:00Z", completed_at: "2026-11-14T00:00:30Z", metrics: { predict_time: 30 } });
		}
		return undefined;
	}
	if (base !== "volcengine" && base !== "modelark") return undefined;
	const tasks = "/contents/generations/tasks";
	if (method === "POST" && rest === tasks) return json({ id: OVR_VIDEO_TASK });
	if (method === "GET" && rest === `${tasks}/${OVR_VIDEO_TASK}`) {
		return json({ id: OVR_VIDEO_TASK, model: "doubao-seedance-1-0-pro-250528", status: "succeeded", content: { video_url: "https://fixture.invalid/ovr/video.mp4" }, usage: { completion_tokens: 108900, total_tokens: 108900 }, resolution: "720p", ratio: "16:9", duration: 5, created_at: 1762700000, updated_at: 1762700090 });
	}
	if (method === "GET" && rest === `${tasks}/${OVR_VIDEO_TASK}/content`) return { status: 200, contentType: "video/mp4", body: "ovr video bytes" };
	if (method === "DELETE" && rest === `${tasks}/${OVR_VIDEO_TASK}`) return { status: 200, body: "" };
	return undefined;
};
const ovrAnswer = (method, path) => {
	const notFound = { status: 404, body: errorBody(`the vendor behind the path overrides serves nothing at ${method} ${path}`, "invalid_request_error", "not_found") };
	const [videoBase, ...restParts] = path.slice(OVR_ROOT.length + 1).split("/");
	const video = ovrVideoAnswer(videoBase, method, "/" + restParts.join("/"));
	if (video) return video;
	const [base, moved, requestType, id, ...more] = path.slice(OVR_ROOT.length + 1).split("/");
	const answer = moved === "moved" && more.length === 0 && ovrAnswers[base] ? ovrAnswers[base][`${method} ${requestType}`] : undefined;
	if (!answer || id !== (requestType.startsWith("batch_") ? OVR_BATCH_ID : OVR_FILE_ID)) return notFound;
	return { status: answer.status || 200, contentType: answer.contentType, body: answer.raw !== undefined ? answer.raw : JSON.stringify(answer.body) };
};

// xAI-compatible batch and files (folder 479, TXE-7084). tkp_xaicompat in the fixture config is an xai-based custom
// provider for a vendor that serves xAI's batch and file API under /api/v2 instead of /v1, its request_path_overrides
// naming those paths ({batch_id} and {file_id} where the id goes). Every call under /pp/xaicompat is kept for
// GET /__wire with its method, path, body and the name of the key it carried (never the key), and anything outside
// /api/v2 is a 404, so a call the gateway built on xAI's own /v1 path fails and shows in the wire. The answers are
// the xAI shapes core/providers/xai decodes (the two-call create: POST /batches, then POST /batches/{id}/requests).
const XAICOMPAT_ROOT = "/pp/xaicompat/api/v2";
const XAICOMPAT_BATCH_ID = "xb_479";
const XAICOMPAT_FILE_ID = "xf_479";
const XAICOMPAT_KEY_NAMES = { "Bearer xc-fixture-key": "tk-key-xaicompat" };
const xaicompatBatch = (state) => ({ batch_id: XAICOMPAT_BATCH_ID, name: "bifrost-batch", create_time: "2026-11-11T00:00:00Z", state });
const xaicompatFile = { id: XAICOMPAT_FILE_ID, object: "file", bytes: 64, filename: "xai-batch-input.jsonl", purpose: "batch", created_at: 1762819200 };
const xaicompatChat = { id: "chatcmpl-xc", object: "chat.completion", created: 1762819200, model: "grok-4.3", choices: [{ index: 0, message: { role: "assistant", content: "ok" }, finish_reason: "stop" }], usage: { prompt_tokens: 9, completion_tokens: 1, total_tokens: 10, cost_in_usd_ticks: 4200 } };
const xaicompatAnswer = (method, path) => {
	const notFound = { status: 404, body: errorBody(`the xAI-compatible vendor serves nothing at ${method} ${path}`, "invalid_request_error", "not_found") };
	if (!path.startsWith(XAICOMPAT_ROOT + "/")) return notFound;
	const rest = path.slice(XAICOMPAT_ROOT.length);
	const ok = (body) => ({ status: 200, body: JSON.stringify(body) });
	if (method === "POST" && rest === "/batches") return ok(xaicompatBatch({ num_requests: 0, num_pending: 0, num_success: 0, num_error: 0, num_cancelled: 0 }));
	if (method === "POST" && rest === `/batches/${XAICOMPAT_BATCH_ID}/requests`) return ok({});
	if (method === "GET" && rest === "/batches") return ok({ batches: [xaicompatBatch({ num_requests: 1, num_pending: 0, num_success: 1, num_error: 0, num_cancelled: 0 })], pagination_token: "" });
	if (method === "GET" && rest === `/batches/${XAICOMPAT_BATCH_ID}`) return ok(xaicompatBatch({ num_requests: 1, num_pending: 0, num_success: 1, num_error: 0, num_cancelled: 0 }));
	if (method === "POST" && rest === `/batches/${XAICOMPAT_BATCH_ID}:cancel`) return ok({ ...xaicompatBatch({ num_requests: 1, num_pending: 0, num_success: 1, num_error: 0, num_cancelled: 0 }), cancel_time: "2026-11-11T00:01:00Z" });
	if (method === "GET" && rest === `/batches/${XAICOMPAT_BATCH_ID}/results`) {
		return ok({ results: [{ batch_request_id: "line-1", batch_result: { response: { chat_get_completion: xaicompatChat } } }], pagination_token: "" });
	}
	if (method === "POST" && rest === "/files") return ok(xaicompatFile);
	if (method === "GET" && rest === "/files") return ok({ data: [xaicompatFile], has_more: false });
	if (method === "GET" && rest === `/files/${XAICOMPAT_FILE_ID}`) return ok(xaicompatFile);
	if (method === "DELETE" && rest === `/files/${XAICOMPAT_FILE_ID}`) return ok({ id: XAICOMPAT_FILE_ID, deleted: true });
	if (method === "GET" && rest === `/files/${XAICOMPAT_FILE_ID}/content`) {
		return { status: 200, contentType: "application/octet-stream", body: '{"custom_id":"line-1","method":"POST","url":"/v1/chat/completions"}\n' };
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
			// TXE-6994: the same for the openai-based custom provider cp_plain, whose Responses requests the fixture records.
			"cp-tiered": { provider: "cp_plain", mode: "chat", service_tiers: ["default", "priority"], supported_endpoints: ["/v1/chat/completions", "/v1/responses"] },
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
		for (const socket of mediaTunnelSockets) socket.destroy();
		mediaTunnelSockets.clear();
		for (const k of Object.keys(hits)) delete hits[k];
		openAPICalls.length = 0;
		wireCalls.length = 0;
		for (const k of Object.keys(sentBodies)) delete sentBodies[k];
		assemblyAICalls.length = 0;
		klingCalls.length = 0;
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
	// DashScope's native API: Wan video, TXE-6432 (folder 423), and the synchronous image endpoint, TXE-6607
	// (folder 425). The gateway's dashscope provider has both of its bases at /pp/dashscope. Every request is kept in /__wire with its method and its X-DashScope-Async header (no
	// other header, so no key reaches a report). A create answers a PENDING task. The task id of a query or a
	// cancel picks the answer (see dashScopeTasks): only ds-pending can be cancelled, as at DashScope, which
	// answers 400 UnsupportedOperation for a task in any other state. GET /api/v1/tasks is the task list.
	if (path.startsWith("/pp/dashscope/")) {
		const body = [];
		req.on("data", (c) => body.push(c));
		req.on("end", () => {
			const url = new URL(req.url, "http://localhost");
			const bytes = Buffer.concat(body).toString("utf8");
			wireCalls.push({ method: req.method, path, query: url.search, body: bytes, async: req.headers["x-dashscope-async"] || "" });
			// The synchronous image endpoint (TXE-6607, folder 425) answers by the model in the body.
			if (req.method === "POST" && path === dashScopeImagePath) {
				let parsed = {};
				try { parsed = JSON.parse(bytes) || {}; } catch {}
				const image = dashScopeImageAnswer(parsed);
				return send(image.status, {}, image.body);
			}
			let parsedBody;
			try { parsedBody = JSON.parse(bytes); } catch {}
			const answer = dashScopeAnswer(req.method, path.slice("/pp/dashscope".length), url.search, parsedBody);
			send(answer.status, {}, JSON.stringify(answer.body));
		});
		return undefined;
	}
	// The same record without the bodies: their size and both ends, for rows whose request is megabytes long (folder 475).
	if (req.method === "GET" && path === "/__wire/summary") {
		const calls = wireCalls.map((c) => ({ path: c.path, bytes: Buffer.byteLength(c.body), head: c.body.slice(0, 400), tail: c.body.slice(-400) }));
		return send(200, {}, JSON.stringify({ data: [{ calls }] }));
	}
	if (path.startsWith("/pp/vidvendor/")) {
		drain(req, () => {
			wireCalls.push({ method: req.method, path, query: new URL(req.url, "http://localhost").search, body: "", key: VIDVENDOR_KEY_NAMES[req.headers.authorization] || "unknown" });
			const answer = vidvendorAnswer(req.method, path);
			send(answer.status, answer.contentType ? { "Content-Type": answer.contentType } : {}, answer.body);
		});
		return undefined;
	}
	if (path.startsWith(OVR_ROOT + "/")) {
		req.resume();
		req.on("end", () => {
			wireCalls.push({ method: req.method, path, query: new URL(req.url, "http://localhost").search, body: "", key: ovrKeyName(req.headers) });
			const answer = ovrAnswer(req.method, path);
			send(answer.status, answer.contentType ? { "Content-Type": answer.contentType } : {}, answer.body);
		});
		return undefined;
	}
	if (path.startsWith("/pp/xaicompat/")) {
		const body = [];
		req.on("data", (c) => body.push(c));
		req.on("end", () => {
			const bytes = Buffer.concat(body).toString("utf8");
			wireCalls.push({ method: req.method, path, query: new URL(req.url, "http://localhost").search, body: bytes, key: XAICOMPAT_KEY_NAMES[req.headers.authorization] || "unknown" });
			const answer = xaicompatAnswer(req.method, path);
			send(answer.status, answer.contentType ? { "Content-Type": answer.contentType } : {}, answer.body);
		});
		return undefined;
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
	// Folder 520 (TXE-7329): the fixture/rendered-longer prediction has succeeded, with its video on the
	// provider's own origin, so the gateway downloads it with the provider's client. Each read is counted.
	const renderedPoll = /^\/pp\/replicate\/v1\/predictions\/pred_(rendered_[a-z_]+)$/.exec(path);
	if (req.method === "GET" && renderedPoll && RENDERED_VIDEOS[renderedPoll[1].replace(/_/g, "-")]) {
		const name = renderedPoll[1].replace(/_/g, "-");
		countHit(`replicate.${name}.poll`);
		const host = RENDERED_FOREIGN.has(name) ? "localhost" : "127.0.0.1";
		return send(200, {}, JSON.stringify({ id: `pred_${renderedPoll[1]}`, status: "succeeded", output: `http://${host}:${port}/pp/replicate/v1/files/${name}.mp4` }));
	}
	const renderedFile = /^\/pp\/replicate\/v1\/files\/([a-z-]+)\.mp4$/.exec(path);
	if (req.method === "GET" && renderedFile && RENDERED_VIDEOS[renderedFile[1]]) {
		const video = RENDERED_VIDEOS[renderedFile[1]];
		countHit(`replicate.${renderedFile[1]}.download`);
		res.writeHead(200, { "Content-Type": "video/mp4", "Content-Length": video.length });
		return res.end(video);
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
		const taskId = decodeURIComponent(path.slice("/pp/minimax/v2/query/video_generation/".length));
		if (taskId === MINIMAX_BAND_TASK || taskId === MINIMAX_IMAGES_TASK) {
			countHit(`minimax.query ${taskId}`);
			const model = taskId === MINIMAX_BAND_TASK ? "fixture-minimax-2k" : "fixture-minimax-images";
			return send(200, {}, JSON.stringify({ task: { id: taskId, model, status: "succeeded", content: { url: `http://127.0.0.1:${port}/pp/media/internal.mp4` }, resolution: "2K", duration: 6 } }));
		}
		const answer = miniMaxBaseRespAnswer(taskId);
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
	if (req.method === "DELETE" && path.startsWith(VOLCENGINE_TASKS + "/")) {
		const id = decodeURIComponent(path.slice(VOLCENGINE_TASKS.length + 1));
		req.resume();
		countHit(`volcengine.delete ${id}`);
		if (id === "cgt-volc-empty-object") return send(200, {}, "{}");
		return send(200, {}, "");
	}
	if (req.method === "POST" && (path.startsWith("/pp/") || path === "/v1/embeddings" || path === "/anthropic/v1/messages" || isOpenAIImagePath(path) || isBedrockConversePath(path) || isBedrockConverseStreamPath(path))) {
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
			send(answer.status, answer.headers || {}, answer.body);
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
	// The xAI video link, fetched through this fixture as a forward proxy (see XAI_MEDIA_HOST). The row
	// says whether the fetch carried credentials: the link is public and must get none.
	const proxiedHost = /^http:\/\//.test(req.url) ? new URL(req.url).hostname : (req.headers.host || "").split(":")[0];
	// Only the video's own path: the media tunnel joins other requests for this address to the
	// fixture's ordinary routes (/__hits among them).
	if (req.method === "GET" && proxiedHost === XAI_MEDIA_HOST && path.startsWith("/xai-video-")) {
		wireCalls.push({ method: "GET", path, query: "", body: "", host: XAI_MEDIA_HOST, authorization: Boolean(req.headers.authorization) });
		res.writeHead(200, { "Content-Type": "video/mp4", "Content-Length": FIXTURE_MP4.length });
		return res.end(FIXTURE_MP4);
	}
	// An xAI video task query (folder 415, TXE-6429), recorded as a /__wire row with its method.
	const xaiVideoQuery = path.match(/^\/pp\/xai\/v1\/videos\/([^/]+)$/);
	if (req.method === "GET" && xaiVideoQuery) {
		wireCalls.push({ method: "GET", path, query: "", body: "" });
		return send(200, {}, JSON.stringify(xaiVideoTask(decodeURIComponent(xaiVideoQuery[1]))));
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
	// The Gemini Interactions API reads of folder 418 (TXE-6538); see omniInteraction above.
	const omniRetrieve = path.match(/^\/pp\/gemini\/interactions\/([^/]+)$/);
	if (req.method === "GET" && omniRetrieve) {
		countHit(`omni.retrieve ${omniRetrieve[1]}`);
		return send(200, {}, JSON.stringify(omniInteraction(omniRetrieve[1], `http://${req.headers.host}`)));
	}
	if (req.method === "GET" && path === "/pp/gemini/files/omni-file:download") {
		countHit("omni.file");
		res.writeHead(200, { "Content-Type": "video/mp4", "Content-Length": FIXTURE_MP4.length });
		return res.end(FIXTURE_MP4);
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
	// Azure's video generation jobs API for first-generation sora (folder 420, TXE-6533), on the azure key's
	// endpoint. Every call is counted under "sora1 <METHOD> <job|content> <api-key>" and its query and body kept
	// in /__wire. A create answers the quickstart's queued job, or the documented ResponsibleAIPolicyViolation
	// error when the prompt contains "blocked"; a query answers the job succeeded with one generation; the
	// generation's video is FIXTURE_MP4; a delete answers 204 with no body, as documented. The jobs API has no cancel endpoint.
	const soraJob = path.match(/^\/openai\/v1\/video\/generations\/jobs(?:\/([^/]+))?$/);
	const soraJobContent = path.match(/^\/openai\/v1\/video\/generations\/([^/]+)\/content\/video$/);
	if (soraJob || soraJobContent) {
		const body = [];
		req.on("data", (c) => body.push(c));
		req.on("end", () => {
			const bytes = Buffer.concat(body).toString("utf8");
			countHit(`sora1 ${req.method} ${soraJobContent ? "content" : "job"} ${req.headers["api-key"] || ""}`);
			wireCalls.push({ path, query: new URL(req.url, "http://localhost").search, body: bytes });
			const job = (status, generations) => ({
				object: "video.generation.job", id: "task_fixture_sora1", status, created_at: 1748469875,
				finished_at: status === "succeeded" ? 1748470000 : null, expires_at: status === "succeeded" ? 1748556400 : null,
				generations, prompt: "a paper boat on a pond", model: "sora", n_variants: 1, n_seconds: 5, height: 480, width: 480,
				failure_reason: null,
			});
			if (soraJobContent && req.method === "GET" && soraJobContent[1] === "gen_fixture_sora1") {
				res.writeHead(200, { "Content-Type": "video/mp4", "Content-Length": FIXTURE_MP4.length });
				return res.end(FIXTURE_MP4);
			}
			if (soraJob && !soraJob[1] && req.method === "POST") {
				let parsed = {};
				try { parsed = JSON.parse(bytes) || {}; } catch {}
				if (String(parsed.prompt || "").includes("blocked")) {
					return send(400, {}, JSON.stringify({ error: { code: "content_policy_violation", message: "Your request was rejected as a result of our safety system.", param: "prompt", inner_error: { code: "ResponsibleAIPolicyViolation" } } }));
				}
				return send(200, {}, JSON.stringify(job("queued", [])));
			}
			if (soraJob && soraJob[1] === "task_fixture_sora1" && req.method === "DELETE") {
				res.writeHead(204);
				return res.end();
			}
			if (soraJob && soraJob[1] === "task_fixture_sora1" && req.method === "GET") {
				return send(200, {}, JSON.stringify(job("succeeded", [{ object: "video.generation", id: "gen_fixture_sora1", job_id: "task_fixture_sora1", created_at: 1748469990, width: 480, height: 480, n_seconds: 5, prompt: "a paper boat on a pond" }])));
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
	// The Kling API on its new design standard (TXE-6430): the three create paths, the query by task id or
	// by the caller's own id, and the list by cursor. Only the keys of the kling provider (kling-secret-a,
	// kling-secret-b) are served here. Every request is kept, with its key and JSON body, under
	// /__kling-calls and counted under "kling <METHOD> <path> <bearer token>", so a case can prove the body
	// the gateway sent, which key it used, and that a request it refuses never got here. Kling has no cancel
	// and no delete API, so the fixture has none. A prompt that says "blocked" is answered as Kling answers
	// its content security policy: HTTP 400, service code 1301.
	if (req.method === "GET" && path === "/__kling-calls") {
		return send(200, {}, JSON.stringify({ data: [{ calls: klingCalls }] }));
	}
	if (bearer.startsWith("kling-secret-")) {
		const url = new URL(req.url, "http://localhost");
		return collect(req, (raw) => {
			let body = null;
			try { body = raw ? JSON.parse(raw) : null; } catch { body = raw; }
			klingCalls.push({ method: req.method, path, query: url.search.replace(/^\?/, ""), key: bearer, body });
			countHit(`kling ${req.method} ${path} ${bearer}`);
			if (req.method === "POST" && /^\/(text-to-video|image-to-video|omni-video)\/[^/]+$/.test(path)) {
				const prompt = body?.prompt || (body?.contents || []).find((c) => c.type === "prompt")?.text || "";
				if (/blocked/.test(prompt)) {
					return send(400, {}, JSON.stringify({ code: 1301, message: "The content you uploaded appears to violate the community guidelines", request_id: "kling-fixture-1301" }));
				}
				const taskID = prompt.includes("[pro-mode]") ? KLING_PRO_MODE_TASK_ID : KLING_TASK_ID;
				return send(200, {}, klingEnvelope({ id: taskID, status: "submitted", create_time: 1781080778802, update_time: 1781080778802, external_id: body?.options?.external_task_id || "" }));
			}
			if (req.method === "GET" && path === "/tasks") {
				const taskID = url.searchParams.get("task_ids");
				const externalID = url.searchParams.get("external_task_ids");
				if (taskID) return send(200, {}, klingEnvelope([klingTask(taskID, "tk_fixture_task")]));
				// No task was created with a reference that starts "missing": Kling answers an empty list.
				if (externalID && !externalID.startsWith("missing")) return send(200, {}, klingEnvelope([klingTask(KLING_TASK_ID, externalID)]));
				return send(200, {}, klingEnvelope([]));
			}
			if (req.method === "POST" && path === "/tasks") {
				return send(200, {}, klingEnvelope({ result: [klingTask(KLING_TASK_ID, "tk_fixture_task")], count: 1, next_cursor: "", has_more: false }));
			}
			return send(404, {}, JSON.stringify({ code: 1203, message: "The requested resource does not exist", request_id: "kling-fixture-1203" }));
		});
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
		const requested = typeof parsed.model === "string" ? parsed.model : "";
		// An /any-key request (ANY_KEY_PREFIX) is answered as upstream-ok whatever model it names.
		if (req.anyKey) parsed.model = "upstream-ok";
		const model = req.anyKey ? "upstream-ok" : requested;
		const hitName = req.anyKey ? "any-key:" + requested : model;
		hits[hitName] = (hits[hitName] || 0) + 1;
		// test-key is the only credential the fixture accepts. A request bearing any other key is refused
		// with 401 before its model is looked at, which is how a provider refuses a caller's direct key.
		const auth = req.headers.authorization;
		if (!req.anyKey && auth !== undefined && auth !== "Bearer test-key") {
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
		if (path === "/v1/responses" && model === "agent-usage") {
			const answer = perplexityAgentResponse(model, parsed.stream === true);
			if (answer.stream !== undefined) {
				res.writeHead(answer.status, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
				return res.end(answer.stream);
			}
			return send(answer.status, {}, answer.body);
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
// /any-key/<path> is an OpenAI-compatible upstream that accepts every key and serves every model, as a real
// provider serves a caller's own key (folder 185 row 8, TXE-7289). The prefix is stripped, the request skips
// the test-key check, every chat answers as upstream-ok, and its hit is counted under "any-key:<model>", so a
// case tells this upstream's attempts from the strict one's on the same model name.
const ANY_KEY_PREFIX = "/any-key/";
// The S3-compatible object store a hybrid log store offloads to (TXE-7326), with a control API that
// holds or fails a chosen PUT; lib/s3-object-fixture.mjs documents both.
const s3Objects = createS3ObjectFixture();
const server = http.createServer((req, res) => {
	const url = new URL(req.url, "http://localhost");
	if (s3Objects.handles(url.pathname)) {
		const chunks = [];
		req.on("data", (c) => chunks.push(c));
		req.on("end", () => s3Objects.handle(req, res, url, Buffer.concat(chunks)));
		return;
	}
	if (req.url.startsWith(ANY_KEY_PREFIX)) {
		req.url = req.url.slice(ANY_KEY_PREFIX.length - 1);
		req.anyKey = true;
	}
	return handleRequest(req, res);
});

// The forward-proxy role (folder 415, TXE-6429). A client that tunnels answers to CONNECT host:port; the
// tunnel ends here whatever host it names, so the requests inside it are served by this same server: one for
// this fixture's own address is an ordinary request, one for XAI_MEDIA_HOST is the video link.
server.on("connect", (req, socket, head) => {
	socket.write("HTTP/1.1 200 Connection Established\r\n\r\n");
	if (head && head.length) socket.unshift(head);
	server.emit("connection", socket);
});

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
	mediaTunnelSockets.add(socket);
	socket.on("close", () => mediaTunnelSockets.delete(socket));
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
