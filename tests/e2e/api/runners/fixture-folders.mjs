// Runs every provider-harness folder that a local fixture can answer, offline, and fails on a row
// that fails (TXE-7285).
//
// Why. Most harness folders call real vendors and run only in the release's live test-core job,
// which every fork merge skips (--skip-core-test). Fixture folders need no vendor: a local fixture
// stands in for the upstream. But nothing ran them either, so row 161.11 kept expecting 200 for
// Vertex service_tier priority on a regional key for two days after TXE-6535 made the gateway
// refuse it with 400, and 9 more rows were found failing on main the day this runner was added.
// The `harness fixture folders` job in go-unit-tests.yml runs this, and the required
// `Go unit tests (offline)` check needs that job.
//
//   node fixture-folders.mjs plan
//       Classifies every folder file by the fixture variables it reads. Exits 1 when a folder reads
//       a fixture variable that no group below starts and fixture-folders.json does not declare
//       it in notRun, or when a notRun or quarantine entry is stale. Needs nothing running; the
//       offline suites run it through fixture-folders.test.mjs.
//   node fixture-folders.mjs run --gateway <bifrost-http binary> [--group <id>] [--work-dir <dir>]
//       For each group: starts its fixture and a gateway on free loopback ports, runs the group's
//       folders through newman, and stops both. Exits 1 on a failing row that is not quarantined,
//       a quarantined row that passed or did not run, or a group in which no row ran. Run it under
//       scripts/test-offline.sh, so no credential reaches the gateway, the fixtures or newman.
//
// Groups. A group is one fixture, the gateway config it needs, and the newman variables that
// switch its rows on. The fixture's default ports are rewritten to free ones in the gateway config
// and in the selected collection, so a run never collides with another on the same machine (except a
// group marked fixedPorts, whose rows assert a port number). A checked-in gateway config goes through
// writeCheckedInFixtureConfig (lib/tls-intercept-proxy.mjs), which refuses one with inference auth on
// and gives one that names no pricing datasheet two empty ones from a loopback server here.
// A folder belongs to every group whose variables it reads; a row that reads another group's
// switch skips itself there, as it does in a live run.
//
// fixture-folders.json holds the three exception lists: notRun (folders that use a fixture this
// runner does not start yet, with the reason and a ticket), quarantine (rows that fail on main, with
// a ticket) and skippedRows (rows that skip themselves in a fixture run on purpose, a live row in a
// fixture folder, with the reason). Every other row of a selected folder must be answered by some
// group: a row that skipped itself checked nothing. All three only shrink: an entry whose row
// passes, runs, or is gone fails the run until the entry is removed.

import { spawn } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import net from "node:net";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { listFolderFiles, PROVIDER_HARNESS_DIR } from "./lib/provider-harness-source.mjs";
import { freeLoopbackPort } from "./lib/loopback-port.mjs";
import { S3_BUCKET } from "./lib/s3-object-fixture.mjs";
import { remapLoopbackPorts, writeCheckedInFixtureConfig } from "./lib/tls-intercept-proxy.mjs";

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = resolve(HERE, "../../../..");
export const DATA_FILE = resolve(HERE, "../collections/fixture-folders.json");
const SETUP_TOKEN = "bifrost-e2e-setup-token"; // provider-error-fixture.config.json's setup_token

// A variable a folder reads to reach or switch on a fixture: providerErrorFixture,
// vertexEndpointFixtureUrl, a2aFixtureBase. A new fixture's variable matches too, so a folder
// that adds one fails `plan` until a group starts that fixture or notRun names the folder.
export const FIXTURE_VARIABLE = /\b[a-z][a-zA-Z0-9]*Fixture(?:Url|Base|StallProxyUrl)?\b/g;

