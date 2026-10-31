#!/usr/bin/env node
// Wire checks for the gateway's request, bootstrap and export bounds, against a running gateway
// and the provider error fixture (TXE-5542). A collection cannot send these: they need compressed
// request bodies, raw WebSocket frames, a real git client and a look at the gateway's own disk and
// peak memory. The wildcard rows repeat folder 275 of the provider harness on this config. Start the gateway from provider_config/bounds-wire.config.json with TMPDIR set.
//
//   BOUNDS_BASE_URL     the gateway, http://127.0.0.1:<port>
//   BOUNDS_FIXTURE_URL  the fixture, http://127.0.0.1:8791
//   BOUNDS_SETUP_TOKEN  the gateway's setup token
//   BOUNDS_GATEWAY_TMP  the gateway's TMPDIR, where it exports skill repositories
//   BOUNDS_GATEWAY_PID  the gateway's pid, for its peak memory
//   BOUNDS_REPORT       where to write the rows as JSON
//
// Exit 0: every row passed. Exit 1: a row failed. Exit 2: setup failed, so the run proves nothing.
import { execFile } from "node:child_process";
import { randomBytes } from "node:crypto";
import fs from "node:fs";
import http from "node:http";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import zlib from "node:zlib";

const env = (name) => {
	const value = process.env[name];
	if (!value) { console.error(`${name} is required`); process.exit(2); }
	return value;
};
const base = new URL(env("BOUNDS_BASE_URL"));
const fixture = env("BOUNDS_FIXTURE_URL");
const setupToken = env("BOUNDS_SETUP_TOKEN");
const gatewayTmp = env("BOUNDS_GATEWAY_TMP");
const gatewayPid = env("BOUNDS_GATEWAY_PID");
const reportPath = env("BOUNDS_REPORT");

const BODY_LIMIT = 1024 * 1024; // provider_config/bounds-wire.config.json: max_request_body_size_mb
const BOOTSTRAP_LIMIT = 1 << 20; // realtimeTranscriptionBootstrapMaxBytes
const EXPORT_CAP = 32; // skillsGitRepoCacheMaxEntries
const SERVE_SLOTS = 4; // skillsCorpusServeConcurrency
const EXPORT_TTL_MS = 60_000; // skillsGitRepoCacheTTL
const SKILLS = EXPORT_CAP + 6;
const execGit = promisify(execFile);

const rows = [];
const row = (name, ok, detail = "") => {
	rows.push({ name, ok: !!ok, detail: String(detail).slice(0, 300) });
	console.log(`${ok ? "ok    " : "FAILED"} ${name}${detail === "" ? "" : ` :: ${String(detail).slice(0, 300)}`}`);
};
const setupFailure = (message) => { console.error(`setup failed: ${message}`); process.exit(2); };

const request = (method, url, { headers = {}, body, chunked = false } = {}) => new Promise((resolve, reject) => {
	const req = http.request(url, { method, headers: { ...headers, ...(body !== undefined && !chunked ? { "Content-Length": Buffer.byteLength(body) } : {}) } }, (res) => {
		const chunks = [];
		res.on("data", (c) => chunks.push(c));
		res.on("end", () => resolve({ status: res.statusCode, body: Buffer.concat(chunks).toString("utf8") }));
	});
	req.on("error", reject);
	req.setTimeout(30_000, () => req.destroy(new Error("request timed out")));
	if (body === undefined) return req.end();
	if (!chunked) return req.end(body);
	// Written in two parts so the gateway sees a chunked body of unknown length.
	const middle = Math.floor(body.length / 2);
	req.write(body.subarray(0, middle));
	req.end(body.subarray(middle));
	return undefined;
});
const api = (method, apiPath, body) => request(method, new URL(apiPath, base), {
	headers: { "X-Bifrost-Setup-Token": setupToken, "Content-Type": "application/json" },
	body: body === undefined ? undefined : JSON.stringify(body),
});
const wireCalls = async () => JSON.parse((await request("GET", `${fixture}/__wire`)).body).data[0].calls;
const resetFixture = () => request("POST", `${fixture}/__reset`);

// A chat body of exactly size bytes. The padding is random base64, so the compressed body stays
// large enough that the expansion allowance is the cap and not a multiple of the compressed size.
const chatBody = (size) => {
	const shell = (content) => JSON.stringify({ model: "openai/upstream-ok", messages: [{ role: "user", content }] });
	const padding = size - shell("").length;
	return Buffer.from(shell(randomBytes(padding).toString("base64").slice(0, padding)));
};

