// Tests for fixture-folders.mjs (TXE-7285). Run directly: `node fixture-folders.test.mjs`; needs
// `newman` on PATH (or the pinned one in .github/workflows/tools), no gateway, no network, no
// credentials. The offline API-collection suites find and run it,
// so this is where a new fixture folder that no runner group starts fails on its pull request.
//
// The run itself (fixture, gateway, newman) is the `harness fixture folders` job in
// go-unit-tests.yml; these tests pin the rules that decide what it runs and what fails it.
import assert from "node:assert";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";

import { GROUPS, fixtureVariables, loadData, plan, readFolders, rowsFromReport, verdict } from "./fixture-folders.mjs";
import { remapLoopbackPorts, writeCheckedInFixtureConfig } from "./lib/tls-intercept-proxy.mjs";

const REPO = path.resolve(path.dirname(new URL(import.meta.url).pathname), "../../../..");

let passed = 0;
const check = (name, fn) => {
	fn();
	passed++;
	console.log(`  ok - ${name}`);
};

const folder = (name, ...lines) => ({ name, item: [{ name: `${name} row`, event: [{ listen: "prerequest", script: { exec: lines } }] }] });

check("the real harness: every folder that reads a fixture variable is run by a group or declared in notRun", () => {
	const { members, problems } = plan(readFolders(), loadData());
	assert.deepStrictEqual(problems, []);
	// The folder whose stale row started this (161.11) is run by the Vertex group.
	assert.ok(members.get("vertex-endpoint").some((f) => f.name.startsWith("161. ")), "folder 161 is in the vertex-endpoint group");
	assert.ok(members.get("provider-error").length >= 60, "the provider error fixture's folders are found");
});

check("a folder reading a fixture variable no group starts fails plan, naming the folder and the variable", () => {
	const { problems } = plan([{ file: "900-new.json", folder: folder("900. new", "if (pm.variables.get('kafkaBrokerFixtureUrl')) {}") }], { notRun: [], quarantine: [] });
	assert.strictEqual(problems.length, 1);
	assert.match(problems[0], /900-new\.json reads kafkaBrokerFixtureUrl/);
});

check("a notRun entry with its reason and ticket excuses that folder", () => {
	const data = { notRun: [{ file: "900-new.json", reason: "needs a broker", ticket: "TXE-1" }], quarantine: [] };
	const { problems, declared } = plan([{ file: "900-new.json", folder: folder("900. new", "pm.variables.get('kafkaBrokerFixtureUrl')") }], data);
	assert.deepStrictEqual(problems, []);
	assert.deepStrictEqual(declared, ["900-new.json"]);
});

check("stale notRun entries fail plan: a missing file, a folder without fixture variables, no ticket", () => {
	const folders = [{ file: "1-live.json", folder: folder("1. live", "pm.response.code") }];
	const data = {
		notRun: [
			{ file: "2-gone.json", reason: "r", ticket: "TXE-1" },
			{ file: "1-live.json", reason: "r", ticket: "TXE-1" },
			{ file: "1-live.json", reason: "r" },
		],
		quarantine: [],
	};
	const { problems } = plan(folders, data);
	assert.ok(problems.some((p) => /2-gone\.json names no folder file/.test(p)));
	assert.ok(problems.some((p) => /1-live\.json names a folder that reads no fixture variable/.test(p)));
	assert.ok(problems.some((p) => /needs a ticket and a reason/.test(p)));
});

check("a folder reading two groups' variables is run by both", () => {
	const { members } = plan([{ file: "314-x.json", folder: folder("314. x", "pm.variables.get('providerErrorFixture')", "pm.variables.get('vertexEndpointFixture')") }], { notRun: [], quarantine: [] });
	assert.deepStrictEqual(members.get("provider-error").map((f) => f.file), ["314-x.json"]);
	assert.deepStrictEqual(members.get("vertex-endpoint").map((f) => f.file), ["314-x.json"]);
});

check("fixture variables are found in scripts and {{ }} templates; a JS name like nFixtures is not one", () => {
	const f = { name: "x", item: [{ name: "r", request: { url: "{{providerErrorFixtureUrl}}/x" }, event: [{ script: { exec: ["var nFixtures = 3; pm.variables.get('a2aFixtureBase')"] } }] }] };
	assert.deepStrictEqual(fixtureVariables(f), ["a2aFixtureBase", "providerErrorFixtureUrl"]);
});

check("every group variable matches the fixture-variable pattern, so plan sees the group's folders", () => {
	for (const g of GROUPS) for (const v of g.variables) assert.deepStrictEqual(fixtureVariables({ v: `{{${v}}}` }), [v], `${g.id}: ${v}`);
});

