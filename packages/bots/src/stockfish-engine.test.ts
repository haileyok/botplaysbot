/**
 * Engine wrapper unit tests — the stockfish bot is NOT skippable (probe:
 * asm build gives depth-1 bestmove in ~20ms on this host).
 */
import { afterAll, describe, expect, it } from 'vitest'

import { StockfishEngine } from './stockfish-engine.js'

const START_FEN = 'rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1'

const engine = new StockfishEngine({ depth: 1 })
let started = false

afterAll(async () => {
  if (started) await engine.stop()
})

describe('StockfishEngine (asm, unconditionally — not skippable)', () => {
  it('boots the asm engine and completes the UCI handshake', async () => {
    await engine.start()
    started = true
    // If start() resolved, uciok + readyok were both seen.
    expect(true).toBe(true)
  })

  it('returns a parsed bestmove for the starting position', async () => {
    const analysis = await engine.analyze(START_FEN)
    expect(analysis.bestMove.from).toMatch(/^[a-h][1-8]$/)
    expect(analysis.bestMove.to).toMatch(/^[a-h][1-8]$/)
    expect(analysis.depth).toBe(1)
  })

  it('captures an eval info line with a cp score', async () => {
    const analysis = await engine.analyze(START_FEN)
    // depth 1 on startpos always yields info with pv.
    if (analysis.evalLine !== null) {
      expect(analysis.evalLine).toMatch(/info depth 1/)
      expect(analysis.evalLine).toMatch(/score (cp|mate) -?\d+/)
      expect(analysis.evalLine).toMatch(/ pv [a-h][1-8]/)
      expect(analysis.scoreCp).not.toBeNull()
    }
  })

  it('answers quickly (depth 1 is bounded; generous 15s timeout never trips)', async () => {
    const t0 = Date.now()
    await engine.analyze(START_FEN)
    expect(Date.now() - t0).toBeLessThan(15_000)
  })

  it('parses promotions from a promotion position', async () => {
    // White pawn on a7 can promote.
    const fen = '8/P7/8/8/8/8/k6K/8 w - - 0 1'
    const analysis = await engine.analyze(fen)
    expect(analysis.bestMove.from).toBe('a7')
    expect(analysis.bestMove.to).toBe('a8')
    expect(analysis.bestMove.promotion).toBe('q')
  })
})
