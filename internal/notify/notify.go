// Package notify delivers outbound status callbacks (SPEC.md §8.2).
//
// The single rule this package exists to honour: **delivery must never affect
// the sync**. A callback URL that is down, slow, or accepts a connection and
// then says nothing forever is somebody else's server behaving badly, and a
// backup must not be delayed, failed, or held open by it. Everything here —
// the buffered queue, the bounded client, the dropped-on-full policy — follows
// from that and not from any desire to be clever.
//
// This is the same discipline CLAUDE.md applies to blocking I/O against a
// share, pointed at a socket instead: nothing may hang forever because
// something on the other end stopped answering.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/alokw/cn4m-cascade/internal/store"
)

const (
	// deliveryTimeout bounds one attempt end to end, including connect, write
	// and read. A receiver that accepts the connection and then goes quiet is
	// the case this exists for; without it that goroutine waits forever.
	deliveryTimeout = 10 * time.Second
	// maxAttempts and the backoff give a receiver that is restarting a fair
	// chance without turning one event into an unbounded retry storm.
	maxAttempts = 3
	// queueDepth is how many pending deliveries **per webhook** are held
	// before new ones are dropped. Dropping is deliberate: the alternative is
	// an unbounded queue that turns a dead receiver into memory growth for the
	// whole of a long run, and a notification is not worth that.
	queueDepth = 256
)

var backoff = [maxAttempts]time.Duration{0, 2 * time.Second, 8 * time.Second}

// Circuit-breaker bounds for best-effort endpoints, matching the pattern
// cn4m's own reference client uses: back off from a minute, doubling per
// consecutive failure, up to half an hour.
//
// Applied to best-effort hooks only. A JSON webhook's failures are reported
// against the run, so someone can see and fix them; suppressing those would
// hide something the user asked to be told about. A cn4m that is not running
// is a normal state, and hammering it three times an event for the life of the
// process is work that helps nobody.
const (
	outageBackoffStart = time.Minute
	outageBackoffMax   = 30 * time.Minute
)

// Event names, as SPEC.md §8.2 defines them.
const (
	EventRunStarted   = "run_started"
	EventProgress     = "progress"
	EventPrompt       = "target_unavailable_prompt"
	EventRunCompleted = "run_completed"
	EventRunFailed    = "run_failed"
)

// Payload is one callback body: the polling status shape plus `event`.
//
// A type *alias* rather than a named type, so this satisfies the runner's
// Notifier interface without the runner having to import this package. Go
// requires method signatures to match exactly, and a named map type would not
// match `map[string]any` — which would force the dependency the interface
// exists to avoid.
type Payload = map[string]any

// SecretLookup turns a webhook's stored ciphertext into its signing key.
//
// Injected rather than held, so this package never sees the encryption key —
// the same arrangement mountmgr uses for target passwords.
type SecretLookup func(encrypted string) (string, error)

// Notifier queues and delivers callbacks.
type Notifier struct {
	db      *store.DB
	log     *slog.Logger
	client  *http.Client
	decrypt SecretLookup

	wg   sync.WaitGroup
	once sync.Once
	stop chan struct{}
	// ctx is the lifetime handed to Start; lanes created afterwards inherit it.
	ctx context.Context

	mu sync.Mutex
	// lanes holds one delivery queue per webhook, each drained by its own
	// goroutine, so a hook's events go out **in the order they were queued**.
	//
	// That ordering is load-bearing since cn4m's `progress` level (2026-09-12):
	// a progress post is a per-app slot that the app's next non-progress post
	// clears. With a shared pool, a progress request and the run's outcome can
	// be in flight on two workers at once, and if the progress one lands
	// second it re-fills the slot *after* the outcome cleared it — "97%" then
	// sits on the rail for two minutes after the sync finished, and the
	// finished sync itself never displaced it. Per-hook lanes make that
	// impossible; different hooks still deliver in parallel, and a slow
	// receiver only ever holds up its own lane.
	//
	// Created on first use rather than at Start, because hooks come and go at
	// runtime and are few — one goroutine each is nothing.
	lanes   map[string]chan job
	stopped bool
	// lastProgress throttles the progress event per (webhook, run).
	lastProgress map[string]time.Time
	// down tracks best-effort endpoints that are not answering, so a receiver
	// that is simply absent is left alone rather than retried on every event.
	down map[string]*outage
}

// outage is a best-effort endpoint's consecutive-failure state.
type outage struct {
	failures int
	retryAt  time.Time
}

type job struct {
	hook    store.Webhook
	event   string
	runID   string
	payload Payload
}

