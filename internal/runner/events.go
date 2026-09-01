package runner

import (
	"context"
	"log/slog"
	"sync"

	"github.com/alokw/cn4m-cascade/internal/engine"
	"github.com/alokw/cn4m-cascade/internal/store"
)

// eventBuffer batches run events so a 100k-file run does not perform 100k
// separate database writes. Every event is logged as it arrives, whether or
// not it has been flushed yet (CLAUDE.md).
type eventBuffer struct {
	db     *store.DB
	runID  string
	destID string
	log    *slog.Logger
	// ctx is the run's finalisation context: it outlives cancellation, so a
	// cancelled run can still record why it stopped.
	ctx context.Context

	mu      sync.Mutex
	pending []store.RunEvent
}

// flushThreshold bounds how much is held in memory between ticks.
const flushThreshold = 500

func newEventBuffer(ctx context.Context, db *store.DB, runID, destID string, log *slog.Logger) *eventBuffer {
	return &eventBuffer{ctx: ctx, db: db, runID: runID, destID: destID, log: log}
}

// Add records an event. Safe to call from the executor's worker goroutines.
func (b *eventBuffer) Add(ev engine.Event) {
	b.log.Log(b.ctx, levelToSlog(ev.Level), ev.Message,
		"run_id", b.runID, "relpath", ev.RelPath)

	b.mu.Lock()
	b.pending = append(b.pending, store.RunEvent{
		RunID:        b.runID,
		Level:        ev.Level,
		DestTargetID: b.destID,
		RelPath:      ev.RelPath,
		Message:      ev.Message,
	})
	overflowing := len(b.pending) >= flushThreshold
	b.mu.Unlock()

	if overflowing {
		b.Flush(b.ctx)
	}
}

// Flush writes whatever has accumulated.
func (b *eventBuffer) Flush(ctx context.Context) {
	b.mu.Lock()
	batch := b.pending
	b.pending = nil
	b.mu.Unlock()

	if len(batch) == 0 {
		return
	}
	if err := b.db.AppendEvents(ctx, batch); err != nil {
		// The log line already went out when the event was added, so the
		// information is not lost even if the database write fails.
		b.log.Error("could not write run events", "run_id", b.runID, "count", len(batch), "error", err)
	}
}

func levelToSlog(l store.EventLevel) slog.Level {
	switch l {
	case store.LevelError:
		return slog.LevelError
	case store.LevelWarn:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}
