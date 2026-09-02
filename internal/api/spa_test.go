package api

import (
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/alokw/cn4m-cascade/web"
)

// synthetic build stands in for a real Vite output so these tests are
// deterministic and do not require `npm run build` to have happened.
func testBuild() fs.FS {
	return fstest.MapFS{
		"index.html":                 {Data: []byte("<!doctype html><div id=\"root\"></div>")},
		web.AssetDir + "/app-abc.js": {Data: []byte("console.log(1)")},
		"favicon.svg":                {Data: []byte("<svg/>")},
	}
}

func testServer() *Server {
	return &Server{log: slog.New(slog.DiscardHandler)}
}

func TestSPARouting(t *testing.T) {
	s := testServer()
	h := s.spaFrom(testBuild())

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantBody   string
	}{
		{"root serves the shell", http.MethodGet, "/", http.StatusOK, "<div id=\"root\">"},
		{"a real asset is served", http.MethodGet, "/" + web.AssetDir + "/app-abc.js", http.StatusOK, "console.log(1)"},
		{"a real root file is served", http.MethodGet, "/favicon.svg", http.StatusOK, "<svg/>"},

		// The rule that matters: a missing asset must 404 rather than fall
		// back to the shell. Returning HTML with a 200 for a .js URL makes the
		// browser report a syntax error somewhere unrelated to the real fault.
		{"a missing asset 404s", http.MethodGet, "/" + web.AssetDir + "/gone.js", http.StatusNotFound, ""},

		// Deep links must survive a cold load, not only client-side routing.
		{"a deep link serves the shell", http.MethodGet, "/runs/abc123", http.StatusOK, "<div id=\"root\">"},
		{"a nested deep link serves the shell", http.MethodGet, "/jobs/1/edit", http.StatusOK, "<div id=\"root\">"},

		// A mistyped API path should not look like it worked.
		{"a non-GET is refused", http.MethodPost, "/whatever", http.StatusMethodNotAllowed, ""},
		{"a non-GET on a deep link is refused", http.MethodDelete, "/runs/abc", http.StatusMethodNotAllowed, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))

			if rec.Code != tc.wantStatus {
				t.Fatalf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.wantStatus)
			}
			if tc.wantBody != "" && !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("%s %s body = %q, want it to contain %q", tc.method, tc.path, rec.Body.String(), tc.wantBody)
			}
		})
	}
}

// A path that escapes the build must not reach the filesystem. http.FileServer
// cleans paths itself, but the asset branch does its own lookup, so the
// traversal has to be refused on both routes.
//
// Asserting only "the body does not contain /etc/passwd" would pass even if
// every missing asset fell through to the shell — the exact bug the asset
// branch exists to prevent — so the status code is asserted too.
func TestSPARefusesTraversal(t *testing.T) {
	s := testServer()
	h := s.spaFrom(testBuild())

	tests := []struct {
		path       string
		wantStatus int
	}{
		// Under the asset directory after cleaning, so the 404 rule applies.
		{"/" + web.AssetDir + "/../" + web.AssetDir + "/gone.js", http.StatusNotFound},
		// These clean to somewhere outside the asset directory, so they take
		// the shell fallback — but must serve the shell, never a real file.
		{"/" + web.AssetDir + "/../../etc/passwd", http.StatusOK},
		{"/../embed.go", http.StatusOK},
		// %2f is decoded into Path before cleaning, so this also lands outside
		// the asset directory and takes the shell fallback rather than 404ing.
		{"/" + web.AssetDir + "/..%2f..%2fetc%2fpasswd", http.StatusOK},
	}

	for _, tc := range tests {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))

		if rec.Code != tc.wantStatus {
			t.Fatalf("GET %s = %d, want %d", tc.path, rec.Code, tc.wantStatus)
		}
		body := rec.Body.String()
		if strings.Contains(body, "root:") || strings.Contains(body, "package web") {
			t.Fatalf("GET %s escaped the build: %q", tc.path, body)
		}
		if tc.wantStatus == http.StatusOK && !strings.Contains(body, `id="root"`) {
			t.Fatalf("GET %s returned %q, want the shell", tc.path, body)
		}
	}
}

// The shell must not be cached — it is rewritten by every deploy and carries
// no hash — while the hashed assets it references should be cached hard.
func TestSPACacheHeaders(t *testing.T) {
	s := testServer()
	h := s.spaFrom(testBuild())

	shell := httptest.NewRecorder()
	h.ServeHTTP(shell, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := shell.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("shell Cache-Control = %q, want no-cache", got)
	}

	asset := httptest.NewRecorder()
	h.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/"+web.AssetDir+"/app-abc.js", nil))
	if got := asset.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Fatalf("asset Cache-Control = %q, want it to be immutable", got)
	}
}

// A binary built without a frontend build must degrade to a legible message
// rather than crashing or serving nothing: the API is still fully usable.
func TestSPAWithoutABuildExplainsItself(t *testing.T) {
	s := testServer()
	h := s.spaFrom(fstest.MapFS{}) // no index.html

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "make web-build") {
		t.Fatalf("the placeholder does not say how to fix it: %q", rec.Body.String())
	}
}
