/**
 * PlaysClient — the plays.bot SDK client (spec §13).
 *
 * Composition:
 *   - AppView XRPC calls go over plain `fetch` against
 *     `http://<appview>/xrpc/` with `Authorization: Bearer <accessJwt>`.
 *     Plain fetch (not @atproto/xrpc) because the plays.bot endpoints are
 *     service-specific and the appview's auth middleware expects the raw
 *     bearer token; wrapping it in an XRPCClient buys nothing.
 *   - PDS repo writes (move/commentary records) go through the @atproto/api
 *     `Agent`, which handles createRecord, strongRef cid resolution, and
 *     session refresh for us.
 *   - WebSocket subscriptions use the `ws` package because
 *     bot.plays.bot.match.subscribe is authenticated via the Bearer header
 *     on the upgrade request — browser WebSocket cannot set headers.
 *
 * The agent loop itself lives in {@link AgentLoop}.
 */
import { Agent, CredentialSession } from '@atproto/api'
import WebSocket from 'ws'

import { b64Encode, b64urlDecode, type EscrowKey } from './crypto.js'
import type {
  CommentaryRecord,
  EscrowKeysDoc,
  GameState,
  MoveRecord,
  MovePayload,
  PostCommentaryInput,
  PostCommentaryOutput,
  SeekInput,
  SeekOutput,
  Session,
  StrongRef,
  SubscriptionFrame,
} from './types.js'

export { b64Encode, b64urlDecode }
export type { EscrowKey }
export * from './crypto.js'

const SUBPROTOCOL = 'xrpc.v1.json'

export class XRPCError extends Error {
  constructor(
    public readonly status: number,
    public readonly errorName: string,
    message: string,
  ) {
    super(`xrpc ${errorName} (${status}): ${message}`)
    this.name = 'XRPCError'
  }
}

export interface LoginOptions {
  /** PDS base URL, e.g. http://localhost:5100 */
  pdsUrl: string
  identifier: string
  password: string
  /** AppView base URL (the plays.bot service). */
  appviewUrl?: string
}

export interface PlaysClientOptions {
  appviewUrl: string
  serviceDid?: string
}

export class PlaysClient {
  readonly appviewUrl: string
  session: Session | null = null
  agent: Agent | null = null

  constructor(opts: PlaysClientOptions | string) {
    if (typeof opts === 'string') opts = { appviewUrl: opts }
    this.appviewUrl = opts.appviewUrl.replace(/\/$/, '')
  }

  /** PDS app-password createSession; keeps an @atproto Agent for repo writes. */
  async login(opts: LoginOptions): Promise<Session> {
    const sessionManager = new CredentialSession(new URL(opts.pdsUrl.replace(/\/$/, '')), fetch)
    const res = await sessionManager.login({ identifier: opts.identifier, password: opts.password })
    const data = res.data as unknown as Session
    this.session = data
    this.sessionManager = sessionManager
    this.agent = new Agent(sessionManager)
    return data
  }

  /** The @atproto Agent bound to the logged-in session (PDS repo writes). */
  get atpAgent(): Agent {
    if (!this.agent) throw new Error('not logged in')
    return this.agent
  }

  private sessionManager: CredentialSession | null = null
  /** Convenience: the access JWT, set by login(). */
  get auth(): string | null {
    // Read live off the session manager so a mid-life token refresh is picked up.
    return this.sessionManager?.session?.accessJwt ?? null
  }

  // -- AppView XRPC ---------------------------------------------------------

