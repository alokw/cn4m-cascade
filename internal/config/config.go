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

// DefaultListenAddr is where the server listens unless LISTEN_ADDR says
// otherwise. Exported because the healthcheck has to dial the same address,
// and two copies of a port number is two places to get it wrong.
const DefaultListenAddr = ":2649"

// DefaultCN4MStatusURL is cn4m on the same machine, which is how this is
// normally deployed — cn4m's own client documents the same default.
const DefaultCN4MStatusURL = "http://localhost:2640/suite/status"

// CN4MReportingOff is the CN4M_CASCADE_STATUS_URL value that disables suite reporting
// entirely. A word rather than an empty string, because an empty environment
// variable is far more often a mistake — an unset value in a compose file
// expanding to nothing — than a decision.
const CN4MReportingOff = "off"

// Config is the fully resolved runtime configuration.
type Config struct {
	ListenAddr string // LISTEN_ADDR, default ":2649"
	DataDir    string // DATA_DIR, holds the SQLite database; platform default
	MountRoot  string // MOUNT_ROOT, where CIFS mounts go; unused on Windows

	// CN4MStatusURL is where this reports its status in the cn4m suite
	// (SPEC.md §8.2). Defaults to cn4m on the same machine, which is the
	// common deployment; set CN4M_CASCADE_STATUS_URL when cn4m is elsewhere, or to
	// "off" to report nowhere.
	CN4MStatusURL string

	// DiscordWebhookURL seeds a Discord notification on a fresh database.
	// Empty means none, which is the default: a chat notification nobody asked
	// for is worse than no notification.
	DiscordWebhookURL string

	// EncryptionKey is the raw secret from ENCRYPTION_KEY. It is never the
	// key used directly; secrets.NewBox derives from it.
	EncryptionKey string

	// ConfigPath is the config file that was layered in, or "" when
	// configuration came entirely from the environment. Reported at startup so
	// an operator can see *which* file the process actually read — the usual
	// confusion with a discovered config file is editing a different copy.
	ConfigPath string

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
func (c *Config) DBPath() string { return filepath.Join(c.DataDir, "cn4m-cascade.db") }

// minKeyLen is the shortest ENCRYPTION_KEY we accept. Short keys are a
// footgun: the derived key is only as strong as the input entropy.
const minKeyLen = 16

// Load reads configuration from the environment, after layering in a config
// file if one is present.
//
// The file is a convenience for deployments with no orchestrator to interpolate
// one: `docker compose` reads `.env` on the operator's behalf, and a native
// install has nothing doing that (SPEC.md §10). Environment variables still win
// over the file, so a one-off override needs no edit.
func Load() (*Config, error) {
	configPath, err := applyEnvFile()
	if err != nil {
		return nil, err
	}

	c := &Config{
		ConfigPath:        configPath,
		ListenAddr:        envStr("LISTEN_ADDR", DefaultListenAddr),
		DataDir:           envStr("DATA_DIR", defaultDataDir()),
		MountRoot:         envStr("MOUNT_ROOT", defaultMountRoot()),
		CN4MStatusURL:     envStr("CN4M_CASCADE_STATUS_URL", DefaultCN4MStatusURL),
		DiscordWebhookURL: envStr("CN4M_CASCADE_DISCORD_WEBHOOK", ""),
		EncryptionKey:     os.Getenv("ENCRYPTION_KEY"),
		MountUID:          os.Getuid(),
		MountGID:          os.Getgid(),
		MountTimeout:      30 * time.Second,
		StatFSTimeout:     5 * time.Second,
		UnmountTimeout:    15 * time.Second,
		IdleGrace:         60 * time.Second,
	}

	if c.EncryptionKey == "" {
		return nil, fmt.Errorf("ENCRYPTION_KEY is not set: it is required to store target credentials, refusing to start")
	}
	if len(c.EncryptionKey) < minKeyLen {
		return nil, fmt.Errorf("ENCRYPTION_KEY is too short (%d characters): use at least %d", len(c.EncryptionKey), minKeyLen)
	}

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
