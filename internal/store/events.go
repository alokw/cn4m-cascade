package store

import (
	"context"
	"fmt"
	"time"
)

// EventLevel is the severity of a run event (SPEC.md §7).
type EventLevel string

const (
	LevelInfo  EventLevel = "info"
	LevelWarn  EventLevel = "warn"
	LevelError EventLevel = "error"
)

// RunEvent is one line of the task/error log.
type RunEvent struct {
	ID           int64      `json:"id"`
	RunID        string     `json:"run_id"`
	TS           time.Time  `json:"ts"`
	Level        EventLevel `json:"level"`
	DestTargetID string     `json:"dest_target_id,omitempty"`
	RelPath      string     `json:"relpath,omitempty"`
	Message      string     `json:"message"`
}

// AppendEvent writes one event. Callers also log it (CLAUDE.md: every
// run_event written to the DB is also logged).
func (d *DB) AppendEvent(ctx context.Context, e *RunEvent) error {
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	res, err := d.sql.ExecContext(ctx,
		`INSERT INTO run_events (run_id, ts, level, dest_target_id, relpath, message)
		 VALUES (?,?,?,?,?,?)`,
		e.RunID, formatTime(e.TS), string(e.Level), e.DestTargetID, e.RelPath, e.Message)
	if err != nil {
		return fmt.Errorf("writing a run event for run %s: %w", e.RunID, err)
	}
	if id, err := res.LastInsertId(); err == nil {
		e.ID = id
	}
	return nil
}

// AppendEvents writes a batch in one transaction. The executor buffers events
// so that a 100k-file run does not do 100k separate writes.
func (d *DB) AppendEvents(ctx context.Context, events []RunEvent) error {
	if len(events) == 0 {
		return nil
	}

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("writing run events: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO run_events (run_id, ts, level, dest_target_id, relpath, message)
		 VALUES (?,?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("writing run events: %w", err)
	}
	defer stmt.Close()

	for i := range events {
		e := &events[i]
		if e.TS.IsZero() {
			e.TS = time.Now().UTC()
		}
		if _, err := stmt.ExecContext(ctx,
			e.RunID, formatTime(e.TS), string(e.Level), e.DestTargetID, e.RelPath, e.Message); err != nil {
			return fmt.Errorf("writing run events: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("writing run events: %w", err)
	}
	return nil
}

// EventFilter narrows an event listing.
//
// Every field is optional. With no RunID this reads across every run, which is
// what the global log view of SPEC.md §8 (`GET /api/logs`) needs; migration
// 0004 adds the ts and (level, ts) indexes that keeps off a full table scan.
type EventFilter struct {
	RunID        string
	JobID        string
	Level        EventLevel
	DestTargetID string
	// Since bounds the listing to events at or after this time. The zero
	// value means no lower bound.
	Since time.Time
	// Newest reverses the order. A task log reads oldest first; a global
	// error log reads newest first.
	Newest bool
	Limit  int
	Offset int
}

// ListEvents returns events oldest first by default, which is the order a task
// log reads; set Newest for the global log view.
func (d *DB) ListEvents(ctx context.Context, f EventFilter) ([]RunEvent, error) {
	query := `SELECT e.id, e.run_id, e.ts, e.level, e.dest_target_id, e.relpath, e.message
		FROM run_events e`
	args := []any{}

	// Only join when filtering by job: the join is pure cost otherwise, and
	// this table is the largest in the schema.
	if f.JobID != "" {
		query += ` JOIN runs r ON r.id = e.run_id`
	}
	query += ` WHERE 1=1`

	if f.RunID != "" {
		query += ` AND e.run_id = ?`
		args = append(args, f.RunID)
	}
	if f.JobID != "" {
		query += ` AND r.job_id = ?`
		args = append(args, f.JobID)
	}
	if f.Level != "" {
		query += ` AND e.level = ?`
		args = append(args, string(f.Level))
	}
	if f.DestTargetID != "" {
		query += ` AND e.dest_target_id = ?`
		args = append(args, f.DestTargetID)
	}
	if !f.Since.IsZero() {
		query += ` AND e.ts >= ?`
		args = append(args, formatTime(f.Since.UTC()))
	}
	if f.Newest {
		query += ` ORDER BY e.ts DESC, e.id DESC`
	} else {
		query += ` ORDER BY e.id`
	}

	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 200
	}
	query += ` LIMIT ? OFFSET ?`
	args = append(args, f.Limit, max(f.Offset, 0))

	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing run events: %w", err)
	}
	defer rows.Close()

	events := []RunEvent{}
	for rows.Next() {
		var (
			e     RunEvent
			ts    string
			level string
		)
		if err := rows.Scan(&e.ID, &e.RunID, &ts, &level, &e.DestTargetID, &e.RelPath, &e.Message); err != nil {
			return nil, fmt.Errorf("listing run events: %w", err)
		}
		e.TS = parseTime(ts)
		e.Level = EventLevel(level)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing run events: %w", err)
	}
	return events, nil
}

// CountEventsByLevel summarises a run's log, for the run detail view.
func (d *DB) CountEventsByLevel(ctx context.Context, runID string) (map[EventLevel]int64, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT level, COUNT(*) FROM run_events WHERE run_id = ? GROUP BY level`, runID)
	if err != nil {
		return nil, fmt.Errorf("counting events for run %s: %w", runID, err)
	}
	defer rows.Close()

	counts := map[EventLevel]int64{}
	for rows.Next() {
		var (
			level string
			n     int64
		)
		if err := rows.Scan(&level, &n); err != nil {
			return nil, fmt.Errorf("counting events for run %s: %w", runID, err)
		}
		counts[EventLevel(level)] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("counting events for run %s: %w", runID, err)
	}
	return counts, nil
}
