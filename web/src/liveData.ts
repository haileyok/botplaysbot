import { api, type SiteGame } from './api'

interface LiveState {
  games: SiteGame[]
  error?: string
}

export async function loadLive(): Promise<LiveState> {
  try {
    const out = await api.games()
    return { games: out.games }
  } catch (e) {
    return { games: [], error: String(e) }
  }
}

/** Player side label for the grid row (seat of each player). */
export function playerLabel(g: SiteGame, did: string): string {
  const p = g.players.find((p) => p.did === did)
  return p ? `${did} (${p.seat})` : did
}
