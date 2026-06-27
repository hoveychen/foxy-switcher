package refresh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/anthropic"
	"github.com/hoveychen/foxy-switcher/server/store"

	_ "modernc.org/sqlite"
)

// usageServer serves /api/oauth/usage with the given status/body and counts hits.
func usageServer(status int, body string, hits *int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/usage" {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		if hits != nil {
			atomic.AddInt32(hits, 1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

const orgDisabledBody = `{"type":"error","error":{"type":"permission_error","message":"OAuth authentication is currently not allowed for this organization.","details":{"error_visibility":"user_facing"}}}`

func orgAccount(t *testing.T, st *store.Store, status string) *store.Account {
	t.Helper()
	a := &store.Account{
		Name:         "org",
		Email:        "org@example.com",
		AccessToken:  "live-access",
		RefreshToken: "live-refresh",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
		Status:       status,
		// Populated/consistent so the profile-backfill path is skipped.
		AccountUUID:   "u-org-1",
		Plan:          "Claude Team Premium",
		RateLimitTier: "default_claude_max_5x",
	}
	if err := st.Upsert(context.Background(), a); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	return a
}

// A 403 permission_error on the usage poll must flag the account org_disabled
// (so the selector excludes it) instead of leaving it active.
func TestUsagePollerMarksOrgDisabled(t *testing.T) {
	ctx := context.Background()
	srv := usageServer(http.StatusForbidden, orgDisabledBody, nil)
	defer srv.Close()
	prev := anthropic.BaseURL
	anthropic.BaseURL = srv.URL
	defer func() { anthropic.BaseURL = prev }()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	a := orgAccount(t, st, store.StatusActive)

	NewUsagePoller(st, nil).tick(ctx)

	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != store.StatusOrgDisabled {
		t.Fatalf("status = %q, want %q", got.Status, store.StatusOrgDisabled)
	}
}

// When a previously org_disabled account's poll succeeds (org re-enabled OAuth)
// the poller must auto-heal it back to active.
func TestUsagePollerAutoHealsOrgDisabled(t *testing.T) {
	ctx := context.Background()
	srv := usageServer(http.StatusOK, `{"five_hour":{"utilization":5.0,"resets_at":""}}`, nil)
	defer srv.Close()
	prev := anthropic.BaseURL
	anthropic.BaseURL = srv.URL
	defer func() { anthropic.BaseURL = prev }()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	a := orgAccount(t, st, store.StatusOrgDisabled)

	NewUsagePoller(st, nil).tick(ctx)

	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != store.StatusActive {
		t.Fatalf("status = %q, want %q (should auto-heal on successful poll)", got.Status, store.StatusActive)
	}
}
