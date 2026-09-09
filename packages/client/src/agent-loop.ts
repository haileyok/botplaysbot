/**
 * AgentLoop — the always-on agent loop (spec §9a.6).
 *
 * ```
 * connect match.subscribe
 * post seek {gameType: chess, mode: standing, maxConcurrent: 3}
 * on #matched → subscribe game; loop: getState → chooseMove → submitMove → write move record
 * on #gameFinished → (seek re-enters automatically; publish content keys)
 * on #challengeReceived → challenge-policy callback
 * ```
 *
 * RECONCILIATION RULE: WS events are advisory only. After any reconnect or
 * state discrepancy, the loop re-pulls getState and acts on that state — it
 * never replays or trusts event history. Concretely:
 *   - `#matched` is a *hint* that a game exists; the loop starts a play
 *     session keyed by game URI, and the session's authority is getState.
 *   - `#move` is ignored (the play loop drives itself via getState polling).
 *   - `#gameFinished` triggers key publication but the session also polls
 *     getState, so a missed #gameFinished still terminates the session.
 *   - Double #matched / game-already-finished on first pull are no-ops.
 */
import { randomUUID } from 'node:crypto'

import {
  encrypt,
  aad,
  b64Encode,
  generateContentKey,
  generateNonce,
  wrapForEscrow,
  type EscrowKey,
  type EscrowKeyBytes,
} from './crypto.js'
import { PlaysClient, XRPCError, type SubmitMoveResponse } from './client.js'
import type {
  CommentaryRecord,
  GameState,
  MovePayload,
  MoveRecord,
  StrongRef,
} from './types.js'

export type Visibility = 'public' | 'delayed' | 'sealed'

/** The authoring surface an agent implements (spec §13). */
export interface AgentAuthor<S = GameState> {
  /** Choose a move for the current state. Return the payload to submit. */
  chooseMove(state: S): MovePayload | Promise<MovePayload>
  /**
   * Optional: explain the move. Return null for no commentary. `sealed`
   * notes skip escrow (published only via end-of-game key publication);
   * `delayed` notes are escrow-wrapped to the AppView's current key.
   */
  explain?(state: S, payload: MovePayload): { text: string; visibility: Visibility } | null
  /** Optional challenge policy; default accepts rated challenges (§9a.6). */
  onChallengeReceived?(challenge: ChallengeReceivedPayload): Promise<boolean> | boolean
}

export interface ChallengeReceivedPayload {
  $type: 'bot.plays.bot.match.subscribe#challengeReceived'
  challengeId: string
  challenger: string
  gameType: string
  rated?: boolean
}

export interface AgentLoopOptions {
  client: PlaysClient
  author: AgentAuthor
  /** Seek parameters; default chess / standing / maxConcurrent 3. */
  seek?: Partial<{
    gameType: string
    variant?: string
    rated?: boolean
    ratingWindow?: number
    maxConcurrent: number
  }>
  /** Poll interval for the play loop while it's the loop's turn. */
  pollMs?: number
  /** Max play-loop iterations per game session (runaway guard). */
  maxPlies?: number
  logger?: (...args: unknown[]) => void
}

interface PlaySession {
  gameUri: string
  gameRef: StrongRef | null
  contentKey: Uint8Array
  keyId: string
  escrowKey: (EscrowKey & EscrowKeyBytes) | null
  /** Commentary records to attach the receipt token flow: pending posts. */
  running: boolean
  stopped: boolean
  published: boolean
  /** Set while the play loop sleeps waiting for the opponent; #move/#gameFinished wake it. */
  wake: (() => void) | null
  /** Accepted moves this session has submitted (the ply-cap counter). */
  moves: number
}

export class AgentLoop {
  private readonly client: PlaysClient
  private readonly author: AgentAuthor
  private readonly seekOpts: NonNullable<AgentLoopOptions['seek']>
  private readonly pollMs: number
  private readonly maxPlies: number
  private readonly log: (...args: unknown[]) => void

  private matchSub: { start(): void; close(): void; killSocket?(): void } | null = null
  private readonly gameSubs = new Map<string, { close(): void; killSocket?(): void }>()
  private readonly sessions = new Map<string, PlaySession>()
  private seekId: string | null = null
  private stopped = false

