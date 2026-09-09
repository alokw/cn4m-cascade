package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadRequiresEncryptionKey(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "")

	_, err := Load()
	if err == nil {
		t.Fatal("Load succeeded without ENCRYPTION_KEY; SPEC.md §5 requires startup to fail")
	}
	if !strings.Contains(err.Error(), "ENCRYPTION_KEY") {
		t.Errorf("error %q does not name the missing variable", err)
	}
}

func TestLoadRejectsShortEncryptionKey(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "tooshort")

	if _, err := Load(); err == nil {
		t.Fatal("Load accepted an 8-character encryption key")
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "a-sufficiently-long-key")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	tests := []struct {
		name      string
		got, want any
	}{
		{"listen addr", cfg.ListenAddr, ":2649"},
		{"data dir", cfg.DataDir, "/data"},
		{"mount root", cfg.MountRoot, "/mnt/smb"},
		{"db path", cfg.DBPath(), "/data/cn4m-cascade.db"},
		{"idle grace", cfg.IdleGrace, 60 * time.Second},
		{"statfs timeout", cfg.StatFSTimeout, 5 * time.Second},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
		}
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "a-sufficiently-long-key")
	t.Setenv("LISTEN_ADDR", ":9000")
	t.Setenv("DATA_DIR", "/var/lib/cn4m-cascade")
	t.Setenv("MOUNT_IDLE_GRACE", "5s")
	t.Setenv("MOUNT_UID", "1234")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != ":9000" {
		t.Errorf("listen addr = %q, want %q", cfg.ListenAddr, ":9000")
	}
	if cfg.DBPath() != "/var/lib/cn4m-cascade/cn4m-cascade.db" {
		t.Errorf("db path = %q", cfg.DBPath())
	}
	if cfg.IdleGrace != 5*time.Second {
		t.Errorf("idle grace = %v, want 5s", cfg.IdleGrace)
	}
	if cfg.MountUID != 1234 {
		t.Errorf("mount uid = %d, want 1234", cfg.MountUID)
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{"non-numeric uid", "MOUNT_UID", "root"},
		{"unparseable duration", "MOUNT_IDLE_GRACE", "sixty"},
		{"negative duration", "MOUNT_IDLE_GRACE", "-5s"},
		{"zero duration", "STATFS_TIMEOUT", "0s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ENCRYPTION_KEY", "a-sufficiently-long-key")
			t.Setenv(tt.key, tt.value)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load accepted %s=%q", tt.key, tt.value)
			}
			if !strings.Contains(err.Error(), tt.key) {
				t.Errorf("error %q does not name %s", err, tt.key)
			}
		})
	}
}
