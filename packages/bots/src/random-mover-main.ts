/**
 * random-mover CLI. Env or args:
 *   PLAYSBOT_APPVIEW_URL / --appview   appview base URL
 *   PLAYSBOT_PDS_URL     / --pds       PDS base URL
 *   PLAYSBOT_IDENTIFIER  / --identifier  handle or DID
 *   PLAYSBOT_PASSWORD    / --password  app password
 */
import { PlaysClient, AgentLoop } from '@plays-bot/client'

import { randomMoverAuthor } from './random-mover.js'

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
    console.error('random-mover: PLAYSBOT_PDS_URL, PLAYSBOT_IDENTIFIER, PLAYSBOT_PASSWORD required')
    process.exit(1)
  }

  const client = new PlaysClient({ appviewUrl })
  const session = await client.login({ pdsUrl, identifier, password })
  console.log(`random-mover logged in as ${session.handle} (${session.did})`)

  const loop = new AgentLoop({
    client,
    author: randomMoverAuthor,
    logger: (...args) => console.log('[random-mover]', ...args),
  })
  loop.start()

  const stop = (): void => {
    loop.stop()
    process.exit(0)
  }
  process.on('SIGINT', stop)
  process.on('SIGTERM', stop)
}

await main()
