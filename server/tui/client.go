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
	"net/url"
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
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	ExpiresAt        int64  `json:"expires_at"`
	Scopes           string `json:"scopes"`
	SubscriptionType string `json:"subscription_type"`
	// RateLimitTier mirrors httpapi.accountView.rate_limit_tier — the
	// authoritative quota label ("default_claude_pro" /
	// "default_claude_max_5x" / "default_claude_max_20x"). Empty on legacy
	// rows that haven't been backfilled by UsagePoller yet. The dashboard's
	// pool-aggregate weighting keys off this; subscription_type alone
	// can't tell personal Max 5x apart from Max 20x.
	RateLimitTier    string       `json:"rate_limit_tier"`
	OrganizationUUID string       `json:"organization_uuid"`
	Status           string       `json:"status"`
	TokenExpired     bool         `json:"token_expired"`
	LastUsedAt       int64        `json:"last_used_at"`
	CreatedAt        int64        `json:"created_at"`
	UpdatedAt        int64        `json:"updated_at"`
	Email            string       `json:"email"`
	FullName         string       `json:"full_name"`
	OrganizationName string       `json:"organization_name"`
	Plan             string       `json:"plan"`
	FiveHour         *UsageWindow `json:"five_hour,omitempty"`
	SevenDay         *UsageWindow `json:"seven_day,omitempty"`
	// SevenDaySonnet is a legacy name (JSON key kept for wire compat): it carries
	// the per-model weekly-scoped window (Fable/…). Render its label with
	// ScopedModelLabel(). See server/store Account.SevenDaySonnetUtil.
	SevenDaySonnet *UsageWindow `json:"seven_day_sonnet,omitempty"`
	// SevenDayScopedLabel is the model display name for the seven_day_sonnet
	// slot, which now carries the per-model weekly-scoped window (e.g. "Fable").
	// Empty for legacy data; use ScopedModelLabel() to render.
	SevenDayScopedLabel string `json:"seven_day_scoped_label,omitempty"`
	UsageFetchedAt      int64  `json:"usage_fetched_at"`
	// Per-account utilization thresholds (0–100). 100 disables the
	// corresponding window — the selector won't skip the account on it.
	FiveHourThreshold       float64 `json:"five_hour_threshold"`
	SevenDayThreshold       float64 `json:"seven_day_threshold"`
	SevenDaySonnetThreshold float64 `json:"seven_day_sonnet_threshold"`
	// Lease mirrors httpapi.accountLeaseView. Nil when no live lease exists.
	// Populated by the daemon's /api/accounts handler from
	// store.ListAccountsWithLeases. The TUI uses this so multi-device users
	// can see "held by Device X" instead of clicking 'u' on a foreign-leased
	// account and hitting a 409 from the leases_account_id_uniq index.
	Lease *AccountLease `json:"lease,omitempty"`
}

// ScopedModelLabel is the display name of the per-model weekly-scoped usage
// window (the seven_day_sonnet slot), e.g. "Fable". Defaults to "Sonnet" for
// legacy data that predates the label so existing rows keep their wording.
func (a Account) ScopedModelLabel() string {
	if a.SevenDayScopedLabel != "" {
		return a.SevenDayScopedLabel
	}
	return "Sonnet"
}