const providerError = {
	variables: ["providerErrorFixture", "providerErrorFixtureUrl", "providerErrorFixtureStallProxyUrl"],
	ports: { main: 8791, stall: 8799, tls: 8793, tunnel: 8788 },
	fixture: (p) => ({
		script: "provider-error-fixture.mjs",
		args: [],
		env: {
			PROVIDER_ERROR_FIXTURE_PORT: p.main,
			PROVIDER_ERROR_FIXTURE_STALL_PORT: p.stall,
			PROVIDER_ERROR_FIXTURE_TLS_PORT: p.tls,
			PROVIDER_ERROR_FIXTURE_MEDIA_TUNNEL_PORT: p.tunnel,
		},
	}),
	config: "tests/e2e/api/provider_config/provider-error-fixture.config.json",
	vars: (p) => ({
		providerErrorFixture: "1",
		providerErrorFixtureUrl: `http://127.0.0.1:${p.main}`,
		providerErrorFixtureStallProxyUrl: `http://127.0.0.1:${p.stall}`,
		setupToken: SETUP_TOKEN,
	}),
};

// A group with `files` runs exactly those folder files, and no group without `files` runs them.
export const GROUPS = [
	{ id: "provider-error", ...providerError },
	{
		// Folders whose fixture rows are all tagged [PREVIEW]: the collection skips those unless
		// include_preview=1. Setting it for every provider-error folder would also switch on the live
		// preview rows other folders carry (416.20), so these run apart with it set.
		id: "provider-error-preview",
		...providerError,
		files: ["190-webrtc-setup-billing-preserves-incurred-usage-openai-local-f.json", "199-webrtc-event-filter-preserves-billing-openai-local-fixture.json"],
		vars: (p) => ({ ...providerError.vars(p), include_preview: "1" }),
	},
	{
		// Folder 185's routing rows create their own run-scoped providers on the fixture and assert
		// what the model catalog picks among them, so the gateway may serve no model of its own:
		// provider-error-fixture.config.json has dozens of wildcard keys the catalog would pick from.
		id: "routing-error",
		...providerError,
		files: ["185-routing-retries-first-token-cutoffs-and-refused-keys-against.json"],
		config: "tests/e2e/api/provider_config/routing-error-fixture.config.json",
	},
	{
		// The hybrid log store (TXE-7326): the provider-error gateway, with its log store offloading
		// payloads to the S3-compatible store that provider-error-fixture.mjs serves on its main port
		// (lib/s3-object-fixture.mjs). Rows hold or fail a chosen upload through that store's control API.
		// Only the pinned folders run here: offloading changes what every other folder's log reads see.
		id: "hybrid-logstore",
		...providerError,
		variables: ["hybridLogstoreFixture"],
		files: ["547-hybrid-log-store-serves-each-log-while-its-upload-is-held-or.json"],
		configPatch: (config, dir, ports) => {
			config.logs_store = {
				enabled: true,
				type: "sqlite",
				config: { path: join(dir, "logs.db") },
				object_storage: {
					type: "s3",
					bucket: S3_BUCKET,
					prefix: "harness",
					region: "us-east-1",
					endpoint: `http://127.0.0.1:${ports.main}`,
					access_key_id: "harness",
					secret_access_key: "harness",
					force_path_style: true,
				},
			};
		},
		vars: (p) => ({ ...providerError.vars(p), hybridLogstoreFixture: "1" }),
	},
	{
		id: "vertex-endpoint",
		variables: ["vertexEndpointFixture", "vertexEndpointFixtureUrl"],
		ports: { main: 8792 },
		// The fixture writes the gateway config into the app dir: one key per region, the proxy and its CA.
		fixture: (p, appDir) => ({ script: "vertex-endpoint-fixture.mjs", args: ["--app-dir", appDir, "--port", String(p.main)], env: {} }),
		config: null,
		vars: (p) => ({ vertexEndpointFixture: "1", vertexEndpointFixtureUrl: `http://127.0.0.1:${p.main}`, vertexGcsBucket: "fixture-bucket" }),
	},
	{
		id: "bedrock-endpoint",
		variables: ["bedrockEndpointFixture"],
		// The fixture also listens on main+2 (agent runtime) and main+4 (runtime), and rows assert those
		// two port numbers, so this group keeps the default ports and fails if one is taken.
		ports: { main: 8794 },
		span: [0, 2, 4],
		fixedPorts: true,
		fixture: (p, appDir) => ({ script: "bedrock-endpoint-fixture.mjs", args: ["--app-dir", appDir, "--port", String(p.main)], env: {} }),
		config: null,
		vars: () => ({ bedrockEndpointFixture: "1" }),
	},
	{
		id: "azure-stream-preamble",
		variables: ["azureStreamPreambleFixture"],
		ports: { main: 8789 },
		fixture: (p) => ({ script: "azure-stream-preamble-fixture.mjs", args: [], env: { AZURE_STREAM_FIXTURE_PORT: p.main } }),
		config: "tests/e2e/api/provider_config/azure-stream-preamble.config.json",
		vars: () => ({ azureStreamPreambleFixture: "1" }),
	},
	{
		// Folder 186 (TXE-7290): the same fixture, behind OpenAI with one retry and request logging
		// on, since the rows read back the log row of a stream that failed once and was retried.
		id: "stream-retry",
		variables: ["streamRetryFixture"],
		ports: { main: 8789 },
		fixture: (p) => ({ script: "azure-stream-preamble-fixture.mjs", args: [], env: { AZURE_STREAM_FIXTURE_PORT: p.main } }),
		config: "tests/e2e/api/provider_config/stream-retry.config.json",
		vars: () => ({ streamRetryFixture: "1", setupToken: SETUP_TOKEN }),
	},
	{
		// Folder 208 (TXE-7290): the rows create the databricks provider with the token fixture as
		// its proxy, and fall back to openai/gpt-4o-mini, which the config aliases to the
		// provider-error fixture's upstream-ok. Its second row is [PREVIEW], hence include_preview.
		id: "databricks-oauth",
		variables: ["databricksTokenFixtureUrl"],
		ports: { main: 8795, error: 8791, errorStall: 8799, errorTls: 8793, errorTunnel: 8788 },
		fixture: (p) => [
			{ script: "databricks-token-fixture.mjs", args: ["--port", String(p.main)], env: {}, port: p.main },
			{ ...providerError.fixture({ main: p.error, stall: p.errorStall, tls: p.errorTls, tunnel: p.errorTunnel }), port: p.error },
		],
		config: "tests/e2e/api/provider_config/databricks-oauth.config.json",
		vars: (p) => ({ databricksTokenFixtureUrl: `http://127.0.0.1:${p.main}`, include_preview: "1", setupToken: SETUP_TOKEN }),
	},
	{
		// Folders 266 and 267 (TXE-7290): the A2A push fixture is the upstream agent and the
		// client's push callback, on loopback, so the config allows private push callbacks.
		id: "a2a-push",
		variables: ["a2aPushFixtureUrl", "a2aFixtureBase"],
		ports: { main: 18091 },
		fixture: (p) => ({ script: "a2a-push-fixture.mjs", args: [String(p.main)], env: {} }),
		config: "tests/e2e/api/provider_config/a2a-push.config.json",
		vars: (p) => ({ a2aPushFixtureUrl: `http://127.0.0.1:${p.main}`, setupToken: SETUP_TOKEN }),
	},
];

