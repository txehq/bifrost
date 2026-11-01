#!/usr/bin/env node
// The provider behind run-warp-wire.mjs: every model call, embedding and pricing read a Warp turn
// makes lands here, so a test can script the model and read what the gateway sent it.
//
//   POST /__script   {key, steps}: what /v1/responses answers for a request naming [case:<key>]
//   GET  /__calls?key=<key>   the request bodies that named it
//   GET  /__embeddings        every embedding input so far
//   POST /__hold     {match}: hold an embedding whose input contains match
//   GET  /__held     whether one is being held
//   POST /__release  answer whatever is held
//
// A step is {tool, args} or {tools: [{tool, args}]} for tool calls, {text}, {text, tool, args} for a
// message beside a tool call, or {status} for a failure. Past the last step it answers "done.".
// An embedding input holding "wrong-shape" gets a vector of the wrong size and "refused" a 400.
import http from "node:http";

const port = Number(process.env.WARP_WIRE_FIXTURE_PORT || "8792");
export const CHAT_INPUT = 100;
export const CHAT_OUTPUT = 10;
export const EMBED_TOKENS = 7;
const RATES = {
	"wire-chat": { provider: "openai", mode: "chat", input_cost_per_token: 0.000001, output_cost_per_token: 0.000002 },
	"wire-embed": { provider: "openai", mode: "embedding", input_cost_per_token: 0.0000001, output_cost_per_token: 0 },
};

const scripts = new Map();
const calls = new Map();
const embeddings = [];
let hold = null;
let held = null;
let nextId = 0;

const readBody = (req) => new Promise((resolve, reject) => {
	const chunks = [];
	req.on("data", (c) => chunks.push(c));
	req.on("end", () => resolve(Buffer.concat(chunks).toString("utf8")));
	req.on("error", reject);
});

const toolCall = ({ tool, args }) => {
	nextId++;
	return { type: "function_call", id: `fc_${nextId}`, call_id: `call_${nextId}`, name: tool, arguments: JSON.stringify(args ?? {}), status: "completed" };
};
const message = (text) => ({ type: "message", id: `msg_${++nextId}`, role: "assistant", status: "completed", content: [{ type: "output_text", text, annotations: [] }] });

function modelResponse(step, model) {
	const output = [];
	if (step.text) output.push(message(step.text));
	if (step.tool) output.push(toolCall(step));
	for (const call of step.tools ?? []) output.push(toolCall(call));
	return {
		id: `resp_${++nextId}`, object: "response", created_at: 1, status: "completed", model, output,
		usage: { input_tokens: CHAT_INPUT, output_tokens: CHAT_OUTPUT, total_tokens: CHAT_INPUT + CHAT_OUTPUT },
	};
}

const server = http.createServer(async (req, res) => {
	const url = new URL(req.url, "http://localhost");
	const send = (status, body) => {
		res.writeHead(status, { "Content-Type": "application/json" });
		res.end(JSON.stringify(body));
	};
	const raw = req.method === "POST" ? await readBody(req) : "";

	if (url.pathname === "/pricing.json") return send(200, RATES);
	if (url.pathname === "/model-parameters.json") return send(200, {});
	if (url.pathname === "/v1/models") return send(200, { object: "list", data: Object.keys(RATES).map((id) => ({ id, object: "model", owned_by: "wire" })) });

	if (url.pathname === "/__script") {
		const { key, steps } = JSON.parse(raw);
		scripts.set(key, { steps, next: 0 });
		calls.set(key, []);
		return send(200, { ok: true });
	}
	if (url.pathname === "/__calls") return send(200, { calls: calls.get(url.searchParams.get("key")) ?? [] });
	if (url.pathname === "/__embeddings") return send(200, { inputs: embeddings });
	if (url.pathname === "/__hold") { hold = JSON.parse(raw).match; return send(200, { ok: true }); }
	if (url.pathname === "/__held") return send(200, { held: held !== null });
	if (url.pathname === "/__release") {
		hold = null;
		held?.();
		held = null;
		return send(200, { ok: true });
	}

	if (url.pathname === "/v1/responses") {
		const body = JSON.parse(raw);
		const key = raw.match(/\[case:([a-z0-9-]+)\]/)?.[1];
		const script = scripts.get(key);
		if (script === undefined) return send(500, { error: { message: `no script for ${key}`, type: "server_error" } });
		calls.get(key).push(body);
		const step = script.steps[script.next++] ?? { text: "done." };
		if (step.status) return send(step.status, { error: { message: "the model is unavailable", type: "server_error", code: "wire_failure" } });
		return send(200, modelResponse(step, body.model));
	}

	if (url.pathname === "/v1/embeddings") {
		const body = JSON.parse(raw);
		const text = Array.isArray(body.input) ? body.input.join(" ") : String(body.input);
		embeddings.push(text);
		if (hold !== null && text.includes(hold)) await new Promise((resolve) => { held = resolve; });
		if (text.includes("refused")) return send(400, { error: { message: "embedding refused", type: "invalid_request_error" } });
		const size = text.includes("wrong-shape") ? 3 : (body.dimensions ?? 8);
		return send(200, {
			object: "list", model: body.model,
			data: [{ object: "embedding", index: 0, embedding: Array.from({ length: size }, () => 0.5) }],
			usage: { prompt_tokens: EMBED_TOKENS, total_tokens: EMBED_TOKENS },
		});
	}
	return send(404, { error: { message: `no route ${url.pathname}`, type: "invalid_request_error" } });
});

server.listen(port, "127.0.0.1", () => console.log(`warp wire fixture listening on 127.0.0.1:${port}`));
