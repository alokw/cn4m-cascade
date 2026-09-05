package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

// SyncMode is how a destination is made to match the source (SPEC.md §1).
type SyncMode string

const (
	// ModeMirror makes the destination match the source exactly, deleting
	// files the source no longer has.
	ModeMirror SyncMode = "mirror"
	// ModeUpdate copies new and newer files and never deletes.
	ModeUpdate SyncMode = "update"
	// ModeTwoWay arrives in Phase 5.
)

// CompareMethod is how two files are judged the same (SPEC.md §6.2).
type CompareMethod string

const (
	// CompareFast is size plus mtime within the job's tolerance.
	CompareFast CompareMethod = "fast"
	// CompareContent (streaming hash of both sides) arrives in Phase 6.
	CompareContent CompareMethod = "content"
)

// ErrorPolicy is what a per-file failure does to the run (SPEC.md §6.3).
type ErrorPolicy string

const (
	ErrorPolicySkip  ErrorPolicy = "skip"
	ErrorPolicyAbort ErrorPolicy = "abort"
)

// DeletePolicy decides what happens to mirror deletions when a run did not
// fully succeed.
//
// SPEC.md §7 lists delete_policy without defining it; this is the meaning it
// is given. Note that it governs the *copy failure* case only — an incomplete
// source scan blocks deletions unconditionally, because a directory that
// could not be read makes everything beneath it look extraneous at the
// destination, and mirroring that would delete live data.
type DeletePolicy string

const (
	// DeletePolicySkipDeletes leaves extraneous files alone when anything
	// failed. The default: extra files at the destination are corrected by
	// the next clean run, whereas a wrong deletion is not.
	DeletePolicySkipDeletes DeletePolicy = "skip_deletes"
	// DeletePolicyProceed deletes as planned despite copy failures.
	DeletePolicyProceed DeletePolicy = "proceed"
)

// CreateDestDirs decides what a run does when a destination's folder does not
// exist yet. It is separate from UnavailablePolicy on purpose: an unreachable
// share and an absent folder are different problems, and a job set to skip the
// former should not silently create the latter.
type CreateDestDirs string

const (
	// CreateDirsAsk prompts, and skips the destination if nobody answers.
	CreateDirsAsk CreateDestDirs = "ask"
	// CreateDirsAlways creates it without asking, and logs that it did.
	CreateDirsAlways CreateDestDirs = "always"
	// CreateDirsNever treats a missing folder as an unavailable destination.
	CreateDirsNever CreateDestDirs = "never"
)

// UnavailablePolicy is what a run does when a destination cannot be
// resolved (SPEC.md §6.1 step 2).
type UnavailablePolicy string

const (
	// PolicySkip logs it, marks that destination skipped, and continues
	// with the rest. A run finishing with any skipped destination is
	// `partial`.
	PolicySkip UnavailablePolicy = "skip"
	// PolicyAbort fails the whole run immediately.
	PolicyAbort UnavailablePolicy = "abort"
	// PolicyPrompt parks that destination and asks, leaving the run's other
	// destinations to carry on. An unanswered prompt falls back to
	// PromptFallback after PromptTimeoutSec.
	PolicyPrompt UnavailablePolicy = "prompt"
)

// PromptFallback is what an unanswered prompt does (SPEC.md §9's "countdown
// to the fallback action").
type PromptFallback string

const (
	// FallbackSkip leaves the destination alone and lets the run finish
	// partial. The default: a skipped destination is corrected by the next
	// run, which makes it the recoverable direction.
	FallbackSkip PromptFallback = "skip"
	// FallbackAbort ends the run.
	FallbackAbort PromptFallback = "abort"
)

// Prompt timeout bounds. A run may be unattended — from Phase 5 it may be
// started by cron with nobody watching at all — so the wait is always bounded.
const (
	MinPromptTimeoutSec     = 5
	MaxPromptTimeoutSec     = 86400
	DefaultPromptTimeoutSec = 600
)

