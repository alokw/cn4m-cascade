// Package config loads and validates process configuration from the
// environment. Validation is strict and happens once at startup so that a
// misconfigured container fails immediately with a legible message rather
// than at the first mount attempt.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Config is the fully resolved runtime configuration.
type Config struct {
	ListenAddr string // LISTEN_ADDR, default ":8384"
	DataDir    string // DATA_DIR, default "/data" — holds the SQLite database
	MountRoot  string // MOUNT_ROOT, default "/mnt/smb"

	// EncryptionKey is the raw secret from ENCRYPTION_KEY. It is never the
	// key used directly; secrets.NewBox derives from it.
	EncryptionKey string

	// MountUID/MountGID become uid=/gid= in the CIFS mount options, so that
	// files on the share are owned by the account the engine runs as.
	MountUID int
	MountGID int

	// Timeouts. Nothing that touches an SMB path is allowed to be unbounded
	// (SPEC.md §5).
	MountTimeout   time.Duration // whole mount attempt, per version rung
	StatFSTimeout  time.Duration // stale-mount watchdog
	UnmountTimeout time.Duration
	IdleGrace      time.Duration // refcount-zero grace before unmounting
}

// DBPath is the location of the SQLite database file.
func (c *Config) DBPath() string { return filepath.Join(c.DataDir, "smbsync.db") }

// minKeyLen is the shortest ENCRYPTION_KEY we accept. Short keys are a
// footgun: the derived key is only as strong as the input entropy.
const minKeyLen = 16

// Load reads configuration from the environment.
func Load() (*Config, error) {
	c := &Config{
		ListenAddr:     envStr("LISTEN_ADDR", ":8384"),
		DataDir:        envStr("DATA_DIR", "/data"),
		MountRoot:      envStr("MOUNT_ROOT", "/mnt/smb"),
		EncryptionKey:  os.Getenv("ENCRYPTION_KEY"),
		MountUID:       os.Getuid(),
		MountGID:       os.Getgid(),
		MountTimeout:   30 * time.Second,
		StatFSTimeout:  5 * time.Second,
		UnmountTimeout: 15 * time.Second,
		IdleGrace:      60 * time.Second,
	}

	if c.EncryptionKey == "" {
		return nil, fmt.Errorf("ENCRYPTION_KEY is not set: it is required to store target credentials, refusing to start")
	}
	if len(c.EncryptionKey) < minKeyLen {
		return nil, fmt.Errorf("ENCRYPTION_KEY is too short (%d characters): use at least %d", len(c.EncryptionKey), minKeyLen)
	}

	var err error
	if c.MountUID, err = envInt("MOUNT_UID", c.MountUID); err != nil {
		return nil, err
	}
	if c.MountGID, err = envInt("MOUNT_GID", c.MountGID); err != nil {
		return nil, err
	}
	for _, d := range []struct {
		name string
		dst  *time.Duration
	}{
		{"MOUNT_TIMEOUT", &c.MountTimeout},
		{"STATFS_TIMEOUT", &c.StatFSTimeout},
		{"UNMOUNT_TIMEOUT", &c.UnmountTimeout},
		{"MOUNT_IDLE_GRACE", &c.IdleGrace},
	} {
		if *d.dst, err = envDuration(d.name, *d.dst); err != nil {
			return nil, err
		}
	}

	return c, nil
}

func envStr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer, got %q", name, v)
	}
	return n, nil
}

func envDuration(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration such as \"30s\", got %q", name, v)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %q", name, v)
	}
	return d, nil
}
