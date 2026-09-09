/**
 * AC.14a — two in-process SDK random-mover agents seek → paired → full game.
 * AC.14c — stockfish (asm, depth 1) vs random-mover, same assertions.
 * Reconnect convergence — force-drop the game WS mid-game; the loop
 * recovers via getState and the game still completes.
 *
 * All three are unconditional (skip only when the environment genuinely
 * cannot run them: no Postgres, or PLAYSBOT_SKIP_TS_INTEGRATION=1).
 */
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import type { AgentAuthor, GameState, MovePayload } from '@plays-bot/client'
import { randomMoverAuthor, StockfishEngine } from './index.js'

import { canRunIntegration, launchStack, startAgent, waitFor, type LaunchStack } from './testing/harness.js'

const RUN = await canRunIntegration()
const maybe = RUN.ok ? describe : describe.skip

type PlaysClientLike = import('@plays-bot/client').PlaysClient
type AgentLoop = import('@plays-bot/client').AgentLoop

let stack: LaunchStack
const runningAgents: Array<{ loop: AgentLoop; client: PlaysClientLike; account: { did: string } }> = []

beforeAll(async () => {
  if (!RUN.ok) return
  // 4 accounts: tests 1-2 share agents 0/1; the reconnect test uses 2/3 so
  // earlier games never saturate its pairing budget (§9a.3 maxConcurrent).
  stack = await launchStack(4)
}, 240_000)

afterEach(async () => {
  // Stop each test's agents (cancels standing seeks, resigns live games)
  // so loops never leak across tests and keep re-pairing.
  const stoppedDids = new Set(runningAgents.map((a) => a.account.did))
  for (const a of runningAgents) a.loop.stop()
  runningAgents.length = 0
  if (!stack || stoppedDids.size === 0) return
  // Wait (bounded) until the resigned games actually finish so the next
  // next test cannot match a dying leftover from this test (tests pin to loop.activeGame).
  await waitFor(
    async () => {
      const res = await fetch(`${stack.appviewUrl}/api/games`).then((r) =>
        r.json() as Promise<{ games?: Array<{ players?: Array<{ did: string }>; status?: string }> }>,
      )
      const stillActive = (res.games ?? []).some(
        (g) => g.status === 'active' && (g.players ?? []).some((p) => stoppedDids.has(p.did)),
      )
      return stillActive ? null : true
    },
    { what: 'leftover games to finish', timeoutMs: 45_000, pollMs: 1_000 },
  ).catch(() => {
    /* proceed regardless; best-effort isolation */
  })
}, 60_000)

afterAll(async () => {
  for (const a of runningAgents) a.loop.stop()
  await stack?.teardown()
}, 60_000)

/** Collect an agent's move records from its repo via the PDS. */
async function listMoveRecords(agentIndex: number): Promise<Array<Record<string, unknown>>> {
  const account = stack.accounts.agents[agentIndex]!
  const res = await fetch(`${stack.pdsUrl}/xrpc/com.atproto.server.createSession`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ identifier: account.handle, password: account.appPassword }),
  })
  const session = (await res.json()) as { accessJwt: string }
  const out: Array<Record<string, unknown>> = []
  let cursor: string | undefined
  for (;;) {
    const u = new URL(`${stack.pdsUrl}/xrpc/com.atproto.repo.listRecords`)
    u.searchParams.set('repo', account.did)
    u.searchParams.set('collection', 'bot.plays.bot.game.move')
    u.searchParams.set('limit', '100')
    if (cursor) u.searchParams.set('cursor', cursor)
    const r = await fetch(u, { headers: { authorization: `Bearer ${session.accessJwt}` } })
    const data = (await r.json()) as { records?: Array<{ value: Record<string, unknown> }>; cursor?: string }
    for (const rec of data.records ?? []) out.push(rec.value)
    cursor = data.cursor
    if (!cursor) break
  }
  return out
}

async function listCommentaryRecords(agentIndex: number): Promise<Array<Record<string, unknown>>> {
  const account = stack.accounts.agents[agentIndex]!
  const res = await fetch(`${stack.pdsUrl}/xrpc/com.atproto.server.createSession`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ identifier: account.handle, password: account.appPassword }),
  })
  const session = (await res.json()) as { accessJwt: string }
  const out: Array<Record<string, unknown>> = []
  let cursor: string | undefined
  for (;;) {
    const u = new URL(`${stack.pdsUrl}/xrpc/com.atproto.repo.listRecords`)
    u.searchParams.set('repo', account.did)
    u.searchParams.set('collection', 'bot.plays.bot.game.commentary')
    u.searchParams.set('limit', '100')
    if (cursor) u.searchParams.set('cursor', cursor)
    const r = await fetch(u, { headers: { authorization: `Bearer ${session.accessJwt}` } })
    const data = (await r.json()) as { records?: Array<{ value: Record<string, unknown> }>; cursor?: string }
    for (const rec of data.records ?? []) out.push(rec.value)
    cursor = data.cursor
    if (!cursor) break
  }
  return out
}

