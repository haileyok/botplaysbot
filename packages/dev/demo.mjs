/**
 * demo.mjs — plays.bot demo: boots the full stack (Postgres via compose db,
 * in-memory dev PDS, the Go appview) and pits two example bots against each
 * other — a TypeScript random-mover (SDK-driven) vs the Go random-mover
 * (atmos-driven) — then prints the URL to watch live.
 *
 *   pnpm demo              TS random vs Go random (default)
 *   pnpm demo:stockfish    TS stockfish vs Go random
 *
 * Runs until Ctrl-C (SIGINT): teardown stops the bots, drops the throwaway
 * database, and kills the stack. Needs: docker (compose db), node, and
 * `make build` artifacts (built automatically if missing).
 */
import { spawn, spawnSync } from 'node:child_process'
import { existsSync } from 'node:fs'
import { createRequire } from 'node:module'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const require = createRequire(import.meta.url)
const __dirname = path.dirname(fileURLToPath(import.meta.url))
const ROOT = path.resolve(__dirname, '../..')

const args = process.argv.slice(2)
const stockfish = args.includes('--stockfish') || process.env['DEMO_STOCKFISH'] === '1'
const maxPlies = Number.parseInt(process.env['DEMO_MAX_PLIES'] ?? '60', 10)

// launch.mjs (and the appview) expect DATABASE_URL; default to the compose
// `db` service the demo just ensured.
process.env['DATABASE_URL'] ??= 'postgres://playsbot:playsbot@localhost:5432/playsbot?sslmode=disable'

function run(cmd, cmdArgs, opts = {}) {
  const res = spawnSync(cmd, cmdArgs, { stdio: 'inherit', cwd: ROOT, ...opts })
  if (res.status !== 0) {
    console.error(`demo: \`${cmd} ${cmdArgs.join(' ')}\` failed (${res.status})`)
    process.exit(1)
  }
}

async function main() {
  // 1. Postgres (compose service `db`); tolerate an already-running server.
  console.log('demo: ensuring Postgres (docker compose up -d db)…')
  run('docker', ['compose', 'up', '-d', 'db'])

  // 2. Build artifacts (appview + Go bot + web bundle).
  if (!existsSync(path.join(ROOT, 'bin', 'appview')) || !existsSync(path.join(ROOT, 'bin', 'bot-random'))) {
    console.log('demo: building (make build)…')
    run('make', ['build'])
  }

  // 3. Full stack: fresh DB + in-memory PDS + appview + seeded accounts.
  const { launch } = await import('./launch.mjs')
  const stack = await launch({ agents: 2 })
  const procs = []
  let stopping = false

  async function teardown() {
    if (stopping) return
    stopping = true
    console.log('\ndemo: shutting down…')
    for (const p of procs) p.kill('SIGTERM')
    await stack.teardown().catch(() => {})
    console.log('demo: done (throwaway database dropped, PDS + appview stopped).')
    process.exit(0)
  }
  process.on('SIGINT', () => void teardown())
  process.on('SIGTERM', () => void teardown())

  try {
    // 4. The bots.
    const [a, b] = stack.accounts.agents
    const env = {
      ...process.env,
      MAX_PLIES: String(maxPlies),
      PLAYSBOT_APPVIEW_URL: stack.appviewUrl,
      PLAYSBOT_PDS_URL: stack.pdsUrl,
    }

    // Go random-mover (atmos client): APPVIEW_URL/PDS_URL/BOT_*.
    procs.push(
      spawn(path.join(ROOT, 'bin', 'bot-random'), [], {
        cwd: ROOT,
        env: {
          ...env,
          APPVIEW_URL: stack.appviewUrl,
          PDS_URL: stack.pdsUrl,
          BOT_IDENTIFIER: a.handle,
          BOT_APP_PASSWORD: a.appPassword,
        },
        stdio: ['ignore', 'inherit', 'inherit'],
      }),
    )

    // TypeScript bot: random-mover (default) or stockfish via tsx.
    const tsMain = stockfish ? 'src/stockfish-main.ts' : 'src/random-mover-main.ts'
    const tsx = path.join(ROOT, 'packages', 'bots', 'node_modules', '.bin', 'tsx')
    procs.push(
      spawn(tsx, [tsMain], {
        cwd: path.join(ROOT, 'packages', 'bots'),
        env: { ...env, PLAYSBOT_IDENTIFIER: b.handle, PLAYSBOT_PASSWORD: b.appPassword },
        stdio: ['ignore', 'inherit', 'inherit'],
      }),
    )

    console.log('\n──────────────────────────────────────────────────────────')
    console.log(`  plays.bot demo: ${stockfish ? 'TS stockfish' : 'TS random-mover'} vs Go random-mover`)
    console.log(`  ply cap ${maxPlies} (resignation ends the game; keys publish after)`)
    console.log('')
    console.log(`  ▶ Watch live:  ${stack.appviewUrl}/`)
    console.log(`  ▶ This game:   ${stack.appviewUrl}/  (Live grid → click the game)`)
    console.log(`  ▶ Docs:        ${stack.appviewUrl}/docs`)
    console.log('')
    console.log('  Ctrl-C to stop (bots resign live games, database dropped).')
    console.log('──────────────────────────────────────────────────────────\n')

    // Keep running until SIGINT. Standing seeks keep the bots playing games.
    await new Promise(() => {})
  } catch (err) {
    console.error('demo failed:', err)
    await teardown()
  }
}

main().catch((err) => {
  console.error('demo failed:', err)
  process.exit(1)
})
