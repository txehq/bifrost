#!/usr/bin/env node
// Orchestrates the Playwright UI suite: builds Bifrost, starts the dedicated
// Postgres (env/docker-compose.yml, 127.0.0.1:55432), boots WORKERS isolated
// Bifrost instances (own port + fresh database, seeded from env/config.json),
// schedules spec files across them balanced by last run's timings, then runs
// Playwright with one browser per instance.
//
//   node scripts/run-e2e.mjs [--workers n] [--features x,y] [--rerun file.json]
//                            [--grep re] [--headed] [--ui] [--skip-build] [--keep]
//                            [--down] [--offline] [--shard i/n] [-- <extra playwright args>]
//
// --offline (TXE-7385) runs with no provider keys and no egress: empty pricing datasheets from a
// loopback server, no MCP library sync, and tests/e2e/quarantine.json applied. --shard i/n runs
// one of n balanced slices of the selected spec files; the "E2E UI (offline)" job uses both.
import { spawn, spawnSync } from 'node:child_process'
import { copyFileSync, existsSync, mkdirSync, openSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { createServer } from 'node:http'
import net from 'node:net'
import { dirname, join, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { packFiles, shardFiles, shardWeight, specFiles } from './e2e-plan.mjs'

const E2E_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const REPO_ROOT = resolve(E2E_ROOT, '../..')
const WORK_DIR = join(REPO_ROOT, 'tmp', 'e2e')
const BINARY = join(WORK_DIR, 'bifrost-http')
const COMPOSE = ['compose', '-f', join(E2E_ROOT, 'env', 'docker-compose.yml')]
const SEED_CONFIG = join(E2E_ROOT, 'env', 'config.json')
const RESULTS_FILE = join(E2E_ROOT, 'reports', 'e2e-results.json')
const TIMINGS_FILE = join(E2E_ROOT, 'reports', 'e2e-timings.json')
const PLAN_FILE = join(WORK_DIR, 'plan.json')
const LOCK_FILE = join(WORK_DIR, 'run.lock')
const BASE_PORT = Number(process.env.BIFROST_E2E_BASE_PORT || 18181)
const DEFAULT_WORKERS = 4
// Spec folders that read LLM logs; workers running them get seeded traffic first.
const SEED_LOG_FEATURES = ['dashboard', 'logs']
const SEED_LOG_COUNT = 30
const PG_PORT = process.env.BIFROST_E2E_PG_PORT || '55432'
const ADMIN_USERNAME = process.env.BIFROST_ADMIN_USERNAME || 'admin'
const ADMIN_PASSWORD = process.env.BIFROST_ADMIN_PASSWORD || 'bifrost-e2e-password'
const HEALTH_TIMEOUT_MS = 180_000

const children = []

const log = (msg) => console.log(`\x1b[36m[e2e]\x1b[0m ${msg}`)
const fail = (msg) => {
  console.error(`\x1b[31m[e2e] ${msg}\x1b[0m`)
  process.exit(1)
}

function parseArgs(argv) {
  const opts = { workers: null, features: null, rerun: null, grep: null, headed: false, ui: false, skipBuild: false, keep: false, down: false, passthrough: [] }
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i]
    const list = () => argv[++i].split(',').map((s) => s.trim()).filter(Boolean)
    if (a === '--') { opts.passthrough = argv.slice(i + 1); break }
    else if (a === '--workers') opts.workers = Number(argv[++i])
    else if (a === '--features') opts.features = list()
    else if (a === '--rerun') opts.rerun = argv[++i]
    else if (a === '--grep') opts.grep = argv[++i]
    else if (a === '--headed') opts.headed = true
    else if (a === '--ui') opts.ui = true
    else if (a === '--skip-build') opts.skipBuild = true
    else if (a === '--keep') opts.keep = true
    else if (a === '--down') opts.down = true
    else if (a === '--offline') opts.offline = true
    else if (a === '--shard') {
      const m = /^(\d+)\/(\d+)$/.exec(argv[++i] ?? '')
      if (!m || Number(m[1]) < 1 || Number(m[1]) > Number(m[2])) fail('--shard takes i/n with 1 <= i <= n, e.g. --shard 1/2')
      opts.shard = { index: Number(m[1]), total: Number(m[2]) }
    }
    else fail(`unknown argument: ${a}`)
  }
  return opts
}