// AccountLease is the per-account lease metadata. Mine is server-computed
// from the BearerAuth ctx (or implicit true in combined mode where loopback
// is the only caller); the TUI trusts it as-is and never compares device
// ids itself.
type AccountLease struct {
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name"`
	Mine       bool   `json:"mine"`
	AcquiredAt int64  `json:"acquired_at"`
	ExpiresAt  int64  `json:"expires_at"`
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

func (c *Client) RefreshNow(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/api/accounts/%d/refresh", id), nil, nil)
}

// SelectAccount pins this account as the next pick — the daemon's reconcile
// loop will inject its credentials on the next tick. Mirrors the UI's "Use
// now" button.
func (c *Client) SelectAccount(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/api/accounts/%d/select", id), nil, nil)
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

// DashboardKPIs mirrors httpapi.DashboardKPIs. The TUI doesn't render
// the in_use[] list at the dashboard level — its accounts list already
// surfaces foreign-device leases per row via Account.Lease, so a
// duplicate dashboard sidebar would be redundant. The "in use" indicator
// in the TUI header is still derived from the local credinject
// Coordinator's ManagedAccountID, not from this struct.
type DashboardKPIs struct {
	PoolSize        int   `json:"pool_size"`
	ActiveCount     int   `json:"active_count"`
	CoolingCount    int   `json:"cooling_count"`
	NextResetAt     int64 `json:"next_reset_at"`
	PeakUtilPercent int   `json:"peak_util_percent"`
}

// DashboardTrendBucket mirrors httpapi.DashboardTrendBucket. One hour-aligned
// bucket carrying both the legacy max-utilization fields (FiveHour / SevenDay
// / SevenDaySonnet — the pool-wide peak in the bucket, used by the TUI for
// historical reasons) and the weighted-pool aggregates the dashboard now
// renders (FiveHour{Used,Capacity,Pct} / SevenDay{Used,Capacity,Pct}). All
// percentages are 0–100. *Pct is the canonical "how full is the pool"
// number — used / capacity * 100, with 0 when capacity is 0.
type DashboardTrendBucket struct {
	TS               int64   `json:"ts"`
	FiveHour         float64 `json:"five_hour"`
	SevenDay         float64 `json:"seven_day"`
	SevenDaySonnet   float64 `json:"seven_day_sonnet"`
	FiveHourUsed     float64 `json:"five_hour_used"`
	FiveHourCapacity float64 `json:"five_hour_capacity"`
	FiveHourPct      float64 `json:"five_hour_pct"`
	SevenDayUsed     float64 `json:"seven_day_used"`
	SevenDayCapacity float64 `json:"seven_day_capacity"`
	SevenDayPct      float64 `json:"seven_day_pct"`
}

// Dashboard mirrors httpapi.DashboardResponse.
type Dashboard struct {
	KPIs  DashboardKPIs          `json:"kpis"`
	Trend []DashboardTrendBucket `json:"trend"`
}

func (c *Client) GetDashboard(ctx context.Context) (Dashboard, error) {
	var out Dashboard
	if err := c.do(ctx, http.MethodGet, "/api/dashboard", nil, &out); err != nil {
		return Dashboard{}, err
	}
	return out, nil
}

// About mirrors httpapi.AboutResponse.
type About struct {
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	CommitDirty   bool   `json:"commit_dirty"`
	BuildTime     string `json:"build_time"`
	GoVersion     string `json:"go_version"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	PID           int    `json:"pid"`
	Port          int    `json:"port"`
	DataDir       string `json:"data_dir"`
	SQLitePath    string `json:"sqlite_path"`
	SQLiteSizeB   int64  `json:"sqlite_size_b"`
	StartedAtMS   int64  `json:"started_at_ms"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	// Mode + VaultURL + DeviceID let the onboarding decision know
	// whether the daemon is already paired ("agent" with a vault_url)
	// or running combined. The desktop already consumes these fields;
	// the TUI mirrors them here to drive the same auto-dismiss rule.
	Mode     string `json:"mode"`
	VaultURL string `json:"vault_url"`
	DeviceID string `json:"device_id"`
}

// PairInitOut mirrors the daemon's POST /api/pair/init response.
type PairInitOut struct {
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	ExpiresInMs     int64  `json:"expires_in_ms"`
}

// PairMeta is the optional device-meta payload for pair-init. Mirrors the
// wire format vault/httpclient publishes; defined locally so the TUI
// doesn't take a dependency on the agent-side httpclient package.
type PairMeta struct {
	Hostname   string `json:"hostname,omitempty"`
	OS         string `json:"os,omitempty"`
	OSVersion  string `json:"os_version,omitempty"`
	Arch       string `json:"arch,omitempty"`
	Model      string `json:"model,omitempty"`
	AppVersion string `json:"app_version,omitempty"`
	ClientType string `json:"client_type,omitempty"`
}

// PairPollOut mirrors the daemon's POST /api/pair/poll response.
// Status is one of "pending" | "approved" | "denied" | "expired".
// device_id / device_token are only populated for "approved".
type PairPollOut struct {
	Status      string `json:"status"`
	DeviceID    string `json:"device_id,omitempty"`
	DeviceToken string `json:"device_token,omitempty"`
}

// PairInit starts the device-flow handshake against vaultURL via the
// daemon's loopback proxy. The proxy is unaffected by daemon mode —
// combined-mode daemons are perfectly happy to forward the request to
// the cloud vault during onboarding, before any agent-config.json
// exists.
func (c *Client) PairInit(ctx context.Context, vaultURL, deviceName, clientNonce string, meta *PairMeta) (PairInitOut, error) {
	body := map[string]any{
		"vault_url":    vaultURL,
		"device_name":  deviceName,
		"client_nonce": clientNonce,
	}
	if meta != nil {
		body["device_meta"] = meta
	}
	var out PairInitOut
	if err := c.do(ctx, http.MethodPost, "/api/pair/init", body, &out); err != nil {
		return PairInitOut{}, err
	}
	return out, nil
}

// PairPoll asks the daemon for the current pairing state.
func (c *Client) PairPoll(ctx context.Context, vaultURL, clientNonce string) (PairPollOut, error) {
	body := map[string]string{
		"vault_url":    vaultURL,
		"client_nonce": clientNonce,
	}
	var out PairPollOut
	if err := c.do(ctx, http.MethodPost, "/api/pair/poll", body, &out); err != nil {
		return PairPollOut{}, err
	}
	return out, nil
}

func (c *Client) GetAbout(ctx context.Context) (About, error) {
	var out About
	if err := c.do(ctx, http.MethodGet, "/api/about", nil, &out); err != nil {
		return About{}, err
	}
	return out, nil
}

// ActivityEvent mirrors activity.Event. Severity is "info" / "warn" / "error".
type ActivityEvent struct {
	ID        int64           `json:"id"`
	Timestamp int64           `json:"timestamp"`
	Type      string          `json:"type"`
	Severity  string          `json:"severity"`
	AccountID int64           `json:"account_id,omitempty"`
	Message   string          `json:"message"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// ActivityFilter is the subset of /api/activity query params the TUI uses.
type ActivityFilter struct {
	Limit    int      // 0 = server default (200)
	SinceID  int64    // 0 = no incremental anchor
	Types    []string // OR-ed; empty = all
	Severity string   // "" / "info" / "warn" / "error"
}

// Settings mirrors store.Settings. Numeric fields are clamped server-side, so
// callers can submit raw values and rely on the response to echo the canonical
// form (the TUI snaps its row state to the response).
type Settings struct {
	Theme                          string  `json:"theme"`
	SidebarMode                    string  `json:"sidebar_mode"`
	UsagePollIntervalSec           int     `json:"usage_poll_interval_sec"`
	DefaultFiveHourThreshold       float64 `json:"default_five_hour"`
	DefaultSevenDayThreshold       float64 `json:"default_seven_day"`
	DefaultSevenDaySonnetThreshold float64 `json:"default_seven_day_sonnet"`
	RestoreNativeOnQuit            bool    `json:"restore_native_on_quit"`
}

func (c *Client) GetSettings(ctx context.Context) (Settings, error) {
	var out Settings
	if err := c.do(ctx, http.MethodGet, "/api/settings", nil, &out); err != nil {
		return Settings{}, err
	}
	return out, nil
}

func (c *Client) SetSettings(ctx context.Context, s Settings) (Settings, error) {
	var out Settings
	if err := c.do(ctx, http.MethodPut, "/api/settings", s, &out); err != nil {
		return Settings{}, err
	}
	return out, nil
}

// AutoSwitch mirrors httpapi.autoSwitchView. Policies the daemon honors are
// "lru" / "lowest" / "rr" — clients that submit anything else get a 400.
type AutoSwitch struct {
	Enabled bool   `json:"enabled"`
	Policy  string `json:"policy"`
}

func (c *Client) GetAutoSwitch(ctx context.Context) (AutoSwitch, error) {
	var out AutoSwitch
	if err := c.do(ctx, http.MethodGet, "/api/auto-switch", nil, &out); err != nil {
		return AutoSwitch{}, err
	}
	return out, nil
}

func (c *Client) SetAutoSwitch(ctx context.Context, v AutoSwitch) (AutoSwitch, error) {
	var out AutoSwitch
	if err := c.do(ctx, http.MethodPost, "/api/auto-switch", v, &out); err != nil {
		return AutoSwitch{}, err
	}
	return out, nil
}

// ResetData wipes the daemon's persistent state and triggers a process exit.
// The next poll will fail with connection refused; the TUI surfaces that as a
// regular error rather than try to recover (the user expects a clean restart).
func (c *Client) ResetData(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/reset", nil, nil)
}

// SetThresholds writes per-account utilization thresholds (0–100). Used by the
// Accounts page's stepper. Values outside the range are clamped server-side.
func (c *Client) SetThresholds(ctx context.Context, id int64, fiveHour, sevenDay, sevenDaySonnet float64) error {
	body := map[string]float64{
		"five_hour":        fiveHour,
		"seven_day":        sevenDay,
		"seven_day_sonnet": sevenDaySonnet,
	}
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/api/accounts/%d/thresholds", id), body, nil)
}

func (c *Client) ListActivity(ctx context.Context, f ActivityFilter) ([]ActivityEvent, error) {
	q := url.Values{}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	if f.SinceID > 0 {
		q.Set("since", strconv.FormatInt(f.SinceID, 10))
	}
	if len(f.Types) > 0 {
		q.Set("type", strings.Join(f.Types, ","))
	}
	if f.Severity != "" {
		q.Set("severity", f.Severity)
	}
	path := "/api/activity"
	if enc := q.Encode(); enc != "" {
		path = path + "?" + enc
	}
	var out struct {
		Events []ActivityEvent `json:"events"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Events, nil
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