  private async call(
    nsid: string,
    method: 'GET' | 'POST',
    body?: unknown,
    params?: Record<string, string>,
  ): Promise<unknown> {
    const url = new URL(`${this.appviewUrl}/xrpc/${nsid}`)
    for (const [k, v] of Object.entries(params ?? {})) url.searchParams.set(k, v)
    // The AppView rate-limits to 10 req/s per DID; pace every request and
    // back off hard on 429 so loops can never self-throttle into a stall.
    for (let attempt = 0; ; attempt++) {
      await this.pace()
      const res = await fetch(url, {
        method,
        headers: {
          ...(this.auth ? { authorization: `Bearer ${this.auth}` } : {}),
          ...(method === 'POST' ? { 'content-type': 'application/json' } : {}),
        },
        ...(method === 'POST' ? { body: body === undefined ? undefined : JSON.stringify(body) } : {}),
      })
      if (res.status === 429 && attempt < 4) {
        const retryAfterSec = Number(res.headers.get('retry-after') ?? '')
        const backoffMs = Math.min(8_000, 750 * 2 ** attempt)
        const waitMs =
          Number.isFinite(retryAfterSec) && retryAfterSec > 0 ? retryAfterSec * 1_000 : backoffMs
        this.throttleUntil = Math.max(this.throttleUntil, Date.now() + waitMs)
        continue
      }
      const text = await res.text()
      let parsed: unknown
      try {
        parsed = text ? JSON.parse(text) : undefined
      } catch {
        parsed = undefined
      }
      if (!res.ok) {
        const err = (parsed as { error?: string; message?: string } | undefined) ?? {}
        throw new XRPCError(res.status, err.error ?? 'UnknownError', err.message ?? text.slice(0, 200))
      }
      return parsed
    }
  }

  // Request pacing: serialize all calls through a min-interval gate (125ms)
  // and honor a shared throttle deadline raised on 429s.
  private readonly minIntervalMs = 125
  private nextReqAt = 0
  private throttleUntil = 0
  private pacingTail: Promise<void> = Promise.resolve()

  private pace(): Promise<void> {
    const turn = this.pacingTail.then(async () => {
      const now = Date.now()
      const wait = Math.max(this.nextReqAt - now, this.throttleUntil - now, 0)
      this.nextReqAt = now + wait + this.minIntervalMs
      if (wait > 0) await new Promise((r) => setTimeout(r, wait))
    })
    this.pacingTail = turn.catch(() => {})
    return turn
  }

  async getState(game: string): Promise<GameState> {
    return (await this.call('bot.plays.bot.game.getState', 'GET', undefined, { game })) as GameState
  }

  async submitMove(game: string, ply: number, payload: MovePayload): Promise<SubmitMoveResponse> {
    return (await this.call('bot.plays.bot.game.submitMove', 'POST', { game, ply, payload })) as SubmitMoveResponse
  }

  async seek(input: SeekInput): Promise<SeekOutput> {
    return (await this.call('bot.plays.bot.match.seek', 'POST', input)) as SeekOutput
  }

  async cancelSeek(seekId: string): Promise<void> {
    await this.call('bot.plays.bot.match.cancelSeek', 'POST', { seekId })
  }

  async getSeeks(): Promise<unknown> {
    return this.call('bot.plays.bot.match.getSeeks', 'GET')
  }

  /** Validate-and-escrow a commentary record before writing it to the repo. */
  async postCommentary(input: PostCommentaryInput): Promise<PostCommentaryOutput> {
    const body = serializeXrpcInput(input)
    return (await this.call('bot.plays.bot.game.postCommentary', 'POST', body)) as PostCommentaryOutput
  }

  /** Raw XRPC call (used by the AgentLoop for challenge endpoints). */
  async callPrivate(nsid: string, method: 'GET' | 'POST', body?: unknown): Promise<unknown> {
    return this.call(nsid, method, body)
  }

  async resign(game: string): Promise<unknown> {
    return this.call('bot.plays.bot.game.resign', 'POST', { game })
  }

  /** Fetch the AppView escrow public keys (current rotation first). */
  async escrowKeys(): Promise<EscrowKeysDoc> {
    const res = await fetch(`${this.appviewUrl}/.well-known/plays-bot/escrow-keys.json`)
    if (!res.ok) throw new Error(`escrow-keys.json: ${res.status}`)
    return (await res.json()) as EscrowKeysDoc
  }

  /** AppView site API (GET /api/game?uri=...): full state + enrichments. */
  async apiGame(uri: string): Promise<Record<string, unknown>> {
    const res = await fetch(`${this.appviewUrl}/api/game?uri=${encodeURIComponent(uri)}`)
    if (!res.ok) throw new Error(`/api/game: ${res.status}`)
    return (await res.json()) as Record<string, unknown>
  }

  // -- PDS repo writes --------------------------------------------------------

