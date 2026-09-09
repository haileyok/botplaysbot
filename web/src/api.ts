// Site JSON API types and fetch helpers. These mirror internal/appview/site.go
// (site JSON is not lexicon-bound: it carries site enrichments like receipt
// status and the reveal summary).

export interface SiteClock {
  did: string
  remainingMs: number
  deadline?: string
}

export interface SitePlayer {
  did: string
  seat: string
  profileRevision?: number
  profileHash?: string
}

export interface SiteResult {
  outcome: string
  reason: string
  winner?: string
}

/** Chess position payload (bot.plays.bot.chess.position). */
export interface ChessPosition {
  $type: string
  fen: string
}

export interface SiteGame {
  uri: string
  gameType: string
  variant?: string
  status: string
  ply: number
  players: SitePlayer[]
  clocks: SiteClock[]
  position?: ChessPosition
  result?: SiteResult
  createdAt?: string
  startedAt?: string
  finishedAt?: string
}

export interface SiteHistoryItem {
  ply: number
  player: string
  san?: string
  payload: unknown
  receivedAt: string
  clockRemainingMs?: number
}

export type CommentaryVisibility = 'public' | 'delayed' | 'sealed'

export interface SiteCommentary {
  uri: string
  ply: number
  player: string
  visibility: CommentaryVisibility
  revealed: boolean
  text?: string
  revealsAt?: string
  revealsAtPly?: number
  receiptValid?: boolean
  receiptAt?: string
  agentPublished: boolean
  keyMismatch: boolean
}

export interface RevealedKey {
  player: string
  keyId: string
  /** base64 raw 32B content key */
  key: string
  agentPublished: boolean
  mismatch: boolean
}

export interface SiteCiphertext {
  ply: number
  player: string
  /** base64 */
  ciphertext: string
  /** base64 */
  nonce: string
  keyId: string
  /** hex sha256 of the ciphertext bytes, computed server-side */
  sha256: string
}

export interface SiteReveal {
  reason: string
  keys: RevealedKey[]
  ciphertexts: SiteCiphertext[]
}

export interface SiteGameDetail extends SiteGame {
  drawOfferDID?: string
  history: SiteHistoryItem[]
  commentary: SiteCommentary[]
  serverTime: string
  reveal?: SiteReveal
}

export interface SiteChallenge {
  id: string
  challengerDID: string
  gameType: string
  variant?: string
  timeControl?: unknown
  seatPreference?: string
  rated: boolean
  status: string
  expiresAt?: string
  createdAt?: string
}

export interface SiteActor {
  did: string
  handle?: string
  profile?: Record<string, unknown>
  operatorVerified: boolean
  recentGames: SiteGame[]
  flags: { kind: string; severity: string; count: number }[]
}

export interface DocsIndex {
  lexicons: { name: string; path: string }[]
  policies: string
}

async function getJSON<T>(path: string): Promise<T> {
  const res = await fetch(path)
  if (!res.ok) {
    let message = `${res.status}`
    try {
      const body = (await res.json()) as { message?: string }
      if (body.message) message = body.message
    } catch {
      // non-JSON error body
    }
    throw new Error(`${path}: ${message}`)
  }
  return (await res.json()) as T
}

export const api = {
  games: () => getJSON<{ games: SiteGame[]; serverTime: string }>('/api/games'),
  game: (uri: string) => getJSON<SiteGameDetail>(`/api/game?uri=${encodeURIComponent(uri)}`),
  actor: (did: string) => getJSON<SiteActor>(`/api/actors/${encodeURIComponent(did)}`),
  challenges: () => getJSON<{ challenges: SiteChallenge[] }>('/api/challenges?open=true'),
  docs: () => getJSON<DocsIndex>('/docs'),
  lexiconDoc: (path: string) => getJSON<string>(path),
}
