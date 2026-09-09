package api

import (
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/alokw/cn4m-cascade/internal/runner"
	"github.com/alokw/cn4m-cascade/internal/store"
)

// Rate limiting for the hook endpoints.
//
// These are the only routes in the application reachable without a session, so
// they are the only ones where an anonymous caller can guess indefinitely. A
// 32-byte token is not guessable by brute force in any practical sense, but a
// limiter costs nothing and turns "not practically guessable" into "not
// attemptable" — and it also caps the damage of a loop that hammers a status
// endpoint with a stale token.
const (
	hookAttemptWindow = 5 * time.Minute
	maxHookFailures   = 20
	// maxTrackedAddrs triggers a sweep of expired addresses. A ceiling on
	// bookkeeping, not on callers: legitimate ones never appear here at all,
	// because only failures are recorded.
	maxTrackedAddrs = 4096
)

// hookLimiter blocks an address that keeps presenting bad tokens.
//
// Deliberately keyed on *failures* only: a working integration polling its
// status endpoint every few seconds must never be throttled, and it never
// records anything here.
type hookLimiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
}

func newHookLimiter() *hookLimiter { return &hookLimiter{failures: map[string][]time.Time{}} }

func (l *hookLimiter) blocked(addr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	kept := l.prune(addr)
	return len(kept) >= maxHookFailures
}

// record notes a failed attempt, and sweeps expired addresses.
//
// The sweep is here rather than only in blocked() because pruning one address
// on its *next* request never reclaims an address that never returns. One bad
// request each from a walk of source addresses — trivial on an IPv6 /64, and
// this is the one endpoint designed to be reachable anonymously — would
// otherwise leave an entry per address forever. An earlier comment here
// claimed that leak had been avoided; it had not.
func (l *hookLimiter) record(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.failures[addr] = append(l.prune(addr), time.Now())

	if len(l.failures) > maxTrackedAddrs {
		l.sweep()
	}
}

// prune drops this address's expired attempts, deleting the key when none
// remain. Caller holds the lock.
func (l *hookLimiter) prune(addr string) []time.Time {
	cutoff := time.Now().Add(-hookAttemptWindow)
	kept := l.failures[addr][:0]
	for _, t := range l.failures[addr] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.failures, addr)
		return nil
	}
	l.failures[addr] = kept
	return kept
}

// sweep bounds the map. Caller holds the lock.
//
// Two passes, because expiry alone is not a bound: a burst of failures from
// many fresh addresses inside the window leaves every entry unexpired and the
// map arbitrarily large, which is precisely the flood this is meant to
// survive. So expired entries go first, and if that is not enough the oldest
// survivors are evicted down to the ceiling.
//
// Evicting a live entry does forget someone's failures early. That is the
// right trade: the ceiling exists to stop an anonymous caller exhausting
// memory, and an attacker still failing is one whose entry is among the
// newest and therefore evicted last.
func (l *hookLimiter) sweep() {
	cutoff := time.Now().Add(-hookAttemptWindow)
	for addr, times := range l.failures {
		if len(times) == 0 || !times[len(times)-1].After(cutoff) {
			delete(l.failures, addr)
		}
	}
	if len(l.failures) <= maxTrackedAddrs {
		return
	}

	type entry struct {
		addr string
		last time.Time
	}
	all := make([]entry, 0, len(l.failures))
	for addr, times := range l.failures {
		all = append(all, entry{addr: addr, last: times[len(times)-1]})
	}
	slices.SortFunc(all, func(a, b entry) int { return a.last.Compare(b.last) })

	for _, e := range all[:len(all)-maxTrackedAddrs] {
		delete(l.failures, e.addr)
	}
}

// hookToken pulls the bearer token off a request.
//
// Both forms are accepted. `Authorization: Bearer` is what the documentation
// and the curl examples use, because a credential in a query string ends up in
// proxy logs, browser history and Referer headers. `?token=` is accepted
// anyway because SPEC.md §8.1 specifies it and integrations expect it — a
// webhook you cannot paste into a URL field is a webhook people work around.
func hookToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if after, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(after)
		}
	}
	return r.URL.Query().Get("token")
}

