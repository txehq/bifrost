// Tests port-check.mjs, and runs its check over every script test in tests/e2e/api: a test that
// binds a literal port, or starts a fixture with one of its ports left on the default, fails
// here (TXE-7106). Run directly: `node port-check.test.mjs`.
import assert from "node:assert/strict";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { literalPortFindings, portSettings, readPortCheckInputs } from "./port-check.mjs";

const ROOT = path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "..");

test("portSettings reads a script's port variables and its --port flag", () => {
	assert.deepEqual(portSettings('const a = process.env.A_PORT || "1"; const b = Number(process.env.B_TLS_PORT);'), { variables: ["A_PORT", "B_TLS_PORT"], flag: false });
	assert.deepEqual(portSettings('argValue(args, "--port", process.env.C_PORT || "2")'), { variables: ["C_PORT"], flag: true });
	assert.deepEqual(portSettings("const x = process.env.HOME;"), { variables: [], flag: false });
});

test("the check fires on a literal listen and on a fixture port left on its default", () => {
	const scripts = {
		"three-port-fixture.mjs": 'process.env.F_PORT || "8791"; process.env.F_STALL_PORT || "8799"; process.env.F_TLS_PORT || "8793";',
		"flag-fixture.mjs": 'argValue(args, "--port", process.env.G_PORT || "8794")',
	};
	const findings = (source) => literalPortFindings({ tests: { "x.test.mjs": source }, scripts });

	assert.match(findings("server.listen(8791, '127.0.0.1')")[0], /listens on the literal port 8791/);
	assert.match(findings("http.createServer(h).listen( 20001, cb)")[0], /literal port 20001/);
	assert.deepEqual(findings("server.listen(0, '127.0.0.1'); proxy.listen(() => {}); srv.listen(port, host)"), []);

	// The defect this ticket is about: two of the fixture's three ports set, the TLS one forgotten.
	const forgot = findings('spawn(node, ["three-port-fixture.mjs"], { env: { F_PORT: p, F_STALL_PORT: q } })');
	assert.equal(forgot.length, 1);
	assert.match(forgot[0], /starts three-port-fixture\.mjs without setting F_TLS_PORT/);
	assert.deepEqual(findings('spawn(node, ["three-port-fixture.mjs"], { env: { F_PORT: p, F_STALL_PORT: q, F_TLS_PORT: r } })'), []);

	assert.match(findings('spawn(node, ["flag-fixture.mjs", "--app-dir", dir])')[0], /without setting G_PORT \(or --port\)/);
	assert.deepEqual(findings('spawn(node, ["flag-fixture.mjs", "--app-dir", dir, "--port", String(p)])'), []);
	assert.deepEqual(findings('spawn(node, ["flag-fixture.mjs"], { env: { G_PORT: p } })'), []);

	assert.deepEqual(findings("// ports-not-bound: flag-fixture.mjs (read as text only)\nfs.readFileSync('flag-fixture.mjs')"), []);
	assert.deepEqual(findings("no fixture here; a URL like http://127.0.0.1:8791 is data, not a listener"), []);
});

test("no script test under tests/e2e/api binds a literal port or leaves a fixture port on its default", () => {
	const inputs = readPortCheckInputs(ROOT);
	// This file holds the samples the check is tested with above; it starts nothing but its own listeners.
	delete inputs.tests[path.join("runners", "lib", "port-check.test.mjs")];
	assert.ok(Object.keys(inputs.tests).length >= 30, `found ${Object.keys(inputs.tests).length} script tests, want the whole set`);
	for (const fixture of ["provider-error-fixture.mjs", "bedrock-endpoint-fixture.mjs", "vertex-endpoint-fixture.mjs"]) {
		assert.ok(inputs.scripts[fixture], `${fixture} was not read as a script that takes ports`);
	}
	assert.deepEqual(literalPortFindings(inputs), []);
});
