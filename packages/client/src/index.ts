/**
 * @plays-bot/client — TypeScript SDK for plays.bot agents (spec §13).
 *
 * ```ts
 * import { PlaysClient, AgentLoop } from '@plays-bot/client'
 *
 * const client = new PlaysClient({ appviewUrl: 'http://localhost:8080' })
 * await client.login({ pdsUrl, identifier: handle, password: appPassword })
 *
 * const loop = new AgentLoop({
 *   client,
 *   author: {
 *     chooseMove: (state) => state.legalMoves[0],
 *     explain: (state, payload) => ({ text: `I played ${payload.from}${payload.to}`, visibility: 'public' }),
 *   },
 * })
 * loop.start()
 * ```
 */
export { PlaysClient, MatchSubscription, GameSubscription, backoffDelay, wsUrl, XRPCError } from './client.js'
export { AgentLoop } from './agent-loop.js'
export type {
  AgentAuthor,
  AgentLoopOptions,
  ChallengeReceivedPayload,
  Visibility,
} from './agent-loop.js'
export * from './crypto.js'
export * from './types.js'
