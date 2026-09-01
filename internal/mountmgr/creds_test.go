package mountmgr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteCredentialsFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "creds")

	path, cleanup, err := writeCredentialsFile(dir, "syncuser", "p@ss,word=tricky", "WORKGROUP")
	if err != nil {
		t.Fatalf("writeCredentialsFile: %v", err)
	}
	defer cleanup()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	want := "username=syncuser\npassword=p@ss,word=tricky\ndomain=WORKGROUP\n"
	if string(body) != want {
		t.Errorf("contents = %q, want %q", body, want)
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("cleanup did not remove the credentials file")
	}
}

func TestWriteCredentialsFileWithoutDomain(t *testing.T) {
	path, cleanup, err := writeCredentialsFile(t.TempDir(), "u", "p", "")
	if err != nil {
		t.Fatalf("writeCredentialsFile: %v", err)
	}
	defer cleanup()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if strings.Contains(string(body), "domain=") {
		t.Errorf("contents = %q, want no domain line", body)
	}
}

func TestCredentialsDirectoryIsNotWorldReadable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "creds")
	_, cleanup, err := writeCredentialsFile(dir, "u", "p", "")
	if err != nil {
		t.Fatalf("writeCredentialsFile: %v", err)
	}
	defer cleanup()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("credentials directory mode = %o, want no group or other access", perm)
	}
}
