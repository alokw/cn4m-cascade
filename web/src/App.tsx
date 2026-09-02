import { NavLink, Navigate, Route, Routes } from 'react-router-dom'
import { AuthProvider, useAuth } from './auth'
import { useEvents } from './hooks/useEvents'
import { SignIn } from './screens/SignIn'
import { Targets } from './screens/Targets'

/** The feed is either live or falling back to polling; both keep the data
 *  fresh, so this is a status line rather than an error. */
function FeedIndicator() {
  const { status, runs } = useEvents()
  const active = [...runs.values()].filter((r) => r.run.status === 'running').length

  return (
    <span className={`feed ${status}`} title={`Event feed: ${status}`}>
      <span className="dot" />
      {status === 'polling' ? 'polling' : status === 'connecting' ? 'connecting…' : 'live'}
      {active > 0 && <span className="muted"> · {active} running</span>}
    </span>
  )
}

function Shell() {
  const { signOut } = useAuth()

  return (
    <div className="shell">
      <header>
        <span className="brand">SMB Sync</span>
        <nav>
          <NavLink to="/targets">Targets</NavLink>
        </nav>
        <FeedIndicator />
        <button className="link" onClick={() => void signOut()}>
          Sign out
        </button>
      </header>

      <main>
        <Routes>
          <Route path="/targets" element={<Targets />} />
          {/* Dashboard, job editor, run detail and logs arrive in 4b-2. */}
          <Route path="*" element={<Navigate to="/targets" replace />} />
        </Routes>
      </main>
    </div>
  )
}

function Gate() {
  const { state } = useAuth()

  switch (state.status) {
    case 'loading':
      return <div className="centred muted">Loading…</div>
    case 'setup':
      return <SignIn mode="setup" />
    case 'anonymous':
      return <SignIn mode="login" />
    case 'signed-in':
      return <Shell />
  }
}

export function App() {
  return (
    <AuthProvider>
      <Gate />
    </AuthProvider>
  )
}
