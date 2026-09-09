import { NavLink, Navigate, Route, Routes } from 'react-router-dom'
import { AuthProvider, useAuth } from './auth'
import { EventsProvider, useEvents } from './hooks/useEvents'
import { SignIn } from './screens/SignIn'
import { Dashboard } from './screens/Dashboard'
import { JobEditor } from './screens/JobEditor'
import { Jobs } from './screens/Jobs'
import { Logs } from './screens/Logs'
import { RunDetail } from './screens/RunDetail'
import { Settings } from './screens/Settings'
import { Runs } from './screens/Runs'
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
    <EventsProvider>
    <div className="shell">
      <header>
        <span className="brand">cn4m cascade</span>
        <nav>
          {/* `end` so the dashboard is not marked active on every other page:
              every path starts with "/". */}
          <NavLink to="/" end>
            Dashboard
          </NavLink>
          <NavLink to="/jobs">Jobs</NavLink>
          <NavLink to="/targets">Targets</NavLink>
          <NavLink to="/runs">Runs</NavLink>
          <NavLink to="/logs">Logs</NavLink>
          <NavLink to="/settings">Settings</NavLink>
        </nav>
        <FeedIndicator />
        <button className="link" onClick={() => void signOut()}>
          Sign out
        </button>
      </header>

      <main>
        <Routes>
          <Route path="/" element={<Dashboard />} />
          <Route path="/jobs" element={<Jobs />} />
          <Route path="/jobs/new" element={<JobEditor />} />
          <Route path="/jobs/:id" element={<JobEditor />} />
          <Route path="/targets" element={<Targets />} />
          <Route path="/runs" element={<Runs />} />
          <Route path="/runs/:id" element={<RunDetail />} />
          <Route path="/logs" element={<Logs />} />
          <Route path="/settings" element={<Settings />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </main>
    </div>
    </EventsProvider>
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
