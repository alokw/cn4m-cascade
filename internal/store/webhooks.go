package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Webhook is one outbound status callback (SPEC.md §8.2).
type Webhook struct {
	ID string `json:"id"`
	// JobID empty means every job: configuring "tell my dashboard about
	// everything" once beats repeating it per job and forgetting one.
	JobID  string   `json:"job_id,omitempty"`
	URL    string   `json:"url"`
	Events []string `json:"events"`
	// SecretEncrypted is the HMAC key at rest, in the same form target
	// passwords use: the store never encrypts or decrypts, it only carries the
	// ciphertext. Whoever needs the key holds the box — mountmgr already works
	// this way, and copying it keeps the encryption key out of the store
	// entirely.
	SecretEncrypted string `json:"-"`
	Enabled         bool   `json:"enabled"`
	// Format is the wire shape: "json" for a signed JSON body, "cn4m" for the
	// form-encoded app/message/level triple cn4m's /suite/status accepts.
	Format         string    `json:"format"`
	MinIntervalSec int       `json:"min_interval_sec"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Callback wire formats.
const (
	// FormatJSON posts the §8.2 status payload as JSON with an HMAC signature.
	FormatJSON = "json"
	// FormatCN4M posts form-encoded app/message/level to cn4m's /suite/status.
	//
	// Failures are best effort and never recorded against a run: cn4m is
	// optional infrastructure whose absence is a normal state rather than a
	// fault of the sync, and a callback that put a warning on every run of a
	// machine without cn4m would teach people to ignore run warnings.
	FormatCN4M = "cn4m"
	// FormatDiscord posts a one-line message to a Discord webhook.
	//
	// Best-effort like cn4m: a chat notification that cannot be delivered must
	// never fail a sync or fill the run log with warnings about somebody
	// else's outage.
	FormatDiscord = "discord"
)

// BestEffort reports whether delivery failures should stay out of the run log.
func (w *Webhook) BestEffort() bool {
	return w.Format == FormatCN4M || w.Format == FormatDiscord
}

// KnownEvents is every event a webhook may subscribe to (SPEC.md §8.2).
var KnownEvents = []string{
	"run_started", "progress", "target_unavailable_prompt", "run_completed", "run_failed",
}

// Subscribes reports whether this hook wants a given event. An empty list
// means every event, so a hook created without choosing is useful rather than
// silent.
func (w *Webhook) Subscribes(event string) bool {
	if !w.Enabled {
		return false
	}
	if len(w.Events) == 0 {
		return true
	}
	return slices.Contains(w.Events, event)
}

// Validate checks a webhook. Messages are user-facing.
func (w *Webhook) Validate() error {
	trimmed := strings.TrimSpace(w.URL)
	if trimmed == "" {
		return fmt.Errorf("a callback URL is required")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("%q is not a valid URL: %w", trimmed, err)
	}
	// http and https only. Without this, "file:///..." and friends reach an
	// HTTP client that will do something surprising with them.
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("the callback URL must start with http:// or https://, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("%q has no host", trimmed)
	}

	for _, e := range w.Events {
		if !slices.Contains(KnownEvents, e) {
			return fmt.Errorf("%q is not an event this sends; choose from %s",
				e, strings.Join(KnownEvents, ", "))
		}
	}
	if w.MinIntervalSec < 0 {
		return fmt.Errorf("min_interval_sec must not be negative, got %d", w.MinIntervalSec)
	}
	switch w.Format {
	case FormatJSON, FormatCN4M, FormatDiscord:
	default:
		return fmt.Errorf("format must be %q, %q or %q, got %q", FormatJSON, FormatCN4M, FormatDiscord, w.Format)
	}
	return nil
}

// ApplyDefaults fills in what the API lets a caller omit.
func (w *Webhook) ApplyDefaults() {
	w.URL = strings.TrimSpace(w.URL)
	if w.MinIntervalSec == 0 {
		w.MinIntervalSec = 30
	}
	if w.Events == nil {
		w.Events = []string{}
	}
	if w.Format == "" {
		w.Format = FormatJSON
	}
}

const webhookColumns = `id, job_id, url, events_json, secret_encrypted, enabled,
	min_interval_sec, format, created_at, updated_at`

// WebhooksFor returns the enabled hooks that apply to a job: its own, plus the
// global ones.
//
// Secrets come back as ciphertext, like every other secret this package
// handles. Whoever signs holds the box.
func (d *DB) WebhooksFor(ctx context.Context, jobID string) ([]Webhook, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+webhookColumns+` FROM webhooks
		 WHERE enabled = 1 AND (job_id = ? OR job_id IS NULL)
		 ORDER BY created_at, id`, jobID)
	if err != nil {
		return nil, fmt.Errorf("loading the webhooks for job %s: %w", jobID, err)
	}
	defer rows.Close()
	return scanWebhooks(rows)
}

// ListWebhooks returns every webhook, for the UI.
func (d *DB) ListWebhooks(ctx context.Context) ([]Webhook, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+webhookColumns+` FROM webhooks ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("listing the webhooks: %w", err)
	}
	defer rows.Close()
	return scanWebhooks(rows)
}

