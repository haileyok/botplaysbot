// Server-time offset tracking and live clock countdown.
//
// Clocks tick client-side from the deadline timestamps plus the offset
// between the server's clock and ours (spec §7: the AppView stamps
// receivedAt; the site displays countdowns from the deadlines it reports).
// The offset is refreshed from every API response's serverTime.

import { useEffect, useState } from 'react'

/** Clock singleton: the freshest known offset (serverTime - clientNow, ms). */
let serverOffsetMs = 0

export function noteServerTime(serverTimeIso: string) {
  const server = Date.parse(serverTimeIso)
  if (!Number.isNaN(server)) {
    serverOffsetMs = server - Date.now()
  }
}

export function serverNow(): number {
  return Date.now() + serverOffsetMs
}

/** now: the current (offset-corrected) time, re-rendered every second. */
export function useNow(): number {
  const [now, setNow] = useState(serverNow())
  useEffect(() => {
    const t = setInterval(() => setNow(serverNow()), 1000)
    return () => clearInterval(t)
  }, [])
  return now
}

/** remainingMs left on a clock at time now (0 once past the deadline). */
export function remainingMs(clock: { remainingMs: number; deadline?: string }, now: number): number {
  if (!clock.deadline) return clock.remainingMs
  const dl = Date.parse(clock.deadline)
  if (Number.isNaN(dl)) return clock.remainingMs
  return Math.max(0, dl - now)
}

export function formatClock(ms: number): string {
  const total = Math.max(0, Math.ceil(ms / 1000))
  const m = Math.floor(total / 60)
  const s = total % 60
  return `${m}:${String(s).padStart(2, '0')}`
}
