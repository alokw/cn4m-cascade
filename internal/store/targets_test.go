package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	for attempt := 1; attempt <= 3; attempt++ {
		db, err := Open(context.Background(), path)
		if err != nil {
			t.Fatalf("Open attempt %d: %v", attempt, err)
		}
		if _, err := db.ListTargets(context.Background()); err != nil {
			t.Fatalf("ListTargets on attempt %d: %v", attempt, err)
		}
		db.Close()
	}
}

func TestTargetRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	original := &Target{
		Name:              "NAS-A",
		Type:              TargetSMB,
		Host:              "192.168.1.50",
		Share:             "media",
		Subpath:           "photos/2026",
		Port:              4450,
		Username:          "syncuser",
		PasswordEncrypted: "ZW5jcnlwdGVk",
		Domain:            "WORKGROUP",
		MountOptsOverride: "cache=none",
		Multichannel:      true,
	}
	if err := db.CreateTarget(ctx, original); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	if original.ID == "" {
		t.Fatal("CreateTarget did not assign an ID")
	}
	if original.CreatedAt.IsZero() {
		t.Error("CreateTarget did not set created_at")
	}

	loaded, err := db.GetTarget(ctx, original.ID)
	if err != nil {
		t.Fatalf("GetTarget: %v", err)
	}
	for _, field := range []struct {
		name      string
		got, want any
	}{
		{"name", loaded.Name, original.Name},
		{"type", loaded.Type, original.Type},
		{"host", loaded.Host, original.Host},
		{"share", loaded.Share, original.Share},
		{"subpath", loaded.Subpath, original.Subpath},
		{"port", loaded.Port, original.Port},
		{"username", loaded.Username, original.Username},
		{"password_encrypted", loaded.PasswordEncrypted, original.PasswordEncrypted},
		{"domain", loaded.Domain, original.Domain},
		{"mount_opts_override", loaded.MountOptsOverride, original.MountOptsOverride},
		{"multichannel", loaded.Multichannel, original.Multichannel},
	} {
		if field.got != field.want {
			t.Errorf("%s = %v, want %v", field.name, field.got, field.want)
		}
	}
}

func TestTargetNameIsUnique(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	first := &Target{Name: "NAS-A", Type: TargetSMB, Host: "10.0.0.1", Share: "media"}
	if err := db.CreateTarget(ctx, first); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}

	second := &Target{Name: "NAS-A", Type: TargetSMB, Host: "10.0.0.2", Share: "backup"}
	err := db.CreateTarget(ctx, second)
	if !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate name = %v, want ErrNameTaken", err)
	}
}

func TestGetMissingTarget(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.GetTarget(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetTarget of a missing id = %v, want ErrNotFound", err)
	}
}

func TestDeleteTarget(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	tgt := &Target{Name: "NAS-A", Type: TargetSMB, Host: "10.0.0.1", Share: "media"}
	if err := db.CreateTarget(ctx, tgt); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	if err := db.DeleteTarget(ctx, tgt.ID); err != nil {
		t.Fatalf("DeleteTarget: %v", err)
	}
	if err := db.DeleteTarget(ctx, tgt.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second DeleteTarget = %v, want ErrNotFound", err)
	}
}

func TestSetNegotiatedVers(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	tgt := &Target{Name: "NAS-A", Type: TargetSMB, Host: "10.0.0.1", Share: "media"}
	if err := db.CreateTarget(ctx, tgt); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	if err := db.SetNegotiatedVers(ctx, tgt.ID, "2.1"); err != nil {
		t.Fatalf("SetNegotiatedVers: %v", err)
	}

	loaded, err := db.GetTarget(ctx, tgt.ID)
	if err != nil {
		t.Fatalf("GetTarget: %v", err)
	}
	if loaded.NegotiatedVers != "2.1" {
		t.Errorf("negotiated_vers = %q, want %q", loaded.NegotiatedVers, "2.1")
	}
}

