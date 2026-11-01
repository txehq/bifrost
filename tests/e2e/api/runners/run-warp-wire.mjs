#!/usr/bin/env node
// Wire checks for Warp's usage, namespaces, charts, traces, search and backfill, against a running
// gateway and warp-wire-fixture.mjs (TXE-5543). A collection cannot script a model turn by turn, hold a
// backfill between two pages, or write rows that share a timestamp, so this drives POST /api/warp/chat
// over SSE and reads what the gateway sent the fixture. Start the gateway from
// provider_config/warp-wire.config.json with a logs_store and config_store for WARP_BACKEND.
//
//   WARP_BASE_URL       the gateway
//   WARP_FIXTURE_URL    the fixture
//   WARP_WEAVIATE_URL   the Weaviate the gateway uses, to read which namespaces exist
//   WARP_SETUP_TOKEN    the gateway's setup token
//   WARP_BACKEND        sqlite | postgres | clickhouse: the logs store
//   WARP_SEED_BIN       tests/cmd/seed/warpwireseed
//   WARP_LOGS_CONFIG    the gateway's logs_store object, as a file
//   WARP_CONFIG_DB      the config store: sqlite:<path> or postgres:<dsn>
//   WARP_RUN_TAG        lower-case alphanumerics, unique per run, for namespaces and ids
//   WARP_REPORT         where to write the rows as JSON
//
// Exit 0: every row passed. Exit 1: a row failed. Exit 2: setup failed, so the run proves nothing.
import { execFile } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { promisify } from "node:util";

const env = (name) => {
	const value = process.env[name];
	if (!value) { console.error(`${name} is required`); process.exit(2); }
	return value;
};
const base = env("WARP_BASE_URL");
const fixture = env("WARP_FIXTURE_URL");
const weaviate = env("WARP_WEAVIATE_URL");
const setupToken = env("WARP_SETUP_TOKEN");
const backend = env("WARP_BACKEND");
const seedBin = env("WARP_SEED_BIN");
const logsConfig = env("WARP_LOGS_CONFIG");
const configDb = env("WARP_CONFIG_DB");
const tag = env("WARP_RUN_TAG");
const reportPath = env("WARP_REPORT");

const CHAT_TOKENS = 110; // warp-wire-fixture.mjs: 100 in, 10 out
const CHAT_COST = 100 * 0.000001 + 10 * 0.000002;
const EMBED_TOKENS = 7;
const EMBED_COST = 7 * 0.0000001;
const BATCH = 100; // backfillBatchSize
const close = (a, b) => Math.abs(a - b) < 1e-9;
const exec = promisify(execFile);

const rows = [];
const row = (name, ok, detail = "") => {
	const named = `${backend} ${name}`;
	rows.push({ name: named, ok: !!ok, detail: String(detail).slice(0, 400) });
	console.log(`${ok ? "ok    " : "FAILED"} ${named}${detail === "" ? "" : ` :: ${String(detail).slice(0, 400)}`}`);
};
const setupFailure = (message) => { console.error(`setup failed: ${message}`); process.exit(2); };

const api = async (method, apiPath, body) => {
	const res = await fetch(new URL(apiPath, base), {
		method,
		headers: { "X-Bifrost-Setup-Token": setupToken, "Content-Type": "application/json" },
		body: body === undefined ? undefined : JSON.stringify(body),
		signal: AbortSignal.timeout(60_000),
	});
	const text = await res.text();
	let json = null;
	try { json = JSON.parse(text); } catch { /* an SSE body or an empty one */ }
	return { status: res.status, text, json };
};
const fx = async (apiPath, body) => (await fetch(new URL(apiPath, fixture), body === undefined ? {} : { method: "POST", body: JSON.stringify(body) })).json();
const script = (key, steps) => fx("/__script", { key, steps });
const toolOutputs = async (key) => {
	const { calls } = await fx(`/__calls?key=${key}`);
	return calls.flatMap((call) => (call.input ?? []).filter((item) => item.type === "function_call_output").map((item) => {
		const output = typeof item.output === "string" ? item.output : JSON.stringify(item.output);
		try { return JSON.parse(output); } catch { return output; }
	}));
};
// The last call shows every output so far, so the last entry is the newest.
const lastOutput = async (key) => (await toolOutputs(key)).at(-1);

