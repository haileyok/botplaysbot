import { render, screen, waitFor } from '@testing-library/react'
import { expect, describe, it, vi } from 'vitest'
import { DocsPage, renderMarkdown } from './DocsPage'
import { ActorPage } from './ActorPage'
import { api } from './api'

vi.mock('./api', async (importOriginal) => {
  const orig = await importOriginal<typeof import('./api')>()
  return { ...orig, api: { ...orig.api, docs: vi.fn(), actor: vi.fn() } }
})

describe('DocsPage', () => {
  it('renders the policies markdown (verbatim §7/§8.3 content) and the lexicon list', async () => {
    vi.mocked(api.docs).mockResolvedValue({
      policies: [
        '# Policies',
        '',
        '## Clock',
        '',
        '- Deadline = `previousMove.receivedAt + perMoveSeconds`.',
        '',
        '> Reveal when **either** ply N+P has been accepted.',
      ].join('\n'),
      lexicons: [{ name: 'bot.plays.bot.game', path: '/docs/lexicons/bot.plays.bot.game.md' }],
    })
    render(<DocsPage />)
    await waitFor(() => expect(screen.getByTestId('policies')).toBeInTheDocument())
    expect(screen.getByTestId('policies').textContent).toContain('previousMove.receivedAt + perMoveSeconds')
    expect(screen.getByTestId('lexicon-list')).toBeInTheDocument()
    expect(screen.getByText('bot.plays.bot.game')).toBeInTheDocument()
  })
})

describe('renderMarkdown', () => {
  it('renders headings, lists, blockquotes, and inline styles', () => {
    const { container } = render(
      <div>{renderMarkdown('# H\n\n- item one\n- item two\n\n> quoted **bold** and `code`\n\nplain *em*')}</div>,
    )
    expect(container.querySelector('h2')?.textContent).toBe('H')
    expect(container.querySelectorAll('li')).toHaveLength(2)
    expect(container.querySelector('blockquote')).not.toBeNull()
    expect(container.querySelector('strong')?.textContent).toBe('bold')
    expect(container.querySelector('code')?.textContent).toBe('code')
    expect(container.querySelector('em')?.textContent).toBe('em')
  })
})

describe('ActorPage', () => {
  it('renders handle, DID, verified badge, and recent games', async () => {
    vi.mocked(api.actor).mockResolvedValue({
      did: 'did:plc:white',
      handle: 'white.example.com',
      operatorVerified: true,
      recentGames: [
        {
          uri: 'at://did:plc:svc/bot.plays.bot.game/abc',
          gameType: 'bot.plays.bot.chess',
          status: 'finished',
          ply: 12,
          players: [],
          clocks: [],
        },
      ],
      flags: [{ kind: 'timingAnomaly', severity: 'info', count: 2 }],
    })
    render(<ActorPage did="did:plc:white" />)
    await waitFor(() => expect(screen.getByTestId('actor-page')).toBeInTheDocument())
    expect(screen.getByTestId('actor-handle').textContent).toBe('white.example.com')
    expect(screen.getByTestId('actor-did').textContent).toBe('did:plc:white')
    expect(screen.getByTestId('actor-verified')).toBeInTheDocument()
    expect(screen.getAllByTestId('actor-game-row')).toHaveLength(1)
  })
})
