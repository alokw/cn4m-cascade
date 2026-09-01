package mountmgr

import (
	"strings"
	"testing"
)

func TestParseMountInfo(t *testing.T) {
	// Real /proc/self/mountinfo lines. The optional-fields section before
	// the "-" separator is variable length, which is what makes this format
	// worth a test.
	const sample = `25 30 0:23 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
26 30 0:24 / /sys rw,nosuid,nodev,noexec,relatime shared:7 - sysfs sysfs rw
120 30 0:52 / /mnt/smb/abc123 rw,relatime - cifs //192.168.1.50/media rw,vers=3.1.1,cache=loose
121 30 0:53 / /mnt/smb/with\040space rw,relatime shared:9 master:2 - cifs //192.168.1.51/backup rw
30 1 259:2 / / rw,relatime - ext4 /dev/nvme0n1p2 rw
this line is far too short
`

	got, err := parseMountInfo(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("parseMountInfo: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("parsed %d mounts, want 5 (the malformed line must be skipped)", len(got))
	}

	tests := []struct {
		index      int
		wantDir    string
		wantFSType string
		wantSource string
	}{
		{0, "/proc", "proc", "proc"},
		{1, "/sys", "sysfs", "sysfs"},
		{2, "/mnt/smb/abc123", "cifs", "//192.168.1.50/media"},
		{3, "/mnt/smb/with space", "cifs", "//192.168.1.51/backup"},
		{4, "/", "ext4", "/dev/nvme0n1p2"},
	}
	for _, tt := range tests {
		mi := got[tt.index]
		if mi.Dir != tt.wantDir {
			t.Errorf("mount %d dir = %q, want %q", tt.index, mi.Dir, tt.wantDir)
		}
		if mi.FSType != tt.wantFSType {
			t.Errorf("mount %d fstype = %q, want %q", tt.index, mi.FSType, tt.wantFSType)
		}
		if mi.Source != tt.wantSource {
			t.Errorf("mount %d source = %q, want %q", tt.index, mi.Source, tt.wantSource)
		}
	}
}

func TestUnescapeMountField(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"/mnt/smb/plain", "/mnt/smb/plain"},
		{`/mnt/with\040space`, "/mnt/with space"},
		{`/mnt/with\011tab`, "/mnt/with\ttab"},
		{`/mnt/with\134backslash`, `/mnt/with\backslash`},
		{`/mnt/two\040\040spaces`, "/mnt/two  spaces"},
	}
	for _, tt := range tests {
		if got := unescapeMountField(tt.in); got != tt.want {
			t.Errorf("unescapeMountField(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