func TestTargetValidate(t *testing.T) {
	tests := []struct {
		name    string
		target  Target
		wantErr string // substring; empty means valid
	}{
		{
			name:   "valid smb target",
			target: Target{Name: "NAS", Type: TargetSMB, Host: "192.168.1.50", Share: "media"},
		},
		{
			name:   "valid guest smb target",
			target: Target{Name: "NAS", Type: TargetSMB, Host: "192.168.1.50", Share: "public"},
		},
		{
			name:   "valid local target",
			target: Target{Name: "Local", Type: TargetLocal, LocalPath: "/mnt/local/stuff"},
		},
		{
			name:    "missing name",
			target:  Target{Type: TargetSMB, Host: "h", Share: "s"},
			wantErr: "name is required",
		},
		{
			name:    "unknown type",
			target:  Target{Name: "x", Type: "sftp"},
			wantErr: "type must be",
		},
		{
			name:    "smb without host",
			target:  Target{Name: "x", Type: TargetSMB, Share: "media"},
			wantErr: "host is required",
		},
		{
			name:    "smb without share",
			target:  Target{Name: "x", Type: TargetSMB, Host: "10.0.0.1"},
			wantErr: "share is required",
		},
		{
			name:    "host with a slash is really a UNC path",
			target:  Target{Name: "x", Type: TargetSMB, Host: "//10.0.0.1/media", Share: "media"},
			wantErr: "without slashes",
		},
		{
			name:    "share with a slash",
			target:  Target{Name: "x", Type: TargetSMB, Host: "10.0.0.1", Share: "media/photos"},
			wantErr: "without slashes",
		},
		{
			name:    "port out of range",
			target:  Target{Name: "x", Type: TargetSMB, Host: "10.0.0.1", Share: "m", Port: 70000},
			wantErr: "out of range",
		},
		{
			name:    "absolute subpath",
			target:  Target{Name: "x", Type: TargetSMB, Host: "10.0.0.1", Share: "m", Subpath: "/etc"},
			wantErr: "relative to the share root",
		},
		{
			name:    "subpath escaping the share",
			target:  Target{Name: "x", Type: TargetSMB, Host: "10.0.0.1", Share: "m", Subpath: "a/../../etc"},
			wantErr: `must not contain ".."`,
		},
		{
			name:    "local without a path",
			target:  Target{Name: "x", Type: TargetLocal},
			wantErr: "local_path is required",
		},
		{
			name:    "relative local path",
			target:  Target{Name: "x", Type: TargetLocal, LocalPath: "relative/dir"},
			wantErr: "absolute path",
		},
		{
			name:    "smb fields on a local target",
			target:  Target{Name: "x", Type: TargetLocal, LocalPath: "/mnt/x", Host: "10.0.0.1"},
			wantErr: "only valid for an SMB target",
		},
		{
			name:    "local path on an smb target",
			target:  Target{Name: "x", Type: TargetSMB, Host: "10.0.0.1", Share: "m", LocalPath: "/mnt/x"},
			wantErr: "only valid for a local target",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.target.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestIsGuest(t *testing.T) {
	if !(&Target{}).IsGuest() {
		t.Error("a target with no username should mount as guest")
	}
	if (&Target{Username: "syncuser"}).IsGuest() {
		t.Error("a target with a username is not a guest mount")
	}
}

func TestUNCPath(t *testing.T) {
	smb := &Target{Type: TargetSMB, Host: "192.168.1.50", Share: "media"}
	if got := smb.UNCPath(); got != "//192.168.1.50/media" {
		t.Errorf("UNCPath() = %q, want %q", got, "//192.168.1.50/media")
	}
	local := &Target{Type: TargetLocal, LocalPath: "/mnt/local/stuff"}
	if got := local.UNCPath(); got != "/mnt/local/stuff" {
		t.Errorf("UNCPath() = %q, want %q", got, "/mnt/local/stuff")
	}
}

// A target keeps its own name when it is updated.
//
// The symptom this pins was reported from the UI: create a target with a
// wrong IP, "save and test", watch the test fail, correct the address, save
// again — refused for a name already in use, which was its own from moments
// earlier. The cause was entirely client-side (the modal kept POSTing because
// it had not noticed the first save succeeded), and the server was always
// right. This test says so, so that a later change to the uniqueness check
// cannot quietly make the server wrong too.
func TestUpdatingATargetKeepsItsOwnName(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	target := &Target{Name: "nas-with-a-typo", Type: TargetSMB, Host: "192.168.1.99", Share: "media"}
	if err := db.CreateTarget(ctx, target); err != nil {
		t.Fatalf("creating: %v", err)
	}

	// Same name, corrected address — the exact second save from the report.
	target.Host = "192.168.1.50"
	if err := db.UpdateTarget(ctx, target); err != nil {
		t.Fatalf("updating a target without changing its name: %v", err)
	}

	stored, err := db.GetTarget(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Host != "192.168.1.50" {
		t.Fatalf("host = %q, want the corrected address", stored.Host)
	}

	// And a *different* target may still not take the name.
	clash := &Target{Name: "nas-with-a-typo", Type: TargetSMB, Host: "192.168.1.51", Share: "media"}
	if err := db.CreateTarget(ctx, clash); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("a second target took the name: %v", err)
	}
}