const seed = async (logs) => {
	const file = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "warp-wire-")), "rows.json");
	fs.writeFileSync(file, JSON.stringify(logs));
	await exec(seedBin, ["-config", logsConfig, "-insert", file]);
};
const unseed = (id) => exec(seedBin, ["-config", logsConfig, "-delete", id]);
const configSql = async (sql) => {
	const [kind, target] = [configDb.slice(0, configDb.indexOf(":")), configDb.slice(configDb.indexOf(":") + 1)];
	const { stdout } = kind === "sqlite" ? await exec("sqlite3", [target, sql]) : await exec("psql", [target, "-Atqc", sql]);
	return stdout.trim();
};

// Weaviate takes UUIDs only, as the gateway's own log ids are. The index comes last, zero-padded, so the
// ids sort as the indexes do and a test can say which row a page ends on.
const rowId = (group, index) => {
	const prefix = createHash("sha256").update(`${tag}-${group}`).digest("hex");
	return `${prefix.slice(0, 8)}-${prefix.slice(8, 12)}-4${prefix.slice(13, 16)}-8${prefix.slice(17, 20)}-${String(index).padStart(12, "0")}`;
};

const logRow = (id, timestamp, fields = {}) => ({
	id, timestamp: new Date(timestamp).toISOString(), created_at: new Date(timestamp).toISOString(),
	object: "chat_completion", provider: "openai", model: `wire-${tag}`, status: "success", latency: 100, content_summary: `summary of ${id}`, ...fields,
});

const chatBody = (question, extra = {}) => ({ messages: [{ role: "user", content: question }], timezone: "UTC", utc_offset_minutes: 0, ...extra });
const turn = async (question, extra = {}) => {
	const res = await api("POST", "/api/warp/chat", { ...chatBody(question, extra), stream: true });
	const frames = res.text.split("\n\n").flatMap((block) => block.split("\n").filter((line) => line.startsWith("data:")).map((line) => JSON.parse(line.slice(5))));
	return { status: res.status, frames, terminal: frames.findLast((frame) => frame.type === "done" || frame.type === "error") };
};
const answer = async (question) => (await api("POST", "/api/warp/chat", { ...chatBody(question), stream: false })).json;
const stored = async (id) => {
	const res = await api("GET", `/api/warp/conversations/${id}`);
	return (res.json?.messages ?? []).filter((message) => message.role === "assistant");
};

const spaceInput = (namespace, fields = {}) => ({
	enabled: true, provider: "openai", model: "wire-chat", max_iterations: 8, request_timeout_seconds: 60, history_retention_days: 30,
	embedding_provider: "openai", embedding_model: "wire-embed", embedding_dimension: 8, log_vector_store_namespace: namespace,
	semantic_search_threshold: 0.1, semantic_search_limit: 10, ...fields,
});
const ns = (name) => `Warpwire${tag}${name}`;
const namespaceExists = async (namespace) => (await fetch(`${weaviate}/v1/schema/${namespace}`)).status === 200;