export function loadData(path = DATA_FILE) {
	const data = JSON.parse(readFileSync(path, "utf8"));
	return { notRun: data.notRun || [], quarantine: data.quarantine || [], skippedRows: data.skippedRows || [] };
}

// fixtureVariables returns the fixture variables a folder object reads, sorted.
export function fixtureVariables(folder) {
	return [...new Set(JSON.stringify(folder).match(FIXTURE_VARIABLE) || [])].sort();
}

// plan classifies folders ({file, folder}) into the groups that run them and lists the problems.
export function plan(folders, { notRun = [], quarantine = [] } = {}, groups = GROUPS) {
	const byVariable = new Map();
	for (const g of groups.filter((g) => !g.files)) for (const v of g.variables) byVariable.set(v, g.id);
	const pinned = new Map(groups.flatMap((g) => (g.files || []).map((f) => [f, g])));
	const notRunFiles = new Map(notRun.map((e) => [e.file, e]));
	const problems = [];
	const members = new Map(groups.map((g) => [g.id, []]));
	const declared = [];
	const seenFiles = new Set();
	for (const { file, folder } of folders) {
		seenFiles.add(file);
		const variables = fixtureVariables(folder);
		if (variables.length === 0) continue;
		if (notRunFiles.has(file)) {
			declared.push(file);
			continue;
		}
		const unknown = pinned.has(file) ? [] : variables.filter((v) => !byVariable.has(v));
		if (unknown.length) {
			problems.push(`${file} reads ${unknown.join(", ")}, which no group in fixture-folders.mjs starts: add a group that runs it, or a notRun entry in fixture-folders.json with the reason and a ticket`);
			continue;
		}
		if (pinned.has(file)) {
			members.get(pinned.get(file).id).push({ file, name: folder.name });
			continue;
		}
		for (const id of new Set(variables.map((v) => byVariable.get(v)))) members.get(id).push({ file, name: folder.name });
	}
	for (const [file, g] of pinned) if (!seenFiles.has(file)) problems.push(`group ${g.id} names ${file}, which is no folder file`);
	for (const entry of notRun) {
		if (!entry.ticket || !entry.reason) problems.push(`notRun entry ${entry.file} needs a ticket and a reason`);
		if (!seenFiles.has(entry.file)) problems.push(`notRun entry ${entry.file} names no folder file: remove it`);
		else if (!declared.includes(entry.file)) problems.push(`notRun entry ${entry.file} names a folder that reads no fixture variable: remove it`);
	}
	for (const entry of quarantine) if (!entry.ticket || !entry.row) problems.push(`quarantine entry ${JSON.stringify(entry).slice(0, 120)} needs a row and a ticket`);
	return { members, declared, problems };
}

