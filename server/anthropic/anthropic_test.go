package anthropic

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchProfile_MaxAccount(t *testing.T) {
	// Plain Max profile — no rate_limit_tier surfaced. Falls back to the
	// generic "Claude Max" label so legacy responses without the field
	// keep their old presentation.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/profile" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Fatalf("auth header = %q", got)
		}
		if got := r.Header.Get("anthropic-beta"); got != "oauth-2025-04-20" {
			t.Fatalf("beta header = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
		  "account": {"email":"a@b.com","full_name":"Alice","has_claude_max":true,"has_claude_pro":false},
		  "organization": {"name":"Acme"}
		}`))
	}))
	defer srv.Close()

	old := BaseURL
	BaseURL = srv.URL
	defer func() { BaseURL = old }()

	p, err := FetchProfile(context.Background(), "tok")
	if err != nil {
		t.Fatalf("FetchProfile: %v", err)
	}
	if p.Email != "a@b.com" || p.FullName != "Alice" || p.OrganizationName != "Acme" {
		t.Errorf("profile fields wrong: %+v", p)
	}
	if p.Plan != "Claude Max" || p.SubscriptionType != "max" {
		t.Errorf("plan/subType wrong: %+v", p)
	}
}

func TestFetchProfile_MaxAccount_5x(t *testing.T) {
	// Personal Max with rate_limit_tier=default_claude_max_5x. The plan
	// label is split out so the UI can tell 5x apart from 20x without
	// reading rate_limit_tier directly.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
		  "account": {"has_claude_max":true,"has_claude_pro":false},
		  "organization": {"rate_limit_tier":"default_claude_max_5x"}
		}`))
	}))
	defer srv.Close()
	old := BaseURL
	BaseURL = srv.URL
	defer func() { BaseURL = old }()

	p, err := FetchProfile(context.Background(), "tok")
	if err != nil {
		t.Fatalf("FetchProfile: %v", err)
	}
	if p.Plan != "Claude Max 5x" || p.SubscriptionType != "max" {
		t.Errorf("expected Max 5x, got plan=%q subType=%q", p.Plan, p.SubscriptionType)
	}
	if p.RateLimitTier != "default_claude_max_5x" {
		t.Errorf("rate_limit_tier = %q", p.RateLimitTier)
	}
}

func TestFetchProfile_MaxAccount_20x(t *testing.T) {
	// Personal Max with rate_limit_tier=default_claude_max_20x — the
	// case that motivated adding rate_limit_tier in the first place.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
		  "account": {"has_claude_max":true,"has_claude_pro":false},
		  "organization": {"rate_limit_tier":"default_claude_max_20x"}
		}`))
	}))
	defer srv.Close()
	old := BaseURL
	BaseURL = srv.URL
	defer func() { BaseURL = old }()

	p, err := FetchProfile(context.Background(), "tok")
	if err != nil {
		t.Fatalf("FetchProfile: %v", err)
	}
	if p.Plan != "Claude Max 20x" || p.SubscriptionType != "max" {
		t.Errorf("expected Max 20x, got plan=%q subType=%q", p.Plan, p.SubscriptionType)
	}
	if p.RateLimitTier != "default_claude_max_20x" {
		t.Errorf("rate_limit_tier = %q", p.RateLimitTier)
	}
}

func TestFetchProfile_ProFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"account": {"has_claude_pro": true}, "organization": {}}`))
	}))
	defer srv.Close()
	old := BaseURL
	BaseURL = srv.URL
	defer func() { BaseURL = old }()

	p, err := FetchProfile(context.Background(), "tok")
	if err != nil {
		t.Fatalf("FetchProfile: %v", err)
	}
	if p.Plan != "Claude Pro" || p.SubscriptionType != "pro" {
		t.Errorf("expected Pro, got %+v", p)
	}
}