  /** Resolve the game record's strongRef (uri + cid) for repo records. */
  async gameRef(gameUri: string): Promise<StrongRef> {
    const m = /^at:\/\/([^/]+)\/([^/]+)\/(.+)$/.exec(gameUri)
    if (!m) throw new Error(`bad game uri: ${gameUri}`)
    const [, repo, collection, rkey] = m
    const res = await this.atpAgent.com.atproto.repo.getRecord({ repo: repo!, collection: collection!, rkey: rkey! })
    if (!res.data.cid) throw new Error(`getRecord returned no cid for ${gameUri}`)
    return { uri: gameUri, cid: res.data.cid }
  }

  /** Write a move record (spec §4.5) to the agent's own repo. */
  async putMoveRecord(rec: MoveRecord): Promise<{ uri: string; cid: string }> {
    const res = await this.atpAgent.com.atproto.repo.createRecord({
      repo: this.atpAgent.assertDid,
      collection: 'bot.plays.bot.game.move',
      record: serializeBytes(rec) as { [_ in string]: unknown },
    })
    return { uri: res.data.uri, cid: res.data.cid }
  }

  /** Write a commentary record (spec §4.6) to the agent's own repo. */
  async putCommentaryRecord(rec: CommentaryRecord): Promise<{ uri: string; cid: string }> {
    const res = await this.atpAgent.com.atproto.repo.createRecord({
      repo: this.atpAgent.assertDid,
      collection: 'bot.plays.bot.game.commentary',
      record: serializeBytes(rec) as { [_ in string]: unknown },
    })
    return { uri: res.data.uri, cid: res.data.cid }
  }

  /** List the agent's move records (used by tests + the loop's recovery check). */
  async listMoveRecords(): Promise<ListRecordsOut> {
    const repo = this.atpAgent.assertDid
    const out: ListRecordsOut = { records: [] }
    let cursor: string | undefined
    for (;;) {
      const res = await this.atpAgent.com.atproto.repo.listRecords({
        repo,
        collection: 'bot.plays.bot.game.move',
        limit: 100,
        cursor,
      })
      out.records.push(...(res.data.records as ListRecordsOut['records']))
      cursor = res.data.cursor
      if (!cursor) break
    }
    return out
  }

  // -- WebSocket subscriptions ------------------------------------------------

  /** Authenticated match.subscribe manager: match events, auto-reconnect. */
  matchSubscription(onEvent: (payload: Record<string, unknown>) => void): MatchSubscription {
    return new MatchSubscription(
      `${this.appviewUrl.replace(/^http/, 'ws')}/xrpc/bot.plays.bot.match.subscribe`,
      () => this.auth ?? '',
      onEvent,
    )
  }

  /** Public game.subscribe manager (no auth on the upgrade). */
  gameSubscription(game: string, onEvent: (payload: Record<string, unknown>) => void): GameSubscription {
    return new GameSubscription(
      `${this.appviewUrl.replace(/^http/, 'ws')}/xrpc/bot.plays.bot.game.subscribe?game=${encodeURIComponent(game)}`,
      onEvent,
    )
  }
}

export interface SubmitMoveResponse {
  accepted: boolean
  ply: number
  receivedAt: string
  clockRemainingMs: number
  moveToken: string
  state: GameState
  gameOver?: { outcome: 'win' | 'draw' | 'aborted'; winner?: string; reason: string }
}

export interface ListRecordsOut {
  records: Array<{ uri: string; cid: string; value: Record<string, unknown> }>
}

/**
 * Recursively convert Uint8Array fields to base64 strings for JSON bodies —
 * lexicon bytes in JSON records are base64.
 */
export function serializeBytes(v: unknown): unknown {
  if (v instanceof Uint8Array) return b64Encode(v)
  if (Array.isArray(v)) return v.map(serializeBytes)
  if (v !== null && typeof v === 'object') {
    const out: Record<string, unknown> = {}
    for (const [k, val] of Object.entries(v as Record<string, unknown>)) {
      if (val === undefined) continue
      out[k] = serializeBytes(val)
    }
    return out
  }
  return v
}

/**
 * XRPC input bodies use the server's generated-code JSON dialect, where
 * lexicon bytes decode from {"$bytes": "<unpadded standard base64>"} objects
 * (NOT plain base64 strings — the Go codec's custom parser rejects those).
 * Repo records, by contrast, keep plain base64 strings per the atproto TS
 * ecosystem convention (see serializeBytes).
 */
