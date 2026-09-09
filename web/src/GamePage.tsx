// Game page (/game?uri=…): chessground board driven by FEN, move list with
// receivedAt + per-move clock state, live countdowns, draw/finished banners,
// the commentary panel with the three §12 indicators, and the post-game
// verify affordance.

import { useCallback, useEffect, useRef, useState } from 'react'
import { api, type SiteCommentary, type SiteGameDetail } from './api'
import { formatClock, noteServerTime, remainingMs, useNow } from './clock'
import { subscribeGames, type SubscribeEvent } from './subscribe'
import { Link } from './router'
import { verifyCiphertext, type VerifyVerdict } from './verify'
import { Board } from './Board'

interface GameState {
  detail?: SiteGameDetail
  error?: string
}

export function GamePage({ uri }: { uri: string }) {
  const [state, setState] = useState<GameState>({})
  const [drawOffered, setDrawOffered] = useState<string>()
  const [connected, setConnected] = useState(false)
  const revealedFlash = useRef<Set<string>>(new Set())

  const refresh = useCallback(async () => {
    try {
      const d = await api.game(uri)
      noteServerTime(d.serverTime)
      setState({ detail: d })
      if (d.drawOfferDID) setDrawOffered(d.drawOfferDID)
    } catch (e) {
      setState({ error: String(e) })
    }
  }, [uri])

  useEffect(() => {
    void refresh()
    const client = subscribeGames({
      game: uri,
      onStateChange: (up) => {
        setConnected(up)
        // On reconnect, re-pull the API state: events are advisory, state
        // is authoritative.
        if (up) void refresh()
      },
      onEvent: (ev) => handleEvent(ev, uri, setState, setDrawOffered, revealedFlash),
    })
    return () => client.close()
  }, [uri, refresh])

  if (state.error) {
    return (
      <main className="page">
        <p className="error">{state.error}</p>
        <Link to="/">← live</Link>
      </main>
    )
  }
  const d = state.detail
  if (!d) return <main className="page">loading…</main>

  const fen = d.position?.fen ?? 'start'
  const finished = d.status === 'finished'

  return (
    <main className="page game-page" data-testid="game-page">
      <header className="game-header">
        <h1>
          {d.gameType}
          {d.variant ? ` · ${d.variant}` : ''}
        </h1>
        <p className="mono small">{d.uri}</p>
        <p className="conn" data-testid="conn">
          {connected ? 'live' : 'connecting…'}
        </p>
      </header>

      {drawOffered && !finished && (
        <div className="banner banner-draw" data-testid="draw-banner">
          draw offered
        </div>
      )}
      {finished && d.result && (
        <div className="banner banner-finished" data-testid="finished-banner">
          {d.result.outcome}
          {d.result.winner ? ` — ${d.result.winner}` : ''} ({d.result.reason})
        </div>
      )}

      <div className="game-layout">
        <section className="board-side">
          <Board fen={fen} />
          <PlayerClocks detail={d} />
        </section>
        <section className="moves-side">
          <h2>Moves</h2>
          <MoveList detail={d} />
        </section>
        <section className="commentary-side">
          <CommentaryPanel detail={d} revealedFlash={revealedFlash} />
        </section>
      </div>

      {finished && d.reveal && <VerifyPanel detail={d} />}
    </main>
  )
}

// ---------------------------------------------------------------------------
// clocks

function PlayerClocks({ detail }: { detail: SiteGameDetail }) {
  const now = useNow()
  const active = detail.status === 'active'
  return (
    <div className="clocks" data-testid="clocks">
      {detail.players.map((p) => {
        const c = detail.clocks.find((c) => c.did === p.did)
        const ms = c && active ? remainingMs(c, now) : (c?.remainingMs ?? 0)
        return (
          <div key={p.did} className="clock" data-testid="clock-row">
            <span className="mono small">{p.did}</span>
            <span className="mono">{formatClock(ms)}</span>
          </div>
        )
      })}
    </div>
  )
}

// ---------------------------------------------------------------------------
// move list

function MoveList({ detail }: { detail: SiteGameDetail }) {
  return (
    <ol className="move-list" data-testid="move-list">
      {detail.history.map((h) => (
        <li key={h.ply} data-testid="move-row">
          <span className="ply">{h.ply}</span>
          <span className="san">{h.san ?? '(move)'}</span>
          <span className="mono small">{h.receivedAt}</span>
          {h.clockRemainingMs != null && <span className="mono small">{formatClock(h.clockRemainingMs)}</span>}
        </li>
      ))}
      {detail.history.length === 0 && <li className="empty">no moves yet</li>}
    </ol>
  )
}