func TestFetchProfile_TeamPremium(t *testing.T) {
	// Real-shape response captured from a Claude Team Premium account: max/pro
	// flags are false on `account`, the team signal lives on `organization`,
	// and rate_limit_tier carries the "claude_max" parity that distinguishes
	// Premium from the standard Team tier.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
		  "account": {"email":"x@y.com","full_name":"X","has_claude_max":false,"has_claude_pro":false},
		  "organization": {"name":"Acme","organization_type":"claude_team","rate_limit_tier":"default_claude_max_5x","seat_tier":"team_tier_1"}
		}`))
	}))
	defer srv.Close()
	old := BaseURL
	BaseURL = srv.URL
	defer func() { BaseURL = old }()

	p, err := FetchProfile(context.Background(), "tok")
	if err != nil {
		t.Fatalf("FetchProfile: %v", err)
	}
	if p.Plan != "Claude Team Premium" || p.SubscriptionType != "team_premium" {
		t.Errorf("expected Team Premium, got plan=%q subType=%q", p.Plan, p.SubscriptionType)
	}
}

func TestFetchProfile_TeamStandard(t *testing.T) {
	// Standard Team tier: organization_type is still claude_team, but
	// rate_limit_tier reflects pro-level limits (not max). Field value is
	// inferred from user-supplied schema knowledge, not a captured response.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
		  "account": {"has_claude_max":false,"has_claude_pro":false},
		  "organization": {"organization_type":"claude_team","rate_limit_tier":"default_claude_pro"}
		}`))
	}))
	defer srv.Close()
	old := BaseURL
	BaseURL = srv.URL
	defer func() { BaseURL = old }()

	p, err := FetchProfile(context.Background(), "tok")
	if err != nil {
		t.Fatalf("FetchProfile: %v", err)
	}
	if p.Plan != "Claude Team" || p.SubscriptionType != "team" {
		t.Errorf("expected Team, got plan=%q subType=%q", p.Plan, p.SubscriptionType)
	}
}

func TestFetchProfile_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "expired token", http.StatusUnauthorized)
	}))
	defer srv.Close()
	old := BaseURL
	BaseURL = srv.URL
	defer func() { BaseURL = old }()

	if _, err := FetchProfile(context.Background(), "tok"); err == nil {
		t.Fatal("expected error on 401, got nil")
	}
}

func TestFetchUsage_AllWindows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/usage" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
		  "five_hour":         {"utilization": 12.5, "resets_at": "2026-04-30T05:00:00Z"},
		  "seven_day":         {"utilization": 78.0, "resets_at": "2026-05-07T00:00:00Z"},
		  "seven_day_sonnet":  {"utilization": 33.3, "resets_at": "2026-05-07T00:00:00Z"}
		}`))
	}))
	defer srv.Close()
	old := BaseURL
	BaseURL = srv.URL
	defer func() { BaseURL = old }()

	u, err := FetchUsage(context.Background(), "tok")
	if err != nil {
		t.Fatalf("FetchUsage: %v", err)
	}
	if u.FiveHour == nil || u.FiveHour.Utilization != 12.5 {
		t.Errorf("five_hour = %+v", u.FiveHour)
	}
	if u.FiveHour.ResetsAt != "2026-04-30T05:00:00Z" {
		t.Errorf("five_hour resets_at = %q", u.FiveHour.ResetsAt)
	}
	if u.SevenDay == nil || u.SevenDay.Utilization != 78.0 {
		t.Errorf("seven_day = %+v", u.SevenDay)
	}
	if u.SevenDaySonnet == nil || u.SevenDaySonnet.Utilization != 33.3 {
		t.Errorf("seven_day_sonnet = %+v", u.SevenDaySonnet)
	}
	// Legacy top-level seven_day_sonnet (no limits[]) is labelled "Sonnet".
	if u.ScopedLabel != "Sonnet" {
		t.Errorf("ScopedLabel = %q, want %q", u.ScopedLabel, "Sonnet")
	}
}

// TestFetchUsage_ScopedFromLimits covers the current Anthropic shape: the
// model-scoped weekly cap lives in a limits[] entry with kind "weekly_scoped"
// (e.g. Fable), and takes precedence over any legacy top-level
// seven_day_sonnet. percent is 0–100 and carried through unchanged.
func TestFetchUsage_ScopedFromLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
		  "five_hour":        {"utilization": 12.5, "resets_at": "2026-04-30T05:00:00Z"},
		  "seven_day":        {"utilization": 78.0, "resets_at": "2026-05-07T00:00:00Z"},
		  "seven_day_sonnet": {"utilization": 1.0,  "resets_at": "2026-05-07T00:00:00Z"},
		  "limits": [
		    {"kind": "weekly", "percent": 78.0, "resets_at": "2026-05-07T00:00:00Z"},
		    {"kind": "weekly_scoped", "percent": 42.0, "resets_at": "2026-05-07T12:00:00Z",
		     "scope": {"model": {"display_name": "Fable"}}}
		  ]
		}`))
	}))
	defer srv.Close()
	old := BaseURL
	BaseURL = srv.URL
	defer func() { BaseURL = old }()

	u, err := FetchUsage(context.Background(), "tok")
	if err != nil {
		t.Fatalf("FetchUsage: %v", err)
	}
	if u.SevenDaySonnet == nil || u.SevenDaySonnet.Utilization != 42.0 {
		t.Errorf("scoped window = %+v, want util 42.0 from limits[]", u.SevenDaySonnet)
	}
	if u.SevenDaySonnet.ResetsAt != "2026-05-07T12:00:00Z" {
		t.Errorf("scoped resets_at = %q", u.SevenDaySonnet.ResetsAt)
	}
	if u.ScopedLabel != "Fable" {
		t.Errorf("ScopedLabel = %q, want %q", u.ScopedLabel, "Fable")
	}
}

