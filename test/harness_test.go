//go:build integration

// Package test holds the integration tests for the Samba harness
// (docker-compose.test.yml). They run inside the dev container, which is the
// only place with a kernel that can mount CIFS and with mount.cifs installed.
package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alokw/cn4m-cascade/internal/api"
	"github.com/alokw/cn4m-cascade/internal/health"
	"github.com/alokw/cn4m-cascade/internal/mountmgr"
	"github.com/alokw/cn4m-cascade/internal/notify"
	"github.com/alokw/cn4m-cascade/internal/runner"
	"github.com/alokw/cn4m-cascade/internal/scheduler"
	"github.com/alokw/cn4m-cascade/internal/secrets"
	"github.com/alokw/cn4m-cascade/internal/storage"
	"github.com/alokw/cn4m-cascade/internal/store"
)

// Fixtures seeded by test/samba/entrypoint.sh.
const (
	shareCredentialed = "private"
	shareGuest        = "public"
	fixtureFile       = "hello.txt"
	fixtureUnicode    = "ünïcodé-📁.txt"
	fixtureDir        = "nested"

	userName     = "syncuser"
	userPassword = "syncpass"
	otherName    = "otheruser"
	otherPass    = "otherpass"
)

// adminPassword is what the harness sets up and signs in with. Every API
// route except /api/auth/* and /healthz needs a session from Phase 4a onwards.
const adminPassword = "integration-harness-password"

// harness is a fully wired server backed by the real ExecMounter.
type harness struct {
	t      *testing.T
	server *httptest.Server
	// client carries the session cookie. Tests that need to check the
	// unauthenticated behaviour use anonClient instead.
	client     *http.Client
	anonClient *http.Client
	db         *store.DB
	mounts     *mountmgr.Manager
	runner     *runner.Runner
	scheduler  *scheduler.Scheduler
	api        *api.Server
	logs       *syncBuffer
	mountRoot  string
}

// syncBuffer collects log output so tests can assert that credentials never
// reach it (CLAUDE.md: never put credentials in logs).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// newHarness builds a server with a temporary database and mount root.
func newHarness(t *testing.T, tune func(*mountmgr.Config)) *harness {
	t.Helper()

	h := buildHarness(t, tune)
	h.signIn()
	return h
}

// newHarnessNoSignIn is newHarness without first-run setup, for the tests that
// need to drive setup and login themselves.
func newHarnessNoSignIn(t *testing.T) *harness {
	t.Helper()
	return buildHarness(t, nil)
}

// anonJar returns a second client against the same server with its own empty
// cookie jar, so a test can prove a login works rather than riding the session
// an earlier call already established.
func (h *harness) anonJar(t *testing.T) *harness {
	t.Helper()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("building a cookie jar: %v", err)
	}
	client := h.server.Client()
	client.Jar = jar

	fresh := *h
	fresh.client = client
	return &fresh
}