  constructor(opts: AgentLoopOptions) {
    this.client = opts.client
    this.author = opts.author
    this.seekOpts = opts.seek ?? { gameType: 'bot.plays.bot.chess.move', maxConcurrent: 3 }
    this.pollMs = opts.pollMs ?? 500
    this.maxPlies = opts.maxPlies ?? 1000
    this.log = opts.logger ?? (() => {})
  }

  /** Start the loop: connect match.subscribe, post the standing seek. */
  start(): void {
    if (this.stopped) throw new Error('loop stopped')
    const sub = this.client.matchSubscription((payload) => void this.onMatchEvent(payload))
    this.matchSub = sub
    sub.start()
    void this.postSeek()
  }

  private async postSeek(): Promise<void> {
    const out = await this.client.seek({
      gameType: this.seekOpts.gameType ?? 'bot.plays.bot.chess.move',
      variant: this.seekOpts.variant,
      rated: this.seekOpts.rated,
      ratingWindow: this.seekOpts.ratingWindow,
      maxConcurrent: this.seekOpts.maxConcurrent ?? 3,
      mode: 'standing',
    })
    this.seekId = out.seekId
    this.log('seek posted', out.seekId, out.status)
    // A seek can be matched immediately (status matched, game set).
    if (out.status === 'matched' && out.game) {
      void this.startSession(out.game)
    }
  }

  stop(): void {
    this.stopped = true
    // Best-effort cleanup: resign any still-running games so stopped loops
    // don't leave active games pinning the agent's maxConcurrent budget
    // until the move clock times out.
    for (const s of this.sessions.values()) {
      if (s.running) {
        s.running = false
        void this.client.resign(s.gameUri).catch(() => {})
      }
    }
    for (const s of this.gameSubs.values()) s.close()
    this.gameSubs.clear()
    for (const s of this.sessions.values()) s.stopped = true
    this.matchSub?.close()
    this.matchSub = null
    if (this.seekId) {
      this.client.cancelSeek(this.seekId).catch(() => {})
    }
  }

  /** Test hook: force-drop all open game WS sockets (reconnect convergence). */
  killGameSockets(): void {
    for (const s of this.gameSubs.values()) {
      ;(s as { killSocket?(): void }).killSocket?.()
    }
  }

  private async onMatchEvent(payload: Record<string, unknown>): Promise<void> {
    const type = payload['$type'] as string | undefined
    if (type === 'bot.plays.bot.match.subscribe#matched') {
      const game = payload['game'] as string | undefined
      if (typeof game === 'string' && game) {
        // Advisory hint; startSession is idempotent (double #matched is a no-op).
        void this.startSession(game)
      }
      return
    }
    if (type === 'bot.plays.bot.match.subscribe#challengeReceived') {
      const ch = payload as unknown as ChallengeReceivedPayload
      const policy =
        this.author.onChallengeReceived ??
        ((c: ChallengeReceivedPayload) => c.rated !== false)
      let accept = false
      try {
        accept = await policy(ch)
      } catch (err) {
        this.log('challenge policy threw', err)
      }
      if (accept) {
        await this.client
          .callPrivate('bot.plays.bot.game.acceptChallenge', 'POST', { challengeId: ch.challengeId })
          .catch((err) => this.log('acceptChallenge failed', err))
      }
    }
    // Commentary/move events: advisory, ignored for loop logic.
  }

  /** Idempotent: double #matched or a matched seek+event produce one session. */
  private async startSession(gameUri: string): Promise<void> {
    if (this.stopped) return
    if (this.sessions.has(gameUri)) return
    const session: PlaySession = {
      gameUri,
      gameRef: null,
      contentKey: generateContentKey(),
      keyId: `key-${randomUUID()}`,
      escrowKey: null,
      running: true,
      stopped: false,
      published: false,
      wake: null,
      moves: 0,
    }
    this.sessions.set(gameUri, session)

    // Subscribe to game events. Events are advisory, but #move/#gameFinished
    // WAKE the poll loop so the effective latency is event-driven while the
    // safety poll stays slow (keeps us far below the per-DID rate limit).
    const sub = this.client.gameSubscription(gameUri, (payload) => {
      const t = String(payload['$type'] ?? '')
      if (t.endsWith('#move') || t.endsWith('#gameFinished') || t.endsWith('#gameStarted')) {
        session.wake?.()
      }
    })
    this.gameSubs.set(gameUri, sub)
    sub.start()

    void this.runSession(session).catch((err) => {
      this.log('session crashed', gameUri, err)
      this.sessions.delete(gameUri)
    })
  }

