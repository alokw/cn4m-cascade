package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// seedTriggerJob builds a minimal valid job with two distinct local targets.
func seedTriggerJob(t *testing.T, db *DB, name string) *Job {
	t.Helper()
	ctx := context.Background()

	src := &Target{Name: name + "-src", Type: TargetLocal, LocalPath: t.TempDir()}
	if err := db.CreateTarget(ctx, src); err != nil {
		t.Fatalf("creating the source: %v", err)
	}
	dst := &Target{Name: name + "-dst", Type: TargetLocal, LocalPath: t.TempDir()}
	if err := db.CreateTarget(ctx, dst); err != nil {
		t.Fatalf("creating the destination: %v", err)
	}

	job := &Job{
		Name: name, SourceTargetID: src.ID, Mode: ModeMirror, Enabled: true,
		Destinations: []JobDestination{{DestTargetID: dst.ID}},
	}
	if err := db.CreateJob(ctx, job); err != nil {
		t.Fatalf("creating the job: %v", err)
	}
	return job
}

func TestTriggerTokenLifecycle(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	job := seedTriggerJob(t, db, "hooked")

	// A job starts with no token, so its hook endpoints refuse everything.
	if has, err := db.HasTriggerToken(ctx, job.ID); err != nil || has {
		t.Fatalf("a new job already has a token (%v, %v)", has, err)
	}

	token, err := db.IssueTriggerToken(ctx, job.ID)
	if err != nil {
		t.Fatalf("issuing: %v", err)
	}
	if len(token) < 40 {
		t.Fatalf("token is only %d characters; 32 random bytes should be longer", len(token))
	}

	found, err := db.JobForTriggerToken(ctx, token)
	if err != nil {
		t.Fatalf("looking up a freshly issued token: %v", err)
	}
	if found.ID != job.ID {
		t.Fatalf("token resolved to job %s, want %s", found.ID, job.ID)
	}
	if !found.HasTriggerToken {
		t.Fatal("the job does not report having a token it was just issued")
	}

	// Regenerating must invalidate the previous token in the same act.
	// A regenerate that leaves the old one working is worse than none at all,
	// because it looks like the problem was dealt with.
	second, err := db.IssueTriggerToken(ctx, job.ID)
	if err != nil {
		t.Fatalf("regenerating: %v", err)
	}
	if second == token {
		t.Fatal("regenerating returned the same token")
	}
	if _, err := db.JobForTriggerToken(ctx, token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the old token still works after regenerating: %v", err)
	}
	if _, err := db.JobForTriggerToken(ctx, second); err != nil {
		t.Fatalf("the new token does not work: %v", err)
	}

	// Revoking must actually revoke.
	if err := db.RevokeTriggerToken(ctx, job.ID); err != nil {
		t.Fatalf("revoking: %v", err)
	}
	if _, err := db.JobForTriggerToken(ctx, second); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a revoked token still works: %v", err)
	}
	if has, err := db.HasTriggerToken(ctx, job.ID); err != nil || has {
		t.Fatalf("the job still reports a token after revocation (%v, %v)", has, err)
	}

	// Revoking again is not an error: the caller's intent is already satisfied.
	if err := db.RevokeTriggerToken(ctx, job.ID); err != nil {
		t.Fatalf("revoking twice: %v", err)
	}
}

// The token must not be recoverable from the database, only verifiable.
// This is the property the whole "shown once" design rests on, so it is
// asserted directly rather than assumed from the absence of a getter.
func TestTriggerTokenIsStoredOnlyAsAHash(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	job := seedTriggerJob(t, db, "hash-only")

	token, err := db.IssueTriggerToken(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}

	var stored string
	if err := db.sql.QueryRowContext(ctx,
		`SELECT api_trigger_token_hash FROM jobs WHERE id = ?`, job.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == token {
		t.Fatal("the token is stored in the clear")
	}
	if strings.Contains(stored, token) {
		t.Fatal("the stored value contains the token")
	}
	if stored != hashToken(token) {
		t.Fatalf("stored value is not the SHA-256 of the token")
	}

	// And nothing that reads a job carries it into Go.
	loaded, err := db.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.HasTriggerToken {
		t.Fatal("HasTriggerToken is false for a job that has one")
	}
}

// An unknown token and a nonexistent job are the same answer. Distinguishing
// them would tell an anonymous prober which guesses are getting warmer.
func TestUnknownTriggerTokenIsIndistinguishableFromNoJob(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	seedTriggerJob(t, db, "quiet")

	for _, token := range []string{"", "not-a-real-token", strings.Repeat("A", 43)} {
		if _, err := db.JobForTriggerToken(ctx, token); !errors.Is(err, ErrNotFound) {
			t.Fatalf("token %q gave %v, want ErrNotFound", token, err)
		}
	}
}

// The cn4m callback is created once and then belongs to whoever runs the
// installation.
//
// The rule matters because CN4M_CASCADE_STATUS_URL lives in a compose file: if it
// re-applied on every start, someone who re-pointed the callback in the UI
// would find it silently reverted at the next restart. Same reasoning as the
// admin password env var, which never overwrites an existing password.
func TestEnsureCN4MWebhookSeedsOnceAndNeverOverwrites(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	created, err := db.EnsureCN4MWebhook(ctx, "http://cn4m.example.lan:2640/suite/status")
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if !created {
		t.Fatal("nothing was created on a fresh database")
	}

	hooks, err := db.ListWebhooks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(hooks) != 1 {
		t.Fatalf("got %d callbacks, want 1", len(hooks))
	}
	if hooks[0].Format != FormatCN4M {
		t.Fatalf("format = %q, want %q", hooks[0].Format, FormatCN4M)
	}
	if hooks[0].JobID != "" {
		t.Fatalf("the suite callback is scoped to job %q; it should apply to every job", hooks[0].JobID)
	}
	if !hooks[0].Enabled {
		t.Fatal("the suite callback was created disabled")
	}

	// A second start with a *different* address must change nothing.
	created, err = db.EnsureCN4MWebhook(ctx, "http://somewhere-else:2640/suite/status")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("a second callback was created")
	}

	hooks, err = db.ListWebhooks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(hooks) != 1 {
		t.Fatalf("got %d callbacks after restart, want 1", len(hooks))
	}
	if hooks[0].URL != "http://cn4m.example.lan:2640/suite/status" {
		t.Fatalf("the environment variable overwrote the stored URL: %q", hooks[0].URL)
	}

	// And a callback someone re-pointed by hand still counts as "there is
	// one", because the check is on format rather than address.
	edited := hooks[0]
	edited.URL = "http://hand-edited:2640/suite/status"
	if err := db.UpdateWebhook(ctx, edited.ID, &edited); err != nil {
		t.Fatal(err)
	}
	if created, err := db.EnsureCN4MWebhook(ctx, DefaultTestURL); err != nil || created {
		t.Fatalf("a second callback appeared after the first was re-pointed (created=%v, err=%v)", created, err)
	}
}

// DefaultTestURL stands in for the configured default in the test above.
const DefaultTestURL = "http://localhost:2640/suite/status"
