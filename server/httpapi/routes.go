// Package httpapi exposes the localhost HTTP surface used by the Tauri
// front-end and the TUI. Everything binds to 127.0.0.1 only — the server has
// no auth layer because there's no remote attacker model in the single-user
// product.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hoveychen/foxy-switcher/server/activity"
	"github.com/hoveychen/foxy-switcher/server/anthropic"
	"github.com/hoveychen/foxy-switcher/server/authz"
	"github.com/hoveychen/foxy-switcher/server/credinject"
	openai "github.com/hoveychen/foxy-switcher/server/openai"
	"github.com/hoveychen/foxy-switcher/server/refresh"
	"github.com/hoveychen/foxy-switcher/server/selector"
	"github.com/hoveychen/foxy-switcher/server/store"
	vaulthttpserver "github.com/hoveychen/foxy-switcher/server/vault/httpserver"
)

// leaseMine reports whether the caller in ctx is the holder of a lease
// owned by leaseDeviceID:
//   - combined mode (no BearerAuth wrap; ctx carries no device id) →
//     true, because loopback is the only attacker model and the local
//     owner is implicit.
//   - vault mode + cookie session (SessionDeviceID sentinel) → false:
//     web admins are not a device with leases, so badges should render
//     device names for every entry rather than mark some as "yours".
//   - vault mode + Bearer device → equality check.
func leaseMine(ctx context.Context, leaseDeviceID string) bool {
	devID, ok := vaulthttpserver.DeviceFromContext(ctx)
	if !ok {
		return true
	}
	if devID == vaulthttpserver.SessionDeviceID {
		return false
	}
	return devID == leaseDeviceID
}

// Server bundles the dependencies of the HTTP layer. Construct with New.
type Server struct {
	Store     *store.Store
	PKCE      *authz.PKCEStore
	Refresher *refresh.Scheduler
	DataDir   string                  // ~/.foxy-switcher; used to resolve credinject state files
	Port      int                     // populated after net.Listen
	Cred      *credinject.Coordinator // optional; routes that change account state call .Trigger() — safe on nil
	// Bus is the activity hub. Mutating handlers emit account.* events
	// through it so the Activity page reflects user actions immediately.
	// Nil-safe — tests and the legacy --no-activity path leave it unset and
	// the per-call Emit becomes a no-op.
	Bus *activity.Bus
	// StartedAt is the daemon's wall-clock start time, surfaced by
	// /api/about so the Settings page can show "uptime". Set in New so the
	// value matches the process even if main() does work before binding.
	StartedAt time.Time
	// Middleware lets callers (today: main, when running --mode=vault)
	// inject wrappers between cors and the route mux. Used for Bearer
	// auth on the public internet — combined mode leaves it nil so
	// loopback frontend traffic stays open. Wrappers are applied in
	// reverse order, so Middleware[0] ends up outermost (just inside
	// cors) and Middleware[len-1] ends up innermost (just before mux).
	Middleware []func(http.Handler) http.Handler
	// Mode + VaultURL are reflected back through /api/about so the
	// frontend's Settings → Vault card can show what topology this
	// daemon is running. Set by main; safe to leave empty (frontend
	// treats "" the same as "combined").
	Mode         string
	VaultURL     string
	Codex        *openai.Manager
	CodexStorage openai.CredentialStorage
	// codexLogins tracks in-flight Codex device-code logins (see codex_login.go).
	// Zero value is ready; the map is created lazily on first use.
	codexLogins codexLoginStore
}

func New(st *store.Store, pk *authz.PKCEStore, rf *refresh.Scheduler, dataDir string) *Server {
	return &Server{Store: st, PKCE: pk, Refresher: rf, DataDir: dataDir, StartedAt: time.Now()}
}

// Handler returns the *http.ServeMux wired with every route. Bind it on
// 127.0.0.1 only — there is no authentication.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/accounts", s.handleListAccounts)
	mux.HandleFunc("POST /api/accounts/login", s.handleLoginStart)
	mux.HandleFunc("POST /api/accounts/callback", s.handleLoginCallback)
	mux.HandleFunc("POST /api/accounts/import-codex", s.handleImportCodex)
	mux.HandleFunc("POST /api/accounts/codex-login", s.handleCodexLoginStart)
	mux.HandleFunc("POST /api/accounts/codex-login/poll", s.handleCodexLoginPoll)
	mux.HandleFunc("DELETE /api/accounts/{id}", s.handleDeleteAccount)
	mux.HandleFunc("POST /api/accounts/{id}/pause", s.handlePause)
	mux.HandleFunc("POST /api/accounts/{id}/resume", s.handleResume)
	mux.HandleFunc("POST /api/accounts/{id}/refresh", s.handleRefreshNow)
	mux.HandleFunc("POST /api/accounts/{id}/select", s.handleSelect)
	mux.HandleFunc("POST /api/accounts/{id}/thresholds", s.handleSetThresholds)
	mux.HandleFunc("GET /api/accounts/{id}/attribution", s.handleAttribution)
	mux.HandleFunc("GET /api/cred/status", s.handleCredStatus)
	mux.HandleFunc("GET /api/auto-switch", s.handleGetAutoSwitch)
	mux.HandleFunc("POST /api/auto-switch", s.handleSetAutoSwitch)
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings", s.handleSetSettings)
	mux.HandleFunc("POST /api/settings/apply-thresholds", s.handleApplyThresholdDefaults)
	mux.HandleFunc("GET /api/activity", s.handleListActivity)
	mux.HandleFunc("GET /api/activity/stream", s.handleActivityStream)
	mux.HandleFunc("GET /api/dashboard", s.handleGetDashboard)
	mux.HandleFunc("GET /api/about", s.handleGetAbout)
	mux.HandleFunc("POST /api/pair/init", s.handlePairInit)
	mux.HandleFunc("POST /api/pair/poll", s.handlePairPoll)
	mux.HandleFunc("GET /api/devices", s.handleListDevices)
	mux.HandleFunc("POST /api/reset", s.handleResetData)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	var inner http.Handler = mux
	for i := len(s.Middleware) - 1; i >= 0; i-- {
		inner = s.Middleware[i](inner)
	}
	return cors(inner)
}