// New builds a Notifier. Call Start before use.
func New(db *store.DB, decrypt SecretLookup, log *slog.Logger) *Notifier {
	return &Notifier{
		db:      db,
		decrypt: decrypt,
		log:     log.With("component", "notify"),
		client: &http.Client{
			Timeout: deliveryTimeout,
			// Redirects are not followed. A URL that was reviewed when it was
			// configured can start pointing somewhere else entirely, and the
			// signed body would go with it — this is the one hop where a
			// receiver could redirect a credentialed request elsewhere.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		lanes:        map[string]chan job{},
		stop:         make(chan struct{}),
		lastProgress: map[string]time.Time{},
		down:         map[string]*outage{},
	}
}

// Start records the lifetime deliveries run under. The lanes themselves are
// spawned as hooks first appear.
func (n *Notifier) Start(ctx context.Context) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ctx = ctx
}

// Stop drains what is already queued, bounded by ctx.
//
// Actually drains: the workers finish the backlog before returning, so the
// run_completed callbacks produced while the server was shutting down are the
// ones most likely to be delivered rather than the ones most likely to be
// lost. A caller that does not want to wait passes a short context.
func (n *Notifier) Stop(ctx context.Context) error {
	n.once.Do(func() {
		// Marked under the lock before the lanes are told, so no lane can be
		// created (and a WaitGroup incremented) once Wait has begun.
		n.mu.Lock()
		n.stopped = true
		n.mu.Unlock()
		close(n.stop)
	})

	done := make(chan struct{})
	go func() {
		n.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Notify queues a callback for every webhook subscribed to this event.
//
// **Never blocks and never returns an error.** It is called from the run
// pipeline, and a delivery problem is not a sync problem: the worst outcome
// here is a log line and a run_event, never a slower or failed run.
func (n *Notifier) Notify(ctx context.Context, jobID, runID, event string, payload Payload) {
	hooks, err := n.db.WebhooksFor(ctx, jobID)
	if err != nil {
		n.log.Warn("could not load webhooks; no callbacks sent for this event",
			"job_id", jobID, "event", event, "error", err)
		return
	}

	for _, hook := range hooks {
		if !hook.Subscribes(event) {
			continue
		}
		if event == EventProgress && !n.progressDue(hook, runID) {
			continue
		}
		// Checked before queueing, not before sending: a doomed delivery
		// should not occupy a queue slot that a working receiver could use.
		if hook.BestEffort() && n.backedOff(hook) {
			continue
		}

		body := Payload{}
		for k, v := range payload {
			body[k] = v
		}
		body["event"] = event

		if !n.enqueue(job{hook: hook, event: event, runID: runID, payload: body}) {
			// Full means a receiver is not keeping up. Drop rather than block:
			// blocking here would push a stranger's outage into the sync
			// pipeline, which is the one thing this package must not do.
			n.log.Warn("callback queue is full; dropped an event",
				"url", hook.URL, "event", event, "run_id", runID)
		}
	}
}

// enqueue places a job on its webhook's lane, starting the lane if this is
// the hook's first event. Reports false when the lane is full, or when the
// notifier has been stopped — Notify's producers are meant to be gone by then,
// and a late event is dropped rather than starting a goroutine nothing will
// wait for.
func (n *Notifier) enqueue(j job) bool {
	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return false
	}
	lane, ok := n.lanes[j.hook.ID]
	if !ok {
		lane = make(chan job, queueDepth)
		n.lanes[j.hook.ID] = lane
		n.wg.Add(1)
		go n.worker(n.ctx, lane)
	}
	n.mu.Unlock()

	select {
	case lane <- j:
		return true
	default:
		return false
	}
}

// progressDue enforces each webhook's min_interval_sec for progress events,
// which would otherwise fire once a second for the length of a run.
func (n *Notifier) progressDue(hook store.Webhook, runID string) bool {
	key := hook.ID + "\x00" + runID

	n.mu.Lock()
	defer n.mu.Unlock()

	interval := time.Duration(hook.MinIntervalSec) * time.Second
	if last, ok := n.lastProgress[key]; ok && time.Since(last) < interval {
		return false
	}
	n.lastProgress[key] = time.Now()
	return true
}

// backedOff reports whether this endpoint is inside its retry window.
func (n *Notifier) backedOff(hook store.Webhook) bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	o, ok := n.down[hook.ID]
	return ok && time.Now().Before(o.retryAt)
}

