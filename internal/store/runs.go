package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RunStatus is the state of a whole run (SPEC.md §7).
type RunStatus string

const (
	RunRunning RunStatus = "running"
	// RunAwaitingConfirmation is a previewed run that has planned its work
	// and is holding it, plus its mounts, until a human confirms or the
	// job's prompt timeout expires (SPEC.md §6.1 step 6).
	RunAwaitingConfirmation RunStatus = "awaiting_confirmation"
	RunSuccess              RunStatus = "success"
	RunPartial              RunStatus = "partial"
	RunFailed               RunStatus = "failed"
	RunCancelled            RunStatus = "cancelled"
)

// RunTrigger records what started a run.
type RunTrigger string

const (
	TriggerManual RunTrigger = "manual"
	// TriggerSchedule marks a run the scheduler started with nobody watching,
	// which is why it is worth distinguishing in the log: an unattended run
	// that fell back on a prompt reads very differently from one a person
	// declined to answer.
	TriggerSchedule RunTrigger = "schedule"
	// TriggerWebhook marks a run some other piece of software started through
	// /api/hooks/* — a NAS task, n8n, Home Assistant. Worth distinguishing
	// from `schedule` as well as from `manual`: when a run misbehaves, "which
	// integration asked for this?" is a different question from "did the cron
	// fire?".
	TriggerWebhook RunTrigger = "webhook"
)

// DestStatus is the state of one destination within a run.
type DestStatus string

const (
	DestPending   DestStatus = "pending"
	DestRunning   DestStatus = "running"
	DestSuccess   DestStatus = "success"
	DestFailed    DestStatus = "failed"
	DestCancelled DestStatus = "cancelled"
	// DestSkippedUnavailable means the destination could not be reached and
	// the job's unavailable_policy said to carry on without it.
	DestSkippedUnavailable DestStatus = "skipped_unavailable"
	// DestAwaitingPrompt means the destination could not be reached and the
	// job's unavailable_policy is `prompt`, so it is waiting for an answer.
	// The *run* stays running and its other destinations keep working: only
	// the one that cannot be reached waits.
	DestAwaitingPrompt DestStatus = "awaiting_prompt"
)

// Run is one execution of a job.
type Run struct {
	ID           string     `json:"id"`
	JobID        string     `json:"job_id"`
	Trigger      RunTrigger `json:"trigger"`
	Status       RunStatus  `json:"status"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	FilesScanned int64      `json:"files_scanned"`
	BytesTotal   int64      `json:"bytes_total"`
	ErrorSummary string     `json:"error_summary,omitempty"`

	Destinations []RunDestination `json:"destinations"`
}

// RunDestination is the per-destination result and progress of a run.
type RunDestination struct {
	RunID        string     `json:"run_id"`
	DestTargetID string     `json:"dest_target_id"`
	Status       DestStatus `json:"status"`
	FilesTotal   int64      `json:"files_total"`
	FilesDone    int64      `json:"files_done"`
	FilesDeleted int64      `json:"files_deleted"`
	BytesTotal   int64      `json:"bytes_total"`
	BytesDone    int64      `json:"bytes_done"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	ErrorSummary string     `json:"error_summary,omitempty"`
}

// Terminal reports whether a run has finished, however it ended.
//
// The terminal states are enumerated rather than derived from "not running":
// a run awaiting confirmation is neither running nor finished, and the older
// `Status != RunRunning` form would have called it done and let callers treat
// a parked run as a completed one.
func (r *Run) Terminal() bool {
	switch r.Status {
	case RunSuccess, RunPartial, RunFailed, RunCancelled:
		return true
	default:
		return false
	}
}

// Active reports whether a run still holds resources — it is either working
// or parked waiting for a human. Shutdown and the "already running" check both
// need this rather than Terminal's inverse, because a parked run holds mounts.
func (r *Run) Active() bool { return !r.Terminal() }

const runColumns = `id, job_id, trigger, status, started_at, finished_at,
	files_scanned, bytes_total, error_summary`

const runDestColumns = `run_id, dest_target_id, status, files_total, files_done,
	files_deleted, bytes_total, bytes_done, started_at, finished_at, error_summary`

