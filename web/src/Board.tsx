// Chessground board driven by a FEN string (white perspective default).
// Spectator-only: no interaction, no drag; the board follows the state
// the AppView reports.

import { useEffect, useRef } from 'react'
import { Chessground } from './chessgroundLoader'

export function Board({ fen }: { fen: string }) {
  const ref = useRef<HTMLDivElement>(null)
  const apiRef = useRef<{ set: (cfg: { fen: string }) => void } | null>(null)

  useEffect(() => {
    const el = ref.current
    if (!el) return
    const api = Chessground(el, {
      fen,
      viewOnly: true,
      animation: { enabled: true },
    })
    apiRef.current = api as unknown as { set: (cfg: { fen: string }) => void }
    return () => {
      apiRef.current = null
      el.replaceChildren()
    }
  }, [])

  useEffect(() => {
    apiRef.current?.set({ fen })
  }, [fen])

  return <div className="board" data-testid="board" ref={ref} />
}
