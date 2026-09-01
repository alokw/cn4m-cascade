//go:build integration

package test

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alokw/cn4m-cascade/internal/health"
	"github.com/alokw/cn4m-cascade/internal/mountmgr"
	"github.com/alokw/cn4m-cascade/internal/store"
)

// Phase 1 exit criterion: "can add an SMB target by IP via curl, test it".
func TestAddSMBTargetByIPAndTestIt(t *testing.T) {
	h := newHarness(t, nil)
	host := sambaA(t)

	id := h.createTarget(smbTarget(uniqueName("nas-a"), host, shareCredentialed, userName, userPassword))

	status, body := h.do(http.MethodPost, "/api/targets/"+id+"/test", nil)
	if status != http.StatusOK {
		t.Fatalf("test = %d, want 200: %v", status, body)
	}

	if ok, _ := body["ok"].(bool); !ok {
		t.Errorf("ok = false: %v", body)
	}
	if vers, _ := body["negotiated_vers"].(string); vers == "" {
		t.Error("negotiated_vers is empty; SPEC.md §5 requires recording which version worked")
	}
	if capacity, _ := body["capacity_bytes"].(float64); capacity <= 0 {
		t.Error("capacity_bytes is zero; statfs did not run")
	}

	names := entryNames(t, body)
	for _, want := range []string{fixtureFile, fixtureUnicode, fixtureDir} {
		if !names[want] {
			t.Errorf("root listing is missing %q; got %v", want, keys(names))
		}
	}
}

// SPEC.md §5 / PROGRESS.md D-3: an empty username mounts as guest.
func TestGuestShareMountsWithoutCredentials(t *testing.T) {
	h := newHarness(t, nil)

	id := h.createTarget(map[string]any{
		"name": uniqueName("guest"), "type": "smb",
		"host": sambaA(t), "share": shareGuest,
	})

	status, body := h.do(http.MethodPost, "/api/targets/"+id+"/test", nil)
	if status != http.StatusOK {
		t.Fatalf("test = %d, want 200: %v", status, body)
	}
	if !entryNames(t, body)[fixtureFile] {
		t.Errorf("guest share listing is missing %q", fixtureFile)
	}
}

// Phase 1 exit criterion: "see legible errors for bad creds/offline hosts".
func TestLegibleFailures(t *testing.T) {
	tests := []struct {
		name          string
		payload       func(t *testing.T) map[string]any
		wantStatus    int
		wantKind      string
		wantInMessage string
	}{
		{
			name: "wrong password",
			payload: func(t *testing.T) map[string]any {
				return smbTarget(uniqueName("badcreds"), sambaA(t), shareCredentialed, userName, "definitely-not-the-password")
			},
			wantStatus:    http.StatusUnauthorized,
			wantKind:      string(mountmgr.KindAuth),
			wantInMessage: "authentication failed",
		},
		{
			name: "unknown user",
			payload: func(t *testing.T) map[string]any {
				return smbTarget(uniqueName("baduser"), sambaA(t), shareCredentialed, "nosuchuser", "whatever")
			},
			wantStatus:    http.StatusUnauthorized,
			wantKind:      string(mountmgr.KindAuth),
			wantInMessage: "authentication failed",
		},
		{
			name: "share does not exist",
			payload: func(t *testing.T) map[string]any {
				return smbTarget(uniqueName("badshare"), sambaA(t), "no-such-share", userName, userPassword)
			},
			wantStatus:    http.StatusBadGateway,
			wantInMessage: "does not exist",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			id := h.createTarget(tt.payload(t))

			status, body := h.do(http.MethodPost, "/api/targets/"+id+"/test", nil)
			if status != tt.wantStatus {
				t.Fatalf("test = %d, want %d: %v", status, tt.wantStatus, body)
			}

			message, kind, _ := errorOf(t, body)
			if !strings.Contains(message, tt.wantInMessage) {
				t.Errorf("message %q does not contain %q", message, tt.wantInMessage)
			}
			if tt.wantKind != "" && kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", kind, tt.wantKind)
			}
			if strings.Contains(message, "exit status") || strings.Contains(message, "mount error(") {
				t.Errorf("message %q reads like raw tool output, not a legible error", message)
			}

			// Nothing may be left mounted after a failed test.
			if left := mountedUnder(t, h.mountRoot); len(left) != 0 {
				t.Errorf("a failed test left mounts behind: %v", left)
			}
		})
	}
}

