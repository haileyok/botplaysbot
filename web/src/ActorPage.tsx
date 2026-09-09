// Actor page (/actor?did=…): handle, DID, recent games.

import { useEffect, useState } from 'react'
import { api, type SiteActor } from './api'
import { Link } from './router'

export function ActorPage({ did }: { did: string }) {
  const [actor, setActor] = useState<SiteActor>()
  const [error, setError] = useState<string>()

  useEffect(() => {
    let alive = true
    void api
      .actor(did)
      .then((a) => {
        if (alive) setActor(a)
      })
      .catch((e) => {
        if (alive) setError(String(e))
      })
    return () => {
      alive = false
    }
  }, [did])

  if (error) {
    return (
      <main className="page">
        <p className="error">{error}</p>
        <Link to="/">← live</Link>
      </main>
    )
  }
  if (!actor) return <main className="page">loading…</main>

  return (
    <main className="page actor-page" data-testid="actor-page">
      <h1 data-testid="actor-handle">{actor.handle || actor.did}</h1>
      <p className="mono small" data-testid="actor-did">
        {actor.did}
      </p>
      {actor.operatorVerified && <span className="badge" data-testid="actor-verified">verified operator</span>}
      <h2>Recent games</h2>
      <ul className="actor-games" data-testid="actor-games">
        {actor.recentGames.map((g) => (
          <li key={g.uri} data-testid="actor-game-row">
            <Link to={`/game?uri=${encodeURIComponent(g.uri)}`}>
              {g.gameType}
              {g.variant ? ` · ${g.variant}` : ''} — ply {g.ply} — {g.status}
            </Link>
          </li>
        ))}
        {actor.recentGames.length === 0 && <li className="empty">no games</li>}
      </ul>
    </main>
  )
}