// authorizeHook resolves the token on a request to the job it authorises.
//
// Every failure looks identical from outside: no token, an unknown token, a
// token for a job that has since been deleted, and a token that does not match
// the job named in the path all produce the same 401 with the same body. The
// distinctions are real and are worth logging, but telling an anonymous caller
// which of them applied is telling them whether a guess got closer.
func (s *Server) authorizeHook(w http.ResponseWriter, r *http.Request) (*store.Job, bool) {
	addr := clientAddr(r)

	// Note the ordering: the token is checked *first*, and the limiter only
	// decides what happens to a request that has already failed.
	//
	// Blocking before verifying would mean one client with a stale token
	// permanently 429s every other caller at the same address — and behind a
	// NAT, a reverse proxy or a Docker bridge, every integration shares one
	// address. Since a bad token still gets recorded and still gets blocked,
	// brute force is bounded exactly as before; what changes is that a working
	// integration is never collateral damage.
	deny := func(reason string) (*store.Job, bool) {
		s.hooks.record(addr)
		s.log.Warn("rejected a webhook call", "reason", reason, "addr", addr, "path", r.URL.Path)
		if s.hooks.blocked(addr) {
			writeError(w, http.StatusTooManyRequests, "rate_limited",
				"Too many failed attempts from this address. Try again shortly.", "")
			return nil, false
		}
		writeError(w, http.StatusUnauthorized, "unauthenticated",
			"A valid trigger token is required.", "")
		return nil, false
	}

	token := hookToken(r)
	if token == "" {
		return deny("no token presented")
	}
	job, err := s.db.JobForTriggerToken(r.Context(), token)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return deny("unknown token")
		}
		writeError(w, http.StatusInternalServerError, "auth_failed",
			"Could not check the trigger token.", err.Error())
		return nil, false
	}

	// A token is scoped to one job. If the path names a different one, this is
	// a valid credential being used somewhere it does not belong.
	if want := r.PathValue("id"); want != "" && want != job.ID {
		return deny("token does not authorise the job in the path")
	}
	return job, true
}

// hookRunRequest is the optional body of a trigger call (SPEC.md §8.1).
type hookRunRequest struct {
	Preview bool `json:"preview"`
	// UnavailablePolicyOverride replaces the job's setting for this run only.
	//
	// It exists because an unattended trigger is exactly the situation where a
	// job configured to *ask* about an unreachable destination should not:
	// nobody is there, so the run would sit until its prompt timed out. The
	// caller that knows it is a machine can say so.
	//
	// Not honoured for `prompt`, and not silently: see below.
	UnavailablePolicyOverride string `json:"unavailable_policy_override"`
}

// handleHookRun starts a run from an external trigger.
func (s *Server) handleHookRun(w http.ResponseWriter, r *http.Request) {
	job, ok := s.authorizeHook(w, r)
	if !ok {
		return
	}

	var req hookRunRequest
	if err := decodeJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_body",
			"The request body is not valid JSON for a run.", err.Error())
		return
	}

	if req.UnavailablePolicyOverride != "" {
		policy := store.UnavailablePolicy(req.UnavailablePolicyOverride)
		switch policy {
		case store.PolicySkip, store.PolicyAbort:
			// A copy, so the override lasts for this run and does not edit the
			// stored job — a webhook must not be able to change a job's
			// configuration by triggering it.
			adjusted := *job
			adjusted.UnavailablePolicy = policy
			job = &adjusted
		case store.PolicyPrompt:
			writeError(w, http.StatusBadRequest, "invalid_override",
				`"prompt" cannot be used as an override on a webhook run: there is nobody to answer it.`,
				"Use \"skip\" or \"abort\".")
			return
		default:
			writeError(w, http.StatusBadRequest, "invalid_override",
				`unavailable_policy_override must be "skip" or "abort".`,
				"Got: "+req.UnavailablePolicyOverride)
			return
		}
	}

	start := s.runner.StartWebhook
	if req.Preview {
		// A previewed webhook run parks for confirmation, which nobody is
		// there to give — so it self-cancels on its deadline. Allowed because
		// SPEC.md §8.1 offers it and "plan without executing" is a legitimate
		// thing to ask a machine for.
		start = s.runner.StartPreview
	}

	run, err := start(r.Context(), job)
	if errors.Is(err, runner.ErrAlreadyRunning) && r.URL.Query().Get("queue") == "1" {
		if s.runner.QueueNext(job.ID) {
			writeJSON(w, http.StatusAccepted, map[string]any{
				"status": "queued",
				"job_id": job.ID,
				"detail": "A run of this job is in progress; one run is queued to follow it. " +
					"A queued run is held in memory and is not restored if the server restarts.",
			})
			return
		}
		// QueueNext says no for two different reasons, and only one is a
		// conflict. If the run finished between start() failing and this
		// call — a window of microseconds, but one an eager poller will find —
		// there is nothing to queue behind, and answering "already in
		// progress" would be both wrong and a lost trigger. Try again instead.
		if !s.runner.Queued(job.ID) {
			run, err = start(r.Context(), job)
		}
	}
	if err != nil {
		if errors.Is(err, runner.ErrAlreadyRunning) {
			writeError(w, http.StatusConflict, "already_running",
				"A run of this job is already in progress.",
				"Pass ?queue=1 to queue one run to follow it.")
			return
		}
		if errors.Is(err, runner.ErrShuttingDown) {
			writeError(w, http.StatusServiceUnavailable, "shutting_down",
				"The server is shutting down and is not starting new runs.", "")
			return
		}
		writeError(w, http.StatusBadGateway, "run_failed", "Could not start the run.", err.Error())
		return
	}

	s.log.Info("run started by webhook", "run_id", run.ID, "job_id", job.ID, "name", job.Name)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"run_id":     run.ID,
		"status_url": "/api/hooks/runs/" + run.ID + "/status",
	})
}

