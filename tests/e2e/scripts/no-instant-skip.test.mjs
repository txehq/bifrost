// No skip on a probe that does not wait, in the Playwright UI suite (TXE-7733).
//
// Playwright's isVisible(), isEnabled(), isDisabled(), isHidden(), isEditable() and isChecked() answer
// at once. The suite used them to decide test.skip(): a control that had not rendered yet read as
// "unavailable" and the test skipped, so the run stayed green with the test never run. On one tree
// routing-rules skipped 22 of its 28 tests on one run and 67 of 85 on another.
//
// So, in every TypeScript file of the suite:
//   - a runtime skip, test.skip(<condition>, ...) or test.fixme(<condition>, ...) or their
//     testInfo / test.info() forms, carries `// skip-ok: <why>` on its line or the line above. Skip on
//     the page's state through core/utils/availability.ts (skipUnlessVisible, skipUnlessEnabled,
//     skipUnlessCount), which wait first;
//   - a runtime skip within 8 lines after an instant probe fails even when marked: that is the race.
// A bare test.skip() in a test body is a runtime skip too. A test.skip('title', fn) declaration skips
// by name and is not a runtime decision; in an offline run it is still a skip, which
// offline-skips.json must list (reporters/skips.ts). Playwright's isVisible({ timeout }) ignores the
// timeout and answers at once, so it counts as an instant probe.
import assert from 'node:assert/strict'
import { readdirSync, readFileSync } from 'node:fs'
import { dirname, join, relative, resolve } from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

const E2E_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const SKIPPED_DIRS = new Set(['node_modules', 'api', 'scripts', 'test-results', 'playwright-report', 'reports', 'selftest'])
const SOURCE = /\.(ts|tsx|mts|cts)$/

// test.skip( / test.fixme( / testInfo.skip( / test.info().skip( ..., followed by its first argument.
const SKIP_CALL = /\b(?:test|testInfo|test\.info\(\))\.(skip|fixme)\(\s*/g
const PROBE = /\.(isVisible|isEnabled|isDisabled|isHidden|isEditable|isChecked)\(/
const MARKER = /\/\/\s*skip-ok:\s*\S/
const PROBE_WINDOW = 8

// Returns [{ line, rule, text }] for every runtime skip in `source` that the rules above reject.
export function findInstantSkips(source) {
  const lines = source.split('\n')
  const lineOf = (index) => source.slice(0, index).split('\n').length
  const found = []
  for (const match of source.matchAll(SKIP_CALL)) {
    const first = source[match.index + match[0].length]
    // A declaration (a title string) is not a runtime decision. A bare test.skip() in a test body
    // is: it skips unconditionally, usually in an else after a probe, so it is checked like the rest.
    if (first === '"' || first === "'" || first === '`') continue
    const line = lineOf(match.index)
    const text = lines[line - 1]
    if (text.trimStart().startsWith('//') || text.trimStart().startsWith('*')) continue
    const marked = MARKER.test(text) || (line > 1 && MARKER.test(lines[line - 2]))
    const window = lines.slice(Math.max(0, line - 1 - PROBE_WINDOW), line)
    const probe = window.find((l) => PROBE.test(l) && !l.trimStart().startsWith('//'))
    if (probe) found.push({ line, rule: 'probe', text: `${text.trim()}  <- ${probe.trim()}` })
    else if (!marked) found.push({ line, rule: 'unmarked', text: text.trim() })
  }
  return found
}

function suiteFiles(dir = E2E_ROOT) {
  return readdirSync(dir, { withFileTypes: true }).flatMap((d) => {
    if (!d.isDirectory()) return SOURCE.test(d.name) ? [join(dir, d.name)] : []
    const skipped = d.name === 'node_modules' || (dir === E2E_ROOT && SKIPPED_DIRS.has(d.name))
    return skipped ? [] : suiteFiles(join(dir, d.name))
  })
}

test('the UI suite skips only on a state it waited for, or says why', () => {
  const files = suiteFiles()
  assert.ok(files.some((f) => f.includes(join('features', 'routing-rules'))), 'the scan found no spec files; the walk is broken')
  const offences = files.flatMap((file) =>
    findInstantSkips(readFileSync(file, 'utf8')).map((f) => `${relative(E2E_ROOT, file)}:${f.line} ${f.rule}: ${f.text}`),
  )
  assert.deepEqual(
    offences,
    [],
    `skips that do not wait for the page; skip through core/utils/availability.ts (skipUnlessVisible, skipUnlessEnabled, skipUnlessCount), or mark a skip on a condition that is not the page's state with // skip-ok: <reason>:\n${offences.join('\n')}`,
  )
})

test('the detector finds a probe-gated skip and an unmarked one, and leaves the rest alone', () => {
  const sample = [
    "const shown = await btn.isVisible().catch(() => false)", // 1
    "if (!shown) {", // 2
    "  test.skip(true, 'not visible')", // 3: probe
    "}", // 4
    "", "", "", "", "", "", "", "", // 5-12
    "test.skip(!process.env.X, 'needs X')", // 13: unmarked
    "// skip-ok: the backend refused the plugin, which offline-skips.json records", // 14
    "test.skip(rejected, 'backend rejected')", // 15: marked
    "test.skip('a declared skip', async () => {})", // 16: declaration
    "test.info().skip(", // 17: unmarked, condition on the next line
    "  count === 0,", // 18
    "  'no rows')", // 19
    "await skipUnlessVisible(btn, 'not visible')", // 20: the helper
    "", "", "", "", "", "", "", "", // 21-28
    "} else {", // 29
    "  test.skip()", // 30: a bare runtime skip, unmarked
  ].join('\n')
  assert.deepEqual(
    findInstantSkips(sample).map((f) => `${f.line} ${f.rule}`),
    ['3 probe', '13 unmarked', '17 unmarked', '30 unmarked'],
  )
})

test('a marker does not excuse a skip right after an instant probe', () => {
  const sample = [
    "const enabled = await save.isEnabled()",
    "test.skip(!enabled, 'save disabled') // skip-ok: RBAC",
  ].join('\n')
  assert.deepEqual(findInstantSkips(sample).map((f) => f.rule), ['probe'])
})
