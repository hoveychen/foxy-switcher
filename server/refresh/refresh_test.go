package refresh

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/activity"
	"github.com/hoveychen/foxy-switcher/server/anthropic"
	"github.com/hoveychen/foxy-switcher/server/authz"
	"github.com/hoveychen/foxy-switcher/server/openai"
	"github.com/hoveychen/foxy-switcher/server/store"

	_ "modernc.org/sqlite"
)

// "Paused" (Status="paused") is purely a "do not select for routing" signal
// — the account must still get its access_token rotated and its usage polled,
// otherwise the next resume lands on a long-expired token and a stale usage
// card.

func TestSchedulerRotatesPausedAccount(t *testing.T) {
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "rotated-access",
			"refresh_token": "rotated-refresh",
			"expires_in":    8 * 3600,
			"token_type":    "Bearer",
		})
	}))
	defer srv.Close()
	prevURL := authz.ClaudeTokenURL
	authz.ClaudeTokenURL = srv.URL
	defer func() { authz.ClaudeTokenURL = prevURL }()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	a := &store.Account{
		Name:         "paused",
		Email:        "paused@example.com",
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		// Already expired so the "needs refresh within Threshold" gate fires.
		ExpiresAt: time.Now().Add(-time.Minute).UnixMilli(),
		Status:    "paused",
	}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	s := New(st, nil)
	s.tick(ctx)

	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AccessToken != "rotated-access" {
		t.Fatalf("paused account was not rotated; access_token = %q (want %q)",
			got.AccessToken, "rotated-access")
	}
}

// TestSchedulerRotatesInjectedAccountWhenTokenExpired guards the deadlock
// fix: while the injected account's token is still alive, the scheduler
// defers to Claude Code (which owns the refresh path). Once the token has
// actually expired and Claude Code hasn't rotated it (idle user, no API
// traffic), the daemon must take over — otherwise the account stays
// permanently "in use" with a dead token.
func TestSchedulerRotatesInjectedAccountWhenTokenExpired(t *testing.T) {
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "rescued-access",
			"refresh_token": "rescued-refresh",
			"expires_in":    8 * 3600,
			"token_type":    "Bearer",
		})
	}))
	defer srv.Close()
	prevURL := authz.ClaudeTokenURL
	authz.ClaudeTokenURL = srv.URL
	defer func() { authz.ClaudeTokenURL = prevURL }()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	a := &store.Account{
		Name:         "in-use",
		Email:        "inuse@example.com",
		AccessToken:  "stale-access",
		RefreshToken: "stale-refresh",
		ExpiresAt:    time.Now().Add(-5 * time.Minute).UnixMilli(),
		Status:       "active",
	}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	s := New(st, nil)
	s.IsAccountInUse = func(id int64) bool { return id == a.ID } // pretend it's currently injected
	s.tick(ctx)

	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AccessToken != "rescued-access" {
		t.Fatalf("expired injected account was not rescued; access_token = %q (want %q)",
			got.AccessToken, "rescued-access")
	}
}

// TestSchedulerRotatesInjectedAccountInsideFallbackWindow exercises the
// "Claude Code didn't show up in time" path. While CC is actively running it
// rotates the injected account's token before it gets close to expiry — and
// reverseSync would have copied the rotation back into the store, so a small
// `remaining` while still injected effectively means CC is *not* doing its
// job (idle user, machine was asleep, etc.). The scheduler must take over
// before the token actually dies, otherwise the user opens CC and finds a
// dead session.
func TestSchedulerRotatesInjectedAccountInsideFallbackWindow(t *testing.T) {
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fallback-access",
			"refresh_token": "fallback-refresh",
			"expires_in":    8 * 3600,
			"token_type":    "Bearer",
		})
	}))
	defer srv.Close()
	prevURL := authz.ClaudeTokenURL
	authz.ClaudeTokenURL = srv.URL
	defer func() { authz.ClaudeTokenURL = prevURL }()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	a := &store.Account{
		Name:         "in-use",
		Email:        "inuse-fallback@example.com",
		AccessToken:  "stale-access",
		RefreshToken: "stale-refresh",
		// 10 minutes left — still positive, but inside the in-use fallback
		// window. CC clearly hasn't refreshed (otherwise reverseSync would
		// have bumped this), so the scheduler must.
		ExpiresAt: time.Now().Add(10 * time.Minute).UnixMilli(),
		Status:    "active",
	}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	s := New(st, nil)
	s.IsAccountInUse = func(id int64) bool { return id == a.ID }
	s.tick(ctx)

	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AccessToken != "fallback-access" {
		t.Fatalf("near-expiry injected account was not rotated; access_token = %q (want %q)",
			got.AccessToken, "fallback-access")
	}
}

