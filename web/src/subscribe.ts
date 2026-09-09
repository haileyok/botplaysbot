// Client for the public bot.plays.bot.game.subscribe XRPC subscription
// (spec §5.4). The browser connects to /xrpc/bot.plays.bot.game.subscribe
// negotiating the xrpc.v1.json subprotocol (the plain WebSocket constructor
// accepts a subprotocols array; the server picks it in the 101 response).
// Frames are single JSON text objects:
//
//   {"$type":"message","payload":{"$type":"bot.plays.bot.game.subscribe#move",...}}
//
// Events are advisory; state is authoritative. The wrapper therefore
// re-pulls the REST state on every (re)connect — mirroring the SDK's
// "events + state pull" philosophy — and reconnects with capped backoff.
// Pass a game AT-URI to filter to one game; omit it for the spectator
// firehose.

export type SubscribeEvent =
  | { $type: 'bot.plays.bot.game.subscribe#gameStarted'; game: string }
  | {
      $type: 'bot.plays.bot.game.subscribe#move'
      game: string
      ply: number
      player: string
      payload: { $type: string; from?: string; to?: string; [k: string]: unknown }
      san?: string
      receivedAt?: string
      position?: { $type: string; fen: string }
      clocks?: { did: string; remainingMs: number; deadline?: string }[]
    }
  | { $type: 'bot.plays.bot.game.subscribe#drawOffered'; game: string; player: string }
  | { $type: 'bot.plays.bot.game.subscribe#gameFinished'; game: string; result: SiteResultLike }
  | {
      $type: 'bot.plays.bot.game.subscribe#commentaryPosted'
      game: string
      ply: number
      player: string
      visibility: string
      revealsAt?: string
      revealsAtPly?: number
    }
  | {
      $type: 'bot.plays.bot.game.subscribe#commentaryRevealed'
      game: string
      ply: number
      player: string
      text: string
    }
  | { $type: 'bot.plays.bot.game.subscribe#error'; error: string; message?: string }

// Local structural type to avoid importing api.ts here (keeps this module
// dependency-free and trivially testable).
interface SiteResultLike {
  outcome: string
  reason: string
  winner?: string
}

export interface GameSubscribeClient {
  close(): void
}

const MAX_BACKOFF_MS = 10_000

export function subscribeGames(
  opts: { game?: string; onEvent: (ev: SubscribeEvent) => void; onStateChange?: (connected: boolean) => void },
): GameSubscribeClient {
  let stopped = false
  let attempt = 0
  let ws: WebSocket | undefined
  let reconnectTimer: ReturnType<typeof setTimeout> | undefined

  const connect = () => {
    if (stopped) return
    const url = new URL('/xrpc/bot.plays.bot.game.subscribe', window.location.origin)
    if (opts.game) url.searchParams.set('game', opts.game)
    // The second constructor argument selects subprotocols; the browser
    // sends them in the `Sec-WebSocket-Protocol` header and asserts the
    // server's pick on the 101. Negotiating xrpc.v1.json yields JSON text
    // frames instead of CBOR.
    ws = new WebSocket(url, ['xrpc.v1.json'])

    ws.onopen = () => {
      attempt = 0
      opts.onStateChange?.(true)
    }
    ws.onmessage = (m) => {
      if (typeof m.data !== 'string') return
      let frame: { $type?: string; payload?: { $type?: string; error?: string; message?: string } }
      try {
        frame = JSON.parse(m.data)
      } catch {
        return
      }
      if (frame.$type === 'error') {
        opts.onEvent({
          $type: 'bot.plays.bot.game.subscribe#error',
          error: frame.payload?.error ?? 'unknown',
          message: frame.payload?.message,
        })
        return
      }
      const payload = frame.payload as SubscribeEvent | undefined
      if (payload && typeof payload.$type === 'string' && payload.$type.startsWith('bot.plays.bot.game.subscribe#')) {
        opts.onEvent(payload)
      }
    }
    ws.onclose = () => {
      opts.onStateChange?.(false)
      if (stopped) return
      const delay = Math.min(MAX_BACKOFF_MS, 500 * 2 ** attempt) + Math.random() * 250
      attempt++
      reconnectTimer = setTimeout(connect, delay)
    }
    ws.onerror = () => {
      // onclose follows; nothing to do here.
    }
  }

  connect()

  return {
    close() {
      stopped = true
      if (reconnectTimer) clearTimeout(reconnectTimer)
      ws?.close()
    },
  }
}
