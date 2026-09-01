package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/alokw/cn4m-cascade/internal/runner"
	"github.com/alokw/cn4m-cascade/internal/store"
)

// broadcastInterval is how often live runs are pushed to connected clients.
// The same cadence as the database progress flush (SPEC.md §6.1.1).
const broadcastInterval = time.Second

// recentRunWindow is how many recent runs each tick considers. Large enough
// that a burst of short runs is not missed between ticks, small enough that
// the query stays trivial.
const recentRunWindow = 50

// clientBuffer is how many frames a slow client may fall behind before it is
// dropped. Small on purpose: this is a progress feed, so a client that cannot
// keep up wants the latest state, not a backlog of stale ones.
const clientBuffer = 8

// Event names on the wire (SPEC.md §8's WS event list).
const (
	eventRunProgress = "run_progress"
	eventRunFinished = "run_finished"
	eventPrompt      = "target_unavailable_prompt"
)

// wsEvent is one frame.
type wsEvent struct {
	Event string    `json:"event"`
	TS    time.Time `json:"ts"`

	RunID    string              `json:"run_id,omitempty"`
	JobID    string              `json:"job_id,omitempty"`
	Run      *store.Run          `json:"run,omitempty"`
	Progress *runner.RunSnapshot `json:"progress,omitempty"`
}

// client is one connected browser.
type client struct {
	send chan wsEvent
	done chan struct{}
	once sync.Once
}

// close stops a client exactly once, however many goroutines notice at the
// same moment.
func (c *client) close() {
	c.once.Do(func() { close(c.done) })
}

// hub fans run progress out to connected clients.
//
// The hub must never be able to slow a run down. It reads progress through
// the runner's existing snapshot method and never holds anything the runner
// needs; a client that cannot keep up is dropped rather than blocking the
// broadcast. SPEC.md §9 asks for a polling fallback, and GET /api/runs/{id}
// already serves live progress, so a dropped client degrades rather than
// breaks.
type hub struct {
	runs *runner.Runner
	db   *store.DB
	log  *slog.Logger

	mu      sync.Mutex
	clients map[*client]struct{}
	// seen remembers the last status broadcast for a run, so the hub emits
	// exactly one completion event per run and never repeats it.
	seen map[string]store.RunStatus

	// since is when the hub started. Runs that finished before it are
	// history and are never announced; runs that started after it are ours
	// to report even if they began and ended inside a single tick.
	since time.Time

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newHub(runs *runner.Runner, db *store.DB, log *slog.Logger) *hub {
	return &hub{
		runs: runs, db: db, log: log,
		clients: map[*client]struct{}{},
		seen:    map[string]store.RunStatus{},
		since:   time.Now().UTC(),
	}
}

// start begins the broadcast loop.
func (h *hub) start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	h.cancel = cancel

	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		ticker := time.NewTicker(broadcastInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.tick(ctx)
			}
		}
	}()
}

// stop ends the broadcast loop and disconnects every client.
func (h *hub) stop() {
	if h.cancel != nil {
		h.cancel()
	}
	h.wg.Wait()

	h.mu.Lock()
	for c := range h.clients {
		c.close()
	}
	h.clients = map[*client]struct{}{}
	h.mu.Unlock()
}

// tick pushes one round of updates.
//
// It reads the most recent runs rather than only the live ones. A short run
// can start and finish inside a single tick, and a feed that only watched
// live runs would never mention it at all — the dashboard would show a job
// that quietly never reported anything.
func (h *hub) tick(ctx context.Context) {
	if h.clientCount() == 0 {
		return
	}

	runs, err := h.db.ListRuns(ctx, store.RunFilter{Limit: recentRunWindow})
	if err != nil {
		h.log.Warn("could not list runs for the event feed", "error", err)
		return
	}

	for _, run := range runs {
		previous, known := h.previousStatus(run.ID)

		if run.Terminal() {
			// Announce a completion once: either because the hub watched
			// the run live, or because it began on our watch and finished
			// before we could see it.
			alreadyAnnounced := known && previous == run.Status
			ours := known || run.StartedAt.After(h.since)
			if ours && !alreadyAnnounced {
				h.setStatus(run.ID, run.Status)
				h.broadcast(wsEvent{
					Event: eventRunFinished, TS: time.Now().UTC(),
					RunID: run.ID, JobID: run.JobID, Run: run,
				})
			}
			continue
		}

		h.setStatus(run.ID, run.Status)
		ev := wsEvent{Event: eventRunProgress, TS: time.Now().UTC(), RunID: run.ID, JobID: run.JobID, Run: run}
		if snap, ok := h.runs.Progress(run.ID); ok {
			ev.Progress = &snap
			if awaitingPrompt(snap) {
				ev.Event = eventPrompt
			}
		}
		h.broadcast(ev)
	}

	h.forgetOld(runs)
}

// previousStatus reports the last status broadcast for a run.
func (h *hub) previousStatus(runID string) (store.RunStatus, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.seen[runID]
	return st, ok
}

func (h *hub) setStatus(runID string, status store.RunStatus) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seen[runID] = status
}

// forgetOld drops bookkeeping for runs that have fallen out of the window, so
// the map cannot grow without bound on a long-lived server.
func (h *hub) forgetOld(current []*store.Run) {
	inWindow := make(map[string]bool, len(current))
	for _, run := range current {
		inWindow[run.ID] = true
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for runID := range h.seen {
		if !inWindow[runID] {
			delete(h.seen, runID)
		}
	}
}

// awaitingPrompt reports whether any destination is waiting for an answer, so
// the frame can be labelled as the prompt the UI must show.
func awaitingPrompt(snap runner.RunSnapshot) bool {
	for _, d := range snap.Destinations {
		if d.Status == store.DestAwaitingPrompt {
			return true
		}
	}
	return false
}

func (h *hub) clientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// broadcast sends to every client, dropping any that cannot keep up.
func (h *hub) broadcast(ev wsEvent) {
	h.mu.Lock()
	clients := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()

	for _, c := range clients {
		select {
		case c.send <- ev:
		default:
			// The client is not draining. Dropping it is the whole point:
			// a stalled browser must never hold up a sync.
			h.log.Debug("dropping a websocket client that fell behind")
			c.close()
		}
	}
}

func (h *hub) add(c *client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *hub) remove(c *client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	c.close()
}

// handleWS upgrades a connection and streams events until it drops.
//
// It sits behind requireSession like every other /api route, so the session
// cookie authenticates the socket too.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The SPA is served by this same process, so a cross-origin
		// upgrade is never legitimate.
		OriginPatterns: []string{r.Host},
	})
	if err != nil {
		s.log.Debug("websocket upgrade failed", "error", err)
		return
	}

	c := &client{send: make(chan wsEvent, clientBuffer), done: make(chan struct{})}
	s.hub.add(c)
	defer s.hub.remove(c)
	defer func() { _ = conn.CloseNow() }()

	ctx := r.Context()

	// Reading serves two purposes: it processes the protocol's pings, and it
	// notices when the browser goes away. Nothing sent by a client is acted
	// on — this feed is one-way.
	go func() {
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				c.close()
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case ev := <-c.send:
			body, err := json.Marshal(ev)
			if err != nil {
				s.log.Warn("could not encode a websocket event", "error", err)
				continue
			}
			writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err = conn.Write(writeCtx, websocket.MessageText, body)
			cancel()
			if err != nil {
				return
			}
		}
	}
}
