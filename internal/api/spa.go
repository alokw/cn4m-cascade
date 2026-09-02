package api

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/alokw/cn4m-cascade/web"
)

// spaHandler serves the embedded React build (SPEC.md §9). It is mounted
// outside the session guard: the login screen is part of the SPA, so the shell
// has to load before anyone can authenticate.
//
// Three rules, and the first is the one that matters for debugging:
//
//  1. A request under the asset directory is served from the build or 404s. It
//     must never fall through to index.html — a mistyped or stale asset URL
//     would then return HTML with a 200, which the browser tries to parse as
//     JavaScript and reports as a syntax error somewhere unrelated. Failing
//     loudly here turns that into an obvious 404.
//  2. Any other GET falls back to index.html, so deep links like /runs/{id}
//     work on a cold load rather than only via client-side navigation.
//  3. A non-GET to an unrouted path is a 405, not the shell. A mistyped API
//     path should not look like it succeeded.
func (s *Server) spaHandler() http.Handler {
	build, err := web.Build()
	if err != nil {
		s.log.Error("the embedded web build is unavailable; serving a placeholder", "error", err)
	}
	return s.spaFrom(build)
}

// spaFrom is spaHandler over an arbitrary build, so the routing rules can be
// tested without running a frontend build. A nil build — or one with no
// index.html — degrades to a legible placeholder rather than a crash: the API
// is still perfectly usable without the UI.
func (s *Server) spaFrom(build fs.FS) http.Handler {
	if build == nil || !exists(build, "/index.html") {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "The web interface was not built into this binary. Run `make web-build`.",
				http.StatusServiceUnavailable)
		})
	}

	files := http.FileServer(http.FS(build))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed",
				"That path does not accept this method.", "")
			return
		}

		clean := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))

		if strings.HasPrefix(clean, "/"+web.AssetDir+"/") {
			if !exists(build, clean) {
				http.NotFound(w, r)
				return
			}
			// Hashed filenames from the bundler make these immutable.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			files.ServeHTTP(w, r)
			return
		}

		if clean != "/" && exists(build, clean) {
			files.ServeHTTP(w, r)
			return
		}

		s.serveShell(w, r, build)
	})
}

// serveShell writes index.html for a route the SPA owns.
func (s *Server) serveShell(w http.ResponseWriter, r *http.Request, build fs.FS) {
	index, err := fs.ReadFile(build, "index.html")
	if err != nil {
		s.log.Error("could not read the embedded index.html", "error", err)
		http.Error(w, "The web interface is unavailable.", http.StatusInternalServerError)
		return
	}

	// The shell is rewritten by every deploy and carries no hash, so it must
	// not be cached; the assets it references are immutable and are.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := w.Write(index); err != nil {
		s.log.Debug("could not write the shell", "error", err)
	}
}

// exists reports whether a path resolves to a regular file in the build.
func exists(build fs.FS, clean string) bool {
	info, err := fs.Stat(build, strings.TrimPrefix(clean, "/"))
	return err == nil && !info.IsDir()
}
