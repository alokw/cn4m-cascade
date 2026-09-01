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

// TargetType distinguishes the two v1 storage backends.
type TargetType string

const (
	TargetSMB   TargetType = "smb"
	TargetLocal TargetType = "local"
)

// Target is a configured source or destination location.
type Target struct {
	ID   string     `json:"id"`
	Name string     `json:"name"`
	Type TargetType `json:"type"`

	// SMB fields.
	Host              string `json:"host,omitempty"`
	Share             string `json:"share,omitempty"`
	Port              int    `json:"port,omitempty"`
	Username          string `json:"username,omitempty"`
	Domain            string `json:"domain,omitempty"`
	MountOptsOverride string `json:"mount_opts_override,omitempty"`
	Multichannel      bool   `json:"multichannel"`
	NegotiatedVers    string `json:"negotiated_vers,omitempty"`

	// Local field.
	LocalPath string `json:"local_path,omitempty"`

	// Subpath is a path *below* the share root or local path, applied when a
	// job uses this target.
	Subpath string `json:"subpath,omitempty"`

	// PasswordEncrypted never leaves the process in cleartext and is never
	// serialised to API responses.
	PasswordEncrypted string `json:"-"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// UNCPath renders the share as //host/share, for display and mount sources.
func (t *Target) UNCPath() string {
	if t.Type != TargetSMB {
		return t.LocalPath
	}
	return "//" + t.Host + "/" + t.Share
}

// Describe is the phrase used in user-facing errors: legible, and enough to
// identify which target went wrong.
func (t *Target) Describe() string {
	if t.Type == TargetSMB {
		return fmt.Sprintf("%q (%s)", t.Name, t.UNCPath())
	}
	return fmt.Sprintf("%q (%s)", t.Name, t.LocalPath)
}

// IsGuest reports whether the target authenticates anonymously. An empty
// username means guest (PROGRESS.md D-3).
func (t *Target) IsGuest() bool { return t.Username == "" }

// Validate checks a target for internal consistency. Its messages are shown
// to users, so they name the field and say what is wrong with it.
func (t *Target) Validate() error {
	if strings.TrimSpace(t.Name) == "" {
		return errors.New("name is required")
	}
	if t.Subpath != "" && path.IsAbs(t.Subpath) {
		return errors.New("subpath must be relative to the share root, not an absolute path")
	}
	for _, segment := range strings.Split(t.Subpath, "/") {
		if segment == ".." {
			return errors.New("subpath must not contain \"..\" path segments")
		}
	}
	if err := validateMountOpts(t.MountOptsOverride); err != nil {
		return err
	}

	switch t.Type {
	case TargetSMB:
		if strings.TrimSpace(t.Host) == "" {
			return errors.New("host is required for an SMB target")
		}
		// Hostnames are accepted, but an IP is the documented case and the
		// only one guaranteed to work under network_mode: host.
		if strings.ContainsAny(t.Host, `/\ `) {
			return fmt.Errorf("host %q must be an IP address or hostname, without slashes", t.Host)
		}
		if strings.TrimSpace(t.Share) == "" {
			return errors.New("share is required for an SMB target")
		}
		if strings.ContainsAny(t.Share, `/\`) {
			return fmt.Errorf("share %q must be a single share name, without slashes", t.Share)
		}
		if t.Port < 0 || t.Port > 65535 {
			return fmt.Errorf("port %d is out of range (1-65535, or 0 for the default)", t.Port)
		}
		if t.Username == "" && t.PasswordEncrypted != "" {
			return errors.New("a password was set without a username: clear the password to mount as guest, or set the username it belongs to")
		}
		if t.LocalPath != "" {
			return errors.New("local_path is only valid for a local target")
		}
	case TargetLocal:
		if strings.TrimSpace(t.LocalPath) == "" {
			return errors.New("local_path is required for a local target")
		}
		if !path.IsAbs(t.LocalPath) {
			return fmt.Errorf("local_path %q must be an absolute path inside the container", t.LocalPath)
		}
		if t.Host != "" || t.Share != "" || t.Username != "" {
			return errors.New("host, share and username are only valid for an SMB target")
		}
	default:
		return fmt.Errorf("type must be %q or %q, got %q", TargetSMB, TargetLocal, t.Type)
	}
	return nil
}

// forbiddenMountOpts cannot be set through the advanced options field.
//
//   - credentials/password: CLAUDE.md and SPEC.md §5 require credentials to
//     reach mount.cifs only through a 0600 file. An option here goes into
//     argv, where `ps` exposes it and where our own failure reporting would
//     copy it into logs and API responses.
//   - hard: SPEC.md §5 depends on `soft` to make I/O against a dead server
//     return errors instead of hanging forever.
//   - sharesock: undoes the separate session that keeps two targets on one
//     share from sharing each other's credentials.
var forbiddenMountOpts = map[string]string{
	"password":    "set the password in the target's own password field, where it is stored encrypted and passed to mount.cifs through a private file",
	"pass":        "set the password in the target's own password field, where it is stored encrypted and passed to mount.cifs through a private file",
	"credentials": "credentials are supplied automatically from the target's own username and password",
	"cred":        "credentials are supplied automatically from the target's own username and password",
	"hard":        "it makes I/O against an unreachable share hang indefinitely instead of failing, which this tool relies on not happening",
	"sharesock":   "it lets two targets on the same share share one session, so one target's credentials could be used for another's transfers",
}

// validateMountOpts rejects advanced options that would defeat a guarantee
// the rest of the system depends on.
func validateMountOpts(override string) error {
	for _, part := range strings.Split(override, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, _, _ := strings.Cut(part, "=")
		key = strings.ToLower(strings.TrimSpace(key))

		if reason, forbidden := forbiddenMountOpts[key]; forbidden {
			return fmt.Errorf("mount option %q is not allowed: %s", key, reason)
		}
	}
	return nil
}

const targetColumns = `id, name, type, host, share, subpath, local_path, port, username,
	password_encrypted, domain, mount_opts_override, multichannel, negotiated_vers,
	created_at, updated_at`

// CreateTarget inserts a target, assigning its ID and timestamps.
func (d *DB) CreateTarget(ctx context.Context, t *Target) error {
	if err := t.Validate(); err != nil {
		return err
	}
	id, err := newID()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	t.ID, t.CreatedAt, t.UpdatedAt = id, now, now

	_, err = d.sql.ExecContext(ctx, `INSERT INTO targets (`+targetColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Name, string(t.Type), t.Host, t.Share, t.Subpath, t.LocalPath, t.Port,
		t.Username, t.PasswordEncrypted, t.Domain, t.MountOptsOverride,
		boolToInt(t.Multichannel), t.NegotiatedVers,
		formatTime(t.CreatedAt), formatTime(t.UpdatedAt))
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("a target named %q already exists: %w", t.Name, ErrNameTaken)
		}
		return fmt.Errorf("creating target %q: %w", t.Name, err)
	}
	return nil
}

// GetTarget loads one target by ID.
func (d *DB) GetTarget(ctx context.Context, id string) (*Target, error) {
	row := d.sql.QueryRowContext(ctx, `SELECT `+targetColumns+` FROM targets WHERE id = ?`, id)
	t, err := scanTarget(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("target %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("loading target %s: %w", id, err)
	}
	return t, nil
}

// ListTargets returns every target, newest first.
func (d *DB) ListTargets(ctx context.Context) ([]*Target, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+targetColumns+` FROM targets ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("listing targets: %w", err)
	}
	defer rows.Close()

	targets := []*Target{}
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, fmt.Errorf("listing targets: %w", err)
		}
		targets = append(targets, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing targets: %w", err)
	}
	return targets, nil
}

