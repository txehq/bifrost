// Pins how the offline UI job splits the suite (TXE-7385): every spec file runs in exactly one
// shard, and two runners compute the same split. Run: node --test tests/e2e/scripts/e2e-plan.test.mjs
import assert from 'node:assert/strict'
import { dirname, join } from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'
import { readFileSync } from 'node:fs'
import { packFiles, shardFiles, shardWeight, specFiles } from './e2e-plan.mjs'

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..')

test('specFiles lists the suite sorted, without the accessibility audit', () => {
  const files = specFiles(ROOT)
  assert.ok(files.length >= 40, `found ${files.length} spec files, want the whole suite`)
  assert.deepEqual(files, [...files].sort())
  assert.ok(files.every((f) => f.startsWith('features/') && f.endsWith('.spec.ts')))
  assert.ok(!files.some((f) => f.startsWith('features/accessibility/')))
})

test('the shards of the real suite are disjoint and cover every spec file', () => {
  const files = specFiles(ROOT)
  for (const total of [2, 3]) {
    const shards = Array.from({ length: total }, (_, i) => shardFiles(ROOT, files, i + 1, total))
    assert.ok(shards.every((s) => s.length > 0), `an empty shard of ${total}`)
    const all = shards.flat()
    assert.equal(all.length, files.length, `${total} shards hold ${all.length} files, want ${files.length}`)
    assert.deepEqual([...all].sort(), files)
  }
})

test('the split depends on the files, not on the order they were listed in', () => {
  const files = specFiles(ROOT)
  const reversed = [...files].reverse()
  const shuffled = [...files].sort((a, b) => (a.length - b.length) || (b < a ? -1 : 1))
  for (const listing of [reversed, shuffled]) {
    for (const index of [1, 2]) assert.deepEqual(shardFiles(ROOT, listing, index, 2), shardFiles(ROOT, files, index, 2))
  }
})

test('shard-weights.json names only spec files that exist, and balances the two shards', () => {
  const files = specFiles(ROOT)
  const measured = JSON.parse(readFileSync(join(ROOT, 'shard-weights.json'), 'utf8'))
  const stale = Object.keys(measured).filter((k) => !k.startsWith('_') && !files.includes(k))
  assert.deepEqual(stale, [], 'remove these from shard-weights.json: the spec files are gone')
  const weight = shardWeight(ROOT)
  const [a, b] = [1, 2].map((i) => shardFiles(ROOT, files, i, 2).reduce((sum, f) => sum + weight(f), 0))
  assert.ok(Math.abs(a - b) <= Math.max(a, b) * 0.25, `shard estimates ${a / 1000}s and ${b / 1000}s differ by more than a quarter`)
})

test('packFiles breaks weight ties by name and fills the lightest bin first', () => {
  const weight = (f) => ({ a: 3, b: 3, c: 2, d: 1 })[f]
  assert.deepEqual(packFiles(['d', 'c', 'b', 'a'], 2, weight).map((bin) => bin.files), [['a', 'c'], ['b', 'd']])
  assert.deepEqual(packFiles(['a', 'b', 'c', 'd'], 2, weight).map((bin) => bin.files), [['a', 'c'], ['b', 'd']])
  assert.deepEqual(packFiles(['a'], 3, weight).map((bin) => bin.files), [['a']])
})