async function checkBodyLimit() {
	const encoders = { gzip: zlib.gzipSync, deflate: zlib.deflateSync, br: zlib.brotliCompressSync, zstd: zlib.zstdCompressSync };
	for (const [encoding, encode] of Object.entries(encoders)) {
		for (const chunked of [false, true]) {
			for (const [label, size, fits] of [["limit-1", BODY_LIMIT - 1, true], ["limit", BODY_LIMIT, true], ["limit+1", BODY_LIMIT + 1, false]]) {
				const framing = chunked ? "chunked" : "sized";
				const res = await request("POST", new URL("/v1/chat/completions", base), {
					headers: { "Content-Type": "application/json", "Content-Encoding": encoding },
					body: encode(chatBody(size)),
					chunked,
				});
				row(`body ${encoding} ${framing} ${label}`, fits ? res.status === 200 : res.status === 413, `status ${res.status}`);
			}
		}
	}
}

// One raw WebSocket client: masked frames out, and text and close frames in. The zero mask leaves
// the payload as it is, so a large frame costs no copy.
function openRealtime() {
	return new Promise((resolve, reject) => {
		const socket = net.connect(Number(base.port), base.hostname);
		const conn = { socket, texts: [], closeCode: null, ended: false };
		let buffer = Buffer.alloc(0);
		let upgraded = false;
		socket.on("error", () => {});
		socket.on("close", () => { conn.ended = true; if (!upgraded) reject(new Error("closed before the upgrade")); });
		socket.on("connect", () => socket.write([
			"GET /v1/realtime?intent=transcription HTTP/1.1", `Host: ${base.host}`, "Upgrade: websocket", "Connection: Upgrade",
			`Sec-WebSocket-Key: ${randomBytes(16).toString("base64")}`, "Sec-WebSocket-Version: 13", "", "",
		].join("\r\n")));
		socket.on("data", (data) => {
			buffer = Buffer.concat([buffer, data]);
			if (!upgraded) {
				const end = buffer.indexOf("\r\n\r\n");
				if (end < 0) return;
				if (!buffer.subarray(0, end).toString().startsWith("HTTP/1.1 101")) { reject(new Error("upgrade refused")); socket.destroy(); return; }
				upgraded = true;
				buffer = buffer.subarray(end + 4);
				resolve(conn);
			}
			while (buffer.length >= 2) {
				const opcode = buffer[0] & 0x0f;
				let length = buffer[1] & 0x7f;
				let offset = 2;
				if (length === 126) { if (buffer.length < 4) return; length = buffer.readUInt16BE(2); offset = 4; }
				else if (length === 127) { if (buffer.length < 10) return; length = Number(buffer.readBigUInt64BE(2)); offset = 10; }
				if (buffer.length < offset + length) return;
				const payload = buffer.subarray(offset, offset + length);
				if (opcode === 1) conn.texts.push(payload.toString("utf8"));
				if (opcode === 8) conn.closeCode = payload.length >= 2 ? payload.readUInt16BE(0) : 1005;
				buffer = buffer.subarray(offset + length);
			}
		});
	});
}
const frameHeader = (length) => {
	const header = Buffer.alloc(14);
	header[0] = 0x81; // FIN, text
	header[1] = 0x80 | 127; // masked, 64-bit length
	header.writeBigUInt64BE(BigInt(length), 2);
	return header; // the last four bytes are the zero mask
};
const sendFrame = (conn, payload) => { conn.socket.write(frameHeader(payload.length)); conn.socket.write(payload); };
const textFrame = (size, withModel) => {
	const prefix = withModel
		? '{"type":"session.update","session":{"audio":{"input":{"transcription":{"model":"openai/gpt-4o-transcribe"}}}},"padding":"'
		: '{"type":"input_audio_buffer.append","audio":"';
	const suffix = '"}';
	return Buffer.from(prefix + "A".repeat(size - prefix.length - suffix.length) + suffix);
};
const settled = async (conn, predicate, ms) => {
	const deadline = Date.now() + ms;
	while (Date.now() < deadline && !predicate()) await new Promise((r) => setTimeout(r, 20));
	return predicate();
};
const peakMemoryKiB = () => {
	const match = fs.readFileSync(`/proc/${gatewayPid}/status`, "utf8").match(/VmHWM:\s+(\d+) kB/);
	if (!match) setupFailure("no VmHWM for the gateway");
	return Number(match[1]);
};