func buildHarness(t *testing.T, tune func(*mountmgr.Config)) *harness {
	t.Helper()

	dir := t.TempDir()

	// The mount root deliberately does NOT live under t.TempDir(), and this is
	// not tidiness — it is the difference between a dirty /tmp and a destroyed
	// share.
	//
	// t.TempDir() registers a RemoveAll of everything beneath it. Shares are
	// mounted at <root>/<target-id>, so if a mount is still attached when that
	// cleanup runs, RemoveAll walks *into* the live share and deletes the
	// server's files. That is not hypothetical: it silently emptied
	// //172.28.0.10/private on 2026-09-05, .seeded and all, and left no trace
	// beyond one unrelated-looking error about the *other* mount, whose server
	// happened to be blackholed and so refused the same recursion.
	//
	// A mount outliving its test is a normal outcome here, not a bug: an
	// unmount abandoned on its deadline is exactly what mountmgr is designed to
	// do when a server dies (D-80), and the whole point of that design is that
	// the kernel may still hold the mount afterwards. So the cleanup below must
	// be safe in that state rather than assuming it away.
	mountRoot, err := os.MkdirTemp("", "cn4m-mnt-")
	if err != nil {
		t.Fatalf("creating the mount root: %v", err)
	}
	t.Cleanup(func() { removeMountRoot(t, mountRoot) })

	db, err := store.Open(context.Background(), filepath.Join(dir, "cn4m-cascade.db"))
	if err != nil {
		t.Fatalf("opening the database: %v", err)
	}

	box, err := secrets.NewBox("integration-test-encryption-key")
	if err != nil {
		t.Fatalf("building the secret box: %v", err)
	}

	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(io.MultiWriter(logs, os.Stderr), &slog.HandlerOptions{Level: slog.LevelDebug}))

	cfg := mountmgr.Config{
		MountRoot:      mountRoot,
		CredsDir:       filepath.Join(dir, "creds"),
		MountTimeout:   20 * time.Second,
		StatFSTimeout:  5 * time.Second,
		UnmountTimeout: 15 * time.Second,
		IdleGrace:      60 * time.Second,
		Params:         mountmgr.Params{UID: os.Getuid(), GID: os.Getgid()},
	}
	if tune != nil {
		tune(&cfg)
	}

	healthc := health.NewCache()
	mounts := mountmgr.New(cfg, mountmgr.NewExecMounter(), db, func(_ context.Context, tgt *store.Target) (string, error) {
		return box.Decrypt(tgt.PasswordEncrypted)
	}, healthc, log)
	mounts.Start()

	provider := storage.NewProvider(mounts, healthc)
	runs := runner.New(db, provider, log)

	// Outbound callbacks, wired exactly as main.go wires them so the
	// integration tests exercise the real delivery path rather than a stub.
	notifier := notify.New(db, box.Decrypt, log)
	notifyCtx, stopNotify := context.WithCancel(context.Background())
	notifier.Start(notifyCtx)
	runs.SetNotifier(notifier)
	apiSrv := api.NewServer(db, provider, mounts, healthc, box, runs, log)

	// The event hub and session sweeper are background work the server owns;
	// without Start the WebSocket feed never broadcasts.
	apiCtx, stopAPI := context.WithCancel(context.Background())
	apiSrv.Start(apiCtx)

	srv := httptest.NewServer(apiSrv.Handler())

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("building a cookie jar: %v", err)
	}
	authed := srv.Client()
	authed.Jar = jar

	// Built but deliberately not Started: tests drive Tick with an explicit
	// time instead of waiting for wall-clock minutes to pass. The one test
	// that does want the real loop starts it itself.
	sched := scheduler.New(db, runs, log)

	h := &harness{
		t: t, server: srv, client: authed, anonClient: &http.Client{},
		db: db, mounts: mounts, runner: runs, scheduler: sched, api: apiSrv,
		logs: logs, mountRoot: mountRoot,
	}

	t.Cleanup(func() {
		stopAPI()
		apiSrv.Stop()
		srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// Stop any run still going before the mounts it uses go away.
		if err := runs.Shutdown(ctx); err != nil {
			t.Logf("run shutdown: %v", err)
		}
		if err := mounts.Shutdown(ctx); err != nil {
			t.Logf("mount shutdown: %v", err)
		}
		// Its own deadline, not the one above. Unmounting a blackholed server
		// can consume the whole 30s, and passing the exhausted context on made
		// the notifier report "context deadline exceeded" for having been
		// given no time rather than for taking too long — a real failure
		// message about an imaginary failure.
		notifyCtx, cancelNotify := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelNotify()
		if err := notifier.Stop(notifyCtx); err != nil {
			t.Logf("notifier shutdown: %v", err)
		}
		stopNotify()
		db.Close()
	})
	return h
}

// do issues an API request and decodes the JSON response.
func (h *harness) do(method, path string, body any) (int, map[string]any) {
	h.t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("encoding the request body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, h.server.URL+path, reader)
	if err != nil {
		h.t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("reading the response: %v", err)
	}
	if len(raw) == 0 {
		return resp.StatusCode, nil
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		h.t.Fatalf("%s %s returned non-JSON body %q", method, path, raw)
	}
	return resp.StatusCode, decoded
}

// createTarget POSTs a target and returns its ID.
func (h *harness) createTarget(payload map[string]any) string {
	h.t.Helper()

	status, body := h.do(http.MethodPost, "/api/targets", payload)
	if status != http.StatusCreated {
		h.t.Fatalf("POST /api/targets = %d, want 201: %v", status, body)
	}
	id, _ := body["id"].(string)
	if id == "" {
		h.t.Fatalf("created target has no id: %v", body)
	}
	return id
}

// smbTarget is the payload for a credentialed share on one of the servers.
func smbTarget(name, host, share, username, password string) map[string]any {
	return map[string]any{
		"name":     name,
		"type":     "smb",
		"host":     host,
		"share":    share,
		"username": username,
		"password": password,
	}
}

// errorOf pulls the error envelope out of a failed response.
func errorOf(t *testing.T, body map[string]any) (message, kind, code string) {
	t.Helper()
	obj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("response has no error envelope: %v", body)
	}
	message, _ = obj["message"].(string)
	kind, _ = obj["kind"].(string)
	code, _ = obj["code"].(string)
	return message, kind, code
}

// mountedUnder reports the CIFS mountpoints currently under root.
func mountedUnder(t *testing.T, root string) []string {
	t.Helper()

	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatalf("reading the kernel mount table: %v", err)
	}

	var found []string
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if strings.HasPrefix(fields[4], strings.TrimSuffix(root, "/")+"/") {
			found = append(found, fields[4])
		}
	}
	return found
}

