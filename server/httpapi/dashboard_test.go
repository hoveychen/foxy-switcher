package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
)

// TestDashboard_VaultMode_FallsBackToLeases is the regression for the vault
// Web UI showing "未注入账号" forever. In vault mode the Server has no
// credinject Coordinator (s.Cred is nil), so the only authoritative source
// for "which account is currently being used" is the leases table that
// remote agents renew every few seconds. The handler must surface that
// account_id as kpis.in_use_account_id.
func TestDashboard_VaultMode_FallsBackToLeases(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	a := &store.Account{
		Name:         "alice",
		AccessToken:  "at-a",
		RefreshToken: "rt-a",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
		Status:       "active",
	}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := st.AcquireLease(ctx, "lease-1", a.ID, "agent-device", time.Minute); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	srv := New(st, nil, nil, "")
	srv.Mode = "vault"

	req := httptest.NewRequest(http.MethodGet, "/api/dashboard", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", w.Code, w.Body.String())
	}
	var got DashboardResponse
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.KPIs.InUseAccountID != a.ID {
		t.Fatalf("in_use_account_id: got %d want %d (agent's lease should surface here in vault mode)",
			got.KPIs.InUseAccountID, a.ID)
	}
}

// TestDashboard_NoLeases_ReturnsZero confirms the fallback doesn't fabricate
// an account when nothing is leased — vault mode with an empty leases table
// should report in_use_account_id=0, matching the "nobody is using a foxy
// account right now" UI state.
func TestDashboard_NoLeases_ReturnsZero(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	a := &store.Account{
		Name:         "alice",
		AccessToken:  "at-a",
		RefreshToken: "rt-a",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
		Status:       "active",
	}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	srv := New(st, nil, nil, "")
	srv.Mode = "vault"

	req := httptest.NewRequest(http.MethodGet, "/api/dashboard", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	var got DashboardResponse
	_ = json.NewDecoder(w.Body).Decode(&got)
	if got.KPIs.InUseAccountID != 0 {
		t.Errorf("in_use_account_id: got %d want 0 (no leases)", got.KPIs.InUseAccountID)
	}
}