// One turn that searches, priced exactly: tokens and cost for the model calls the fixture saw and the
// embeddings billed.
const expectSpend = async (name, key, frame, embeddings, modelCalls) => {
	const calls = modelCalls ?? (await fx(`/__calls?key=${key}`)).calls.length;
	const tokens = calls * CHAT_TOKENS + embeddings * EMBED_TOKENS;
	const cost = calls * CHAT_COST + embeddings * EMBED_COST;
	const usage = frame?.usage;
	row(`${name} reports each embedding once`, usage?.total_tokens === tokens && close(usage?.cost?.total_cost ?? 0, cost), `tokens ${usage?.total_tokens} want ${tokens}, cost ${usage?.cost?.total_cost} want ${cost}`);
	const assistant = (await stored(frame?.conversation_id)).at(-1);
	row(`${name} persists what it reported`, assistant?.total_tokens === tokens && close(assistant?.cost ?? 0, cost), `stored tokens ${assistant?.total_tokens}, cost ${assistant?.cost}`);
};
const search = (queries, filters = {}) => ({ tool: "semantic_search_logs", args: { queries, filters } });

async function checkUsage() {
	let key = `${tag}-usage-ok`;
	await script(key, [search([`declined cards [case:${key}]`, `failed payments [case:${key}]`]), { text: "none found." }]);
	const embedded = (await fx("/__embeddings")).inputs.length;
	let result = await turn(`why did payments fail? [case:${key}]`);
	row("usage success ends done", result.terminal?.type === "done", JSON.stringify(result.terminal).slice(0, 200));
	row("usage success embeds each phrasing once", (await fx("/__embeddings")).inputs.length - embedded === 2);
	await expectSpend("usage success", key, result.terminal, 2);

	// Both answered and billed; one vector cannot be used, so the tool fails and the turn goes on.
	key = `${tag}-usage-shape`;
	await script(key, [search([`wrong-shape one [case:${key}]`, `good one [case:${key}]`]), { text: "search failed." }]);
	result = await turn(`search please [case:${key}]`);
	await expectSpend("usage rejected vector", key, result.terminal, 2);

	// The refused call was not billed.
	key = `${tag}-usage-declined`;
	await script(key, [search([`refused one [case:${key}]`, `good one [case:${key}]`]), { text: "search failed." }]);
	result = await turn(`search please [case:${key}]`);
	await expectSpend("usage refused call", key, result.terminal, 1);

	// A failed turn reports and files what it spent, and the next question continues its thread.
	key = `${tag}-usage-failed`;
	await script(key, [search([`failed turn [case:${key}]`]), { status: 500 }, { tool: "count_logs", args: { filters: {} } }, { text: "answered." }]);
	result = await turn(`this one fails [case:${key}]`);
	row("usage failed turn ends error", result.terminal?.type === "error", JSON.stringify(result.terminal).slice(0, 200));
	row("usage failed turn names its thread", Boolean(result.terminal?.conversation_id));
	await expectSpend("usage failed turn", key, result.terminal, 1, 1);
	const next = await turn(`try again [case:${key}]`, { conversation_id: result.terminal?.conversation_id });
	row("usage failed turn thread continues", next.terminal?.type === "done" && next.terminal?.conversation_id === result.terminal?.conversation_id, JSON.stringify(next.terminal).slice(0, 200));

	// Two turns at once spend on their own totals.
	const [a, b] = [`${tag}-usage-a`, `${tag}-usage-b`];
	await script(a, [search([`one [case:${a}]`]), { text: "a." }]);
	await script(b, [search([`one [case:${b}]`, `two [case:${b}]`, `three [case:${b}]`]), { text: "b." }]);
	const [ra, rb] = await Promise.all([turn(`first [case:${a}]`), turn(`second [case:${b}]`)]);
	const want = (embeddings) => 2 * CHAT_TOKENS + embeddings * EMBED_TOKENS;
	row("usage concurrent turns do not mix", ra.terminal?.usage?.total_tokens === want(1) && rb.terminal?.usage?.total_tokens === want(3), `${ra.terminal?.usage?.total_tokens}, ${rb.terminal?.usage?.total_tokens}`);
}

