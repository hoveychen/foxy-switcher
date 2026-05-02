package refresh

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/anthropic"
	"github.com/hoveychen/foxy-switcher/server/authz"
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
	s.SkipAccountID = func() int64 { return a.ID } // pretend it's currently injected
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
	s.SkipAccountID = func() int64 { return a.ID }
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
