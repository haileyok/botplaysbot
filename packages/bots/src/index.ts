export { randomMove, randomExplain, randomMoverAuthor } from './random-mover.js'
export { StockfishEngine, stockfishAuthor } from './stockfish-engine.js'
export type { EngineAnalysis, StockfishOptions } from './stockfish-engine.js'

// Re-export the SDK so bot authors (and the integration tests) import one module.
export * from '@plays-bot/client'