async function checkAnswers() {
	// The fence the fork's own build would emit. A real code fence stays.
	let key = `${tag}-link`;
	const link = "https://github.com/capsohq/bifrost/issues/new?title=[Warp]+x&labels=enhancement";
	await script(key, [{ tool: "count_logs", args: { filters: {} } }, { text: `I cannot reach that.\n\n\`\`\`text\n${link}\n\`\`\`\n` }]);
	const body = await answer(`file it [case:${key}]`);
	row("link fork tracker unfenced", body?.answer?.includes(`](${link})`) && !body?.answer?.includes("```"), body?.answer);

	key = `${tag}-question`;
	await script(key, [{ text: "Let me narrow this down.", tool: "ask_user", args: { question: "Which time range?", kind: "time_range", options: [{ label: "Last 7 days", hint: "-7d" }, { label: "Today", hint: "today" }] } }]);
	const asked = await turn(`how many requests failed? [case:${key}]`);
	const posed = asked.frames.find((frame) => frame.type === "question");
	row("question keeps its options", asked.terminal?.finish_reason === "question" && posed?.question?.options?.length === 2, JSON.stringify(asked.terminal).slice(0, 200));
	const message = (await stored(asked.terminal?.conversation_id)).at(-1);
	row("question stored with its options", message?.content === "Which time range?" && message?.question?.options?.[0]?.hint === "-7d", JSON.stringify(message).slice(0, 200));
}

async function checkConfig() {
	const [a, b, c, d, e] = ["Move", "Back", "Cleared", "Failed", "Keep"].map(ns);
	let res = await api("PUT", "/api/warp/config", spaceInput(a));
	if (res.status !== 200) setupFailure(`config ${res.status} ${res.text.slice(0, 300)}`);
	res = await api("PUT", "/api/warp/config", spaceInput(b));
	row("config moves to a new namespace", res.status === 200, res.text.slice(0, 200));

	res = await api("PUT", "/api/warp/config", spaceInput(a));
	row("config refuses a retired namespace", res.status === 400 && res.text.includes("was retired"), `${res.status} ${res.text.slice(0, 200)}`);

	res = await api("PUT", "/api/warp/config", spaceInput(b, { enabled: false, embedding_model: "" }));
	row("config refuses clearing in place", res.status === 400 && res.text.includes("is cleared"), `${res.status} ${res.text.slice(0, 200)}`);

	res = await api("PUT", "/api/warp/config", spaceInput(c, { enabled: false, embedding_model: "" }));
	row("config clears onto a new namespace", res.status === 200, `${res.status} ${res.text.slice(0, 200)}`);
	const retired = await configSql("SELECT retired_log_vector_store_namespaces FROM warp_config");
	row("config retires what clearing left", retired.includes(b), retired.slice(0, 300));
	res = await api("PUT", "/api/warp/config", spaceInput(b, { embedding_model: "wire-embed" }));
	row("config refuses moving back after clearing", res.status === 400, `${res.status} ${res.text.slice(0, 200)}`);

	// A write the store refuses, after the namespace was created for it.
	const refuse = configDb.startsWith("sqlite:")
		? [`CREATE TRIGGER wire_refuse_i BEFORE INSERT ON warp_config WHEN NEW.model = 'wire-refused' BEGIN SELECT RAISE(ABORT, 'refused'); END;`,
			`CREATE TRIGGER wire_refuse_u BEFORE UPDATE ON warp_config WHEN NEW.model = 'wire-refused' BEGIN SELECT RAISE(ABORT, 'refused'); END;`]
		: [`CREATE FUNCTION wire_refuse() RETURNS trigger AS $$ BEGIN IF NEW.model = 'wire-refused' THEN RAISE EXCEPTION 'refused'; END IF; RETURN NEW; END $$ LANGUAGE plpgsql;`,
			`CREATE TRIGGER wire_refuse BEFORE INSERT OR UPDATE ON warp_config FOR EACH ROW EXECUTE FUNCTION wire_refuse();`];
	for (const statement of refuse) await configSql(statement);
	res = await api("PUT", "/api/warp/config", spaceInput(d, { model: "wire-refused" }));
	row("config failed save reports the failure", res.status >= 500, `${res.status} ${res.text.slice(0, 200)}`);
	row("config failed save keeps its namespace", await namespaceExists(d));
	res = await api("PUT", "/api/warp/config", spaceInput(d));
	row("config retry reuses the namespace", res.status === 200 && await namespaceExists(d), `${res.status} ${res.text.slice(0, 200)}`);
	row("config foreign namespace untouched", await namespaceExists(a) && await namespaceExists(b));
	res = await api("PUT", "/api/warp/config", spaceInput(e));
	if (res.status !== 200) setupFailure(`config ${res.status} ${res.text.slice(0, 300)}`);
}