// UpdateTarget writes every mutable column of t.
func (d *DB) UpdateTarget(ctx context.Context, t *Target) error {
	if err := t.Validate(); err != nil {
		return err
	}
	t.UpdatedAt = time.Now().UTC()

	res, err := d.sql.ExecContext(ctx, `UPDATE targets SET
		name=?, type=?, host=?, share=?, subpath=?, local_path=?, port=?, username=?,
		password_encrypted=?, domain=?, mount_opts_override=?, multichannel=?,
		negotiated_vers=?, updated_at=? WHERE id=?`,
		t.Name, string(t.Type), t.Host, t.Share, t.Subpath, t.LocalPath, t.Port, t.Username,
		t.PasswordEncrypted, t.Domain, t.MountOptsOverride, boolToInt(t.Multichannel),
		t.NegotiatedVers, formatTime(t.UpdatedAt), t.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("a target named %q already exists: %w", t.Name, ErrNameTaken)
		}
		return fmt.Errorf("updating target %s: %w", t.ID, err)
	}
	return checkAffected(res, t.ID)
}

// SetNegotiatedVers records which SMB dialect actually mounted (SPEC.md §5).
func (d *DB) SetNegotiatedVers(ctx context.Context, id, vers string) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE targets SET negotiated_vers=?, updated_at=? WHERE id=?`,
		vers, formatTime(time.Now().UTC()), id)
	if err != nil {
		return fmt.Errorf("recording negotiated SMB version for target %s: %w", id, err)
	}
	return nil
}

// DeleteTarget removes a target.
func (d *DB) DeleteTarget(ctx context.Context, id string) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM targets WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting target %s: %w", id, err)
	}
	return checkAffected(res, id)
}

func checkAffected(res sql.Result, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("target %s: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("target %s: %w", id, ErrNotFound)
	}
	return nil
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

func scanTarget(s scanner) (*Target, error) {
	var (
		t                     Target
		typ, created, updated string
		multichannel          int
	)
	err := s.Scan(&t.ID, &t.Name, &typ, &t.Host, &t.Share, &t.Subpath, &t.LocalPath, &t.Port,
		&t.Username, &t.PasswordEncrypted, &t.Domain, &t.MountOptsOverride, &multichannel,
		&t.NegotiatedVers, &created, &updated)
	if err != nil {
		return nil, err
	}
	t.Type = TargetType(typ)
	t.Multichannel = multichannel != 0
	t.CreatedAt = parseTime(created)
	t.UpdatedAt = parseTime(updated)
	return &t, nil
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	// datetime('now') output, used by schema_migrations.
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t
	}
	return time.Time{}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// isUniqueViolation detects a UNIQUE constraint failure without depending on
// the driver's error type.
func isUniqueViolation(err error) bool {
	return strings.Contains(strings.ToUpper(err.Error()), "UNIQUE CONSTRAINT FAILED")
}