// Workers bounds, from SPEC.md §6.1 step 7.
const (
	MinWorkers     = 1
	MaxWorkers     = 16
	DefaultWorkers = 4
)

// Job is a sync definition: one source, and in Phase 2 exactly one
// destination.
type Job struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	SourceTargetID string `json:"source_target_id"`
	SourceSubpath  string `json:"source_subpath,omitempty"`

	Mode    SyncMode      `json:"mode"`
	Compare CompareMethod `json:"compare"`

	// CompareToleranceSec absorbs the coarse mtime granularity of SMB and
	// FAT (SPEC.md §6.2).
	CompareToleranceSec int  `json:"compare_tolerance_sec"`
	IgnoreDSTHour       bool `json:"ignore_dst_hour"`

	Workers      int          `json:"workers"`
	OnError      ErrorPolicy  `json:"on_error"`
	DeletePolicy DeletePolicy `json:"delete_policy"`

	// LogEveryFile writes a run_event per file. Off by default: a 100k-file
	// run would otherwise write 100k rows (SPEC.md §6.5).
	LogEveryFile bool `json:"log_every_file"`

	// UnavailablePolicy governs an unreachable destination.
	UnavailablePolicy UnavailablePolicy `json:"unavailable_policy"`
	// PromptTimeoutSec bounds how long a `prompt` run waits for an answer,
	// and how long a previewed run holds its plan before being cancelled.
	PromptTimeoutSec int `json:"prompt_timeout_sec"`
	// PromptFallback is what happens when nobody answers in time.
	PromptFallback PromptFallback `json:"prompt_fallback"`
	// CreateDestDirs decides whether a missing destination folder is created,
	// asked about, or treated as unavailable.
	CreateDestDirs CreateDestDirs `json:"create_dest_dirs"`
	// ParallelDestinations runs destinations at the same time. Off by
	// default: fan-out multiplies read load on the source share
	// (SPEC.md §6.1 step 7).
	ParallelDestinations bool `json:"parallel_destinations"`

	Destinations []JobDestination `json:"destinations"`
	Filters      []FilterRule     `json:"filters"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// JobDestination is one target a job writes to.
type JobDestination struct {
	ID           string `json:"id"`
	JobID        string `json:"job_id"`
	DestTargetID string `json:"dest_target_id"`
	DestSubpath  string `json:"dest_subpath,omitempty"`
	Position     int    `json:"position"`
}

// Tolerance is the mtime comparison window.
func (j *Job) Tolerance() time.Duration {
	return time.Duration(j.CompareToleranceSec) * time.Second
}

// ApplyDefaults fills in the values the API lets a caller omit.
func (j *Job) ApplyDefaults() {
	if j.Mode == "" {
		j.Mode = ModeMirror
	}
	if j.Compare == "" {
		j.Compare = CompareFast
	}
	if j.Workers == 0 {
		j.Workers = DefaultWorkers
	}
	if j.OnError == "" {
		j.OnError = ErrorPolicySkip
	}
	if j.DeletePolicy == "" {
		j.DeletePolicy = DeletePolicySkipDeletes
	}
	if j.CompareToleranceSec == 0 {
		j.CompareToleranceSec = 2
	}
	if j.UnavailablePolicy == "" {
		j.UnavailablePolicy = PolicySkip
	}
	if j.PromptTimeoutSec == 0 {
		j.PromptTimeoutSec = DefaultPromptTimeoutSec
	}
	if j.PromptFallback == "" {
		j.PromptFallback = FallbackSkip
	}
	if j.CreateDestDirs == "" {
		j.CreateDestDirs = CreateDirsAsk
	}
	for i := range j.Filters {
		j.Filters[i].ApplyDefaults()
	}
}

// Validate checks a job for internal consistency. Messages are user-facing.
func (j *Job) Validate() error {
	if strings.TrimSpace(j.Name) == "" {
		return errors.New("name is required")
	}
	if j.SourceTargetID == "" {
		return errors.New("a source target is required")
	}
	if err := validateSubpath("source_subpath", j.SourceSubpath); err != nil {
		return err
	}

	switch j.Mode {
	case ModeMirror, ModeUpdate:
	case "twoway":
		return errors.New(`mode "twoway" is not implemented yet`)
	default:
		return fmt.Errorf("mode must be %q or %q, got %q", ModeMirror, ModeUpdate, j.Mode)
	}

	switch j.Compare {
	case CompareFast:
	case CompareContent:
		return errors.New(`compare "content" is not implemented yet; use "fast"`)
	default:
		return fmt.Errorf("compare must be %q, got %q", CompareFast, j.Compare)
	}

	switch j.OnError {
	case ErrorPolicySkip, ErrorPolicyAbort:
	default:
		return fmt.Errorf("on_error must be %q or %q, got %q", ErrorPolicySkip, ErrorPolicyAbort, j.OnError)
	}

	switch j.DeletePolicy {
	case DeletePolicySkipDeletes, DeletePolicyProceed:
	default:
		return fmt.Errorf("delete_policy must be %q or %q, got %q",
			DeletePolicySkipDeletes, DeletePolicyProceed, j.DeletePolicy)
	}

	if j.Workers < MinWorkers || j.Workers > MaxWorkers {
		return fmt.Errorf("workers must be between %d and %d, got %d", MinWorkers, MaxWorkers, j.Workers)
	}
	if j.CompareToleranceSec < 0 {
		return fmt.Errorf("compare_tolerance_sec must not be negative, got %d", j.CompareToleranceSec)
	}

	switch j.UnavailablePolicy {
	case PolicySkip, PolicyAbort, PolicyPrompt:
	default:
		return fmt.Errorf("unavailable_policy must be %q, %q or %q, got %q",
			PolicySkip, PolicyAbort, PolicyPrompt, j.UnavailablePolicy)
	}

	switch j.PromptFallback {
	case FallbackSkip, FallbackAbort:
	default:
		return fmt.Errorf("prompt_fallback must be %q or %q, got %q",
			FallbackSkip, FallbackAbort, j.PromptFallback)
	}
	switch j.CreateDestDirs {
	case CreateDirsAsk, CreateDirsAlways, CreateDirsNever:
	default:
		return fmt.Errorf("create_dest_dirs must be %q, %q or %q, got %q",
			CreateDirsAsk, CreateDirsAlways, CreateDirsNever, j.CreateDestDirs)
	}
	if j.PromptTimeoutSec < MinPromptTimeoutSec || j.PromptTimeoutSec > MaxPromptTimeoutSec {
		return fmt.Errorf("prompt_timeout_sec must be between %d and %d, got %d",
			MinPromptTimeoutSec, MaxPromptTimeoutSec, j.PromptTimeoutSec)
	}

	if len(j.Destinations) == 0 {
		return errors.New("at least one destination is required")
	}

	seen := map[string]bool{}
	for _, d := range j.Destinations {
		if d.DestTargetID == "" {
			return errors.New("each destination needs a target")
		}
		if err := validateSubpath("dest_subpath", d.DestSubpath); err != nil {
			return err
		}
		if d.DestTargetID == j.SourceTargetID {
			// Not just equality: a destination nested inside the source (or
			// vice versa) means each run copies the tree into itself one
			// level deeper, and in mirror mode the deletion pass then
			// operates on a tree containing its own source.
			if overlaps(j.SourceSubpath, d.DestSubpath) {
				return errors.New("a job's destination must not be the same location as its source, or nested inside it")
			}
		}

		// One destination per target, per job. `run_destinations` is keyed
		// by (run_id, dest_target_id) in SPEC.md §7, so a second
		// destination on the same target could be saved but could never
		// run — and per-destination progress, filters and log entries are
		// all keyed by target ID too.
		if seen[d.DestTargetID] {
			return errors.New("a job cannot have two destinations on the same target; use one destination per target")
		}
		seen[d.DestTargetID] = true
	}

	for i := range j.Filters {
		rule := &j.Filters[i]
		if err := rule.Validate(); err != nil {
			return fmt.Errorf("filter rule %d: %w", i+1, err)
		}
		if rule.Scope == ScopeTarget && !j.hasDestination(rule.ScopeTargetID) {
			return fmt.Errorf("filter rule %d is scoped to target %s, which is not a destination of this job",
				i+1, rule.ScopeTargetID)
		}
	}
	return nil
}

// hasDestination reports whether a target is one of this job's destinations.
func (j *Job) hasDestination(targetID string) bool {
	for _, d := range j.Destinations {
		if d.DestTargetID == targetID {
			return true
		}
	}
	return false
}

// overlaps reports whether two subpaths of the same target are the same
// location or one contains the other.
func overlaps(a, b string) bool {
	a = strings.Trim(path.Clean("/"+a), "/")
	b = strings.Trim(path.Clean("/"+b), "/")

	if a == b {
		return true
	}
	if a == "" || b == "" {
		// One of them is the target root, which contains everything.
		return true
	}
	return strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func validateSubpath(field, subpath string) error { return ValidateSubpath(field, subpath) }

// ValidateSubpath rejects a path that is absolute or escapes its root.
// Exported because the API's path picker validates browse paths the same way
// job subpaths are validated (SPEC.md §8's /api/browse).
func ValidateSubpath(field, subpath string) error {
	if subpath == "" {
		return nil
	}
	if path.IsAbs(subpath) {
		return fmt.Errorf("%s must be relative to the target root, not an absolute path", field)
	}
	for _, segment := range strings.Split(subpath, "/") {
		if segment == ".." {
			return fmt.Errorf("%s must not contain \"..\" path segments", field)
		}
	}
	return nil
}

const jobColumns = `id, name, source_target_id, source_subpath, mode, compare,
	compare_tolerance_sec, ignore_dst_hour, workers, on_error, delete_policy,
	log_every_file, unavailable_policy, parallel_destinations,
	prompt_timeout_sec, prompt_fallback, create_dest_dirs, created_at, updated_at`

// CreateJob inserts a job and its destinations in one transaction.
func (d *DB) CreateJob(ctx context.Context, j *Job) error {
	j.ApplyDefaults()
	if err := j.Validate(); err != nil {
		return err
	}

	id, err := newID()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	j.ID, j.CreatedAt, j.UpdatedAt = id, now, now

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("creating job %q: %w", j.Name, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs (`+jobColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, jobInsertArgs(j)...); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("a job named %q already exists: %w", j.Name, ErrNameTaken)
		}
		return fmt.Errorf("creating job %q: %w", j.Name, err)
	}

	if err := insertJobChildren(ctx, tx, j); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("creating job %q: %w", j.Name, err)
	}
	return nil
}

