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

// credStatusBody mirrors credinject.Status's JSON shape — duplicated here to
// keep the test independent of the credinject package's struct.
type credStatusBody struct {
	ManagedAccountID    int64 `json:"managed_account_id"`
	NativeBackupPresent bool  `json:"native_backup_present"`
	InjectedAt          int64 `json:"injected_at"`
}

// TestCredStatus_VaultMode_FallsBackToLeases is the regression for the
// vault-mode "未注入账号" UI. App.tsx drives managedAccountId off
// /api/cred/status, not /api/dashboard, so f7c4a4e's dashboard fallback
// alone wasn't enough — handleCredStatus needs the same FirstActiveLease
// fallback when s.Cred is nil.
func TestCredStatus_VaultMode_FallsBackToLeases(t *testing.T) {
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

	req := httptest.NewRequest(http.MethodGet, "/api/cred/status", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", w.Code, w.Body.String())
	}
	var got credStatusBody
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ManagedAccountID != a.ID {
		t.Fatalf("managed_account_id: got %d want %d (agent's lease should surface here in vault mode)",
			got.ManagedAccountID, a.ID)
	}
}

// TestCredStatus_NoLeases_ReturnsZero confirms the fallback doesn't fabricate
// an account when nothing is leased — vault mode with an empty leases table
// must keep managed_account_id=0 so the UI's "未注入账号" state is honest.
func TestCredStatus_NoLeases_ReturnsZero(t *testing.T) {
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

	req := httptest.NewRequest(http.MethodGet, "/api/cred/status", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	var got credStatusBody
	_ = json.NewDecoder(w.Body).Decode(&got)
	if got.ManagedAccountID != 0 {
		t.Errorf("managed_account_id: got %d want 0 (no leases)", got.ManagedAccountID)
	}
}