async function checkBootstrap() {
	const REFUSED = 1009;
	// The refusal comes from the frame header. With nothing more to send the client reads close 1009;
	// a client still writing its payload is reset, which can drop that close frame from its receive
	// buffer, so there the refusal is 1009 or a reset with nothing read. Reading the whole message and
	// answering in text, as an after-the-fact check does, is what fails.
	const refused = async (name, send, { payloadInFlight }) => {
		const conn = await openRealtime();
		send(conn);
		const closed = await settled(conn, () => conn.closeCode !== null || conn.ended || conn.texts.length > 0, 3000);
		const reset = payloadInFlight && conn.ended && conn.texts.length === 0;
		row(name, closed && (conn.closeCode === REFUSED || reset), `close ${conn.closeCode}, texts ${conn.texts.length}, ended ${conn.ended}`);
		conn.socket.destroy();
	};
	await refused("bootstrap frame header over the allowance, no payload sent", (c) => c.socket.write(frameHeader(2 ** 40)), { payloadInFlight: false });
	await refused("bootstrap one message limit+1", (c) => sendFrame(c, textFrame(BOOTSTRAP_LIMIT + 1, true)), { payloadInFlight: true });
	await refused("bootstrap two reads limit+1", (c) => {
		sendFrame(c, textFrame(400 << 10, false));
		sendFrame(c, textFrame(BOOTSTRAP_LIMIT - (400 << 10) + 1, true));
	}, { payloadInFlight: true });

	// At the limit the model is read and bootstrap hands over; the fixture has no realtime endpoint,
	// so what ends the session is the upstream, not the allowance.
	const atLimit = await openRealtime();
	sendFrame(atLimit, textFrame(400 << 10, false));
	sendFrame(atLimit, textFrame(BOOTSTRAP_LIMIT - (400 << 10), true));
	await settled(atLimit, () => atLimit.texts.length > 0 || atLimit.closeCode !== null, 15_000);
	row("bootstrap two reads limit reaches the upstream step",
		atLimit.closeCode !== REFUSED && atLimit.texts.length > 0 && !atLimit.texts.join("").includes("exceeded 1 MiB"),
		`close ${atLimit.closeCode}, text ${atLimit.texts.join("").slice(0, 160)}`);
	atLimit.socket.destroy();

	const spent = await openRealtime();
	sendFrame(spent, textFrame(BOOTSTRAP_LIMIT, false)); // and nothing more: the spent allowance ends it, not a later frame
	await settled(spent, () => spent.texts.length > 0 || spent.closeCode !== null, 5000);
	row("bootstrap spent allowance reports the limit", spent.texts.join("").includes("exceeded 1 MiB"), spent.texts.join("").slice(0, 160));
	spent.socket.destroy();

	// What the refusal saves: unadmitted connections each sending a large message.
	const before = peakMemoryKiB();
	const large = textFrame(256 << 20, false);
	const conns = await Promise.all([openRealtime(), openRealtime()]);
	for (const conn of conns) sendFrame(conn, large);
	await Promise.all(conns.map((conn) => settled(conn, () => conn.closeCode !== null || conn.texts.length > 0 || conn.ended, 20_000)));
	for (const conn of conns) conn.socket.destroy();
	const growth = (peakMemoryKiB() - before) / 1024;
	row("bootstrap 256 MiB messages do not reach gateway memory", growth < 64, `peak grew ${growth.toFixed(1)} MiB`);
}

const exportDirs = () => fs.readdirSync(gatewayTmp).filter((name) => name.startsWith("bifrost-skill-"));
const skillName = (i) => `bounds-skill-${i}`;
const refs = (i) => request("GET", new URL(`/api/skills/serve/claude-code/plugins/bifrost-${skillName(i)}/info/refs?service=git-upload-pack`, base));
const fetchRefs = async (i) => {
	for (let attempt = 0; ; attempt++) {
		const res = await refs(i);
		if (res.status !== 503 || attempt === 40) return res;
		// A 503 with Retry-After is the gateway's answer when its four serving slots are taken, so
		// a client retries it; it is protocol behaviour, not a retried test. Anything else is final.
		await new Promise((r) => setTimeout(r, 50));
	}
};

