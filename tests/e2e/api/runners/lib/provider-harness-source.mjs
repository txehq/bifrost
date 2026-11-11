// The provider harness is stored as one file per top-level folder (TXE-7136):
//
//   tests/e2e/api/collections/provider-harness/
//     _collection.json   everything above the folders: info, event, variable
//     _order.json        the frozen order of the folders that existed at the split
//     <number>-<slug>.json   one top-level folder each, exactly as it sits in "item"
//
// One file held every folder before, and almost every pull request appended one, so every
// merge to main conflicted with every queued pull request. Two pull requests that add
// different folders now touch different files.
//
// assembleProviderHarness() is the one build step. Everything that reads the harness
// (augment-provider-harness.mjs, the script tests, the Makefile target) goes through it,
// and nothing assembled is committed: a committed copy would be the shared file again.
//
// Order. The folders listed in _order.json come first, in that order, so the assembled
// collection is byte for byte the file that was split. A folder not listed there comes
// after them, by its number. _order.json is rewritten only by `split` (a whole collection
// imported, as an upstream sync does); a pull request that adds a folder never edits it.

import { existsSync, mkdirSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));

export const PROVIDER_HARNESS_DIR = resolve(HERE, "../../collections/provider-harness");
// Where the single file used to be. It must not come back: see checkProviderHarnessSource.
export const PROVIDER_HARNESS_MONOLITH = resolve(HERE, "../../collections/provider-harness.json");
export const COLLECTION_FILE = "_collection.json";
export const ORDER_FILE = "_order.json";

const FOLDER_FILE_RE = /^[a-z0-9]+(?:-[a-z0-9]+)*\.json$/;
const ORDER_NOTE =
  "Frozen order of the folders that existed when the collection was split (TXE-7136). " +
  "Do not add a new folder here: a folder file that is not listed is placed after these, by its number. " +
  "Only `provider-harness-folders.mjs split` rewrites this file.";

export const serialize = (value) => JSON.stringify(value, null, 2) + "\n";
const same = (a, b) => JSON.stringify(a) === JSON.stringify(b);
const clone = (value) => JSON.parse(JSON.stringify(value));

// "490. Addresses a vendor's answer names ..." -> 490. null when the name has no number.
export function folderNumber(name) {
  const m = /^(\d+)\. /.exec(String(name));
  return m ? Number(m[1]) : null;
}

function slug(text) {
  const s = String(text)
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 60)
    .replace(/-+$/g, "");
  return s || "folder";
}

// The file a folder is written to: "<number>-<slug>.json", or "unnumbered-<slug>.json".
// `taken` holds the names already in use; a clash gets "-2", "-3", ...
export function folderFileName(name, taken = new Set()) {
  const number = folderNumber(name);
  const stem =
    number === null ? `unnumbered-${slug(name)}` : `${number}-${slug(String(name).replace(/^\d+\.\s*/, ""))}`;
  let candidate = `${stem}.json`;
  for (let n = 2; taken.has(candidate); n++) candidate = `${stem}-${n}.json`;
  return candidate;
}

function readJson(path) {
  try {
    return JSON.parse(readFileSync(path, "utf8"));
  } catch (err) {
    throw new Error(`${path}: ${err.message}`);
  }
}

export function listFolderFiles(dir = PROVIDER_HARNESS_DIR) {
  return readdirSync(dir)
    .filter((f) => f.endsWith(".json") && !f.startsWith("_"))
    .sort();
}

// The source as it is on disk: the top of the collection, the frozen order, and each
// folder file's parsed folder.
export function loadProviderHarnessSource(dir = PROVIDER_HARNESS_DIR) {
  if (!existsSync(join(dir, COLLECTION_FILE))) {
    throw new Error(`${join(dir, COLLECTION_FILE)} not found: the provider harness is stored one file per folder there`);
  }
  const top = readJson(join(dir, COLLECTION_FILE));
  const order = existsSync(join(dir, ORDER_FILE)) ? readJson(join(dir, ORDER_FILE)).folders || [] : [];
  const folders = new Map();
  for (const file of listFolderFiles(dir)) folders.set(file, readJson(join(dir, file)));
  return { dir, top, order, folders };
}