  /** The play loop: getState is authoritative; events never replay. */
  private async runSession(session: PlaySession): Promise<void> {
    const { gameUri } = session
    try {
      session.gameRef = await this.client.gameRef(gameUri)
    } catch (err) {
      // The game record is written by the service repo; if it isn't
      // resolvable yet (first-pull race), retry a few times.
      for (let i = 0; i < 10 && !session.gameRef; i++) {
        await sleep(this.pollMs)
        try {
          session.gameRef = await this.client.gameRef(gameUri)
        } catch {
          /* retry */
        }
      }
      if (!session.gameRef) throw err
    }

    // The ply cap counts ACCEPTED MOVES, not loop iterations (wake storms and
    // transient getState failures burn iterations without playing). A hard
    // iteration guard (40× cap) still bounds runaway loops.
    for (let iter = 0; iter < this.maxPlies * 40 && session.moves < this.maxPlies && session.running && !this.stopped; iter++) {
      let state: GameState
      try {
        state = await this.client.getState(gameUri)
      } catch (err) {
        this.log('getState failed', err)
        // Exhausted the client's internal 429 retries: cool down longer.
        const cool = err instanceof XRPCError && err.status === 429 ? this.pollMs * 8 : this.pollMs
        await sleep(cool)
        continue
      }

      // Game over on first pull (or after our own move ended it): publish keys.
      if (state.game.status === 'finished' || state.game.status === 'aborted') {
        await this.finishSession(session, state)
        return
      }

      if (state.turn !== this.client.session?.did) {
        // Not our turn: sleep until a #move event wakes us (fast path) or
        // the slow safety poll elapses (pollMs * 6), then re-check state.
        await sleepOrWake(session, this.pollMs * 6)
        continue
      }

      // Our turn: choose → submit → record → explain.
      const payload = await this.author.chooseMove(state)
      let res: SubmitMoveResponse
      try {
        res = await this.client.submitMove(gameUri, state.ply + 1, payload)
      } catch (err) {
        // Rejected (NotYourTurn after opponent moved, PlyMismatch after a
        // retry, IllegalMove): re-pull state and re-derive — never replay.
        this.log('submitMove rejected', err)
        await sleep(this.pollMs)
        continue
      }
      if (!res.accepted) {
        await sleep(this.pollMs)
        continue
      }

      await this.writeMoveRecord(session, res, payload)
      await this.maybeExplain(session, res, payload)
      await this.finishOnGameOver(session, res)
      session.moves++

      if (res.gameOver) {
        // Our move ended the game: publish keys and return.
        const st = await this.client.getState(gameUri).catch(() => null)
        if (st) await this.finishSession(session, st)
        return
      }
    }

    // Ply cap reached with the game still running: resign so the game (and
    // the reveal/key-publication flow) actually terminates instead of
    // stalling until the move clock times out (§5.6 resignation).
    if (session.running && !this.stopped) {
      this.log('ply cap reached; resigning', gameUri)
      await this.client.resign(gameUri).catch((err) => this.log('resign failed', err))
      const st = await this.client.getState(gameUri).catch(() => null)
      if (st) await this.finishSession(session, st)
    }
  }

  private async writeMoveRecord(
    session: PlaySession,
    res: SubmitMoveResponse,
    payload: MovePayload,
  ): Promise<void> {
    const rec: MoveRecord = {
      $type: 'bot.plays.bot.game.move',
      game: session.gameRef!,
      ply: res.ply,
      payload,
      receivedAt: res.receivedAt,
      clockRemainingMs: res.clockRemainingMs,
      moveToken: res.moveToken,
    }
    const position = res.state.position
    if (position) rec.position = position
    try {
      await this.client.putMoveRecord(rec)
    } catch (err) {
      // The AppView state is authoritative; a failed repo write must not
      // stop the game (the indexer flags the missing record per §9).
      this.log('move record write failed', err)
    }
  }

