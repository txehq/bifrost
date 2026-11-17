// Pins the offline skip rules (skips.ts, TXE-7733) by running the sample suite in selftest/skips
// through Playwright against generated skip lists:
//   - a test that skips and is not listed fails the run, naming the test and its reason;
//   - a listed test that skips does not fail the run;
//   - a listed test that ran without skipping fails the run;
//   - an entry for a renamed test or a removed file fails the run;
//   - a test serial mode skipped after a failure is not reported as a skip;
//   - outside an offline run the list does nothing.
// Needs Playwright's Chromium (the base fixture opens a page). Run:
//   node --test tests/e2e/scripts/skips.test.mjs
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

const E2E_ROOT = join(dirname(fileURLToPath(import.meta.url)), '..')
const SPEC = 'selftest/skips/rules.spec.ts'
const SKIPPER = 'Skip rules › a test that skips at run time'
const RUNNER = 'Skip rules › a test that runs'

// Runs the sample suite with the given list; returns the exit status, the reporter's output and
// each test's outcome by title.
function runSample(entries, { offline = true, grep } = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'skips-selftest-'))
  try {
    const list = join(dir, 'offline-skips.json')
    const json = join(dir, 'results.json')
    writeFileSync(list, JSON.stringify(entries))
    const env = { ...process.env, E2E_SKIPS_FILE: list, SKIPS_SELFTEST_JSON: json, E2E_OFFLINE: offline ? '1' : '' }
    delete env.CI
    const args = ['playwright', 'test', '-c', 'selftest/playwright.skips.config.ts', ...(grep ? ['--grep', grep] : [])]
    const run = spawnSync('npx', args, { cwd: E2E_ROOT, env, encoding: 'utf8' })
    const outcomes = {}
    const walk = (suite) => {
      for (const spec of suite.specs ?? []) outcomes[spec.title] = spec.tests[0]?.results?.at(-1)?.status
      for (const child of suite.suites ?? []) walk(child)
    }
    for (const suite of JSON.parse(readFileSync(json, 'utf8')).suites) walk(suite)
    return { status: run.status, output: `${run.stdout}\n${run.stderr}`, outcomes }
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
}

const entry = (...titles) => ({ reason: 'sample', file: SPEC, tests: titles })

test('an unlisted test that skips fails the run, naming it and its reason', () => {
  const run = runSample({}, { grep: 'Skip rules' })
  assert.equal(run.outcomes['a test that skips at run time'], 'skipped')
  assert.match(run.output, /rules\.spec\.ts :: Skip rules › a test that skips at run time skipped offline \(the sample skips on purpose\) and is not in offline-skips\.json/)
  assert.notEqual(run.status, 0)
})

test('a listed test that skips does not fail the run', () => {
  // A ticket may list one entry or several; this one uses the list form.
  const run = runSample({ 'TXE-1': [entry(SKIPPER)] }, { grep: 'Skip rules' })
  assert.equal(run.outcomes['a test that skips at run time'], 'skipped')
  assert.equal(run.outcomes['a test that runs'], 'passed')
  assert.equal(run.status, 0, run.output)
})

test('a listed test that ran without skipping fails the run', () => {
  const run = runSample({ 'TXE-1': entry(SKIPPER, RUNNER) }, { grep: 'Skip rules' })
  assert.match(run.output, /\(TXE-1\) lists selftest\/skips\/rules\.spec\.ts :: Skip rules › a test that runs, which ran without skipping/)
  assert.notEqual(run.status, 0)
})

test('an entry for a renamed test or a removed file fails the run and names it', () => {
  const run = runSample({
    'TXE-1': entry(SKIPPER, 'Skip rules › a test that was renamed'),
    'TXE-2': { reason: 'sample', file: 'selftest/skips/removed.spec.ts', tests: ['Gone › a test in a removed file'] },
  })
  assert.match(run.output, /\(TXE-1\) lists selftest\/skips\/rules\.spec\.ts :: Skip rules › a test that was renamed, which this run does not have/)
  assert.match(run.output, /\(TXE-2\) lists .* but selftest\/skips\/removed\.spec\.ts does not exist/)
  assert.notEqual(run.status, 0)
})

test('a test serial mode skipped after a failure is not reported as a skip', () => {
  const run = runSample({}, { grep: 'Serial knock-on' })
  assert.equal(run.outcomes['a serial test that fails'], 'failed')
  assert.equal(run.outcomes['a serial test after the failure'], 'skipped')
  assert.doesNotMatch(run.output, /a serial test after the failure skipped offline/)
  assert.notEqual(run.status, 0) // the failure fails it
})

test('outside an offline run the list does nothing', () => {
  const run = runSample({}, { offline: false, grep: 'Skip rules' })
  assert.equal(run.outcomes['a test that skips at run time'], 'skipped')
  assert.doesNotMatch(run.output, /\[skips\]/)
  assert.equal(run.status, 0, run.output)
})
