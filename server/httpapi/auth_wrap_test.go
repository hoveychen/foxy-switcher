package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
	vaultauth "github.com/hoveychen/foxy-switcher/server/vault/auth"
	"github.com/hoveychen/foxy-switcher/server/vault/httpserver"
)

// TestVaultModeBearer_GuardsApi locks down the Step 6 promise: when main
// installs httpserver.BearerAuth as a Middleware on httpapi.Server, every
// /api/* call needs a recognised device token. Combined-mode uses the
// same Server type with no Middleware, so the existing tests still
// exercise the open path; this test only asserts the wrapped path.
func TestVaultModeBearer_GuardsApi(t *testing.T) {
	st, _ := newTestStore(t)
	srv := New(st, nil, nil, "")
	srv.Middleware = append(srv.Middleware, httpserver.BearerAuth(st))

	// 1. No Authorization → 401.
	req := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no-auth /api/accounts: got %d want 401, body=%q", w.Code, w.Body.String())
	}

	// 2. Bogus token → 401.
	req = httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bogus-bearer: got %d want 401", w.Code)
	}

	// 3. Real token → 200.
	token := vaultauth.NewToken()
	if err := st.InsertDevice(context.Background(), store.Device{
		ID:        vaultauth.NewID(),
		Name:      "vault-mode-test",
		TokenHash: vaultauth.HashToken(token),
	}); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("real-bearer: got %d want 200, body=%q", w.Code, w.Body.String())
	}
}

// TestVaultModeBearer_AcceptsSessionCookie covers the Step 9 admission
// that an embedded React build, served from the vault's origin, drives
// /api/* with the same Web UI session cookie used by /devices and
// /pair — no bearer token. Without this path, cookie-auth'd browser
// users would 401 on every account fetch.
func TestVaultModeBearer_AcceptsSessionCookie(t *testing.T) {
	st, _ := newTestStore(t)
	srv := New(st, nil, nil, "")
	srv.Middleware = append(srv.Middleware, httpserver.BearerAuth(st))

	// Seed an active session row directly — equivalent to the user
	// having logged in via /login on the Web UI.
	sessID := vaultauth.NewToken()
	expires := time.Now().Add(time.Hour).UnixMilli()
	if err := st.CreateWebSession(context.Background(), sessID, expires); err != nil {
		t.Fatalf("CreateWebSession: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	req.AddCookie(&http.Cookie{Name: httpserver.SessionCookieName, Value: sessID})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("session cookie auth: got %d want 200, body=%q", w.Code, w.Body.String())
	}
}

// TestVaultModeBearer_LeavesCorsAlone confirms the CORS preflight path
// still short-circuits with 204 before the bearer wrap can reject it —
// browsers send OPTIONS without Authorization, and getting 401 here
// would block every cross-origin request from the React build.
func TestVaultModeBearer_LeavesCorsAlone(t *testing.T) {
	st, _ := newTestStore(t)
	srv := New(st, nil, nil, "")
	srv.Middleware = append(srv.Middleware, httpserver.BearerAuth(st))

	req := httptest.NewRequest(http.MethodOptions, "/api/accounts", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS preflight: got %d want 204", w.Code)
	}
}
