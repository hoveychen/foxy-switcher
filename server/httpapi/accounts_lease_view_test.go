package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
	vaultauth "github.com/hoveychen/foxy-switcher/server/vault/auth"
	"github.com/hoveychen/foxy-switcher/server/vault/httpserver"
)

// TestAccountsLeaseView covers the new per-account lease metadata
// exposed by GET /api/accounts (multi-device-lease-visibility plan):
//
//   - account without an active lease → Lease == nil.
//   - account with an active lease, combined mode (no BearerAuth) →
//     Lease populated; Mine == true (the local owner is implicit).
//   - account with an active lease, vault mode + Bearer auth as the
//     holding device → Mine == true.
//   - same lease, vault mode + Bearer auth as a different device →
//     Mine == false; device_id / device_name populated for the badge.
//   - vault mode + cookie session → Mine == false (web admins are not a
//     device with leases; the badges should render device names).
func TestAccountsLeaseView(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	// Two accounts: alpha (held by dev-self), bravo (held by dev-other).
	alpha := &store.Account{Name: "alpha", AccessToken: "at-a", RefreshToken: "rt-a", Status: "active",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	if err := st.Upsert(ctx, alpha); err != nil {
		t.Fatalf("upsert alpha: %v", err)
	}
	bravo := &store.Account{Name: "bravo", AccessToken: "at-b", RefreshToken: "rt-b", Status: "active",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	if err := st.Upsert(ctx, bravo); err != nil {
		t.Fatalf("upsert bravo: %v", err)
	}
	// charlie has no lease at all.
	charlie := &store.Account{Name: "charlie", AccessToken: "at-c", RefreshToken: "rt-c", Status: "active",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	if err := st.Upsert(ctx, charlie); err != nil {
		t.Fatalf("upsert charlie: %v", err)
	}

	// Two devices with different display names so we can assert
	// device_name routing.
	selfToken := vaultauth.NewToken()
	selfDev := store.Device{
		ID: vaultauth.NewID(), Name: "Self-Mac",
		TokenHash: vaultauth.HashToken(selfToken),
	}
	if err := st.InsertDevice(ctx, selfDev); err != nil {
		t.Fatalf("ins self: %v", err)
	}
	otherToken := vaultauth.NewToken()
	otherDev := store.Device{
		ID: vaultauth.NewID(), Name: "Other-Linux",
		TokenHash: vaultauth.HashToken(otherToken),
	}
	if err := st.InsertDevice(ctx, otherDev); err != nil {
		t.Fatalf("ins other: %v", err)
	}

	if _, err := st.AcquireLease(ctx, "lease-a", alpha.ID, selfDev.ID, time.Minute); err != nil {
		t.Fatalf("acq alpha lease: %v", err)
	}
	if _, err := st.AcquireLease(ctx, "lease-b", bravo.ID, otherDev.ID, time.Minute); err != nil {
		t.Fatalf("acq bravo lease: %v", err)
	}

	type viewByName map[string]accountView
	fetch := func(t *testing.T, srv *Server, prep func(*http.Request)) viewByName {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
		if prep != nil {
			prep(req)
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status: got %d body=%q", w.Code, w.Body.String())
		}
		var out struct {
			Accounts []accountView `json:"accounts"`
		}
		if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		m := make(viewByName, len(out.Accounts))
		for _, v := range out.Accounts {
			m[v.Name] = v
		}
		return m
	}

	// --- (1) combined mode: no BearerAuth wrap ---
	combined := New(st, nil, nil, "")
	got := fetch(t, combined, nil)
	if v := got["charlie"]; v.Lease != nil {
		t.Errorf("combined charlie: expected nil lease, got %+v", v.Lease)
	}
	if v := got["alpha"]; v.Lease == nil ||
		v.Lease.DeviceID != selfDev.ID || v.Lease.DeviceName != "Self-Mac" || !v.Lease.Mine {
		t.Errorf("combined alpha: expected dev=self/Self-Mac/mine=true, got %+v", v.Lease)
	}
	if v := got["bravo"]; v.Lease == nil ||
		v.Lease.DeviceID != otherDev.ID || v.Lease.DeviceName != "Other-Linux" || !v.Lease.Mine {
		t.Errorf("combined bravo: expected dev=other/Other-Linux/mine=true (combined treats local as owner), got %+v",
			v.Lease)
	}

	// --- (2) vault mode + Bearer auth as Self device ---
	vault := New(st, nil, nil, "")
	vault.Mode = "vault"
	vault.Middleware = append(vault.Middleware, httpserver.BearerAuth(st))

	got = fetch(t, vault, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+selfToken) })
	if v := got["alpha"]; v.Lease == nil || !v.Lease.Mine {
		t.Errorf("self bearer alpha: expected mine=true, got %+v", v.Lease)
	}
	if v := got["bravo"]; v.Lease == nil || v.Lease.Mine {
		t.Errorf("self bearer bravo: expected mine=false (held by other), got %+v", v.Lease)
	}
	if v := got["bravo"]; v.Lease.DeviceName != "Other-Linux" {
		t.Errorf("self bearer bravo: expected device_name=Other-Linux for badge, got %q", v.Lease.DeviceName)
	}

	// --- (3) vault mode + cookie session (web admin) ---
	sessID := vaultauth.NewToken()
	if err := st.CreateWebSession(ctx, sessID, time.Now().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatalf("CreateWebSession: %v", err)
	}
	got = fetch(t, vault, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: httpserver.SessionCookieName, Value: sessID})
	})
	for _, name := range []string{"alpha", "bravo"} {
		if v := got[name]; v.Lease == nil || v.Lease.Mine {
			t.Errorf("session cookie %s: expected mine=false (web admin is not a device), got %+v", name, v.Lease)
		}
	}
}