func scanWebhooks(rows *sql.Rows) ([]Webhook, error) {
	out := []Webhook{}
	for rows.Next() {
		var (
			w                Webhook
			jobID            *string
			eventsJSON       string
			enabled          int
			created, updated string
		)
		if err := rows.Scan(&w.ID, &jobID, &w.URL, &eventsJSON, &w.SecretEncrypted, &enabled,
			&w.MinIntervalSec, &w.Format, &created, &updated); err != nil {
			return nil, fmt.Errorf("reading a webhook: %w", err)
		}
		if jobID != nil {
			w.JobID = *jobID
		}
		if err := json.Unmarshal([]byte(eventsJSON), &w.Events); err != nil {
			return nil, fmt.Errorf("decoding the events of webhook %s: %w", w.ID, err)
		}
		w.Enabled = enabled != 0
		w.CreatedAt, w.UpdatedAt = parseTime(created), parseTime(updated)
		out = append(out, w)
	}
	return out, rows.Err()
}

// CreateWebhook inserts a callback. SecretEncrypted is stored verbatim; the
// caller encrypts.
func (d *DB) CreateWebhook(ctx context.Context, w *Webhook) error {
	w.ApplyDefaults()
	if err := w.Validate(); err != nil {
		return err
	}

	id, err := newID()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	w.ID, w.CreatedAt, w.UpdatedAt = id, now, now

	events, err := json.Marshal(w.Events)
	if err != nil {
		return fmt.Errorf("encoding the events of a webhook: %w", err)
	}
	if _, err := d.sql.ExecContext(ctx,
		`INSERT INTO webhooks (`+webhookColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		w.ID, nullableJobID(w.JobID), w.URL, string(events), w.SecretEncrypted,
		boolToInt(w.Enabled), w.MinIntervalSec, w.Format,
		formatTime(w.CreatedAt), formatTime(w.UpdatedAt),
	); err != nil {
		return fmt.Errorf("creating a webhook: %w", err)
	}
	return nil
}

// UpdateWebhook replaces a callback's settings.
//
// An empty SecretEncrypted means "leave the secret alone", so the UI can save
// a hook it never received the secret for — the same shape target passwords
// use, and for the same reason: a form that must round-trip a secret in order
// to save unrelated fields is a form that leaks it.
func (d *DB) UpdateWebhook(ctx context.Context, id string, w *Webhook) error {
	w.ApplyDefaults()
	if err := w.Validate(); err != nil {
		return err
	}

	events, err := json.Marshal(w.Events)
	if err != nil {
		return fmt.Errorf("encoding the events of a webhook: %w", err)
	}
	w.UpdatedAt = time.Now().UTC()

	res, err := d.sql.ExecContext(ctx,
		`UPDATE webhooks SET job_id=?, url=?, events_json=?, enabled=?, min_interval_sec=?,
		 format=?, secret_encrypted = CASE WHEN ?='' THEN secret_encrypted ELSE ? END, updated_at=?
		 WHERE id=?`,
		nullableJobID(w.JobID), w.URL, string(events), boolToInt(w.Enabled), w.MinIntervalSec,
		w.Format, w.SecretEncrypted, w.SecretEncrypted, formatTime(w.UpdatedAt), id)
	if err != nil {
		return fmt.Errorf("updating webhook %s: %w", id, err)
	}
	w.ID = id
	return checkAffected(res, id)
}

// DeleteWebhook removes a callback.
func (d *DB) DeleteWebhook(ctx context.Context, id string) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM webhooks WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting webhook %s: %w", id, err)
	}
	return checkAffected(res, id)
}

