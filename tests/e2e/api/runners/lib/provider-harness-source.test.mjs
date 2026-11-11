// The provider harness source: one file per folder, assembled on read (TXE-7136).
// Runs in `API collection offline suites` by existing (every tests/e2e/api *.test.mjs is run).

import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import {
  adoptCollectionChange,
  assembleProviderHarness,
  checkProviderHarnessSource,
  COLLECTION_FILE,
  folderFileName,
  folderNumber,
  ORDER_FILE,
  PROVIDER_HARNESS_DIR,
  readHarnessCollection,
  serialize,
  splitProviderHarness,
} from "./provider-harness-source.mjs";

const CLI = path.join(path.dirname(fileURLToPath(import.meta.url)), "../provider-harness-folders.mjs");
const scratch = fs.mkdtempSync(path.join(os.tmpdir(), "harness-source-"));
test.after(() => fs.rmSync(scratch, { recursive: true, force: true }));

let counter = 0;
const row = (name, body = "{}") => ({ name, request: { method: "POST", url: "{{baseUrl}}/v1/x", body: { mode: "raw", raw: body } } });
const folder = (name, ...rows) => ({ name, item: rows.length ? rows : [row(`${name} row`)] });
const collection = (...folders) => ({
  info: { name: "fixture" },
  event: [{ listen: "test", script: { exec: ["// shared"] } }],
  variable: [{ key: "baseUrl", value: "http://localhost" }],
  item: folders,
});
// A source directory holding `folders`, all frozen in _order.json as the migration left them.
function sourceOf(...folders) {
  const dir = path.join(scratch, `src-${++counter}`, "provider-harness");
  splitProviderHarness(collection(...folders), dir);
  return dir;
}
const check = (dir) => checkProviderHarnessSource(dir, { monolith: path.join(path.dirname(dir), "provider-harness.json") });
const names = (dir) => assembleProviderHarness(dir).item.map((f) => f.name);
const write = (dir, file, value) => fs.writeFileSync(path.join(dir, file), serialize(value));

test("the real source is sound", () => {
  assert.deepEqual(checkProviderHarnessSource(), []);
});

test("the real source assembles to a Postman collection, the same one every time", () => {
  const first = assembleProviderHarness();
  assert.deepEqual(Object.keys(first), ["info", "event", "variable", "item"]);
  assert.ok(first.item.length >= 432, `only ${first.item.length} folders`);
  assert.ok(first.item.every((f) => typeof f.name === "string" && Array.isArray(f.item)));
  assert.equal(serialize(assembleProviderHarness()), serialize(first));
  assert.deepEqual(readHarnessCollection(PROVIDER_HARNESS_DIR), first);
});

test("a split collection assembles back to the same bytes, in the collection's own order", () => {
  const original = collection(folder("2. Second"), folder("1. First"), folder("Unnumbered one"), folder("2. Second again"));
  const dir = path.join(scratch, "roundtrip", "provider-harness");
  const order = splitProviderHarness(original, dir);
  assert.deepEqual(order, ["2-second.json", "1-first.json", "unnumbered-unnumbered-one.json", "2-second-again.json"]);
  assert.equal(serialize(assembleProviderHarness(dir)), serialize(original));
  assert.deepEqual(check(dir), []);
  // An import of the same collection keeps every file name.
  assert.deepEqual(splitProviderHarness(original, dir), order);
});

test("a folder that is not in the frozen order comes after it, by number", () => {
  const dir = sourceOf(folder("9. Old nine"), folder("3. Old three"));
  write(dir, "12-twelve.json", folder("12. Twelve"));
  write(dir, "10-ten.json", folder("10. Ten"));
  write(dir, "5-five.json", folder("5. Five"));
  assert.deepEqual(names(dir), ["9. Old nine", "3. Old three", "5. Five", "10. Ten", "12. Twelve"]);
  assert.deepEqual(check(dir), []);
  assert.deepEqual(JSON.parse(fs.readFileSync(path.join(dir, ORDER_FILE), "utf8")).folders, ["9-old-nine.json", "3-old-three.json"]);
});