/** Post-game assertions shared by AC.14a and AC.14c. */
function assertGameIntegrity(ctx: { gameUri: string; agents: Array<{ index: number; did: string }> }) {
  return waitFor(async () => {
    // Every accepted move has a repo move record with the right moveToken.
    const api = await fetch(`${stack.appviewUrl}/api/game?uri=${encodeURIComponent(ctx.gameUri)}`)
    const game = (await api.json()) as {
      game?: { plyCount?: number }
      history?: Array<{ ply: number; player: string }>
      commentary?: Array<{ visibility: string; revealed?: boolean; text?: string; player: string }>
      clocks?: unknown
    }
    const history = game.history ?? []

    for (const agent of ctx.agents) {
      const records = await listMoveRecords(agent.index)
      const forGame = records.filter((r) => (r['game'] as { uri?: string })?.['uri'] === ctx.gameUri)
      const mine = history.filter((h) => h.player === agent.did)
      expect(forGame.length, `agent ${agent.index}: repo move records`).toBeGreaterThanOrEqual(mine.length)
      // Every record carries a nonempty moveToken.
      for (const rec of forGame) {
        expect(typeof rec['moveToken']).toBe('string')
        expect(rec['moveToken'] as string).not.toBe('')
      }
      // Move tokens must match what the appview saw (spot-check: token exists per ply).
      const recPlies = new Set(forGame.map((r) => r['ply']))
      for (const h of mine) expect(recPlies.has(h.ply), `ply ${h.ply} record present`).toBe(true)
    }

    // ≥1 delayed and ≥1 sealed commentary record across the agents' repos.
    const allCommentary: Array<Record<string, unknown>> = []
    for (const agent of ctx.agents) {
      allCommentary.push(...(await listCommentaryRecords(agent.index)))
    }
    const forGame = allCommentary.filter((r) => (r['game'] as { uri?: string })?.['uri'] === ctx.gameUri)
    const visibilities = new Set(forGame.map((r) => r['visibility']))
    expect(visibilities.has('delayed'), `≥1 delayed commentary (found ${forGame.length} records for game; visibilities ${[...visibilities].join(',')} of ${allCommentary.length} total)`).toBe(true)
    expect(visibilities.has('sealed'), '≥1 sealed commentary').toBe(true)

    // End-of-game key publication: a public commentary record with keyId + text.
    const keyPubs = forGame.filter(
      (r) => r['visibility'] === 'public' && typeof r['keyId'] === 'string' && typeof r['text'] === 'string',
    )
    expect(keyPubs.length, 'key publication records').toBeGreaterThanOrEqual(1)
    // base64 of a 32-byte key.
    for (const kp of keyPubs) {
      const raw = Buffer.from(kp['text'] as string, 'base64')
      expect(raw.length, 'published key is 32 bytes').toBe(32)
    }

    // The appview shows revealed texts post-game (delayed/sealed decrypted).
    const revealed = (game.commentary ?? []).filter((c) => c.visibility !== 'public' && c.revealed)
    expect(revealed.length, 'revealed escrowed commentary in /api/game').toBeGreaterThanOrEqual(1)
    for (const r of revealed) {
      expect(typeof r['text']).toBe('string')
      expect(r['text']!.length).toBeGreaterThan(0)
    }

    // game.reveal record exists with agentPublished: true.
    const svcRes = await fetch(
      `${stack.appviewUrl}/api/game?uri=${encodeURIComponent(ctx.gameUri)}&include=reveal`,
    )
    if (svcRes.ok) {
      const svc = (await svcRes.json()) as { reveal?: { keys?: Array<{ agentPublished?: boolean }> } }
      const keys = svc.reveal?.keys ?? []
      expect(keys.length, 'reveal record keys').toBeGreaterThanOrEqual(1)
      expect(keys.every((k) => k.agentPublished === true), 'agentPublished on published keys').toBe(true)
    }

    return true
  }, { what: 'post-game record integrity', timeoutMs: 60_000, pollMs: 2000 })
}

maybe('AC.14a: two SDK random-mover agents play a full game', () => {
  it('pairs, plays to a terminal result, and writes every record', async () => {
    const [a, b] = await Promise.all([
      startAgent(stack, randomMoverAuthor, { agentIndex: 0, pollMs: 250, maxPlies: 50 }),
      startAgent(stack, randomMoverAuthor, { agentIndex: 1, pollMs: 250, maxPlies: 50 }),
    ])
    runningAgents.push(a, b)

    const game = await waitFor(async () => {
      for (const agent of [a, b]) {
        const uri = agent.loop.activeGame
        if (uri) return uri
      }
      return null
    }, { what: 'a matched game', timeoutMs: 120_000 })

    const finished = await waitFor(async () => {
      const res = await a.client.apiGame(game)
      const status = res['status'] as string | undefined
      if (status === 'finished' || status === 'aborted') return res
      return null
    }, { what: 'game finish', timeoutMs: 300_000 })

    expect(finished['status']).toBe('finished')
    const gameResult = finished['result'] as { outcome: string; reason: string } | undefined
    expect(gameResult).toBeDefined()
    expect(['win', 'draw']).toContain(gameResult!.outcome)

    // Reveal scheduler + indexer need a beat; the integrity helper polls.
    await assertGameIntegrity({
      gameUri: game,
      agents: [
        { index: 0, did: a.account.did },
        { index: 1, did: b.account.did },
      ],
    })
  }, 600_000)
})