async function checkCustomProviders() {
	let key = `${tag}-custom-open`;
	let res = await api("PUT", "/api/warp/config", spaceInput(ns("Open"), { embedding_provider: "wire-open" }));
	if (res.status !== 200) setupFailure(`config ${res.status} ${res.text.slice(0, 300)}`);
	await script(key, [search([`unrestricted [case:${key}]`]), { text: "done." }]);
	let result = await turn(`search [case:${key}]`);
	let call = result.frames.find((frame) => frame.type === "tool_call_end");
	row("custom unrestricted provider embeds", call && !call.failed, JSON.stringify(call).slice(0, 300));

	key = `${tag}-custom-chat`;
	res = await api("PUT", "/api/warp/config", spaceInput(ns("Chat"), { embedding_provider: "wire-chatonly" }));
	if (res.status !== 200) setupFailure(`config ${res.status} ${res.text.slice(0, 300)}`);
	await script(key, [search([`restricted [case:${key}]`]), { text: "done." }]);
	const before = (await fx("/__embeddings")).inputs.length;
	result = await turn(`search [case:${key}]`);
	call = result.frames.find((frame) => frame.type === "tool_call_end");
	row("custom restricted provider refused", call?.failed === true && (await fx("/__embeddings")).inputs.length === before, JSON.stringify(call).slice(0, 300));

	key = `${tag}-custom-key`;
	res = await api("PUT", "/api/warp/config", spaceInput(ns("Allow"), { embedding_model: "wire-embed-blocked" }));
	if (res.status !== 200) setupFailure(`config ${res.status} ${res.text.slice(0, 300)}`);
	await script(key, [search([`blocked [case:${key}]`]), { text: "done." }]);
	result = await turn(`search [case:${key}]`);
	call = result.frames.find((frame) => frame.type === "tool_call_end");
	row("key allowlist refuses its model", call?.failed === true, JSON.stringify(call).slice(0, 300));
	res = await api("PUT", "/api/warp/config", spaceInput(ns("Live")));
	if (res.status !== 200) setupFailure(`config ${res.status} ${res.text.slice(0, 300)}`);
}

const hour = (ms) => Math.floor(ms / 3_600_000) * 3_600_000;
const DAY = 86_400_000;

