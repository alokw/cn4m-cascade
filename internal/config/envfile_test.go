package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The tolerances here are not politeness — each one is a real way these files
// get written, and each failure mode is a config file that visibly sets a value
// the program then reports as unset.
func TestLoadEnvFile(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    map[string]string
		wantErr string
	}{
		{
			name:    "plain key and value",
			content: "ENCRYPTION_KEY=abc123\n",
			want:    map[string]string{"ENCRYPTION_KEY": "abc123"},
		},
		{
			// Notepad writes a BOM by default. Without stripping it the first
			// key is U+FEFF + the name, which nothing reads.
			name:    "UTF-8 BOM on the first line",
			content: "\ufeffENCRYPTION_KEY=abc123\n",
			want:    map[string]string{"ENCRYPTION_KEY": "abc123"},
		},
		{
			// A trailing CR inside the value would become part of the key
			// material, and then nothing decrypts.
			name:    "CRLF line endings",
			content: "ENCRYPTION_KEY=abc123\r\nLOG_LEVEL=debug\r\n",
			want:    map[string]string{"ENCRYPTION_KEY": "abc123", "LOG_LEVEL": "debug"},
		},
		{
			name:    "comments and blank lines are ignored",
			content: "# a comment\n\n  # indented comment\nDATA_DIR=C:/data\n",
			want:    map[string]string{"DATA_DIR": "C:/data"},
		},
		{
			name:    "export prefix is accepted",
			content: "export LISTEN_ADDR=:2649\n",
			want:    map[string]string{"LISTEN_ADDR": ":2649"},
		},
		{
			name:    "surrounding double quotes are removed",
			content: "ENCRYPTION_KEY=\"abc 123\"\n",
			want:    map[string]string{"ENCRYPTION_KEY": "abc 123"},
		},
		{
			name:    "surrounding single quotes are removed",
			content: "ENCRYPTION_KEY='abc 123'\n",
			want:    map[string]string{"ENCRYPTION_KEY": "abc 123"},
		},
		{
			// A generated password can contain a quote. Only a matching
			// outermost pair is stripped, so this must survive.
			name:    "an unmatched quote inside a value is kept",
			content: "ENCRYPTION_KEY=ab\"c123\n",
			want:    map[string]string{"ENCRYPTION_KEY": "ab\"c123"},
		},
		{
			name:    "a value containing = is kept whole",
			content: "ENCRYPTION_KEY=YWJjMTIzNA==\n",
			want:    map[string]string{"ENCRYPTION_KEY": "YWJjMTIzNA=="},
		},
		{
			name:    "an empty value is allowed",
			content: "CN4M_CASCADE_STATUS_URL=\n",
			want:    map[string]string{"CN4M_CASCADE_STATUS_URL": ""},
		},
		{
			name:    "the last of a repeated key wins",
			content: "LOG_LEVEL=info\nLOG_LEVEL=debug\n",
			want:    map[string]string{"LOG_LEVEL": "debug"},
		},
		{
			name:    "a line that is not an assignment is an error naming the line",
			content: "ENCRYPTION_KEY=abc\nthis is not a setting\n",
			wantErr: ":2:",
		},
		{
			name:    "an empty key is an error",
			content: "=value\n",
			wantErr: "empty key",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".env")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}

			got, err := LoadEnvFile(path)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("LoadEnvFile succeeded with %v, want an error containing %q", got, tc.wantErr)
				}
				if !contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadEnvFile: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("values = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// The environment must win over the file, matching what docker compose does
// with its own .env. The reverse would make an exported variable silently
// ineffective, which is the worst kind of configuration bug to debug.
func TestEnvironmentOverridesTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "LOG_LEVEL=info\nDATA_DIR=from-the-file\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv(ConfigPathVar, path)
	t.Setenv("LOG_LEVEL", "debug") // already set: the file must not clobber it
	os.Unsetenv("DATA_DIR")        // not set: the file should supply it

	used, err := applyEnvFile()
	if err != nil {
		t.Fatalf("applyEnvFile: %v", err)
	}
	if used != path {
		t.Fatalf("used %q, want %q", used, path)
	}
	if got := os.Getenv("LOG_LEVEL"); got != "debug" {
		t.Errorf("LOG_LEVEL = %q, want debug — the file overrode the environment", got)
	}
	if got := os.Getenv("DATA_DIR"); got != "from-the-file" {
		t.Errorf("DATA_DIR = %q, want it to come from the file", got)
	}
	os.Unsetenv("DATA_DIR")
}

// Pointing at a file that is not there is a mistake worth reporting, not one to
// paper over: whoever set the variable meant that path.
func TestAnExplicitConfigPathMustExist(t *testing.T) {
	t.Setenv(ConfigPathVar, filepath.Join(t.TempDir(), "absent.env"))

	if _, err := applyEnvFile(); err == nil {
		t.Fatal("a missing explicit config file was accepted; it must be an error")
	}
}

// No file at all is the original behaviour and must stay silent.
func TestNoConfigFileIsNotAnError(t *testing.T) {
	t.Setenv(ConfigPathVar, "")

	used, err := applyEnvFile()
	if err != nil {
		t.Fatalf("applyEnvFile with no file: %v", err)
	}
	// used may legitimately be a .env beside the test binary; the contract is
	// only that the absence of one is not an error.
	_ = used
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}())
}