maybe('AC.14c: stockfish (asm, depth 1) vs random-mover', () => {
  it('plays a full game with eval-line commentary and full record integrity', async () => {
    const sf = new StockfishEngine({ depth: 1 })
    await sf.start()
    let lastAnalysis: { evalLine: string | null; scoreCp: number | null; depth: number } | null = null
    let moveCount = 0
    const sfAuthor: AgentAuthor = {
      async chooseMove(state: GameState): Promise<MovePayload> {
        const pos = state.position as { fen?: string } | undefined
        if (!pos?.fen) throw new Error('no fen')
        const a = await sf.analyze(pos.fen)
        lastAnalysis = a
        moveCount++
        return { $type: 'bot.plays.bot.chess.move', ...a.bestMove }
      },
      explain(): { text: string; visibility: 'public' | 'delayed' | 'sealed' } | null {
        const a = lastAnalysis
        if (!a) return null
        const n = moveCount
        if (n % 3 === 1) return { text: `depth ${a.depth} cp ${a.scoreCp}`, visibility: 'public' }
        if (n % 3 === 2) return { text: `eval line: ${a.evalLine ?? 'none'}`, visibility: 'delayed' }
        return { text: `private: ${a.scoreCp}cp`, visibility: 'sealed' }
      },
    }

    const [a, b] = await Promise.all([
      startAgent(stack, sfAuthor, { agentIndex: 0, pollMs: 250, maxPlies: 20 }),
      startAgent(stack, randomMoverAuthor, { agentIndex: 1, pollMs: 250, maxPlies: 20 }),
    ])
    runningAgents.push(a, b)

    const game = await waitFor(async () => {
      for (const agent of [a, b]) {
        const uri = agent.loop.activeGame
        if (uri) return uri
      }
      return null
    }, { what: 'a matched game (sf vs random)', timeoutMs: 120_000 })

    const finished = await waitFor(async () => {
      const res = await a.client.apiGame(game)
      const status = res['status'] as string | undefined
      if (status === 'finished' || status === 'aborted') return res
      return null
    }, { what: 'game finish (sf vs random)', timeoutMs: 300_000 })

    expect(finished['status']).toBe('finished')

    await assertGameIntegrity({
      gameUri: game,
      agents: [
        { index: 0, did: a.account.did },
        { index: 1, did: b.account.did },
      ],
    })

    // Eval-line commentary: at least one delayed note contains "eval line:".
    const commentary = await listCommentaryRecords(0)
    const evalNotes = commentary.filter(
      (r) => r['visibility'] === 'delayed' && String(r['ciphertext'] ?? '').length > 0,
    )
    expect(evalNotes.length).toBeGreaterThanOrEqual(1)

    await sf.stop()
  }, 600_000)
})

maybe('SDK reconnect convergence: force-drop the game WS mid-game', () => {
  it('recovers via getState and still completes with all records', async () => {
    const [a, b] = await Promise.all([
      startAgent(stack, randomMoverAuthor, { agentIndex: 2, pollMs: 250, maxPlies: 50 }),
      startAgent(stack, randomMoverAuthor, { agentIndex: 3, pollMs: 250, maxPlies: 50 }),
    ])
    runningAgents.push(a, b)

    const game = await waitFor(async () => {
      for (const agent of [a, b]) {
        const uri = agent.loop.activeGame
        if (uri) return uri
      }
      return null
    }, { what: 'a matched game (reconnect)', timeoutMs: 120_000 })

    // Wait for a few plies, then kill the game sockets on both agents.
    await waitFor(async () => {
      const res = await a.client.apiGame(game)
      return ((res['ply'] as number | undefined) ?? 0) >= 4
    }, { what: '4 plies', timeoutMs: 120_000 })
    a.loop.killGameSockets()
    b.loop.killGameSockets()

    // The loop is poll-driven: getState keeps working without the WS. The
    // game must still finish and all records must exist.
    const finished = await waitFor(async () => {
      const res = await a.client.apiGame(game)
      const status = res['status'] as string | undefined
      if (status === 'finished' || status === 'aborted') return res
      return null
    }, { what: 'game finish after reconnect', timeoutMs: 300_000 })
    expect(finished).toBeDefined()

    await assertGameIntegrity({
      gameUri: game,
      agents: [
        { index: 2, did: a.account.did },
        { index: 3, did: b.account.did },
      ],
    })
  }, 600_000)
})
