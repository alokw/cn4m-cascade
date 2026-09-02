import { createContext, useContext, useEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { api } from '../api/client'
import type { Run, RunSnapshot, WsEvent } from '../api/types'
import { isTerminal } from '../api/types'

export interface LiveRun {
  run: Run
  progress?: RunSnapshot
}

export type FeedStatus = 'connecting' | 'live' | 'polling'

interface Feed {
  /** Live runs keyed by run id. */
  runs: Map<string, LiveRun>
  status: FeedStatus
}

const POLL_MS = 2000
const RECONNECT_MIN_MS = 1000
const RECONNECT_MAX_MS = 15000
/** Cap on remembered runs, so a long-lived tab does not grow without bound. */
const MAX_TRACKED = 100

/**
 * One WebSocket for the whole app, with a polling fallback (SPEC.md §9:
 * "polling fallback if WS drops").
 *
 * The server drops a client that does not keep up — an 8-frame buffer, then
 * the socket is closed (internal/api/hub.go). So a close is an ordinary event
 * to recover from, not an error worth showing anyone. While the socket is down
 * we poll, which is why `status` distinguishes 'polling' from 'live'.
 *
 * The polling path deliberately fetches each live run *individually* as well as
 * the list. GET /api/runs returns bare store.Run records; `progress` exists
 * only on GET /api/runs/{id} (internal/api/runs.go). Polling the list alone
 * would advance the DB-flushed counters while phase, throughput, ETA, in-flight
 * files and — the one that matters — a prompt's countdown deadline all sat
 * frozen at whatever the last WS frame said.
 */
/**
 * The feed itself. Call this once, in EventsProvider — every call opens its
 * own WebSocket, and three components each wanting live data would mean three
 * sockets, three copies of the state, and three times the chance of tripping
 * the hub's slow-client drop.
 */
function useFeed(): Feed {
  const [runs, setRuns] = useState<Map<string, LiveRun>>(new Map())
  const [status, setStatus] = useState<FeedStatus>('connecting')

  // Refs the effect owns; see the cleanup for why the cancellation flag is a
  // local rather than a ref.
  const socketRef = useRef<WebSocket | null>(null)
  const pollRef = useRef<number | null>(null)
  const retryRef = useRef<number | null>(null)
  const backoffRef = useRef(RECONNECT_MIN_MS)

  useEffect(() => {
    // Local to this effect run, not a component-lifetime ref. Under
    // StrictMode the effect mounts, cleans up and mounts again; a shared ref
    // gets flipped back to false by the second mount, so the *first* socket's
    // late onclose would then treat itself as live, clobber socketRef and open
    // a third connection that nothing can ever close.
    let cancelled = false

    const stopPolling = () => {
      if (pollRef.current !== null) {
        window.clearInterval(pollRef.current)
        pollRef.current = null
      }
    }

    const merge = (run: Run, progress?: RunSnapshot) =>
      setRuns((prev) => {
        const next = new Map(prev)
        const previous = next.get(run.id)
        // run_finished carries no progress — the runner handle is gone by
        // then. Keep the last snapshot so a finished run still shows its final
        // numbers instead of blanking out.
        next.set(run.id, { run, progress: progress ?? previous?.progress })

        if (next.size > MAX_TRACKED) {
          const stale = [...next.values()]
            .filter((r) => isTerminal(r.run.status))
            .sort((a, b) => a.run.started_at.localeCompare(b.run.started_at))
          for (const r of stale) {
            if (next.size <= MAX_TRACKED) break
            next.delete(r.run.id)
          }
        }
        return next
      })

    const poll = async () => {
      try {
        const list = await api.runs.list({ limit: 50 })
        if (cancelled) return
        for (const run of list) merge(run)

        // Only live runs have progress worth fetching, and only those cost a
        // request per tick.
        const live = list.filter((r) => !isTerminal(r.status))
        const details = await Promise.allSettled(live.map((r) => api.runs.get(r.id)))
        if (cancelled) return
        for (const d of details) {
          if (d.status === 'fulfilled') merge(d.value, d.value.progress)
        }
      } catch {
        // Keep polling; one failed tick must not kill the fallback.
      }
    }

    const startPolling = () => {
      if (pollRef.current !== null || cancelled) return
      setStatus('polling')
      void poll()
      pollRef.current = window.setInterval(() => void poll(), POLL_MS)
    }

    const connect = () => {
      if (cancelled) return

      const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
      const socket = new WebSocket(`${proto}//${window.location.host}/api/ws`)
      socketRef.current = socket

      socket.onopen = () => {
        if (cancelled) return
        backoffRef.current = RECONNECT_MIN_MS
        stopPolling()
        setStatus('live')
      }

      socket.onmessage = (ev) => {
        if (cancelled) return
        let msg: WsEvent
        try {
          msg = JSON.parse(ev.data as string) as WsEvent
        } catch {
          return
        }
        if (msg.run) merge(msg.run, msg.progress)
      }

      socket.onclose = () => {
        if (socketRef.current === socket) socketRef.current = null
        if (cancelled) return
        startPolling()
        const wait = backoffRef.current
        backoffRef.current = Math.min(wait * 2, RECONNECT_MAX_MS)
        retryRef.current = window.setTimeout(connect, wait)
      }

      socket.onerror = () => socket.close()
    }

    connect()

    return () => {
      cancelled = true
      stopPolling()
      if (retryRef.current !== null) {
        window.clearTimeout(retryRef.current)
        retryRef.current = null
      }
      const socket = socketRef.current
      socketRef.current = null
      if (socket) {
        // Detach first: a close handler that runs after teardown would
        // otherwise restart polling and schedule another connect.
        socket.onopen = null
        socket.onmessage = null
        socket.onclose = null
        socket.onerror = null
        socket.close()
      }
    }
  }, [])

  return { runs, status }
}

const FeedContext = createContext<Feed | null>(null)

/** Mounts the single feed for the app. */
export function EventsProvider({ children }: { children: ReactNode }) {
  const feed = useFeed()
  return <FeedContext.Provider value={feed}>{children}</FeedContext.Provider>
}

/** Reads the shared feed. Safe to call from as many components as you like. */
export function useEvents(): Feed {
  const ctx = useContext(FeedContext)
  if (!ctx) throw new Error('useEvents used outside EventsProvider')
  return ctx
}
