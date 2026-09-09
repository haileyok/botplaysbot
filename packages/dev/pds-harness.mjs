// plays.bot dev PDS harness.
//
// Boots an in-memory ATProto PDS (@atproto/dev-env TestPds) plus its own PLC
// (TestPlc), and exposes a small localhost-only control API for integration
// tests:
//
//   GET  /healthz    -> {"ok":true,"pds":...,"plc":...}
//   POST /accounts   -> creates an agent account, returns
//                       {"did","handle","password","appPassword"}
//   POST /shutdown   -> stops the PDS, PLC, and control server, exits 0
//
// Usage: node packages/dev/pds-harness.mjs [--port N]
//   --port N  control API port (default: ephemeral)
//
// On startup the harness prints one ready line on stdout:
//
//   playsbot-pds-harness ready {"pds":"http://localhost:PORT1","plc":"http://localhost:PORT2","control":"http://localhost:PORT3"}
//
// Point PLAYSBOT_PLC_DIRECTORY_URL at the printed `plc` URL so the AppView
// resolves did:plc DIDs created by this PDS.

import http from 'node:http'
import { TestPds, TestPlc } from '@atproto/dev-env'

function argPort(name) {
  const i = process.argv.indexOf(name)
  if (i === -1) return undefined
  const v = Number.parseInt(process.argv[i + 1], 10)
  return Number.isInteger(v) && v > 0 && v < 65536 ? v : undefined
}

const controlPort = argPort('--port')

let pds
let plc
let controlServer
let accountCounter = 0

async function createAccount(body = {}) {
  accountCounter += 1
  // The dev PDS enforces a short first label for handles under its service
  // domains, so keep the generated handle compact: ag-<base36 time>.test.
  const handle = body.handle || `ag-${Date.now().toString(36)}${accountCounter}.test`
  const password = body.password || `pw-${crypto.randomUUID()}`

  // com.atproto.server.createAccount (no invite required in dev-env; email
  // is mandatory even in dev mode).
  const createRes = await fetch(`${pds.url}/xrpc/com.atproto.server.createAccount`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ email: `${handle.split('@')[0] || handle}@test.invalid`, handle, password }),
  })
  const created = await createRes.json()
  if (!createRes.ok) {
    throw new Error(`createAccount failed: ${createRes.status} ${JSON.stringify(created)}`)
  }

  // Session with the account password, then mint an app password, which is
  // what agents actually present to third-party services.
  const sessionRes = await fetch(`${pds.url}/xrpc/com.atproto.server.createSession`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ identifier: handle, password }),
  })
  const session = await sessionRes.json()
  if (!sessionRes.ok) {
    throw new Error(`createSession failed: ${sessionRes.status} ${JSON.stringify(session)}`)
  }

  const appPwRes = await fetch(`${pds.url}/xrpc/com.atproto.server.createAppPassword`, {
    method: 'POST',
    headers: {
      'content-type': 'application/json',
      authorization: `Bearer ${session.accessJwt}`,
    },
    body: JSON.stringify({ name: `playsbot-${accountCounter}` }),
  })
  const appPw = await appPwRes.json()
  if (!appPwRes.ok) {
    throw new Error(`createAppPassword failed: ${appPwRes.status} ${JSON.stringify(appPw)}`)
  }

  return {
    did: created.did,
    handle: created.handle,
    password,
    appPassword: appPw.password,
  }
}

function json(res, status, body) {
  const payload = JSON.stringify(body)
  res.writeHead(status, { 'content-type': 'application/json' })
  res.end(payload)
}

async function startControlServer() {
  const server = http.createServer((req, res) => {
    const url = new URL(req.url, 'http://localhost')
    if (req.method === 'GET' && url.pathname === '/healthz') {
      json(res, 200, { ok: true, pds: pds.url, plc: plc.url })
      return
    }
    if (req.method === 'POST' && url.pathname === '/accounts') {
      let raw = ''
      req.on('data', (chunk) => {
        raw += chunk
      })
      req.on('end', () => {
        let body = {}
        if (raw.length > 0) {
          try {
            body = JSON.parse(raw)
          } catch {
            json(res, 400, { error: 'invalid JSON body' })
            return
          }
        }
        createAccount(body).then(
          (account) => json(res, 201, account),
          (err) => json(res, 500, { error: String(err.message || err) }),
        )
      })
      return
    }
    if (req.method === 'POST' && url.pathname === '/shutdown') {
      json(res, 200, { ok: true })
      // Let the response flush before tearing down.
      setTimeout(() => shutdown(0), 50)
      return
    }
    json(res, 404, { error: 'not found' })
  })

  await new Promise((resolve, reject) => {
    server.once('error', reject)
    server.listen(controlPort ?? 0, '127.0.0.1', resolve)
  })

  const addr = server.address()
  return { server, port: addr.port }
}

async function shutdown(code) {
  for (const closer of [
    () => controlServer?.close(),
    () => pds?.close(),
    () => plc?.close(),
  ]) {
    try {
      await closer()
    } catch (err) {
      console.error('harness shutdown error', err)
    }
  }
  process.exit(code)
}

process.on('SIGTERM', () => shutdown(0))
process.on('SIGINT', () => shutdown(0))

try {
  plc = await TestPlc.create({})
  // NOTE: current @atproto/pds builds key the PLC endpoint as `didPlcUrl`;
  // `plcUrl` is kept for dev-env versions whose PdsConfig still uses it.
  pds = await TestPds.create({ plcUrl: plc.url, didPlcUrl: plc.url })
  const control = await startControlServer()
  controlServer = control.server

  const ready = { pds: pds.url, plc: plc.url, control: `http://127.0.0.1:${control.port}` }
  console.log('playsbot-pds-harness ready ' + JSON.stringify(ready))
} catch (err) {
  console.error('playsbot-pds-harness failed to start:', err)
  await shutdown(1)
}