// TestUsagePollerRefreshesStalePlanLabel guards a one-shot migration: an
// account that was profile-fetched before the Plan label distinguished
// "Claude Max 5x" / "Claude Max 20x" has Plan="Claude Max" and
// RateLimitTier="default_claude_max_20x" stored. The original backfill
// condition (Plan=="" || AccountUUID=="" || RateLimitTier=="") never re-fires
// because all three fields are populated, so the stale label is frozen.
// UsagePoller must detect the mismatch (Plan doesn't match what FetchProfile's
// current algorithm would derive from the stored SubscriptionType +
// RateLimitTier) and re-fetch the profile.
func TestUsagePollerRefreshesStalePlanLabel(t *testing.T) {
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/oauth/profile":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"account":      map[string]any{"uuid": "u-1", "has_claude_max": true},
				"organization": map[string]any{"rate_limit_tier": "default_claude_max_20x"},
			})
		case "/api/oauth/usage":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"five_hour": map[string]any{"utilization": 0.0},
			})
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	prevURL := anthropic.BaseURL
	anthropic.BaseURL = srv.URL
	defer func() { anthropic.BaseURL = prevURL }()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	a := &store.Account{
		Name:             "stale",
		Email:            "stale@example.com",
		AccessToken:      "live-access",
		RefreshToken:     "live-refresh",
		ExpiresAt:        time.Now().Add(time.Hour).UnixMilli(),
		Status:           "active",
		AccountUUID:      "u-1",
		Plan:             "Claude Max",
		SubscriptionType: "max",
		RateLimitTier:    "default_claude_max_20x",
	}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	p := NewUsagePoller(st, nil)
	p.tick(ctx)

	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Plan != "Claude Max 20x" {
		t.Fatalf("stale plan label not refreshed; Plan = %q (want %q)", got.Plan, "Claude Max 20x")
	}
}

func TestUsagePollerLabelsBusinessProLite(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan_type": "self_serve_business_prolite",
			"rate_limit": map[string]any{
				"primary_window": map[string]any{"used_percent": 11.0, "reset_at": time.Now().Add(6 * 24 * time.Hour).Unix(), "limit_window_seconds": int64(604800)},
			},
		})
	}))
	defer srv.Close()
	prevURL := openai.UsageURL
	openai.UsageURL = srv.URL
	defer func() { openai.UsageURL = prevURL }()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	a := &store.Account{
		Provider: store.ProviderCodex, Name: "business", AccessToken: "live-access",
		RefreshToken: "live-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		Status: "active", AccountUUID: "business-acct", Plan: "Codex Plus", SubscriptionType: "plus",
	}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	p := NewUsagePoller(st, nil)
	if !p.pollCodex(ctx, *a) {
		t.Fatal("pollCodex returned false")
	}
	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Plan != "Codex Business Premium" || got.SubscriptionType != "self_serve_business_prolite" {
		t.Fatalf("business plan mismatch after refresh: %+v", got)
	}
	if got.FiveHourUtil != 11 {
		t.Fatalf("usage not stored; FiveHourUtil = %v (want 11)", got.FiveHourUtil)
	}
	if got.FiveHourWindowSeconds != 604800 {
		t.Fatalf("window duration not stored; FiveHourWindowSeconds = %d (want 604800)", got.FiveHourWindowSeconds)
	}
	var buckets []openai.UsageBucket
	if err := json.Unmarshal([]byte(got.CodexUsageJSON), &buckets); err != nil || len(buckets) != 1 || buckets[0].LimitID != "codex" {
		t.Fatalf("dynamic Codex usage not stored: buckets=%+v err=%v", buckets, err)
	}
}

func TestUsagePollerRepairsStaleCodexPlanLabel(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan_type": "self_serve_business_prolite",
			"rate_limit": map[string]any{
				"primary_window": map[string]any{"used_percent": 11.0, "reset_at": time.Now().Add(6 * 24 * time.Hour).Unix()},
			},
		})
	}))
	defer srv.Close()
	prevURL := openai.UsageURL
	openai.UsageURL = srv.URL
	defer func() { openai.UsageURL = prevURL }()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	a := &store.Account{
		Provider: store.ProviderCodex, Name: "business", AccessToken: "live-access",
		RefreshToken: "live-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		Status: "active", AccountUUID: "business-acct",
		Plan:             "Codex Self_serve_business_prolite",
		SubscriptionType: "self_serve_business_prolite",
	}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	p := NewUsagePoller(st, nil)
	if !p.pollCodex(ctx, *a) {
		t.Fatal("pollCodex returned false")
	}
	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Plan != "Codex Business Premium" {
		t.Fatalf("stale Codex plan label not repaired; Plan = %q (want %q)", got.Plan, "Codex Business Premium")
	}
}

