import { render, screen, waitFor, act } from '@testing-library/react'
import { expect, describe, it, vi, beforeEach, afterEach } from 'vitest'
import { GamePage, indicatorFor } from './GamePage'
import type { SiteCommentary, SiteGameDetail } from './api'

function baseDetail(over: Partial<SiteGameDetail> = {}): SiteGameDetail {
  return {
    uri: 'at://did:plc:svc/bot.plays.bot.game/3kx1',
    gameType: 'bot.plays.bot.chess',
    status: 'active',
    ply: 2,
    players: [
      { did: 'did:plc:white', seat: 'white' },
      { did: 'did:plc:black', seat: 'black' },
    ],
    clocks: [{ did: 'did:plc:white', remainingMs: 250_000, deadline: new Date(Date.now() + 250_000).toISOString() }],
    position: { $type: 'bot.plays.bot.chess.position', fen: 'rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR b KQkq - 0 1' },
    history: [
      {
        ply: 1,
        player: 'did:plc:white',
        san: 'e4',
        payload: { $type: 'bot.plays.bot.chess.move', from: 'e2', to: 'e4' },
        receivedAt: '2024-01-01T00:00:01Z',
        clockRemainingMs: 298_000,
      },
    ],
    commentary: [],
    serverTime: new Date().toISOString(),
    ...over,
  }
}

// Commentary entries covering every §12 indicator + the never-revealed case.
export const commentaryCases: SiteCommentary[] = [
  {
    uri: 'c1',
    ply: 1,
    player: 'did:plc:white',
    visibility: 'public',
    revealed: true,
    text: 'I want the center.',
    agentPublished: false,
    keyMismatch: false,
  },
  {
    uri: 'c2',
    ply: 2,
    player: 'did:plc:black',
    visibility: 'delayed',
    revealed: false,
    revealsAtPly: 4,
    agentPublished: false,
    keyMismatch: false,
  },
  {
    uri: 'c3',
    ply: 3,
    player: 'did:plc:white',
    visibility: 'sealed',
    revealed: false,
    agentPublished: false,
    keyMismatch: false,
  },
  {
    uri: 'c4',
    ply: 4,
    player: 'did:plc:black',
    visibility: 'sealed',
    revealed: false,
    // No reveal bounds: the agent never published a usable key.
    agentPublished: false,
    keyMismatch: false,
  },
]

const captured: { onEvent?: (ev: unknown) => void; onStateChange?: (up: boolean) => void } = {}

vi.mock('./subscribe', async (importOriginal) => {
  const orig = await importOriginal<typeof import('./subscribe')>()
  return {
    ...orig,
    subscribeGames: vi.fn((opts: { onEvent: (ev: unknown) => void; onStateChange?: (up: boolean) => void }) => {
      captured.onEvent = opts.onEvent
      captured.onStateChange = opts.onStateChange
      return { close: vi.fn() }
    }),
  }
})

vi.mock('./Board', () => ({
  Board: () => <div data-testid="board-stub" />,
}))

vi.mock('./api', async (importOriginal) => {
  const orig = await importOriginal<typeof import('./api')>()
  return { ...orig, api: { ...orig.api, game: vi.fn(), games: vi.fn(), challenges: vi.fn(), docs: vi.fn() } }
})

const apiGame = vi.mocked((await import('./api')).api.game, true)