// The folder files in collection order: the frozen ones first, then the rest by number.
export function orderedFolderFiles(source) {
  const listed = source.order.filter((f) => source.folders.has(f));
  const frozen = new Set(source.order);
  const rest = [...source.folders.keys()]
    .filter((f) => !frozen.has(f))
    .sort((a, b) => {
      const na = folderNumber(source.folders.get(a).name) ?? Infinity;
      const nb = folderNumber(source.folders.get(b).name) ?? Infinity;
      return na - nb || (a < b ? -1 : a > b ? 1 : 0);
    });
  return [...listed, ...rest];
}

// The build step: the Postman collection newman runs, from the per-folder files.
export function assembleProviderHarness(dir = PROVIDER_HARNESS_DIR) {
  const source = loadProviderHarnessSource(dir);
  return { ...source.top, item: orderedFolderFiles(source).map((f) => source.folders.get(f)) };
}

// Reads a harness collection from either form: the per-folder directory, or one JSON
// file (an augmented or filtered collection, or a collection from before the split).
export function readHarnessCollection(path = PROVIDER_HARNESS_DIR) {
  if (existsSync(path) && existsSync(join(path, COLLECTION_FILE))) return assembleProviderHarness(path);
  return readJson(path);
}

// What is wrong with the source, as a list of sentences; empty when nothing is.
export function checkProviderHarnessSource(dir = PROVIDER_HARNESS_DIR, { monolith = PROVIDER_HARNESS_MONOLITH } = {}) {
  const problems = [];
  if (monolith && existsSync(monolith)) {
    problems.push(
      `${monolith} exists. The harness is stored one file per folder in ${dir}; a copy of the whole collection ` +
        "beside it is the shared file every pull request conflicted on. Move its changes into the folder files " +
        "(node tests/e2e/api/runners/provider-harness-folders.mjs adopt) and delete it."
    );
  }
  let source;
  try {
    source = loadProviderHarnessSource(dir);
  } catch (err) {
    return [...problems, err.message];
  }
  const { top, order, folders } = source;

  if (!top || typeof top !== "object" || Array.isArray(top)) problems.push(`${COLLECTION_FILE} is not a JSON object`);
  else if ("item" in top) problems.push(`${COLLECTION_FILE} carries "item": folders belong in their own files`);

  for (const name of readdirSync(dir)) {
    if (name === COLLECTION_FILE || name === ORDER_FILE) continue;
    if (!FOLDER_FILE_RE.test(name)) {
      problems.push(`${name}: not a folder file name (lowercase letters, digits and single hyphens, ending .json)`);
    }
  }

  const frozen = new Set(order);
  if (frozen.size !== order.length) problems.push(`${ORDER_FILE} lists a file twice`);
  for (const file of order) {
    if (!folders.has(file)) problems.push(`${ORDER_FILE} lists ${file}, which does not exist`);
  }

  const byNumber = new Map();
  const byName = new Map();
  for (const [file, folder] of folders) {
    const raw = readFileSync(join(dir, file), "utf8");
    if (raw !== serialize(folder)) {
      problems.push(`${file}: not in the canonical format (2-space JSON, one trailing newline). Run: provider-harness-folders.mjs format`);
    }
    if (!folder || typeof folder !== "object" || typeof folder.name !== "string" || !Array.isArray(folder.item)) {
      problems.push(`${file}: must hold one folder, an object with "name" and an "item" array`);
      continue;
    }
    if (byName.has(folder.name)) problems.push(`${file} and ${byName.get(folder.name)} hold a folder with the same name: ${folder.name}`);
    byName.set(folder.name, file);

    const number = folderNumber(folder.name);
    if (number === null) {
      if (!frozen.has(file)) problems.push(`${file}: the folder name must start with its number, as in "491. What it covers"`);
      continue;
    }
    if (!file.startsWith(`${number}-`)) problems.push(`${file}: holds folder ${number}, so its name must start with "${number}-"`);
    if (!byNumber.has(number)) byNumber.set(number, []);
    byNumber.get(number).push(file);
  }
  // Numbers repeated among the folders that existed at the split are kept as they are
  // (renumbering them would break every FOLDER= filter and smoke-manifest entry that
  // names them). A folder added since must not share a number with any other.
  for (const [number, files] of byNumber) {
    if (files.length > 1 && files.some((f) => !frozen.has(f))) {
      problems.push(`folder number ${number} is claimed by ${files.length} files: ${files.join(", ")}. Give the new folder a free number.`);
    }
  }

  if (readFileSync(join(dir, COLLECTION_FILE), "utf8") !== serialize(top)) {
    problems.push(`${COLLECTION_FILE}: not in the canonical format (2-space JSON, one trailing newline)`);
  }
  if (existsSync(join(dir, ORDER_FILE)) && readFileSync(join(dir, ORDER_FILE), "utf8") !== serialize(readJson(join(dir, ORDER_FILE)))) {
    problems.push(`${ORDER_FILE}: not in the canonical format (2-space JSON, one trailing newline)`);
  }
  return problems;
}

