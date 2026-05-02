package httpserver

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
	vaultauth "github.com/hoveychen/foxy-switcher/server/vault/auth"
)

// SessionCookieName is the Web UI session cookie. Distinct from the agent
// bearer token so a logged-in admin browser doesn't accidentally
// authenticate as an agent (or vice versa).
const SessionCookieName = "foxy_session"

// SessionTTL is how long a Web UI cookie is good for. 7 days matches the
// usual "remember me" default; the user can /logout earlier.
const SessionTTL = 7 * 24 * time.Hour

// startSession persists a session row and writes the cookie. Caller has
// already verified the password; this just stamps the credential.
func (s *Server) startSession(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	id := vaultauth.NewToken()
	expiresAt := time.Now().Add(SessionTTL)
	if err := s.st.CreateWebSession(ctx, id, expiresAt.UnixMilli()); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    id,
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   int(SessionTTL.Seconds()),
		HttpOnly: true,
		// Secure is set when the request reached us over TLS or behind a
		// trusted reverse proxy that announced X-Forwarded-Proto=https.
		// Combined-mode loopback is plain HTTP, so Secure stays off there
		// and the cookie still works.
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// endSession clears the cookie and deletes the row. Idempotent.
func (s *Server) endSession(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookieName); err == nil {
		_ = s.st.DeleteWebSession(ctx, c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
	})
}

// hasSession reports whether the current request carries a valid session
// cookie. Web UI handlers call this; the convention is "redirect to
// /login when false" (302), not 401 (a browser is the client, not an API).
func (s *Server) hasSession(r *http.Request) bool {
	c, err := r.Cookie(SessionCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	_, err = s.st.LookupWebSession(r.Context(), c.Value)
	return err == nil
}

// requireSession is the middleware wrapper for protected Web UI handlers.
// Unauthenticated requests get a 302 to /login; the original target is
// preserved as ?next=… so post-login lands the user back where they were.
func (s *Server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// First-run: no password set yet → /setup gates everything.
		ok, err := s.st.HasPassword(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !ok {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		if !s.hasSession(r) {
			next := "/login"
			if r.URL.Path != "/" && r.URL.Path != "/login" {
				next += "?next=" + r.URL.Path
			}
			http.Redirect(w, r, next, http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

// notFoundIs reports whether err is store.ErrNotFound — a tiny helper to
// keep the Web UI handlers tidy.
func notFoundIs(err error) bool { return errors.Is(err, store.ErrNotFound) }
