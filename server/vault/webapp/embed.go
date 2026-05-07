// Package webapp serves the React account panel to browsers visiting
// the cloud vault. The build pipeline copies pnpm's `dist/` output
// into ./static/ before `go build`, so the panel travels with the
// vault binary as a single deployment artifact.
//
// In dev (no `pnpm build` has been run, ./static/ only contains the
// .gitkeep), Available() returns false and the vault's home page
// keeps redirecting to /login instead of /app. That keeps `go test`
// and a fresh checkout working without forcing every contributor to
// run the React build.
package webapp

import (
	"bytes"
	"embed"
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed all:static
var staticFS embed.FS

// indexFile is the SPA entry point. Stored as a constant so tests can
// double-check we're serving the file we expect on a path miss.
const indexFile = "static/index.html"

// Available reports whether the React build was bundled into this
// binary. Empty when running off a fresh checkout that hasn't run the
// build pipeline yet.
func Available() bool {
	_, err := staticFS.ReadFile(indexFile)
	return err == nil
}

// Handler returns an http.Handler that serves the React build. Two
// path prefixes resolve to the SPA entry (index.html):
//   - /app, /app/*    — the desktop-app-in-browser surface (legacy)
//   - /admin, /admin/* — the cloud-vault admin SPA (login/devices/pair/password)
//
// Both prefixes share the same React bundle; the bundle's main.tsx
// dispatches to the appropriate root component based on
// window.location.pathname. /assets/* serves hashed asset bundles.
//
// Mount with main.go:
//
//	rootMux.Handle("/app", appHandler)
//	rootMux.Handle("/app/", appHandler)
//	rootMux.Handle("/admin", appHandler)
//	rootMux.Handle("/admin/", appHandler)
//	rootMux.Handle("/assets/", appHandler)
//
// Anything outside those prefixes falls through to the existing
// frontend httpapi / Web UI handlers — appHandler 404s on unknown
// paths so it doesn't accidentally swallow them.
func Handler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// fs.Sub on a static path can't fail in practice; surface as
		// a 500 if it ever does so the operator notices.
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "vault webapp embed broken: "+err.Error(),
				http.StatusInternalServerError)
		})
	}
	indexBytes, _ := fs.ReadFile(sub, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /app, /admin, or any descendant → serve index.html; React
		// routes internally so we don't try to map the URL to a file.
		if isSPARoute(r.URL.Path) {
			if len(indexBytes) == 0 {
				http.Error(w, "Web UI not bundled into this build (run `pnpm build` first)",
					http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			// No long cache — index.html points at hashed assets, but
			// the SPA shell itself should always be fresh so newly
			// deployed bundles are picked up promptly.
			w.Header().Set("Cache-Control", "no-cache")
			http.ServeContent(w, r, "index.html", emptyTime, bytes.NewReader(indexBytes))
			return
		}
		// /assets/* (or any other top-level static) → serve from the
		// embed. http.FS handles the hashed-asset routing.
		clean := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		f, err := sub.Open(clean)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = f.Close()
		// Long cache for hashed asset filenames; vite emits content
		// hashes by default so the URL itself changes when content does.
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
		}
		http.FileServer(http.FS(sub)).ServeHTTP(w, r)
	})
}

// emptyTime is a zero time.Time — http.ServeContent omits the
// Last-Modified header when the value is zero, which matches what we
// want (the embed loses the original mtimes anyway).
var emptyTime time.Time

// isSPARoute reports whether the URL path should resolve to the SPA
// entry (index.html). The bundle's main.tsx dispatches based on
// window.location.pathname:
//   - /app, /app/*    — desktop SPA in browser (legacy embed prefix)
//   - /admin, /admin/* — admin bootstrap surface (setup + login only after
//     the vault-app-admin-merge migration; devices/pair/password moved out)
//   - /, /devices, /pair, /password — the merged App with admin sidebar
//     items. These are gated by httpserver.gateAppRoute before the request
//     reaches us, so unsigned visitors don't make it here.
func isSPARoute(p string) bool {
	switch p {
	case "/", "/app", "/app/", "/admin", "/admin/",
		"/devices", "/pair", "/password":
		return true
	}
	return strings.HasPrefix(p, "/app/") || strings.HasPrefix(p, "/admin/")
}
