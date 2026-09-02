//go:build integration

package test

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alokw/cn4m-cascade/internal/store"
)

// rawGet issues a GET and returns the status, content type and body without
// assuming JSON — the SPA routes return HTML, which h.do cannot parse.
func (h *harness) rawGet(path string, anon bool) (int, string, string) {
	h.t.Helper()

	client := h.client
	if anon {
		client = h.anonClient
	}

	req, err := http.NewRequest(http.MethodGet, h.server.URL+path, nil)
	if err != nil {
		h.t.Fatalf("building the request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		h.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("reading the response: %v", err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(body)
}

// The security-relevant assertion of Phase 4b-1. Serving the SPA moved the
// session guard from "wraps everything" to "mounted on /api/", so that the
// shell can load for a browser with no cookie — the login screen is part of
// the SPA. This pins both halves of that: the shell became public, and the
// data did not.
func TestStaticShellIsPublicButAPIIsNot(t *testing.T) {
	h := newHarness(t, nil)

	// With no cookie at all.
	status, ctype, body := h.rawGet("/", true)
	if status != http.StatusOK {
		t.Fatalf("anonymous GET / = %d, want 200 — the SPA shell must load without a session: %s", status, body)
	}
	if !strings.Contains(ctype, "text/html") {
		t.Fatalf("anonymous GET / content-type = %q, want text/html", ctype)
	}
	if !strings.Contains(body, `id="root"`) {
		t.Fatalf("anonymous GET / did not return the SPA shell: %q", truncate(body))
	}

	// The data behind it is still closed.
	for _, path := range []string{
		"/api/targets",
		"/api/jobs",
		"/api/runs",
		"/api/logs",
		"/api/browse?target_id=x",
		"/api/ws",
	} {
		status, _, body := h.rawGet(path, true)
		if status != http.StatusUnauthorized {
			t.Fatalf("anonymous GET %s = %d, want 401 — the session guard must still cover it: %s",
				path, status, truncate(body))
		}
		if !strings.Contains(body, "unauthenticated") {
			t.Fatalf("anonymous GET %s did not return the unauthenticated envelope: %s", path, truncate(body))
		}
	}

	// And the auth endpoints stay reachable, or nobody could ever sign in.
	if status, _, _ := h.rawGet("/api/auth/session", true); status != http.StatusOK {
		t.Fatalf("anonymous GET /api/auth/session = %d, want 200", status)
	}
	if status, _, _ := h.rawGet("/healthz", true); status != http.StatusOK {
		t.Fatalf("anonymous GET /healthz = %d, want 200", status)
	}
}

// Deep links have to survive a cold load: a user pasting a run URL gets the
// shell and the client router takes over. A missing asset must not, because
// HTML served for a .js request surfaces as a syntax error somewhere
// unrelated to the actual fault.
func TestSPADeepLinksAndMissingAssets(t *testing.T) {
	h := newHarness(t, nil)

	for _, path := range []string{"/targets", "/runs/does-not-exist", "/jobs/1/edit"} {
		status, ctype, body := h.rawGet(path, false)
		if status != http.StatusOK || !strings.Contains(ctype, "text/html") {
			t.Fatalf("GET %s = %d (%s), want 200 text/html", path, status, ctype)
		}
		if !strings.Contains(body, `id="root"`) {
			t.Fatalf("GET %s did not return the SPA shell", path)
		}
	}

	if status, _, _ := h.rawGet("/assets/definitely-not-built.js", false); status != http.StatusNotFound {
		t.Fatalf("GET a missing asset = %d, want 404 — it must not fall back to the shell", status)
	}
}

// The real Vite build is what ships, so assert the embedded one is real rather
// than the placeholder: index.html must reference a hashed bundle under the
// asset directory, and that bundle must actually be served.
func TestEmbeddedBuildIsTheRealSPA(t *testing.T) {
	h := newHarness(t, nil)

	_, _, shell := h.rawGet("/", true)
	if !strings.Contains(shell, "/assets/") {
		t.Fatalf("the embedded index.html references no bundle — is this the placeholder? %s", truncate(shell))
	}

	asset := betweenQuotes(shell, "/assets/")
	if asset == "" {
		t.Fatalf("could not find an asset URL in the shell: %s", truncate(shell))
	}
	status, ctype, _ := h.rawGet(asset, true)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 — the shell references an asset that is not served", asset, status)
	}
	if strings.Contains(ctype, "text/html") {
		t.Fatalf("GET %s returned HTML (%s) — the asset branch fell through to the shell", asset, ctype)
	}
}

// betweenQuotes pulls the first quoted string containing needle out of s.
func betweenQuotes(s, needle string) string {
	i := strings.Index(s, needle)
	if i < 0 {
		return ""
	}
	start := strings.LastIndexAny(s[:i], `"'`)
	end := strings.IndexAny(s[i:], `"'`)
	if start < 0 || end < 0 {
		return ""
	}
	return s[start+1 : i+end]
}

func truncate(s string) string {
	if len(s) <= 300 {
		return s
	}
	return s[:300] + "…"
}

// localTarget is the payload the UI's target modal sends for a local folder.
// The server validates the two types as mutually exclusive, so the SMB fields
// are sent empty rather than omitted — that is what lets an existing SMB
// target be switched to local without being rejected.
func localTarget(name, path string) map[string]any {
	return map[string]any{
		"name": name, "type": "local", "local_path": path,
		"host": "", "share": "", "username": "", "password": "",
	}
}

// A local folder as the *source* of a job, mirrored to an SMB share. The
// storage provider has had a LocalStorage branch since Phase 1 and the store
// has validated the type for as long, but nothing exercised the path
// end-to-end — the UI could not create one until the target modal grew a type
// selector, so the whole combination was untested.
func TestLocalSourceMirrorsToSMB(t *testing.T) {
	h := newHarness(t, nil)

	srcRoot := t.TempDir()
	seedTree(t, srcRoot, map[string]int{"a.txt": 64, "nested/b.bin": 2048})

	scope := fmt.Sprintf("local-%d", time.Now().UnixNano())
	srcID := h.createTarget(localTarget(uniqueName("local-src"), srcRoot))
	dstID := h.createTarget(smbTarget(uniqueName("dst"), sambaA(t), shareGuest, "", ""))

	dstRoot := filepath.Join(h.mountFor(t, dstID), scope)
	if err := os.MkdirAll(dstRoot, 0o755); err != nil {
		t.Fatalf("creating the destination subpath: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dstRoot) })

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("local-to-smb"),
		"source_target_id": srcID,
		"mode":             string(store.ModeMirror),
		"destinations":     []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
	})

	runID := h.runJob(t, jobID)
	run := h.awaitRun(t, runID, 60*time.Second)
	if got := run["status"]; got != string(store.RunSuccess) {
		t.Fatalf("local-source run = %v, want success: %v", got, run)
	}
	assertSMBTreesMatch(t, srcRoot, dstRoot)

	// A local target needs no mount and no credentials, so testing it must
	// succeed without either — and report the folder as its root.
	status, body := h.do(http.MethodPost, "/api/targets/"+srcID+"/test", nil)
	if status != http.StatusOK {
		t.Fatalf("testing a local target = %d, want 200: %v", status, body)
	}
	if got, _ := body["root"].(string); got != srcRoot {
		t.Fatalf("local target root = %q, want %q", got, srcRoot)
	}
}

// Switching an existing SMB target to local has to clear host, share and
// username, which the server rejects if they survive. This is the payload the
// modal sends; if it regresses, editing a target's type fails with a
// validation error that reads as if the user did something wrong.
func TestTargetCanBeSwitchedBetweenSMBAndLocal(t *testing.T) {
	h := newHarness(t, nil)

	id := h.createTarget(smbTarget(uniqueName("switcher"), sambaA(t), shareCredentialed, userName, userPassword))

	local := t.TempDir()
	status, body := h.do(http.MethodPatch, "/api/targets/"+id, localTarget("switched", local))
	if status != http.StatusOK {
		t.Fatalf("switching SMB → local = %d, want 200: %v", status, body)
	}
	if got := body["type"]; got != "local" {
		t.Fatalf("type after the switch = %v, want local", got)
	}
	if got := body["local_path"]; got != local {
		t.Fatalf("local_path = %v, want %q", got, local)
	}
	// The credentials must be gone: a stored password with no username is a
	// state the server refuses if the target is ever switched back.
	if got, _ := body["has_password"].(bool); got {
		t.Fatal("the password survived the switch to a local target")
	}
	if got, _ := body["host"].(string); got != "" {
		t.Fatalf("host survived the switch: %q", got)
	}

	// And back again.
	back := smbTarget("switched-back", sambaA(t), shareGuest, "", "")
	back["local_path"] = ""
	if status, body := h.do(http.MethodPatch, "/api/targets/"+id, back); status != http.StatusOK {
		t.Fatalf("switching local → SMB = %d, want 200: %v", status, body)
	}
}

// The admin password has no policy: a blank one is a supported choice for a
// closed network (README, "Admin password"). This is the whole flow through
// the API — set it, then actually sign in with it — because a blank password
// that could be set but not used would be worse than refusing it.
func TestBlankAndShortAdminPasswordsWork(t *testing.T) {
	for _, password := range []string{"", "x", "ab"} {
		t.Run("password="+password, func(t *testing.T) {
			h := newHarnessNoSignIn(t)

			status, body := h.do(http.MethodPost, "/api/auth/setup", map[string]any{"password": password})
			if status != http.StatusOK {
				t.Fatalf("setup with %q = %d, want 200: %v", password, status, body)
			}

			// Setup must close behind us, or anyone could claim the instance.
			if status, body := h.do(http.MethodPost, "/api/auth/setup",
				map[string]any{"password": "someone-else"}); status != http.StatusConflict {
				t.Fatalf("second setup = %d, want 409: %v", status, body)
			}

			// A fresh client, so the session cookie from setup cannot mask a
			// broken login.
			fresh := h.anonJar(t)
			status, body = fresh.do(http.MethodPost, "/api/auth/login", map[string]any{"password": password})
			if status != http.StatusOK {
				t.Fatalf("login with %q = %d, want 200: %v", password, status, body)
			}
			if status, body := fresh.do(http.MethodGet, "/api/targets", nil); status != http.StatusOK {
				t.Fatalf("after login, GET /api/targets = %d, want 200: %v", status, body)
			}

			// Still a real credential: the wrong password is refused.
			other := h.anonJar(t)
			if status, _ := other.do(http.MethodPost, "/api/auth/login",
				map[string]any{"password": password + "-wrong"}); status != http.StatusUnauthorized {
				t.Fatalf("login with the wrong password = %d, want 401", status)
			}
		})
	}
}
