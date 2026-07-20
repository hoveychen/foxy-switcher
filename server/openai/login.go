package openai

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

	"github.com/hoveychen/foxy-switcher/server/store"
)

// Codex login uses the standard OAuth authorization-code + PKCE flow that
// `codex login` itself uses — not the device-code flow. Device-code auth is a
// ChatGPT workspace toggle that admins can (and often do) disable, which
// surfaces as "contact your workspace admin to enable device code
// authentication"; the authorization-code flow has no such gate.
//
// The parameters mirror codex-rs login/src/server.rs: authorize at
// {issuer}/oauth/authorize with a loopback redirect_uri, then exchange the
// returned code at TokenURL reusing that same redirect_uri. In a remote/vault
// deployment the loopback never resolves, so the user copies the failed
// callback URL from their browser's address bar (it still carries
// ?code=...&state=...) and pastes it back — the web equivalent of the CLI's
// localhost listener. This mirrors the Claude paste-code login in server/authz.

// CodexAuthorizeURL is a var (not const) so tests can point it at an httptest
// server.
var CodexAuthorizeURL = "https://auth.openai.com/oauth/authorize"

// CodexRedirectURI is the loopback callback codex-rs registers with the OAuth
// client. It must be sent identically on the authorize request and the token
// exchange, so it lives here as the single source of truth.
const CodexRedirectURI = "http://localhost:1455/auth/callback"

// codexOAuthScopes mirrors the scope set codex-rs requests.
const codexOAuthScopes = "openid profile email offline_access api.connectors.read api.connectors.invoke"

// AuthorizeURL builds the consent URL the user opens in their browser. It
// mirrors codex-rs: response_type=code, S256 PKCE, the loopback redirect_uri,
// and the id_token_add_organizations / codex_cli_simplified_flow flags that
// make ChatGPT attach the workspace/org claims Codex needs.
func AuthorizeURL(codeChallenge, state string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", codexOAuthClientID)
	q.Set("redirect_uri", CodexRedirectURI)
	q.Set("scope", codexOAuthScopes)
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("state", state)
	return CodexAuthorizeURL + "?" + q.Encode()
}

// NewPKCEPair returns a fresh (verifier, challenge) PKCE pair. The verifier is
// the secret kept server-side; the challenge goes into the authorize URL.
func NewPKCEPair() (verifier, challenge string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("pkce verifier: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// NewState returns a base64url random string used as the OAuth state, echoed
// back via the redirect to defeat cross-flow paste accidents.
func NewState() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// ParsePastedCode extracts the authorization code and state from whatever the
// user pasted back. OpenAI redirects to the loopback callback with code+state
// as query params, so the user typically pastes the whole URL
// ("http://localhost:1455/auth/callback?code=...&state=...") copied from the
// browser address bar. For resilience we also accept a bare query string
// ("code=...&state=...") or the "code#state" fragment form used by the Claude
// flow.
func ParsePastedCode(pasted string) (code, state string, err error) {
	pasted = strings.TrimSpace(pasted)
	if pasted == "" {
		return "", "", errors.New("empty code")
	}
	// Full URL with a query string.
	if u, perr := url.Parse(pasted); perr == nil && u.RawQuery != "" {
		if c := u.Query().Get("code"); c != "" {
			return c, u.Query().Get("state"), nil
		}
	}
	// Bare "code=...&state=..." query string.
	if q, perr := url.ParseQuery(pasted); perr == nil {
		if c := q.Get("code"); c != "" {
			return c, q.Get("state"), nil
		}
	}
	// "code#state" fallback (mirrors the Claude paste form).
	if i := strings.IndexByte(pasted, '#'); i > 0 && i < len(pasted)-1 {
		return pasted[:i], pasted[i+1:], nil
	}
	return "", "", errors.New("could not find an authorization code — copy the whole callback URL from your browser's address bar")
}

// CompleteLogin exchanges the pasted authorization code (with the PKCE verifier
// kept from AuthorizeURL) for tokens and builds a store.Account ready to
// persist. The redirect_uri must match the one used to obtain the code.
func CompleteLogin(ctx context.Context, verifier, code string) (*store.Account, error) {
	if verifier == "" || code == "" {
		return nil, errors.New("Codex login not started or missing authorization code")
	}
	tokens, err := exchangeAuthorizationCode(ctx, code, verifier, CodexRedirectURI)
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
// bound to the code. Codex is a public client, so no client_secret is sent.
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
