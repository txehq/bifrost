#!/usr/bin/env node
// Finds the newman the Makefile pins and links it, alone, into --link-dir (fork, TXE-7308).
//
//   node tests/e2e/api/runners/resolve-newman.mjs --root <repo> --link-dir <dir>
//
// The pins are .github/workflows/tools/package.json's newman and newman-reporter-htmlextra, which
// CI installs with `npm ci`; the Makefile reads its NEWMAN_VERSION and NEWMAN_HTMLEXTRA_VERSION
// from the same file. A candidate is accepted only when its newman is the pinned version and the
// reporter it loads is the pinned version too. Candidates, in order: the tools directory's own
// install, then every newman on PATH, in PATH order. So the answer does not depend on where
// `nvm use` put nvm's node, which is what made the offline check find a newman without the
// reporter and refuse the run even with the pinned tools installed.
//
// Prints the link directory on success; the Makefile puts it first on PATH. On failure, exits 1
// and names every candidate it looked at and what each one lacks.
import { createRequire } from "node:module";
import { existsSync, mkdirSync, readFileSync, realpathSync, renameSync, rmSync, symlinkSync } from "node:fs";
import { delimiter, dirname, join, resolve } from "node:path";

export const TOOLS_DIR = join(".github", "workflows", "tools");

// pins reads the pinned versions from the tools package.json.
export function pins(root) {
  const pkg = JSON.parse(readFileSync(join(root, TOOLS_DIR, "package.json"), "utf8"));
  const newman = pkg.dependencies?.newman;
  const htmlextra = pkg.dependencies?.["newman-reporter-htmlextra"];
  if (!newman || !htmlextra) throw new Error(`${join(TOOLS_DIR, "package.json")} pins no newman or newman-reporter-htmlextra`);
  return { newman, htmlextra };
}

const versionAt = (packageJson) => {
  try {
    return JSON.parse(readFileSync(packageJson, "utf8")).version;
  } catch {
    return undefined;
  }
};

// inspect reports what one newman executable is: its package's version and the version of the
// reporter it would load, resolved the way newman resolves it (from its own package).
export function inspect(executable) {
  let real;
  try {
    real = realpathSync(executable);
  } catch {
    return { executable, problem: "not found" };
  }
  // <package>/bin/newman.js
  const packageDir = dirname(dirname(real));
  const newman = versionAt(join(packageDir, "package.json"));
  if (!newman) return { executable, problem: "is not inside a newman package" };
  let htmlextra;
  try {
    htmlextra = versionAt(createRequire(join(packageDir, "package.json")).resolve("newman-reporter-htmlextra/package.json"));
  } catch {
    htmlextra = undefined;
  }
  return { executable, newman, htmlextra };
}

// candidates lists the newman executables to try, in order.
export function candidates(root, path, linkDir) {
  const out = [resolve(root, TOOLS_DIR, "node_modules", ".bin", "newman")];
  for (const dir of (path || "").split(delimiter).filter(Boolean)) {
    if (linkDir && resolve(dir) === resolve(linkDir)) continue;
    // Absolute, so the link made from it resolves from anywhere: a relative PATH entry
    // (node_modules/.bin) would otherwise be read from the link's own directory, and dangle.
    const exe = resolve(dir, "newman");
    if (existsSync(exe) && !out.includes(exe)) out.push(exe);
  }
  return out;
}

// choose returns the first candidate whose newman and reporter are the pinned versions, or the
// reasons none is.
export function choose(found, want) {
  const reasons = [];
  for (const c of found) {
    if (c.problem) {
      reasons.push(`${c.executable}: ${c.problem}`);
    } else if (c.newman !== want.newman) {
      reasons.push(`${c.executable}: newman ${c.newman}, want ${want.newman}`);
    } else if (!c.htmlextra) {
      reasons.push(`${c.executable}: newman ${c.newman} has no newman-reporter-htmlextra beside it`);
    } else if (c.htmlextra !== want.htmlextra) {
      reasons.push(`${c.executable}: newman-reporter-htmlextra ${c.htmlextra}, want ${want.htmlextra}`);
    } else {
      return { chosen: c, reasons };
    }
  }
  return { chosen: null, reasons };
}

// link points linkDir/newman at the chosen executable, replacing an earlier link atomically.
export function link(executable, linkDir) {
  mkdirSync(linkDir, { recursive: true });
  const target = join(linkDir, "newman");
  const temp = `${target}.${process.pid}`;
  rmSync(temp, { force: true });
  symlinkSync(executable, temp);
  renameSync(temp, target);
}

const main = () => {
  const args = Object.fromEntries(
    process.argv.slice(2).reduce((acc, cur, i, arr) => (cur.startsWith("--") ? [...acc, [cur.slice(2), arr[i + 1]]] : acc), [])
  );
  const root = resolve(args.root || process.cwd());
  const linkDir = args["link-dir"] ? resolve(args["link-dir"]) : null;
  const want = pins(root);
  const { chosen, reasons } = choose(candidates(root, process.env.PATH, linkDir).map(inspect), want);
  if (!chosen) {
    console.error(
      `newman ${want.newman} with newman-reporter-htmlextra ${want.htmlextra} (the pins in ${join(TOOLS_DIR, "package.json")}) was not found.\n` +
        reasons.map((r) => `  ${r}`).join("\n") +
        `\nInstall the pinned tools outside the offline runner: (cd ${TOOLS_DIR} && npm ci)`
    );
    process.exit(1);
  }
  if (linkDir) {
    link(chosen.executable, linkDir);
    process.stdout.write(`${linkDir}\n`);
  } else {
    process.stdout.write(`${chosen.executable}\n`);
  }
};

if (import.meta.url === `file://${process.argv[1]}` || process.argv[1]?.endsWith("resolve-newman.mjs")) main();
