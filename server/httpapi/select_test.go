package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
)

// handleSelect should make the targeted account the LRU minimum so the next
// reconcile picks it. We assert the side effect on store directly: the route
// is the only public API that's supposed to write last_used_at = 0, and the
// behaviour is what the UI's "Use now" button relies on.
func TestSelect_ZeroesLastUsedAt(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	a := &store.Account{Name: "a", Email: "a@x", AccessToken: "at", RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.MarkUsed(ctx, a.ID); err != nil {
		t.Fatalf("markused: %v", err)
	}
	srv := New(st, nil, nil, "")

	req := httptest.NewRequest(http.MethodPost, "/api/accounts/"+itoa(a.ID)+"/select", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d body=%q", w.Code, w.Body.String())
	}

	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LastUsedAt != 0 {
		t.Fatalf("expected last_used_at zeroed, got %d", got.LastUsedAt)
	}
}

// Threshold-throttled accounts shouldn't be selectable — the reconcile would
// skip them, so the UI needs to know up-front rather than silently no-op.
func TestSelect_RejectsThrottled(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	a := &store.Account{
		Name:              "a",
		Email:             "a@x",
		AccessToken:       "at",
		RefreshToken:      "rt",
		ExpiresAt:         time.Now().Add(time.Hour).UnixMilli(),
		FiveHourThreshold: 80,
	}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	resets := time.Now().Add(time.Hour).Format(time.RFC3339)
	if err := st.SetUsage(ctx, a.ID, 95, resets, 0, "", 0, "", ""); err != nil {
		t.Fatalf("setusage: %v", err)
	}
	srv := New(st, nil, nil, "")

	req := httptest.NewRequest(http.MethodPost, "/api/accounts/"+itoa(a.ID)+"/select", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d body=%q", w.Code, w.Body.String())
	}
}

// An OpenRouter account cannot be "selected". /select pins the account for the
// next pick and kicks the Claude credinject coordinator; neither reaches
// OpenRouter, whose picker (vault.OpenRouterKeys.pickAccount) deliberately
// ignores pins and orders by id so a device keeps the same account. Returning
// 204 therefore reported success for a no-op — the UI showed the switch as
// done and nothing had happened. Reject it instead.
func TestSelect_RejectsOpenRouter(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	a := &store.Account{
		Provider:    store.ProviderOpenRouter,
		Name:        "foxy",
		AccountUUID: "openrouter:foxy",
		Status:      store.StatusActive,
	}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	srv := New(st, nil, nil, "")

	req := httptest.NewRequest(http.MethodPost, "/api/accounts/"+itoa(a.ID)+"/select", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d body=%q", w.Code, w.Body.String())
	}

	// And it must not have written a pin on the way out.
	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.PinnedDeviceID != "" {
		t.Fatalf("expected no pin written, got %q", got.PinnedDeviceID)
	}
}

func newTestStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, dir
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
