// Live page (/): grid of active games, updated by the unfiltered spectator
// subscription (#gameStarted/#move/#gameFinished), plus an open-challenges
// strip from /api/challenges.

import { useCallback, useEffect, useState } from 'react'
import { api, type SiteChallenge, type SiteGame } from './api'
import { noteServerTime } from './clock'
import { subscribeGames, type SubscribeEvent } from './subscribe'
import { Link } from './router'

export function LivePage() {
  const [games, setGames] = useState<SiteGame[]>([])
  const [challenges, setChallenges] = useState<SiteChallenge[]>([])
  const [connected, setConnected] = useState(false)
  const [error, setError] = useState<string>()

  const refresh = useCallback(async () => {
    try {
      const [g, c] = await Promise.all([api.games(), api.challenges()])
      noteServerTime(g.serverTime)
      setGames(g.games)
      setChallenges(c.challenges)
      setError(undefined)
    } catch (e) {
      setError(String(e))
    }
  }, [])

  useEffect(() => {
    // State first, then live updates. On reconnect the wrapper fires
    // onStateChange(true) and we re-pull (events are advisory; state is
    // authoritative).
    void refresh()
    const client = subscribeGames({
      onStateChange: (up) => {
        setConnected(up)
        if (up) void refresh()
      },
      onEvent: (ev: SubscribeEvent) => {
        void applyEvent(setGames, ev)
      },
    })
    return () => client.close()
  }, [refresh])

  return (
    <main className="page live-page">
      <h1>Live games</h1>
      <p className="conn" data-testid="conn">
        {connected ? 'live' : 'connecting…'}
      </p>
      {error && <p className="error">{error}</p>}
      <section className="game-grid" data-testid="game-grid">
        {games.map((g) => (
          <GameCard key={g.uri} game={g} />
        ))}
        {games.length === 0 && <p className="empty">No active games right now.</p>}
      </section>
      <section className="challenges" data-testid="challenges">
        <h2>Open challenges</h2>
        {challenges.length === 0 && <p className="empty">No open challenges.</p>}
        <ul>
          {challenges.map((c) => (
            <li key={c.id} data-testid="challenge-row">
              <span className="mono">{c.challengerDID}</span>{' '}
              <span>
                {c.gameType}
                {c.variant ? ` · ${c.variant}` : ''}
              </span>{' '}
              <span>{c.rated ? 'rated' : 'casual'}</span>
            </li>
          ))}
        </ul>
      </section>
    </main>
  )
}

/** Reduce one subscription event into the games grid state. */
export async function applyEvent(
  setGames: (fn: (prev: SiteGame[]) => SiteGame[]) => void,
  ev: SubscribeEvent,
): Promise<void> {
  switch (ev.$type) {
    case 'bot.plays.bot.game.subscribe#gameStarted': {
      // New game: pull its authoritative state into the grid.
      try {
        const detail = await api.game(ev.game)
        setGames((prev) => (prev.some((g) => g.uri === ev.game) ? prev : [...prev, detail]))
      } catch {
        // game may have finished already; ignore
      }
      return
    }
    case 'bot.plays.bot.game.subscribe#move': {
      setGames((prev) =>
        prev.map((g) =>
          g.uri === ev.game
            ? {
                ...g,
                ply: ev.ply,
                position: ev.position ?? g.position,
                clocks: ev.clocks?.map((c) => ({ did: c.did, remainingMs: c.remainingMs, deadline: c.deadline })) ?? g.clocks,
              }
            : g,
        ),
      )
      return
    }
    case 'bot.plays.bot.game.subscribe#gameFinished': {
      setGames((prev) =>
        prev.map((g) => (g.uri === ev.game ? { ...g, status: 'finished', result: ev.result } : g)),
      )
      return
    }
    default:
      return
  }
}

function GameCard({ game }: { game: SiteGame }) {
  const white = game.players.find((p) => p.seat === 'white')?.did ?? '—'
  const black = game.players.find((p) => p.seat === 'black')?.did ?? '—'
  return (
    <Link to={`/game?uri=${encodeURIComponent(game.uri)}`} className="game-card" testId="game-card">
      <div className="game-card-players">
        <span>white: {white}</span>
        <span>black: {black}</span>
      </div>
      <div className="game-card-meta">
        {game.gameType}
        {game.variant ? ` · ${game.variant}` : ''} · ply {game.ply} · {game.status}
      </div>
      {game.result && (
        <div className="game-card-result">
          {game.result.outcome} ({game.result.reason})
        </div>
      )}
    </Link>
  )
}
