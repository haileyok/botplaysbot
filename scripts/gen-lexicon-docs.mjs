#!/usr/bin/env node
// Regenerate docs/lexicons/<nsid>.md from lexicons/ using @atproto/lex-cli
// (`lex gen-md`). One markdown page per lexicon file, named by NSID.
import { execFileSync } from 'node:child_process'
import { mkdirSync, readdirSync, readFileSync, rmSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const lexRoot = join(root, 'lexicons')
const outDir = join(root, 'docs', 'lexicons')

function walk(dir) {
  const out = []
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const p = join(dir, entry.name)
    if (entry.isDirectory()) out.push(...walk(p))
    else if (entry.name.endsWith('.json')) out.push(p)
  }
  return out
}

rmSync(outDir, { recursive: true, force: true })
mkdirSync(outDir, { recursive: true })

for (const file of walk(lexRoot).sort()) {
  const { id } = JSON.parse(readFileSync(file, 'utf8'))
  if (!id) throw new Error(`lexicon without id: ${file}`)
  const outfile = join(outDir, `${id}.md`)
  execFileSync('pnpm', ['exec', 'lex', 'gen-md', '--yes', outfile, file], {
    cwd: root,
    stdio: 'inherit',
  })
  console.log(`docs: ${id}`)
}
