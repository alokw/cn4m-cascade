package store

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Admin password storage (SPEC.md §8: "single admin password (env var or
// first-run setup), session cookie").
//
// PBKDF2-HMAC-SHA256 from the standard library rather than bcrypt or argon2:
// both would pull in golang.org/x/crypto for one function, and the cgo-free,
// dependency-light build is a stated goal (SPEC.md §3.1). The iteration count
// is stored alongside the hash so it can be raised later without invalidating
// existing passwords.
const (
	settingPasswordHash = "admin_password_hash"
	settingPasswordSalt = "admin_password_salt"
	settingPasswordIter = "admin_password_iter"

	// pbkdf2Iterations follows the OWASP guidance for PBKDF2-HMAC-SHA256.
	pbkdf2Iterations = 600_000
	pbkdf2KeyLen     = 32
	saltLen          = 16
)

// ErrNoAdminPassword means first-run setup has not happened yet.
var ErrNoAdminPassword = errors.New("no admin password has been set")

// GetSetting reads one settings row. Missing keys return ok=false rather than
// an error: absence is a normal state for every setting here.
func (d *DB) GetSetting(ctx context.Context, key string) (value string, ok bool, err error) {
	err = d.sql.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading setting %q: %w", key, err)
	}
	return value, true, nil
}

// SetSetting writes one settings row.
func (d *DB) SetSetting(ctx context.Context, key, value string) error {
	if _, err := d.sql.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
		return fmt.Errorf("writing setting %q: %w", key, err)
	}
	return nil
}

// AdminPasswordSet reports whether first-run setup has happened.
func (d *DB) AdminPasswordSet(ctx context.Context) (bool, error) {
	_, ok, err := d.GetSetting(ctx, settingPasswordHash)
	return ok, err
}

// SetAdminPassword hashes and stores the admin password. Setting it again
// changes it; callers are responsible for requiring the old one first.
//
// It does not invalidate existing sessions — see DeleteAllSessions, which the
// password-change path calls so a changed password logs everyone out.
func (d *DB) SetAdminPassword(ctx context.Context, password string) error {
	if len(password) < 8 {
		return errors.New("the admin password must be at least 8 characters")
	}

	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("generating a password salt: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, pbkdf2KeyLen)
	if err != nil {
		return fmt.Errorf("hashing the admin password: %w", err)
	}

	// All three land together or none do. A crash between the salt write and
	// the hash write would otherwise pair a new salt with the old hash, so
	// neither the old nor the new password verifies — and since
	// AdminPasswordSet still reports true, first-run setup stays closed and
	// the instance can only be recovered by editing SQLite by hand.
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storing the admin password: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const upsert = `INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`
	for _, kv := range [][2]string{
		{settingPasswordSalt, base64.StdEncoding.EncodeToString(salt)},
		{settingPasswordIter, fmt.Sprint(pbkdf2Iterations)},
		{settingPasswordHash, base64.StdEncoding.EncodeToString(key)},
	} {
		if _, err := tx.ExecContext(ctx, upsert, kv[0], kv[1]); err != nil {
			return fmt.Errorf("writing setting %q: %w", kv[0], err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storing the admin password: %w", err)
	}
	return nil
}

// VerifyAdminPassword reports whether a password matches. A wrong password is
// (false, nil); only a broken stored hash is an error.
func (d *DB) VerifyAdminPassword(ctx context.Context, password string) (bool, error) {
	encHash, ok, err := d.GetSetting(ctx, settingPasswordHash)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, ErrNoAdminPassword
	}
	encSalt, _, err := d.GetSetting(ctx, settingPasswordSalt)
	if err != nil {
		return false, err
	}
	encIter, _, err := d.GetSetting(ctx, settingPasswordIter)
	if err != nil {
		return false, err
	}

	want, err := base64.StdEncoding.DecodeString(encHash)
	if err != nil {
		return false, fmt.Errorf("the stored admin password hash is corrupt: %w", err)
	}
	salt, err := base64.StdEncoding.DecodeString(encSalt)
	if err != nil {
		return false, fmt.Errorf("the stored admin password salt is corrupt: %w", err)
	}
	iter := pbkdf2Iterations
	if _, err := fmt.Sscanf(encIter, "%d", &iter); err != nil || iter <= 0 {
		return false, fmt.Errorf("the stored admin password iteration count is corrupt: %q", encIter)
	}

	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false, fmt.Errorf("hashing the admin password: %w", err)
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// Session lifetime. Long enough not to interrupt a working session, short
// enough that a forgotten browser tab does not stay authenticated forever.
const SessionTTL = 7 * 24 * time.Hour

// hashToken is what the sessions table stores. The raw token only ever exists
// in the cookie: a stolen database must not yield live sessions, the same
// reasoning that encrypts target passwords (SPEC.md §5).
//
// A plain SHA-256 is right here where it would be wrong for a password: the
// token is 256 bits of entropy from crypto/rand, so there is no dictionary to
// attack and nothing for a slow KDF to buy.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateSession issues a session and returns the raw token, which the caller
// puts in a cookie and never stores.
func (d *DB) CreateSession(ctx context.Context) (token string, expiresAt time.Time, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("generating a session token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)

	now := time.Now().UTC()
	expiresAt = now.Add(SessionTTL)
	if _, err := d.sql.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, created_at, expires_at, last_seen_at) VALUES (?,?,?,?)`,
		hashToken(token), formatTime(now), formatTime(expiresAt), formatTime(now)); err != nil {
		return "", time.Time{}, fmt.Errorf("creating a session: %w", err)
	}
	return token, expiresAt, nil
}

// LookupSession reports whether a token names a live session, refreshing its
// last-seen time. An expired row is not valid even if it is still present, so
// a lapsed purge can never extend a session.
func (d *DB) LookupSession(ctx context.Context, token string) (bool, error) {
	if token == "" {
		return false, nil
	}
	var expires string
	err := d.sql.QueryRowContext(ctx,
		`SELECT expires_at FROM sessions WHERE token_hash = ?`, hashToken(token)).Scan(&expires)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("looking up a session: %w", err)
	}
	if !time.Now().UTC().Before(parseTime(expires)) {
		return false, nil
	}

	// Refreshing last-seen is bookkeeping: the session is already known to be
	// valid, so a failed write must not turn into a failed authentication.
	_, _ = d.sql.ExecContext(ctx,
		`UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?`,
		formatTime(time.Now().UTC()), hashToken(token))
	return true, nil
}

// DeleteSession logs one session out.
func (d *DB) DeleteSession(ctx context.Context, token string) error {
	if _, err := d.sql.ExecContext(ctx,
		`DELETE FROM sessions WHERE token_hash = ?`, hashToken(token)); err != nil {
		return fmt.Errorf("deleting a session: %w", err)
	}
	return nil
}

// DeleteAllSessions logs everyone out. Used when the password changes.
func (d *DB) DeleteAllSessions(ctx context.Context) error {
	if _, err := d.sql.ExecContext(ctx, `DELETE FROM sessions`); err != nil {
		return fmt.Errorf("deleting sessions: %w", err)
	}
	return nil
}

// PurgeExpiredSessions removes rows that can no longer authenticate anything.
func (d *DB) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	res, err := d.sql.ExecContext(ctx,
		`DELETE FROM sessions WHERE expires_at <= ?`, formatTime(time.Now().UTC()))
	if err != nil {
		return 0, fmt.Errorf("purging expired sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("purging expired sessions: %w", err)
	}
	return n, nil
}
