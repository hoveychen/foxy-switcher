// Package httpclient is the agent-side implementation of vault.Service:
// every method is a JSON-over-HTTP call to a vault running httpserver.
// Combined with vault.InProc this completes the Service surface — main
// picks one based on --mode.
//
// Step 2 is unauthenticated; the Authorization header is added in Step 3.
package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hoveychen/foxy-switcher/server/selector"
	"github.com/hoveychen/foxy-switcher/server/vault"
)

// Client implements vault.Service against a remote httpserver.
type Client struct {
	baseURL string
	hc      *http.Client
	token   string // bearer token; empty until SetToken
}

// New constructs a Client. baseURL must include scheme and host (e.g.
// "https://vault.example.com"); a trailing slash is tolerated. The HTTP
// client defaults to a 30s per-request timeout — generous for the slow path
// (Pick can fall behind Anthropic's API in the worst case) but tight enough
// to cancel a stuck connection before the agent's reconcile tick lapses.
func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		hc:      &http.Client{Timeout: 30 * time.Second},
	}
}

// SetToken installs the bearer token used by every subsequent HTTP call.
// The agent receives the token from the device-flow pair-poll response and
// persists it; on startup it loads the token from disk and calls this.
func (c *Client) SetToken(token string) {
	c.token = token
}

// Compile-time assertion.
var _ vault.Service = (*Client)(nil)

func (c *Client) ListAccounts(ctx context.Context) ([]vault.Account, error) {
	var out struct {
		Accounts []vault.Account `json:"accounts"`
	}
	if err := c.get(ctx, "/agent/v1/accounts", &out); err != nil {
		return nil, err
	}
	return out.Accounts, nil
}

func (c *Client) GetAutoSwitch(ctx context.Context) (vault.AutoSwitch, error) {
	var v vault.AutoSwitch
	if err := c.get(ctx, "/agent/v1/auto-switch", &v); err != nil {
		return vault.AutoSwitch{}, err
	}
	return v, nil
}

func (c *Client) Pick(ctx context.Context, _ time.Time) (*vault.Account, error) {
	return c.pickProvider(ctx, "")
}

func (c *Client) pickProvider(ctx context.Context, provider string) (*vault.Account, error) {
	// 204 from the server = no eligible account. The inproc path returns
	// selector.ErrNoAvailable in that case; mirror it so the coordinator's
	// errors.Is check works regardless of which Service implementation it
	// holds.
	path := "/agent/v1/pick"
	if provider != "" {
		path += "?provider=" + url.QueryEscape(provider)
	}
	resp, err := c.do(ctx, http.MethodPost, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, selector.ErrNoAvailable
	}
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	var out struct {
		Account *vault.Account `json:"account"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode pick response: %w", err)
	}
	return out.Account, nil
}

// PickForDevice forwards to /agent/v1/pick — the server reads the
// caller's device id from the BearerAuth ctx (the same Bearer token
// the client already attaches), so deviceID is intentionally unused
// here. Kept on the wire as a Service-interface method for symmetry
// with InProc.
func (c *Client) PickForDevice(ctx context.Context, now time.Time, _ string) (*vault.Account, error) {
	return c.Pick(ctx, now)
}

func (c *Client) PickProviderForDevice(ctx context.Context, _ time.Time, _ string, provider string) (*vault.Account, error) {
	return c.pickProvider(ctx, provider)
}

func (c *Client) MarkUsed(ctx context.Context, id int64) error {
	return c.postNoBody(ctx, "/agent/v1/accounts/"+strconv.FormatInt(id, 10)+"/used")
}

func (c *Client) UpdateTokens(ctx context.Context, id int64, accessToken, refreshToken string, expiresAt int64) error {
	return c.UpdateProviderCredential(ctx, id, accessToken, refreshToken, expiresAt, "")
}

func (c *Client) UpdateProviderCredential(ctx context.Context, id int64, accessToken, refreshToken string, expiresAt int64, credentialJSON string) error {
	body := map[string]any{
		"access_token":    accessToken,
		"refresh_token":   refreshToken,
		"expires_at":      expiresAt,
		"credential_json": credentialJSON,
	}
	return c.postJSON(ctx, "/agent/v1/accounts/"+strconv.FormatInt(id, 10)+"/tokens", body, nil)
}

func (c *Client) AcquireLease(ctx context.Context, accountID int64, deviceID string, ttl time.Duration) (vault.Lease, error) {
	body := map[string]any{
		"account_id": accountID,
		"device_id":  deviceID,
		"ttl_ms":     ttl.Milliseconds(),
	}
	resp, err := c.do(ctx, http.MethodPost, "/agent/v1/leases", body)
	if err != nil {
		return vault.Lease{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		// Cross-device contention — caller's coordinator picks a different
		// account on its next reconcile.
		return vault.Lease{}, vault.ErrLeaseLocked
	}
	if resp.StatusCode != http.StatusOK {
		return vault.Lease{}, decodeError(resp)
	}
	var lease vault.Lease
	if err := json.NewDecoder(resp.Body).Decode(&lease); err != nil {
		return vault.Lease{}, err
	}
	return lease, nil
}

func (c *Client) RenewLease(ctx context.Context, leaseID string, ttl, idleFor time.Duration) (vault.Lease, error) {
	if idleFor < 0 {
		idleFor = 0
	}
	body := map[string]any{"ttl_ms": ttl.Milliseconds(), "idle_ms": idleFor.Milliseconds()}
	var lease vault.Lease
	resp, err := c.do(ctx, http.MethodPost, "/agent/v1/leases/"+url.PathEscape(leaseID)+"/renew", body)
	if err != nil {
		return vault.Lease{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return vault.Lease{}, vault.ErrLeaseNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return vault.Lease{}, decodeError(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(&lease); err != nil {
		return vault.Lease{}, fmt.Errorf("decode lease: %w", err)
	}
	return lease, nil
}

func (c *Client) ReleaseLease(ctx context.Context, leaseID string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/agent/v1/leases/"+url.PathEscape(leaseID), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		return nil
	}
	return decodeError(resp)
}

// --- transport helpers ---------------------------------------------------

func (c *Client) get(ctx context.Context, path string, out any) error {
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return decodeError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) postNoBody(ctx context.Context, path string) error {
	resp, err := c.do(ctx, http.MethodPost, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return decodeError(resp)
	}
	return nil
}

func (c *Client) postJSON(ctx context.Context, path string, body, out any) error {
	resp, err := c.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return decodeError(resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return c.hc.Do(req)
}

// decodeError extracts the JSON `{"error":"…"}` body the server emits.
// Falls back to a plain status string when the body isn't JSON.
func decodeError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	var parsed struct {
		Err string `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Err != "" {
		return fmt.Errorf("vault %s: %s", resp.Status, parsed.Err)
	}
	if len(body) > 0 {
		return fmt.Errorf("vault %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return fmt.Errorf("vault %s", resp.Status)
}
