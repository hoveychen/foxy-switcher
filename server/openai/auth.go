// Package openai implements the Codex subscription credential format and
// the small set of ChatGPT endpoints Foxy needs for account rotation.
package openai

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
)

const authClaimNamespace = "https://api.openai.com/auth"

var ErrNotChatGPTLogin = errors.New("Codex is not signed in with a ChatGPT subscription")

type TokenData struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
}

// AuthFile retains unknown top-level fields so a newer Codex CLI can extend
// auth.json without Foxy deleting those fields during a refresh or switch.
type AuthFile struct {
	AuthMode    string    `json:"auth_mode"`
	Tokens      TokenData `json:"tokens"`
	LastRefresh string    `json:"last_refresh,omitempty"`
	raw         map[string]json.RawMessage
}

func ParseAuthFile(data []byte) (*AuthFile, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse Codex auth.json: %w", err)
	}
	var auth AuthFile
	if v := raw["auth_mode"]; len(v) > 0 {
		_ = json.Unmarshal(v, &auth.AuthMode)
	}
	if v := raw["tokens"]; len(v) > 0 {
		if err := json.Unmarshal(v, &auth.Tokens); err != nil {
			return nil, fmt.Errorf("parse Codex tokens: %w", err)
		}
	}
	if v := raw["last_refresh"]; len(v) > 0 {
		_ = json.Unmarshal(v, &auth.LastRefresh)
	}
	if auth.Tokens.AccessToken == "" || auth.Tokens.RefreshToken == "" {
		return nil, ErrNotChatGPTLogin
	}
	claims, err := parseJWTClaims(auth.Tokens.IDToken)
	if err != nil {
		return nil, fmt.Errorf("parse Codex id_token: %w", err)
	}
	if auth.Tokens.AccountID == "" {
		auth.Tokens.AccountID = claims.Auth.ChatGPTAccountID
	}
	if auth.Tokens.AccountID == "" {
		return nil, fmt.Errorf("%w: account id is missing", ErrNotChatGPTLogin)
	}
	auth.raw = raw
	return &auth, nil
}

func (a *AuthFile) Marshal() ([]byte, error) {
	raw := make(map[string]json.RawMessage, len(a.raw)+3)
	for k, v := range a.raw {
		raw[k] = v
	}
	mode, _ := json.Marshal(a.AuthMode)
	tokens, err := json.Marshal(a.Tokens)
	if err != nil {
		return nil, err
	}
	raw["auth_mode"] = mode
	raw["tokens"] = tokens
	if a.LastRefresh != "" {
		last, _ := json.Marshal(a.LastRefresh)
		raw["last_refresh"] = last
	}
	return json.MarshalIndent(raw, "", "  ")
}

func DefaultAuthPath() (string, error) {
	home, err := defaultCodexHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "auth.json"), nil
}

func ImportCurrentStorage(storage CredentialStorage) (*store.Account, error) {
	data, found, err := storage.Load()
	if err != nil {
		return nil, fmt.Errorf("read Codex credentials from %s: %w", storage.Kind(), err)
	}
	if !found {
		return nil, fmt.Errorf("no Codex credentials found in %s", storage.Kind())
	}
	auth, err := ParseAuthFile(data)
	if err != nil {
		return nil, err
	}
	return auth.Account()
}

func ImportCurrent(path string) (*store.Account, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read Codex auth.json: %w", err)
	}
	auth, err := ParseAuthFile(data)
	if err != nil {
		return nil, err
	}
	return auth.Account()
}

func (a *AuthFile) Account() (*store.Account, error) {
	claims, err := parseJWTClaims(a.Tokens.IDToken)
	if err != nil {
		return nil, err
	}
	raw, err := a.Marshal()
	if err != nil {
		return nil, err
	}
	accountID := a.Tokens.AccountID
	if accountID == "" {
		accountID = claims.Auth.ChatGPTAccountID
	}
	name := claims.Email
	if claims.Name != "" {
		name = claims.Name
	}
	if name == "" {
		name = "Codex account"
	}
	plan := codexPlanLabel(claims.Auth.ChatGPTPlanType)
	return &store.Account{
		Provider:         store.ProviderCodex,
		Name:             name,
		AccessToken:      a.Tokens.AccessToken,
		RefreshToken:     a.Tokens.RefreshToken,
		ExpiresAt:        tokenExpiryMillis(a.Tokens.AccessToken, a.Tokens.IDToken),
		SubscriptionType: claims.Auth.ChatGPTPlanType,
		AccountUUID:      accountID,
		Email:            claims.Email,
		FullName:         claims.Name,
		Plan:             plan,
		CredentialJSON:   string(raw),
		Status:           store.StatusActive,
	}, nil
}

type jwtClaims struct {
	Email string `json:"email"`
	Name  string `json:"name"`
	Exp   int64  `json:"exp"`
	Auth  struct {
		ChatGPTAccountID string `json:"chatgpt_account_id"`
		ChatGPTPlanType  string `json:"chatgpt_plan_type"`
	} `json:"https://api.openai.com/auth"`
}

func parseJWTClaims(token string) (jwtClaims, error) {
	var claims jwtClaims
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return claims, errors.New("token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims, err
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return claims, err
	}
	return claims, nil
}

func tokenExpiryMillis(tokens ...string) int64 {
	for _, token := range tokens {
		claims, err := parseJWTClaims(token)
		if err == nil && claims.Exp > 0 {
			return claims.Exp * 1000
		}
	}
	return 0
}

// reverseSyncAllowed reports whether the credential currently sitting in this
// device's Codex storage may be written back over the pool's copy of the same
// account.
//
// The device is not automatically the newer side. Both the pool's refresh
// scheduler and the local Codex CLI rotate the one-time-use refresh_token, so a
// device that was offline (laptop asleep, agent killed, account not selected
// for a week) holds a credential the pool has long since rotated past. Pushing
// that copy up replaces a live refresh_token with a spent one; the next refresh
// then gets HTTP 401, the account is marked needs_reauth, and because the stale
// file is still on disk it clobbers the user's re-login within one reconcile
// tick too — the account can never be revived. That is exactly how account 68
// ("Harry C") died on 2026-08-20 with an access token 174h past expiry.
//
// So we push only when the local credential is demonstrably at least as new as
// the pool's, and never when it has already expired.
func reverseSyncAllowed(local *AuthFile, stored *store.Account, now time.Time) bool {
	if local == nil || stored == nil {
		return false
	}
	localExpiry := tokenExpiryMillis(local.Tokens.AccessToken, local.Tokens.IDToken)
	if localExpiry == 0 {
		// No parseable expiry, so the direction is unknowable. Refuse: a
		// missed write-back only costs one redundant pool-side refresh, while
		// a wrong-direction push costs the whole account.
		return false
	}
	if localExpiry <= now.UnixMilli() {
		return false
	}
	// Equal expiry means the same access-token generation — the local file may
	// still carry non-token edits from a newer Codex CLI, so let those through.
	return localExpiry >= stored.ExpiresAt
}

func codexPlanLabel(raw string) string {
	if raw == "" {
		return "Codex"
	}
	return "Codex " + strings.ToUpper(raw[:1]) + raw[1:]
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
