/**
 * launch.mjs — full plays.bot stack launcher for integration tests (and the
 * later demo):
 *
 *   1. Creates a fresh, empty Postgres database (random name) against the
 *      compose `db`; the appview applies migrations on boot.
 *   2. Starts the PDS harness (packages/dev/pds-harness.mjs: in-memory PDS +
 *      PLC) and seeds accounts: the service account (appview's repo writer)
 *      plus N agent accounts via the control API.
 *   3. Spawns ./bin/appview (built via `make build`) with the right env and
 *      waits for /healthz.
 *   4. Returns handles for teardown: kill the appview, stop the PDS, drop
 *      the database.
 *
 * Usage:
 *   import { launch } from './launch.mjs'
 *   const stack = await launch({ agents: 2 })
 *   try { ... stack.appviewUrl, stack.accounts.service, stack.accounts.agents[i] ... }
 *   finally { await stack.teardown() }
 */
import { spawn } from 'node:child_process'
import { randomBytes, randomUUID } from 'node:crypto'
import { mkdtempSync } from 'node:fs'
import { createRequire } from 'node:module'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const require = createRequire(import.meta.url)
const pg = require('pg')

const __dirname = path.dirname(fileURLToPath(import.meta.url))

const ROOT = path.resolve(__dirname, '..', '..')
const HARNESS = path.join(ROOT, 'packages', 'dev', 'pds-harness.mjs')

function randName() {
  return `playsbot_h1_${randomBytes(6).toString('hex')}`
}

function freePort() {
  return new Promise((resolve, reject) => {
    const net = require('node:net')
    const srv = net.createServer()
    srv.listen(0, '127.0.0.1', () => {
      const port = srv.address().port
      srv.close(() => resolve(port))
    })
    srv.on('error', reject)
  })
}

