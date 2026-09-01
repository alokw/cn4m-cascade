package api

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/alokw/cn4m-cascade/internal/store"
)

// sessionCookie is the cookie the SPA carries.
const sessionCookie = "smbsync_session"

// loginAttemptWindow and maxLoginAttempts rate-limit password guessing.
// SPEC.md §8 asks for rate limiting on the hook endpoints; the login form is
// the one place a password can be guessed at all, so it gets the same
// treatment.
const (
	loginAttemptWindow = 15 * time.Minute
	maxLoginAttempts   = 10
)

// loginLimiter counts recent failures per client address.
//
// In memory rather than in the database: it protects against online guessing,
// which only matters while the process is up, and a restart clearing it is not
// a weakness worth a write per attempt.
type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{attempts: map[string][]time.Time{}}
}

// blocked reports whether an address has failed too often lately.
func (l *loginLimiter) blocked(addr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := time.Now().Add(-loginAttemptWindow)
	kept := l.attempts[addr][:0]
	for _, t := range l.attempts[addr] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	l.attempts[addr] = kept
	return len(kept) >= maxLoginAttempts
}

// record notes a failed attempt.
func (l *loginLimiter) record(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.attempts[addr] = append(l.attempts[addr], time.Now())
}

// clear forgets an address's failures after a successful login.
func (l *loginLimiter) clear(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, addr)
}

// clientAddr identifies a caller for rate limiting. It deliberately ignores
// X-Forwarded-For: this service is reached directly (SPEC.md §3 host
// networking), so a forwarded header is attacker-controlled and would let a
// guesser reset their own budget at will.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// requireSession rejects unauthenticated requests. It is the middleware
// SPEC.md §8 asks for: everything under /api/ needs a session except the auth
// endpoints themselves.
func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.publicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		cookie, err := r.Cookie(sessionCookie)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.", "")
			return
		}
		ok, err := s.db.LookupSession(r.Context(), cookie.Value)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "session_check_failed",
				"Could not check your session.", err.Error())
			return
		}
		if !ok {
			// Clear the dead cookie so the browser stops sending it.
			http.SetCookie(w, s.sessionCookie(r, "", -1))
			writeError(w, http.StatusUnauthorized, "unauthenticated", "Your session has expired. Sign in again.", "")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// publicPath lists what is reachable without a session: the auth endpoints
// (you cannot log in through a gate that requires being logged in) and the
// liveness probe (a container health check has no cookie).
func (s *Server) publicPath(path string) bool {
	return path == "/healthz" || strings.HasPrefix(path, "/api/auth/")
}

// sessionCookie builds the cookie, set or cleared.
//
// Secure is set only for a request that arrived over TLS. This service is
// normally reached over plain HTTP on a LAN (SPEC.md §3: host networking, no
// TLS terminator), and an unconditional Secure flag would make the browser
// discard the cookie and login would fail with no visible reason.
func (s *Server) sessionCookie(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	}
}

type authRequest struct {
	Password string `json:"password"`
}

type sessionResponse struct {
	Authenticated bool `json:"authenticated"`
	// SetupRequired means no admin password exists yet, so the UI should
	// show first-run setup rather than a login form.
	SetupRequired bool `json:"setup_required"`
}

// handleSession reports whether the caller is signed in, and whether the
// instance has been set up at all. It is public: it is what the SPA asks
// before deciding which screen to show.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	set, err := s.db.AdminPasswordSet(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "setup_check_failed",
			"Could not read the server configuration.", err.Error())
		return
	}

	resp := sessionResponse{SetupRequired: !set}
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if ok, err := s.db.LookupSession(r.Context(), cookie.Value); err == nil {
			resp.Authenticated = ok
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleSetup sets the admin password on a fresh instance. It refuses once a
// password exists, so it can never be used to take over a configured server.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	set, err := s.db.AdminPasswordSet(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "setup_check_failed",
			"Could not read the server configuration.", err.Error())
		return
	}
	if set {
		writeError(w, http.StatusConflict, "already_set_up",
			"This server already has an admin password. Sign in instead.", "")
		return
	}

	var req authRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "The request body is not valid JSON.", err.Error())
		return
	}
	if err := s.db.SetAdminPassword(r.Context(), req.Password); err != nil {
		writeError(w, http.StatusBadRequest, "weak_password", err.Error(), "")
		return
	}

	s.log.Info("admin password set through first-run setup")
	s.issueSession(w, r)
}

// handleLogin exchanges the admin password for a session.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	addr := clientAddr(r)
	if s.logins.blocked(addr) {
		writeError(w, http.StatusTooManyRequests, "too_many_attempts",
			"Too many failed sign-in attempts. Wait a few minutes and try again.", "")
		return
	}

	var req authRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "The request body is not valid JSON.", err.Error())
		return
	}

	ok, err := s.db.VerifyAdminPassword(r.Context(), req.Password)
	if errors.Is(err, store.ErrNoAdminPassword) {
		writeError(w, http.StatusConflict, "setup_required",
			"This server has no admin password yet. Complete first-run setup.", "")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "login_failed", "Could not check the password.", err.Error())
		return
	}
	if !ok {
		s.logins.record(addr)
		// Deliberately vague, and identical whatever went wrong.
		writeError(w, http.StatusUnauthorized, "invalid_password", "That password is not correct.", "")
		return
	}

	s.logins.clear(addr)
	s.issueSession(w, r)
}

// issueSession creates a session and sets the cookie.
func (s *Server) issueSession(w http.ResponseWriter, r *http.Request) {
	token, expires, err := s.db.CreateSession(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_failed", "Could not start a session.", err.Error())
		return
	}
	http.SetCookie(w, s.sessionCookie(r, token, int(time.Until(expires).Seconds())))
	writeJSON(w, http.StatusOK, sessionResponse{Authenticated: true})
}

// handleLogout ends the caller's session.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if err := s.db.DeleteSession(r.Context(), cookie.Value); err != nil {
			s.log.Warn("could not delete a session", "error", err)
		}
	}
	http.SetCookie(w, s.sessionCookie(r, "", -1))
	writeJSON(w, http.StatusOK, sessionResponse{Authenticated: false})
}