// CreateRun opens a run in the running state, with one pending row per
// destination.
func (d *DB) CreateRun(ctx context.Context, jobID string, trigger RunTrigger, destTargetIDs []string) (*Run, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}

	run := &Run{
		ID:        id,
		JobID:     jobID,
		Trigger:   trigger,
		Status:    RunRunning,
		StartedAt: time.Now().UTC(),
	}

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("starting a run of job %s: %w", jobID, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `INSERT INTO runs (`+runColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		run.ID, run.JobID, string(run.Trigger), string(run.Status),
		formatTime(run.StartedAt), nil, 0, 0, ""); err != nil {
		return nil, fmt.Errorf("starting a run of job %s: %w", jobID, err)
	}

	for _, destID := range destTargetIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO run_destinations (`+runDestColumns+`)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			run.ID, destID, string(DestPending), 0, 0, 0, 0, 0, nil, nil, ""); err != nil {
			return nil, fmt.Errorf("starting a run of job %s: %w", jobID, err)
		}
		run.Destinations = append(run.Destinations, RunDestination{
			RunID: run.ID, DestTargetID: destID, Status: DestPending,
		})
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("starting a run of job %s: %w", jobID, err)
	}
	return run, nil
}

// SetRunScanTotals records what the scan found, so progress has a denominator.
func (d *DB) SetRunScanTotals(ctx context.Context, runID string, filesScanned, bytesTotal int64) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE runs SET files_scanned = ?, bytes_total = ? WHERE id = ?`,
		filesScanned, bytesTotal, runID)
	if err != nil {
		return fmt.Errorf("recording scan totals for run %s: %w", runID, err)
	}
	return nil
}

// FinishRun closes a run out.
func (d *DB) FinishRun(ctx context.Context, runID string, status RunStatus, errorSummary string) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE runs SET status = ?, finished_at = ?, error_summary = ? WHERE id = ?`,
		string(status), formatTime(time.Now().UTC()), errorSummary, runID)
	if err != nil {
		return fmt.Errorf("finishing run %s: %w", runID, err)
	}
	return nil
}

// StartRunDestination marks a destination as being worked on and records the
// plan's totals for it.
func (d *DB) StartRunDestination(ctx context.Context, runID, destTargetID string, filesTotal, bytesTotal int64) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE run_destinations SET status = ?, started_at = ?, files_total = ?, bytes_total = ?
		 WHERE run_id = ? AND dest_target_id = ?`,
		string(DestRunning), formatTime(time.Now().UTC()), filesTotal, bytesTotal, runID, destTargetID)
	if err != nil {
		return fmt.Errorf("starting destination %s of run %s: %w", destTargetID, runID, err)
	}
	return nil
}

// UpdateRunDestinationProgress flushes the in-memory progress counters to the
// database. Called periodically rather than per file.
func (d *DB) UpdateRunDestinationProgress(ctx context.Context, runID, destTargetID string, filesDone, filesDeleted, bytesDone int64) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE run_destinations SET files_done = ?, files_deleted = ?, bytes_done = ?
		 WHERE run_id = ? AND dest_target_id = ?`,
		filesDone, filesDeleted, bytesDone, runID, destTargetID)
	if err != nil {
		return fmt.Errorf("updating progress for destination %s of run %s: %w", destTargetID, runID, err)
	}
	return nil
}

// SetRunDestinationStatus records a non-terminal state change, such as a
// destination parking to wait for an answer. FinishRunDestination remains the
// only way to record an outcome.
func (d *DB) SetRunDestinationStatus(ctx context.Context, runID, destTargetID string, status DestStatus) error {
	if _, err := d.sql.ExecContext(ctx,
		`UPDATE run_destinations SET status = ? WHERE run_id = ? AND dest_target_id = ?`,
		string(status), runID, destTargetID); err != nil {
		return fmt.Errorf("recording the state of destination %s in run %s: %w", destTargetID, runID, err)
	}
	return nil
}

// SetRunStatus records a non-terminal run state, such as parking on a preview.
func (d *DB) SetRunStatus(ctx context.Context, runID string, status RunStatus) error {
	if _, err := d.sql.ExecContext(ctx,
		`UPDATE runs SET status = ? WHERE id = ?`, string(status), runID); err != nil {
		return fmt.Errorf("recording the state of run %s: %w", runID, err)
	}
	return nil
}