func TestUsagePollerPollsPausedAccount(t *testing.T) {
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/usage" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"five_hour": map[string]any{"utilization": 42.0, "resets_at": ""},
		})
	}))
	defer srv.Close()
	prevURL := anthropic.BaseURL
	anthropic.BaseURL = srv.URL
	defer func() { anthropic.BaseURL = prevURL }()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	a := &store.Account{
		Name:         "paused",
		Email:        "paused2@example.com",
		AccessToken:  "live-access",
		RefreshToken: "live-refresh",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
		Status:       "paused",
		Plan:         "Claude Max", // skip profile backfill path
	}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	p := NewUsagePoller(st, nil)
	p.tick(ctx)

	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.UsageFetchedAt == 0 {
		t.Fatalf("paused account usage was not polled; UsageFetchedAt still 0")
	}
	if got.FiveHourUtil != 42 {
		t.Fatalf("paused account usage not written; FiveHourUtil = %v (want 42)", got.FiveHourUtil)
	}
}

// TestUsagePollerHonorsRetryAfter pins the 429-backoff contract: when
// /api/oauth/usage returns 429 with a Retry-After window, the poller must skip
// that account on subsequent ticks until the window passes. Without this,
// every tick (default 60s) re-hits the same 429 and produces the user-facing
// "Rate limited" log spam that motivated the change.
func TestUsagePollerHonorsRetryAfter(t *testing.T) {
	ctx := context.Background()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/usage" {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Retry-After", "60")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"Rate limited"}}`))
	}))
	defer srv.Close()
	prevURL := anthropic.BaseURL
	anthropic.BaseURL = srv.URL
	defer func() { anthropic.BaseURL = prevURL }()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// Plan / AccountUUID / RateLimitTier all populated and consistent so the
	// profile-backfill path is skipped — we only want to exercise the usage
	// fetch and its 429 handling.
	a := &store.Account{
		Name:             "ratelimited",
		Email:            "rl@example.com",
		AccessToken:      "live-access",
		RefreshToken:     "live-refresh",
		ExpiresAt:        time.Now().Add(time.Hour).UnixMilli(),
		Status:           "active",
		AccountUUID:      "u-1",
		Plan:             "Claude Max 5x",
		SubscriptionType: "max",
		RateLimitTier:    "default_claude_max_5x",
	}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	p := NewUsagePoller(st, nil)

	// Tick 1: 429 — poller records the backoff window.
	p.tick(ctx)
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("after tick1, hits = %d (want 1)", got)
	}

	// Tick 2: still inside the Retry-After window, account must be skipped
	// (no extra HTTP call to Anthropic).
	p.tick(ctx)
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("after tick2 (within backoff), hits = %d (want still 1 — poller did not honor Retry-After)", got)
	}

	// Roll the clock forward by clearing the backoff entry, then tick again
	// — the account must be polled.
	p.clearBackoff(a.ID)
	p.tick(ctx)
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("after tick3 (window expired), hits = %d (want 2)", got)
	}
}

// TestUsagePollerAnthropicFloor pins the per-account floor on
// /api/oauth/usage: after a successful call the same Anthropic account is not
// asked again on the next ticks, however short the configured interval, while
// a Codex account on the same poller is still read every tick.
func TestUsagePollerAnthropicFloor(t *testing.T) {
	ctx := context.Background()

	var claudeHits, codexHits int32
	claudeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/usage" {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		atomic.AddInt32(&claudeHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"five_hour": map[string]any{"utilization": 10.0},
		})
	}))
	defer claudeSrv.Close()
	prevURL := anthropic.BaseURL
	anthropic.BaseURL = claudeSrv.URL
	defer func() { anthropic.BaseURL = prevURL }()

	codexSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&codexHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan_type": "plus",
			"rate_limit": map[string]any{
				"primary_window": map[string]any{"used_percent": 5.0, "reset_at": time.Now().Add(time.Hour).Unix()},
			},
		})
	}))
	defer codexSrv.Close()
	prevCodexURL := openai.UsageURL
	openai.UsageURL = codexSrv.URL
	defer func() { openai.UsageURL = prevCodexURL }()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	claude := &store.Account{
		Name: "claude", Email: "floor@example.com", AccessToken: "live-access",
		RefreshToken: "live-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		Status: "active", AccountUUID: "u-floor", Plan: "Claude Max 5x",
		SubscriptionType: "max", RateLimitTier: "default_claude_max_5x",
	}
	codex := &store.Account{
		Provider: store.ProviderCodex, Name: "codex", AccessToken: "codex-access",
		RefreshToken: "codex-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		Status: "active", AccountUUID: "codex-floor", Plan: "Codex Plus", SubscriptionType: "plus",
	}
	for _, a := range []*store.Account{claude, codex} {
		if err := st.Upsert(ctx, a); err != nil {
			t.Fatalf("upsert %s: %v", a.Name, err)
		}
	}

	p := NewUsagePoller(st, nil)
	for range 3 {
		p.tick(ctx)
	}
	if got := atomic.LoadInt32(&claudeHits); got != 1 {
		t.Fatalf("Anthropic usage hits over 3 back-to-back ticks = %d (want 1 — the floor must hold the account)", got)
	}
	if got := atomic.LoadInt32(&codexHits); got != 3 {
		t.Fatalf("Codex usage hits over 3 ticks = %d (want 3 — the floor is Anthropic-only)", got)
	}

	// Once the floor has passed the account is polled again.
	p.clearBackoff(claude.ID)
	p.tick(ctx)
	if got := atomic.LoadInt32(&claudeHits); got != 2 {
		t.Fatalf("after the floor passed, Anthropic usage hits = %d (want 2)", got)
	}
}

// TestUsagePollerEmitsBackoffEvent pins the user-facing surface of the 429
// path: the Activity page must see one info-level "usage.backoff" event per
// backoff window (not the alarming "error.usage" event, since 429 is a known
// degraded state, not a failure). The skipped second tick must produce
// nothing — that's the whole point of the backoff.
func TestUsagePollerEmitsBackoffEvent(t *testing.T) {
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/usage" {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"Rate limited"}}`))
	}))
	defer srv.Close()
	prevURL := anthropic.BaseURL
	anthropic.BaseURL = srv.URL
	defer func() { anthropic.BaseURL = prevURL }()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	busDB, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "act.db"))
	if err != nil {
		t.Fatalf("open bus db: %v", err)
	}
	defer busDB.Close()
	bus, err := activity.NewBus(busDB, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("new bus: %v", err)
	}

	a := &store.Account{
		Name:             "ratelimited",
		Email:            "rl-evt@example.com",
		AccessToken:      "live-access",
		RefreshToken:     "live-refresh",
		ExpiresAt:        time.Now().Add(time.Hour).UnixMilli(),
		Status:           "active",
		AccountUUID:      "u-evt-1",
		Plan:             "Claude Max 5x",
		SubscriptionType: "max",
		RateLimitTier:    "default_claude_max_5x",
	}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	p := NewUsagePoller(st, nil)
	p.Bus = bus

	// Tick 1: 429 → expect exactly one usage.backoff info event, and zero
	// error.usage events (since 429 is a handled degraded state).
	p.tick(ctx)
	events := bus.List(activity.Filter{})
	backoffCount := 0
	errorCount := 0
	for _, ev := range events {
		switch ev.Type {
		case activity.TypeUsageBackoff:
			backoffCount++
			if ev.Severity != activity.SeverityInfo {
				t.Errorf("usage.backoff severity = %q, want info", ev.Severity)
			}
			if ev.AccountID != a.ID {
				t.Errorf("usage.backoff account_id = %d, want %d", ev.AccountID, a.ID)
			}
		case activity.TypeErrorUsage:
			errorCount++
		}
	}
	if backoffCount != 1 {
		t.Errorf("after tick1, usage.backoff events = %d, want 1", backoffCount)
	}
	if errorCount != 0 {
		t.Errorf("after tick1, error.usage events = %d, want 0 (429 should not be an error event)", errorCount)
	}

	// Tick 2: still in backoff window → poller skips, no new events.
	p.tick(ctx)
	events2 := bus.List(activity.Filter{})
	if len(events2) != len(events) {
		t.Errorf("tick2 emitted %d new events; want 0 (account should be silently skipped)", len(events2)-len(events))
	}
}