// Picks the spec files to run and the Playwright filters (file:line for reruns).
function selectFiles(opts) {
  const all = specFiles(E2E_ROOT)
  if (opts.rerun) {
    const path = resolve(process.cwd(), opts.rerun)
    if (!existsSync(path)) fail(`rerun file not found: ${opts.rerun}`)
    const results = JSON.parse(readFileSync(path, 'utf8'))
    const targets = results.rerun?.targets ?? []
    if (!targets.length) {
      // A spec that failed to load or a global setup error fails the run without any test target.
      if (results.status !== 'passed') fail(`${opts.rerun} is a ${results.status} run with no test to re-run; re-run the full suite (or the same FLOW)`)
      log(`${opts.rerun} has no failed tests; nothing to re-run`)
      process.exit(0)
    }
    // A failed login setup only skipped the feature tests behind it, so they are not targets.
    const setup = targets.filter((t) => !t.startsWith('features/'))
    if (setup.length) fail(`${setup.join(', ')} failed last run, so its feature tests never ran; re-run the full suite (or the same FLOW)`)
    return { files: [...new Set(targets.map((t) => t.replace(/:\d+$/, '')))], filters: targets }
  }
  if (opts.features) {
    const files = all.filter((f) => opts.features.some((feat) => f.startsWith(`features/${feat}/`)))
    const unknown = opts.features.filter((feat) => !files.some((f) => f.startsWith(`features/${feat}/`)))
    if (unknown.length) fail(`no spec files for feature(s): ${unknown.join(', ')}`)
    return { files, filters: [] }
  }
  return { files: all, filters: [] }
}

// Spreads files over the workers (packFiles in e2e-plan.mjs). Weights are last run's durations;
// files without one (every file on a fresh CI runner) take the checked-in shard weights.
function schedule(files, requested) {
  const timings = existsSync(TIMINGS_FILE) ? JSON.parse(readFileSync(TIMINGS_FILE, 'utf8')) : {}
  const fallback = shardWeight(E2E_ROOT)
  return packFiles(files, requested, (f) => timings[f] ?? fallback(f)).map((bin, i) => ({ name: `w${i + 1}`, port: BASE_PORT + i, ...bin }))
}

function run(cmd, args, options = {}) {
  const r = spawnSync(cmd, args, { stdio: 'inherit', ...options })
  if (r.status !== 0) fail(`${cmd} ${args.join(' ')} exited with ${r.status}`)
}

function capture(cmd, args, options = {}) {
  return spawnSync(cmd, args, { encoding: 'utf8', ...options })
}

// The command line of a live process, or null; guards against signalling a reused PID.
function commandOf(pid) {
  const r = capture('ps', ['-p', String(pid), '-o', 'command='])
  return r.status === 0 ? r.stdout.trim() : null
}

// One run at a time: every run resets the same worker databases and directories.
function acquireLock() {
  if (existsSync(LOCK_FILE)) {
    const pid = Number(readFileSync(LOCK_FILE, 'utf8'))
    if (pid && pid !== process.pid && commandOf(pid)?.includes('run-e2e.mjs')) {
      fail(`another e2e run (pid ${pid}) is in progress; wait for it or stop it first`)
    }
  }
  mkdirSync(WORK_DIR, { recursive: true })
  writeFileSync(LOCK_FILE, String(process.pid))
  process.on('exit', () => {
    try { if (Number(readFileSync(LOCK_FILE, 'utf8')) === process.pid) rmSync(LOCK_FILE) } catch {}
  })
}

