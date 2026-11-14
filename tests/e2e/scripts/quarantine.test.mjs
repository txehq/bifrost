// Pins the offline quarantine's rules (quarantine.ts, TXE-7385) by running the sample suite in
// selftest/quarantine through Playwright against generated lists:
//   - a listed test that fails does not fail the run;
//   - a listed test that passes fails the run;
//   - an entry whose test or whose file is gone fails the run, naming the entry;
//   - outside an offline run the list does nothing.
// Needs Playwright's Chromium (the base fixture opens a page). Run:
//   node --test tests/e2e/scripts/quarantine.test.mjs
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

const E2E_ROOT = join(dirname(fileURLToPath(import.meta.url)), '..')
const SPEC = 'selftest/quarantine/rules.spec.ts'
const TITLE = (name) => `Quarantine rules › ${name}`

// Runs the sample suite with the given list; returns the exit status, the reporter's output and
// each test's outcome by title (expected, unexpected, flaky or skipped).
function runSample(entries, { offline = true, args = [] } = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'quarantine-selftest-'))
  try {
    const list = join(dir, 'quarantine.json')
    const json = join(dir, 'results.json')
    writeFileSync(list, JSON.stringify(entries))
    const env = { ...process.env, E2E_QUARANTINE_FILE: list, QUARANTINE_SELFTEST_JSON: json, E2E_OFFLINE: offline ? '1' : '' }
    delete env.CI
    const run = spawnSync('npx', ['playwright', 'test', '-c', 'selftest/playwright.quarantine.config.ts', ...args], { cwd: E2E_ROOT, env, encoding: 'utf8' })
    const outcomes = {}
    const walk = (suite) => {
      for (const spec of suite.specs ?? []) {
        const t = spec.tests[0]
        outcomes[spec.title] = t?.status
        outcomes[`${spec.title} (expected/actual)`] = `${t?.expectedStatus}/${t?.results?.at(-1)?.status}`
      }
      for (const child of suite.suites ?? []) walk(child)
    }
    for (const suite of JSON.parse(readFileSync(json, 'utf8')).suites) walk(suite)
    return { status: run.status, output: `${run.stdout}\n${run.stderr}`, outcomes }
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
}

const entry = (...names) => ({ kind: 'sample', file: SPEC, tests: names.map(TITLE) })

test('a listed test that fails does not fail the run', () => {
  const run = runSample({ 'TXE-1': entry('a listed test that fails') })
  assert.equal(run.outcomes['a listed test that fails'], 'expected')
  assert.equal(run.outcomes['an unlisted test that passes'], 'expected')
  assert.equal(run.status, 0, run.output)
})

test('a listed test that passes fails the run', () => {
  const run = runSample({ 'TXE-1': entry('a listed test that fails', 'a listed test that passes') })
  assert.equal(run.outcomes['a listed test that passes'], 'unexpected')
  // Expected to fail, but passed.
  assert.equal(run.outcomes['a listed test that passes (expected/actual)'], 'failed/passed')
  assert.notEqual(run.status, 0)
})

test('an entry for a renamed test or a removed file fails the run and names it', () => {
  const run = runSample({
    'TXE-1': entry('a listed test that fails', 'a test that was renamed'),
    'TXE-2': { kind: 'sample', file: 'selftest/quarantine/removed.spec.ts', tests: ['Gone › a test in a removed file'] },
  })
  assert.equal(run.outcomes['a listed test that fails'], 'expected')
  assert.match(run.output, /\(TXE-1\) lists selftest\/quarantine\/rules\.spec\.ts :: Quarantine rules › a test that was renamed, which this run does not have/)
  assert.match(run.output, /\(TXE-2\) lists .* but selftest\/quarantine\/removed\.spec\.ts does not exist/)
  assert.notEqual(run.status, 0)
})

test('a filtered run does not call the listed tests it left out stale', () => {
  const run = runSample({ 'TXE-1': entry('a listed test that fails', 'a listed test that passes') }, { args: ['--grep', 'a listed test that fails'] })
  assert.equal(run.outcomes['a listed test that fails'], 'expected')
  assert.equal(run.outcomes['a listed test that passes'], undefined)
  assert.doesNotMatch(run.output, /which this run does not have/)
  assert.equal(run.status, 0, run.output)
})

test('outside an offline run the list does nothing', () => {
  const run = runSample({ 'TXE-1': entry('a listed test that fails') }, { offline: false })
  assert.equal(run.outcomes['a listed test that fails'], 'unexpected')
  assert.notEqual(run.status, 0)
})
