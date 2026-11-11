#!/usr/bin/env node
// The provider harness is stored one file per folder (lib/provider-harness-source.mjs,
// TXE-7136). This is the tool for that layout.
//
//   check                      Fail when the source is not sound: a copy of the whole collection
//                              is back, two folders claim one number, a file is not canonical.
//   build [--out <file>]       Write the assembled collection (default tmp/provider-harness.json),
//                              for jq, Postman, or anything that wants one file. Never commit it.
//   format                     Rewrite the source files in the canonical format.
//   adopt [--commit <rev>]     Move a change a commit made to the old single file into the folder
//         [--base <file> --theirs <file>]
//                              files. During a rebase the commit is REBASE_HEAD, so plain `adopt`
//                              is the whole step: it also removes the single file the stopped
//                              rebase left behind and stages the folder files. Exits 1 when
//                              something needs a hand merge. With --base and --theirs it reads
//                              two collection files and touches nothing in git.
//   split --from <file>        Replace the folder files with a whole collection (an upstream sync).
//
// --dir <path> points every command at another source directory.

import { execFileSync } from "node:child_process";
import { mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { dirname, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import {
  adoptCollectionChange,
  assembleProviderHarness,
  checkProviderHarnessSource,
  formatProviderHarnessSource,
  listFolderFiles,
  PROVIDER_HARNESS_DIR,
  PROVIDER_HARNESS_MONOLITH,
  serialize,
  splitProviderHarness,
} from "./lib/provider-harness-source.mjs";

const REPO = resolve(dirname(fileURLToPath(import.meta.url)), "../../../..");
const MONOLITH_IN_REPO = "tests/e2e/api/collections/provider-harness.json";

const [command, ...rest] = process.argv.slice(2);
const args = {};
for (let i = 0; i < rest.length; i++) {
  if (!rest[i].startsWith("--")) fail(`unexpected argument: ${rest[i]}`);
  const next = rest[i + 1];
  args[rest[i].slice(2)] = next && !next.startsWith("--") ? rest[++i] : "true";
}
const dir = args.dir ? resolve(args.dir) : PROVIDER_HARNESS_DIR;
const shown = (path) => relative(process.cwd(), path) || ".";

function fail(message) {
  console.error(`[provider-harness-folders] ${message}`);
  process.exit(2);
}

function git(...argv) {
  return execFileSync("git", argv, { cwd: REPO, encoding: "utf8", maxBuffer: 1 << 30, stdio: ["ignore", "pipe", "pipe"] });
}

function check() {
  // A copy of the whole collection is looked for beside the real source only.
  const problems = checkProviderHarnessSource(dir, { monolith: args.dir ? null : PROVIDER_HARNESS_MONOLITH });
  if (problems.length === 0) {
    console.log(`[provider-harness-folders] ok: ${listFolderFiles(dir).length} folders in ${shown(dir)}`);
    return 0;
  }
  for (const problem of problems) console.error(`[provider-harness-folders] ${problem}`);
  console.error(`[provider-harness-folders] ${problems.length} problem(s) in ${shown(dir)}`);
  return 1;
}

function collectionAt(rev) {
  try {
    return JSON.parse(git("show", `${rev}:${MONOLITH_IN_REPO}`));
  } catch (err) {
    fail(`cannot read ${MONOLITH_IN_REPO} at ${rev}: ${String(err.stderr || err.message).trim()}`);
  }
}

function adopt() {
  let base;
  let theirs;
  const fromGit = !(args.base || args.theirs);
  if (args.base || args.theirs) {
    if (!args.base || !args.theirs) fail("adopt takes --base and --theirs together");
    base = JSON.parse(readFileSync(args.base, "utf8"));
    theirs = JSON.parse(readFileSync(args.theirs, "utf8"));
  } else {
    let rev = args.commit;
    if (!rev) {
      for (const head of ["REBASE_HEAD", "CHERRY_PICK_HEAD"]) {
        try {
          git("rev-parse", "-q", "--verify", head);
          rev = head;
          break;
        } catch {
          // not in that operation
        }
      }
    }
    if (!rev) fail("no rebase or cherry-pick is stopped here: name the commit with --commit <rev>, or pass --base and --theirs");
    base = collectionAt(`${rev}~1`);
    theirs = collectionAt(rev);
  }

  const report = adoptCollectionChange({ base, theirs, dir });
  if (fromGit) {
    // The stopped rebase left the commit's copy of the single file in the tree, unmerged.
    // Its change is in the folder files now, so the conflict is settled as "deleted".
    git("rm", "-q", "-f", "--ignore-unmatch", "--", MONOLITH_IN_REPO);
    rmSync(PROVIDER_HARNESS_MONOLITH, { force: true });
    git("add", "-A", "--", relative(REPO, dir));
  }
  for (const file of report.added) console.log(`  new folder file   ${file}`);
  for (const file of report.changed) console.log(`  merged into       ${file}`);
  for (const file of report.removed) console.log(`  removed           ${file}`);
  for (const file of report.top) console.log(`  merged into       ${file}`);
  if (report.added.length + report.changed.length + report.removed.length + report.top.length === 0) {
    console.log("  nothing to move: the folder files already hold this change");
  }
  for (const conflict of report.conflicts) console.error(`  NOT MERGED        ${conflict}`);
  if (report.conflicts.length > 0) {
    console.error(
      `[provider-harness-folders] ${report.conflicts.length} change(s) need a hand merge. The folder files hold main's ` +
        "content for them; edit the file and re-run `check`."
    );
  }
  const status = check();
  if (report.conflicts.length > 0) return 1;
  if (fromGit && status === 0) {
    console.log(`[provider-harness-folders] ${MONOLITH_IN_REPO} is removed and the folder files are staged. Next: git rebase --continue`);
  }
  return status;
}

switch (command) {
  case "check":
    process.exit(check());
    break;
  case "build": {
    const out = resolve(args.out || resolve(REPO, "tmp/provider-harness.json"));
    if (out === PROVIDER_HARNESS_MONOLITH) fail(`${MONOLITH_IN_REPO} is where the single file used to be; write the assembled copy elsewhere`);
    const collection = assembleProviderHarness(dir);
    mkdirSync(dirname(out), { recursive: true });
    writeFileSync(out, serialize(collection));
    console.log(`[provider-harness-folders] ${collection.item.length} folders -> ${shown(out)}`);
    break;
  }
  case "format": {
    const rewritten = formatProviderHarnessSource(dir);
    console.log(`[provider-harness-folders] ${rewritten.length} file(s) rewritten${rewritten.length ? ": " + rewritten.join(", ") : ""}`);
    break;
  }
  case "adopt":
    process.exit(adopt());
    break;
  case "split": {
    if (!args.from) fail("split takes --from <collection file>");
    const order = splitProviderHarness(JSON.parse(readFileSync(args.from, "utf8")), dir);
    console.log(`[provider-harness-folders] ${order.length} folders written to ${shown(dir)}`);
    process.exit(check());
    break;
  }
  default:
    fail("usage: provider-harness-folders.mjs check | build [--out <file>] | format | adopt [--commit <rev>] | split --from <file>");
}
