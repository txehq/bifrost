// The check that every script test gives each listener it starts a port of its own (TXE-7106).
//
// Several sessions run the script tests on one machine. A test that binds a fixed port, or
// starts a fixture and leaves one of its ports on the default, fails with EADDRINUSE whenever
// another session's fixture holds that port (provider-error-fixture.test.mjs left the TLS
// listener on 8793). Ports come from lib/loopback-port.mjs (TXE-7326: below the ephemeral
// ranges), or a test's own listener listens on 0. literalPortFindings below fails a test that
// does neither. fixture-folders.test.mjs separately fails a hand-rolled random port.
import fs from "node:fs";
import path from "node:path";

// portSettings lists how a runner script takes its ports: every environment variable whose name
// contains PORT that it reads, and whether it takes --port. A script with neither binds nothing
// a test could leave on a default.
export function portSettings(source) {
	const variables = [...new Set([...source.matchAll(/process\.env\.([A-Z0-9_]*PORT[A-Z0-9_]*)/g)].map((m) => m[1]))].sort();
	return { variables, flag: /["']--port["']/.test(source) };
}

// A listen call whose first argument is a number other than 0, or a createServer(...).listen
// chain with one: `.listen(8791`, `.listen( 20001,`.
const LITERAL_LISTEN = /\.listen\(\s*([1-9][0-9]*)\b/g;

// literalPortFindings checks the script tests under root (tests/e2e/api) and returns one line per
// defect; an empty list passes. tests maps a test file's path to its source and scripts maps a
// runner script's file name to its source. A test fails the check when it:
//
//   - calls listen with a literal port other than 0, or
//   - names a runner script that takes ports and does not set every one of them: each port
//     environment variable the script reads, or its --port flag when it has one. A script it
//     only names in a comment or reads as text counts too: the check cannot tell, and a test
//     that needs no port for a script can say so with a `ports-not-bound: <script>` comment.
export function literalPortFindings({ tests, scripts }) {
	const findings = [];
	for (const [file, source] of Object.entries(tests)) {
		for (const match of source.matchAll(LITERAL_LISTEN)) {
			findings.push(`${file}: listens on the literal port ${match[1]}; take one from runners/lib/loopback-port.mjs (freeLoopbackPort) or listen on 0`);
		}
		for (const [script, scriptSource] of Object.entries(scripts)) {
			if (!source.includes(script) || source.includes(`ports-not-bound: ${script}`)) continue;
			const { variables, flag } = portSettings(scriptSource);
			if (flag && /["']--port["']/.test(source)) continue;
			const unset = variables.filter((name) => !source.includes(name));
			if (unset.length > 0) {
				findings.push(`${file}: starts ${script} without setting ${unset.join(", ")}${flag ? " (or --port)" : ""}, so that listener stays on its default port; pass ports from runners/lib/loopback-port.mjs`);
			}
		}
	}
	return findings;
}

// readPortCheckInputs reads what literalPortFindings checks: every *.test.mjs under root, and
// every other .mjs under root/runners that takes a port.
export function readPortCheckInputs(root) {
	const tests = {};
	const scripts = {};
	const walk = (dir) => {
		for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
			if (entry.name === "node_modules") continue;
			const full = path.join(dir, entry.name);
			if (entry.isDirectory()) walk(full);
			else if (entry.name.endsWith(".test.mjs")) tests[path.relative(root, full)] = fs.readFileSync(full, "utf8");
			else if (entry.name.endsWith(".mjs") && dir === path.join(root, "runners")) {
				const source = fs.readFileSync(full, "utf8");
				const { variables, flag } = portSettings(source);
				if (variables.length > 0 || flag) scripts[entry.name] = source;
			}
		}
	};
	walk(root);
	return { tests, scripts };
}