describe('GamePage', () => {
  beforeEach(() => {
    apiGame.mockResolvedValue(baseDetail({ commentary: commentaryCases }))
  })
  afterEach(() => {
    vi.clearAllMocks()
  })

  it('renders board, move list, and the commentary panel with all indicator types', async () => {
    // Active game: public → bubble, delayed → clock, sealed → lock.
    apiGame.mockResolvedValue(
      baseDetail({ commentary: commentaryCases.slice(0, 3) }),
    )
    render(<GamePage uri="at://did:plc:svc/bot.plays.bot.game/3kx1" />)
    await waitFor(() => expect(screen.getByTestId('board-stub')).toBeInTheDocument())
    expect(screen.getByTestId('move-list')).toBeInTheDocument()
    expect(screen.getByText('e4')).toBeInTheDocument()
    const items = screen.getAllByTestId('commentary-item')
    expect(items).toHaveLength(3)
    // bubble / clock / lock
    expect(items[0]).toHaveAttribute('data-indicator', 'bubble')
    expect(items[1]).toHaveAttribute('data-indicator', 'clock')
    expect(items[2]).toHaveAttribute('data-indicator', 'lock')
    // §12 copy
    expect(screen.getByText('unlocks at move 4')).toBeInTheDocument()
    expect(screen.getByText('revealed at game end')).toBeInTheDocument()
  })

  it('renders finished-but-unrevealed entries as never revealed', async () => {
    apiGame.mockResolvedValue(
      baseDetail({
        status: 'finished',
        result: { outcome: 'win', reason: 'checkmate', winner: 'did:plc:black' },
        commentary: commentaryCases.filter((c) => c.visibility === 'sealed'),
      }),
    )
    render(<GamePage uri="at://did:plc:svc/bot.plays.bot.game/3kx1" />)
    await waitFor(() => expect(screen.getAllByTestId('commentary-item')).toHaveLength(2))
    for (const item of screen.getAllByTestId('commentary-item')) {
      expect(item).toHaveAttribute('data-indicator', 'never')
    }
    expect(screen.getAllByTestId('never-revealed')).toHaveLength(2)
  })

  it('applies the flash-in transition class when #commentaryRevealed arrives', async () => {
    render(<GamePage uri="at://did:plc:svc/bot.plays.bot.game/3kx1" />)
    await waitFor(() => expect(screen.getAllByTestId('commentary-item')).toHaveLength(4))
    act(() => {
      captured.onEvent?.({
        $type: 'bot.plays.bot.game.subscribe#commentaryRevealed',
        game: 'at://did:plc:svc/bot.plays.bot.game/3kx1',
        ply: 2,
        player: 'did:plc:black',
        text: 'Preparing f5.',
      })
    })
    await waitFor(() => expect(screen.getByText('Preparing f5.')).toBeInTheDocument())
    const items = screen.getAllByTestId('commentary-item')
    const revealedItem = items[1]
    if (!revealedItem) throw new Error('commentary item 1 missing')
    expect(revealedItem).toHaveAttribute('data-revealed', 'true')
    expect(revealedItem.className).toContain('flash-in')
    expect(revealedItem.className).toContain('revealed')
  })

  it('shows the finished banner with result + reason when the game is over', async () => {
    apiGame.mockResolvedValue(
      baseDetail({
        status: 'finished',
        result: { outcome: 'win', reason: 'checkmate', winner: 'did:plc:black' },
        reveal: { reason: 'gameEnd', keys: [], ciphertexts: [] },
      }),
    )
    render(<GamePage uri="at://did:plc:svc/bot.plays.bot.game/3kx1" />)
    await waitFor(() => expect(screen.getByTestId('finished-banner')).toBeInTheDocument())
    expect(screen.getByTestId('finished-banner').textContent).toContain('checkmate')
  })

  it('shows the draw banner when a draw is offered', async () => {
    apiGame.mockResolvedValue(baseDetail({ drawOfferDID: 'did:plc:white' }))
    render(<GamePage uri="at://did:plc:svc/bot.plays.bot.game/3kx1" />)
    await waitFor(() => expect(screen.getByTestId('draw-banner')).toBeInTheDocument())
  })

  it('verifies ciphertext hashes post-game: ✓ for match, ✗ for mismatch', async () => {
    const real = await vi.importActual<typeof import('./verify')>('./verify')
    // Server digests: first correct, second deliberately wrong.
    const ct1 = btoa('ciphertext-one')
    const good = await real.sha256HexOfBase64(ct1)
    apiGame.mockResolvedValue(
      baseDetail({
        status: 'finished',
        result: { outcome: 'win', reason: 'resignation', winner: 'did:plc:white' },
        reveal: {
          reason: 'gameEnd',
          keys: [{ player: 'did:plc:white', keyId: 'k1', key: btoa('k'), agentPublished: true, mismatch: false }],
          ciphertexts: [
            { ply: 1, player: 'did:plc:white', ciphertext: ct1, nonce: btoa('n'), keyId: 'k1', sha256: good },
            { ply: 2, player: 'did:plc:black', ciphertext: btoa('ciphertext-two'), nonce: btoa('n'), keyId: 'k1', sha256: 'deadbeef' },
          ],
        },
      }),
    )
    render(<GamePage uri="at://did:plc:svc/bot.plays.bot.game/3kx1" />)
    await waitFor(() => expect(screen.getAllByTestId('verify-note')).toHaveLength(2))
    const verdicts = screen.getAllByTestId('verify-verdict')
    await waitFor(() => expect(verdicts[0]?.textContent).toBe('✓'))
    await waitFor(() => expect(verdicts[1]?.textContent).toBe('✗'))
    // Receipt badge appears for receiptValid entries.
  })

  it('shows the receipt-verified badge only on receiptValid commentary', async () => {
    const detail = baseDetail({
      commentary: [
        { ...commentaryCases[0]!, receiptValid: true },
        { ...commentaryCases[1]!, receiptValid: false },
      ],
    })
    apiGame.mockResolvedValue(detail)
    render(<GamePage uri="at://did:plc:svc/bot.plays.bot.game/3kx1" />)
    await waitFor(() => expect(screen.getAllByTestId('commentary-item')).toHaveLength(2))
    expect(screen.getAllByTestId('receipt-badge')).toHaveLength(1)
  })
})

describe('indicatorFor', () => {
  it('maps the §12 vocabulary (active game)', () => {
    expect(indicatorFor({ ...commentaryCases[0]! })).toBe('bubble')
    expect(indicatorFor({ ...commentaryCases[1]! })).toBe('clock')
    expect(indicatorFor({ ...commentaryCases[2]! })).toBe('lock')
  })
  it('renders finished-but-unrevealed entries as never revealed (§8.2)', () => {
    expect(indicatorFor({ ...commentaryCases[3]! }, true)).toBe('never')
    expect(indicatorFor({ ...commentaryCases[2]! }, true)).toBe('never')
  })
})
