// Minimal history-API router (SPA fallback is served by the embedded
// web handler; routes never hit the server for unknown paths).

import { useEffect, useState, type ReactNode } from 'react'

export interface Route {
  path: string
  params: URLSearchParams
}

export function parseRoute(): Route {
  const u = new URL(window.location.href)
  return { path: u.pathname, params: u.searchParams }
}

export function navigate(path: string) {
  window.history.pushState(null, '', path)
  window.dispatchEvent(new PopStateEvent('popstate'))
}

export function Link(props: { to: string; children: ReactNode; className?: string; testId?: string }) {
  return (
    <a
      href={props.to}
      className={props.className}
      data-testid={props.testId}
      onClick={(e) => {
        if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
        e.preventDefault()
        navigate(props.to)
      }}
    >
      {props.children}
    </a>
  )
}

export function useRoute(): Route {
  const [route, setRoute] = useState(parseRoute)
  useEffect(() => {
    const on = () => setRoute(parseRoute())
    window.addEventListener('popstate', on)
    return () => window.removeEventListener('popstate', on)
  }, [])
  return route
}