// Writes a whole collection as per-folder files, replacing what the directory holds.
// Every folder of the collection is frozen in _order.json, in the collection's order, so
// assembling the result gives the collection back exactly. A folder keeps the file it
// already has (matched by name), so an import does not rename files.
export function splitProviderHarness(collection, dir = PROVIDER_HARNESS_DIR) {
  if (!collection || !Array.isArray(collection.item)) throw new Error("not a Postman collection: no item array");
  const existing = new Map();
  if (existsSync(dir)) {
    for (const file of listFolderFiles(dir)) {
      try {
        existing.set(readJson(join(dir, file)).name, file);
      } catch {
        // an unreadable file is replaced
      }
    }
    for (const file of readdirSync(dir)) if (file.endsWith(".json")) rmSync(join(dir, file));
  }
  mkdirSync(dir, { recursive: true });

  const { item, ...top } = collection;
  writeFileSync(join(dir, COLLECTION_FILE), serialize(top));
  const reserved = new Set(item.map((f) => existing.get(f.name)).filter(Boolean));
  const taken = new Set();
  const order = [];
  for (const folder of item) {
    if (!folder || typeof folder.name !== "string" || !Array.isArray(folder.item)) {
      throw new Error(`top-level entry ${JSON.stringify(folder && folder.name)} is not a folder`);
    }
    let file = existing.get(folder.name);
    if (!file || taken.has(file)) file = folderFileName(folder.name, new Set([...taken, ...reserved]));
    taken.add(file);
    order.push(file);
    writeFileSync(join(dir, file), serialize(folder));
  }
  writeFileSync(join(dir, ORDER_FILE), serialize({ note: ORDER_NOTE, folders: order }));
  return order;
}

// ---- adopt: move one change made to the single file into the folder files ----
//
// A branch cut before the split changed provider-harness.json. `base` is that file before
// the change and `theirs` after it. The difference between them is applied to the folder
// files, which hold main's current content: a new folder becomes a new file, and a folder
// both sides changed is merged row by row (rows are matched by name). What cannot be
// merged is reported and left as main has it.

function keyedIndex(list, keyOf) {
  const index = new Map();
  for (const entry of list) {
    const key = keyOf(entry);
    if (index.has(key)) return null; // not unique: the list cannot be merged entry by entry
    index.set(key, entry);
  }
  return index;
}

