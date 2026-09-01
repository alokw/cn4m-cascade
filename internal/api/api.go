// Package api serves the HTTP interface. Phase 1 exposes target CRUD plus
// the connection test; the rest of SPEC.md §8 arrives with its own phase.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
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
}

// NewServer wires up the API.
func NewServer(db *store.DB, provider *storage.Provider, mounts *mountmgr.Manager, healthc *health.Cache, box *secrets.Box, runs *runner.Runner, log *slog.Logger) *Server {
	return &Server{db: db, provider: provider, mounts: mounts, healthc: healthc, box: box, runner: runs, log: log}
}

// Handler returns the routed, middleware-wrapped handler.
//
// Session authentication (SPEC.md §8) is deliberately absent in Phase 1 and
// lands with the UI in Phase 4 (PROGRESS.md D-4); the middleware seam is
// this function.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleLiveness)

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
	mux.HandleFunc("POST /api/jobs/{id}/run", s.handleRunJob)
	mux.HandleFunc("POST /api/jobs/{id}/filter-test", s.handleFilterTest)

	mux.HandleFunc("GET /api/runs", s.handleListRuns)
	mux.HandleFunc("GET /api/runs/{id}", s.handleGetRun)
	mux.HandleFunc("GET /api/runs/{id}/events", s.handleRunEvents)
	mux.HandleFunc("POST /api/runs/{id}/cancel", s.handleCancelRun)

	return s.withLogging(mux)
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