// verdict compares the rows a run executed with the quarantine list, and the rows the run selected
// with the rows it answered. A selected row that no group answered skipped itself (a [PREVIEW]
// tag, a switch no group sets, a key augment found unset), so it checked nothing: it fails the run
// unless skippedRows in fixture-folders.json names it with the reason.
// rows: Map of row name -> {failed: [message], ran: boolean}; selected: Set of row names.
export function verdict(rows, quarantine, { selected = new Set(), skippedRows = [] } = {}) {
	const quarantined = new Map(quarantine.map((e) => [e.row, e]));
	const problems = [];
	const allowedSkips = new Map(skippedRows.map((e) => [e.row, e]));
	for (const name of selected) {
		const answered = rows.get(name)?.ran;
		if (!answered && !allowedSkips.has(name)) problems.push(`SKIPPED ${name}\n      no group answered this row; make a group run it, or name it in skippedRows in fixture-folders.json with the reason`);
	}
	for (const [name, e] of allowedSkips) {
		if (!e.reason) problems.push(`skippedRows entry needs a reason: ${name}`);
		if (!selected.has(name)) problems.push(`skippedRows entry names no selected row (renamed or removed?): ${name}`);
		else if (rows.get(name)?.ran) problems.push(`skippedRows row runs now: remove its entry from fixture-folders.json: ${name}`);
	}
	for (const [name, r] of rows) {
		if (r.failed.length && !quarantined.has(name)) problems.push(`FAIL ${name}\n      ${r.failed[0]}`);
	}
	for (const [name, e] of quarantined) {
		const r = rows.get(name);
		if (!r) problems.push(`quarantined row did not run (renamed or removed?): ${name} (${e.ticket})`);
		else if (r.failed.length === 0) problems.push(`quarantined row passes now: remove its entry from fixture-folders.json (${e.ticket}): ${name}`);
	}
	return problems;
}