test("check fails when a new folder takes a number another folder has", () => {
  const dir = sourceOf(folder("7. Seven"));
  write(dir, "7-another-seven.json", folder("7. Another seven"));
  assert.match(check(dir).join("\n"), /folder number 7 is claimed by 2 files: 7-another-seven\.json, 7-seven\.json/);

  const two = sourceOf(folder("7. Seven"));
  write(two, "8-first.json", folder("8. First eight"));
  write(two, "8-second.json", folder("8. Second eight"));
  assert.match(check(two).join("\n"), /folder number 8 is claimed by 2 files/);
});

test("check keeps the numbers that were already repeated at the split", () => {
  assert.deepEqual(check(sourceOf(folder("115. One"), folder("115. Two"), folder("No number"))), []);
});

test("check fails when the whole collection is back as one file", () => {
  const dir = sourceOf(folder("1. One"));
  fs.writeFileSync(path.join(path.dirname(dir), "provider-harness.json"), serialize(collection(folder("1. One"))));
  assert.match(check(dir).join("\n"), /provider-harness\.json exists/);
});

test("check fails on a file that is not what the tool writes", () => {
  const dir = sourceOf(folder("1. One"));
  fs.writeFileSync(path.join(dir, "2-two.json"), JSON.stringify(folder("2. Two"), null, 4));
  assert.match(check(dir).join("\n"), /2-two\.json: not in the canonical format/);

  const cases = [
    ["3-three.json", folder("4. Four"), /3-three\.json: holds folder 4, so its name must start with "4-"/],
    ["new-folder.json", folder("A folder without a number"), /new-folder\.json: the folder name must start with its number/],
    ["5-five.json", { name: "5. Five" }, /5-five\.json: must hold one folder/],
    ["6-copy.json", folder("1. One"), /hold a folder with the same name: 1\. One/],
    ["Bad_Name.json", folder("9. Nine"), /Bad_Name\.json: not a folder file name/],
  ];
  for (const [file, value, expected] of cases) {
    const each = sourceOf(folder("1. One"));
    write(each, file, value);
    assert.match(check(each).join("\n"), expected);
  }

  const top = sourceOf(folder("1. One"));
  write(top, COLLECTION_FILE, collection(folder("1. One")));
  assert.match(check(top).join("\n"), /_collection\.json carries "item"/);

  const gone = sourceOf(folder("1. One"), folder("2. Two"));
  fs.rmSync(path.join(gone, "2-two.json"));
  assert.match(check(gone).join("\n"), /_order\.json lists 2-two\.json, which does not exist/);
});

test("file names come from the folder name", () => {
  assert.equal(folderNumber("490. Addresses a vendor's answer names"), 490);
  assert.equal(folderNumber("Code mode execution budget"), null);
  assert.equal(folderFileName("490. A vendor's answer (vendor named address) [SERIAL]"), "490-a-vendor-s-answer-vendor-named-address-serial.json");
  assert.equal(folderFileName("12. X", new Set(["12-x.json", "12-x-2.json"])), "12-x-3.json");
  assert.ok(folderFileName(`77. ${"long ".repeat(40)}`).length <= 70);
});

// ---- adopt: a change made to the old single file moves into the folder files ----

test("adopt writes a folder the branch added as a new file", () => {
  const base = collection(folder("1. One"), folder("2. Two"));
  const dir = sourceOf(...base.item);
  // main moved on since the branch was cut: another pull request added folder 50.
  write(dir, "50-fifty.json", folder("50. Fifty"));
  const theirs = collection(folder("1. One"), folder("2. Two"), folder("51. Mine"));
  const report = adoptCollectionChange({ base, theirs, dir });
  assert.deepEqual(report, { added: ["51-mine.json"], changed: [], removed: [], top: [], conflicts: [] });
  assert.deepEqual(names(dir), ["1. One", "2. Two", "50. Fifty", "51. Mine"]);
  assert.deepEqual(check(dir), []);
  // Applying the same change again moves nothing.
  assert.deepEqual(adoptCollectionChange({ base, theirs, dir }).added, []);
});

