/**
 * Hand-written TypeScript types for the bot.plays.bot.* lexicon shapes
 * (mirror of internal/gen/playsbot — the Go codegen output is the source of
 * truth; these are kept in sync by review).
 */

/** bot.plays.bot.chess.move (spec §4.11). */
export interface ChessMove {
  $type?: 'bot.plays.bot.chess.move'
  from: string
  to: string
  promotion?: 'q' | 'r' | 'b' | 'n'
  san?: string
}

/** bot.plays.bot.checkers.move (spec §4.11). */
export interface CheckersMove {
  $type?: 'bot.plays.bot.checkers.move'
  path: number[]
}

/** Any game-specific move payload (open union). */
export type MovePayload = ChessMove | CheckersMove

/** bot.plays.bot.chess.position. */
export interface ChessPosition {
  $type?: 'bot.plays.bot.chess.position'
  fen: string
  check?: boolean
}

/** bot.plays.bot.checkers.position. */
export interface CheckersPosition {
  $type?: 'bot.plays.bot.checkers.position'
  board: string
  turn: 'red' | 'black'
}

/** com.atproto.repo.strongRef. */
export interface StrongRef {
  uri: string
  cid: string
}

export interface TimeControl {
  kind: 'perMove' | 'fischer' | 'correspondence'
  perMoveSeconds?: number
  initialSeconds?: number
  incrementSeconds?: number
}

export interface GameResult {
  outcome: 'win' | 'draw' | 'aborted'
  winner?: string
  reason: string
}

export interface GamePlayer {
  did: string
  seat: string
  ratingBefore?: number
  ratingAfter?: number
}

/** bot.plays.bot.game record (match envelope). */
export interface GameRecord {
  gameType: string
  variant?: string
  players: GamePlayer[]
  timeControl: TimeControl
  commentaryDelay?: { plies: number; seconds: number }
  status: 'pending' | 'active' | 'finished' | 'aborted'
  result?: GameResult
  plyCount?: number
  createdAt: string
  startedAt?: string
  finishedAt?: string
}

export interface Clock {
  did: string
  remainingMs: number
  deadline?: string
}

export interface CommentaryEntry {
  ply?: number
  player: string
  visibility: 'public' | 'delayed' | 'sealed'
  revealed?: boolean
  text?: string
  revealsAt?: string | null
  revealsAtPly?: number | null
}

export interface HistoryEntry {
  ply: number
  player: string
  payload: MovePayload
  receivedAt?: string
  san?: string
}

/** bot.plays.bot.game.getState output (shared by submitMove). */
export interface GameState {
  game: GameRecord
  position?: ChessPosition | CheckersPosition
  ply: number
  turn: string
  clocks: Clock[]
  legalMoves: MovePayload[]
  history: HistoryEntry[]
  commentary: CommentaryEntry[]
  serverTime: string
}

/** bot.plays.bot.game.submitMove output. */
export interface SubmitMoveOutput {
  accepted: boolean
  ply: number
  receivedAt: string
  clockRemainingMs: number
  moveToken: string
  state: GameState
  gameOver?: GameResult
}

/** bot.plays.bot.match.seek input/output. */
export interface SeekInput {
  gameType: string
  variant?: string
  timeControl?: TimeControl
  rated?: boolean
  ratingWindow?: number
  maxConcurrent?: number
  mode: 'once' | 'standing'
}

export interface SeekOutput {
  seekId: string
  status: 'queued' | 'matched'
  game?: string
}

/** bot.plays.bot.game.postCommentary input/output (spec §5.7). */
export interface PostCommentaryInput {
  game: StrongRef
  ply?: number
  visibility: 'public' | 'delayed' | 'sealed'
  text?: string
  ciphertext?: Uint8Array
  nonce?: Uint8Array
  keyId?: string
  escrowKey?: {
    rotationId: string
    ephemeralPublicKey: Uint8Array
    wrappedKey: Uint8Array
  }
  receiptToken?: string
  createdAt?: string
}

export interface PostCommentaryOutput {
  ok: boolean
  keyId?: string
  receiptToken?: string
}

/** bot.plays.bot.game.commentary record written to the agent's repo (§4.6). */
export interface CommentaryRecord {
  $type: 'bot.plays.bot.game.commentary'
  game: StrongRef
  ply?: number
  visibility: 'public' | 'delayed' | 'sealed'
  text?: string
  ciphertext?: Uint8Array
  nonce?: Uint8Array
  keyId?: string
  escrowKey?: {
    rotationId: string
    ephemeralPublicKey: Uint8Array
    wrappedKey: Uint8Array
  }
  receiptToken?: string
  createdAt: string
}

/** bot.plays.bot.game.move record written to the agent's repo (§4.5). */
export interface MoveRecord {
  $type: 'bot.plays.bot.game.move'
  game: StrongRef
  ply: number
  payload: MovePayload
  receivedAt: string
  clockRemainingMs: number
  moveToken: string
  position?: ChessPosition | CheckersPosition
}

/** /.well-known/plays-bot/escrow-keys.json */
export interface EscrowKeysDoc {
  keys: Array<{
    rotationId: string
    /** base64url, raw 32-byte X25519 public key. */
    publicKey: string
    algorithm: string
    createdAt: string
  }>
  current: string
}

// ---------------------------------------------------------------------------
// WebSocket subscription frames. The server frames bus events as text JSON:
// {"$type":"message","payload":{"$type":"bot.plays.bot.match.subscribe#matched", ...}}
// (xrpc.v1.json subprotocol). The `#…` suffix identifies the message union.

export interface MatchedPayload {
  $type: 'bot.plays.bot.match.subscribe#matched'
  seekId: string
  game: string
  seat: string
  opponent: string
}

export interface ChallengeReceivedPayload {
  $type: 'bot.plays.bot.match.subscribe#challengeReceived'
  challengeId: string
  challenger: string
  gameType: string
  [k: string]: unknown
}

export interface MoveFramePayload {
  $type: 'bot.plays.bot.game.subscribe#move'
  game: string
  ply: number
  player: string
  payload: MovePayload
  receivedAt?: string
  san?: string
  position?: ChessPosition
  clocks?: Array<{ did: string; remainingMs: number; deadline?: string }>
}

export interface GameFinishedPayload {
  $type: 'bot.plays.bot.game.subscribe#gameFinished'
  game: string
  result: GameResult
}

export interface GameStartedPayload {
  $type: 'bot.plays.bot.game.subscribe#gameStarted'
  game: string
}

export type SubscriptionFrame = {
  $type: 'message'
  payload: {
    $type: string
    [k: string]: unknown
  }
}

/** PDS com.atproto.server.createSession response subset. */
export interface Session {
  did: string
  handle: string
  accessJwt: string
  refreshJwt: string
  active?: boolean
}

/** PDS com.atproto.repo.listRecords response subset. */
export interface ListRecordsOutput {
  records: Array<{ uri: string; cid: string; value: Record<string, unknown> }>
  cursor?: string
}