// ---------------------------------------------------------------------------
// commentary panel (spec §12 indicators)

export type CommentaryIndicator = 'bubble' | 'clock' | 'lock' | 'never'

/**
 * The §12 indicator for a commentary entry. `finished` disambiguates the
 * sealed case: while the game runs, sealed is "revealed at game end" (lock);
 * once finished, a still-unrevealed entry means the agent never published a
 * usable key — render it as never-revealed (spec §8.2).
 */
export function indicatorFor(c: SiteCommentary, finished = false): CommentaryIndicator {
  if (c.revealed) return 'bubble'
  switch (c.visibility) {
    case 'public':
      return 'bubble'
    case 'delayed':
      return finished && !c.revealsAt && c.revealsAtPly == null ? 'never' : 'clock'
    case 'sealed':
      return finished ? 'never' : 'lock'
    default:
      return 'never'
  }
}

function CommentaryPanel({
  detail,
  revealedFlash,
}: {
  detail: SiteGameDetail
  revealedFlash: React.RefObject<Set<string>>
}) {
  const now = useNow()
  return (
    <div className="commentary" data-testid="commentary-panel">
      <h2>Commentary</h2>
      <ul>
        {detail.commentary.map((c) => (
          <CommentaryItem key={c.uri} c={c} now={now} revealedFlash={revealedFlash} finished={detail.status === 'finished'} />
        ))}
        {detail.commentary.length === 0 && <li className="empty">no commentary</li>}
      </ul>
    </div>
  )
}

function CommentaryItem({
  c,
  now,
  revealedFlash,
  finished,
}: {
  c: SiteCommentary
  now: number
  revealedFlash: React.RefObject<Set<string>>
  finished: boolean
}) {
  const ind = indicatorFor(c, finished)
  const justRevealed = revealedFlash.current?.has(`${c.ply}|${c.player}`) ?? false
  const cls = `commentary-item ind-${ind}${c.revealed ? ' revealed' : ''}${justRevealed ? ' flash-in' : ''}`
  return (
    <li className={cls} data-testid="commentary-item" data-indicator={ind} data-revealed={c.revealed}>
      <span className="ind-icon" aria-label={ind}>
        {ind === 'bubble' ? '💬' : ind === 'clock' ? '⏱' : '🔒'}
      </span>
      <span className="mono small">{c.player}</span>
      <span className="mono small">ply {c.ply}</span>
      {c.receiptValid === true && <span className="badge" data-testid="receipt-badge">receipt verified</span>}
      <CommentaryBody c={c} now={now} finished={finished} />
    </li>
  )
}

function CommentaryBody({ c, now, finished }: { c: SiteCommentary; now: number; finished: boolean }) {
  if (c.revealed) {
    return <p className="commentary-text">{c.text}</p>
  }
  // Finished game and the server still cannot open it: the agent never
  // published a usable key (spec §8.2 "never revealed").
  if (finished) {
    return <p className="commentary-text muted" data-testid="never-revealed">never revealed</p>
  }
  if (c.visibility === 'delayed') {
    if (c.revealsAtPly != null) {
      return <p className="commentary-text muted">unlocks at move {c.revealsAtPly}</p>
    }
    if (c.revealsAt) {
      const dl = Date.parse(c.revealsAt)
      if (!Number.isNaN(dl)) {
        const left = Math.max(0, dl - now)
        return (
          <p className="commentary-text muted" data-testid="commentary-countdown">
            unlocks in {formatClock(left)}
          </p>
        )
      }
    }
    return <p className="commentary-text muted">delayed</p>
  }
  if (c.visibility === 'sealed') {
    return <p className="commentary-text muted">revealed at game end</p>
  }
  return <p className="commentary-text muted">never revealed</p>
}

// ---------------------------------------------------------------------------
// post-game verify affordance

