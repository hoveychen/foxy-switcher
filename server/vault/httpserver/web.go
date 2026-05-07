package httpserver

import (
	"net/http"
	"net/url"
)

// RegisterWebRoutes wires the browser-facing routes for the merged
// App+admin SPA. After vault-app-admin-merge there's a single sidebar
// layout (dashboard / accounts / activity / devices / pair / password
// / settings) served from the top-level path; only the bootstrap pages
// (setup + login) live under /admin/*. This file:
//
//   - Routes /, /devices, /pair, /password to the SPA (gated by an
//     admin session — unsigned visitors land on /admin/login, brand-new
//     vaults on /admin/setup).
//   - Keeps /admin/setup and /admin/login as the unauthenticated
//     bootstrap surface (handled by webapp.Handler() at /admin/* in
//     main.go, no extra routes needed here).
//   - 301-redirects the legacy /admin/devices, /admin/pair,
//     /admin/password to their new top-level paths.
//   - 301-redirects the pre-React /setup, /login bookmarks to their
//     /admin/* counterparts so old links keep working.
//
// JSON API stays at /admin/api/* (registered via RegisterAPIRoutes).
func (s *Server) RegisterWebRoutes(mux *http.ServeMux) {
	// Pre-React bookmark redirects. /setup and /login still belong on the
	// /admin/* bootstrap surface because they're the unauthenticated entry
	// points; preserve the original query string (notably ?next=).
	mux.HandleFunc("GET /setup", redirectTo("/admin/setup"))
	mux.HandleFunc("GET /login", redirectToLogin)

	// Legacy admin paths → new top-level paths. The merged sidebar serves
	// devices/pair/password directly under /; the /admin/<name> URLs from
	// the old top-bar layout stay reachable via 301 so existing bookmarks
	// and pair-init verification URLs ("…/admin/pair?code=…") still land
	// users on the right page.
	mux.HandleFunc("GET /admin/devices", redirectTo("/devices"))
	mux.HandleFunc("GET /admin/pair", redirectTo("/pair"))
	mux.HandleFunc("GET /admin/password", redirectTo("/password"))

	// Top-level App routes. gateAppRoute requires an admin session and
	// hands off to webapp.Handler() (the React bundle); webapp's
	// isSPARoute whitelist recognizes these paths and returns index.html.
	// Dashboard pushes to "/" (not "/dashboard"), so it's covered by the
	// "/" handler.
	gated := s.gateAppRoute()
	mux.Handle("GET /{$}", gated)
	mux.Handle("GET /accounts", gated)
	mux.Handle("GET /activity", gated)
	mux.Handle("GET /settings", gated)
	mux.Handle("GET /devices", gated)
	mux.Handle("GET /pair", gated)
	mux.Handle("GET /password", gated)
}

// redirectTo returns a handler that 301-redirects to a fixed path,
// preserving the request's query string (useful for /pair?code=…).
func redirectTo(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dest := target
		if q := r.URL.RawQuery; q != "" {
			dest += "?" + q
		}
		http.Redirect(w, r, dest, http.StatusMovedPermanently)
	}
}

// redirectToLogin sends old /login URLs (with optional ?next=) to the
// SPA login route. The `next` parameter is preserved so the SPA can
// land the user on the right post-login page.
func redirectToLogin(w http.ResponseWriter, r *http.Request) {
	dest := "/admin/login"
	if next := r.URL.Query().Get("next"); next != "" {
		dest += "?next=" + next
	}
	http.Redirect(w, r, dest, http.StatusMovedPermanently)
}

// gateAppRoute serves the React SPA when the visitor has a valid admin
// session. Brand-new vaults (no password set) bounce to /admin/setup;
// signed-out visitors to /admin/login?next=<original> so post-login
// returns them where they meant to go. Without an embedded SPA bundle
// (e.g. a `go test` build that didn't run `pnpm build`) we fall back
// to handleRoot, which keeps the pre-merge redirect behavior.
func (s *Server) gateAppRoute() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.AppHandler == nil {
			s.handleRoot(w, r)
			return
		}
		hasPw, err := s.st.HasPassword(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !hasPw {
			http.Redirect(w, r, "/admin/setup", http.StatusSeeOther)
			return
		}
		if !s.hasSession(r) {
			dest := "/admin/login"
			// Skip ?next when landing at "/" since the SPA's default route
			// is dashboard, which is already where post-login lands.
			if r.URL.Path != "/" {
				dest += "?next=" + url.QueryEscape(r.URL.RequestURI())
			}
			http.Redirect(w, r, dest, http.StatusSeeOther)
			return
		}
		s.AppHandler.ServeHTTP(w, r)
	}
}

// handleRoot is the fallback for "/" when no SPA bundle is embedded.
// It keeps the pre-merge bookmark-bouncing behavior so a vault built
// without `pnpm build` still routes signed-in admins toward the
// devices page (now /devices instead of /admin/devices).
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	hasPw, err := s.st.HasPassword(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	switch {
	case !hasPw:
		http.Redirect(w, r, "/admin/setup", http.StatusSeeOther)
	case s.hasSession(r):
		http.Redirect(w, r, "/devices", http.StatusSeeOther)
	default:
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
	}
}
