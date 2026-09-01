package mountmgr

import (
	"strings"
	"testing"

	"github.com/alokw/cn4m-cascade/internal/store"
)

func TestBuildOptions(t *testing.T) {
	params := Params{UID: 1000, GID: 1000}

	tests := []struct {
		name        string
		target      store.Target
		dialect     string
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:    "defaults from SPEC.md §5",
			target:  store.Target{Username: "syncuser"},
			dialect: "3.1.1",
			wantContain: []string{
				"vers=3.1.1", "rsize=4194304", "wsize=4194304", "cache=loose",
				"actimeo=30", "soft", "echo_interval=2", "uid=1000", "gid=1000",
				"iocharset=utf8",
			},
			wantAbsent: []string{"guest", "multichannel", "port="},
		},
		{
			name:        "empty username mounts as guest",
			target:      store.Target{},
			dialect:     "3.0",
			wantContain: []string{"guest", "vers=3.0"},
		},
		{
			name:        "multichannel when enabled per target",
			target:      store.Target{Username: "u", Multichannel: true},
			dialect:     "3.1.1",
			wantContain: []string{"multichannel"},
		},
		{
			name:        "non-default port",
			target:      store.Target{Username: "u", Port: 4450},
			dialect:     "3.1.1",
			wantContain: []string{"port=4450"},
		},
		{
			name:        "override replaces a default",
			target:      store.Target{Username: "u", MountOptsOverride: "cache=none,actimeo=1"},
			dialect:     "3.1.1",
			wantContain: []string{"cache=none", "actimeo=1"},
			wantAbsent:  []string{"cache=loose", "actimeo=30"},
		},
		{
			name:        "override adds a bare flag",
			target:      store.Target{Username: "u", MountOptsOverride: "noserverino"},
			dialect:     "3.1.1",
			wantContain: []string{"noserverino"},
		},
		{
			name:        "separate sessions so identical shares stay distinct",
			target:      store.Target{Username: "u"},
			dialect:     "3.1.1",
			wantContain: []string{"nosharesock"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildOptions(&tt.target, params, tt.dialect, tt.target.Multichannel)
			for _, want := range tt.wantContain {
				if !hasOption(got, want) {
					t.Errorf("options %q missing %q", got, want)
				}
			}
			for _, absent := range tt.wantAbsent {
				if hasOption(got, absent) {
					t.Errorf("options %q unexpectedly contains %q", got, absent)
				}
			}
		})
	}
}

// hasOption matches whole comma-separated options, so "port=" does not match
// inside "echo_interval=10" and "guest" does not match inside another word.
func hasOption(optionString, want string) bool {
	for _, part := range strings.Split(optionString, ",") {
		if part == want || (strings.HasSuffix(want, "=") && strings.HasPrefix(part, want)) {
			return true
		}
	}
	return false
}

func TestBuildOptionsNeverCarriesCredentials(t *testing.T) {
	tgt := store.Target{Username: "syncuser", Domain: "WORKGROUP"}
	got := buildOptions(&tgt, Params{}, "3.1.1", false)

	for _, forbidden := range []string{"password", "pass=", "credentials"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("option string %q contains %q; credentials must only reach mount.cifs via the 0600 credentials file", got, forbidden)
		}
	}
}

func TestDialectsFor(t *testing.T) {
	tests := []struct {
		name   string
		target store.Target
		want   []string
	}{
		{
			name:   "full ladder by default",
			target: store.Target{},
			want:   []string{"3.1.1", "3.0", "2.1"},
		},
		{
			name:   "a pinned version disables the ladder",
			target: store.Target{MountOptsOverride: "vers=2.0"},
			want:   []string{"2.0"},
		},
		{
			name:   "a pinned version among other options",
			target: store.Target{MountOptsOverride: "cache=none,vers=3.0,noserverino"},
			want:   []string{"3.0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dialectsFor(&tt.target)
			if len(got) != len(tt.want) {
				t.Fatalf("dialects = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("dialects = %v, want %v", got, tt.want)
				}
			}
		})
	}
}
