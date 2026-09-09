import { render, screen, waitFor } from '@testing-library/react'
import { expect, describe, it, vi, beforeEach, afterEach } from 'vitest'
import { LivePage } from './LivePage'
import { api, type SiteGame } from './api'

// Minimal mock state for the Live grid.
export function mockGame(over: Partial<SiteGame> = {}): SiteGame {
  return {
    uri: 'at://did:plc:svc/bot.plays.bot.game/3kx1',
    gameType: 'bot.plays.bot.chess',
    status: 'active',
    ply: 4,
    players: [
      { did: 'did:plc:white', seat: 'white' },
      { did: 'did:plc:black', seat: 'black' },
    ],
    clocks: [],
    ...over,
  }
}

vi.mock('./api', async (importOriginal) => {
  const orig = await importOriginal<typeof import('./api')>()
  return {
    ...orig,
    api: {
      ...orig.api,
      games: vi.fn(),
      game: vi.fn(),
      challenges: vi.fn(),
      actor: vi.fn(),
      docs: vi.fn(),
      lexiconDoc: vi.fn(),
    },
  }
})

const mock = vi.mocked(api, true)

describe('LivePage', () => {
  beforeEach(() => {
    vi.stubGlobal(
      'WebSocket',
      class FakeWebSocket {
        static instances: FakeWebSocket[] = []
        constructor() {
          FakeWebSocket.instances.push(this)
        }
        close = vi.fn()
        onopen: unknown = null
        onclose: unknown = null
        onmessage: unknown = null
        onerror: unknown = null
      },
    )
  })
  afterEach(() => {
    vi.unstubAllGlobals()
    vi.clearAllMocks()
  })

  it('renders grid rows for each active game', async () => {
    mock.games.mockResolvedValue({ games: [mockGame(), mockGame({ uri: 'at://x/2', ply: 9 })], serverTime: new Date().toISOString() })
    mock.challenges.mockResolvedValue({
      challenges: [
        {
          id: 'ch1',
          challengerDID: 'did:plc:challenger',
          gameType: 'bot.plays.bot.chess',
          rated: true,
          status: 'open',
        },
      ],
    })
    render(<LivePage />)
    await waitFor(() => expect(screen.getAllByTestId('game-card')).toHaveLength(2))
    expect(screen.getAllByTestId('challenge-row')).toHaveLength(1)
    // Player label renders as "white: <did>" inside one element, once per card.
    expect(screen.getAllByText((_, el) => el?.textContent === 'white: did:plc:white')).toHaveLength(2)
  })

  it('shows the empty state when no games are active', async () => {
    mock.games.mockResolvedValue({ games: [], serverTime: new Date().toISOString() })
    mock.challenges.mockResolvedValue({ challenges: [] })
    render(<LivePage />)
    await waitFor(() => expect(screen.getByText('No active games right now.')).toBeInTheDocument())
  })
})