async function checkCharts() {
	// openai: 60 successes, 20 errors, 20 cancelled, 10 in flight. azure: 10 and 10, on the same model.
	const at = hour(Date.now() - 3 * DAY) + 1_800_000;
	const model = `wire-${tag}`;
	const team = `team-${tag}`;
	const logs = [];
	const add = (provider, status, count) => { for (let i = 0; i < count; i++) logs.push(logRow(`chart-${tag}-${logs.length}`, at, { provider, status, team_id: team, cost: 0.01, tokens: 10 })); };
	add("openai", "success", 60); add("openai", "error", 20); add("openai", "cancelled", 20); add("openai", "processing", 10); add("azure", "success", 10); add("azure", "error", 10);
	// Every request failed and none cost anything.
	const downModel = `wire-down-${tag}`;
	for (let i = 0; i < 5; i++) logs.push(logRow(`chart-${tag}-down-${i}`, at, { provider: "cerebras", model: downModel, status: "error" }));
	await seed(logs);
	if (backend === "postgres") await configSql("REFRESH MATERIALIZED VIEW mv_logs_hourly");

	const chart = async (label, args) => {
		const key = `${tag}-chart-${label}`;
		await script(key, [{ tool: "render_chart", args: { kind: "bar", title: "chart", ...args } }, { text: "drawn." }]);
		await turn(`chart it [case:${key}]`);
		const output = await lastOutput(key);
		return output?.points ?? output;
	};
	// A whole week reads the hourly view on Postgres; two hours reads the table everywhere.
	for (const [window, filters] of [["week", { start_time: "-8d", models: [model] }], ["hours", { start_time: new Date(at - 1_800_000).toISOString(), end_time: new Date(at + 1_800_000).toISOString(), models: [model] }]]) {
		const byValue = (points) => Object.fromEntries((Array.isArray(points) ? points : []).map((point) => [point.x, point.y]));
		let points = await chart(`model-${window}`, { metric: "error_rate", group: "model", filters });
		row(`chart ${window} one bar per model`, Array.isArray(points) && points.length === 1 && points[0].y === 25, JSON.stringify(points).slice(0, 200));
		points = await chart(`provider-${window}`, { metric: "error_rate", group: "provider", filters });
		row(`chart ${window} provider rates`, byValue(points).openai === 20 && byValue(points).azure === 50, JSON.stringify(points).slice(0, 200));
		points = await chart(`team-${window}`, { metric: "error_rate", group: "team", filters });
		row(`chart ${window} team agrees with model`, byValue(points)[team] === 25, JSON.stringify(points).slice(0, 200));
		points = await chart(`success-${window}`, { metric: "error_rate", group: "provider", filters: { ...filters, status: ["success"] } });
		row(`chart ${window} excluded errors are not counted`, byValue(points).openai === 0 && byValue(points).azure === 0, JSON.stringify(points).slice(0, 200));
		points = await chart(`down-${window}`, { metric: "error_rate", group: "provider", filters: { ...filters, models: [downModel] } });
		row(`chart ${window} a provider with no spend still has a bar`, byValue(points).cerebras === 100, JSON.stringify(points).slice(0, 200));
		points = await chart(`cost-${window}`, { metric: "cost", group: "model", filters });
		row(`chart ${window} cost sums across providers`, Array.isArray(points) && points.length === 1 && close(points[0].y, 1.2), JSON.stringify(points).slice(0, 200));
	}
}

async function checkMetrics() {
	const start = hour(Date.now() - 2 * DAY);
	const model = `wire-m-${tag}`;
	await seed([
		logRow(`m-${tag}-before`, start + 600_000, { model, cost: 1, tokens: 100 }),
		logRow(`m-${tag}-inside`, start + 2_400_000, { model, cost: 2, tokens: 200 }),
		logRow(`m-${tag}-after`, start + 6_000_000, { model, cost: 4, tokens: 400 }),
	]);
	if (backend === "postgres") await configSql("REFRESH MATERIALIZED VIEW mv_logs_hourly");
	const key = `${tag}-metrics`;
	await script(key, [{ tool: "query_metrics", args: { metrics: ["cost", "tokens"], filters: { start_time: new Date(start + 1_800_000).toISOString(), end_time: new Date(start + 5_400_000).toISOString(), models: [model] } } }, { text: "done." }]);
	await turn(`how much? [case:${key}]`);
	const output = await lastOutput(key);
	row("metrics total is the window's own", close(output?.cost?.total_cost?.total ?? 0, 2) && output?.tokens?.total_tokens?.total === 200, JSON.stringify(output?.cost?.total_cost).slice(0, 200));
}