// Three-way merge of a list whose entries are matched by key. Returns the merged list;
// pushes a sentence to `conflicts` for each entry it had to leave as `current` has it.
function mergeKeyedList(base, current, theirs, keyOf, mergeEntry, where, conflicts) {
  if (same(base, theirs)) return current;
  if (same(base, current)) return clone(theirs);
  if (same(current, theirs)) return current;
  const b = keyedIndex(base, keyOf);
  const c = keyedIndex(current, keyOf);
  const t = keyedIndex(theirs, keyOf);
  if (!b || !c || !t) {
    conflicts.push(`${where}: changed on main and on this branch, and its entries cannot be matched by name (a name repeats)`);
    return current;
  }
  // Entries are merged where main has them, so an order this branch changed would be lost
  // without a word (a producer moved ahead of its consumer, say). That is a hand merge.
  const kept = (list) => list.map(keyOf).filter((key) => b.has(key) && t.has(key));
  if (!same(kept(base), kept(theirs))) {
    conflicts.push(`${where}: this branch reordered its entries and main changed it too; main's order is kept`);
  }
  let result = current.slice();
  const position = (key) => result.findIndex((entry) => keyOf(entry) === key);
  let previous = null;
  for (const entry of theirs) {
    const key = keyOf(entry);
    const was = b.get(key);
    const now = c.get(key);
    if (was === undefined) {
      if (now === undefined) {
        // After the entry that precedes it on this branch; when main no longer has that
        // one, before the next entry of this branch that main does have.
        let at = previous === null ? 0 : position(previous) + 1;
        if (previous !== null && at === 0) {
          const following = theirs.slice(theirs.indexOf(entry) + 1).map(keyOf).find((k) => position(k) !== -1);
          at = following === undefined ? result.length : position(following);
        }
        result.splice(at, 0, clone(entry));
      } else if (!same(now, entry)) {
        conflicts.push(`${where} > ${key}: added on main and on this branch with different content`);
      }
    } else if (!same(was, entry)) {
      if (now === undefined) conflicts.push(`${where} > ${key}: changed on this branch, removed on main`);
      else if (same(now, was)) result[position(key)] = clone(entry);
      else if (!same(now, entry)) {
        const merged = mergeEntry ? mergeEntry(was, now, entry, `${where} > ${key}`, conflicts) : null;
        if (merged) result[position(key)] = merged;
        else if (!mergeEntry) conflicts.push(`${where} > ${key}: changed on main and on this branch`);
      }
    }
    previous = key;
  }
  for (const entry of base) {
    const key = keyOf(entry);
    if (t.has(key)) continue;
    const now = c.get(key);
    if (now === undefined) continue;
    if (same(now, entry)) result = result.filter((e) => keyOf(e) !== key);
    else conflicts.push(`${where} > ${key}: removed on this branch, changed on main`);
  }
  return result;
}

// Three-way merge of a folder (or a row that is a sub-folder). A row that is a request is
// not merged field by field: two different edits to one request are a conflict.
function mergeFolder(base, current, theirs, where, conflicts) {
  const isFolder = (x) => x && Array.isArray(x.item);
  if (!isFolder(base) || !isFolder(current) || !isFolder(theirs)) {
    conflicts.push(`${where}: changed on main and on this branch`);
    return null;
  }
  const result = { ...current };
  for (const key of new Set([...Object.keys(base), ...Object.keys(current), ...Object.keys(theirs)])) {
    if (key === "item") continue;
    if (same(base[key], theirs[key]) || same(current[key], theirs[key])) continue;
    if (same(base[key], current[key])) {
      if (theirs[key] === undefined) delete result[key];
      else result[key] = clone(theirs[key]);
    } else conflicts.push(`${where}: "${key}" changed on main and on this branch`);
  }
  result.item = mergeKeyedList(base.item, current.item, theirs.item, (row) => row.name, mergeFolder, where, conflicts);
  return result;
}

