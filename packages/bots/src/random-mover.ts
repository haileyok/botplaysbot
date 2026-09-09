/**
 * random-mover — the simplest possible plays.bot agent (spec §13).
 *
 * chooseMove: uniform random from state.legalMoves.
 * explain: mixes all three visibilities so integration tests exercise the
 * whole commentary stack (public → repo plaintext; delayed → escrow wrap +
 * reveal scheduler; sealed → revealed at game end via key publication).
 */
import type { AgentAuthor, GameState, MovePayload } from '@plays-bot/client'

export function randomMove(state: GameState): MovePayload {
  const moves = state.legalMoves
  if (moves.length === 0) {
    throw new Error('random-mover: no legal moves but the game is not terminal?')
  }
  return moves[Math.floor(Math.random() * moves.length)]!
}

/**
 * The visibility mix: ply 1 → public, ply 2 → sealed, otherwise cycle
 * public/delayed/sealed with a random component. Every game is guaranteed
 * to produce ≥1 delayed and ≥1 sealed note (ply 2 is always sealed and ply
 * 3 always delayed, and chess games always last ≥4 plies).
 */
export function randomExplain(state: GameState, payload: MovePayload): { text: string; visibility: 'public' | 'delayed' | 'sealed' } | null {
  const move = payload as { from?: string; to?: string }
  const san = `${move.from ?? '?'}${move.to ?? '?'}`
  const ideas = [
    `I'm eyeing the center — ${san} keeps my options open.`,
    `${san} felt natural. Development before material.`,
    `Not sure about ${san}; my eval is hazy here.`,
    `A quiet move. ${san} sets a small trap.`,
  ]
  const text = ideas[Math.floor(Math.random() * ideas.length)]!

  // Deterministic mix by ply, plus randomness so both orderings occur.
  const ply = state.ply + 1
  if (ply === 1) return { text: `Game on! Playing ${san}.`, visibility: 'public' }
  if (ply === 2) return { text: `Opening secret: I plan ${san} next.`, visibility: 'sealed' }
  if (ply === 3) return { text: `Planning ${san} — you'll see why in two moves.`, visibility: 'delayed' }
  const roll = Math.random()
  if (roll < 0.34) return { text, visibility: 'public' }
  if (roll < 0.67) return { text, visibility: 'delayed' }
  return { text, visibility: 'sealed' }
}

export const randomMoverAuthor: AgentAuthor = {
  chooseMove: randomMove,
  explain: randomExplain,
}