async function checkTrace() {
	const at = hour(Date.now() - DAY);
	const [primary, hop, session] = [`tr-${tag}-primary`, `tr-${tag}-hop`, `session-${tag}`];
	await seed([
		logRow(primary, at, { status: "error", parent_request_id: session }),
		logRow(hop, at + 1000, {
			status: "error", fallback_index: 1, parent_request_id: primary,
			error_details: { status_code: 529, error: { type: "overloaded_error", message: "overloaded" } },
			attempt_trail: [{ attempt: 1, key_name: "primary", fail_reason: "overloaded" }],
		}),
	]);
	const key = `${tag}-trace`;
	await script(key, [{ tool: "get_request_trace", args: { log_id: primary } }, { text: "traced." }]);
	await turn(`trace it [case:${key}]`);
	const output = await lastOutput(key);
	row("trace session parent is not a root", output?.root_id === primary, JSON.stringify(output).slice(0, 200));
	const node = output?.nodes?.find?.((entry) => entry.id === hop);
	row("trace hop carries its diagnostics", node?.error?.type === "overloaded_error" && node?.attempts?.length === 1, String(JSON.stringify(node)).slice(0, 300));
}

const waitForBackfill = async () => {
	for (let i = 0; i < 300; i++) {
		const res = await api("GET", "/api/warp/log-index/backfill/status");
		if (["completed", "failed", "cancelled"].includes(res.json?.status)) return res.json;
		await new Promise((resolve) => setTimeout(resolve, 100));
	}
	return null;
};
const startBackfill = (start, end) => api("POST", "/api/warp/log-index/backfill", { start_time: new Date(start).toISOString(), end_time: new Date(end).toISOString() });

async function checkBackfill() {
	// More rows than a page, one timestamp. Retention takes a consumed row while the first page is open,
	// and a new row lands at the same instant.
	const start = hour(Date.now() - 10 * DAY);
	const tied = start + 1_800_123;
	const id = (index) => rowId("backfill", index);
	const logs = Array.from({ length: BATCH + 2 }, (_, index) => logRow(id(index), tied));
	await seed(logs);
	await fx("/__hold", { match: id(BATCH - 1) });
	let res = await startBackfill(start, start + 3_600_000);
	if (res.status !== 200 && res.status !== 201 && res.status !== 202) setupFailure(`backfill ${res.status} ${res.text.slice(0, 300)}`);
	let holding = false;
	for (let i = 0; i < 300 && !holding; i++) {
		holding = (await fx("/__held")).held;
		if (!holding) await new Promise((resolve) => setTimeout(resolve, 100));
	}
	if (!holding) setupFailure("the first backfill page never reached its last row");
	await unseed(id(0));
	await seed([logRow(id(900), tied)]);
	await fx("/__release");
	let job = await waitForBackfill();
	const seen = (await fx("/__embeddings")).inputs;
	const times = (rowId) => seen.filter((text) => text.includes(rowId)).length;
	row("backfill finishes", job?.status === "completed", JSON.stringify(job).slice(0, 300));
	row("backfill indexes every survivor", times(id(BATCH)) === 1 && times(id(BATCH + 1)) === 1, `${id(BATCH)} ${times(id(BATCH))}, ${id(BATCH + 1)} ${times(id(BATCH + 1))}`);
	row("backfill indexes a row that arrived behind it", times(id(900)) === 1, `${times(id(900))}`);
	row("backfill scans nothing twice", logs.every((entry) => times(entry.id) === 1) && job?.scanned === BATCH + 3, `scanned ${job?.scanned}`);

	// A job an earlier release left: its checkpoint carries an offset and no row.
	const legacyStart = hour(Date.now() - 12 * DAY);
	const legacyTied = legacyStart + 1_800_456;
	const legacyId = (index) => rowId("legacy", index);
	await seed(Array.from({ length: 5 }, (_, index) => logRow(legacyId(index), legacyTied)));
	res = await startBackfill(legacyStart, legacyStart + 3_600_000);
	await waitForBackfill();
	const stored = await configSql("SELECT id || '|' || metadata FROM sidekiq ORDER BY created_at DESC LIMIT 1");
	const separator = stored.indexOf("|");
	const meta = JSON.parse(stored.slice(separator + 1));
	Object.assign(meta, { cursor_time: new Date(legacyTied).toISOString(), cursor_offset: 2, scanned: 2, indexed: 2 });
	delete meta.cursor_id;
	await configSql(`UPDATE sidekiq SET status = 'failed', metadata = '${JSON.stringify(meta).replaceAll("'", "''")}' WHERE id = '${stored.slice(0, separator)}'`);
	const before = (await fx("/__embeddings")).inputs.length;
	res = await startBackfill(legacyStart, legacyStart + 3_600_000);
	job = await waitForBackfill();
	const resumed = (await fx("/__embeddings")).inputs.slice(before);
	row("backfill resumes an offset checkpoint", job?.status === "completed" && job?.scanned === 5 && resumed.length === 3, `status ${job?.status}, scanned ${job?.scanned}, embedded ${resumed.length}`);
}