export function adoptCollectionChange({ base, theirs, dir = PROVIDER_HARNESS_DIR }) {
  const source = loadProviderHarnessSource(dir);
  const report = { added: [], changed: [], removed: [], top: [], conflicts: [] };
  const fileOf = new Map([...source.folders].map(([file, folder]) => [folder.name, file]));
  const baseByName = keyedIndex(base.item, (f) => f.name);
  const theirsByName = keyedIndex(theirs.item, (f) => f.name);
  if (!baseByName || !theirsByName) throw new Error("a folder name repeats in the collection: its folders cannot be matched by name");

  // A folder keeps the place main gives it (the frozen order, then by number), so an order
  // of folders this commit changed cannot be carried over. Say so rather than drop it.
  const keptOrder = (list) => list.map((f) => f.name).filter((name) => baseByName.has(name) && theirsByName.has(name));
  if (!same(keptOrder(base.item), keptOrder(theirs.item))) {
    report.conflicts.push(
      "the order of the top-level folders: this branch reordered them; the folder files keep main's order " +
        "(a new folder is placed by its number, so renumber a folder that must run later)"
    );
  }

  for (const folder of theirs.item) {
    const was = baseByName.get(folder.name);
    if (was && same(was, folder)) continue;
    const file = fileOf.get(folder.name);
    if (!file) {
      if (was) {
        report.conflicts.push(`${folder.name}: changed on this branch, but main no longer has a folder of that name`);
        continue;
      }
      const name = folderFileName(folder.name, new Set(readdirSync(dir)));
      writeFileSync(join(dir, name), serialize(folder));
      report.added.push(name);
      continue;
    }
    const current = source.folders.get(file);
    if (same(current, folder)) continue;
    const conflicts = [];
    const merged = mergeFolder(was || { name: folder.name, item: [] }, current, folder, folder.name, conflicts);
    report.conflicts.push(...conflicts);
    if (merged && !same(merged, current)) {
      writeFileSync(join(dir, file), serialize(merged));
      report.changed.push(file);
    }
  }
  for (const folder of base.item) {
    if (theirsByName.has(folder.name)) continue;
    const file = fileOf.get(folder.name);
    if (!file) continue;
    if (same(source.folders.get(file), folder)) {
      rmSync(join(dir, file));
      report.removed.push(file);
      if (source.order.includes(file)) {
        const orderPath = join(dir, ORDER_FILE);
        const order = readJson(orderPath);
        order.folders = order.folders.filter((f) => f !== file);
        writeFileSync(orderPath, serialize(order));
      }
    } else report.conflicts.push(`${folder.name}: removed (or renamed) on this branch, changed on main`);
  }

  // Everything above the folders: info, the collection scripts, the collection variables.
  const { item: _b, ...baseTop } = base;
  const { item: _t, ...theirsTop } = theirs;
  if (!same(baseTop, theirsTop)) {
    const top = { ...source.top };
    for (const key of new Set([...Object.keys(baseTop), ...Object.keys(theirsTop)])) {
      if (same(baseTop[key], theirsTop[key]) || same(top[key], theirsTop[key])) continue;
      const conflicts = [];
      if (key === "variable" && [baseTop[key], top[key], theirsTop[key]].every(Array.isArray)) {
        top[key] = mergeKeyedList(baseTop[key], top[key], theirsTop[key], (v) => v.key, null, "collection variable", conflicts);
      } else if (same(baseTop[key], top[key])) {
        if (theirsTop[key] === undefined) delete top[key];
        else top[key] = clone(theirsTop[key]);
      } else conflicts.push(`collection "${key}": changed on main and on this branch`);
      report.conflicts.push(...conflicts);
    }
    if (!same(top, source.top)) {
      writeFileSync(join(dir, COLLECTION_FILE), serialize(top));
      report.top.push(COLLECTION_FILE);
    }
  }
  return report;
}

// Rewrites every source file in the canonical format. A hand edit with another indent, or
// a missing final newline, is the only thing it changes.
export function formatProviderHarnessSource(dir = PROVIDER_HARNESS_DIR) {
  const rewritten = [];
  for (const file of readdirSync(dir).filter((f) => f.endsWith(".json"))) {
    const path = join(dir, file);
    const raw = readFileSync(path, "utf8");
    const canonical = serialize(JSON.parse(raw));
    if (raw !== canonical) {
      writeFileSync(path, canonical);
      rewritten.push(file);
    }
  }
  return rewritten;
}