// jobInsertArgs lists a job's columns in jobColumns order. Create and update
// share it so the two can never drift out of step.
func jobInsertArgs(j *Job) []any {
	return []any{
		j.ID, j.Name, j.SourceTargetID, j.SourceSubpath, string(j.Mode), string(j.Compare),
		j.CompareToleranceSec, boolToInt(j.IgnoreDSTHour), j.Workers, string(j.OnError),
		string(j.DeletePolicy), boolToInt(j.LogEveryFile), string(j.UnavailablePolicy),
		boolToInt(j.ParallelDestinations), j.PromptTimeoutSec, string(j.PromptFallback),
		string(j.CreateDestDirs), formatTime(j.CreatedAt), formatTime(j.UpdatedAt),
	}
}

// insertJobChildren writes a job's destinations and filter rules. Both are
// fully owned by the job, which is what lets UpdateJob replace them wholesale.
func insertJobChildren(ctx context.Context, tx *sql.Tx, j *Job) error {
	for i := range j.Destinations {
		destID, err := newID()
		if err != nil {
			return err
		}
		j.Destinations[i].ID = destID
		j.Destinations[i].JobID = j.ID
		j.Destinations[i].Position = i

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO job_destinations (id, job_id, dest_target_id, dest_subpath, position)
			 VALUES (?,?,?,?,?)`,
			destID, j.ID, j.Destinations[i].DestTargetID, j.Destinations[i].DestSubpath, i); err != nil {
			return fmt.Errorf("adding a destination to job %q: %w", j.Name, err)
		}
	}

	for i := range j.Filters {
		if err := insertFilterRule(ctx, tx, j.ID, &j.Filters[i], i); err != nil {
			return err
		}
	}
	return nil
}

// UpdateJob replaces a job, its destinations and its filter rules in one
// transaction (SPEC.md §8's "create/update job").
//
// Destinations and filters are replaced wholesale rather than diffed: they are
// positional, fully owned by the job, and the UI edits them as a single form.
// Callers must refuse this while the job is running — see api.handleUpdateJob;
// changing destinations under a live diff is not something the engine is built
// to survive.
func (d *DB) UpdateJob(ctx context.Context, id string, j *Job) error {
	j.ApplyDefaults()
	if err := j.Validate(); err != nil {
		return err
	}

	existing, err := d.GetJob(ctx, id)
	if err != nil {
		return err
	}
	j.ID = existing.ID
	j.CreatedAt = existing.CreatedAt
	j.UpdatedAt = time.Now().UTC()

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("updating job %q: %w", j.Name, err)
	}
	defer func() { _ = tx.Rollback() }()

	const set = `UPDATE jobs SET name=?, source_target_id=?, source_subpath=?, mode=?, compare=?,
		compare_tolerance_sec=?, ignore_dst_hour=?, workers=?, on_error=?, delete_policy=?,
		log_every_file=?, unavailable_policy=?, parallel_destinations=?,
		prompt_timeout_sec=?, prompt_fallback=?, create_dest_dirs=?, updated_at=? WHERE id=?`

	if _, err := tx.ExecContext(ctx, set,
		j.Name, j.SourceTargetID, j.SourceSubpath, string(j.Mode), string(j.Compare),
		j.CompareToleranceSec, boolToInt(j.IgnoreDSTHour), j.Workers, string(j.OnError),
		string(j.DeletePolicy), boolToInt(j.LogEveryFile), string(j.UnavailablePolicy),
		boolToInt(j.ParallelDestinations), j.PromptTimeoutSec, string(j.PromptFallback),
		string(j.CreateDestDirs), formatTime(j.UpdatedAt), j.ID); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("a job named %q already exists: %w", j.Name, ErrNameTaken)
		}
		return fmt.Errorf("updating job %q: %w", j.Name, err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM job_destinations WHERE job_id = ?`, j.ID); err != nil {
		return fmt.Errorf("updating the destinations of job %q: %w", j.Name, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM filter_rules WHERE job_id = ?`, j.ID); err != nil {
		return fmt.Errorf("updating the filters of job %q: %w", j.Name, err)
	}
	if err := insertJobChildren(ctx, tx, j); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("updating job %q: %w", j.Name, err)
	}
	return nil
}

// GetJob loads a job with its destinations.
func (d *DB) GetJob(ctx context.Context, id string) (*Job, error) {
	row := d.sql.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("job %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("loading job %s: %w", id, err)
	}

	if j.Destinations, err = d.jobDestinations(ctx, id); err != nil {
		return nil, err
	}
	if j.Filters, err = d.ListFilterRules(ctx, id); err != nil {
		return nil, err
	}
	return j, nil
}

// ListJobs returns every job, newest first, each with its destinations.
func (d *DB) ListJobs(ctx context.Context) ([]*Job, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("listing jobs: %w", err)
	}
	defer rows.Close()

	jobs := []*Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("listing jobs: %w", err)
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing jobs: %w", err)
	}

	for _, j := range jobs {
		if j.Destinations, err = d.jobDestinations(ctx, j.ID); err != nil {
			return nil, err
		}
		if j.Filters, err = d.ListFilterRules(ctx, j.ID); err != nil {
			return nil, err
		}
	}
	return jobs, nil
}

// DeleteJob removes a job; its destinations cascade.
// DeleteJob removes a job and its run history.
//
// The history has to go with it: runs.job_id references jobs(id) *without*
// ON DELETE CASCADE — unlike job_destinations and filter_rules — so deleting a
// job that has ever run otherwise fails with a raw
// "FOREIGN KEY constraint failed (787)" that reached the user verbatim.
//
// Deleting the runs is the deliberate choice over refusing: a job nobody can
// delete because it once ran is worse, and an orphaned run is a log entry
// pointing at a job that no longer exists. run_destinations and run_events do
// cascade from runs, so removing the runs takes their children with them. The
// API surfaces the count first so the loss is stated before it happens.
func (d *DB) DeleteJob(ctx context.Context, id string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("deleting job %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE job_id = ?`, id); err != nil {
		return fmt.Errorf("deleting the run history of job %s: %w", id, err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting job %s: %w", id, err)
	}
	if err := checkAffected(res, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("deleting job %s: %w", id, err)
	}
	return nil
}

// CountRuns reports how many runs a job has, so a delete can say what it is
// about to destroy rather than discovering it afterwards.
func (d *DB) CountRuns(ctx context.Context, jobID string) (int, error) {
	var n int
	err := d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE job_id = ?`, jobID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting the runs of job %s: %w", jobID, err)
	}
	return n, nil
}

func (d *DB) jobDestinations(ctx context.Context, jobID string) ([]JobDestination, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT id, job_id, dest_target_id, dest_subpath, position
		 FROM job_destinations WHERE job_id = ? ORDER BY position`, jobID)
	if err != nil {
		return nil, fmt.Errorf("loading the destinations of job %s: %w", jobID, err)
	}
	defer rows.Close()

	dests := []JobDestination{}
	for rows.Next() {
		var d JobDestination
		if err := rows.Scan(&d.ID, &d.JobID, &d.DestTargetID, &d.DestSubpath, &d.Position); err != nil {
			return nil, fmt.Errorf("loading the destinations of job %s: %w", jobID, err)
		}
		dests = append(dests, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("loading the destinations of job %s: %w", jobID, err)
	}
	return dests, nil
}

func scanJob(s scanner) (*Job, error) {
	var (
		j                                 Job
		mode, compare, onErr, delPol      string
		unavailable, promptFallback       string
		createDestDirs                    string
		created, updated                  string
		ignoreDST, logEveryFile, parallel int
	)
	err := s.Scan(&j.ID, &j.Name, &j.SourceTargetID, &j.SourceSubpath, &mode, &compare,
		&j.CompareToleranceSec, &ignoreDST, &j.Workers, &onErr, &delPol, &logEveryFile,
		&unavailable, &parallel, &j.PromptTimeoutSec, &promptFallback, &createDestDirs,
		&created, &updated)
	if err != nil {
		return nil, err
	}
	j.UnavailablePolicy = UnavailablePolicy(unavailable)
	j.CreateDestDirs = CreateDestDirs(createDestDirs)
	j.PromptFallback = PromptFallback(promptFallback)
	j.ParallelDestinations = parallel != 0
	j.Mode = SyncMode(mode)
	j.Compare = CompareMethod(compare)
	j.OnError = ErrorPolicy(onErr)
	j.DeletePolicy = DeletePolicy(delPol)
	j.IgnoreDSTHour = ignoreDST != 0
	j.LogEveryFile = logEveryFile != 0
	j.CreatedAt = parseTime(created)
	j.UpdatedAt = parseTime(updated)
	return &j, nil
}