check("verdict: a failing row fails the run unless quarantined; a quarantined row that passes or did not run fails it", () => {
	const rows = new Map([
		["new failure", { failed: ["Status code is 200: got 400"], ran: true }],
		["known failure", { failed: ["x"], ran: true }],
		["fixed row", { failed: [], ran: true }],
		["passing", { failed: [], ran: true }],
	]);
	const problems = verdict(rows, [
		{ row: "known failure", ticket: "TXE-1" },
		{ row: "fixed row", ticket: "TXE-2" },
		{ row: "renamed row", ticket: "TXE-3" },
	]);
	assert.strictEqual(problems.length, 3);
	assert.match(problems[0], /^FAIL new failure/);
	assert.ok(problems.some((p) => /passes now: remove its entry .*TXE-2.*fixed row/.test(p)));
	assert.ok(problems.some((p) => /did not run .*renamed row \(TXE-3\)/.test(p)));
	assert.deepStrictEqual(verdict(new Map([["known failure", { failed: ["x"], ran: true }]]), [{ row: "known failure", ticket: "TXE-1" }]), []);
});

check("rowsFromReport: a row fails on a failed assertion or a request error, and a skipped row neither ran nor failed", () => {
	const { rows } = rowsFromReport({
		run: {
			executions: [
				{ item: { name: "a" }, response: { code: 200 }, assertions: [{ assertion: "ok" }] },
				{ item: { name: "b" }, response: { code: 400 }, assertions: [{ assertion: "Status code is 200", error: { message: "expected 400 to equal 200" } }] },
				{ item: { name: "c" }, requestError: { message: "ECONNREFUSED" } },
				{ item: { name: "d" } },
			],
		},
	});
	assert.deepStrictEqual(rows.get("a"), { failed: [], ran: true });
	assert.deepStrictEqual(rows.get("b").failed, ["Status code is 200: expected 400 to equal 200"]);
	assert.deepStrictEqual(rows.get("c"), { failed: ["request error: ECONNREFUSED"], ran: false });
	assert.deepStrictEqual(rows.get("d"), { failed: [], ran: false });
});

check("remapLoopbackPorts rewrites a whole default port on loopback only, never a longer port that starts with it", () => {
	const text = "http://127.0.0.1:8791/x localhost:8791 127.0.0.1:87910 10.0.0.1:8791";
	assert.strictEqual(remapLoopbackPorts(text, [[8791, 23456]]), "http://127.0.0.1:23456/x localhost:23456 127.0.0.1:87910 10.0.0.1:8791");
});

check("writeCheckedInFixtureConfig remaps ports, adds loopback datasheets only where none are named, and refuses inference auth on", () => {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "fixture-folders-"));
	const write = (name, config) => {
		const p = path.join(dir, name);
		fs.writeFileSync(p, JSON.stringify(config));
		return p;
	};
	const bare = write("bare.json", { client: { enforce_auth_on_inference: false }, providers: { openai: { keys: [{ value: "x" }], network_config: { base_url: "http://127.0.0.1:8789" } } } });
	const out = JSON.parse(fs.readFileSync(writeCheckedInFixtureConfig(path.join(dir, "a"), { source: bare, mapping: [[8789, 30001]], datasheetOrigin: "http://127.0.0.1:1" }), "utf8"));
	assert.strictEqual(out.providers.openai.network_config.base_url, "http://127.0.0.1:30001");
	assert.deepStrictEqual(out.framework.pricing, { pricing_url: "http://127.0.0.1:1/pricing.json", model_parameters_url: "http://127.0.0.1:1/model-parameters.json" });
	const pinned = write("pinned.json", { client: { enforce_auth_on_inference: false }, framework: { pricing: { pricing_url: "http://127.0.0.1:8791/p", model_parameters_url: "http://127.0.0.1:8791/m" } } });
	const kept = JSON.parse(fs.readFileSync(writeCheckedInFixtureConfig(path.join(dir, "b"), { source: pinned, mapping: [[8791, 30002]], datasheetOrigin: "http://127.0.0.1:1" }), "utf8"));
	assert.deepStrictEqual(kept.framework.pricing, { pricing_url: "http://127.0.0.1:30002/p", model_parameters_url: "http://127.0.0.1:30002/m" });
	// The Azure stream preamble config had no client block until TXE-7285, so folder 74 got 401 on every row.
	const authOn = write("auth-on.json", { providers: {} });
	assert.throws(() => writeCheckedInFixtureConfig(path.join(dir, "c"), { source: authOn, datasheetOrigin: "http://127.0.0.1:1" }), /auth-on\.json must set client\.enforce_auth_on_inference: false/);
	fs.rmSync(dir, { recursive: true, force: true });
});

