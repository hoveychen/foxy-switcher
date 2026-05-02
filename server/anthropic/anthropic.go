// Package anthropic wraps the two OAuth-scoped endpoints we need to populate
// account cards: /api/oauth/profile (owner + plan) and /api/oauth/usage
// (per-window utilization). The client_id-style endpoints in package authz
// handle token exchange / refresh; this file only concerns itself with reading
// account metadata once we already hold a valid access_token.
//
// Both endpoints require the Bearer access_token plus the
// `anthropic-beta: oauth-2025-04-20` header — the same beta gate Claude Code
// itself uses. Endpoints and field names were verified against the
// claude-fleet implementation, which has been running against production for
// some time.
package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// BaseURL is exposed as a var so tests can swap in an httptest.Server.
var BaseURL = "https://api.anthropic.com"

const betaHeader = "oauth-2025-04-20"

// requestTimeout caps each individual call. Both endpoints are cheap; if they
// hang past this we'd rather show stale data than block the UI.
const requestTimeout = 5 * time.Second

// Profile is the subset of /api/oauth/profile we surface to the UI. The full
// response contains more (capabilities, feature flags, etc.) but those drift
// across Claude releases and aren't worth coupling to.
type Profile struct {
	// AccountUUID is the stable per-user identifier (`account.uuid` from the
	// OAuth profile response). Email and full_name can change over time
	// (alias swap, SSO rebind, primary-email migration), but uuid is
	// permanent — so this is what the store deduplicates on, not email.
	AccountUUID      string
	Email            string
	FullName         string
	OrganizationName string
	// Plan is a human-readable label. Personal plans come from
	// has_claude_max / has_claude_pro on `account`. Org-level plans come from
	// organization.organization_type ("claude_team"); within Team we split
	// Premium vs standard by rate_limit_tier (Premium reports max-parity
	// limits like "default_claude_max_5x"). Values: "Claude Max",
	// "Claude Pro", "Claude Team Premium", "Claude Team", "API / Free".
	Plan string
	// SubscriptionType is the lower-case raw token. Values: "max", "pro",
	// "team_premium", "team", "free". Useful for programmatic comparisons;
	// UI should prefer Plan.
	SubscriptionType string
}

// UsageWindow is one rate-limit window. Utilization is the percent the
// account has consumed within the window (0–100, NOT 0–1 — this was a
// previous bug in claude-fleet where the unit was guessed wrong).
// ResetsAt is an RFC3339 timestamp marking when the window rolls over.
type UsageWindow struct {
	Utilization float64
	ResetsAt    string
}

// Usage is the parsed response of /api/oauth/usage. Any window can be nil if
// the endpoint omits it (Sonnet-only accounts may not have seven_day_sonnet,
// for example).
type Usage struct {
	FiveHour       *UsageWindow
	SevenDay       *UsageWindow
	SevenDaySonnet *UsageWindow
}

// FetchProfile calls GET /api/oauth/profile with the given access token.
// Used at login time to populate owner / plan columns.
func FetchProfile(ctx context.Context, accessToken string) (*Profile, error) {
	body, err := getJSON(ctx, "/api/oauth/profile", accessToken)
	if err != nil {
		return nil, err
	}

	acct, _ := body["account"].(map[string]any)
	org, _ := body["organization"].(map[string]any)

	hasMax, _ := acct["has_claude_max"].(bool)
	hasPro, _ := acct["has_claude_pro"].(bool)
	orgType, _ := org["organization_type"].(string)
	rateTier, _ := org["rate_limit_tier"].(string)

	var plan, subType string
	switch {
	case hasMax:
		plan, subType = "Claude Max", "max"
	case hasPro:
		plan, subType = "Claude Pro", "pro"
	case orgType == "claude_team":
		// Premium gets Max-parity rate limits (e.g. "default_claude_max_5x");
		// the standard Team tier reports pro-level limits. Verified against
		// one Team Premium response; the standard-tier mapping is inferred.
		if strings.Contains(rateTier, "claude_max") {
			plan, subType = "Claude Team Premium", "team_premium"
		} else {
			plan, subType = "Claude Team", "team"
		}
	default:
		plan, subType = "API / Free", "free"
	}

	return &Profile{
		AccountUUID:      asString(acct["uuid"]),
		Email:            asString(acct["email"]),
		FullName:         asString(acct["full_name"]),
		OrganizationName: asString(org["name"]),
		Plan:             plan,
		SubscriptionType: subType,
	}, nil
}

// FetchUsage calls GET /api/oauth/usage. Called every 5 minutes by the usage
// scheduler and on the "Refresh now" button click.
func FetchUsage(ctx context.Context, accessToken string) (*Usage, error) {
	body, err := getJSON(ctx, "/api/oauth/usage", accessToken)
	if err != nil {
		return nil, err
	}
	return &Usage{
		FiveHour:       parseWindow(body["five_hour"]),
		SevenDay:       parseWindow(body["seven_day"]),
		SevenDaySonnet: parseWindow(body["seven_day_sonnet"]),
	}, nil
}

func parseWindow(v any) *UsageWindow {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	util, hasUtil := m["utilization"].(float64)
	if !hasUtil {
		return nil
	}
	return &UsageWindow{
		Utilization: util,
		ResetsAt:    asString(m["resets_at"]),
	}
}

func getJSON(ctx context.Context, path, accessToken string) (map[string]any, error) {
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("anthropic-beta", betaHeader)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s: read body: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %d %s", path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("%s: parse json: %w", path, err)
	}
	return body, nil
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
