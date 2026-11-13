// Tests for the endpoint fixtures' gateway config (lib/tls-intercept-proxy.mjs, TXE-7039).
//
// An endpoint fixture writes the config of the isolated gateway its harness cases run against.
// Inference auth is on by default and the cases send no virtual key, so a config that does not
// turn it off makes the gateway answer every case with 401 before it reaches the provider: the
// cases then test nothing. The setting lives in one place, fixtureGatewayConfig, and this suite
// fails a fixture whose written config lacks it.
//
// The check is on what each fixture actually writes, not on its source: every runner that uses
// the fixture library and takes --app-dir (the directory of the gateway it configures) is started
// on a scratch directory and its config.json is read back. A new fixture of that kind is picked
// up without being listed here. A fixture with no --app-dir (databricks-token-fixture.mjs) writes
// no gateway config: its cases run against the harness gateway, whose profile is not written here.
//
// Run directly: `node tls-intercept-proxy.test.mjs`. No gateway, no network beyond loopback.
import assert from "node:assert";
import { spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { freeLoopbackPort } from "./loopback-port.mjs";
import { fixtureGatewayConfig, writeFixtureGatewayConfig } from "./tls-intercept-proxy.mjs";

const libDir = path.dirname(fileURLToPath(import.meta.url));
const runnersDir = path.resolve(libDir, "..");

let passed = 0;
async function test(name, fn) {
	await fn();
	passed++;
	console.log(`  ok - ${name}`);
}

// What a fixture gateway config must carry. A case that expects success and gets 401 from its
// fixture gateway is a config without the first row.
function fixtureConfigProblems(config) {
	const problems = [];
	if (config?.client?.enforce_auth_on_inference !== false) {
		problems.push("client.enforce_auth_on_inference is not false: the gateway would refuse every fixture case with 401 (virtual key is required)");
	}
	const pricing = config?.framework?.pricing || {};
	for (const key of ["pricing_url", "model_parameters_url"]) {
		if (!/^http:\/\/127\.0\.0\.1:\d+\//.test(pricing[key] || "")) {
			problems.push(`framework.pricing.${key} is not served by the fixture on loopback: the gateway would go to the network on first start`);
		}
	}
	if (!config?.providers || Object.keys(config.providers).length === 0) {
		problems.push("providers is empty");
	}
	return problems;
}

// Every runner that imports the fixture library, takes --app-dir and writes its config through
// writeFixtureGatewayConfig configures a gateway of its own.
function endpointFixtures() {
	return fs
		.readdirSync(runnersDir)
		.filter((name) => name.endsWith(".mjs"))
		.filter((name) => {
			const source = fs.readFileSync(path.join(runnersDir, name), "utf8");
			// A fixture writes its gateway config through the library. fixture-folders.mjs also imports
			// the library and passes --app-dir, but to start fixtures, not to be one (TXE-7285).
			return /from\s+["']\.\/lib\/tls-intercept-proxy\.mjs["']/.test(source) && source.includes('"--app-dir"') && source.includes("writeFixtureGatewayConfig(");
		})
		.sort();
}

// A free loopback port p with p+2 and p+4 free too (the Bedrock fixture listens on all three),
// from lib/loopback-port.mjs, below the ephemeral ranges.
const freePortTriple = () => freeLoopbackPort([0, 2, 4]);

// Starts a fixture on a scratch app dir, waits until it reports that it is listening, and returns
// the config it wrote. The fixture is stopped before this returns.
async function configWrittenBy(fixture) {
	const appDir = fs.mkdtempSync(path.join(os.tmpdir(), "bifrost-fixture-config-"));
	const port = await freePortTriple();
	const child = spawn(process.execPath, [path.join(runnersDir, fixture), "--app-dir", appDir, "--port", String(port)], { stdio: ["ignore", "pipe", "pipe"] });
	let output = "";
	try {
		await new Promise((resolve, reject) => {
			const timer = setTimeout(() => reject(new Error(`${fixture} did not report listening within 30s. Output:\n${output}`)), 30000);
			const onData = (chunk) => {
				output += chunk;
				if (/listening/.test(output)) {
					clearTimeout(timer);
					resolve();
				}
			};
			child.stdout.on("data", onData);
			child.stderr.on("data", onData);
			child.once("exit", (code) => {
				clearTimeout(timer);
				reject(new Error(`${fixture} exited with code ${code} before listening. Output:\n${output}`));
			});
		});
		const configPath = path.join(appDir, "config.json");
		assert.ok(fs.existsSync(configPath), `${fixture} wrote no config.json into its --app-dir`);
		return JSON.parse(fs.readFileSync(configPath, "utf8"));
	} finally {
		child.removeAllListeners("exit");
		child.kill("SIGKILL");
		fs.rmSync(appDir, { recursive: true, force: true });
	}
}

await test("fixtureGatewayConfig turns inference auth off and serves the datasheets from the fixture", () => {
	const config = fixtureGatewayConfig({ datasheetOrigin: "http://127.0.0.1:20001", providers: { vertex: { keys: [] } } });
	assert.deepStrictEqual(config.client, { enforce_auth_on_inference: false });
	assert.strictEqual(config.framework.pricing.pricing_url, "http://127.0.0.1:20001/pricing.json");
	assert.strictEqual(config.framework.pricing.model_parameters_url, "http://127.0.0.1:20001/model-parameters.json");
	assert.deepStrictEqual(config.providers, { vertex: { keys: [] } });
	assert.deepStrictEqual(fixtureConfigProblems(config), []);
});

await test("writeFixtureGatewayConfig writes that config to <app-dir>/config.json", () => {
	const appDir = fs.mkdtempSync(path.join(os.tmpdir(), "bifrost-fixture-config-"));
	try {
		const configPath = writeFixtureGatewayConfig(path.join(appDir, "nested"), { datasheetOrigin: "http://127.0.0.1:20001", providers: { bedrock: { keys: [] } } });
		assert.strictEqual(configPath, path.join(appDir, "nested", "config.json"));
		assert.deepStrictEqual(fixtureConfigProblems(JSON.parse(fs.readFileSync(configPath, "utf8"))), []);
	} finally {
		fs.rmSync(appDir, { recursive: true, force: true });
	}
});

await test("the check names a config that leaves inference auth on, the shape both fixtures wrote before", () => {
	// The config the Bedrock and Vertex fixtures generated before TXE-7039: no client block.
	const before = { framework: { pricing: { pricing_url: "http://127.0.0.1:8794/pricing.json", model_parameters_url: "http://127.0.0.1:8794/model-parameters.json" } }, providers: { bedrock: {} } };
	assert.match(fixtureConfigProblems(before).join("\n"), /enforce_auth_on_inference is not false/);
	// Leaving the key out and setting it to true are the same refusal.
	assert.match(fixtureConfigProblems({ ...before, client: { enforce_auth_on_inference: true } }).join("\n"), /enforce_auth_on_inference is not false/);
	assert.match(fixtureConfigProblems({ ...before, client: {} }).join("\n"), /enforce_auth_on_inference is not false/);
	// A datasheet URL off loopback would send a fresh gateway to the network.
	const online = fixtureGatewayConfig({ datasheetOrigin: "https://getbifrost.ai", providers: { bedrock: {} } });
	assert.match(fixtureConfigProblems(online).join("\n"), /pricing_url is not served by the fixture/);
});

const fixtures = endpointFixtures();

await test("the Bedrock and Vertex endpoint fixtures are found by their use of the library", () => {
	for (const name of ["bedrock-endpoint-fixture.mjs", "vertex-endpoint-fixture.mjs"]) {
		assert.ok(fixtures.includes(name), `${name} not found among ${JSON.stringify(fixtures)}`);
	}
});

await test("no runner writes a gateway config.json of its own", () => {
	// A fixture that builds its config by hand skips the shared settings whatever its arguments are
	// called, so the only writer of an app dir's config.json under runners/ is the library.
	const writers = fs
		.readdirSync(runnersDir)
		.filter((name) => name.endsWith(".mjs"))
		.filter((name) => /writeFileSync\([^)]*config\.json/.test(fs.readFileSync(path.join(runnersDir, name), "utf8")));
	assert.deepStrictEqual(writers, [], `these runners write config.json themselves; use writeFixtureGatewayConfig: ${writers.join(", ")}`);
});

for (const fixture of fixtures) {
	await test(`${fixture}: the config it writes lets its cases reach the provider`, async () => {
		const config = await configWrittenBy(fixture);
		assert.deepStrictEqual(fixtureConfigProblems(config), [], `${fixture} wrote a gateway config that fails its own cases. Write it with writeFixtureGatewayConfig (lib/tls-intercept-proxy.mjs).`);
	});
}

console.log(`\n${passed} passed`);