// nullableJobID maps the empty string to SQL NULL, which is how a webhook says
// "every job" rather than naming one.
func nullableJobID(jobID string) any {
	if jobID == "" {
		return nil
	}
	return jobID
}

// EnsureCN4MWebhook creates the suite-reporting callback if there is not one
// already, and reports whether it made one.
//
// **Never overwrites an existing row**, which is the same rule the admin
// password follows and for the same reason: an environment variable left in a
// compose file must not silently undo a deliberate change made in the UI. So
// CN4M_CASCADE_STATUS_URL sets the address a *fresh* installation starts with, and
// after that the row belongs to whoever is running the thing.
//
// Keyed on the format rather than the URL, so a hook someone has already
// re-pointed at a different cn4m still counts as "there is one".
func (d *DB) EnsureCN4MWebhook(ctx context.Context, url string) (bool, error) {
	var existing int
	if err := d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM webhooks WHERE format = ?`, FormatCN4M).Scan(&existing); err != nil {
		return false, fmt.Errorf("checking for a cn4m callback: %w", err)
	}
	if existing > 0 {
		return false, nil
	}

	hook := &Webhook{
		URL:    url,
		Format: FormatCN4M,
		// Global: every job's status belongs in the suite view, and a
		// per-job opt-in would mean a job added later silently stops
		// reporting.
		JobID: "",
		// `progress` is included because it is the line the suite view is
		// actually read for: "Sync in Progress: 65%, 910 MB/s" (D-128). It was
		// omitted while a progress message said "Syncing <32-hex-id> — 3/5
		// files", which was noise worth suppressing; that is no longer what it
		// says. Delivery is throttled per webhook by min_interval_sec, so
		// subscribing does not mean one callback per second.
		Events: []string{"run_started", "progress", "run_completed", "run_failed"},
		// Five seconds, not the generic 30. This row exists to drive a live
		// status bar, and the default throttle is measured against the length
		// of a run: a sync that finishes in twenty seconds sends one progress
		// update and then nothing, so the bar shows a number once and freezes.
		// A suite dashboard is the one subscriber where progress is the point.
		MinIntervalSec: 5,
		Enabled:        true,
	}
	if err := d.CreateWebhook(ctx, hook); err != nil {
		return false, err
	}
	return true, nil
}

// EnsureDiscordWebhook seeds a Discord notification on a fresh database, from
// CN4M_CASCADE_DISCORD_WEBHOOK.
//
// The same contract as EnsureCN4MWebhook, for the same reasons: keyed on the
// format so a row someone has re-pointed still counts as "there is one", global
// rather than per-job so a job added later does not silently stop reporting,
// and it **never overwrites**, so an environment variable cannot undo a change
// made in the UI.
//
// It subscribes to **run_completed and run_failed only**. A chat channel is read
// by people: `progress` would post a line every interval for the length of every
// run, and `run_started` doubles the traffic to say something the completion
// message already implies. The outcome is what somebody wants to see.
func (d *DB) EnsureDiscordWebhook(ctx context.Context, url string) (bool, error) {
	var existing int
	if err := d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM webhooks WHERE format = ?`, FormatDiscord).Scan(&existing); err != nil {
		return false, fmt.Errorf("checking for a Discord notification: %w", err)
	}
	if existing > 0 {
		return false, nil
	}

	hook := &Webhook{
		URL:     url,
		Format:  FormatDiscord,
		JobID:   "",
		Events:  []string{"run_completed", "run_failed"},
		Enabled: true,
	}
	if err := d.CreateWebhook(ctx, hook); err != nil {
		return false, err
	}
	return true, nil
}

// CN4MWebhookURL returns the URL of the stored cn4m callback, if there is one.
//
// Exists so startup can compare what is configured against what is stored.
// EnsureCN4MWebhook never overwrites — an environment variable must not undo a
// deliberate change made in the UI — but that rule has a sharp edge: a database
// copied from another deployment carries that deployment's URL, and changing
// the variable afterwards does nothing at all. Silently.
func (d *DB) CN4MWebhookURL(ctx context.Context) (string, bool, error) {
	var url string
	err := d.sql.QueryRowContext(ctx,
		`SELECT url FROM webhooks WHERE format = ? LIMIT 1`, FormatCN4M).Scan(&url)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading the cn4m callback: %w", err)
	}
	return url, true, nil
}
