package store

import (
	"context"
	"testing"
	"time"
)

func TestAdminPassword(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	set, err := db.AdminPasswordSet(ctx)
	if err != nil {
		t.Fatalf("AdminPasswordSet: %v", err)
	}
	if set {
		t.Fatal("a fresh database reports an admin password already set")
	}

	if _, err := db.VerifyAdminPassword(ctx, "anything"); err == nil {
		t.Fatal("verifying against an unset password should report that it is unset")
	}

	if err := db.SetAdminPassword(ctx, "short"); err == nil {
		t.Fatal("an 8-character minimum should reject \"short\"")
	}
	if err := db.SetAdminPassword(ctx, "correct horse battery"); err != nil {
		t.Fatalf("SetAdminPassword: %v", err)
	}

	tests := []struct {
		name     string
		password string
		want     bool
	}{
		{"the right password", "correct horse battery", true},
		{"the wrong password", "correct horse batteru", false},
		{"empty", "", false},
		{"a prefix of the right password", "correct horse", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := db.VerifyAdminPassword(ctx, tc.password)
			if err != nil {
				t.Fatalf("VerifyAdminPassword: %v", err)
			}
			if got != tc.want {
				t.Fatalf("VerifyAdminPassword(%q) = %v, want %v", tc.password, got, tc.want)
			}
		})
	}

	// The stored value must not be the password itself, in any encoding.
	stored, _, err := db.GetSetting(ctx, settingPasswordHash)
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if stored == "correct horse battery" || stored == "" {
		t.Fatalf("the stored hash looks like the password itself: %q", stored)
	}

	// Changing the password invalidates the old one.
	if err := db.SetAdminPassword(ctx, "a different password"); err != nil {
		t.Fatalf("SetAdminPassword: %v", err)
	}
	if ok, _ := db.VerifyAdminPassword(ctx, "correct horse battery"); ok {
		t.Fatal("the old password still verifies after a change")
	}
}

func TestSessions(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	token, expires, err := db.CreateSession(ctx)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if token == "" {
		t.Fatal("CreateSession returned an empty token")
	}
	if !expires.After(time.Now().UTC()) {
		t.Fatalf("session expires in the past: %v", expires)
	}

	// The raw token must never be what is stored: a stolen database must not
	// yield live sessions.
	var storedHash string
	if err := db.sql.QueryRowContext(ctx, `SELECT token_hash FROM sessions`).Scan(&storedHash); err != nil {
		t.Fatalf("reading the session row: %v", err)
	}
	if storedHash == token {
		t.Fatal("the sessions table stores the raw token")
	}

	ok, err := db.LookupSession(ctx, token)
	if err != nil {
		t.Fatalf("LookupSession: %v", err)
	}
	if !ok {
		t.Fatal("a just-created session does not look up")
	}

	for _, bogus := range []string{"", "not-a-token", token + "x"} {
		if ok, _ := db.LookupSession(ctx, bogus); ok {
			t.Fatalf("LookupSession(%q) accepted a token it should not", bogus)
		}
	}

	if err := db.DeleteSession(ctx, token); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if ok, _ := db.LookupSession(ctx, token); ok {
		t.Fatal("a deleted session still authenticates")
	}
}

// An expired row must not authenticate even when it is still present: the
// purge is housekeeping, never the thing that enforces expiry.
func TestExpiredSessionDoesNotAuthenticate(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	token, _, err := db.CreateSession(ctx)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	past := time.Now().UTC().Add(-time.Minute)
	if _, err := db.sql.ExecContext(ctx,
		`UPDATE sessions SET expires_at = ?`, formatTime(past)); err != nil {
		t.Fatalf("expiring the session: %v", err)
	}

	if ok, _ := db.LookupSession(ctx, token); ok {
		t.Fatal("an expired session still authenticates")
	}

	n, err := db.PurgeExpiredSessions(ctx)
	if err != nil {
		t.Fatalf("PurgeExpiredSessions: %v", err)
	}
	if n != 1 {
		t.Fatalf("PurgeExpiredSessions removed %d rows, want 1", n)
	}
}

func TestDeleteAllSessions(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	var tokens []string
	for range 3 {
		token, _, err := db.CreateSession(ctx)
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		tokens = append(tokens, token)
	}

	if err := db.DeleteAllSessions(ctx); err != nil {
		t.Fatalf("DeleteAllSessions: %v", err)
	}
	for _, token := range tokens {
		if ok, _ := db.LookupSession(ctx, token); ok {
			t.Fatal("a session survived DeleteAllSessions")
		}
	}
}
