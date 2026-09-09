// Package api serves the HTTP interface. Phase 1 exposes target CRUD plus
// the connection test; the rest of SPEC.md §8 arrives with its own phase.
package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/alokw/cn4m-cascade/internal/health"
	"github.com/alokw/cn4m-cascade/internal/mountmgr"
	"github.com/alokw/cn4m-cascade/internal/runner"
	"github.com/alokw/cn4m-cascade/internal/secrets"
	"github.com/alokw/cn4m-cascade/internal/storage"
	"github.com/alokw/cn4m-cascade/internal/store"
)

// maxBodyBytes caps request bodies; target definitions are small.
const maxBodyBytes = 1 << 20

// Server holds the handler dependencies.
type Server struct {
	db       *store.DB
	provider *storage.Provider
	mounts   *mountmgr.Manager
	healthc  *health.Cache
	box      *secrets.Box
	runner   *runner.Runner
	log      *slog.Logger

	logins *loginLimiter
	// hooks rate-limits the token endpoints, which are the only routes in the
	// application an anonymous caller can reach at all.
	hooks *hookLimiter
	hub   *hub
}

// NewServer wires up the API.
func NewServer(db *store.DB, provider *storage.Provider, mounts *mountmgr.Manager, healthc *health.Cache, box *secrets.Box, runs *runner.Runner, log *slog.Logger) *Server {
	return &Server{
		db: db, provider: provider, mounts: mounts, healthc: healthc,
		box: box, runner: runs, log: log,
		logins: newLoginLimiter(),
		hooks:  newHookLimiter(),
		hub:    newHub(runs, db, log),
	}
}

// Start begins the background work the API owns: the WebSocket broadcast loop
// and the expired-session sweep.
func (s *Server) Start(ctx context.Context) {
	s.hub.start(ctx)

	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if n, err := s.db.PurgeExpiredSessions(ctx); err != nil {
					s.log.Warn("could not purge expired sessions", "error", err)
				} else if n > 0 {
					s.log.Info("purged expired sessions", "count", n)
				}
			}
		}
	}()
}

// Stop releases what Start acquired.
func (s *Server) Stop() { s.hub.stop() }