// cors wraps a handler with permissive CORS. The server only listens on
// 127.0.0.1, so an attacker-on-the-LAN model doesn't apply, and the React
// front-end loads from a different origin (vite dev on :1420 or
// tauri://localhost in release). Wildcard origin is fine because we don't
// use cookies / credentials.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Max-Age", "600")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- /api/accounts ---------------------------------------------------------

type usageWindowView struct {
	Utilization float64 `json:"utilization"` // 0–100 percent
	ResetsAt    string  `json:"resets_at"`   // RFC3339; "" when API didn't return this window
}

// accountLeaseView is the per-account lease metadata surfaced on
// /api/accounts so multi-device deployments can render "in use by
// Device X" badges in one round-trip. Nil when no live lease exists.
//
// Mine is computed server-side from the BearerAuth ctx device_id (or
// implicit "true" in combined mode where loopback is the only caller);
// the frontend never sees other devices' raw IDs through the Mine
// flag's lens, so it can't be spoofed.
type accountLeaseView struct {
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name"`
	Mine       bool   `json:"mine"`
	AcquiredAt int64  `json:"acquired_at"`
	ExpiresAt  int64  `json:"expires_at"`
}

type accountView struct {
	ID               int64  `json:"id"`
	Provider         string `json:"provider"`
	Name             string `json:"name"`
	ExpiresAt        int64  `json:"expires_at"`
	Scopes           string `json:"scopes"`
	SubscriptionType string `json:"subscription_type"`
	// RateLimitTier is the authoritative quota label from
	// /api/oauth/profile.organization.rate_limit_tier. Values:
	// "default_claude_pro" | "default_claude_max_5x" | "default_claude_max_20x"
	// (and "" for legacy rows backfilled on the next UsagePoller tick).
	// Frontend dashboard pool aggregation keys off this — subscription_type
	// can't tell personal Max 5x from Max 20x.
	RateLimitTier    string `json:"rate_limit_tier"`
	OrganizationUUID string `json:"organization_uuid"`
	// AccountUUID is exposed for debug-surface use (e.g. spotting two local
	// rows that map to the same Anthropic user). Empty for older rows that
	// haven't been backfilled yet by the next UsagePoller tick.
	AccountUUID string `json:"account_uuid"`
	Status      string `json:"status"`
	// TokenExpired is a derived flag (ExpiresAt <= now). Persisted state is
	// just ExpiresAt; this exists so UIs don't all need the same clock-math
	// to render the "can't be used" state. The selector treats this as a
	// disqualifier alongside Status==paused / threshold-throttled.
	TokenExpired bool  `json:"token_expired"`
	LastUsedAt   int64 `json:"last_used_at"`
	CreatedAt    int64 `json:"created_at"`
	UpdatedAt    int64 `json:"updated_at"`
	// Profile fields populated at login.
	Email            string `json:"email"`
	FullName         string `json:"full_name"`
	OrganizationName string `json:"organization_name"`
	Plan             string `json:"plan"`
	// Usage snapshot, refreshed by the usage scheduler.
	FiveHour       *usageWindowView `json:"five_hour,omitempty"`
	SevenDay       *usageWindowView `json:"seven_day,omitempty"`
	SevenDaySonnet *usageWindowView `json:"seven_day_sonnet,omitempty"`
	UsageFetchedAt int64            `json:"usage_fetched_at"`
	// Per-account utilization thresholds (0–100). Schema default is 95;
	// 100 means "do not skip on this window".
	FiveHourThreshold       float64 `json:"five_hour_threshold"`
	SevenDayThreshold       float64 `json:"seven_day_threshold"`
	SevenDaySonnetThreshold float64 `json:"seven_day_sonnet_threshold"`
	// Lease is the per-account current-holder metadata for multi-device
	// deployments: device that holds the live lease, its display name,
	// timestamps, and a server-computed Mine flag for "is the caller the
	// holder". Nil when no live lease exists. Populated by
	// handleListAccounts via store.ListAccountsWithLeases.
	Lease *accountLeaseView `json:"lease,omitempty"`
	InUse bool              `json:"in_use"`
	// Tokens are deliberately omitted from the UI surface.
}

// deviceShareView is one device's attributed contribution to an account's
// usage, in utilization points (0–100, same unit as the bars) for the current
// 5h / 7d / 7d-sonnet window. The frontend turns these into a share % by
// dividing each device's points by the per-window total.
type deviceShareView struct {
	DeviceID       string  `json:"device_id"`   // "" for the unattributed bucket
	DeviceName     string  `json:"device_name"` // "" for the unattributed bucket
	FiveHour       float64 `json:"five_hour"`
	SevenDay       float64 `json:"seven_day"`
	SevenDaySonnet float64 `json:"seven_day_sonnet"`
	// LastUsedAt: approximate last time this device actually drove usage (unix
	// millis); 0/omitted when never observed. Real-consumption grounded, unlike
	// the lease's acquired_at. Not emitted for the unattributed bucket.
	LastUsedAt int64 `json:"last_used_at,omitempty"`
}

// attributionView is the response shape for
// GET /api/accounts/{id}/attribution. Devices is sorted by total contribution
// descending; Unattributed (if non-zero) holds points from intervals with no
// lease holder. SampleCount lets the UI flag thin data.
type attributionView struct {
	AccountID    int64             `json:"account_id"`
	Devices      []deviceShareView `json:"devices"`
	Unattributed *deviceShareView  `json:"unattributed,omitempty"`
	SampleCount  int               `json:"sample_count"`
	SampleStart  int64             `json:"sample_start"`
}

