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
	"github.com/alokw/cn4m-cascade/internal/runner"
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

	dir := t.TempDir()
	mountRoot := filepath.Join(dir, "mnt")
	if err := os.MkdirAll(mountRoot, 0o755); err != nil {
		t.Fatalf("creating the mount root: %v", err)
	}

	db, err := store.Open(context.Background(), filepath.Join(dir, "smbsync.db"))
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

	h := &harness{
		t: t, server: srv, client: authed, anonClient: &http.Client{},
		db: db, mounts: mounts, runner: runs, api: apiSrv,
		logs: logs, mountRoot: mountRoot,
	}
	h.signIn()

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

func sambaA(t *testing.T) string    { return env(t, "SMBSYNC_TEST_SAMBA_A") }
func sambaB(t *testing.T) string    { return env(t, "SMBSYNC_TEST_SAMBA_B") }
func offlineIP(t *testing.T) string { return env(t, "SMBSYNC_TEST_OFFLINE") }

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
