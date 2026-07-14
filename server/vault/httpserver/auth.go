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
// middleware. Protected handlers can call DeviceFromContext to learn who
// authenticated; the lease APIs also still take device_id in the body
// for callers that haven't been migrated to ctx-based lookup.
type agentDeviceCtxKey struct{}

// SessionDeviceID is the sentinel ctx value that BearerAuth attaches
// when authentication succeeded via a Web UI session cookie rather than
// a paired-device Bearer token. Callers that key off "is this caller a
// device with leases" (e.g. the multi-device lease badges on
// /api/accounts) compare against this to skip cookie sessions.
const SessionDeviceID = "session"

// DeviceFromContext returns the device ID stored on ctx by BearerAuth.
// ok == false when the request didn't go through BearerAuth (combined
// mode, where loopback is the only attacker model and the local owner
// is implicit). Returns SessionDeviceID for cookie-authenticated
// browser sessions; the device ID otherwise.
func DeviceFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(agentDeviceCtxKey{}).(string)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

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
		if err == nil && dev.DisabledAt == 0 {
			_ = st.TouchDevice(r.Context(), dev.ID)
			return dev.ID, true
		}
		// A suspended device (DisabledAt != 0) matches by hash but is not
		// authenticated: skip TouchDevice so its "last active" stays
		// truthful, and fall through to the cookie path (an agent has no
		// session cookie, so the request ends in 401 until an admin
		// resumes it — no re-pair needed because the row and token_hash
		// are preserved).
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
			// SessionDeviceID is the sentinel cookie-auth callers carry on
			// ctx; lease-aware handlers compare against it to skip
			// "is this caller the lease holder" comparisons.
			return SessionDeviceID, true
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
