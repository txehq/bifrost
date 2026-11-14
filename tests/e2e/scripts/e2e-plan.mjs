// Which spec files a run takes, and how they are split across workers and shards. run-e2e.mjs
// uses these; e2e-plan.test.mjs pins them (TXE-7385).
import { readdirSync, readFileSync } from 'node:fs'
import { join, relative } from 'node:path'

// Every *.spec.ts under <root>/features, relative to root and sorted by name, so the list does not
// depend on the order a filesystem returns directory entries in. The accessibility audit has its
// own config (playwright.a11y.config.ts) and is not part of the suite.
export function specFiles(root, dir = join(root, 'features')) {
  return readdirSync(dir, { withFileTypes: true })
    .flatMap((d) => {
      const path = join(dir, d.name)
      if (d.isDirectory()) return d.name === 'accessibility' ? [] : specFiles(root, path)
      return d.name.endsWith('.spec.ts') ? [relative(root, path)] : []
    })
    .sort()
}

// A spec file's weight from its test count alone: the same in every checkout.
export const testCountWeight = (root) => (file) =>
  Math.max(5000, (readFileSync(join(root, file), 'utf8').match(/\btest(\.skip|\.fixme)?\(/g)?.length ?? 1) * 6000)

// Longest-first greedy bin packing into count bins: each file goes to the lightest bin, the
// lowest-numbered one on a tie. Files of equal weight go in name order, so the result depends on
// the files and weights only, never on the order the files were listed in.
export function packFiles(files, count, weight) {
  const bins = Array.from({ length: Math.max(1, Math.min(count, files.length)) }, () => ({ files: [], estimateMs: 0 }))
  const ordered = [...files].sort((a, b) => weight(b) - weight(a) || (a < b ? -1 : a > b ? 1 : 0))
  for (const file of ordered) {
    const bin = bins.reduce((min, cur) => (cur.estimateMs < min.estimateMs ? cur : min))
    bin.files.push(file)
    bin.estimateMs += weight(file)
  }
  return bins
}

// A spec file's weight for the shard split: its measured seconds from the checked-in
// shard-weights.json, in ms, else its test count. Checked in, so the same in every checkout.
export function shardWeight(root) {
  const measured = JSON.parse(readFileSync(join(root, 'shard-weights.json'), 'utf8'))
  const byCount = testCountWeight(root)
  return (file) => (typeof measured[file] === 'number' ? Math.max(1, measured[file]) * 1000 : byCount(file))
}

// Shard index (1-based) of total: the files that runner takes. Every runner computes the same
// split from the same checkout, so the shards are disjoint and together hold every file.
export function shardFiles(root, files, index, total) {
  return packFiles(files, total, shardWeight(root))[index - 1]?.files ?? []
}