function portInUse(port) {
  return new Promise((res) => {
    const s = net.connect({ port, host: '127.0.0.1' })
    s.once('connect', () => { s.destroy(); res(true) })
    s.once('error', () => res(false))
  })
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

function build(skip) {
  if (skip) {
    if (!existsSync(BINARY)) fail(`--skip-build given but ${relative(REPO_ROOT, BINARY)} does not exist; run once without it`)
    log(`reusing ${relative(REPO_ROOT, BINARY)}`)
    return
  }
  log('building UI (make build-ui)...')
  run('make', ['build-ui'], { cwd: REPO_ROOT })
  log('building bifrost-http...')
  mkdirSync(WORK_DIR, { recursive: true })
  run('go', ['build', '-o', BINARY, '.'], { cwd: join(REPO_ROOT, 'transports', 'bifrost-http') })
}

async function startPostgres(workers) {
  log(`starting dedicated Postgres on 127.0.0.1:${PG_PORT} (compose project bifrost-e2e)...`)
  run('docker', [...COMPOSE, 'up', '-d', '--wait'], { env: { ...process.env, BIFROST_E2E_PG_PORT: PG_PORT } })
  for (const w of workers) {
    const db = dbName(w)
    const r = capture('docker', [...COMPOSE, 'exec', '-T', 'postgres', 'psql', '-v', 'ON_ERROR_STOP=1', '-U', 'bifrost', '-d', 'postgres',
      '-c', `DROP DATABASE IF EXISTS ${db} WITH (FORCE)`, '-c', `CREATE DATABASE ${db}`])
    if (r.status !== 0) fail(`resetting database ${db} failed:\n${r.stderr}`)
  }
  log(`fresh databases: ${workers.map(dbName).join(', ')}`)
}

const dbName = (w) => `bifrost_e2e_${w.name}`
const workerDir = (name) => join(WORK_DIR, 'workers', name)

// --offline (TXE-7385): the gateway downloads its pricing datasheets and the MCP library at startup,
// and exits when it cannot. An offline run serves both datasheets empty from a loopback server and
// turns the library sync off, as the offline harness fixtures do (fixtureGatewayConfig in
// tests/e2e/api/runners/lib/tls-intercept-proxy.mjs).
//
// The same server answers OpenAI chat completions (TXE-7392), and the openai provider points at it
// with a placeholder key, so seedLogs gives the logs and dashboard specs real traffic offline.
async function serveEmptyDatasheets() {
  let n = 0
  const server = createServer((req, res) => {
    res.writeHead(200, { 'Content-Type': 'application/json' })
    if (req.method === 'POST' && req.url?.endsWith('/chat/completions')) {
      let raw = ''
      req.on('data', (chunk) => (raw += chunk))
      req.on('end', () => {
        let model = 'gpt-4o-mini'
        try { model = JSON.parse(raw).model || model } catch {}
        n++
        res.end(JSON.stringify({
          id: `chatcmpl-e2e-offline-${n}`,
          object: 'chat.completion',
          created: Math.floor(Date.now() / 1000),
          model,
          choices: [{ index: 0, message: { role: 'assistant', content: `Hello from the offline fixture (${n}).` }, finish_reason: 'stop' }],
          usage: { prompt_tokens: 12 + (n % 5), completion_tokens: 5 + (n % 3), total_tokens: 17 + (n % 5) + (n % 3) },
        }))
      })
      return
    }
    res.end('{}')
  })
  await new Promise((r) => server.listen(0, '127.0.0.1', r))
  return { server, origin: `http://127.0.0.1:${server.address().port}` }
}

// The key the offline openai provider sends to the loopback fixture, which ignores it.
const OFFLINE_OPENAI_KEY = 'sk-e2e-offline-fixture'

function writeWorkerConfig(dir, datasheetOrigin) {
  const config = JSON.parse(readFileSync(SEED_CONFIG, 'utf8'))
  if (datasheetOrigin) {
    config.framework = {
      ...config.framework,
      pricing: { ...config.framework?.pricing, pricing_url: `${datasheetOrigin}/pricing.json`, model_parameters_url: `${datasheetOrigin}/model-parameters.json`, mcp_library_sync_interval: 0 },
    }
    const openai = config.providers?.openai
    if (openai) {
      openai.keys = openai.keys.map((key) => ({ ...key, value: OFFLINE_OPENAI_KEY }))
      openai.network_config = { ...openai.network_config, base_url: datasheetOrigin }
    }
  }
  writeFileSync(join(dir, 'config.json'), JSON.stringify(config, null, 2))
}

async function startWorker(w, datasheetOrigin) {
  const dir = workerDir(w.name)
  rmSync(dir, { recursive: true, force: true })
  mkdirSync(dir, { recursive: true })
  writeWorkerConfig(dir, datasheetOrigin)
  const logFile = join(dir, 'bifrost.log')
  const out = openSync(logFile, 'a')
  const child = spawn(BINARY, ['--app-dir', dir, '--port', String(w.port), '--log-level', 'info'], {
    stdio: ['ignore', out, out],
    env: {
      ...process.env,
      BIFROST_E2E_PG_PORT: PG_PORT,
      BIFROST_E2E_PG_DB: dbName(w),
      BIFROST_ADMIN_USERNAME: ADMIN_USERNAME,
      BIFROST_ADMIN_PASSWORD: ADMIN_PASSWORD,
      BIFROST_DISABLE_PROFILER: '1',
    },
  })
  children.push(child)
  const pidFile = join(dir, 'bifrost.pid')
  writeFileSync(pidFile, String(child.pid))
  let exited = null
  child.once('exit', (code) => {
    exited = code
    rmSync(pidFile, { force: true })
  })

  const deadline = Date.now() + HEALTH_TIMEOUT_MS
  while (Date.now() < deadline) {
    if (exited !== null) break
    try {
      const res = await fetch(`http://localhost:${w.port}/health`)
      if (res.ok) {
        log(`worker ${w.name} up on http://localhost:${w.port} (log: ${relative(REPO_ROOT, logFile)})`)
        return
      }
    } catch {}
    await sleep(1000)
  }
  const tail = readFileSync(logFile, 'utf8').split('\n').slice(-40).join('\n')
  throw new Error(`worker ${w.name} ${exited !== null ? `exited with ${exited}` : 'did not become healthy'}; last log lines:\n${tail}`)
}

// offline: the openai provider answers from the loopback fixture (writeWorkerConfig), so no key is needed.
async function seedLogs(w, offline) {
  if (!w.files.some((f) => SEED_LOG_FEATURES.some((feat) => f.startsWith(`features/${feat}/`)))) return
  const count = SEED_LOG_COUNT
  if (!offline && !process.env.OPENAI_API_KEY) {
    log(`OPENAI_API_KEY not set; worker ${w.name} gets no seeded logs (logs/dashboard tests will see empty state)`)
    return
  }
  const model = process.env.SEED_MODEL ?? 'openai/gpt-4o-mini'
  let ok = 0
  for (let batch = 0; batch < count; batch += 5) {
    await Promise.all(Array.from({ length: Math.min(5, count - batch) }, async (_, j) => {
      const i = batch + j
      try {
        const res = await fetch(`http://localhost:${w.port}/v1/chat/completions`, {
          method: 'POST',
          signal: AbortSignal.timeout(30_000),
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ model, messages: [{ role: 'user', content: `E2E seed message ${i + 1}: say hello in ${(i % 5) + 1} words` }], max_tokens: 30 }),
        })
        if (res.ok) ok++
        else if (ok === 0 && i === 0) log(`seed call returned ${res.status}: ${(await res.text()).slice(0, 200)}`)
      } catch {}
    }))
  }
  log(`worker ${w.name}: seeded ${ok}/${count} LLM logs with ${model}`)
}