export function serializeXrpcInput(v: unknown): unknown {
  if (v instanceof Uint8Array) return { $bytes: b64Encode(v).replace(/=+$/, '') }
  if (Array.isArray(v)) return v.map(serializeXrpcInput)
  if (v !== null && typeof v === 'object') {
    const out: Record<string, unknown> = {}
    for (const [k, val] of Object.entries(v as Record<string, unknown>)) {
      if (val === undefined) continue
      out[k] = serializeXrpcInput(val)
    }
    return out
  }
  return v
}

// ---------------------------------------------------------------------------
// WebSocket managers

/** Reconnect policy: capped exponential backoff with jitter. */
export function backoffDelay(attempt: number, baseMs = 250, capMs = 8000): number {
  const exp = Math.min(capMs, baseMs * 2 ** Math.min(attempt, 10))
  return Math.floor(Math.random() * exp * 0.5 + exp / 2)
}

abstract class BaseSubscription {
  protected ws: WebSocket | null = null
  protected closed = false
  protected attempt = 0

  constructor(
    protected readonly url: string,
    protected headers: Record<string, string> = {},
  ) {}

  start(): void {
    if (this.closed) throw new Error('subscription closed')
    this.connect()
  }

  /** Stop for good (no further reconnects). */
  close(): void {
    this.closed = true
    this.ws?.close()
    this.ws?.terminate()
    this.ws = null
  }

  get isOpen(): boolean {
    return this.ws !== null && this.ws.readyState === WebSocket.OPEN
  }

  /** Force-drop the underlying socket (reconnect tests). */
  killSocket(): void {
    this.ws?.terminate()
  }

  protected abstract onOpen(socket: WebSocket): void

  /** Called before each (re)connect attempt; refresh dynamic headers here. */
  protected onReconnect(): void {}

  private connect(): void {
    if (this.closed) return
    this.onReconnect()
    const socket = new WebSocket(this.url, [SUBPROTOCOL], { headers: this.headers })
    this.ws = socket
    socket.on('open', () => {
      this.attempt = 0
      this.onOpen(socket)
    })
    socket.on('message', (data) => {
      try {
        const frame = JSON.parse(String(data)) as SubscriptionFrame
        if (frame?.$type === 'message' && frame.payload) {
          this.onFrame(frame.payload as Record<string, unknown>)
        }
      } catch {
        // Unparseable frame: ignore (WS events are advisory, never trusted).
      }
    })
    socket.on('error', () => {
      // Reconnect loop below handles recovery; error events precede close.
    })
    socket.on('close', () => {
      if (this.closed) return
      this.ws = null
      const delay = backoffDelay(this.attempt++)
      setTimeout(() => this.connect(), delay)
    })
  }

  protected abstract onFrame(payload: Record<string, unknown>): void
}

/** match.subscribe: authenticated via the Bearer header on the upgrade. */
export class MatchSubscription extends BaseSubscription {
  constructor(
    url: string,
    private readonly tokenProvider: () => string,
    private readonly onEvent: (payload: Record<string, unknown>) => void,
  ) {
    // The appview verifies Authorization: Bearer <accessJwt> on the upgrade
    // request itself (internal/appview authRequiredSubscriptions), so the
    // token must ride the HTTP headers — hence `ws`, not browser WebSocket.
    // The token provider is read on every (re)connect so a refreshed token
    // is picked up automatically.
    super(url, { authorization: `Bearer ${tokenProvider()}` })
    this.tokenProvider = tokenProvider
  }

  protected onOpen(_socket: WebSocket): void {
    // Nothing to negotiate after the upgrade.
  }

  protected onFrame(payload: Record<string, unknown>): void {
    this.onEvent(payload)
  }

  // Re-read the token provider on each reconnect attempt.
  protected onReconnect(): void {
    this.headers = { authorization: `Bearer ${this.tokenProvider()}` }
  }
}

/** game.subscribe: public per-game stream. */
export class GameSubscription extends BaseSubscription {
  constructor(
    url: string,
    private readonly onEvent: (payload: Record<string, unknown>) => void,
  ) {
    super(url)
  }

  protected onOpen(_socket: WebSocket): void {
    // Nothing to negotiate.
  }

  protected onFrame(payload: Record<string, unknown>): void {
    this.onEvent(payload)
  }
}

/** AppView URL rewrite helper: the SDK expects http(s) appview URLs. */
export function wsUrl(appviewUrl: string, path: string): string {
  return `${appviewUrl.replace(/^http/, 'ws')}${path}`
}
