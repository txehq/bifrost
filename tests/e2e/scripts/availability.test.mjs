// Pins what the skip helpers in core/utils/availability.ts do on a real page (TXE-7733), by running
// the sample suite in selftest/availability through Playwright and checking each test's outcome:
//   - a control that renders or enables late, rows that arrive late, and a .or() union of two
//     fields that render together all run (the helper waited; no strict-mode skip);
//   - a control that never renders, and a page that settles on its absent state, skip.
// Each sample test's title starts with the outcome it must have. If a helper went back to an
// instant probe, the "runs:" tests would skip and this test would fail.
// Needs Playwright's Chromium. Run:
//   node --test tests/e2e/scripts/availability.test.mjs
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

const E2E_ROOT = join(dirname(fileURLToPath(import.meta.url)), '..')

test('the skip helpers wait for a late state and skip only on one that never comes', () => {
  const dir = mkdtempSync(join(tmpdir(), 'availability-selftest-'))
  try {
    const json = join(dir, 'results.json')
    const env = { ...process.env, AVAILABILITY_SELFTEST_JSON: json }
    delete env.CI
    const run = spawnSync('npx', ['playwright', 'test', '-c', 'selftest/playwright.availability.config.ts'], { cwd: E2E_ROOT, env, encoding: 'utf8' })
    const outcomes = {}
    const walk = (suite) => {
      for (const spec of suite.specs ?? []) outcomes[spec.title] = spec.tests[0]?.results?.at(-1)?.status
      for (const child of suite.suites ?? []) walk(child)
    }
    for (const suite of JSON.parse(readFileSync(json, 'utf8')).suites) walk(suite)
    const titles = Object.keys(outcomes)
    assert.ok(titles.length >= 7, `expected the 7 sample tests, got ${titles.length}:\n${run.stdout}\n${run.stderr}`)
    for (const title of titles) {
      const want = title.startsWith('runs:') ? 'passed' : title.startsWith('skips:') ? 'skipped' : undefined
      assert.ok(want, `sample test "${title}" does not say whether it runs or skips`)
      assert.equal(outcomes[title], want, `"${title}" ${outcomes[title]}, want ${want}\n${run.stdout}\n${run.stderr}`)
    }
    assert.equal(run.status, 0, `${run.stdout}\n${run.stderr}`)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})