// env reads a harness environment variable, skipping the test if the harness
// is not running.
func env(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s is not set: start the harness with `make harness-up`", name)
	}
	return v
}

func sambaA(t *testing.T) string    { return env(t, "CN4M_CASCADE_TEST_SAMBA_A") }
func sambaB(t *testing.T) string    { return env(t, "CN4M_CASCADE_TEST_SAMBA_B") }
func offlineIP(t *testing.T) string { return env(t, "CN4M_CASCADE_TEST_OFFLINE") }

func uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// signIn completes first-run setup and logs the harness client in, so every
// later request carries a session cookie.
func (h *harness) signIn() {
	h.t.Helper()

	status, body := h.do(http.MethodPost, "/api/auth/setup", map[string]any{"password": adminPassword})
	if status != http.StatusOK {
		h.t.Fatalf("POST /api/auth/setup = %d, want 200: %v", status, body)
	}
}

// doAnon issues a request without the session cookie, for asserting that the
// API is actually closed to strangers.
func (h *harness) doAnon(method, path string, body any) (int, map[string]any) {
	h.t.Helper()

	saved := h.client
	h.client = h.anonClient
	defer func() { h.client = saved }()

	return h.do(method, path, body)
}

// removeMountRoot tears down a harness's mount root without ever recursing
// into it.
//
// The rule it enforces is simple and absolute: **nothing here may use
// os.RemoveAll**. The children of the mount root are mountpoints, and a
// recursive delete cannot tell a mountpoint whose server is live from an empty
// directory — it just walks in and deletes the share. os.Remove on a directory
// succeeds only when that directory is empty, so against a still-attached
// mount it returns EBUSY or ENOTEMPTY and touches nothing. The safety is
// structural rather than a check that could be raced or forgotten.
//
// A leftover directory in /tmp is the correct outcome when a mount is still
// held. `make harness-clean` sweeps them, and a dirty /tmp is recoverable in a
// way that a deleted share is not.
func removeMountRoot(t *testing.T, root string) {
	t.Helper()

	// Reading the root itself is safe: it is an ordinary local directory. Its
	// children are the mountpoints, and nothing below reads *into* them.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Logf("mount root %s: %v (leaving it in place)", root, err)
		return
	}

	held := 0
	for _, e := range entries {
		// Bounded because rmdir(2) on a mountpoint whose server has gone can
		// block in the kernel, and a cleanup that hangs is how a suite stops
		// reporting anything at all. Abandoning the goroutine is the accepted
		// cost (CLAUDE.md); the buffered channel lets it finish whenever the
		// kernel returns.
		done := make(chan error, 1)
		path := filepath.Join(root, e.Name())
		go func() { done <- os.Remove(path) }()

		select {
		case err := <-done:
			if err != nil {
				held++
			}
		case <-time.After(5 * time.Second):
			held++
		}
	}

	if held > 0 {
		t.Logf("%d mountpoint(s) still held under %s; leaving it for `make harness-clean` "+
			"rather than deleting through a live mount", held, root)
		return
	}
	if err := os.Remove(root); err != nil {
		t.Logf("mount root %s: %v", root, err)
	}
}

// The harness cleanup must never delete through a mountpoint.
//
// This pins the property with an ordinary non-empty directory standing in for
// a live mount, because the two are indistinguishable to a recursive delete —
// which is precisely the bug. os.Remove refuses both; os.RemoveAll empties
// both, and against a real mount "both" means the server's files.
//
// Verified to fail against the previous implementation, where the mount root
// lived under t.TempDir() and was removed recursively.
func TestMountRootCleanupNeverDeletesThroughAMountpoint(t *testing.T) {
	root, err := os.MkdirTemp("", "cn4m-mnt-test-")
	if err != nil {
		t.Fatalf("creating a mount root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	// Stands in for a mounted share: a mountpoint directory with content
	// underneath that belongs to somebody else.
	mountpoint := filepath.Join(root, "0123456789abcdef")
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		t.Fatalf("creating the mountpoint: %v", err)
	}
	victim := filepath.Join(mountpoint, "hello.txt")
	if err := os.WriteFile(victim, []byte("the server's file\n"), 0o644); err != nil {
		t.Fatalf("seeding the fixture: %v", err)
	}

	removeMountRoot(t, root)

	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("cleanup deleted a file underneath a mountpoint: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("cleanup removed a mount root that still held a mountpoint: %v", err)
	}

	// And with nothing held, it must still tidy up after itself — a cleanup
	// that never removes anything would pass the check above trivially.
	if err := os.RemoveAll(mountpoint); err != nil {
		t.Fatalf("clearing the stand-in mountpoint: %v", err)
	}
	removeMountRoot(t, root)
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("an empty mount root survived cleanup: %v", err)
	}
}
