import http from "node:http";
import net from "node:net";

const port = Number(process.env.PROVIDER_ERROR_FIXTURE_PORT || "8791");
// The stall proxy: an HTTP proxy that accepts the TCP connection and never answers the CONNECT, so a
// gateway routed through it stalls inside connection acquisition (the proxy handshake), before any
// request reaches an upstream. Each CONNECT it reads is counted under __stall_proxy_connects in /__hits.
const stallPort = Number(process.env.PROVIDER_ERROR_FIXTURE_STALL_PORT || "8799");
// How long a stalled connection is held before the fixture drops it, longer than any first-token deadline
// a case arms and shorter than a proxy handshake timeout, so a gateway that waits for the proxy still fails.
const STALL_HOLD_MS = 15000;

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
// upstream-slow answers as upstream-ok does, after SLOW_MS.
behaviours["upstream-slow"] = () => ({ ...behaviours["upstream-ok"](), delayMs: SLOW_MS });
// upstream-cut starts answering as upstream-ok does and drops the connection partway: a stream gets its first
// content chunk and then a closed socket, a non-streamed request a closed socket.
behaviours["upstream-cut"] = () => ({ ...behaviours["upstream-ok"](), cut: true });

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
const wireAnswer = (path, body) => {
	if (path === "/pp/xai/v1/messages" || path === "/pp/anthropic/v1/messages") {
		if (body.model === "pp-fail") return { status: 400, body: JSON.stringify({ type: "error", error: { type: "invalid_request_error", message: "fixture refused" } }) };
		if (body.stream === true) return { status: 200, stream: anthropicMessageStream(body.model) };
		return { status: 200, body: JSON.stringify(anthropicMessage(body.model)) };
	}
	if (path.endsWith(":generateContent")) return { status: 200, body: JSON.stringify(geminiCodeExecution) };
	if (path.endsWith(":streamGenerateContent")) return { status: 200, stream: `data: ${JSON.stringify(geminiCodeExecution)}\n\n` };
	if (path === "/pp/cloudflare/accounts/acct-1/ai/run/@cf/cloudflare/clef-flash") return { status: 200, body: JSON.stringify(clefAnswer) };
	if (path === "/v1/embeddings" || path === "/pp/volcengine/embeddings") return { status: 200, body: JSON.stringify(embeddingAnswer) };
	if (path === "/pp/volcengine/embeddings/multimodal") return { status: 200, body: JSON.stringify({ id: "emb_pp", object: "list", model: body.model, data: { embedding: [0.1, 0.2] }, usage: { prompt_tokens: 1, total_tokens: 1 } }) };
	return { status: 500, body: errorBody("the fixture has no answer here", "server_error", "internal_error") };
};

// Hit counters are keyed by the request body's model, so they must not inherit Object.prototype names.
const hits = Object.create(null);

const server = http.createServer((req, res) => {
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
		}));
	}
	if (req.method === "POST" && path === "/__reset") {
		for (const k of Object.keys(hits)) delete hits[k];
		openAPICalls.length = 0;
		wireCalls.length = 0;
		return send(200, {}, JSON.stringify({ data: [{ reset: true }] }));
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
	if (req.method === "POST" && (path.startsWith("/pp/") || path === "/v1/embeddings")) {
		const body = [];
		req.on("data", (c) => body.push(c));
		req.on("end", () => {
			const bytes = Buffer.concat(body).toString("utf8");
			wireCalls.push({ path, query: new URL(req.url, "http://localhost").search, body: bytes });
			let parsed = {};
			try { parsed = JSON.parse(bytes) || {}; } catch {}
			const answer = wireAnswer(path, parsed);
			if (answer.stream !== undefined) {
				res.writeHead(answer.status, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
				return res.end(answer.stream);
			}
			send(answer.status, {}, answer.body);
		});
		return undefined;
	}
	if (req.method === "GET" && path === "/__hits") {
		return send(200, {}, JSON.stringify({ data: [{ hits: { ...hits } }] }));
	}
	if (req.method === "GET" && path === "/mcp") {
		return send(405, {}, errorBody("no server stream", "invalid_request_error", "method_not_allowed"));
	}
	if (req.method !== "POST" || (path !== "/v1/chat/completions" && path !== "/openai/v1/chat/completions" && path !== "/v1/responses" && path !== "/v1/live/sessions" && path !== "/mcp" && path !== "/v1/decisions" && path !== "/alpha/decisions")) {
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

stallProxy.listen(stallPort, "127.0.0.1", () => {
	server.listen(port, "127.0.0.1", () => {
		console.log(`provider-error-fixture stall proxy on http://127.0.0.1:${stallPort}`);
		console.log(`provider-error-fixture listening on http://127.0.0.1:${port}`);
	});
});