async function checkSkillExports() {
	for (let i = 0; i < SKILLS; i++) {
		const res = await api("POST", "/api/skills", { name: skillName(i), description: "Bounds fixture skill", skill_md_body: `Bounds fixture ${i}.`, version: "1.0.0" });
		if (res.status < 200 || res.status >= 300) setupFailure(`create skill: ${res.status} ${res.body.slice(0, 200)}`);
	}
	for (let i = 0; i < SKILLS; i++) {
		const res = await fetchRefs(i);
		if (res.status !== 200) { row("skills sequential fetches served", false, `skill ${i}: status ${res.status}`); break; }
		if (i === SKILLS - 1) row("skills sequential fetches served", true);
	}
	row("skills exports stay within the cap", exportDirs().length <= EXPORT_CAP, `${exportDirs().length} directories`);

	// Real clones while other repositories churn the cache: a clone needs its export for two
	// requests in a row, so an export removed under it fails the clone.
	const cloneRoot = fs.mkdtempSync(path.join(os.tmpdir(), "bounds-clones-"));
	let peak = 0;
	let churning = true;
	const churn = (async () => {
		for (let i = 0; churning; i++) {
			const res = await fetchRefs(i % SKILLS);
			if (res.status !== 200) throw new Error(`churn fetch: ${res.status}`);
			peak = Math.max(peak, exportDirs().length);
		}
	})();
	const clones = await Promise.allSettled([0, 1, 2].map((n) => execGit("git", [
		"-c", "protocol.version=2", "clone", "--quiet",
		new URL(`/api/skills/serve/claude-code/plugins/bifrost-${skillName(n)}`, base).href, path.join(cloneRoot, String(n)),
	], { timeout: 30_000, env: { PATH: process.env.PATH, HOME: cloneRoot, GIT_TERMINAL_PROMPT: "0" } })));
	churning = false;
	const churned = await churn.then(() => "", (err) => err.message);
	const cloned = clones.every((c, n) => c.status === "fulfilled" && fs.existsSync(path.join(cloneRoot, String(n), ".git")));
	row("skills clones survive cache churn", cloned, clones.map((c) => (c.status === "fulfilled" ? "cloned" : String(c.reason.message).slice(0, 90))).join("; "));
	row("skills churn fetches served", churned === "", churned);
	row("skills exports stay within the cap under churn", peak <= EXPORT_CAP + SERVE_SLOTS, `peak ${peak} directories`);
	fs.rmSync(cloneRoot, { recursive: true, force: true });

	await new Promise((r) => setTimeout(r, EXPORT_TTL_MS + 2000));
	const res = await fetchRefs(0);
	row("skills idle exports of other repositories expire", res.status === 200 && exportDirs().length === 1, `status ${res.status}, ${exportDirs().length} directories`);
}

async function checkWildcardCatalog() {
	const created = await api("POST", "/api/governance/virtual-keys", {
		name: "bounds-wildcard",
		provider_configs: [{ provider: "fireworks", weight: 1, allowed_models: ["*"], key_ids: ["*"] }],
	});
	if (created.status < 200 || created.status >= 300) setupFailure(`create virtual key: ${created.status} ${created.body.slice(0, 200)}`);
	const parsed = JSON.parse(created.body);
	const vk = parsed.virtual_key?.value;
	if (!vk) setupFailure("no virtual key value");
	const chat = (model) => request("POST", new URL("/v1/chat/completions", base), {
		headers: { "Content-Type": "application/json", "x-bf-vk": vk },
		body: JSON.stringify({ model: `fireworks/${model}`, messages: [{ role: "user", content: "hi" }] }),
	});
	const called = async (model) => (await wireCalls()).some((call) => call.path.startsWith("/pp/bounds/") && JSON.parse(call.body).model.endsWith(model));

	await resetFixture();
	const known = await chat("accounts/fixture/models/known");
	row("wildcard datasheet model reaches the provider", known.status === 200 && await called("models/known"), `status ${known.status}`);
	const unlisted = await chat("accounts/fixture/models/unlisted");
	row("wildcard uncatalogued model reaches the provider", unlisted.status === 200 && await called("models/unlisted"), `status ${unlisted.status}`);
	const blocked = await chat("accounts/fixture/models/blocked");
	const reached = await called("models/blocked");
	row("wildcard model the key blacklists is refused by governance", blocked.status === 403 && blocked.body.includes("not allowed") && !reached, `status ${blocked.status}, reached ${reached}, body ${blocked.body.slice(0, 160)}`);
}

await checkBodyLimit();
await checkBootstrap();
await checkSkillExports();
await checkWildcardCatalog();
fs.writeFileSync(reportPath, `${JSON.stringify({ rows }, null, 2)}\n`);
const failed = rows.filter((r) => !r.ok);
console.log(`${rows.length} rows, ${failed.length} failed`);
process.exit(failed.length ? 1 : 0);
