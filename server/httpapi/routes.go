// Package httpapi exposes the localhost HTTP surface used by the Tauri
// front-end and the TUI. Everything binds to 127.0.0.1 only — the server has
// no auth layer because there's no remote attacker model in the single-user
// product.
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hoveychen/foxy-switcher/server/anthropic"
	"github.com/hoveychen/foxy-switcher/server/authz"
	"github.com/hoveychen/foxy-switcher/server/credinject"
	"github.com/hoveychen/foxy-switcher/server/refresh"
	"github.com/hoveychen/foxy-switcher/server/store"
)

// Server bundles the dependencies of the HTTP layer. Construct with New.
type Server struct {
	Store     *store.Store
	PKCE      *authz.PKCEStore
	Refresher *refresh.Scheduler
	DataDir   string                  // ~/.foxy-switcher; used to resolve credinject state files
	Port      int                     // populated after net.Listen
	Cred      *credinject.Coordinator // optional; routes that change account state call .Trigger() — safe on nil
}

func New(st *store.Store, pk *authz.PKCEStore, rf *refresh.Scheduler, dataDir string) *Server {
	return &Server{Store: st, PKCE: pk, Refresher: rf, DataDir: dataDir}
}

// Handler returns the *http.ServeMux wired with every route. Bind it on
// 127.0.0.1 only — there is no authentication.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/accounts", s.handleListAccounts)
	mux.HandleFunc("POST /api/accounts/login", s.handleLoginStart)
	mux.HandleFunc("POST /api/accounts/callback", s.handleLoginCallback)
	mux.HandleFunc("DELETE /api/accounts/{id}", s.handleDeleteAccount)
	mux.HandleFunc("POST /api/accounts/{id}/disable", s.handleDisable)
	mux.HandleFunc("POST /api/accounts/{id}/enable", s.handleEnable)
	mux.HandleFunc("POST /api/accounts/{id}/cooldown", s.handleCooldown)
	mux.HandleFunc("POST /api/accounts/{id}/refresh", s.handleRefreshNow)
	mux.HandleFunc("GET /api/cred/status", s.handleCredStatus)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	return cors(mux)
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

type accountView struct {
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	ExpiresAt        int64  `json:"expires_at"`
	Scopes           string `json:"scopes"`
	SubscriptionType string `json:"subscription_type"`
	OrganizationUUID string `json:"organization_uuid"`
	Status           string `json:"status"`
	CooldownUntil    int64  `json:"cooldown_until"`
	LastUsedAt       int64  `json:"last_used_at"`
	Last429At        int64  `json:"last_429_at"`
	CreatedAt        int64  `json:"created_at"`
	UpdatedAt        int64  `json:"updated_at"`
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
	// Tokens are deliberately omitted from the UI surface.
}

func toView(a store.Account) accountView {
	view := accountView{
		ID: a.ID, Name: a.Name, ExpiresAt: a.ExpiresAt, Scopes: a.Scopes,
		SubscriptionType: a.SubscriptionType,
		OrganizationUUID: a.OrganizationUUID, Status: a.Status,
		CooldownUntil: a.CooldownUntil, LastUsedAt: a.LastUsedAt,
		Last429At: a.Last429At, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
		Email: a.Email, FullName: a.FullName,
		OrganizationName: a.OrganizationName, Plan: a.Plan,
		UsageFetchedAt: a.UsageFetchedAt,
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
	accs, err := s.Store.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]accountView, len(accs))
	for i, a := range accs {
		out[i] = toView(a)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
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
		Email:            prof.Email,
		FullName:         prof.FullName,
		OrganizationName: prof.OrganizationName,
		Plan:             prof.Plan,
		SubscriptionType: prof.SubscriptionType,
		// OrganizationUUID is currently not surfaced by /api/oauth/profile;
		// we keep the column for future use when Anthropic exposes it.
	}
	if err := s.Store.Upsert(r.Context(), &a); err != nil {
		http.Error(w, "save account: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.Cred.Trigger()

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

// --- mutations -------------------------------------------------------------

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.Store.Delete(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Cred.Trigger()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDisable(w http.ResponseWriter, r *http.Request) {
	s.setStatus(w, r, "disabled")
}

func (s *Server) handleEnable(w http.ResponseWriter, r *http.Request) {
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
	s.Cred.Trigger()
	w.WriteHeader(http.StatusNoContent)
}

type cooldownReq struct {
	UntilMillis int64 `json:"until_millis"` // absolute unix-millis; 0 clears
	DurationMS  int64 `json:"duration_ms"`  // alternative — relative offset
}

func (s *Server) handleCooldown(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req cooldownReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var until time.Time
	switch {
	case req.UntilMillis > 0:
		until = time.UnixMilli(req.UntilMillis)
	case req.DurationMS > 0:
		until = time.Now().Add(time.Duration(req.DurationMS) * time.Millisecond)
	}
	if err := s.Store.SetCooldown(r.Context(), id, until); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Cred.Trigger()
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
	s.Cred.Trigger()
	a, err := s.Store.Get(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Pull fresh usage with the just-rotated token. We don't surface this
	// failure: the user's primary intent (rotate token) succeeded, and
	// usage will come in on the next 5-minute tick if Anthropic is
	// transient-erroring right now.
	if u, err := anthropic.FetchUsage(r.Context(), a.AccessToken); err == nil {
		_ = applyUsage(r.Context(), s.Store, a.ID, u)
		if updated, err := s.Store.Get(r.Context(), id); err == nil {
			a = updated
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"account": toView(*a)})
}

// --- credinject status ----------------------------------------------------

// handleCredStatus reports the credinject coordinator's current state for the
// frontend / TUI status surface. Returns zero values when no Coordinator is
// wired (e.g. --no-cred-inject mode).
func (s *Server) handleCredStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Cred.Status())
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
