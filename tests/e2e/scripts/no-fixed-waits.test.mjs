// No fixed sleeps in the Playwright UI suite (TXE-7508).
//
// A test that waits a fixed time is either too short on a loaded CI runner, and fails, or too long
// everywhere else. The suite had 27 of them (waitForTimeout, setTimeout promises, a wait() helper);
// the flaky virtual-keys and provider-settings retries on shard 1 ran through them. A wait names the
// state it waits for instead: a locator assertion, expect.poll, a response, or the page rendering
// (core/utils/test-helpers.ts nextFrames).
//
// The rule covers every TypeScript file of the suite: specs, page objects, fixtures, helpers, the
// global setup and the reporters. tests/e2e/api (the newman collections) and these runner scripts
// are not the suite. A line that must wait a fixed time, such as the interval between readiness
// probes of a process the suite starts, carries `// sleep-ok: <reason>` on that line or the line
// above, the same marker the Go sleep lint takes (core/internal/testtime/lint_test.go).
import assert from 'node:assert/strict'
import { readdirSync, readFileSync } from 'node:fs'
import { dirname, join, relative, resolve } from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

const E2E_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const SKIPPED_DIRS = new Set(['node_modules', 'api', 'scripts', 'test-results', 'playwright-report', 'reports'])
const SOURCE = /\.(ts|tsx|mts|cts)$/

const RULES = [
  { name: 'waitForTimeout', pattern: /\bwaitForTimeout\s*\(/ },
  // Bare, window. or globalThis. timers. test.setTimeout and socket.setTimeout set timeouts, not waits.
  { name: 'setTimeout', pattern: /(?:^|[^.\w$])(?:window\.|globalThis\.)?setTimeout\s*\(/ },
  { name: 'setInterval', pattern: /(?:^|[^.\w$])(?:window\.|globalThis\.)?setInterval\s*\(/ },
  { name: 'sleep', pattern: /(?:^|[^.\w$])sleep\s*\(/ },
]
const MARKER = /\/\/\s*sleep-ok:\s*\S/

// Returns [{ line, rule, text }] for every unmarked fixed wait in `source`.
export function findFixedWaits(source) {
  const lines = source.split('\n')
  const found = []
  lines.forEach((text, i) => {
    const code = text.trimStart()
    if (code.startsWith('//') || code.startsWith('*') || code.startsWith('/*')) return
    for (const rule of RULES) {
      if (!rule.pattern.test(text)) continue
      if (MARKER.test(text) || (i > 0 && MARKER.test(lines[i - 1]))) continue
      found.push({ line: i + 1, rule: rule.name, text: text.trim() })
    }
  })
  return found
}

// Top-level directories that are not the suite, and node_modules at any depth.
function suiteFiles(dir = E2E_ROOT) {
  return readdirSync(dir, { withFileTypes: true }).flatMap((d) => {
    if (!d.isDirectory()) return SOURCE.test(d.name) ? [join(dir, d.name)] : []
    const skipped = d.name === 'node_modules' || (dir === E2E_ROOT && SKIPPED_DIRS.has(d.name))
    return skipped ? [] : suiteFiles(join(dir, d.name))
  })
}

test('the UI suite waits on state, never on a fixed time', () => {
  const files = suiteFiles()
  assert.ok(files.some((f) => f.includes(`${join('features', 'warp')}`)), 'the scan found no spec files; the walk is broken')
  const offences = files.flatMap((file) =>
    findFixedWaits(readFileSync(file, 'utf8')).map((f) => `${relative(E2E_ROOT, file)}:${f.line} ${f.rule}: ${f.text}`),
  )
  assert.deepEqual(
    offences,
    [],
    `fixed waits in the UI suite; wait for the state instead (a locator assertion, expect.poll, a response, nextFrames), or mark a wait that must be fixed with // sleep-ok: <reason>:\n${offences.join('\n')}`,
  )
})

test('the detector finds each kind of fixed wait', () => {
  const sample = [
    'await page.waitForTimeout(500)',
    'await new Promise((resolve) => setTimeout(resolve, 500))',
    'await new Promise((r) => window.setTimeout(r, 10))',
    'const id = setInterval(poll, 100)',
    'await sleep(250)',
    'async function sleep(ms) {}',
  ].join('\n')
  assert.deepEqual(
    findFixedWaits(sample).map((f) => f.rule),
    ['waitForTimeout', 'setTimeout', 'setTimeout', 'setInterval', 'sleep', 'sleep'],
  )
})

test('the detector leaves timeouts, comments and marked waits alone', () => {
  const sample = [
    'test.setTimeout(120000)',
    'socket.setTimeout(1000, () => socket.destroy())',
    "import { setTimeout } from 'timers/promises'",
    '// await page.waitForTimeout(500) used to be here',
    ' * waitForTimeout in a doc comment',
    'await setTimeout(1000) // sleep-ok: interval between readiness probes',
    '// sleep-ok: the vendor rate limit allows one call a second',
    'await setTimeout(1000)',
    'await expect(row).toBeVisible({ timeout: 10000 })',
  ].join('\n')
  assert.deepEqual(findFixedWaits(sample), [])
})

test('a marker needs a reason', () => {
  assert.equal(findFixedWaits('await page.waitForTimeout(500) // sleep-ok:').length, 1)
})
