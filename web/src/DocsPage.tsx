// Docs page (/docs): lexicon markdown listing + the embedded policies page
// (verbatim §7 clock definition and §8.3 escrow policy, per the spec's
// "publish verbatim in docs" requirement). Markdown is rendered client-side
// with a minimal renderer — headings, lists, code, inline code, emphasis —
// keeping dependencies at zero.

import { useEffect, useState } from 'react'
import { api, type DocsIndex } from './api'
import { Link } from './router'

export function DocsPage() {
  const [index, setIndex] = useState<DocsIndex>()
  const [error, setError] = useState<string>()

  useEffect(() => {
    void api
      .docs()
      .then(setIndex)
      .catch((e) => setError(String(e)))
  }, [])

  if (error) {
    return (
      <main className="page">
        <p className="error">{error}</p>
        <Link to="/">← live</Link>
      </main>
    )
  }
  if (!index) return <main className="page">loading…</main>

  return (
    <main className="page docs-page" data-testid="docs-page">
      <h1>Docs</h1>
      <section className="policies" data-testid="policies">
        {renderMarkdown(index.policies)}
      </section>
      <section className="lexicons">
        <h2>Lexicons</h2>
        <ul data-testid="lexicon-list">
          {index.lexicons.map((l) => (
            <li key={l.path}>
              <a href={l.path} target="_blank" rel="noreferrer">
                {l.name}
              </a>
            </li>
          ))}
        </ul>
      </section>
    </main>
  )
}

/** Minimal markdown rendering (headings, lists, blockquotes, code, inline). */
export function renderMarkdown(md: string): React.ReactNode[] {
  const out: React.ReactNode[] = []
  const lines = md.split('\n')
  let list: string[] = []
  let code: string[] | null = null

  const flushList = () => {
    if (list.length) {
      out.push(
        <ul key={`ul-${out.length}`}>
          {list.map((item, i) => (
            <li key={i}>{inline(item)}</li>
          ))}
        </ul>,
      )
      list = []
    }
  }

  for (const raw of lines) {
    if (raw.startsWith('```')) {
      if (code) {
        out.push(
          <pre key={`pre-${out.length}`}>
            <code>{code.join('\n')}</code>
          </pre>,
        )
        code = null
      } else {
        flushList()
        code = []
      }
      continue
    }
    if (code) {
      code.push(raw)
      continue
    }
    if (raw.startsWith('- ')) {
      list.push(raw.slice(2))
      continue
    }
    if (/^\d+\. /.test(raw)) {
      list.push(raw.replace(/^\d+\. /, ''))
      continue
    }
    flushList()
    const h = /^(#{1,4}) (.+)$/.exec(raw)
    if (h) {
      const level = h[1]?.length ?? 1
      const content = inline(h[2] ?? '')
      out.push(
        level <= 2 ? (
          <h2 key={`h-${out.length}`}>{content}</h2>
        ) : (
          <h3 key={`h-${out.length}`}>{content}</h3>
        ),
      )
      continue
    }
    if (raw.startsWith('> ')) {
      out.push(
        <blockquote key={`bq-${out.length}`}>{inline(raw.slice(2))}</blockquote>,
      )
      continue
    }
    if (raw.trim() === '') continue
    out.push(<p key={`p-${out.length}`}>{inline(raw)}</p>)
  }
  flushList()
  if (code) {
    out.push(
      <pre key={`pre-${out.length}`}>
        <code>{code.join('\n')}</code>
      </pre>,
    )
  }
  return out
}

function inline(s: string): React.ReactNode {
  // **bold**, *em*, `code`
  const parts: React.ReactNode[] = []
  const re = /(\*\*[^*]+\*\*|\*[^*]+\*|`[^`]+`)/g
  let last = 0
  let m: RegExpExecArray | null
  while ((m = re.exec(s))) {
    if (m.index > last) parts.push(s.slice(last, m.index))
    const tok = m[0]
    if (tok.startsWith('**')) parts.push(<strong key={m.index}>{tok.slice(2, -2)}</strong>)
    else if (tok.startsWith('`')) parts.push(<code key={m.index}>{tok.slice(1, -1)}</code>)
    else parts.push(<em key={m.index}>{tok.slice(1, -1)}</em>)
    last = m.index + tok.length
  }
  if (last < s.length) parts.push(s.slice(last))
  return parts
}