// TestFetchUsage_NoScopedWindow: no limits[] weekly_scoped and no legacy
// seven_day_sonnet → scoped window nil, label empty.
func TestFetchUsage_NoScopedWindow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
		  "five_hour": {"utilization": 5.0, "resets_at": "x"},
		  "limits": [{"kind": "weekly", "percent": 5.0, "resets_at": "x"}]
		}`))
	}))
	defer srv.Close()
	old := BaseURL
	BaseURL = srv.URL
	defer func() { BaseURL = old }()

	u, err := FetchUsage(context.Background(), "tok")
	if err != nil {
		t.Fatalf("FetchUsage: %v", err)
	}
	if u.SevenDaySonnet != nil {
		t.Errorf("scoped window should be nil, got %+v", u.SevenDaySonnet)
	}
	if u.ScopedLabel != "" {
		t.Errorf("ScopedLabel = %q, want empty", u.ScopedLabel)
	}
}

// TestFetchUsage_RateLimitWithRetryAfter guards the 429 contract: the caller
// must be able to errors.As the result into *RateLimitError and read the
// Retry-After header (in seconds) off it. The poller relies on this to
// schedule per-account backoff instead of hammering /api/oauth/usage on every
// tick.
func TestFetchUsage_RateLimitWithRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"Rate limited. Please try again later."}}`))
	}))
	defer srv.Close()
	old := BaseURL
	BaseURL = srv.URL
	defer func() { BaseURL = old }()

	_, err := FetchUsage(context.Background(), "tok")
	if err == nil {
		t.Fatal("expected error on 429, got nil")
	}
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("expected *RateLimitError, got %T: %v", err, err)
	}
	if rl.RetryAfter != 120*time.Second {
		t.Errorf("RetryAfter = %v, want 120s", rl.RetryAfter)
	}
	if rl.StatusCode != http.StatusTooManyRequests {
		t.Errorf("StatusCode = %d, want 429", rl.StatusCode)
	}
}

// TestFetchUsage_RateLimitFallback covers the case where Anthropic returns 429
// without a Retry-After header — the helper falls back to a sensible default
// so the poller still has a non-zero window to honor.
func TestFetchUsage_RateLimitFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	old := BaseURL
	BaseURL = srv.URL
	defer func() { BaseURL = old }()

	_, err := FetchUsage(context.Background(), "tok")
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("expected *RateLimitError, got %T: %v", err, err)
	}
	if rl.RetryAfter <= 0 {
		t.Errorf("fallback RetryAfter must be > 0, got %v", rl.RetryAfter)
	}
}

func TestFetchUsage_MissingWindowIsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"five_hour": {"utilization": 5.0, "resets_at": "x"}}`))
	}))
	defer srv.Close()
	old := BaseURL
	BaseURL = srv.URL
	defer func() { BaseURL = old }()

	u, err := FetchUsage(context.Background(), "tok")
	if err != nil {
		t.Fatalf("FetchUsage: %v", err)
	}
	if u.FiveHour == nil {
		t.Error("five_hour should be present")
	}
	if u.SevenDay != nil || u.SevenDaySonnet != nil {
		t.Errorf("missing windows should be nil; got seven_day=%v sonnet=%v", u.SevenDay, u.SevenDaySonnet)
	}
}