async function checkSearch() {
	const start = hour(Date.now() - 14 * DAY);
	const [matching, other] = [rowId("search", 1), rowId("search", 2)];
	const conversation = (question, reply) => ({ input_history: [{ role: "user", content: question }], output_message: { role: "assistant", content: reply } });
	await seed([
		logRow(matching, start + 60_000, { project_id: "project-a", tokens: 900, content_summary: "Where is my order 1182?", ...conversation("Where is my order 1182?", "I have issued a refund of $20 to your card.") }),
		logRow(other, start + 120_000, { project_id: "project-b", tokens: 10, content_summary: "Hello", ...conversation("Hello", "Hi there.") }),
	]);
	await startBackfill(start, start + 3_600_000);
	await waitForBackfill();
	const window = { start_time: new Date(start).toISOString(), end_time: new Date(start + 3_600_000).toISOString() };

	let key = `${tag}-search-project`;
	await script(key, [search([`refunds [case:${key}]`], { ...window, project_ids: ["project-a"] }), { text: "done." }]);
	await turn(`search [case:${key}]`);
	let output = await lastOutput(key);
	row("search enforces an accepted filter", output?.rows?.length === 1 && output.rows[0].id === matching, JSON.stringify(output?.rows?.map((r) => r.id)));
	row("search shows both sides", output?.rows?.[0]?.content?.includes("order 1182") && output?.rows?.[0]?.content?.includes("refund of $20"), output?.rows?.[0]?.content);

	key = `${tag}-search-refused`;
	await script(key, [search([`refunds [case:${key}]`], { ...window, tool_call_names: ["lookup_order"] }), { text: "done." }]);
	const before = (await fx("/__embeddings")).inputs.length;
	await turn(`search [case:${key}]`);
	output = await lastOutput(key);
	row("search refuses a filter it cannot apply", JSON.stringify(output).includes("cannot filter by tool_call_names") && (await fx("/__embeddings")).inputs.length === before, JSON.stringify(output).slice(0, 200));
}

async function main() {
	// warp-wire.config.json turns the flag on and locks it.
	const flags = await api("GET", "/api/feature-flags");
	if (!flags.json?.flags?.some((flag) => flag.id === "warp" && flag.enabled)) setupFailure(`feature flag ${flags.status} ${flags.text.slice(0, 200)}`);
	await checkConfig();
	await checkCustomProviders();
	await checkUsage();
	await checkAnswers();
	await checkCharts();
	await checkMetrics();
	await checkTrace();
	await checkBackfill();
	await checkSearch();
	fs.writeFileSync(reportPath, JSON.stringify({ rows }, null, 1));
	const failed = rows.filter((entry) => !entry.ok).length;
	console.log(`${rows.length} rows, ${failed} failed`);
	process.exit(failed === 0 ? 0 : 1);
}

main().catch((error) => setupFailure(error.stack ?? String(error)));
