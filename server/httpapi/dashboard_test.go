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

// TestDashboard_VaultMode_InUseListsAllLeases is the regression for the
// vault Web UI showing only one in-use account when several agents are
// driving different accounts (multi-device-lease-visibility plan). In
// vault mode the Server has no credinject Coordinator (s.Cred is nil),
// so the only authoritative source for in-use accounts is the leases
// table. The handler must surface every active lease as a separate
// kpis.in_use[] entry, joined with the holding device's display name.
func TestDashboard_VaultMode_InUseListsAllLeases(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	a := &store.Account{Name: "alice", AccessToken: "at-a", RefreshToken: "rt-a",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active"}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert a: %v", err)
	}
	b := &store.Account{Name: "bob", AccessToken: "at-b", RefreshToken: "rt-b",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active"}
	if err := st.Upsert(ctx, b); err != nil {
		t.Fatalf("upsert b: %v", err)
	}

	if err := st.InsertDevice(ctx, store.Device{ID: "dev-A", Name: "MacA", TokenHash: "ha"}); err != nil {
		t.Fatalf("ins A: %v", err)
	}
	if err := st.InsertDevice(ctx, store.Device{ID: "dev-B", Name: "MacB", TokenHash: "hb"}); err != nil {
		t.Fatalf("ins B: %v", err)
	}
	if _, err := st.AcquireLease(ctx, "lease-1", a.ID, "dev-A", time.Minute); err != nil {
		t.Fatalf("acquire a: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := st.AcquireLease(ctx, "lease-2", b.ID, "dev-B", time.Minute); err != nil {
		t.Fatalf("acquire b: %v", err)
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
	if len(got.KPIs.InUse) != 2 {
		t.Fatalf("kpis.in_use: got %d entries want 2 (one per active lease); got=%+v",
			len(got.KPIs.InUse), got.KPIs.InUse)
	}
	byAccount := map[int64]InUseEntry{}
	for _, e := range got.KPIs.InUse {
		byAccount[e.AccountID] = e
	}
	if e, ok := byAccount[a.ID]; !ok || e.DeviceID != "dev-A" || e.DeviceName != "MacA" {
		t.Errorf("alice entry: %+v want device dev-A/MacA", e)
	}
	if e, ok := byAccount[b.ID]; !ok || e.DeviceID != "dev-B" || e.DeviceName != "MacB" {
		t.Errorf("bob entry: %+v want device dev-B/MacB", e)
	}
}

// TestDashboard_NoLeases_EmptyInUse confirms the handler doesn't fabricate
// any in-use entries when nothing is leased — vault mode with an empty
// leases table should report kpis.in_use as empty (length 0), matching
// the "nobody is using a foxy account right now" UI state.
func TestDashboard_NoLeases_EmptyInUse(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	a := &store.Account{Name: "alice", AccessToken: "at-a", RefreshToken: "rt-a",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active"}
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
	if len(got.KPIs.InUse) != 0 {
		t.Errorf("kpis.in_use: got %d want 0 (no leases); got=%+v", len(got.KPIs.InUse), got.KPIs.InUse)
	}
}

// TestDashboard_BearerCallerSeesMineFlag verifies the Mine flag on each
// in-use entry: when an agent calls /api/dashboard with its own Bearer
// token, the entry holding the lease for that device is marked Mine=true
// so the frontend can highlight "your" account distinctly from other
// devices'.
func TestDashboard_BearerCallerSeesMineFlag(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	selfTok := vaultauth.NewToken()
	selfDev := store.Device{ID: vaultauth.NewID(), Name: "Self-MBP", TokenHash: vaultauth.HashToken(selfTok)}
	if err := st.InsertDevice(ctx, selfDev); err != nil {
		t.Fatalf("ins self: %v", err)
	}
	otherDev := store.Device{ID: vaultauth.NewID(), Name: "Other-Box", TokenHash: "hother"}
	if err := st.InsertDevice(ctx, otherDev); err != nil {
		t.Fatalf("ins other: %v", err)
	}

	a := &store.Account{Name: "alpha", AccessToken: "at-a", RefreshToken: "rt-a",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active"}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatal(err)
	}
	b := &store.Account{Name: "bravo", AccessToken: "at-b", RefreshToken: "rt-b",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active"}
	if err := st.Upsert(ctx, b); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AcquireLease(ctx, "lease-a", a.ID, selfDev.ID, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AcquireLease(ctx, "lease-b", b.ID, otherDev.ID, time.Minute); err != nil {
		t.Fatal(err)
	}

	srv := New(st, nil, nil, "")
	srv.Mode = "vault"
	srv.Middleware = append(srv.Middleware, httpserver.BearerAuth(st))

	req := httptest.NewRequest(http.MethodGet, "/api/dashboard", nil)
	req.Header.Set("Authorization", "Bearer "+selfTok)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%q", w.Code, w.Body.String())
	}
	var got DashboardResponse
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.KPIs.InUse) != 2 {
		t.Fatalf("expected 2 entries, got %d (%+v)", len(got.KPIs.InUse), got.KPIs.InUse)
	}
	byDev := map[string]InUseEntry{}
	for _, e := range got.KPIs.InUse {
		byDev[e.DeviceID] = e
	}
	if e := byDev[selfDev.ID]; !e.Mine {
		t.Errorf("self entry: expected mine=true, got %+v", e)
	}
	if e := byDev[otherDev.ID]; e.Mine {
		t.Errorf("other entry: expected mine=false, got %+v", e)
	}
}

// TestDashboard_TrendCoolingCountsFull: in the pool trend, a 7d-frozen account
// fills both windows and a 5h-frozen one fills only 5h, matching the KPI cards.
func TestDashboard_TrendCoolingCountsFull(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	mk := func(name string) *store.Account {
		a := &store.Account{Name: name, AccessToken: "at-" + name, RefreshToken: "rt-" + name,
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active",
			RateLimitTier: "default_claude_max_5x"}
		if err := st.Upsert(ctx, a); err != nil {
			t.Fatalf("upsert %s: %v", name, err)
		}
		if err := st.SetThresholds(ctx, a.ID, 90, 90, 90); err != nil {
			t.Fatalf("thresholds %s: %v", name, err)
		}
		return a
	}
	seven := mk("seven")
	five := mk("five")
	ts := time.Now().UnixMilli()
	if err := st.AppendUsageHistory(ctx, seven.ID, ts, 10, 92, 0); err != nil {
		t.Fatalf("history seven: %v", err)
	}
	if err := st.AppendUsageHistory(ctx, five.ID, ts, 91, 40, 0); err != nil {
		t.Fatalf("history five: %v", err)
	}

	srv := New(st, nil, nil, "")
	req := httptest.NewRequest(http.MethodGet, "/api/dashboard", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	var got DashboardResponse
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	last := got.Trend[len(got.Trend)-1]
	// seven: 5h 5 + 7d 5; five: 5h 5 + 7d 0.4*5=2.
	if last.FiveHourUsed != 10 || last.SevenDayUsed != 7 {
		t.Errorf("trend used: 5h=%v 7d=%v want 10/7", last.FiveHourUsed, last.SevenDayUsed)
	}
}

// TestDashboard_TrendAccountCounts: the current bucket counts active accounts
// that aren't cooling (a paused account and a frozen one drop out) and the
// accounts held by a live lease, split into Claude and Codex. OpenRouter and
// DeepSeek accounts are shared, not leased, so they count toward neither.
func TestDashboard_TrendAccountCounts(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	mk := func(name, provider, status string) *store.Account {
		a := &store.Account{Name: name, Provider: provider, AccessToken: "at-" + name, RefreshToken: "rt-" + name,
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: status}
		if err := st.Upsert(ctx, a); err != nil {
			t.Fatalf("upsert %s: %v", name, err)
		}
		if err := st.SetThresholds(ctx, a.ID, 90, 90, 90); err != nil {
			t.Fatalf("thresholds %s: %v", name, err)
		}
		return a
	}
	ok1 := mk("ok1", store.ProviderClaude, "active")
	mk("ok2", store.ProviderClaude, "active")
	frozen := mk("frozen", store.ProviderClaude, "active")
	mk("paused", store.ProviderClaude, "paused")
	cx1 := mk("cx1", store.ProviderCodex, "active")
	mk("cx2", store.ProviderCodex, "active")
	mk("cx3", store.ProviderCodex, "active")
	mk("or", store.ProviderOpenRouter, "active")
	mk("ds", store.ProviderDeepSeek, "active")
	ts := time.Now().UnixMilli()
	if err := st.AppendUsageHistory(ctx, ok1.ID, ts, 20, 30, 0); err != nil {
		t.Fatalf("history ok1: %v", err)
	}
	if err := st.AppendUsageHistory(ctx, frozen.ID, ts, 10, 95, 0); err != nil {
		t.Fatalf("history frozen: %v", err)
	}
	if err := st.InsertDevice(ctx, store.Device{ID: "dev-A", Name: "MacA", TokenHash: "ha"}); err != nil {
		t.Fatalf("ins A: %v", err)
	}
	if _, err := st.AcquireLease(ctx, "lease-1", ok1.ID, "dev-A", time.Minute); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := st.AcquireLease(ctx, "lease-2", cx1.ID, "dev-A", time.Minute); err != nil {
		t.Fatalf("acquire codex: %v", err)
	}

	srv := New(st, nil, nil, "")
	req := httptest.NewRequest(http.MethodGet, "/api/dashboard", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	var got DashboardResponse
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	last := got.Trend[len(got.Trend)-1]
	if last.ClaudeAvailable != 2 || last.ClaudeLeased != 1 {
		t.Errorf("claude counts: available=%d leased=%d want 2/1", last.ClaudeAvailable, last.ClaudeLeased)
	}
	if last.CodexAvailable != 3 || last.CodexLeased != 1 {
		t.Errorf("codex counts: available=%d leased=%d want 3/1", last.CodexAvailable, last.CodexLeased)
	}
}
