// Package credinject manages Claude Code's credential backend on behalf of
// foxy-switcher. Instead of bolting an apiKeyHelper into ~/.claude/settings.json
// (which routes traffic through Claude Code's "external API key" path that
// strips the OAuth beta header and 401s on sk-ant-oat01- tokens), we write
// the picked account's OAuth blob directly into Claude Code's native storage:
//
//   - macOS: the `Claude Code-credentials` keychain item via the `security`
//     CLI; the `Claude Code` managed-API-key item is deleted on inject so
//     Claude Code falls through to the OAuth path.
//   - Linux/Windows: ~/.claude/.credentials.json (mode 0600); ~/.claude.json
//     `primaryApiKey` is dropped on inject.
//
// The blob format matches what Claude Code writes after `claude /login`:
// see docs/keychain-credentials-pool.md §2.1 in this repo for the precise
// shape and the rationale for each field.
package credinject

import (
	"encoding/json"
	"strings"

	"github.com/hoveychen/foxy-switcher/server/store"
)

// claudeCodeClientID is the OAuth client ID Anthropic registered for Claude
// Code (prod). The same constant is shared by every Claude Code installation
// — it identifies the application, not the user — so we can hard-code it.
// Source: claude-code-fork/src/constants/oauth.ts (prod).
const claudeCodeClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

// Backend is the platform-specific persistence layer. Two items are managed
// per platform: the OAuth tokens blob and a managed API key. The managed key
// is deleted on inject (forcing OAuth) and restored from the native backup on
// shutdown.
type Backend interface {
	ReadOAuthBlob() ([]byte, bool, error) // bool = exists
	WriteOAuthBlob(blob []byte) error
	DeleteOAuthBlob() error

	ReadManagedAPIKey() (string, bool, error)
	WriteManagedAPIKey(key string) error
	DeleteManagedAPIKey() error
}

// buildOAuthBlob serialises an Account into the JSON shape Claude Code expects
// in `Claude Code-credentials` (macOS) / ~/.claude/.credentials.json
// (Linux/Windows). The byte layout is identical across platforms.
func buildOAuthBlob(a *store.Account) ([]byte, error) {
	payload := map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":      a.AccessToken,
			"refreshToken":     a.RefreshToken,
			"expiresAt":        a.ExpiresAt,
			"scopes":           parseScopes(a.Scopes),
			"subscriptionType": a.SubscriptionType,
			"rateLimitTier":    deriveRateLimitTier(a.SubscriptionType),
			"clientId":         claudeCodeClientID,
		},
		"organizationUuid": a.OrganizationUUID,
	}
	return json.Marshal(payload)
}

// parseScopes turns the store's flat scope string (space-separated, OAuth 2.0
// §3.3) into the JSON array Claude Code expects. Falls back to the two scopes
// the OAuth app defaults to if the field is empty (login flows that predate
// scope storage).
func parseScopes(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return []string{"user:inference", "user:profile"}
	}
	parts := strings.Fields(s)
	if len(parts) == 0 {
		return []string{"user:inference", "user:profile"}
	}
	return parts
}

// deriveRateLimitTier maps subscription_type to the rateLimitTier string
// Claude Code stores. Per user decision in the plan, we hard-code this rather
// than adding a column. Empty string for unknown tiers — Claude Code treats
// it as "no badge" rather than rejecting the blob.
func deriveRateLimitTier(sub string) string {
	switch sub {
	case "max":
		return "default_claude_max_5x"
	case "team":
		return "default_claude_max_5x"
	case "team_premium":
		return "default_claude_max_20x"
	case "pro":
		return "default_claude_pro"
	default:
		return ""
	}
}

// extractAccessToken pulls claudeAiOauth.accessToken out of a stored blob.
// Returns "" on any parse failure — callers treat that as "not ours / can't
// match", which is the right safe default.
func extractAccessToken(blob []byte) string {
	var parsed struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(blob, &parsed); err != nil {
		return ""
	}
	return parsed.ClaudeAiOauth.AccessToken
}

// extractRotation pulls the (accessToken, refreshToken, expiresAt) tuple out
// of a stored blob — used by reverse-sync to detect when Claude Code has
// rotated the tokens behind our back.
func extractRotation(blob []byte) (accessToken, refreshToken string, expiresAt int64, ok bool) {
	var parsed struct {
		ClaudeAiOauth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(blob, &parsed); err != nil {
		return "", "", 0, false
	}
	return parsed.ClaudeAiOauth.AccessToken, parsed.ClaudeAiOauth.RefreshToken, parsed.ClaudeAiOauth.ExpiresAt, true
}