// rowsFromReport reads a newman JSON report into the verdict's row map. A row that newman ran
// more than once (a retry folder) fails when any execution failed.
//
// A script that throws outside pm.test (a pre-request script, or a test script that fails before
// its first assertion) leaves no failed assertion on the execution: newman records it only under
// run.failures, with the request as its source (as countRun in lib/harness-skips.mjs reads it).
// Such a row fails. A script error whose source is no request (a folder or collection script) is
// returned in `unattributed`, which fails the group: no quarantine entry can excuse it.
export function rowsFromReport(report) {
	const rows = new Map();
	const nameById = new Map();
	for (const e of report.run.executions || []) {
		const name = e.item.name;
		if (e.item.id) nameById.set(e.item.id, name);
		const r = rows.get(name) || { failed: [], ran: false };
		r.ran = r.ran || Boolean(e.response);
		if (e.requestError) r.failed.push(`request error: ${e.requestError.message || e.requestError}`);
		for (const a of e.assertions || []) if (a.error && !a.skipped) r.failed.push(`${a.assertion}: ${String(a.error.message).replace(/\s+/g, " ").slice(0, 300)}`);
		rows.set(name, r);
	}
	const unattributed = [];
	for (const f of report.run.failures || []) {
		// A failed assertion is on its execution already.
		if (f?.error?.name === "AssertionError") continue;
		const message = `${f?.at || "script"}: ${f?.error?.name || "Error"}: ${String(f?.error?.message || "").replace(/\s+/g, " ").slice(0, 300)}`;
		const name = f?.source?.id && nameById.get(f.source.id);
		if (name) rows.get(name).failed.push(message);
		else unattributed.push(`${f?.source?.name || "collection"}: ${message}`);
	}
	return { rows, unattributed };
}

// requestNames lists every request (row) under a collection's folders, at any depth.
export function requestNames(collection) {
	const names = new Set();
	const walk = (items) => {
		for (const i of items || []) i.item ? walk(i.item) : names.add(i.name);
	};
	walk(collection.item);
	return names;
}

export function readFolders(dir = PROVIDER_HARNESS_DIR) {
	return listFolderFiles(dir).map((file) => ({ file, folder: JSON.parse(readFileSync(join(dir, file), "utf8")) }));
}


const canBind = (port) =>
	new Promise((ok) => {
		const s = net.createServer();
		s.once("error", () => ok(false));
		s.listen(port, "127.0.0.1", () => s.close(() => ok(true)));
	});

// Ports come from lib/loopback-port.mjs, below the ephemeral ranges (TXE-7326).
const freePort = freeLoopbackPort;

const until = async (what, probe, seconds) => {
	for (let i = 0; i < seconds * 4; i++) {
		if (await probe()) return;
		await new Promise((r) => setTimeout(r, 250));
	}
	throw new Error(`${what} was not ready within ${seconds}s`);
};

const tcpOpen = (port) =>
	new Promise((ok) => {
		const s = net.connect(port, "127.0.0.1");
		s.once("connect", () => s.end(() => ok(true)));
		s.once("error", () => ok(false));
	});

const httpOk = async (url) => {
	try {
		return (await fetch(url)).ok;
	} catch {
		return false;
	}
};

function start(command, args, { env = {}, log }) {
	const child = spawn(command, args, { cwd: REPO, env: { ...process.env, ...Object.fromEntries(Object.entries(env).map(([k, v]) => [k, String(v)])) }, stdio: ["ignore", "pipe", "pipe"] });
	const lines = [];
	const keep = (chunk) => {
		writeFileSync(log, chunk, { flag: "a" });
		lines.push(...String(chunk).split("\n"));
		if (lines.length > 400) lines.splice(0, lines.length - 400);
	};
	child.stdout.on("data", keep);
	child.stderr.on("data", keep);
	child.exited = new Promise((r) => child.once("exit", (code, signal) => r({ code, signal })));
	child.tail = () => lines.slice(-40).join("\n");
	return child;
}

const stop = async (child) => {
	if (!child || child.exitCode !== null || child.signalCode !== null) return;
	child.kill("SIGTERM");
	const t = setTimeout(() => child.kill("SIGKILL"), 5000);
	await child.exited;
	clearTimeout(t);
};

function newmanBin() {
	const pinned = join(REPO, ".github/workflows/tools/node_modules/.bin/newman");
	return existsSync(pinned) ? pinned : "newman";
}