function stopWorkers() {
  for (const c of children) if (c.exitCode === null) c.kill('SIGTERM')
}

function down() {
  const root = join(WORK_DIR, 'workers')
  for (const name of existsSync(root) ? readdirSync(root) : []) {
    const pidFile = join(workerDir(name), 'bifrost.pid')
    if (!existsSync(pidFile)) continue
    const pid = Number(readFileSync(pidFile, 'utf8'))
    if (pid && commandOf(pid)?.startsWith(BINARY)) {
      try { process.kill(pid, 'SIGTERM') } catch {}
    }
    rmSync(pidFile, { force: true })
  }
  run('docker', [...COMPOSE, 'down', '-v'])
  log('stopped workers and removed the bifrost-e2e Postgres')
}

async function main() {
  const opts = parseArgs(process.argv.slice(2))
  acquireLock()
  if (opts.down) return down()

  const selected = selectFiles(opts)
  const { filters } = selected
  // --shard i/n (TXE-7385): this run takes shard i of n (shardFiles in e2e-plan.mjs). It weighs by
  // test count only: last run's timings differ between runs, so two shards could overlap.
  const files = opts.shard ? shardFiles(E2E_ROOT, selected.files, opts.shard.index, opts.shard.total) : selected.files
  if (opts.shard) log(`shard ${opts.shard.index}/${opts.shard.total}: ${files.length} of ${selected.files.length} spec file(s)`)
  if (!files.length) fail('no spec files selected')
  const requested = opts.workers ?? (Number(process.env.WORKERS) || DEFAULT_WORKERS)
  if (!Number.isInteger(requested) || requested < 1) fail(`WORKERS must be a positive integer, got ${requested}`)
  const workers = schedule(files, requested)
  log(`${files.length} spec file(s) on ${workers.length} worker(s):`)
  for (const w of workers) log(`  ${w.name} :${w.port}  ~${Math.round(w.estimateMs / 1000)}s  ${w.files.map((f) => f.replace(/^features\//, '')).join(', ')}`)

  const busy = []
  for (const w of workers) if (await portInUse(w.port)) busy.push(w.port)
  if (busy.length) fail(`port(s) ${busy.join(', ')} already in use. A previous KEEP=1 run? Stop it with: make e2e-ui-down`)
  for (const p of [3001, 3002, 3003]) {
    if (await portInUse(p)) log(`note: port ${p} is already in use; the MCP demo server on it will be reused (tests may fail if it is something else)`)
  }

  build(opts.skipBuild)
  await startPostgres(workers)

  const cleanup = () => { if (!opts.keep) stopWorkers() }
  process.on('SIGINT', () => { cleanup(); process.exit(130) })
  process.on('SIGTERM', () => { cleanup(); process.exit(143) })

  const datasheets = opts.offline ? await serveEmptyDatasheets() : null
  if (datasheets) log(`offline: empty pricing datasheets on ${datasheets.origin}, MCP library sync off`)
  try {
    await Promise.all(workers.map((w) => startWorker(w, datasheets?.origin)))
    await Promise.all(workers.map((w) => seedLogs(w, !!datasheets)))
  } catch (err) {
    stopWorkers()
    fail(err.message)
  }

  if (existsSync(RESULTS_FILE)) copyFileSync(RESULTS_FILE, RESULTS_FILE.replace(/\.json$/, '.prev.json'))

  writeFileSync(PLAN_FILE, JSON.stringify({ workers: workers.map(({ name, port, files }) => ({ name, port, files })) }, null, 2))
  const pwArgs = ['playwright', 'test', ...filters]
  if (opts.grep) pwArgs.push('--grep', opts.grep)
  if (opts.headed) pwArgs.push('--headed')
  if (opts.ui) pwArgs.push('--ui')
  pwArgs.push(...opts.passthrough)
  log(`npx ${pwArgs.join(' ')}`)

  // Asynchronous, not spawnSync: the offline datasheet server lives on this process's event loop,
  // and a blocked loop would leave a gateway's mid-run datasheet fetch (a force sync) hanging.
  const pw = spawn('npx', pwArgs, {
    cwd: E2E_ROOT,
    stdio: 'inherit',
    env: {
      ...process.env,
      E2E_PLAN: PLAN_FILE,
      // Offline runs apply the quarantine (quarantine.ts); other runs ignore it.
      E2E_OFFLINE: opts.offline ? '1' : '',
      // A filtered run leaves tests out on purpose, so the quarantine reporter cannot call a missing title stale.
      E2E_WHOLE_FILES: filters.length || opts.grep || opts.passthrough.length ? '0' : '1',      // Reruns are for debugging, so they keep traces and videos of failures.
      E2E_DEBUG_ARTIFACTS: opts.rerun ? '1' : (process.env.E2E_DEBUG_ARTIFACTS ?? ''),
      // Only full-file runs are representative enough to schedule the next run on.
      E2E_RECORD_TIMINGS: filters.length || opts.grep || opts.passthrough.length ? '0' : '1',
      BIFROST_ADMIN_USERNAME: ADMIN_USERNAME,
      BIFROST_ADMIN_PASSWORD: ADMIN_PASSWORD,
    },
  })
  const pwStatus = await new Promise((res) => {
    pw.once('error', () => res(1))
    pw.once('exit', (code) => res(code))
  })
  datasheets?.server.close()

  if (opts.keep) {
    log('workers left running (--keep):')
    // Only the throwaway default is printed, as a literal, so no env-sourced value reaches the log.
    const loginHint = process.env.BIFROST_ADMIN_PASSWORD ? '(from BIFROST_ADMIN_PASSWORD)' : 'bifrost-e2e-password'
    for (const w of workers) log(`  ${w.name}: http://localhost:${w.port}  login ${ADMIN_USERNAME} / ${loginHint}`)
    log('stop them with: make e2e-ui-down')
  } else {
    stopWorkers()
  }
  log(`HTML report: cd tests/e2e && npx playwright show-report`)
  process.exit(pwStatus ?? 1)
}

main().catch((err) => {
  stopWorkers()
  fail(err.stack || String(err))
})
