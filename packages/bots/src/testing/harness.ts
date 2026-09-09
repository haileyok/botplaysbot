/**
 * Integration test harness helpers: boot the full stack via launch.mjs and
 * provide assertion utilities over the appview/PDS surface.
 */
import { createRequire } from 'node:module'

import { PlaysClient, AgentLoop, type AgentAuthor } from '../index.js'

const require = createRequire(import.meta.url)
const pg = require('pg')

/** Skip message + predicate for environments without Postgres or the binary. */
/** Boot the full stack (delegates to packages/dev/launch.mjs). */
export async function launchStack(agents = 2): Promise<LaunchStack> {
  const mod = (await import('../../../dev/launch.mjs')) as {
    launch(opts?: { agents?: number; env?: Record<string, string> }): Promise<LaunchStack>
  }
  return mod.launch({ agents })
}

export async function canRunIntegration() {
  const dbURL = process.env['DATABASE_URL']
  if (!dbURL) return { ok: false, reason: 'DATABASE_URL not set (docker compose up -d db)' }
  if (process.env['PLAYSBOT_SKIP_TS_INTEGRATION'] === '1') {
    return { ok: false, reason: 'PLAYSBOT_SKIP_TS_INTEGRATION=1' }
  }
  const client = new pg.Client({ connectionString: dbURL })
  try {
    await client.connect()
    await client.query('SELECT 1')
    return { ok: true }
  } catch (err) {
    return { ok: false, reason: `Postgres unreachable: ${String((err as Error).message)}` }
  } finally {
    await client.end().catch(() => {})
  }
}

/** Login an agent account and start its loop with the given author. */
export async function startAgent(
  stack: LaunchStack,
  author: AgentAuthor,
  opts: { agentIndex?: number; pollMs?: number; maxPlies?: number; logger?: (...args: unknown[]) => void } = {},
) {
  const client = new PlaysClient({ appviewUrl: stack.appviewUrl })
  const account = stack.accounts.agents[opts.agentIndex ?? 0]!
  await client.login({ pdsUrl: stack.pdsUrl, identifier: account.handle, password: account.appPassword })
  const loop = new AgentLoop({
    client,
    author,
    seek: { gameType: 'bot.plays.bot.chess.move', maxConcurrent: 3 },
    pollMs: opts.pollMs ?? 300,
    maxPlies: opts.maxPlies,
    logger: opts.logger ?? (process.env['TEST_LOG'] === '1' ? console.log : undefined),
  })
  loop.start()
  return { client, loop, account }
}

/** The launched stack shape (mirrors launch.mjs's return). */
export interface LaunchStack {
  appviewUrl: string
  pdsUrl: string
  plcUrl: string
  dbURL: string
  dbName: string
  accounts: {
    service: { did: string; handle: string; password: string; appPassword: string }
    agents: Array<{ did: string; handle: string; password: string; appPassword: string }>
  }
  env: Record<string, string>
  appviewProcess: import('node:child_process').ChildProcess
  /** Rolling tail of the appview server's own stdout+stderr logs. */
  logTail(): string
  teardown(): Promise<void>
}

/** Wait for the appview game API to report a finished game. */
export async function waitFor<T>(
  predicate: () => Promise<T | null | undefined> | T | null | undefined,
  { timeoutMs = 180_000, what, pollMs = 500 }: { timeoutMs?: number; what: string; pollMs?: number } = { what: 'condition' },
): Promise<T> {
  const deadline = Date.now() + timeoutMs
  let last
  while (Date.now() < deadline) {
    try {
      const v = await predicate()
      last = v
      if (v) return v as T
    } catch (err) {
      last = err
    }
    await new Promise((r) => setTimeout(r, pollMs))
  }
  throw new Error(`timeout waiting for ${what}: ${String(last).slice(0, 300)}`)
}

/** Build a live PlaysClient without a loop (raw assertions). */
export async function rawClient(stack: LaunchStack) {
  const client = new PlaysClient({ appviewUrl: stack.appviewUrl })
  const account = stack.accounts.service
  await client.login({ pdsUrl: stack.pdsUrl, identifier: account.handle, password: account.appPassword })
  return client
}
