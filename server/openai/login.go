package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/hoveychen/foxy-switcher/server/store"
)

// Device-code login endpoints. They are vars (not consts) so tests can point
// them at an httptest server. The paths mirror codex-rs
// login/src/device_code_auth.rs: usercode/token live under
// "{issuer}/api/accounts/deviceauth/*", the OAuth exchange reuses TokenURL,
// and the exchange redirect_uri is "{issuer}/deviceauth/callback".
var (
	DeviceUserCodeURL   = "https://auth.openai.com/api/accounts/deviceauth/usercode"
	DeviceTokenURL      = "https://auth.openai.com/api/accounts/deviceauth/token"
	DeviceRedirectURI   = "https://auth.openai.com/deviceauth/callback"
	DeviceVerificionURL = "https://auth.openai.com/codex/device"
)

// ErrAuthorizationPending is returned by PollDeviceLogin while the user has not
// yet entered and approved the one-time code. Callers wait DeviceAuth.Interval
// seconds and poll again.
var ErrAuthorizationPending = errors.New("Codex device authorization pending")

// ErrDeviceCodeUnsupported is returned when the issuer does not enable the
// device-code flow (usercode returns 404). Callers should fall back to the
// local `codex login` import path.
var ErrDeviceCodeUnsupported = errors.New("device code login is not enabled for this Codex server")

// DeviceAuth is the user-facing half of a device-code login: show UserCode and
// point the user at VerificationURL, then poll with the opaque DeviceAuthID.
type DeviceAuth struct {
	DeviceAuthID    string `json:"device_auth_id"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	Interval        int    `json:"interval"`
}

// StartDeviceLogin requests a one-time user code from the ChatGPT issuer. It
// mirrors request_user_code + request_device_code in codex-rs: POST
// {client_id} as JSON, and derive the verification URL client-side.
func StartDeviceLogin(ctx context.Context) (*DeviceAuth, error) {
	body, err := json.Marshal(map[string]string{"client_id": codexOAuthClientID})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, DeviceUserCodeURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrDeviceCodeUnsupported
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Codex device code request failed: HTTP %d", resp.StatusCode)
	}
	var out struct {
		DeviceAuthID string  `json:"device_auth_id"`
		UserCode     string  `json:"user_code"`
		UserCodeAlt  string  `json:"usercode"`
		Interval     flexInt `json:"interval"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse Codex device code response: %w", err)
	}
	code := out.UserCode
	if code == "" {
		code = out.UserCodeAlt
	}
	if out.DeviceAuthID == "" || code == "" {
		return nil, errors.New("Codex device code response is missing device_auth_id or user_code")
	}
	interval := int(out.Interval)
	if interval <= 0 {
		interval = 5
	}
	return &DeviceAuth{
		DeviceAuthID:    out.DeviceAuthID,
		UserCode:        code,
		VerificationURL: DeviceVerificionURL,
		Interval:        interval,
	}, nil
}

// PollDeviceLogin performs a single poll of the device token endpoint. While
// the user has not approved the code it returns ErrAuthorizationPending; once
// approved it exchanges the returned authorization code for tokens and builds a
// store.Account (ChatGPT subscription), ready to persist.
func PollDeviceLogin(ctx context.Context, da *DeviceAuth) (*store.Account, error) {
	if da == nil || da.DeviceAuthID == "" || da.UserCode == "" {
		return nil, errors.New("Codex device login not started")
	}
	body, err := json.Marshal(map[string]string{
		"device_auth_id": da.DeviceAuthID,
		"user_code":      da.UserCode,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, DeviceTokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	// 403/404 means the user has not finished entering the code yet — mirror
	// codex-rs poll_for_token, which treats both as "keep polling".
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		return nil, ErrAuthorizationPending
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Codex device auth failed: HTTP %d", resp.StatusCode)
	}
	var code struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeVerifier      string `json:"code_verifier"`
	}
	if err := json.Unmarshal(raw, &code); err != nil {
		return nil, fmt.Errorf("parse Codex device token response: %w", err)
	}
	if code.AuthorizationCode == "" || code.CodeVerifier == "" {
		return nil, errors.New("Codex device token response is missing authorization_code or code_verifier")
	}
	tokens, err := exchangeAuthorizationCode(ctx, code.AuthorizationCode, code.CodeVerifier, DeviceRedirectURI)
	if err != nil {
		return nil, err
	}
	auth := &AuthFile{
		AuthMode:    "chatgpt",
		Tokens:      *tokens,
		LastRefresh: nowRFC3339(),
	}
	return auth.Account()
}

// exchangeAuthorizationCode swaps an authorization code for id/access/refresh
// tokens against the OAuth token endpoint. redirect_uri must match the value
// bound to the code (the device-auth callback for the device flow). Codex is a
// public client, so no client_secret is sent.
func exchangeAuthorizationCode(ctx context.Context, code, verifier, redirectURI string) (*TokenData, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", codexOAuthClientID)
	form.Set("code_verifier", verifier)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
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
		return nil, fmt.Errorf("Codex token exchange failed: HTTP %d", resp.StatusCode)
	}
	var out struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out.AccessToken == "" || out.RefreshToken == "" {
		return nil, errors.New("Codex token exchange returned incomplete credentials")
	}
	td := &TokenData{
		IDToken:      out.IDToken,
		AccessToken:  out.AccessToken,
		RefreshToken: out.RefreshToken,
	}
	if claims, err := parseJWTClaims(out.IDToken); err == nil {
		td.AccountID = claims.Auth.ChatGPTAccountID
	}
	return td, nil
}

// flexInt accepts either a JSON number or a numeric JSON string. codex-rs sends
// the poll interval as a quoted string ("5"); older/newer servers may send a
// bare number, so we tolerate both.
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	s = strings.Trim(s, `"`)
	if s == "" {
		*f = 0
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return err
	}
	*f = flexInt(n)
	return nil
}
