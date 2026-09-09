/**
 * stockfish CLI. Env or args (same as random-mover, plus):
 *   STOCKFISH_VARIANT  asm (default) | wasm
 *   STOCKFISH_DEPTH    depth cap, default 1
 */
import { PlaysClient, AgentLoop } from '@plays-bot/client'

import { stockfishAuthor } from './stockfish-engine.js'

function arg(name: string): string | undefined {
  const i = process.argv.indexOf(`--${name}`)
  return i !== -1 ? process.argv[i + 1] : undefined
}

async function main(): Promise<void> {
  const appviewUrl = arg('appview') ?? process.env['PLAYSBOT_APPVIEW_URL'] ?? 'http://localhost:8080'
  const pdsUrl = arg('pds') ?? process.env['PLAYSBOT_PDS_URL']
  const identifier = arg('identifier') ?? process.env['PLAYSBOT_IDENTIFIER']
  const password = arg('password') ?? process.env['PLAYSBOT_PASSWORD']
  if (!pdsUrl || !identifier || !password) {
    console.error('stockfish: PLAYSBOT_PDS_URL, PLAYSBOT_IDENTIFIER, PLAYSBOT_PASSWORD required')
    process.exit(1)
  }

  const client = new PlaysClient({ appviewUrl })
  const session = await client.login({ pdsUrl, identifier, password })
  console.log(`stockfish logged in as ${session.handle} (${session.did})`)

  const { author, engine } = await stockfishAuthor({})
  const loop = new AgentLoop({
    client,
    author,
    logger: (...args) => console.log('[stockfish]', ...args),
  })
  loop.start()

  const stop = (): void => {
    loop.stop()
    void engine.stop().then(() => process.exit(0))
  }
  process.on('SIGINT', stop)
  process.on('SIGTERM', stop)
}

await main()
