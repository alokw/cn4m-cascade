//go:build !windows

package config

// defaultDataDir is the container's data volume (SPEC.md §10).
func defaultDataDir() string { return "/data" }

// defaultMountRoot is where CIFS mounts are made (SPEC.md §3).
func defaultMountRoot() string { return "/mnt/smb" }
