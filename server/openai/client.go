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
}

type UsageWindow struct {
	UsedPercent float64
	ResetAt     time.Time
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
		PlanType  string `json:"plan_type"`
		RateLimit struct {
			Primary   *usageWindowJSON `json:"primary_window"`
			Secondary *usageWindowJSON `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	return &Usage{
		PlanType:  payload.PlanType,
		Primary:   payload.RateLimit.Primary.value(),
		Secondary: payload.RateLimit.Secondary.value(),
	}, nil
}

type usageWindowJSON struct {
	UsedPercent float64 `json:"used_percent"`
	ResetAt     int64   `json:"reset_at"`
}

func (w *usageWindowJSON) value() *UsageWindow {
	if w == nil {
		return nil
	}
	return &UsageWindow{UsedPercent: w.UsedPercent, ResetAt: time.Unix(w.ResetAt, 0).UTC()}
}
