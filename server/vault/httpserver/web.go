package httpserver

import (
	"net/http"
)

// RegisterWebRoutes used to host a server-rendered admin Web UI
// (Go html/template forms at /setup, /login, /pair, /devices,
// /password). That UI has been retired in favor of the React admin
// SPA mounted at /admin/* by server/vault/webapp. This file now only
// keeps redirect stubs so old bookmarks (and the agent's verification
// URL "/pair" surfaced in pair-init responses) keep landing somewhere
// useful.
//
// The actual admin functionality is served by:
//   - JSON API at /admin/api/*  (registered in api.go via RegisterAPIRoutes)
//   - SPA pages at /admin/*     (served by webapp.Handler())
func (s *Server) RegisterWebRoutes(mux *http.ServeMux) {
	// Bookmark-friendly redirects from the old paths to the SPA. 301
	// (Moved Permanently) so browsers cache the redirect and old links
	// stop hitting the server entirely.
	mux.HandleFunc("GET /setup", redirectTo("/admin/setup"))
	mux.HandleFunc("GET /login", redirectToLogin)
	mux.HandleFunc("GET /devices", redirectTo("/admin/devices"))
	mux.HandleFunc("GET /pair", redirectToPair)
	mux.HandleFunc("GET /password", redirectTo("/admin/password"))

	// Root: bounce signed-in users to /admin/devices, fresh visitors to
	// the appropriate admin entry. We don't 301 here because the
	// destination depends on session state and shouldn't be cached.
	mux.HandleFunc("GET /{$}", s.handleRoot)
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

// redirectToPair sends /pair?code=… (the URL the agent surfaces in its
// verification message) to /admin/pair?code=… so the SPA can pick up
// the code from its own location.
func redirectToPair(w http.ResponseWriter, r *http.Request) {
	dest := "/admin/pair"
	if q := r.URL.RawQuery; q != "" {
		dest += "?" + q
	}
	http.Redirect(w, r, dest, http.StatusMovedPermanently)
}

// handleRoot picks the SPA entry for "/". Signed-in admin → devices,
// otherwise login (or setup, if the vault has no password yet).
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
		http.Redirect(w, r, "/admin/devices", http.StatusSeeOther)
	default:
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
	}
}