// FinishRunDestination closes out one destination.
func (d *DB) FinishRunDestination(ctx context.Context, runID, destTargetID string, status DestStatus, errorSummary string) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE run_destinations SET status = ?, finished_at = ?, error_summary = ?
		 WHERE run_id = ? AND dest_target_id = ?`,
		string(status), formatTime(time.Now().UTC()), errorSummary, runID, destTargetID)
	if err != nil {
		return fmt.Errorf("finishing destination %s of run %s: %w", destTargetID, runID, err)
	}
	return nil
}

// GetRun loads a run with its per-destination rows.
func (d *DB) GetRun(ctx context.Context, id string) (*Run, error) {
	row := d.sql.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, id)
	run, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("run %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("loading run %s: %w", id, err)
	}

	if run.Destinations, err = d.runDestinations(ctx, id); err != nil {
		return nil, err
	}
	return run, nil
}

// RunFilter narrows a run listing.
type RunFilter struct {
	JobID  string
	Status RunStatus
	Limit  int
}

// ListRuns returns runs newest first.
func (d *DB) ListRuns(ctx context.Context, f RunFilter) ([]*Run, error) {
	query := `SELECT ` + runColumns + ` FROM runs WHERE 1=1`
	args := []any{}

	if f.JobID != "" {
		query += ` AND job_id = ?`
		args = append(args, f.JobID)
	}
	if f.Status != "" {
		query += ` AND status = ?`
		args = append(args, string(f.Status))
	}
	query += ` ORDER BY started_at DESC, id`

	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	query += ` LIMIT ?`
	args = append(args, f.Limit)

	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing runs: %w", err)
	}
	defer rows.Close()

	runs := []*Run{}
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("listing runs: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing runs: %w", err)
	}

	for _, run := range runs {
		if run.Destinations, err = d.runDestinations(ctx, run.ID); err != nil {
			return nil, err
		}
	}
	return runs, nil
}

// ReconcileInterruptedRuns settles runs that were still live at startup: the
// process died, so nothing will ever finish them.
//
// A run that was *working* failed. A run that was merely parked awaiting
// confirmation is cancelled instead, because an unconfirmed preview executed
// nothing — calling that a failure would report work as lost that was never
// started. Either way no run may survive a restart still holding a state that
// waits for a human who is no longer being asked.
func (d *DB) ReconcileInterruptedRuns(ctx context.Context) (int64, error) {
	const (
		failedMsg    = "the server stopped while this run was in progress"
		cancelledMsg = "the server stopped while this run was waiting to be confirmed; nothing was copied"
	)
	now := formatTime(time.Now().UTC())

	res, err := d.sql.ExecContext(ctx,
		`UPDATE runs SET status = ?, finished_at = ?, error_summary = ? WHERE status = ?`,
		string(RunFailed), now, failedMsg, string(RunRunning))
	if err != nil {
		return 0, fmt.Errorf("reconciling interrupted runs: %w", err)
	}
	parked, err := d.sql.ExecContext(ctx,
		`UPDATE runs SET status = ?, finished_at = ?, error_summary = ? WHERE status = ?`,
		string(RunCancelled), now, cancelledMsg, string(RunAwaitingConfirmation))
	if err != nil {
		return 0, fmt.Errorf("reconciling interrupted runs: %w", err)
	}
	if _, err := d.sql.ExecContext(ctx,
		`UPDATE run_destinations SET status = ?, finished_at = ? WHERE status IN (?, ?, ?)`,
		string(DestFailed), now,
		string(DestRunning), string(DestPending), string(DestAwaitingPrompt)); err != nil {
		return 0, fmt.Errorf("reconciling interrupted runs: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("counting reconciled runs: %w", err)
	}
	p, err := parked.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("counting reconciled runs: %w", err)
	}
	return n + p, nil
}

func (d *DB) runDestinations(ctx context.Context, runID string) ([]RunDestination, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+runDestColumns+` FROM run_destinations WHERE run_id = ?`, runID)
	if err != nil {
		return nil, fmt.Errorf("loading the destinations of run %s: %w", runID, err)
	}
	defer rows.Close()

	dests := []RunDestination{}
	for rows.Next() {
		var (
			rd                RunDestination
			status            string
			started, finished sql.NullString
		)
		if err := rows.Scan(&rd.RunID, &rd.DestTargetID, &status, &rd.FilesTotal, &rd.FilesDone,
			&rd.FilesDeleted, &rd.BytesTotal, &rd.BytesDone, &started, &finished, &rd.ErrorSummary); err != nil {
			return nil, fmt.Errorf("loading the destinations of run %s: %w", runID, err)
		}
		rd.Status = DestStatus(status)
		rd.StartedAt = nullableTime(started)
		rd.FinishedAt = nullableTime(finished)
		dests = append(dests, rd)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("loading the destinations of run %s: %w", runID, err)
	}
	return dests, nil
}

func scanRun(s scanner) (*Run, error) {
	var (
		run             Run
		trigger, status string
		started         string
		finished        sql.NullString
	)
	if err := s.Scan(&run.ID, &run.JobID, &trigger, &status, &started, &finished,
		&run.FilesScanned, &run.BytesTotal, &run.ErrorSummary); err != nil {
		return nil, err
	}
	run.Trigger = RunTrigger(trigger)
	run.Status = RunStatus(status)
	run.StartedAt = parseTime(started)
	run.FinishedAt = nullableTime(finished)
	return &run, nil
}

func nullableTime(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	t := parseTime(s.String)
	return &t
}