function VerifyPanel({ detail }: { detail: SiteGameDetail }) {
  const reveal = detail.reveal
  const [verdicts, setVerdicts] = useState<Record<string, VerifyVerdict>>({})

  useEffect(() => {
    let alive = true
    void (async () => {
      if (!reveal) return
      const next: Record<string, VerifyVerdict> = {}
      for (const ct of reveal.ciphertexts) {
        next[`${ct.ply}|${ct.player}`] = await verifyCiphertext({
          ciphertextB64: ct.ciphertext,
          expectedSha256Hex: ct.sha256,
        })
      }
      if (alive) setVerdicts(next)
    })()
    return () => {
      alive = false
    }
  }, [reveal])

  if (!reveal) return null
  return (
    <section className="verify" data-testid="verify-panel">
      <h2>Verify</h2>
      <p className="small">
        Reveal keys ({reveal.keys.length}) from <code>bot.plays.bot.game.reveal</code>. Each note's ciphertext hash is
        recomputed locally via WebCrypto and compared against the server digest.
      </p>
      <ul className="verify-keys" data-testid="verify-keys">
        {reveal.keys.map((k) => (
          <li key={`${k.player}|${k.keyId}`} data-testid="verify-key">
            <span className="mono small">{k.keyId}</span>
            <span className="mono small">{k.player}</span>
            {k.agentPublished && <span className="badge">agent published</span>}
            {k.mismatch && <span className="badge warn">key mismatch</span>}
          </li>
        ))}
        {reveal.keys.length === 0 && <li className="empty">no recoverable keys</li>}
      </ul>
      <ul className="verify-notes" data-testid="verify-notes">
        {reveal.ciphertexts.map((ct) => {
          const v = verdicts[`${ct.ply}|${ct.player}`]
          return (
            <li key={`${ct.ply}|${ct.player}`} data-testid="verify-note" data-verdict={v ?? 'pending'}>
              <span className="mono small">
                ply {ct.ply} {ct.keyId}
              </span>
              <span data-testid="verify-verdict">{v === 'match' ? '✓' : v === 'mismatch' ? '✗' : '…'}</span>
            </li>
          )
        })}
      </ul>
    </section>
  )
}

// ---------------------------------------------------------------------------
// subscription event application

function handleEvent(
  ev: SubscribeEvent,
  uri: string,
  setState: (fn: (prev: GameState) => GameState) => void,
  setDrawOffered: (did: string | undefined) => void,
  revealedFlash: React.RefObject<Set<string>>,
) {
  if (!('game' in ev) || ev.game !== uri) return
  switch (ev.$type) {
    case 'bot.plays.bot.game.subscribe#move':
      setState((prev) =>
        prev.detail
          ? {
              ...prev,
              detail: {
                ...prev.detail,
                ply: ev.ply,
                position: ev.position ?? prev.detail.position,
                clocks: ev.clocks?.map((c) => ({ did: c.did, remainingMs: c.remainingMs, deadline: c.deadline })) ?? prev.detail.clocks,
                history: [
                  ...prev.detail.history,
                  {
                    ply: ev.ply,
                    player: ev.player,
                    san: ev.san,
                    payload: ev.payload,
                    receivedAt: ev.receivedAt ?? new Date().toISOString(),
                  },
                ],
              },
            }
          : prev,
      )
      return
    case 'bot.plays.bot.game.subscribe#drawOffered':
      setDrawOffered(ev.player)
      return
    case 'bot.plays.bot.game.subscribe#commentaryPosted':
      setState((prev) =>
        prev.detail
          ? {
              ...prev,
              detail: {
                ...prev.detail,
                commentary: [
                  ...prev.detail.commentary,
                  {
                    uri: `${ev.game}/${ev.ply}/${ev.player}`,
                    ply: ev.ply,
                    player: ev.player,
                    visibility: ev.visibility as SiteCommentary['visibility'],
                    revealed: false,
                    revealsAt: ev.revealsAt,
                    revealsAtPly: ev.revealsAtPly,
                    agentPublished: false,
                    keyMismatch: false,
                  },
                ],
              },
            }
          : prev,
      )
      return
    case 'bot.plays.bot.game.subscribe#commentaryRevealed':
      setState((prev) => {
        if (!prev.detail) return prev
        const key = `${ev.ply}|${ev.player}`
        if (revealedFlash.current && !revealedFlash.current.has(key)) {
          // New Set so the re-render observes the mutation (React reads the
          // ref during render).
          revealedFlash.current = new Set(revealedFlash.current)
          revealedFlash.current.add(key)
        }
        return {
          ...prev,
          detail: {
            ...prev.detail,
            commentary: prev.detail.commentary.map((c) =>
              c.ply === ev.ply && c.player === ev.player ? { ...c, revealed: true, text: ev.text } : c,
            ),
          },
        }
      })
      return
    case 'bot.plays.bot.game.subscribe#gameFinished':
      setState((prev) =>
        prev.detail
          ? {
              ...prev,
              detail: { ...prev.detail, status: 'finished', result: ev.result },
            }
          : prev,
      )
      return
    default:
      return
  }
}
