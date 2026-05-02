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
// On miss / unknown token: 401 with a JSON error body. On success:
// next handler runs with the device id in request context (key is
// internal — no callers consume it today; the pattern is here so
// rate-limit / audit middleware can grow next to this without an API
// change).
func BearerAuth(st *store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := extractBearer(r)
			if !ok {
				writeError(w, http.StatusUnauthorized, errors.New("missing bearer token"))
				return
			}
			dev, err := st.FindDeviceByTokenHash(r.Context(), vaultauth.HashToken(token))
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					writeError(w, http.StatusUnauthorized, errors.New("unknown device token"))
					return
				}
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			// Best-effort heartbeat for the Devices page; failure here is
			// non-fatal — the request still succeeds.
			_ = st.TouchDevice(r.Context(), dev.ID)
			ctx := context.WithValue(r.Context(), agentDeviceCtxKey{}, dev.ID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
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