// Handler returns the routed, middleware-wrapped handler.
//
// The session guard (SPEC.md §8) covers exactly one thing: `/api/`. It is
// mounted on that prefix rather than wrapped around everything, because the
// SPA shell has to be reachable without a session — the login screen *is* the
// SPA, so a browser with no cookie must still be served index.html and its
// assets. Scoping the guard by mount point makes the static files public by
// construction; the alternative, adding them to publicPath, makes "is this
// public?" a question about a list someone has to remember to update.
//
// The data stays behind the guard either way. TestStaticShellIsPublicButAPIIsNot
// pins both directions.
//
// The webhook endpoints of §8, which use bearer tokens rather than sessions,
// arrive in Phase 5 and will mount alongside rather than extend this.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/targets", s.handleCreateTarget)
	mux.HandleFunc("GET /api/targets", s.handleListTargets)
	mux.HandleFunc("GET /api/targets/{id}", s.handleGetTarget)
	mux.HandleFunc("PATCH /api/targets/{id}", s.handleUpdateTarget)
	mux.HandleFunc("DELETE /api/targets/{id}", s.handleDeleteTarget)
	mux.HandleFunc("POST /api/targets/{id}/test", s.handleTestTarget)

	mux.HandleFunc("POST /api/jobs", s.handleCreateJob)
	mux.HandleFunc("GET /api/jobs", s.handleListJobs)
	mux.HandleFunc("GET /api/jobs/{id}", s.handleGetJob)
	mux.HandleFunc("DELETE /api/jobs/{id}", s.handleDeleteJob)
	mux.HandleFunc("PATCH /api/jobs/{id}", s.handleUpdateJob)
	mux.HandleFunc("POST /api/jobs/{id}/run", s.handleRunJob)
	mux.HandleFunc("POST /api/jobs/{id}/confirm", s.handleConfirmJob)
	mux.HandleFunc("POST /api/jobs/{id}/filter-test", s.handleFilterTest)

	mux.HandleFunc("GET /api/runs", s.handleListRuns)
	mux.HandleFunc("GET /api/runs/{id}", s.handleGetRun)
	mux.HandleFunc("GET /api/runs/{id}/events", s.handleRunEvents)
	mux.HandleFunc("GET /api/runs/{id}/plan", s.handleRunPlan)
	mux.HandleFunc("POST /api/runs/{id}/cancel", s.handleCancelRun)
	mux.HandleFunc("POST /api/runs/{id}/prompt", s.handlePrompt)

	mux.HandleFunc("POST /api/filters/check-file", s.handleCheckFilterFile)
	mux.HandleFunc("POST /api/schedule/preview", s.handleSchedulePreview)

	// Token management is session-guarded: issuing a trigger token is an
	// administrative act, and doing it with a trigger token would let a
	// compromised integration mint fresh credentials for itself.
	mux.HandleFunc("POST /api/jobs/{id}/token", s.handleIssueToken)
	mux.HandleFunc("DELETE /api/jobs/{id}/token", s.handleRevokeToken)

	mux.HandleFunc("GET /api/webhooks", s.handleListWebhooks)
	mux.HandleFunc("POST /api/webhooks", s.handleCreateWebhook)
	mux.HandleFunc("PATCH /api/webhooks/{id}", s.handleUpdateWebhook)
	mux.HandleFunc("DELETE /api/webhooks/{id}", s.handleDeleteWebhook)

	mux.HandleFunc("GET /api/settings/filters", s.handleGetGlobalFilters)
	mux.HandleFunc("PUT /api/settings/filters", s.handleReplaceGlobalFilters)

	mux.HandleFunc("GET /api/logs", s.handleLogs)
	mux.HandleFunc("GET /api/browse", s.handleBrowse)

	mux.HandleFunc("POST /api/auth/setup", s.handleSetup)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	mux.HandleFunc("GET /api/auth/session", s.handleSession)

	mux.HandleFunc("GET /api/ws", s.handleWS)

	// The webhook endpoints, mounted OUTSIDE the session guard (SPEC.md §8.1).
	//
	// This is the only part of the application an anonymous caller can reach,
	// and the fact is invisible in the code: it works because Go 1.22's
	// ServeMux gives the longest matching pattern priority, so "/api/hooks/"
	// beats the "/api/" registration below it. Nothing here *says* "this
	// bypasses authentication" except this comment — which is exactly why
	// TestHooksAreReachableWithoutASessionButNothingElseIs asserts both
	// directions rather than trusting the routing to stay this way.
	//
	// These handlers authenticate themselves, per job, with a bearer token.
	hooks := http.NewServeMux()
	hooks.HandleFunc("POST /api/hooks/jobs/{id}/run", s.handleHookRun)
	hooks.HandleFunc("GET /api/hooks/jobs/{id}/status", s.handleHookJobStatus)
	hooks.HandleFunc("GET /api/hooks/runs/{run_id}/status", s.handleHookRunStatus)

	root := http.NewServeMux()
	root.Handle("/api/hooks/", hooks)
	root.Handle("/api/", s.requireSession(mux))
	root.HandleFunc("GET /healthz", s.handleLiveness)
	root.Handle("/", s.spaHandler())

	return s.withLogging(root)
}

// withLogging records one line per request. It never logs request bodies,
// which carry passwords.
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

		next.ServeHTTP(rec, r)

		s.log.Info("http request",
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration_ms", time.Since(started).Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Hijack forwards to the underlying writer so a WebSocket upgrade can take the
// connection. Without it the wrapper silently hides the Hijacker interface and
// the handshake fails with 501 — a middleware breaking a protocol two layers
// away, with nothing in the logs to say so.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("this connection cannot be hijacked")
	}
	return hj.Hijack()
}

// Flush forwards to the underlying writer, so streaming responses are not held
// back by the wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) handleLiveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// errorBody is the single error envelope for every API failure.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Detail  string `json:"detail,omitempty"`
		Kind    string `json:"kind,omitempty"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message, detail string) {
	var body errorBody
	body.Error.Code = code
	body.Error.Message = message
	body.Error.Detail = detail
	writeJSON(w, status, body)
}

// writeMountError renders a mount failure with the classification attached,
// so the UI can distinguish "wrong password" from "host is down".
func writeMountError(w http.ResponseWriter, err error) {
	var me *mountmgr.MountError
	if errors.As(err, &me) {
		status := http.StatusBadGateway
		if me.Kind == mountmgr.KindAuth {
			status = http.StatusUnauthorized
		}
		var body errorBody
		body.Error.Code = "mount_failed"
		body.Error.Message = err.Error()
		body.Error.Detail = me.Detail
		body.Error.Kind = string(me.Kind)
		writeJSON(w, status, body)
		return
	}
	writeError(w, http.StatusBadGateway, "target_unavailable", err.Error(), "")
}

// decodeJSON reads a request body strictly, so a misspelled field is an
// error the user sees rather than a setting silently ignored.
func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return nil
}