// An unreachable host must fail within a bounded time, never hang
// (SPEC.md §5).
func TestOfflineHostFailsQuickly(t *testing.T) {
	h := newHarness(t, func(c *mountmgr.Config) { c.MountTimeout = 15 * time.Second })

	id := h.createTarget(smbTarget(uniqueName("offline"), offlineIP(t), "media", userName, userPassword))

	started := time.Now()
	status, body := h.do(http.MethodPost, "/api/targets/"+id+"/test", nil)
	elapsed := time.Since(started)

	if status == http.StatusOK {
		t.Fatalf("test against an unreachable host succeeded: %v", body)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("took %v to fail; nothing may hang on a dead host", elapsed)
	}
	t.Logf("unreachable host reported in %v", elapsed)

	message, kind, _ := errorOf(t, body)
	if kind != string(mountmgr.KindUnreachable) && kind != string(mountmgr.KindTimeout) {
		t.Errorf("kind = %q, want unreachable or timeout", kind)
	}
	if message == "" {
		t.Error("no message for an unreachable host")
	}

	if left := mountedUnder(t, h.mountRoot); len(left) != 0 {
		t.Errorf("a failed mount left mounts behind: %v", left)
	}
}

// Phase 1 exit criterion: "mounts clean up properly".
func TestMountIsReleasedAfterTheIdleGrace(t *testing.T) {
	h := newHarness(t, func(c *mountmgr.Config) { c.IdleGrace = 2 * time.Second })
	id := h.createTarget(smbTarget(uniqueName("cleanup"), sambaA(t), shareCredentialed, userName, userPassword))

	if status, body := h.do(http.MethodPost, "/api/targets/"+id+"/test", nil); status != http.StatusOK {
		t.Fatalf("test = %d: %v", status, body)
	}

	// The mount survives the test itself, so a run started right after
	// reuses it rather than re-mounting.
	if left := mountedUnder(t, h.mountRoot); len(left) != 1 {
		t.Fatalf("mounts immediately after a test = %v, want exactly one", left)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if len(mountedUnder(t, h.mountRoot)) == 0 {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("the mount was never reaped: %v", mountedUnder(t, h.mountRoot))
}

// Two targets on the same host and share with different credentials must be
// genuinely distinct mounts (SPEC.md §5, PROGRESS.md D-9).
func TestSameShareWithDifferentCredentials(t *testing.T) {
	h := newHarness(t, nil)
	host := sambaA(t)

	first := h.createTarget(smbTarget(uniqueName("creds-one"), host, shareCredentialed, userName, userPassword))
	second := h.createTarget(smbTarget(uniqueName("creds-two"), host, shareCredentialed, otherName, otherPass))

	for _, id := range []string{first, second} {
		status, body := h.do(http.MethodPost, "/api/targets/"+id+"/test", nil)
		if status != http.StatusOK {
			t.Fatalf("test of %s = %d, want 200: %v", id, status, body)
		}
		if !entryNames(t, body)[fixtureFile] {
			t.Errorf("target %s listed no fixture files", id)
		}
	}

	mounts := mountedUnder(t, h.mountRoot)
	if len(mounts) != 2 {
		t.Fatalf("mountpoints = %v, want two distinct mounts", mounts)
	}
	for _, id := range []string{first, second} {
		if !containsSuffix(mounts, id) {
			t.Errorf("no mountpoint for target %s in %v", id, mounts)
		}
	}
}

// The sharper half of R-2: two targets on one share must not share a session.
// Target A authenticates successfully; target B then presents a *wrong*
// password for a different account. If the kernel had reused A's superblock,
// B would ride A's session and succeed — a silent credential leak between
// targets. B failing to authenticate is the proof that it did not.
func TestSameShareCredentialsAreNotSharedBetweenTargets(t *testing.T) {
	h := newHarness(t, nil)
	host := sambaA(t)

	valid := h.createTarget(smbTarget(uniqueName("iso-valid"), host, shareCredentialed, userName, userPassword))
	if status, body := h.do(http.MethodPost, "/api/targets/"+valid+"/test", nil); status != http.StatusOK {
		t.Fatalf("test of the valid target = %d, want 200: %v", status, body)
	}

	// Same host, same share, different account, wrong password.
	leaky := h.createTarget(smbTarget(uniqueName("iso-leaky"), host, shareCredentialed, otherName, "not-the-right-password"))
	status, body := h.do(http.MethodPost, "/api/targets/"+leaky+"/test", nil)

	if status == http.StatusOK {
		t.Fatalf("a target with a wrong password mounted successfully while another target held a session to the same share: credentials are leaking between mounts")
	}
	message, kind, _ := errorOf(t, body)
	if kind != string(mountmgr.KindAuth) {
		t.Errorf("kind = %q, want %q (message: %s)", kind, mountmgr.KindAuth, message)
	}
}

// Deleting a target that a job still holds must be refused, not silently
// orphan the mount.
func TestDeleteIsRefusedWhileTheTargetIsInUse(t *testing.T) {
	h := newHarness(t, nil)
	id := h.createTarget(smbTarget(uniqueName("inuse"), sambaA(t), shareCredentialed, userName, userPassword))

	tgt, err := h.db.GetTarget(context.Background(), id)
	if err != nil {
		t.Fatalf("loading the target: %v", err)
	}
	_, release, err := h.mounts.Acquire(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	status, body := h.do(http.MethodDelete, "/api/targets/"+id, nil)
	if status != http.StatusConflict {
		release()
		t.Fatalf("delete while in use = %d, want 409: %v", status, body)
	}
	_, _, code := errorOf(t, body)
	if code != "target_in_use" {
		t.Errorf("code = %q, want %q", code, "target_in_use")
	}

	release()
	if status, body := h.do(http.MethodDelete, "/api/targets/"+id, nil); status != http.StatusNoContent {
		t.Fatalf("delete after release = %d, want 204: %v", status, body)
	}
	if left := mountedUnder(t, h.mountRoot); len(left) != 0 {
		t.Errorf("deleting a target left its mount behind: %v", left)
	}
}

// SPEC.md §5, startup hygiene: mounts left by an unclean shutdown are
// detached before the server serves traffic.
func TestStartupDetachesLeftoverMounts(t *testing.T) {
	h := newHarness(t, nil)
	id := h.createTarget(smbTarget(uniqueName("leftover"), sambaA(t), shareCredentialed, userName, userPassword))

	// Mount it, then throw away the manager's bookkeeping the way a crash
	// would, leaving the kernel mount in place.
	tgt, err := h.db.GetTarget(context.Background(), id)
	if err != nil {
		t.Fatalf("loading the target: %v", err)
	}
	if _, release, err := h.mounts.Acquire(context.Background(), tgt); err != nil {
		t.Fatalf("Acquire: %v", err)
	} else {
		release()
	}
	if left := mountedUnder(t, h.mountRoot); len(left) != 1 {
		t.Fatalf("expected one mount to be left behind, got %v", left)
	}

	// A fresh manager over the same mount root is what a restart looks like.
	restarted := mountmgr.New(mountmgr.Config{
		MountRoot:      h.mountRoot,
		MountTimeout:   20 * time.Second,
		StatFSTimeout:  5 * time.Second,
		UnmountTimeout: 15 * time.Second,
		IdleGrace:      time.Minute,
	}, mountmgr.NewExecMounter(), h.db, func(context.Context, *store.Target) (string, error) {
		return "", nil
	}, health.NewCache(), slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	if err := restartedReconcile(restarted); err != nil {
		t.Fatalf("ReconcileStale: %v", err)
	}
	if left := mountedUnder(t, h.mountRoot); len(left) != 0 {
		t.Fatalf("startup cleanup left mounts behind: %v", left)
	}
}

// Credentials must never reach the logs (CLAUDE.md).
func TestCredentialsNeverAppearInLogs(t *testing.T) {
	h := newHarness(t, nil)

	ok := h.createTarget(smbTarget(uniqueName("logcheck"), sambaA(t), shareCredentialed, userName, userPassword))
	if status, body := h.do(http.MethodPost, "/api/targets/"+ok+"/test", nil); status != http.StatusOK {
		t.Fatalf("test = %d: %v", status, body)
	}

	bad := h.createTarget(smbTarget(uniqueName("logcheck-bad"), sambaA(t), shareCredentialed, userName, "sekrit-wrong-password"))
	h.do(http.MethodPost, "/api/targets/"+bad+"/test", nil)

	logs := h.logs.String()
	for _, secret := range []string{userPassword, "sekrit-wrong-password"} {
		if strings.Contains(logs, secret) {
			t.Errorf("a password appeared in the logs")
		}
	}
}

// The API must never echo a password back, encrypted or otherwise.
func TestTargetResponsesOmitCredentials(t *testing.T) {
	h := newHarness(t, nil)
	id := h.createTarget(smbTarget(uniqueName("nocreds"), sambaA(t), shareCredentialed, userName, userPassword))

	for _, path := range []string{"/api/targets/" + id, "/api/targets"} {
		status, body := h.do(http.MethodGet, path, nil)
		if status != http.StatusOK {
			t.Fatalf("GET %s = %d: %v", path, status, body)
		}
		rendered := flatten(body)
		if strings.Contains(rendered, userPassword) {
			t.Errorf("GET %s returned the password", path)
		}
		if strings.Contains(rendered, "password_encrypted") {
			t.Errorf("GET %s returned the encrypted password", path)
		}
	}
}

// A target on the second Samba server proves the harness is not accidentally
// testing one host twice.
func TestSecondSambaServer(t *testing.T) {
	h := newHarness(t, nil)
	id := h.createTarget(smbTarget(uniqueName("nas-b"), sambaB(t), shareCredentialed, userName, userPassword))

	status, body := h.do(http.MethodPost, "/api/targets/"+id+"/test", nil)
	if status != http.StatusOK {
		t.Fatalf("test = %d, want 200: %v", status, body)
	}
	if !entryNames(t, body)[fixtureFile] {
		t.Errorf("second server listing is missing %q", fixtureFile)
	}
}

func restartedReconcile(m *mountmgr.Manager) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return m.ReconcileStale(ctx)
}

func entryNames(t *testing.T, body map[string]any) map[string]bool {
	t.Helper()

	raw, ok := body["entries"].([]any)
	if !ok {
		t.Fatalf("response has no entries: %v", body)
	}
	names := map[string]bool{}
	for _, e := range raw {
		if entry, ok := e.(map[string]any); ok {
			if name, ok := entry["name"].(string); ok {
				names[name] = true
			}
		}
	}
	return names
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func containsSuffix(values []string, suffix string) bool {
	for _, v := range values {
		if strings.HasSuffix(v, suffix) {
			return true
		}
	}
	return false
}

func flatten(body map[string]any) string {
	var b strings.Builder
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, val := range t {
				b.WriteString(k)
				b.WriteString("=")
				walk(val)
			}
		case []any:
			for _, val := range t {
				walk(val)
			}
		default:
			b.WriteString(strings.TrimSpace(strings.Join(strings.Fields(toString(v)), " ")))
			b.WriteString(" ")
		}
	}
	walk(body)
	return b.String()
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