// handleHookRunStatus serves one run's status to a poller.
func (s *Server) handleHookRunStatus(w http.ResponseWriter, r *http.Request) {
	job, ok := s.authorizeHook(w, r)
	if !ok {
		return
	}

	run, err := s.db.GetRun(r.Context(), r.PathValue("run_id"))
	if err != nil {
		s.writeRunError(w, err)
		return
	}
	// The token authorises one job, so it may only read that job's runs.
	// Without this a valid token could enumerate every run on the server by id.
	if run.JobID != job.ID {
		writeError(w, http.StatusNotFound, "not_found", "No such run for this job.", "")
		return
	}
	writeJSON(w, http.StatusOK, s.hookStatus(r, run, job))
}

// handleHookJobStatus serves the job's most recent run.
func (s *Server) handleHookJobStatus(w http.ResponseWriter, r *http.Request) {
	job, ok := s.authorizeHook(w, r)
	if !ok {
		return
	}

	runs, err := s.db.ListRuns(r.Context(), store.RunFilter{JobID: job.ID, Limit: 1})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed", "Could not read the job's runs.", err.Error())
		return
	}
	if len(runs) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"job_id": job.ID,
			"status": "never_run",
		})
		return
	}
	writeJSON(w, http.StatusOK, s.hookStatus(r, runs[0], job))
}

// hookStatus builds the polling payload, delegating the shape to the runner so
// that this and the outbound callbacks of §8.2 cannot drift apart.
func (s *Server) hookStatus(r *http.Request, run *store.Run, job *store.Job) map[string]any {
	var snap *runner.RunSnapshot
	if live, ok := s.runner.Progress(run.ID); ok {
		snap = &live
	}
	out := runner.StatusPayload(run, snap, func(id string) string { return s.targetName(r, id) })
	out["job"] = job.Name
	return out
}

// targetName resolves a target id to its name for the status payload, falling
// back to the id. A poller reading "nas-basement failed" is served far better
// than one reading a hex string, and a missing target must not fail the call.
func (s *Server) targetName(r *http.Request, id string) string {
	if t, err := s.db.GetTarget(r.Context(), id); err == nil {
		return t.Name
	}
	return id
}

// handleIssueToken mints a trigger token and returns it in the clear, once.
//
// Session-guarded, unlike everything else in this file: minting a credential
// is an administrative act. If a trigger token could mint tokens, a leaked one
// would be able to issue itself replacements faster than anyone could revoke
// them.
func (s *Server) handleIssueToken(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	if _, err := s.db.GetJob(r.Context(), jobID); err != nil {
		s.writeJobError(w, err)
		return
	}

	token, err := s.db.IssueTriggerToken(r.Context(), jobID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "token_failed",
			"Could not issue a trigger token.", err.Error())
		return
	}

	// The only time this value exists outside the caller's browser. Not
	// logged, here or anywhere: the whole design rests on the server keeping
	// no recoverable copy, and a log line would be a recoverable copy.
	s.log.Info("issued a trigger token", "job_id", jobID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"token": token,
		"note": "This is the only time this token is shown. " +
			"Store it now; if it is lost, regenerate it — which invalidates this one.",
	})
}

// handleRevokeToken removes a job's trigger token.
func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	if err := s.db.RevokeTriggerToken(r.Context(), r.PathValue("id")); err != nil {
		s.writeJobError(w, err)
		return
	}
	s.log.Info("revoked a trigger token", "job_id", r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}
