// Tests resolve-newman.mjs (fork, TXE-7308). Run directly: `node resolve-newman.test.mjs`.
//
// The offline tool check found whichever newman `nvm use` put first on PATH. On this fork's
// machines that was nvm's global newman 6.2.1 with no htmlextra reporter, so an offline harness
// run refused itself even with the pinned tools (newman 6.2.2) installed in .github/workflows/tools.
import assert from "node:assert";
import { execFileSync, spawnSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, readlinkSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { TOOLS_DIR, candidates, choose, inspect, pins } from "./resolve-newman.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, "..", "..", "..", "..");
const script = join(here, "resolve-newman.mjs");

let passed = 0;
function test(name, fn) {
  fn();
  passed++;
  console.log(`  ok - ${name}`);
}

const writeJSON = (file, value) => {
  mkdirSync(dirname(file), { recursive: true });
  writeFileSync(file, JSON.stringify(value));
};

// fakeInstall lays out node_modules/newman (and optionally the reporter beside it) under modules,
// and a bin/newman link pointing at it, the way npm does.
const fakeInstall = (modules, binDir, newman, htmlextra) => {
  writeJSON(join(modules, "newman", "package.json"), { name: "newman", version: newman });
  mkdirSync(join(modules, "newman", "bin"), { recursive: true });
  writeFileSync(join(modules, "newman", "bin", "newman.js"), "#!/usr/bin/env node\n");
  if (htmlextra) writeJSON(join(modules, "newman-reporter-htmlextra", "package.json"), { name: "newman-reporter-htmlextra", version: htmlextra });
  mkdirSync(binDir, { recursive: true });
  symlinkSync(join(modules, "newman", "bin", "newman.js"), join(binDir, "newman"));
  return binDir;
};

const withRoot = (fn) => {
  const root = mkdtempSync(join(tmpdir(), "resolve-newman-"));
  try {
    writeJSON(join(root, TOOLS_DIR, "package.json"), { dependencies: { newman: "6.2.2", "newman-reporter-htmlextra": "1.23.1" } });
    return fn(root);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
};

test("the pins are the tools package.json's", () => {
  const tools = JSON.parse(readFileSync(join(repoRoot, TOOLS_DIR, "package.json"), "utf8")).dependencies;
  assert.deepStrictEqual(pins(repoRoot), { newman: tools.newman, htmlextra: tools["newman-reporter-htmlextra"] });
});

test("the pinned install is found whatever order PATH puts the others in", () => withRoot((root) => {
  const nvm = fakeInstall(join(root, "nvm", "lib", "node_modules"), join(root, "nvm", "bin"), "6.2.1", undefined);
  const right = fakeInstall(join(root, "other", "lib", "node_modules"), join(root, "other", "bin"), "6.2.2", "1.23.1");
  for (const path of [[nvm, right], [right, nvm]].map((dirs) => dirs.join(delimiter))) {
    const { chosen } = choose(candidates(root, path).map(inspect), pins(root));
    assert.ok(chosen, `nothing chosen with PATH ${path}`);
    assert.strictEqual(chosen.executable, join(right, "newman"));
  }
}));

test("the tools directory's own install is preferred to any newman on PATH", () => withRoot((root) => {
  fakeInstall(join(root, TOOLS_DIR, "node_modules"), join(root, TOOLS_DIR, "node_modules", ".bin"), "6.2.2", "1.23.1");
  const other = fakeInstall(join(root, "other", "lib", "node_modules"), join(root, "other", "bin"), "6.2.2", "1.23.1");
  const { chosen } = choose(candidates(root, other).map(inspect), pins(root));
  assert.strictEqual(chosen.executable, join(root, TOOLS_DIR, "node_modules", ".bin", "newman"));
}));

test("with no pinned install, the failure names each newman and what it lacks", () => withRoot((root) => {
  const nvm = fakeInstall(join(root, "nvm", "lib", "node_modules"), join(root, "nvm", "bin"), "6.2.1", "1.23.1");
  const noReporter = fakeInstall(join(root, "bare", "lib", "node_modules"), join(root, "bare", "bin"), "6.2.2", undefined);
  const oldReporter = fakeInstall(join(root, "old", "lib", "node_modules"), join(root, "old", "bin"), "6.2.2", "1.20.0");
  const result = spawnSync("node", [script, "--root", root], { encoding: "utf8", env: { ...process.env, PATH: [nvm, noReporter, oldReporter, dirname(process.execPath)].join(delimiter) } });
  assert.strictEqual(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stderr, /newman 6\.2\.2 with newman-reporter-htmlextra 1\.23\.1 \(the pins in \.github\/workflows\/tools\/package\.json\) was not found/);
  assert.match(result.stderr, /node_modules\/\.bin\/newman: not found/);
  assert.match(result.stderr, new RegExp(`${join(nvm, "newman").replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}: newman 6\\.2\\.1, want 6\\.2\\.2`));
  assert.match(result.stderr, /newman 6\.2\.2 has no newman-reporter-htmlextra beside it/);
  assert.match(result.stderr, /newman-reporter-htmlextra 1\.20\.0, want 1\.23\.1/);
  assert.match(result.stderr, /\(cd \.github\/workflows\/tools && npm ci\)/);
}));

test("--link-dir links the chosen newman alone and prints the directory", () => withRoot((root) => {
  const right = fakeInstall(join(root, "other", "lib", "node_modules"), join(root, "other", "bin"), "6.2.2", "1.23.1");
  const linkDir = join(root, "tmp", "pinned-newman");
  const env = { ...process.env, PATH: [right, dirname(process.execPath)].join(delimiter) };
  for (let i = 0; i < 2; i++) {
    const out = execFileSync("node", [script, "--root", root, "--link-dir", linkDir], { encoding: "utf8", env });
    assert.strictEqual(out.trim(), linkDir);
    assert.strictEqual(readlinkSync(join(linkDir, "newman")), join(right, "newman"), "a second run replaces the link");
  }
}));

test("a relative PATH entry is linked by its absolute path, so the link never dangles", () => withRoot((root) => {
  fakeInstall(join(root, "rel", "lib", "node_modules"), join(root, "rel", "bin"), "6.2.2", "1.23.1");
  const linkDir = join(root, "tmp", "pinned-newman");
  const env = { ...process.env, PATH: ["rel/bin", dirname(process.execPath)].join(delimiter) };
  execFileSync("node", [script, "--root", root, "--link-dir", linkDir], { encoding: "utf8", env, cwd: root });
  const target = readlinkSync(join(linkDir, "newman"));
  assert.ok(target.startsWith("/"), `link target ${target} is relative`);
  assert.ok(existsSync(join(linkDir, "newman")), "the link resolves");
}));

test("the Makefile takes its pins from the tools package.json and pins newman after nvm", () => {
  const makefile = readFileSync(join(repoRoot, "Makefile"), "utf8");
  for (const name of ["NEWMAN_VERSION", "NEWMAN_HTMLEXTRA_VERSION"]) {
    const line = makefile.match(new RegExp(`^${name}\\s*\\??=\\s*(.*)$`, "m"));
    assert.ok(line, `${name} not defined`);
    assert.match(line[1], /\.github\/workflows\/tools\/package\.json/, `${name} must be read from the tools package.json, not restated`);
  }
  const useNode = makefile.match(/^USE_NODE = [\s\S]*?\n(?=\S|\n)/m)?.[0] || "";
  assert.match(useNode, /nvm use[\s\S]*\$\(PIN_NEWMAN\)/, "USE_NODE must pin newman after nvm use");
  assert.match(makefile, /^PIN_NEWMAN = .*resolve-newman\.mjs/m);
  const install = makefile.slice(makefile.indexOf("install-newman:"), makefile.indexOf("run-provider-harness-test:"));
  assert.match(install, /resolve-newman\.mjs/, "install-newman must use the resolver");
  assert.doesNotMatch(install, /which newman/, "install-newman must not trust the first newman on PATH");
});

console.log(`${passed} passed`);
