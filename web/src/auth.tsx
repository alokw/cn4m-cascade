import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import { api, setUnauthorizedHandler } from './api/client'

type AuthState =
  | { status: 'loading' }
  | { status: 'setup' }
  | { status: 'anonymous' }
  | { status: 'signed-in' }

interface AuthContextValue {
  state: AuthState
  signIn: (password: string) => Promise<void>
  completeSetup: (password: string) => Promise<void>
  signOut: () => Promise<void>
}

const AuthContext = createContext<AuthContextValue | null>(null)

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth used outside AuthProvider')
  return ctx
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<AuthState>({ status: 'loading' })

  const refresh = useCallback(async () => {
    try {
      const s = await api.session.get()
      if (s.setup_required) setState({ status: 'setup' })
      else if (s.authenticated) setState({ status: 'signed-in' })
      else setState({ status: 'anonymous' })
    } catch {
      // The session endpoint is public, so a failure here means the server is
      // unreachable rather than that we are signed out. Show the login screen;
      // the attempt will surface the real error.
      setState({ status: 'anonymous' })
    }
  }, [])

  useEffect(() => {
    void refresh()
  }, [refresh])

  // Any 401 from anywhere in the app drops us to the login screen, so no
  // individual caller has to handle an expired session.
  useEffect(() => {
    setUnauthorizedHandler(() => setState({ status: 'anonymous' }))
    return () => setUnauthorizedHandler(() => {})
  }, [])

  const value = useMemo<AuthContextValue>(
    () => ({
      state,
      signIn: async (password) => {
        await api.session.login(password)
        setState({ status: 'signed-in' })
      },
      completeSetup: async (password) => {
        await api.session.setup(password)
        setState({ status: 'signed-in' })
      },
      signOut: async () => {
        try {
          await api.session.logout()
        } finally {
          setState({ status: 'anonymous' })
        }
      },
    }),
    [state],
  )

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}
