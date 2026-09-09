/**
 * Stockfish engine wrapper for plays.bot bots.
 *
 * Uses the npm `stockfish` package (v18): `initEngine("asm")` boots the
 * asm.js Lite build — pure JS, no wasm threading required, `go depth 1`
 * answers in ~20ms. The wasm variant is available behind
 * STOCKFISH_VARIANT=wasm (`initEngine("stockfish-18-single.wasm")`).
 *
 * Output capture: the emscripten module routes every UCI line through
 * `engine.listener` when set (falling back to console.log). We install a
 * listener for the engine's lifetime and ALSO shim console.log so builds
 * that bypass the listener are still captured.
 */
import { createRequire } from 'node:module'

import type { AgentAuthor, GameState, MovePayload } from '@plays-bot/client'

// The stockfish package is CommonJS with no named ESM exports; interop via
// createRequire so `initEngine` is the module.exports function itself.
const require = createRequire(import.meta.url)
const initEngine = require('stockfish') as (enginePath?: string) => Promise<RawEngine>

export interface StockfishOptions {
  /** 'asm' (default) or 'wasm' (STOCKFISH_VARIANT=wasm). */
  variant?: 'asm' | 'wasm'
  /** Search depth cap. Default 1 (env STOCKFISH_DEPTH). */
  depth?: number
  /** Per-command timeout. Default 15s (generous; depth is capped anyway). */
  moveTimeoutMs?: number
}

export interface EngineAnalysis {
  bestMove: { from: string; to: string; promotion?: 'q' | 'r' | 'b' | 'n' }
  /** Last `info depth d ... score cp X ... pv ...` line seen, if any. */
  evalLine: string | null
  depth: number
  scoreCp: number | null
  pv: string | null
}

interface RawEngine {
  ready?: Promise<void>
  listener?: (line: string) => void
  print: (cmd: string) => void
  sendCommand?: (cmd: string) => void
  terminate?: () => void
}

export class StockfishEngine {
  private engine: RawEngine | null = null
  private readonly variant: 'asm' | 'wasm'
  private readonly depth: number
  private readonly moveTimeoutMs: number
  private readonly lines: string[] = []

  constructor(opts: StockfishOptions = {}) {
    const envVariant = process.env['STOCKFISH_VARIANT']
    this.variant = opts.variant ?? (envVariant === 'wasm' ? 'wasm' : 'asm')
    const envDepth = Number.parseInt(process.env['STOCKFISH_DEPTH'] ?? '', 10)
    this.depth = opts.depth ?? (Number.isFinite(envDepth) ? envDepth : 1)
    this.moveTimeoutMs = opts.moveTimeoutMs ?? 15_000
  }

  async start(): Promise<void> {
    // console.log shim: capture engine output for the engine's lifetime
    // (the engine's fallback sink) in addition to the listener.
    const origLog = console.log
    const capture = (line: string): void => {
      if (typeof line === 'string') this.lines.push(line)
    }
    console.log = ((...args: unknown[]) => {
      capture(args.join(' '))
      origLog(...args)
    }) as typeof console.log
    this.restoreLog = () => {
      console.log = origLog
    }

    // initEngine("asm") → stockfish-18-asm.js (asm.js Lite build). The wasm
    // keyword resolves the single-threaded wasm build.
    //
    // The emscripten glue runs `fetch = null` at require time; in its
    // sloppy-mode CJS context that leaks to globalThis and nulls Node's fetch
    // for the rest of the process (breaks @atproto/api's CredentialSession
    // and every other fetch user). Snapshot and restore around the require.
    const enginePath = this.variant === 'wasm' ? 'stockfish-18-single' : 'asm'
    const savedFetch = globalThis.fetch
    let engine: RawEngine
    try {
      engine = (await initEngine(enginePath)) as RawEngine
    } finally {
      globalThis.fetch = savedFetch
    }
    if (engine.ready) await engine.ready
    engine.listener = capture
    this.engine = engine

    this.send('uci')
    await this.waitForLine('uciok')
    this.send('isready')
    await this.waitForLine('readyok')
  }

  private restoreLog: (() => void) | null = null

  private send(cmd: string): void {
    const e = this.engine
    if (!e) throw new Error('engine not started')
    if (typeof e.sendCommand === 'function') e.sendCommand(cmd)
    else e.print(cmd)
  }