// noteOutcome records whether an endpoint answered, extending or clearing its
// backoff.
func (n *Notifier) noteOutcome(hook store.Webhook, ok bool) {
	if !hook.BestEffort() {
		return
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	if ok {
		// One success ends the outage outright. A receiver that has come back
		// should not spend the next half hour being ignored.
		delete(n.down, hook.ID)
		return
	}

	o := n.down[hook.ID]
	if o == nil {
		o = &outage{}
		n.down[hook.ID] = o
	}
	o.failures++

	wait := outageBackoffStart << min(o.failures-1, 8)
	if wait > outageBackoffMax {
		wait = outageBackoffMax
	}
	o.retryAt = time.Now().Add(wait)
}

// Forget drops a finished run's throttle state, so the map does not grow for
// the life of the process.
func (n *Notifier) Forget(runID string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for key := range n.lastProgress {
		if len(key) > len(runID) && key[len(key)-len(runID):] == runID {
			delete(n.lastProgress, key)
		}
	}
}

// worker drains one webhook's lane, one delivery at a time and in order.
func (n *Notifier) worker(ctx context.Context, lane <-chan job) {
	defer n.wg.Done()
	for {
		// The lane is checked on its own first. A plain three-way select
		// picks at random among ready cases, so a worker would abandon a full
		// backlog the instant `stop` closed — which is exactly when the most
		// interesting events (run_completed for the runs being cancelled) are
		// being queued.
		select {
		case j := <-lane:
			n.deliver(ctx, j)
			continue
		default:
		}

		select {
		case <-ctx.Done():
			return
		case <-n.stop:
			// Stopping: deliver whatever is already queued, then finish. New
			// work is not accepted because Notify's producers are gone by now.
			for {
				select {
				case j := <-lane:
					n.deliver(ctx, j)
				default:
					return
				}
			}
		case j := <-lane:
			n.deliver(ctx, j)
		}
	}
}

// deliver POSTs one payload, retrying a few times.
func (n *Notifier) deliver(ctx context.Context, j job) {
	body, contentType, err := bodyFor(j.hook, j.event, j.payload)
	if err != nil {
		n.log.Error("could not encode a callback payload", "url", j.hook.URL, "error", err)
		return
	}

	var lastErr error
	for attempt := range maxAttempts {
		if wait := backoff[attempt]; wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return
			case <-n.stop:
				// Give up the *backoff*, not the attempt: on shutdown a
				// pending retry should not hold the drain open for eight
				// seconds. The first attempt has already been made.
				return
			}
		}

		status, err := n.post(ctx, j.hook, body, contentType)
		if err == nil && status >= 200 && status < 300 {
			n.noteOutcome(j.hook, true)
			return
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("the receiver answered %d", status)
			// A 4xx other than 408/429 will not improve on retry: the request
			// is wrong, not the moment.
			if status >= 400 && status < 500 && status != http.StatusRequestTimeout &&
				status != http.StatusTooManyRequests {
				break
			}
		}
	}

	// A best-effort callback's failure is not the run's business. cn4m is
	// optional infrastructure that is legitimately absent — its own client
	// says so — and a seeded callback that wrote a warning against every run
	// on a machine without cn4m would teach people to ignore run warnings,
	// which is a worse outcome than never noticing cn4m is down.
	if j.hook.BestEffort() {
		n.noteOutcome(j.hook, false)
		n.log.Debug("best-effort callback not delivered", "url", j.hook.URL,
			"event", j.event, "error", lastErr)
		return
	}

	// Recorded against the run so it is visible where the run's other trouble
	// is, at warn rather than error: the sync itself was fine.
	n.log.Warn("callback delivery failed", "url", j.hook.URL, "event", j.event,
		"run_id", j.runID, "error", lastErr)
	// n.db is nil in unit tests that exercise delivery in isolation. CLAUDE.md
	// forbids panics in library code, and "only reachable when three attempts
	// exhaust before Stop" is a timing argument, not a structural one.
	if j.runID != "" && n.db != nil {
		if err := n.db.AppendEvent(context.WithoutCancel(ctx), &store.RunEvent{
			RunID: j.runID, Level: store.LevelWarn,
			Message: fmt.Sprintf("could not deliver the %s callback to %s: %v",
				j.event, j.hook.URL, lastErr),
		}); err != nil {
			n.log.Warn("could not record a failed callback", "error", err)
		}
	}
}

// post performs one attempt, signing the body.
func (n *Notifier) post(ctx context.Context, hook store.Webhook, body []byte, contentType string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hook.URL, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("building the request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "cn4m-cascade")
	if hook.SecretEncrypted != "" {
		secret, err := n.decrypt(hook.SecretEncrypted)
		if err != nil {
			// Refuse to send rather than send unsigned. A receiver that checks
			// signatures would reject it anyway; one that does not would
			// accept an unauthenticated POST it believes is authenticated,
			// which is worse than no delivery.
			return 0, fmt.Errorf("the signing secret could not be read: %w", err)
		}
		req.Header.Set("X-Signature", Sign(secret, body))
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// jsonBody encodes the generic webhook payload.
func jsonBody(payload Payload) ([]byte, error) {
	return json.Marshal(payload)
}

// Sign returns the HMAC-SHA256 of the exact body, hex encoded.
//
// Exported so tests verify the signature the way a receiver would, rather than
// by re-deriving it from the same code that produced it — which would pass
// even if both sides were wrong together.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
