// Package tui implements the `foxy-switcher tui` subcommand: a Bubble Tea
// terminal UI for managing the local account pool over the daemon's HTTP API.
// Designed for SSH-into-server scenarios where the Tauri GUI isn't an option.
package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Client talks to the local daemon's HTTP API. The TUI never touches the
// SQLite store directly so we don't have to coordinate locks with the running
// daemon — same surface as the Tauri front-end.
type Client struct {
	base string
	hc   *http.Client
}

// NewClient resolves the daemon's port from `<dataDir>/port`.
func NewClient(dataDir string) (*Client, error) {
	portFile := filepath.Join(dataDir, "port")
	raw, err := os.ReadFile(portFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("daemon not running: %s does not exist (start the foxy-switcher daemon first)", portFile)
		}
		return nil, fmt.Errorf("read port file: %w", err)
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("port file %s contains %q: %w", portFile, string(raw), err)
	}
	return newClientForBase(fmt.Sprintf("http://127.0.0.1:%d", port)), nil
}

// newClientForBase constructs a client pointing at an explicit base URL.
// Used by tests that spin up an httptest.Server bypassing the port file.
func newClientForBase(base string) *Client {
	return &Client{
		base: base,
		hc:   &http.Client{Timeout: 30 * time.Second},
	}
}

// UsageWindow mirrors httpapi.usageWindowView. Utilization is 0–100 percent.
type UsageWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

// Account mirrors httpapi.accountView. Tokens are deliberately not exposed.
type Account struct {
	ID               int64        `json:"id"`
	Name             string       `json:"name"`
	ExpiresAt        int64        `json:"expires_at"`
	Scopes           string       `json:"scopes"`
	SubscriptionType string       `json:"subscription_type"`
	OrganizationUUID string       `json:"organization_uuid"`
	Status           string       `json:"status"`
	CooldownUntil    int64        `json:"cooldown_until"`
	LastUsedAt       int64        `json:"last_used_at"`
	Last429At        int64        `json:"last_429_at"`
	CreatedAt        int64        `json:"created_at"`
	UpdatedAt        int64        `json:"updated_at"`
	Email            string       `json:"email"`
	FullName         string       `json:"full_name"`
	OrganizationName string       `json:"organization_name"`
	Plan             string       `json:"plan"`
	FiveHour         *UsageWindow `json:"five_hour,omitempty"`
	SevenDay         *UsageWindow `json:"seven_day,omitempty"`
	SevenDaySonnet   *UsageWindow `json:"seven_day_sonnet,omitempty"`
	UsageFetchedAt   int64        `json:"usage_fetched_at"`
}

// CredStatus mirrors handleCredStatus.
type CredStatus struct {
	ManagedAccountID    int64 `json:"managed_account_id"`
	NativeBackupPresent bool  `json:"native_backup_present"`
	InjectedAt          int64 `json:"injected_at"`
}

// LoginStart mirrors handleLoginStart.
type LoginStart struct {
	AuthorizeURL string `json:"authorize_url"`
	State        string `json:"state"`
}

func (c *Client) ListAccounts(ctx context.Context) ([]Account, error) {
	var out struct {
		Accounts []Account `json:"accounts"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/accounts", nil, &out); err != nil {
		return nil, err
	}
	return out.Accounts, nil
}

func (c *Client) CredStatus(ctx context.Context) (CredStatus, error) {
	var out CredStatus
	if err := c.do(ctx, http.MethodGet, "/api/cred/status", nil, &out); err != nil {
		return CredStatus{}, err
	}
	return out, nil
}

func (c *Client) Resume(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/api/accounts/%d/resume", id), nil, nil)
}

func (c *Client) Pause(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/api/accounts/%d/pause", id), nil, nil)
}

func (c *Client) Delete(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodDelete, fmt.Sprintf("/api/accounts/%d", id), nil, nil)
}

// SetCooldown sets a relative cooldown (now + d). Pass d=0 to clear.
func (c *Client) SetCooldown(ctx context.Context, id int64, d time.Duration) error {
	body := map[string]int64{}
	if d > 0 {
		body["duration_ms"] = d.Milliseconds()
	} else {
		body["until_millis"] = 0
	}
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/api/accounts/%d/cooldown", id), body, nil)
}

func (c *Client) RefreshNow(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/api/accounts/%d/refresh", id), nil, nil)
}

func (c *Client) LoginStart(ctx context.Context) (LoginStart, error) {
	var out LoginStart
	if err := c.do(ctx, http.MethodPost, "/api/accounts/login", nil, &out); err != nil {
		return LoginStart{}, err
	}
	return out, nil
}

func (c *Client) LoginCallback(ctx context.Context, pasted, state string) error {
	body := map[string]string{"pasted": pasted, "state": state}
	return c.do(ctx, http.MethodPost, "/api/accounts/callback", body, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