check("every group's checked-in gateway config passes writeCheckedInFixtureConfig", () => {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "fixture-folders-"));
	for (const g of GROUPS.filter((g) => g.config)) writeCheckedInFixtureConfig(path.join(dir, g.id), { source: path.join(REPO, g.config), datasheetOrigin: "http://127.0.0.1:1" });
	fs.rmSync(dir, { recursive: true, force: true });
});

check("rowsFromReport: a script that threw outside pm.test fails its row from run.failures; one outside any row is unattributed", () => {
	const { rows, unattributed } = rowsFromReport({
		run: {
			executions: [
				{ item: { id: "1", name: "pre throws" }, response: { code: 200 }, assertions: [] },
				{ item: { id: "2", name: "asserts" }, response: { code: 400 }, assertions: [{ assertion: "Status code is 200", error: { name: "AssertionError", message: "expected 400 to equal 200" } }] },
			],
			failures: [
				{ at: "prerequest-script", source: { id: "1", name: "pre throws" }, error: { name: "TypeError", message: "Cannot read properties of undefined" } },
				{ at: "assertion:0 in test-script", source: { id: "2", name: "asserts" }, error: { name: "AssertionError", message: "expected 400 to equal 200" } },
				{ at: "test-script", source: { id: "folder-9", name: "9. folder" }, error: { name: "ReferenceError", message: "x is not defined" } },
			],
		},
	});
	assert.deepStrictEqual(rows.get("pre throws").failed, ["prerequest-script: TypeError: Cannot read properties of undefined"]);
	// The assertion is counted once, from the execution.
	assert.deepStrictEqual(rows.get("asserts").failed, ["Status code is 200: expected 400 to equal 200"]);
	assert.deepStrictEqual(unattributed, ["9. folder: test-script: ReferenceError: x is not defined"]);
});

// The same with the real newman: a row whose pre-request script throws, and a row whose test
// script throws before its first assertion, each leave no failed assertion on the execution.
{
	const pinned = path.join(REPO, ".github/workflows/tools/node_modules/.bin/newman");
	const newman = fs.existsSync(pinned) ? pinned : execFileSync("sh", ["-c", "command -v newman"], { encoding: "utf8" }).trim();
	const server = http.createServer((req, res) => res.end("not json"));
	await new Promise((r) => server.listen(0, "127.0.0.1", r));
	const url = `http://127.0.0.1:${server.address().port}/x`;
	const row = (name, listen, exec) => ({ name, event: [{ listen, script: { type: "text/javascript", exec } }], request: { method: "GET", url } });
	const collection = {
		info: { name: "fixture-folders script errors", schema: "https://schema.getpostman.com/json/collection/v2.1.0/collection.json" },
		item: [
			{
				name: "1. folder",
				item: [
					row("pre-request throws", "prerequest", ["undefined.x;"]),
					row("test throws before asserting", "test", ["var j = pm.response.json();", "pm.test('never reached', function () {});"]),
					row("passes", "test", ["pm.test('ok', function () {});"]),
				],
			},
		],
	};
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "fixture-folders-newman-"));
	fs.writeFileSync(path.join(dir, "c.json"), JSON.stringify(collection));
	const reportPath = path.join(dir, "report.json");
	const { spawn } = await import("node:child_process");
	const exit = await new Promise((r) => spawn(newman, ["run", path.join(dir, "c.json"), "--reporters", "json", "--reporter-json-export", reportPath], { stdio: "ignore" }).once("exit", r));
	server.close();
	check("real newman: rows whose scripts threw outside pm.test fail, and the row that passed does not", () => {
		assert.notStrictEqual(exit, 0, "newman reports the script errors in its exit status");
		const { rows, unattributed } = rowsFromReport(JSON.parse(fs.readFileSync(reportPath, "utf8")));
		assert.match(rows.get("pre-request throws").failed.join("\n"), /prerequest-script: TypeError/);
		assert.match(rows.get("test throws before asserting").failed.join("\n"), /test-script: JSONError/);
		assert.deepStrictEqual(rows.get("passes").failed, []);
		assert.deepStrictEqual(unattributed, []);
	});
	fs.rmSync(dir, { recursive: true, force: true });
}

console.log(`\n${passed} passed, 0 failed`);
