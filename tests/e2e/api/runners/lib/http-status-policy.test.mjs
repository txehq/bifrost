import assert from 'node:assert/strict';
import test from 'node:test';
import { assembleProviderHarness } from './provider-harness-source.mjs';

const collection = assembleProviderHarness();
const statusScript = collection.event.find(event => event.listen === 'test').script.exec
  .join('\n').split('// Only register the content-shape test')[0];

function statusFailures(code, requestName = 'ordinary request') {
  const failures = [];
  new Function('pm', statusScript)({
    info: { requestName },
    response: { code, text: () => 'response body' },
    test: (name, fn) => { try { fn(); } catch (error) { failures.push(error); } },
    expect: value => ({ to: {
      equal: expected => assert.equal(value, expected),
      be: { within: (min, max) => assert.ok(value >= min && value <= max) },
    } }),
  });
  return failures.length;
}

test('ordinary requests accept only HTTP 200', () => {
  for (let code = 100; code <= 599; code++) {
    assert.equal(statusFailures(code), code === 200 ? 0 : 1, `HTTP ${code}`);
  }
});

test('explicit status expectations accept only the expected response', () => {
  for (let code = 100; code <= 599; code++) {
    assert.equal(statusFailures(code, '[EXPECT-202] async submit'), code === 202 ? 0 : 1, `async HTTP ${code}`);
    assert.equal(statusFailures(code, '[EXPECT-4XX] rejection'), code >= 400 && code <= 499 ? 0 : 1, `rejection HTTP ${code}`);
  }
});

// The report-side classifiers (analyze-failures, rate-limit-backoff) must agree with
// the collection gate above about which statuses a tagged row expects, or a passing
// [EXPECT-400] row shows up as a failure in the report (seen live with #7307 rows).
test('isExpectedStatus mirrors the collection gate for every tag shape', async () => {
  const { isExpectedStatus } = await import('./expected-status.mjs');
  for (let code = 100; code <= 599; code++) {
    assert.equal(isExpectedStatus('[EXPECT-400] traversal rejection', code), code === 400, `400-tag HTTP ${code}`);
    assert.equal(isExpectedStatus('[EXPECT-202] async submit', code), code === 202, `202-tag HTTP ${code}`);
    assert.equal(isExpectedStatus('[EXPECT-4XX] rejection', code), code >= 400 && code <= 499, `4XX-tag HTTP ${code}`);
    assert.equal(isExpectedStatus('ordinary request', code), false, `untagged HTTP ${code}`);
  }
  assert.equal(isExpectedStatus(undefined, 400), false, 'missing name');
});

// The analyzer and the harness viewer share one failure verdict. A passing assertion
// must not hide an unexpected status (an HTTP 500 that still served JSON), and a tagged
// row that got its expected status is a pass.
test('isFailedExecution fails unexpected statuses even when an assertion passed', async () => {
  const { isFailedExecution } = await import('./expected-status.mjs');
  assert.equal(isFailedExecution('ordinary request', 500, 0, true), true, 'HTTP 500 with only passing assertions');
  assert.equal(isFailedExecution('ordinary request', 404, 0, true), true, 'HTTP 404 with only passing assertions');
  assert.equal(isFailedExecution('ordinary request', 200, 0, true), false, 'HTTP 200, nothing failed');
  assert.equal(isFailedExecution('ordinary request', 200, 1, true), true, 'failed assertion');
  assert.equal(isFailedExecution('[EXPECT-400] traversal rejection', 400, 0, true), false, 'expected 400');
  assert.equal(isFailedExecution('[EXPECT-4XX] rejection', 500, 0, true), true, '5xx never matches a 4XX tag');
  assert.equal(isFailedExecution('ordinary request', 0, 0, true), true, 'zero status');
  assert.equal(isFailedExecution('ordinary request', 200, 0, false), true, 'no response');
});