async function runGroup(group, folders, { gateway, workDir }) {
	const dir = join(workDir, group.id);
	mkdirSync(dir, { recursive: true });
	const mapping = [];
	const ports = {};
	for (const [key, def] of Object.entries(group.ports)) {
		if (group.fixedPorts) {
			for (const d of group.span || [0]) if (!(await canBind(def + d))) throw new Error(`port ${def + d} is taken; this group's rows assert its default ports`);
			ports[key] = def;
			continue;
		}
		ports[key] = await freePort(group.span || [0]);
		mapping.push([def, ports[key]]);
		for (const d of (group.span || [0]).filter((d) => d !== 0)) mapping.push([def + d, ports[key] + d]);
	}
	const datasheets = await serveEmptyDatasheets();
	const fixtures = [];
	let gw;
	try {
		const patch = group.configPatch && ((config) => group.configPatch(config, dir, ports));
		if (group.config) writeCheckedInFixtureConfig(dir, { source: join(REPO, group.config), mapping, datasheetOrigin: datasheets.url, patch });
		// A group may start more than one fixture; each is waited for on its own port.
		const specs = [group.fixture(ports, dir)].flat();
		for (const [i, fx] of specs.entries()) {
			const log = join(dir, specs.length === 1 ? "fixture.log" : `fixture-${i}.log`);
			fixtures.push(start(process.execPath, [join(HERE, fx.script), ...fx.args], { env: fx.env, log }));
			await until(`${group.id} fixture ${fx.script}`, () => tcpOpen(fx.port ?? ports.main), 30);
		}
		if (!existsSync(join(dir, "config.json"))) await until(`${group.id} fixture's gateway config`, async () => existsSync(join(dir, "config.json")), 30);
		const gwPort = await freePort();
		// No proxy tripwire here: scripts/test-offline.sh refuses every non-loopback dial and names
		// it (TXE-7083), and row 476.2 asserts exactly that refusal.
		gw = start(gateway, ["-app-dir", dir, "-host", "127.0.0.1", "-port", String(gwPort)], { log: join(dir, "gateway.log") });
		await until(`${group.id} gateway`, () => httpOk(`http://127.0.0.1:${gwPort}/health`), 180).catch((e) => {
			throw new Error(`${e.message}\n--- gateway log ---\n${gw.tail()}`);
		});
		const augmented = join(dir, "augmented.json");
		const aug = start(process.execPath, [join(HERE, "augment-provider-harness.mjs"), "--source", PROVIDER_HARNESS_DIR, "--profile", join(dir, "config.json"), "--out", augmented], { log: join(dir, "augment.log") });
		const augExit = await aug.exited;
		if (augExit.code !== 0) throw new Error(`augment-provider-harness.mjs exited ${augExit.code}\n${aug.tail()}`);
		const names = new Set(folders.map((f) => f.name));
		const collection = JSON.parse(readFileSync(augmented, "utf8"));
		collection.item = collection.item.filter((f) => names.has(f.name));
		const missing = [...names].filter((n) => !collection.item.some((f) => f.name === n));
		if (missing.length) throw new Error(`augmented collection lacks folders: ${missing.join(", ")}`);
		const selected = join(dir, "selected.json");
		writeFileSync(selected, remapLoopbackPorts(JSON.stringify(collection), mapping));
		const report = join(dir, "report.json");
		const vars = { baseUrl: `http://127.0.0.1:${gwPort}`, ...group.vars(ports) };
		const args = ["run", selected, "--reporters", "cli,json", "--reporter-json-export", report, ...Object.entries(vars).flatMap(([k, v]) => ["--env-var", `${k}=${v}`])];
		const newman = start(newmanBin(), args, { log: join(dir, "newman.log") });
		const { code } = await newman.exited;
		if (!existsSync(report)) throw new Error(`newman exited ${code} without a report\n${newman.tail()}`);
		return { ...rowsFromReport(JSON.parse(readFileSync(report, "utf8"))), newmanExit: code, selected: requestNames(collection) };
	} finally {
		await stop(gw);
		for (const f of fixtures) await stop(f);
		datasheets.server.close();
	}
}

async function serveEmptyDatasheets() {
	const { createServer } = await import("node:http");
	const server = createServer((req, res) => {
		res.writeHead(200, { "Content-Type": "application/json" });
		res.end("{}");
	});
	await new Promise((r) => server.listen(0, "127.0.0.1", r));
	return { server, url: `http://127.0.0.1:${server.address().port}` };
}