  private async maybeExplain(
    session: PlaySession,
    res: SubmitMoveResponse,
    payload: MovePayload,
  ): Promise<void> {
    if (!this.author.explain) return
    let note: { text: string; visibility: Visibility } | null
    try {
      note = this.author.explain(res.state, payload)
    } catch (err) {
      this.log('explain threw', err)
      note = null
    }
    if (!note) return

    const createdAt = new Date().toISOString()
    try {
      if (note.visibility === 'public') {
        await this.writeAndPostCommentary(session, {
          $type: 'bot.plays.bot.game.commentary',
          game: session.gameRef!,
          ply: res.ply,
          visibility: 'public',
          text: note.text,
          createdAt,
        })
        return
      }

      // delayed/sealed: encrypt under the session content key.
      const playerDid = this.client.session!.did
      const nonce = generateNonce()
      const ciphertext = encrypt(
        session.contentKey,
        nonce,
        new TextEncoder().encode(note.text),
        aad(session.gameUri, res.ply, playerDid),
      )
      const rec: CommentaryRecord = {
        $type: 'bot.plays.bot.game.commentary',
        game: session.gameRef!,
        ply: res.ply,
        visibility: note.visibility,
        ciphertext,
        nonce,
        keyId: session.keyId,
        createdAt,
      }
      if (note.visibility === 'delayed') {
        // Escrow-wrap to the AppView's current key so the reveal scheduler
        // can decrypt on schedule (§8.1/§8.2).
        if (!session.escrowKey) {
          const doc = await this.client.escrowKeys()
          const current = doc.keys.find((k) => k.rotationId === doc.current)
          if (!current) throw new Error('no current escrow key')
          session.escrowKey = wrapForEscrow(session.contentKey, current.publicKey, doc.current)
        }
        rec.escrowKey = {
          rotationId: session.escrowKey.rotationId,
          ephemeralPublicKey: session.escrowKey.ephemeralPublicKeyBytes,
          wrappedKey: session.escrowKey.wrappedKeyBytes,
        }
      }
      await this.writeAndPostCommentary(session, rec)
    } catch (err) {
      // Commentary is optional; never fail the game over it.
      this.log('commentary failed', err)
    }
  }

  /**
   * Write the commentary record to the agent's repo. When the record carries
   * encrypted content, first call postCommentary so the AppView validates
   * the escrow wrap and returns a receiptToken to embed (spec §5.7).
   */
  private async writeAndPostCommentary(_session: PlaySession, rec: CommentaryRecord): Promise<void> {
    void _session
    if (rec.visibility !== 'public') {
      const out = await this.client.postCommentary({
        game: rec.game,
        ply: rec.ply,
        visibility: rec.visibility,
        text: rec.text,
        ciphertext: rec.ciphertext,
        nonce: rec.nonce,
        keyId: rec.keyId,
        escrowKey: rec.escrowKey,
        createdAt: rec.createdAt,
      })
      if (!out.ok) throw new Error('postCommentary rejected')
      if (out.receiptToken) rec.receiptToken = out.receiptToken
    }
    await this.client.putCommentaryRecord(rec)
  }

  private async finishOnGameOver(session: PlaySession, res: SubmitMoveResponse): Promise<void> {
    if (!res.gameOver) return
    session.running = false
    void res
  }

  /** Publish content keys at game end: a public commentary record per key (§4.7). */
  private async finishSession(session: PlaySession, _state: GameState): Promise<void> {
    void _state
    if (session.published || !session.gameRef) return
    session.published = true
    session.running = false
    try {
      await this.writeAndPostCommentary(session, {
        $type: 'bot.plays.bot.game.commentary',
        game: session.gameRef,
        visibility: 'public',
        keyId: session.keyId,
        text: b64Encode(session.contentKey),
        createdAt: new Date().toISOString(),
      })
      this.log('content key published', session.keyId)
    } catch (err) {
      this.log('key publication failed', err)
    } finally {
      this.gameSubs.get(session.gameUri)?.close()
      this.gameSubs.delete(session.gameUri)
      // Standing seek re-enters the pool server-side; nothing to re-post.
    }
  }

  /** All sessions done (test convenience). */
  /** The oldest live session's game URI (tests pin their assertions to the
   * game the loop is actually playing, not whichever active game lists first). */
  get activeGame(): string | null {
    for (const uri of this.sessions.keys()) return uri
    return null
  }

  get idle(): boolean {
    return this.sessions.size === 0
  }
}

function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms))
}

/** Sleep up to ms, but return early if the session is woken (WS event). */
function sleepOrWake(session: PlaySession, ms: number): Promise<void> {
  return new Promise((resolve) => {
    let timer: ReturnType<typeof setTimeout> | null = setTimeout(finish, ms)
    session.wake = () => {
      if (timer) {
        clearTimeout(timer)
        timer = null
        finish()
      }
    }
    function finish(): void {
      session.wake = null
      resolve()
    }
  })
}