  private waitForLine(suffix: string, extraMs = 0): Promise<string> {
    return new Promise((resolve, reject) => {
      let poll: ReturnType<typeof setInterval> | null = null
      const fail = (msg: string): void => {
        if (poll) clearInterval(poll)
        reject(new Error(msg))
      }
      const hardTimer = setTimeout(() => fail(`stockfish: timeout waiting for ${suffix}`), this.moveTimeoutMs + extraMs)
      const check = (): void => {
        const line = this.lines.find((l) => l.includes(suffix))
        if (line !== undefined) {
          clearTimeout(hardTimer)
          if (poll) clearInterval(poll)
          resolve(line)
        }
      }
      poll = setInterval(check, 15)
    })
  }

  /** Analyze a FEN at the configured depth cap and parse the best move. */
  async analyze(fen: string, goDepth?: number): Promise<EngineAnalysis> {
    const depth = goDepth ?? this.depth
    this.lines.length = 0
    this.send(`position fen ${fen}`)
    this.send(`go depth ${depth}`)

    await this.waitForLine('bestmove', 0)

    let evalLine: string | null = null
    let scoreCp: number | null = null
    let pv: string | null = null
    for (const line of this.lines) {
      if (line.startsWith('info ') && line.includes(' pv ')) {
        evalLine = line
        const m = /score (cp|mate) (-?\d+)/.exec(line)
        if (m) scoreCp = Number.parseInt(m[2]!, 10) * (m[1] === 'mate' ? 10000 : 1)
        const p = / pv (.+)$/.exec(line)
        if (p) pv = p[1]!
      }
    }

    const bestLine = this.lines.find((l) => l.startsWith('bestmove'))
    const best = bestLine?.split(/\s+/)[1]
    if (!best || best === '(none)') {
      throw new Error(`stockfish: no bestmove for ${fen} (${bestLine ?? 'none'})`)
    }
    const from = best.slice(0, 2)
    const to = best.slice(2, 4)
    const promoRaw = best.length > 4 ? best[4] : undefined
    const promotion = promoRaw === 'q' || promoRaw === 'r' || promoRaw === 'b' || promoRaw === 'n' ? promoRaw : undefined
    return { bestMove: { from, to, promotion }, evalLine, depth, scoreCp, pv }
  }

  async stop(): Promise<void> {
    try {
      this.send('quit')
    } catch {
      /* engine may already be gone */
    }
    try {
      this.engine?.terminate?.()
    } catch {
      /* ignore */
    }
    this.restoreLog?.()
    this.restoreLog = null
    this.engine = null
  }
}

/** Build the stockfish AgentAuthor: eval-line commentary + a sealed private note. */
export async function stockfishAuthor(opts: StockfishOptions = {}): Promise<{
  author: AgentAuthor
  engine: StockfishEngine
}> {
  const engine = new StockfishEngine(opts)
  await engine.start()
  let moveCount = 0
  let lastAnalysis: EngineAnalysis | null = null

  const author: AgentAuthor = {
    async chooseMove(state: GameState): Promise<MovePayload> {
      const pos = state.position as { fen?: string } | undefined
      if (!pos?.fen) throw new Error('stockfish bot: state has no FEN position')
      const analysis = await engine.analyze(pos.fen)
      lastAnalysis = analysis
      moveCount++
      // The server validates payload.$type against the game's move NSID.
      return { $type: 'bot.plays.bot.chess.move', ...analysis.bestMove }
    },
    explain(_state: GameState, payload: MovePayload): { text: string; visibility: 'public' | 'delayed' | 'sealed' } | null {
      void payload
      const a = lastAnalysis
      if (!a) return null
      const n = moveCount
      moveCount++
      if (n % 3 === 1) {
        // Public, occasionally.
        return { text: `depth ${a.depth}: ${a.scoreCp ?? '?'}cp — I liked my position.`, visibility: 'public' }
      }
      if (n % 3 === 2) {
        // Delayed: the raw eval line for the spectators.
        return {
          text: `eval line: ${a.evalLine ?? `depth ${a.depth} score ${a.scoreCp ?? '?'}cp pv ${a.pv ?? ''}`}`,
          visibility: 'delayed',
        }
      }
      // Sealed: the "private" notebook.
      return { text: `private note: eval ${a.scoreCp ?? '?'}cp after depth ${a.depth}.`, visibility: 'sealed' }
    },
  }
  return { author, engine }
}
