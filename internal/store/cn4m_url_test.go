package store

import (
	"context"
	"testing"
)

// A database carried from another deployment keeps that deployment's callback
// URL, and the environment variable then does nothing — silently, because
// delivery is best-effort. Startup compares the two, so the accessor has to
// report what is actually stored.
func TestCN4MWebhookURLReportsWhatIsStored(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if _, ok, err := db.CN4MWebhookURL(ctx); err != nil || ok {
		t.Fatalf("with no callback: ok = %v, err = %v; want false, nil", ok, err)
	}

	const seeded = "http://host.docker.internal:2640/suite/status"
	if created, err := db.EnsureCN4MWebhook(ctx, seeded); err != nil || !created {
		t.Fatalf("seeding: created = %v, err = %v", created, err)
	}

	got, ok, err := db.CN4MWebhookURL(ctx)
	if err != nil || !ok {
		t.Fatalf("after seeding: ok = %v, err = %v", ok, err)
	}
	if got != seeded {
		t.Fatalf("url = %q, want %q", got, seeded)
	}

	// The rule that makes the warning necessary: a second Ensure with a
	// different URL must NOT change the stored row.
	if created, err := db.EnsureCN4MWebhook(ctx, "http://localhost:2640/suite/status"); err != nil || created {
		t.Fatalf("re-seeding: created = %v, err = %v; want false, nil", created, err)
	}
	got, _, _ = db.CN4MWebhookURL(ctx)
	if got != seeded {
		t.Fatalf("url = %q after re-seeding, want it unchanged at %q", got, seeded)
	}
}