async function waitHttp(url, { timeoutMs = 60_000, probe } = {}) {
  const deadline = Date.now() + timeoutMs
  let lastErr
  while (Date.now() < deadline) {
    try {
      const res = await fetch(url)
      if (probe) {
        const ok = await probe(res)
        if (ok) return
      } else if (res.ok) {
        return
      }
      lastErr = new Error(`${url}: ${res.status}`)
    } catch (err) {
      lastErr = err
    }
    await sleep(250)
  }
  throw new Error(`timeout waiting for ${url}: ${lastErr}`)
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

/**
 * Create a fresh database and return its URL. The base DATABASE_URL (compose
 * `db`) is only used for admin connection to the `postgres` database.
 */
async function createFreshDB(baseURL) {
  if (!baseURL) throw new Error('launch: DATABASE_URL is not set (docker compose up -d db)')
  const admin = new pg.Client({ connectionString: rewriteDB(baseURL, 'postgres') })
  await admin.connect()
  const name = randName()
  try {
    await admin.query(`CREATE DATABASE "${name}"`)
  } finally {
    await admin.end()
  }
  return { url: rewriteDB(baseURL, name), name }
}

function rewriteDB(url, db) {
  const u = new URL(url)
  u.pathname = `/${db}`
  return u.toString()
}

async function dropDB(baseURL, name) {
  const admin = new pg.Client({ connectionString: rewriteDB(baseURL, 'postgres') })
  try {
    await admin.connect()
    await admin.query(`DROP DATABASE IF EXISTS "${name}" WITH (FORCE)`)
  } finally {
    await admin.end()
  }
}

/** Start the PDS harness and wait for its ready line. */
async function startPDSHarness() {
  const port = await freePort()
  const child = spawn(process.execPath, [HARNESS, '--port', String(port)], {
    cwd: ROOT,
    stdio: ['ignore', 'pipe', 'inherit'],
  })
  const ready = new Promise((resolve, reject) => {
    let buf = ''
    const timer = setTimeout(() => reject(new Error('pds harness not ready in 60s')), 60_000)
    child.stdout.on('data', (chunk) => {
      buf += chunk
      const line = buf.split('\n').find((l) => l.startsWith('playsbot-pds-harness ready '))
      if (line) {
        clearTimeout(timer)
        resolve(JSON.parse(line.slice('playsbot-pds-harness ready '.length)))
      }
    })
    child.on('exit', (code) => {
      clearTimeout(timer)
      reject(new Error(`pds harness exited early (${code})`))
    })
  })
  return { child, urls: await ready }
}

async function createAccount(controlURL) {
  const res = await fetch(`${controlURL}/accounts`, { method: 'POST' })
  if (!res.ok && res.status !== 201) {
    throw new Error(`create account: ${res.status} ${await res.text()}`)
  }
  return res.json()
}

/** Spawn ./bin/appview with the stack env and wait for /healthz. */
async function startAppview({ dbURL, pdsURL, plcURL, serviceDID, serviceAppPassword, port, env = {} }) {
  const bin = path.join(ROOT, 'bin', 'appview')
  const child = spawn(bin, [], {
    cwd: ROOT,
    env: {
      ...process.env,
      DATABASE_URL: dbURL,
      PLAYSBOT_PDS_URL: pdsURL,
      PLAYSBOT_PLC_DIRECTORY_URL: plcURL,
      PLAYSBOT_SERVICE_DID: serviceDID,
      PLAYSBOT_SERVICE_APP_PASSWORD: serviceAppPassword,
      PLAYSBOT_SERVICE_SIGNING_KEY_FILE: path.join(mkKeyDir(), 'service-signing.key'),
      PLAYSBOT_PORT: String(port),
      // The indexer must consume the DEV PDS's firehose (ws://), not the
      // production default (wss://bsky.network), or it dies terminally and
      // repo records (moves/commentary) never index.
      PLAYSBOT_EVENT_SOURCE: 'firehose',
      PLAYSBOT_EVENT_SOURCE_URL: pdsURL,
      // Tighten tunables for tests: fast pairing, fast sweeps.
      PLAYSBOT_PAIRING_INTERVAL: '1s',
      PLAYSBOT_SWEEPER_INTERVAL: '500ms',
      PLAYSBOT_COMMENTARY_DELAY_PLIES: '2',
      PLAYSBOT_COMMENTARY_DELAY_SECONDS: '60',
      ...env,
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  })
  let stderrTail = ''
  child.stderr.on('data', (c) => {
    stderrTail = (stderrTail + c).slice(-4000)
  })
  // Keep a rolling tail of the appview's own logs so tests/diagnostics can
  // inspect what the server said (indexer, reveals, errors).
  let logTail = ''
  child.stdout.on('data', (c) => {
    logTail = (logTail + c).slice(-16000)
  })
  child.stderr.on('data', (c) => {
    logTail = (logTail + c).slice(-16000)
  })
  const appviewLogTail = () => logTail
  child.logTail = appviewLogTail
  try {
    await waitHttp(`http://127.0.0.1:${port}/healthz`, { timeoutMs: 60_000 })
  } catch (err) {
    child.kill('SIGKILL')
    throw new Error(`appview did not become healthy: ${err.message}; stderr tail: ${stderrTail}`)
  }
  return child
}

let keyDirCounter = 0
function mkKeyDir() {
  return mkdtempSync(path.join(tmpdir(), `playsbot-h1-${process.pid}-${keyDirCounter++}-`))
}

/**
 * Launch the full stack. Options:
 *   - agents: number of agent accounts to seed (default 2)
 *   - env: extra env for the appview process
 * Returns { appviewUrl, pdsUrl, plcUrl, dbURL, dbName, accounts: {service, agents: []}, env, teardown }.
 */
export async function launch(opts = {}) {
  const baseDB = process.env['DATABASE_URL']
  const { url: dbURL, name: dbName } = await createFreshDB(baseDB)

  const pds = await startPDSHarness()

  // Service account: the appview writes game/reveal/rating records here.
  const service = await createAccount(pds.urls.control)
  const agents = []
  for (let i = 0; i < (opts.agents ?? 2); i++) {
    agents.push(await createAccount(pds.urls.control))
  }

  const port = await freePort()
  const appview = await startAppview({
    dbURL,
    pdsURL: pds.urls.pds,
    plcURL: pds.urls.plc,
    serviceDID: service.did,
    serviceAppPassword: service.appPassword,
    port,
    env: opts.env,
  })

  const stack = {
    appviewUrl: `http://127.0.0.1:${port}`,
    pdsUrl: pds.urls.pds,
    plcUrl: pds.urls.plc,
    dbURL,
    dbName,
    accounts: { service, agents },
    env: {
      DATABASE_URL: dbURL,
      PLAYSBOT_PDS_URL: pds.urls.pds,
      PLAYSBOT_PLC_DIRECTORY_URL: pds.urls.plc,
      PLAYSBOT_SERVICE_DID: service.did,
      PLAYSBOT_SERVICE_APP_PASSWORD: service.appPassword,
      PLAYSBOT_PORT: String(port),
    },
    appviewProcess: appview,
    logTail: () => String(appview.logTail?.() ?? ''),
    async teardown() {
      appview.kill('SIGTERM')
      await Promise.race([exited(appview), sleep(3000)])
      appview.kill('SIGKILL')
      pds.child.kill('SIGTERM')
      await Promise.race([exited(pds.child), sleep(3000)])
      pds.child.kill('SIGKILL')
      await dropDB(baseDB, dbName).catch(() => {})
    },
  }
  return stack
}

function exited(child) {
  return new Promise((resolve) => child.once('exit', resolve))
}

/** CLI smoke: `node packages/dev/launch.mjs` boots, prints env JSON, tears down. */
if (process.argv[1] && path.resolve(process.argv[1]) === path.resolve(fileURLToPath(import.meta.url))) {
  const stack = await launch({ agents: Number(process.argv[2] ?? 2) })
  console.log(JSON.stringify({ appviewUrl: stack.appviewUrl, ...stack.env, accounts: stack.accounts }, null, 2))
  await stack.teardown()
}
