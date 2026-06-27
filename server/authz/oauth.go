// Package authz implements the Anthropic OAuth flow used by Claude Code:
// PKCE authorization-code grant for adding a new subscription account, and
// refresh_token grant for keeping an existing account's access_token alive.
//
// The endpoint, client_id and scopes were verified empirically in
// hoveychen/bobo-gambler — keep them in sync if Anthropic ever rotates them.
package authz

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrInvalidGrant is the terminal refresh failure: Anthropic's token endpoint
// answered with OAuth error "invalid_grant", meaning the refresh_token itself
// is dead (revoked, or already rotated by a racing refresh). Unlike a network
// blip or a 5xx — which are transient and worth retrying — an invalid_grant
// will never succeed on retry; the account needs the user to re-authenticate.
// Callers use errors.Is(err, ErrInvalidGrant) to tell the two apart.
var ErrInvalidGrant = errors.New("oauth: refresh_token rejected (invalid_grant)")

const (
	ClaudeOAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	ClaudeAuthorizeURL  = "https://claude.ai/oauth/authorize"
	ManualRedirectURL   = "https://platform.claude.com/oauth/code/callback"
	ClaudeOAuthScopes   = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
)

// ClaudeTokenURL is exposed as a var so tests can swap in an httptest.Server.
var ClaudeTokenURL = "https://platform.claude.com/v1/oauth/token"

// TokenResponse mirrors the JSON returned by both authorization_code and
// refresh_token grants. Anthropic doesn't always populate scope/refresh_token
// on refresh, so callers must treat empty strings as "preserve existing".
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
}

// ExpiresAtMillis converts ExpiresIn (seconds) to an absolute unix-millis
// timestamp anchored to now. Returns 0 when ExpiresIn is unset.
func (r *TokenResponse) ExpiresAtMillis() int64 {
	if r.ExpiresIn <= 0 {
		return 0
	}
	return time.Now().UnixMilli() + r.ExpiresIn*1000
}

// AuthorizeURL builds the consent URL the user copies into their browser.
// codeChallenge must be the SHA-256-then-base64url-no-padding digest of a
// freshly generated code verifier; state is round-tripped back via the
// redirect to defeat cross-flow paste accidents.
func AuthorizeURL(codeChallenge, state string) string {
	return fmt.Sprintf("%s?client_id=%s&code_challenge=%s&code_challenge_method=S256&scope=%s&redirect_uri=%s&state=%s&response_type=code",
		ClaudeAuthorizeURL,
		url.QueryEscape(ClaudeOAuthClientID),
		url.QueryEscape(codeChallenge),
		url.QueryEscape(ClaudeOAuthScopes),
		url.QueryEscape(ManualRedirectURL),
		url.QueryEscape(state),
	)
}

// NewPKCEPair returns a fresh (verifier, challenge) PKCE pair. The verifier
// is the random secret kept server-side; the challenge is what gets sent to
// the authorize URL.
func NewPKCEPair() (verifier, challenge string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("pkce verifier: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	hash := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(hash[:])
	return verifier, challenge, nil
}

// NewState returns a base64url random string used as the PKCE state field.
func NewState() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// SplitPastedCode parses the "code#state" string the user copies from the
// platform.claude.com callback page.
func SplitPastedCode(pasted string) (code, state string, err error) {
	pasted = strings.TrimSpace(pasted)
	if pasted == "" {
		return "", "", fmt.Errorf("empty code")
	}
	parts := strings.SplitN(pasted, "#", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid code format — expected 'authorizationCode#state'")
	}
	return parts[0], parts[1], nil
}

// ExchangeCode trades an authorization code (from /oauth/authorize) plus the
// PKCE verifier for a fresh token pair. Used when adding a new account.
func ExchangeCode(ctx context.Context, verifier, code, state string) (*TokenResponse, error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"code_verifier": verifier,
		"client_id":     ClaudeOAuthClientID,
		"redirect_uri":  ManualRedirectURL,
		"state":         state,
	})
	return postToken(ctx, body)
}

// RefreshToken trades a refresh_token for a new access_token (and possibly a
// rotated refresh_token). Used by the background scheduler.
func RefreshToken(ctx context.Context, refreshToken string) (*TokenResponse, error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     ClaudeOAuthClientID,
	})
	return postToken(ctx, body)
}

func postToken(ctx context.Context, body []byte) (*TokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ClaudeTokenURL, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token endpoint: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, classifyTokenError(resp.StatusCode, respBody)
	}
	var tr TokenResponse
	if err := json.Unmarshal(respBody, &tr); err != nil {
		return nil, fmt.Errorf("parse token response: %w", err)
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("empty access_token in response")
	}
	return &tr, nil
}

// classifyTokenError turns a non-200 response from the token endpoint into an
// error, distinguishing the terminal invalid_grant case (a dead refresh_token
// — wrapped as ErrInvalidGrant) from every other failure (transient, worth
// retrying). The OAuth 2.0 error body is JSON of the form {"error":"..."};
// we read the `error` field rather than the HTTP status because Anthropic
// returns invalid_grant under a 400.
func classifyTokenError(status int, body []byte) error {
	trimmed := strings.TrimSpace(string(body))
	var oerr struct {
		Error string `json:"error"`
	}
	// Body may not be JSON (proxy/5xx HTML) — ignore parse failures and fall
	// through to the generic transient error.
	_ = json.Unmarshal(body, &oerr)
	if oerr.Error == "invalid_grant" {
		return fmt.Errorf("token endpoint returned %d: %s: %w", status, trimmed, ErrInvalidGrant)
	}
	return fmt.Errorf("token endpoint returned %d: %s", status, trimmed)
}

// Scopes splits the space-separated scope field on a TokenResponse.
func Scopes(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return strings.Fields(s)
}
