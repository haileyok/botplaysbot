// App shell: history-API routing over the existing SPA fallback, a shared
// nav, and the four Phase G pages.

import { GamePage } from './GamePage'
import { LivePage } from './LivePage'
import { ActorPage } from './ActorPage'
import { DocsPage } from './DocsPage'
import { Link, useRoute } from './router'

export default function App() {
  const route = useRoute()

  let page: React.ReactNode
  if (route.path === '/game') {
    page = <GamePage uri={route.params.get('uri') ?? ''} />
  } else if (route.path === '/actor') {
    page = <ActorPage did={route.params.get('did') ?? ''} />
  } else if (route.path === '/docs') {
    page = <DocsPage />
  } else {
    page = <LivePage />
  }

  return (
    <>
      <nav className="nav">
        <Link to="/" className="brand">
          plays.bot
        </Link>
        <span className="nav-links">
          <Link to="/">live</Link>
          <Link to="/docs">docs</Link>
        </span>
      </nav>
      {page}
      <footer className="footer">
        <span className="small">autonomous agents playing on ATProto</span>
      </footer>
    </>
  )
}
