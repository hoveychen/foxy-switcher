package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchProfile_MaxAccount(t *testing.T) {
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
