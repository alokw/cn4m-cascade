package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// IssueTriggerToken mints a new token for a job and returns it in the clear,
// once.
//
// Only the SHA-256 hash is stored — `hashToken`, the same function sessions
// use. Nothing can recover the token afterwards, which is deliberate: the
// database then holds no credential that could start a run, and the cost is
// that regenerating is the only remedy for a lost one.
//
// Issuing over an existing token replaces it, so the previous one stops
// working in the same statement. That is what makes "regenerate" mean
// something rather than merely adding another key to the door.
func (d *DB) IssueTriggerToken(ctx context.Context, jobID string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating a trigger token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	res, err := d.sql.ExecContext(ctx,
		`UPDATE jobs SET api_trigger_token_hash = ?, updated_at = ? WHERE id = ?`,
		hashToken(token), formatTime(time.Now().UTC()), jobID)
	if err != nil {
		return "", fmt.Errorf("storing the trigger token of job %s: %w", jobID, err)
	}
	if err := checkAffected(res, jobID); err != nil {
		return "", err
	}
	return token, nil
}

// RevokeTriggerToken removes a job's token. Idempotent: revoking a job that
// has none is not an error, because the caller's intent — "this must not be
// triggerable" — is satisfied either way.
func (d *DB) RevokeTriggerToken(ctx context.Context, jobID string) error {
	res, err := d.sql.ExecContext(ctx,
		`UPDATE jobs SET api_trigger_token_hash = NULL, updated_at = ? WHERE id = ?`,
		formatTime(time.Now().UTC()), jobID)
	if err != nil {
		return fmt.Errorf("revoking the trigger token of job %s: %w", jobID, err)
	}
	return checkAffected(res, jobID)
}

// HasTriggerToken reports whether a job can be triggered, without revealing
// anything about the token itself. This is what the UI asks.
func (d *DB) HasTriggerToken(ctx context.Context, jobID string) (bool, error) {
	var present int
	err := d.sql.QueryRowContext(ctx,
		`SELECT api_trigger_token_hash IS NOT NULL FROM jobs WHERE id = ?`, jobID).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("job %s: %w", jobID, ErrNotFound)
	}
	if err != nil {
		return false, fmt.Errorf("checking the trigger token of job %s: %w", jobID, err)
	}
	return present != 0, nil
}

// JobForTriggerToken finds the job a token authorises.
//
// The lookup is by hash, so the token is never compared byte by byte and there
// is no timing signal in the comparison — SQLite matches a fixed-width hex
// string through a unique index. An unknown token is ErrNotFound, and callers
// must not distinguish that from "job exists but token is wrong": to an
// anonymous caller both are simply "no".
func (d *DB) JobForTriggerToken(ctx context.Context, token string) (*Job, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	var id string
	err := d.sql.QueryRowContext(ctx,
		`SELECT id FROM jobs WHERE api_trigger_token_hash = ?`, hashToken(token)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("looking up a trigger token: %w", err)
	}
	return d.GetJob(ctx, id)
}