func toView(a store.Account) accountView {
	view := accountView{
		ID: a.ID, Provider: a.Provider, Name: a.Name, ExpiresAt: a.ExpiresAt, Scopes: a.Scopes,
		SubscriptionType: a.SubscriptionType,
		RateLimitTier:    a.RateLimitTier,
		OrganizationUUID: a.OrganizationUUID,
		AccountUUID:      a.AccountUUID,
		Status:           a.Status,
		TokenExpired:     a.TokenExpired(time.Now()),
		LastUsedAt:       a.LastUsedAt,
		CreatedAt:        a.CreatedAt, UpdatedAt: a.UpdatedAt,
		Email: a.Email, FullName: a.FullName,
		OrganizationName: a.OrganizationName, Plan: a.Plan,
		UsageFetchedAt:          a.UsageFetchedAt,
		FiveHourThreshold:       a.FiveHourThreshold,
		SevenDayThreshold:       a.SevenDayThreshold,
		SevenDaySonnetThreshold: a.SevenDaySonnetThreshold,
	}
	if a.FiveHourResetsAt != "" {
		view.FiveHour = &usageWindowView{Utilization: a.FiveHourUtil, ResetsAt: a.FiveHourResetsAt}
	}
	if a.SevenDayResetsAt != "" {
		view.SevenDay = &usageWindowView{Utilization: a.SevenDayUtil, ResetsAt: a.SevenDayResetsAt}
	}
	if a.SevenDaySonnetResetsAt != "" {
		view.SevenDaySonnet = &usageWindowView{Utilization: a.SevenDaySonnetUtil, ResetsAt: a.SevenDaySonnetResetsAt}
	}
	return view
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	accs, err := s.Store.ListAccountsWithLeases(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]accountView, len(accs))
	var codexManagedID int64
	if s.Codex != nil {
		codexManagedID = s.Codex.ManagedAccountID(r.Context())
	}
	for i, av := range accs {
		v := toView(av.Account)
		v.InUse = av.Account.Provider == store.ProviderCodex && av.Account.ID == codexManagedID
		if av.Lease != nil {
			v.Lease = &accountLeaseView{
				DeviceID:   av.Lease.DeviceID,
				DeviceName: av.Lease.DeviceName,
				Mine:       leaseMine(r.Context(), av.Lease.DeviceID),
				AcquiredAt: av.Lease.AcquiredAt,
				ExpiresAt:  av.Lease.ExpiresAt,
			}
		}
		out[i] = v
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

func (s *Server) handleImportCodex(w http.ResponseWriter, r *http.Request) {
	if s.Mode == "vault" {
		http.Error(w, "Codex accounts must be imported on the device running Codex CLI", http.StatusConflict)
		return
	}
	storage := s.CodexStorage
	if storage == nil {
		var err error
		storage, err = openai.DefaultCredentialStorage()
		if err != nil {
			http.Error(w, "resolve Codex credential storage: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	a, err := openai.ImportCurrentStorage(storage)
	if err != nil {
		http.Error(w, "import current Codex login: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.Store.Upsert(r.Context(), a); err != nil {
		http.Error(w, "save Codex account: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.Bus.EmitInfo(activity.TypeAccountAdded, a.ID,
		fmt.Sprintf("Imported %s (%s)", a.Name, a.Plan))
	if s.Codex != nil {
		if err := s.Codex.Reconcile(r.Context()); err != nil {
			http.Error(w, "activate Codex account: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"account": toView(*a)})
}

// deviceView is the JSON shape /api/devices returns. Mirrors the columns
// the device-meta migration added to store.Device, minus token_hash —
// the hash never leaves the vault, even to authenticated callers.
type deviceView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Hostname   string `json:"hostname"`
	OS         string `json:"os"`
	OSVersion  string `json:"os_version"`
	Arch       string `json:"arch"`
	Model      string `json:"model"`
	AppVersion string `json:"app_version"`
	ClientType string `json:"client_type"`
	CreatedAt  int64  `json:"created_at"`
	LastSeenAt int64  `json:"last_seen_at"`
}

// handleListDevices powers Settings → "我的设备". In combined mode the
// daemon reads the local store directly; in agent mode the local daemon
// is a thin proxy and the request transparently lands here on the vault
// process, which also runs httpapi.
func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	devs, err := s.Store.ListDevices(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]deviceView, len(devs))
	for i, d := range devs {
		out[i] = deviceView{
			ID:         d.ID,
			Name:       d.Name,
			Hostname:   d.Hostname,
			OS:         d.OS,
			OSVersion:  d.OSVersion,
			Arch:       d.Arch,
			Model:      d.Model,
			AppVersion: d.AppVersion,
			ClientType: d.ClientType,
			CreatedAt:  d.CreatedAt,
			LastSeenAt: d.LastSeenAt,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": out})
}

// handleAttribution returns the per-device quota breakdown for one account:
// how much of each window's current consumption each device drove, estimated
// by replaying usage_history deltas against lease_events held-time. Answers
// "which device used this account up, and in what proportion".
func (s *Server) handleAttribution(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	at, err := s.Store.ComputeAttribution(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := attributionView{
		AccountID:   at.AccountID,
		Devices:     make([]deviceShareView, len(at.Devices)),
		SampleCount: at.SampleCount,
		SampleStart: at.SampleStart,
	}
	for i, d := range at.Devices {
		out.Devices[i] = deviceShareView{
			DeviceID:       d.DeviceID,
			DeviceName:     d.DeviceName,
			FiveHour:       d.FiveHour,
			SevenDay:       d.SevenDay,
			SevenDaySonnet: d.SevenDaySonnet,
			LastUsedAt:     d.LastUsedAt,
		}
	}
	if u := at.Unattributed; u.FiveHour > 0 || u.SevenDay > 0 || u.SevenDaySonnet > 0 {
		out.Unattributed = &deviceShareView{
			FiveHour:       u.FiveHour,
			SevenDay:       u.SevenDay,
			SevenDaySonnet: u.SevenDaySonnet,
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// --- PKCE login ------------------------------------------------------------

func (s *Server) handleLoginStart(w http.ResponseWriter, _ *http.Request) {
	url, state, err := s.PKCE.Start()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"authorize_url": url,
		"state":         state,
	})
}

type callbackReq struct {
	Pasted string `json:"pasted"` // "code#state" copy-pasted from platform.claude.com
	State  string `json:"state"`  // the state returned from /login
}

func (s *Server) handleLoginCallback(w http.ResponseWriter, r *http.Request) {
	var req callbackReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	verifier, ok := s.PKCE.Consume(req.State)
	if !ok {
		http.Error(w, "unknown or expired state", http.StatusBadRequest)
		return
	}
	code, pastedState, err := authz.SplitPastedCode(req.Pasted)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if pastedState != req.State {
		http.Error(w, "state mismatch — paste corresponds to a different login attempt", http.StatusBadRequest)
		return
	}

	tr, err := authz.ExchangeCode(r.Context(), verifier, code, pastedState)
	if err != nil {
		http.Error(w, "token exchange: "+err.Error(), http.StatusBadGateway)
		return
	}
	expiresAt := tr.ExpiresAtMillis()
	if expiresAt == 0 {
		expiresAt = time.Now().Add(8 * time.Hour).UnixMilli()
	}

	// Populate owner / plan immediately. We treat profile fetch as part of
	// the login: a token that can't read its own profile is unusable, so
	// failing here is preferable to silently storing an account that will
	// show "—" forever in the UI.
	prof, err := anthropic.FetchProfile(r.Context(), tr.AccessToken)
	if err != nil {
		http.Error(w, "fetch profile: "+err.Error(), http.StatusBadGateway)
		return
	}

	a := store.Account{
		Name:             deriveAccountName(prof),
		AccessToken:      tr.AccessToken,
		RefreshToken:     tr.RefreshToken,
		ExpiresAt:        expiresAt,
		Scopes:           tr.Scope,
		AccountUUID:      prof.AccountUUID,
		Email:            prof.Email,
		FullName:         prof.FullName,
		OrganizationName: prof.OrganizationName,
		Plan:             prof.Plan,
		SubscriptionType: prof.SubscriptionType,
		RateLimitTier:    prof.RateLimitTier,
		// OrganizationUUID is currently not surfaced by /api/oauth/profile;
		// we keep the column for future use when Anthropic exposes it.
	}
	if err := s.Store.Upsert(r.Context(), &a); err != nil {
		http.Error(w, "save account: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.Bus.EmitInfo(activity.TypeAccountAdded, a.ID,
		fmt.Sprintf("Added %s (%s)", a.Name, a.Plan))
	if a.Provider == store.ProviderCodex && s.Codex != nil {
		_ = s.Codex.Reconcile(r.Context())
	} else {
		s.Cred.Trigger()
	}

	// Best-effort initial usage pull so the new card lights up immediately
	// instead of waiting for the next 5-minute tick. Failures are logged
	// only — the next tick will retry.
	if usage, err := anthropic.FetchUsage(r.Context(), a.AccessToken); err == nil {
		_ = applyUsage(r.Context(), s.Store, a.ID, usage)
		// Re-read so the response includes the freshly-stored usage.
		if updated, err := s.Store.Get(r.Context(), a.ID); err == nil {
			a = *updated
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"account": toView(a)})
}

// deriveAccountName picks a human-recognisable label for a freshly-added
// account from its profile. We previously asked the user for an alias, but
// the profile already exposes email / full name, so the prompt was busywork.
// Email is preferred because it's unique per account; full_name and a
// timestamp fallback handle the rare case where the profile is sparse.
func deriveAccountName(prof *anthropic.Profile) string {
	if e := strings.TrimSpace(prof.Email); e != "" {
		return e
	}
	if n := strings.TrimSpace(prof.FullName); n != "" {
		return n
	}
	return fmt.Sprintf("Account %d", time.Now().Unix())
}

// applyUsage writes a Usage snapshot to the store. Nil windows become zeroed
// columns. Centralised here so the login path, the periodic poller, and the
// "Refresh now" handler all agree on the encoding.
func applyUsage(ctx context.Context, st *store.Store, id int64, u *anthropic.Usage) error {
	var fhU, sdU, ssU float64
	var fhR, sdR, ssR string
	if u.FiveHour != nil {
		fhU, fhR = u.FiveHour.Utilization, u.FiveHour.ResetsAt
	}
	if u.SevenDay != nil {
		sdU, sdR = u.SevenDay.Utilization, u.SevenDay.ResetsAt
	}
	if u.SevenDaySonnet != nil {
		ssU, ssR = u.SevenDaySonnet.Utilization, u.SevenDaySonnet.ResetsAt
	}
	return st.SetUsage(ctx, id, fhU, fhR, sdU, sdR, ssU, ssR)
}

// earliestThrottledReset returns the soonest resets_at (as unix millis) among
// the windows where this account's utilization has reached the matching
// threshold. The bool is false when no window is currently throttling — the
// caller treats that as "this account isn't cooling".
//
// resets_at strings that fail to parse or are empty are skipped: the API is
// authoritative on RFC3339 formatting, but we'd rather omit a problematic
// window from the KPI than return a bogus 0.
func earliestThrottledReset(a store.Account, now time.Time) (int64, bool) {
	candidates := []struct {
		util, threshold float64
		resetsAt        string
	}{
		{a.FiveHourUtil, a.FiveHourThreshold, a.FiveHourResetsAt},
		{a.SevenDayUtil, a.SevenDayThreshold, a.SevenDayResetsAt},
		{a.SevenDaySonnetUtil, a.SevenDaySonnetThreshold, a.SevenDaySonnetResetsAt},
	}
	var best int64
	found := false
	for _, c := range candidates {
		if c.resetsAt == "" || c.util < c.threshold {
			continue
		}
		t, err := time.Parse(time.RFC3339, c.resetsAt)
		if err != nil {
			continue
		}
		ms := t.UnixMilli()
		if ms <= now.UnixMilli() {
			continue
		}
		if !found || ms < best {
			best = ms
			found = true
		}
	}
	return best, found
}

// --- mutations -------------------------------------------------------------

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Snapshot the name before deletion so the activity row carries
	// something more useful than the raw ID — this is the user's only
	// post-hoc record of which account this was.
	name := fmt.Sprintf("#%d", id)
	provider := store.ProviderClaude
	if a, err := s.Store.Get(r.Context(), id); err == nil {
		name = a.Name
		provider = a.Provider
	}
	if err := s.Store.Delete(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Bus.EmitWarn(activity.TypeAccountDeleted, id,
		fmt.Sprintf("Deleted %s", name))
	if err := s.triggerProvider(r.Context(), provider); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	s.setStatus(w, r, "paused")
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	s.setStatus(w, r, "active")
}

func (s *Server) setStatus(w http.ResponseWriter, r *http.Request, status string) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.Store.SetStatus(r.Context(), id, status); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	name := fmt.Sprintf("#%d", id)
	provider := store.ProviderClaude
	if a, err := s.Store.Get(r.Context(), id); err == nil {
		name = a.Name
		provider = a.Provider
	}
	if status == "paused" {
		s.Bus.EmitInfo(activity.TypeAccountPaused, id,
			fmt.Sprintf("Paused %s", name))
	} else {
		s.Bus.EmitInfo(activity.TypeAccountResumed, id,
			fmt.Sprintf("Resumed %s", name))
	}
	if err := s.triggerProvider(r.Context(), provider); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRefreshNow(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.Refresher.RefreshOne(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	a, err := s.Store.Get(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.triggerProvider(r.Context(), a.Provider); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Pull fresh usage with the just-rotated token. We don't surface this
	// failure: the user's primary intent (rotate token) succeeded, and
	// usage will come in on the next 5-minute tick if Anthropic is
	// transient-erroring right now.
	if a.Provider == store.ProviderCodex {
		if u, usageErr := openai.FetchUsage(r.Context(), a.AccessToken, a.AccountUUID); usageErr == nil {
			_ = applyCodexUsage(r.Context(), s.Store, a.ID, u)
		}
	} else if u, usageErr := anthropic.FetchUsage(r.Context(), a.AccessToken); usageErr == nil {
		_ = applyUsage(r.Context(), s.Store, a.ID, u)
	}
	if updated, getErr := s.Store.Get(r.Context(), id); getErr == nil {
		a = updated
	}
	writeJSON(w, http.StatusOK, map[string]any{"account": toView(*a)})
}

func applyCodexUsage(ctx context.Context, st *store.Store, id int64, u *openai.Usage) error {
	var primaryUtil, secondaryUtil float64
	var primaryReset, secondaryReset string
	if u.Primary != nil {
		primaryUtil = u.Primary.UsedPercent
		primaryReset = u.Primary.ResetAt.Format(time.RFC3339)
	}
	if u.Secondary != nil {
		secondaryUtil = u.Secondary.UsedPercent
		secondaryReset = u.Secondary.ResetAt.Format(time.RFC3339)
	}
	return st.SetUsage(ctx, id, primaryUtil, primaryReset, secondaryUtil, secondaryReset, 0, "")
}

// handleSelect promotes one account to the front of the LRU queue so the
// next credinject reconcile picks it. One-shot: subsequent rotations follow
// normal LRU. Rejects accounts that the selector would skip anyway —
// paused, token-expired, or threshold-throttled — with 409, so the UI can
// surface a clear reason.
func (s *Server) handleSelect(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a, err := s.Store.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !selector.IsEligible(*a, time.Now()) {
		http.Error(w, "account is not eligible", http.StatusConflict)
		return
	}
	if err := s.Store.MarkForNextPick(r.Context(), id, s.pinDeviceID(r.Context())); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.triggerProvider(r.Context(), a.Provider); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) triggerProvider(ctx context.Context, provider string) error {
	if provider == store.ProviderCodex && s.Codex != nil {
		return s.Codex.Reconcile(ctx)
	}
	s.Cred.Trigger()
	return nil
}

// pinDeviceID resolves which device a /select pin should be scoped to:
//   - vault mode + Bearer device → that device.
//   - combined mode (no device in ctx) → the local coordinator's device id,
//     so the pin stays invisible to any paired agents sharing the pool.
//   - web-admin cookie session (or no coordinator) → "" — the legacy global
//     pin every device races for, since the admin named no target device.
func (s *Server) pinDeviceID(ctx context.Context) string {
	if devID, ok := vaulthttpserver.DeviceFromContext(ctx); ok {
		if devID == vaulthttpserver.SessionDeviceID {
			return ""
		}
		return devID
	}
	if s.Cred != nil {
		return s.Cred.DeviceID()
	}
	return ""
}

// thresholdsReq is the body shape for POST /api/accounts/{id}/thresholds.
// Each field is a 0–100 percent. Out-of-range values are clamped by the
// store layer rather than rejected, so the UI's "drag the marker" surface
// can submit raw pixel-derived values without re-implementing the bounds.
type thresholdsReq struct {
	FiveHour       float64 `json:"five_hour"`
	SevenDay       float64 `json:"seven_day"`
	SevenDaySonnet float64 `json:"seven_day_sonnet"`
}

func (s *Server) handleSetThresholds(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req thresholdsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.Store.SetThresholds(r.Context(), id, req.FiveHour, req.SevenDay, req.SevenDaySonnet); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Cred.Trigger()
	w.WriteHeader(http.StatusNoContent)
}

// --- auto-switch -----------------------------------------------------------

// autoSwitchView is the wire shape for GET/POST /api/auto-switch. The toggle
// gates whether the credinject coordinator may rotate accounts; policy is
// reserved for future strategies (currently only "lru" is honoured) so the UI
// can persist the user's preference even before alternative pickers ship.
type autoSwitchView struct {
	Enabled bool   `json:"enabled"`
	Policy  string `json:"policy"`
}

var allowedPolicies = map[string]struct{}{
	"lru":    {},
	"lowest": {},
	"rr":     {},
}

func (s *Server) handleGetAutoSwitch(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.GetAutoSwitch(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, autoSwitchView{Enabled: v.Enabled, Policy: v.Policy})
}

func (s *Server) handleSetAutoSwitch(w http.ResponseWriter, r *http.Request) {
	var req autoSwitchView
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Policy == "" {
		req.Policy = store.DefaultAutoSwitch.Policy
	}
	if _, ok := allowedPolicies[req.Policy]; !ok {
		http.Error(w, "invalid policy "+strconv.Quote(req.Policy), http.StatusBadRequest)
		return
	}
	if err := s.Store.SetAutoSwitch(r.Context(), store.AutoSwitch{Enabled: req.Enabled, Policy: req.Policy}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Cred.Trigger()
	writeJSON(w, http.StatusOK, req)
}

// --- activity feed ---------------------------------------------------------

// handleListActivity backs the GET /api/activity endpoint that powers the
// Activity page and the Dashboard's Recent Activity card. Query params:
//
//	limit=N     — cap the number of events (default 200, hard max RingCapacity)
//	since=ID    — return only events with id > ID (incremental polling)
//	type=a,b    — comma-separated whitelist; supports "error.*" wildcard
//	severity=S  — restrict to one severity (info|warn|error)
//
// Always returns 200 with `{"events": [...]}`, even when empty, so the
// frontend can poll without special-casing the cold-start window.
func (s *Server) handleListActivity(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := activity.Filter{}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			f.Limit = n
		}
	}
	if v := q.Get("since"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			f.SinceID = n
		}
	}
	if v := q.Get("type"); v != "" {
		for _, t := range strings.Split(v, ",") {
			if t = strings.TrimSpace(t); t != "" {
				f.Types = append(f.Types, t)
			}
		}
	}
	if v := q.Get("severity"); v != "" {
		f.Severity = activity.Severity(v)
	}
	events := s.Bus.List(f)
	if events == nil {
		events = []activity.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// handleActivityStream is the SSE pendant of /api/activity. The frontend
// opens an EventSource on this URL and gets a push for every new event the
// bus publishes; on disconnect (browser closing the tab, daemon restart,
// network) it can fall back to polling /api/activity until the stream
// reattaches.
//
// Wire format: each event is a single SSE block —
//
//	id: 42\n
//	event: activity\n
//	data: {…json…}\n\n
//
// "id:" lets the browser send Last-Event-ID on reconnect; we honor it by
// flushing every event with id > Last-Event-ID from the ring before
// switching to live subscription. A 25s heartbeat keeps proxies / OS-level
// idle timers from reaping the connection.
func (s *Server) handleActivityStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // belt-and-suspenders for proxies
	w.WriteHeader(http.StatusOK)

	var lastID int64
	if h := r.Header.Get("Last-Event-ID"); h != "" {
		if n, err := strconv.ParseInt(h, 10, 64); err == nil {
			lastID = n
		}
	}

	// Replay anything the client missed since its Last-Event-ID. List()
	// returns newest-first; reverse-iterate so the wire ordering is
	// chronological — that matches the frontend's append-only render.
	if lastID > 0 {
		backlog := s.Bus.List(activity.Filter{SinceID: lastID, Limit: 200})
		for i := len(backlog) - 1; i >= 0; i-- {
			if !writeSSEEvent(w, backlog[i]) {
				return
			}
		}
		flusher.Flush()
	}

	// Subscribe BEFORE writing the initial "ready" comment to avoid a race
	// where an event lands between replay and subscribe.
	ch := make(chan activity.Event, 64)
	subID := s.Bus.Subscribe(ch)
	defer s.Bus.Unsubscribe(subID)

	// Initial comment so browsers / curl know the stream is live and so
	// reverse-proxies flush their buffers immediately.
	if _, err := fmt.Fprintf(w, ": ready\n\n"); err != nil {
		return
	}
	flusher.Flush()

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if !writeSSEEvent(w, ev) {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprintf(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeSSEEvent encodes one activity.Event into the wire format. Returns
// false if the underlying connection failed and the caller should give up.
func writeSSEEvent(w http.ResponseWriter, ev activity.Event) bool {
	payload, err := json.Marshal(ev)
	if err != nil {
		return true // skip malformed event, keep stream alive
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: activity\ndata: %s\n\n", ev.ID, payload)
	return err == nil
}

// --- dashboard ------------------------------------------------------------

// InUseEntry is one row in DashboardKPIs.InUse: an active lease joined
// with the holding device's display name, plus a server-computed Mine
// flag for "is this lease held by the caller". Vault Web UI renders one
// badge per entry; the agent's desktop UI uses Mine to highlight its
// own entry distinctly from other devices'.
type InUseEntry struct {
	AccountID  int64  `json:"account_id"`
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name"`
	Mine       bool   `json:"mine"`
	ExpiresAt  int64  `json:"expires_at"`
}

// DashboardKPIs are the pool-level numbers shown above the trend chart.
type DashboardKPIs struct {
	PoolSize    int `json:"pool_size"`
	ActiveCount int `json:"active_count"`
	// InUse is every active lease (one per account; the leases table
	// uniqueness index enforces 1:1). Replaces the legacy single
	// InUseAccountID — multi-device deployments need to surface every
	// in-flight lease, not just the longest-held one. Empty list when
	// no agent currently holds any account.
	InUse []InUseEntry `json:"in_use"`
	// CoolingCount is how many active accounts are currently throttled by
	// the per-window utilization threshold (selector.exceedsThreshold).
	CoolingCount int `json:"cooling_count"`
	// NextResetAt is the soonest reset across the windows that are currently
	// throttling an active account, in unix millis. 0 means no account is
	// throttled (or none of the throttled windows reported a resets_at yet).
	NextResetAt     int64 `json:"next_reset_at"`
	PeakUtilPercent int   `json:"peak_util_percent"` // max across windows, all accounts
}

// DashboardTrendBucket is one hour of usage history aggregated across the
// pool. Each value is the max utilization observed in the bucket (so the
// chart shows the worst-case headroom, which is what matters for
// "is the pool about to throttle").
//
// The *_used / *_capacity / *_pct fields mirror the dashboard's pool-wide
// quota cards: each account contributes weighted by its subscription tier
// (planWeight). used and capacity are in Pro-equivalents (Pro=1, Max5x=5,
// TeamPremium=20 for 5h / 10 for 7d). For each bucket, used is the sum
// over accounts of (peak_utilization_in_bucket / 100) * weight; capacity
// is the sum of weights across all currently-tracked accounts (constant
// across buckets, but emitted per-bucket to keep the chart self-contained).
type DashboardTrendBucket struct {
	TS               int64   `json:"ts"` // unix millis at bucket start (top of hour)
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

// planWeight returns this account's Pro-equivalent (5h, 7d) weights.
// Mirrors src/api.ts:planWeight — keep in sync.
//
// Primary key: rate_limit_tier from /api/oauth/profile.organization.
// rate_limit_tier — the only field that distinguishes personal Max 5x
// from Max 20x, and that reveals whether a Team Premium org actually
// has Max-parity quota (one observed Team Premium org reports
// "default_claude_max_5x", not 20x — so the old per-subscription_type
// weight of 20/10 was over-counting Team Premium pools).
//
// Fallback: subscription_type for legacy rows whose tier hasn't been
// backfilled by the next UsagePoller tick (RateLimitTier=="").
func planWeight(rateTier, sub string) (w5h, w7d float64) {
	switch rateTier {
	case "default_claude_pro":
		return 1, 1
	case "default_claude_max_5x":
		return 5, 5
	case "default_claude_max_20x":
		return 20, 10
	}
	// Legacy fallback for rows whose tier hasn't been backfilled.
	// Conservatively assume Max=5x and Team Premium=5x (the observed
	// value at our example org), erring on the side of under-stating
	// rather than over-stating the pool.
	switch sub {
	case "pro":
		return 1, 1
	case "max":
		return 5, 5
	case "team":
		return 5, 5
	case "team_premium":
		return 5, 5
	default:
		return 0, 0
	}
}

// DashboardResponse is the wire shape returned by GET /api/dashboard.
type DashboardResponse struct {
	KPIs  DashboardKPIs          `json:"kpis"`
	Trend []DashboardTrendBucket `json:"trend"`
}

// handleGetDashboard backs the Dashboard page. Returns pool-level KPIs and a
// 24h hour-bucketed utilization trend derived from usage_history rows.
//
// Aggregation rule per bucket: max across all accounts and all sample rows
// that fall in the hour. We pick max over avg because the user's mental model
// is "is anything close to throttling" — averaging would mask a single hot
// account.
func (s *Server) handleGetDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accs, err := s.Store.List(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	now := time.Now()
	kpis := DashboardKPIs{PoolSize: len(accs)}
	// Surface every active lease as a separate kpis.in_use[] entry so
	// multi-device deployments can render one badge per holder. Replaces
	// the legacy single InUseAccountID + FirstActiveLease fallback —
	// agents and the vault Web UI now read the same authoritative list.
	leases, err := s.Store.ListActiveLeases(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	kpis.InUse = make([]InUseEntry, 0, len(leases))
	for _, l := range leases {
		kpis.InUse = append(kpis.InUse, InUseEntry{
			AccountID:  l.AccountID,
			DeviceID:   l.DeviceID,
			DeviceName: l.DeviceName,
			Mine:       leaseMine(ctx, l.DeviceID),
			ExpiresAt:  l.ExpiresAt,
		})
	}
	var nextReset int64
	var peak float64
	for _, a := range accs {
		if a.Status == "active" {
			kpis.ActiveCount++
		}
		for _, u := range []float64{a.FiveHourUtil, a.SevenDayUtil, a.SevenDaySonnetUtil} {
			if u > peak {
				peak = u
			}
		}
		if a.Status != "active" {
			continue
		}
		// Throttled-window contributions to NextResetAt: pick the soonest
		// reset across all (account, window) pairs that are currently above
		// threshold. We require the matching resets_at to be non-empty —
		// freshly added accounts haven't been polled yet and shouldn't show
		// up as "cooling forever".
		if soonest, ok := earliestThrottledReset(a, now); ok {
			kpis.CoolingCount++
			if nextReset == 0 || soonest < nextReset {
				nextReset = soonest
			}
		}
	}
	kpis.NextResetAt = nextReset
	kpis.PeakUtilPercent = int(peak + 0.5) // round to nearest

	since := now.Add(-24 * time.Hour).UnixMilli()
	rows, err := s.Store.UsageHistorySince(ctx, since)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Bucket into 24 hourly slots aligned to the top of the current hour.
	const buckets = 24
	hourMs := int64(time.Hour / time.Millisecond)
	bucketStart := now.Truncate(time.Hour).UnixMilli() - int64(buckets-1)*hourMs
	type agg struct{ five, seven, sonnet float64 }
	bucketed := make([]agg, buckets)
	// peakPerAccount tracks each account's peak utilization within each
	// bucket, so we can compute the weighted sum correctly. Sample rows
	// arrive every 5 min; using max-per-account avoids double-counting
	// when an account had multiple polls in the same hour.
	type pa struct{ five, seven float64 }
	peakPerAccount := make(map[int]map[int64]pa) // bucketIdx -> accountID -> peaks
	for _, row := range rows {
		idx := int((row.Timestamp - bucketStart) / hourMs)
		if idx < 0 || idx >= buckets {
			continue
		}
		if row.FiveHourUtil > bucketed[idx].five {
			bucketed[idx].five = row.FiveHourUtil
		}
		if row.SevenDayUtil > bucketed[idx].seven {
			bucketed[idx].seven = row.SevenDayUtil
		}
		if row.SevenDaySonnetUtil > bucketed[idx].sonnet {
			bucketed[idx].sonnet = row.SevenDaySonnetUtil
		}
		m, ok := peakPerAccount[idx]
		if !ok {
			m = make(map[int64]pa)
			peakPerAccount[idx] = m
		}
		cur := m[row.AccountID]
		if row.FiveHourUtil > cur.five {
			cur.five = row.FiveHourUtil
		}
		if row.SevenDayUtil > cur.seven {
			cur.seven = row.SevenDayUtil
		}
		m[row.AccountID] = cur
	}

	// accountWeights maps each currently-tracked account to its (5h, 7d)
	// Pro-equivalent weights. Capacity is the constant sum across accounts
	// — emitted per-bucket so the chart is self-contained.
	accountWeights := make(map[int64]struct{ w5h, w7d float64 }, len(accs))
	var cap5h, cap7d float64
	for _, a := range accs {
		w5h, w7d := planWeight(a.RateLimitTier, a.SubscriptionType)
		accountWeights[a.ID] = struct{ w5h, w7d float64 }{w5h, w7d}
		cap5h += w5h
		cap7d += w7d
	}

	trend := make([]DashboardTrendBucket, buckets)
	for i := 0; i < buckets; i++ {
		var used5h, used7d float64
		for accID, p := range peakPerAccount[i] {
			w := accountWeights[accID]
			used5h += (p.five / 100) * w.w5h
			used7d += (p.seven / 100) * w.w7d
		}
		var pct5h, pct7d float64
		if cap5h > 0 {
			pct5h = used5h / cap5h * 100
		}
		if cap7d > 0 {
			pct7d = used7d / cap7d * 100
		}
		trend[i] = DashboardTrendBucket{
			TS:               bucketStart + int64(i)*hourMs,
			FiveHour:         bucketed[i].five,
			SevenDay:         bucketed[i].seven,
			SevenDaySonnet:   bucketed[i].sonnet,
			FiveHourUsed:     used5h,
			FiveHourCapacity: cap5h,
			FiveHourPct:      pct5h,
			SevenDayUsed:     used7d,
			SevenDayCapacity: cap7d,
			SevenDayPct:      pct7d,
		}
	}
	writeJSON(w, http.StatusOK, DashboardResponse{KPIs: kpis, Trend: trend})
}

// --- settings -------------------------------------------------------------

// handleGetSettings returns the persisted user-prefs blob, with defaults
// substituted for any missing/clamped fields. Always 200 — a fresh install
// returns DefaultSettings rather than 404 so the frontend can hydrate
// uniformly on first launch.
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.GetSettings(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// handleSetSettings persists the supplied prefs. The store clamps numeric
// fields and substitutes defaults for empty strings, so the frontend can
// submit raw values; the response echoes the canonical form so the UI can
// snap to it without re-fetching.
//
// Note: theme / sidebar are frontend-only (the server stores them verbatim);
// usage_poll_interval_sec applies on next daemon start (the running poller
// keeps its current cadence to avoid racing in-flight ticks);
// restore_native_on_quit is read at shutdown by the credinject coordinator.
func (s *Server) handleSetSettings(w http.ResponseWriter, r *http.Request) {
	var req store.Settings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}
	v, err := s.Store.SetSettings(r.Context(), req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Push the restore flag to the coordinator so the shutdown path picks
	// up the new value without a daemon restart.
	if s.Cred != nil {
		s.Cred.SetRestoreOnQuit(v.RestoreNativeOnQuit)
	}
	writeJSON(w, http.StatusOK, v)
}

// handleApplyThresholdDefaults overwrites every account's per-window
// thresholds with the currently-persisted pool-wide defaults. This is an
// indiscriminate bulk overwrite — manually-tuned accounts are reset too — so
// the frontend gates it behind an explicit "apply to all accounts" action.
// It reads the saved settings (rather than a request body) so it always
// applies exactly what the operator sees in the settings form after saving.
func (s *Server) handleApplyThresholdDefaults(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.GetSettings(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	n, err := s.Store.ApplyThresholdsToAll(r.Context(),
		v.DefaultFiveHourThreshold, v.DefaultSevenDayThreshold, v.DefaultSevenDaySonnetThreshold)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.Cred != nil {
		s.Cred.Trigger()
	}
	writeJSON(w, http.StatusOK, map[string]int64{"updated": n})
}

// --- credinject status ----------------------------------------------------

// handleCredStatus reports the credinject coordinator's current state for the
// frontend / TUI status surface. Returns zero values when no Coordinator is
// wired (e.g. --no-cred-inject mode).
//
// Vault mode has no local Coordinator (s.Cred is nil), but App.tsx still drives
// managedAccountId off this endpoint — the dashboard hero, AccountsPage's
// "in use" badge, and the isInUse selector all key off it. Fall back to
// store.FirstActiveLease so a remote agent's renewing lease surfaces here,
// matching the equivalent fallback in handleGetDashboard.
func (s *Server) handleCredStatus(w http.ResponseWriter, r *http.Request) {
	status := s.Cred.Status()
	if s.Cred == nil {
		if id, ok, err := s.Store.FirstActiveLease(r.Context()); err == nil && ok {
			status.ManagedAccountID = id
		}
	}
	writeJSON(w, http.StatusOK, status)
}

// --- helpers ---------------------------------------------------------------

func pathID(r *http.Request) (int64, error) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid id %q", raw)
	}
	return id, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Compile-time guard that we used context. (Avoids "imported and not used"
// when handlers happen to not need it.)
var _ = context.Background