test("adopt merges rows two branches added to one folder", () => {
  const base = collection(folder("1. One", row("a"), row("b")));
  const dir = sourceOf(...base.item);
  write(dir, "1-one.json", folder("1. One", row("a"), row("b"), row("from main")));
  const theirs = collection(folder("1. One", row("a"), row("mine after a"), row("b", '{"edited":true}')));
  const report = adoptCollectionChange({ base, theirs, dir });
  assert.deepEqual(report.conflicts, []);
  assert.deepEqual(report.changed, ["1-one.json"]);
  const merged = assembleProviderHarness(dir).item[0].item;
  assert.deepEqual(merged.map((r) => r.name), ["a", "mine after a", "b", "from main"]);
  assert.equal(merged[2].request.body.raw, '{"edited":true}');
});

test("adopt merges rows inside a sub-folder, and a row the branch removed", () => {
  const sub = (...rows) => ({ name: "sub", item: rows });
  const base = collection(folder("1. One", sub(row("a")), row("drop me")));
  const dir = sourceOf(...base.item);
  write(dir, "1-one.json", folder("1. One", sub(row("a"), row("main row")), row("drop me")));
  const theirs = collection(folder("1. One", sub(row("a"), row("my row"))));
  const report = adoptCollectionChange({ base, theirs, dir });
  assert.deepEqual(report.conflicts, []);
  const merged = assembleProviderHarness(dir).item[0].item;
  assert.deepEqual(merged.map((r) => r.name), ["sub"]);
  assert.deepEqual(merged[0].item.map((r) => r.name), ["a", "my row", "main row"]);
});

test("adopt reports what both sides changed and leaves main's content", () => {
  const base = collection(folder("1. One", row("a")), folder("2. Two"));
  const dir = sourceOf(...base.item);
  write(dir, "1-one.json", folder("1. One", row("a", '{"main":1}')));
  const theirs = collection(folder("1. One", row("a", '{"mine":1}')), folder("2. Two"), folder("3. Three"));
  const report = adoptCollectionChange({ base, theirs, dir });
  assert.deepEqual(report.conflicts, ["1. One > a: changed on main and on this branch"]);
  assert.deepEqual(report.added, ["3-three.json"]);
  assert.equal(assembleProviderHarness(dir).item[0].item[0].request.body.raw, '{"main":1}');
});

test("adopt moves a collection variable the branch added, and removes a folder it removed", () => {
  const base = collection(folder("1. One"), folder("2. Two"));
  const dir = sourceOf(...base.item);
  const top = JSON.parse(fs.readFileSync(path.join(dir, COLLECTION_FILE), "utf8"));
  top.variable.push({ key: "fromMain", value: "m" });
  write(dir, COLLECTION_FILE, top);
  const theirs = collection(folder("1. One"));
  theirs.variable.push({ key: "mine", value: "x" });
  const report = adoptCollectionChange({ base, theirs, dir });
  assert.deepEqual(report.conflicts, []);
  assert.deepEqual(report.removed, ["2-two.json"]);
  assert.deepEqual(report.top, [COLLECTION_FILE]);
  const assembled = assembleProviderHarness(dir);
  assert.deepEqual(assembled.variable.map((v) => v.key), ["baseUrl", "mine", "fromMain"]);
  assert.deepEqual(assembled.item.map((f) => f.name), ["1. One"]);
  assert.deepEqual(check(dir), []);
});

// ---- the point of the layout: two pull requests that add folders do not conflict ----