async function main() {
	const [command, ...rest] = process.argv.slice(2);
	const args = {};
	for (let i = 0; i < rest.length; i += 2) args[rest[i].replace(/^--/, "")] = rest[i + 1];
	const data = loadData();
	const { members, declared, problems } = plan(readFolders(), data);
	if (command === "plan") {
		for (const g of GROUPS) console.log(`${g.id}: ${members.get(g.id).length} folders`);
		console.log(`notRun: ${declared.length} folders (${declared.join(", ")})`);
		console.log(`quarantine: ${data.quarantine.length} rows`);
		for (const p of problems) console.error(`✗ ${p}`);
		process.exit(problems.length ? 1 : 0);
	}
	if (command !== "run" || !args.gateway) {
		console.error("usage: fixture-folders.mjs plan | run --gateway <bifrost-http binary> [--group <id>] [--folder <number>[,<number>...]] [--work-dir <dir>]");
		process.exit(2);
	}
	if (problems.length) {
		for (const p of problems) console.error(`✗ ${p}`);
		process.exit(1);
	}
	const workDir = args["work-dir"] ? resolve(args["work-dir"]) : mkdtempSync(join(tmpdir(), "fixture-folders-"));
	// --folder <number>[,<number>...] narrows every group to those folders, in the group's order, for
	// working on one locally; the quarantined and skipped rows of other folders are then not judged, as
	// with --group.
	if (args.folder) {
		const wanted = args.folder.split(",").map((n) => `${n.trim()}. `);
		for (const [id, list] of members) members.set(id, list.filter((f) => wanted.some((w) => f.name.startsWith(w))));
	}
	const groups = GROUPS.filter((g) => (!args.group || g.id === args.group) && members.get(g.id).length > 0);
	if (groups.length === 0) {
		console.error(`no group ${args.group || ""} runs ${args.folder ? `folder ${args.folder}` : "any folder"}`);
		process.exit(2);
	}
	const rows = new Map();
	const selected = new Set();
	const failures = [];
	for (const g of groups) {
		const folders = members.get(g.id);
		console.log(`▶ ${g.id}: ${folders.length} folders`);
		try {
			const { rows: got, unattributed, newmanExit, selected: picked } = await runGroup(g, folders, { gateway: resolve(args.gateway), workDir });
			for (const n of picked) selected.add(n);
			const ran = [...got.values()].filter((r) => r.ran).length;
			const failed = [...got.values()].filter((r) => r.failed.length).length;
			console.log(`  ${got.size} rows, ${ran} answered, ${failed} failing`);
			if (ran === 0) failures.push(`${g.id}: no row was answered; the group's switch or gateway is broken`);
			for (const u of unattributed) failures.push(`${g.id}: script error outside any row: ${u}`);
			// newman exits non-zero for a failure it saw. One the report explains no row of is a failure too.
			if (newmanExit !== 0 && failed === 0 && unattributed.length === 0) failures.push(`${g.id}: newman exited ${newmanExit} and no row failed; see ${join(workDir, g.id, "newman.log")}`);
			for (const [k, v] of got) {
				const prev = rows.get(k);
				rows.set(k, prev ? { failed: [...prev.failed, ...v.failed], ran: prev.ran || v.ran } : v);
			}
		} catch (e) {
			failures.push(`${g.id}: ${e.message}`);
		}
	}
	// With --group or --folder, only the quarantined and skipped rows that ran can be judged.
	const narrowed = Boolean(args.group || args.folder);
	const quarantine = narrowed ? data.quarantine.filter((e) => rows.has(e.row)) : data.quarantine;
	const skippedRows = narrowed ? data.skippedRows.filter((e) => selected.has(e.row)) : data.skippedRows;
	failures.push(...verdict(rows, quarantine, { selected, skippedRows }));
	console.log(`work dir: ${workDir}`);
	if (failures.length) {
		console.error(`\n✗ ${failures.length} problem(s):`);
		for (const f of failures) console.error(`  ${f}`);
		process.exit(1);
	}
	console.log(`✓ ${rows.size} rows across ${groups.length} group(s); ${quarantine.length} quarantined rows still fail; ${skippedRows.length} rows skip by declaration`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
	main().catch((e) => {
		console.error(e.stack || String(e));
		process.exit(1);
	});
}
