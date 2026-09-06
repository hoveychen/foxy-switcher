package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

var (
	TokenURL = "https://auth.openai.com/oauth/token"
	UsageURL = "https://chatgpt.com/backend-api/wham/usage"
)

const codexOAuthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

var httpClient = &http.Client{Timeout: 10 * time.Second}

type permanentRefreshError struct{ message string }

func (e *permanentRefreshError) Error() string { return e.message }

func IsPermanentRefreshError(err error) bool {
	var target *permanentRefreshError
	return errors.As(err, &target)
}

func Refresh(ctx context.Context, auth *AuthFile) error {
	body, err := json.Marshal(map[string]string{
		"client_id":     codexOAuthClientID,
		"grant_type":    "refresh_token",
		"refresh_token": auth.Tokens.RefreshToken,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := fmt.Sprintf("Codex token refresh failed: HTTP %d", resp.StatusCode)
		if resp.StatusCode == http.StatusUnauthorized || bytes.Contains(raw, []byte("refresh_token_")) {
			return &permanentRefreshError{message: msg}
		}
		return errors.New(msg)
	}
	var out struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	if out.IDToken != "" {
		auth.Tokens.IDToken = out.IDToken
	}
	if out.AccessToken != "" {
		auth.Tokens.AccessToken = out.AccessToken
	}
	if out.RefreshToken != "" {
		auth.Tokens.RefreshToken = out.RefreshToken
	}
	if auth.Tokens.AccessToken == "" || auth.Tokens.RefreshToken == "" {
		return errors.New("Codex token refresh returned incomplete credentials")
	}
	auth.LastRefresh = nowRFC3339()
	return nil
}

type Usage struct {
	PlanType  string
	Primary   *UsageWindow
	Secondary *UsageWindow
	Buckets   []UsageBucket
}

// UsageBucket is one independently-metered Codex quota. The canonical Codex
// quota uses limit_id "codex"; the backend may append arbitrary model/feature
// quotas through additional_rate_limits without requiring a Foxy release.
type UsageBucket struct {
	LimitID         string       `json:"limit_id"`
	LimitName       string       `json:"limit_name,omitempty"`
	NormalModelSlug string       `json:"normal_model_slug,omitempty"`
	Primary         *UsageWindow `json:"primary,omitempty"`
	Secondary       *UsageWindow `json:"secondary,omitempty"`
}

type UsageWindow struct {
	UsedPercent   float64   `json:"used_percent"`
	ResetAt       time.Time `json:"reset_at"`
	WindowSeconds int64     `json:"window_seconds,omitempty"`
}

type rateLimitJSON struct {
	Primary   *usageWindowJSON `json:"primary_window"`
	Secondary *usageWindowJSON `json:"secondary_window"`
}

func FetchUsage(ctx context.Context, accessToken, accountID string) (*Usage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, UsageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("authorization", "Bearer "+accessToken)
	req.Header.Set("chatgpt-account-id", accountID)
	req.Header.Set("user-agent", "codex-cli")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Codex usage failed: HTTP %d", resp.StatusCode)
	}
	var payload struct {
		PlanType             string        `json:"plan_type"`
		RateLimit            rateLimitJSON `json:"rate_limit"`
		AdditionalRateLimits []struct {
			LimitName       string         `json:"limit_name"`
			MeteredFeature  string         `json:"metered_feature"`
			NormalModelSlug string         `json:"normal_model_slug"`
			RateLimit       *rateLimitJSON `json:"rate_limit"`
		} `json:"additional_rate_limits"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	usage := &Usage{
		PlanType:  payload.PlanType,
		Primary:   payload.RateLimit.Primary.value(),
		Secondary: payload.RateLimit.Secondary.value(),
	}
	if usage.Primary != nil || usage.Secondary != nil {
		usage.Buckets = append(usage.Buckets, UsageBucket{
			LimitID: "codex", Primary: usage.Primary, Secondary: usage.Secondary,
		})
	}
	for i, additional := range payload.AdditionalRateLimits {
		if additional.RateLimit == nil {
			continue
		}
		primary := additional.RateLimit.Primary.value()
		secondary := additional.RateLimit.Secondary.value()
		if primary == nil && secondary == nil {
			continue
		}
		limitID := additional.MeteredFeature
		if limitID == "" {
			limitID = additional.LimitName
		}
		if limitID == "" {
			limitID = fmt.Sprintf("additional-%d", i+1)
		}
		usage.Buckets = append(usage.Buckets, UsageBucket{
			LimitID: limitID, LimitName: additional.LimitName,
			NormalModelSlug: additional.NormalModelSlug,
			Primary:         primary, Secondary: secondary,
		})
	}
	return usage, nil
}

type usageWindowJSON struct {
	UsedPercent   float64 `json:"used_percent"`
	ResetAt       int64   `json:"reset_at"`
	WindowSeconds int64   `json:"limit_window_seconds"`
}

func (w *usageWindowJSON) value() *UsageWindow {
	if w == nil {
		return nil
	}
	return &UsageWindow{
		UsedPercent:   w.UsedPercent,
		ResetAt:       time.Unix(w.ResetAt, 0).UTC(),
		WindowSeconds: w.WindowSeconds,
	}
}