function git(cwd, ...argv) {
  return execFileSync("git", argv, {
    cwd,
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
    env: {
      ...process.env,
      GIT_CONFIG_GLOBAL: "/dev/null",
      GIT_CONFIG_SYSTEM: "/dev/null",
      GIT_AUTHOR_NAME: "t",
      GIT_AUTHOR_EMAIL: "t@example.invalid",
      GIT_COMMITTER_NAME: "t",
      GIT_COMMITTER_EMAIL: "t@example.invalid",
    },
  });
}

test("two branches that each add a folder merge in either order, to the same collection", () => {
  const repo = path.join(scratch, "merge-order");
  const dir = path.join(repo, "provider-harness");
  splitProviderHarness(collection(folder("1. One"), folder("2. Two")), dir);
  git(repo, "init", "-q", "-b", "main");
  git(repo, "add", "-A");
  git(repo, "commit", "-q", "-m", "split");
  for (const [branch, file, name] of [
    ["pr-a", "3-from-a.json", "3. From A"],
    ["pr-b", "4-from-b.json", "4. From B"],
  ]) {
    git(repo, "checkout", "-q", "-b", branch, "main");
    write(dir, file, folder(name));
    git(repo, "add", "-A");
    git(repo, "commit", "-q", "-m", branch);
  }
  const assembled = [];
  for (const [first, second] of [
    ["pr-a", "pr-b"],
    ["pr-b", "pr-a"],
  ]) {
    git(repo, "checkout", "-q", "-B", `merge-${first}`, "main");
    git(repo, "merge", "-q", "--no-ff", "-m", `merge ${first}`, first);
    // The second pull request is rebased onto the new main, as the strict queue requires.
    git(repo, "checkout", "-q", "-B", `rebased-${second}`, second);
    git(repo, "rebase", "-q", `merge-${first}`);
    git(repo, "checkout", "-q", `merge-${first}`);
    git(repo, "merge", "-q", "--no-ff", "-m", `merge ${second}`, `rebased-${second}`);
    assert.equal(git(repo, "status", "--porcelain"), "");
    assert.deepEqual(check(dir), []);
    assert.deepEqual(names(dir), ["1. One", "2. Two", "3. From A", "4. From B"]);
    assembled.push(serialize(assembleProviderHarness(dir)));
  }
  assert.equal(assembled[0], assembled[1]);
});

// ---- the tool ----

function cli(...argv) {
  try {
    return { status: 0, out: execFileSync("node", [CLI, ...argv], { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }) };
  } catch (err) {
    return { status: err.status, out: `${err.stdout}${err.stderr}` };
  }
}

test("the tool: check, build, format, adopt by file", () => {
  const base = collection(folder("1. One"));
  const dir = sourceOf(...base.item);
  assert.equal(cli("check", "--dir", dir).status, 0);

  const out = path.join(scratch, "built.json");
  assert.equal(cli("build", "--dir", dir, "--out", out).status, 0);
  assert.equal(fs.readFileSync(out, "utf8"), serialize(base));

  fs.writeFileSync(path.join(dir, "2-two.json"), JSON.stringify(folder("2. Two")));
  assert.equal(cli("check", "--dir", dir).status, 1);
  assert.match(cli("format", "--dir", dir).out, /1 file\(s\) rewritten: 2-two\.json/);
  assert.equal(cli("check", "--dir", dir).status, 0);

  const basePath = path.join(scratch, "adopt-base.json");
  const theirsPath = path.join(scratch, "adopt-theirs.json");
  fs.writeFileSync(basePath, serialize(base));
  fs.writeFileSync(theirsPath, serialize(collection(folder("1. One"), folder("2. Clashes with main's two"))));
  const clash = cli("adopt", "--dir", dir, "--base", basePath, "--theirs", theirsPath);
  assert.equal(clash.status, 1, clash.out);
  assert.match(clash.out, /new folder file\s+2-clashes-with-main-s-two\.json/);
  assert.match(clash.out, /folder number 2 is claimed by 2 files/);
});
