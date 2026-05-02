package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
	vaultauth "github.com/hoveychen/foxy-switcher/server/vault/auth"
)

// PairingTTL is how long a pair-init code is valid. 10 minutes lines up
// with most device-flow conventions (long enough to switch tabs and read
// a code off another screen, short enough that an abandoned session
// doesn't accumulate clutter).
const PairingTTL = 10 * time.Minute

// VerificationPath is where the user types the user_code. The agent
// surfaces this URL to the human via stderr / pair-init response so they
// know where to go.
const VerificationPath = "/pair"

// agentDeviceCtxKey carries the device ID attached by the Bearer
// middleware. Protected handlers can call deviceFromCtx to learn who
// authenticated, but Step 3 doesn't yet need that — the lease APIs already
// take device_id in the body.
type agentDeviceCtxKey struct{}

// requireBearer is the middleware wrapper protected agent routes use.
// Internally it just delegates to BearerAuth so the agent surface and
// any external use of the same auth share a single implementation.
func (s *Server) requireBearer(next http.Handler) http.Handler {
	return BearerAuth(s.st)(next)
}

// BearerAuth is a stand-alone middleware factory exposed for callers
// outside vault/httpserver — main wraps the frontend httpapi /api/*
// surface with this when running in --mode=vault, so a vault on the
// public internet can't be hit by an unauthenticated client. Combined
// mode skips the wrap because loopback is its own attacker model.
//
// Accepts EITHER a paired-device bearer token in the Authorization
// header OR a Web UI session cookie (so a logged-in browser session
// can also drive /api/* without needing to hold a device token).
// Step 9 added the cookie path to make the embedded React account
// panel possible — a browser deployed at the vault origin uses cookies
// for auth, not Bearer.
//
// On miss / unknown credential: 401 with a JSON error body.
func BearerAuth(st *store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if devID, ok := authenticateRequest(r, st); ok {
				ctx := context.WithValue(r.Context(), agentDeviceCtxKey{}, devID)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			writeError(w, http.StatusUnauthorized, errors.New("authentication required"))
		})
	}
}

// authenticateRequest tries the bearer header first, then the Web UI
// session cookie. Returns the device id (or "session" sentinel for
// cookie auth) and ok=true on any successful match. Writes nothing —
// the middleware decides how to respond.
func authenticateRequest(r *http.Request, st *store.Store) (string, bool) {
	if token, ok := extractBearer(r); ok {
		dev, err := st.FindDeviceByTokenHash(r.Context(), vaultauth.HashToken(token))
		if err == nil {
			_ = st.TouchDevice(r.Context(), dev.ID)
			return dev.ID, true
		}
		if !errors.Is(err, store.ErrNotFound) {
			// SQL error is treated as "not authenticated" — better to 401
			// than 500 from the auth layer; the operator sees the underlying
			// failure in store-layer logs.
			return "", false
		}
		// Fall through and try cookie — agents normally don't have a
		// session cookie, but a misrouted request shouldn't 401 just
		// because both surfaces are mounted on the same listener.
	}
	if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
		if _, err := st.LookupWebSession(r.Context(), c.Value); err == nil {
			// "session" is a sentinel value the per-request context can
			// distinguish from a real device id. Today nothing consumes
			// it; if a route later needs to differentiate, it's a clean
			// hook.
			return "session", true
		}
	}
	return "", false
}

func extractBearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}
