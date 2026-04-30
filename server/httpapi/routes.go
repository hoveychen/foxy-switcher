// Package httpapi exposes the localhost HTTP surface used by the apiKeyHelper
// shell script, the Tauri front-end, and the curl-based install/uninstall
// flow. Everything binds to 127.0.0.1 only — the server has no auth layer
// because there's no remote attacker model in the single-user product.
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

	"github.com/hoveychen/foxy-switcher/server/assets"
	"github.com/hoveychen/foxy-switcher/server/authz"
	"github.com/hoveychen/foxy-switcher/server/refresh"
	"github.com/hoveychen/foxy-switcher/server/selector"
	"github.com/hoveychen/foxy-switcher/server/store"
)

// JustInTimeRefreshThreshold is how close to expiry an access_token has to be
// before /api/token blocks on a synchronous refresh. The background scheduler
// already keeps tokens fresh proactively, but this guards the path where the
// scheduler hasn't had a chance to run yet (e.g. server just started).
const JustInTimeRefreshThreshold = 5 * time.Minute

// Server bundles the dependencies of the HTTP layer. Construct with New.
type Server struct {
	Store     *store.Store
	PKCE      *authz.PKCEStore
	Refresher *refresh.Scheduler
	Port      int // populated after net.Listen, used by /install.sh
}

func New(st *store.Store, pk *authz.PKCEStore, rf *refresh.Scheduler) *Server {
	return &Server{Store: st, PKCE: pk, Refresher: rf}
}

// Handler returns the *http.ServeMux wired with every route. Bind it on
// 127.0.0.1 only — there is no authentication.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/token", s.handleGetToken)
	mux.HandleFunc("GET /api/accounts", s.handleListAccounts)
	mux.HandleFunc("POST /api/accounts/login", s.handleLoginStart)
	mux.HandleFunc("POST /api/accounts/callback", s.handleLoginCallback)
	mux.HandleFunc("DELETE /api/accounts/{id}", s.handleDeleteAccount)
	mux.HandleFunc("POST /api/accounts/{id}/disable", s.handleDisable)
	mux.HandleFunc("POST /api/accounts/{id}/enable", s.handleEnable)
	mux.HandleFunc("POST /api/accounts/{id}/cooldown", s.handleCooldown)
	mux.HandleFunc("POST /api/accounts/{id}/refresh", s.handleRefreshNow)
	mux.HandleFunc("GET /install.sh", s.handleInstallScript)
	mux.HandleFunc("GET /uninstall.sh", s.handleUninstallScript)
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

// --- /api/token ------------------------------------------------------------

func (s *Server) handleGetToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := selector.Pick(ctx, s.Store, time.Now())
	if err != nil {
		if errors.Is(err, selector.ErrNoAvailable) {
			http.Error(w, "no available account", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Just-in-time refresh: if the picked token is about to expire, swap it
	// before handing it to the caller. The scheduler usually beats us to it,
	// but cover the fresh-process / paused-laptop case.
	remaining := time.Duration(a.ExpiresAt-time.Now().UnixMilli()) * time.Millisecond
	if remaining < JustInTimeRefreshThreshold {
		if err := s.Refresher.RefreshOne(ctx, a.ID); err != nil {
			http.Error(w, "refresh failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		// Re-read so we hand out the fresh token.
		a, err = s.Store.Get(ctx, a.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if err := s.Store.MarkUsed(ctx, a.ID); err != nil {
		// Log-only — handing out the token is more important than the LRU stamp.
		_ = err
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, a.AccessToken)
}

// --- /api/accounts ---------------------------------------------------------

type accountView struct {
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	ExpiresAt        int64  `json:"expires_at"`
	Scopes           string `json:"scopes"`
	SubscriptionType string `json:"subscription_type"`
	RateLimitTier    string `json:"rate_limit_tier"`
	OrganizationUUID string `json:"organization_uuid"`
	Status           string `json:"status"`
	CooldownUntil    int64  `json:"cooldown_until"`
	LastUsedAt       int64  `json:"last_used_at"`
	Last429At        int64  `json:"last_429_at"`
	CreatedAt        int64  `json:"created_at"`
	UpdatedAt        int64  `json:"updated_at"`
	// Tokens are deliberately omitted from the UI surface.
}

func toView(a store.Account) accountView {
	return accountView{
		ID: a.ID, Name: a.Name, ExpiresAt: a.ExpiresAt, Scopes: a.Scopes,
		SubscriptionType: a.SubscriptionType, RateLimitTier: a.RateLimitTier,
		OrganizationUUID: a.OrganizationUUID, Status: a.Status,
		CooldownUntil: a.CooldownUntil, LastUsedAt: a.LastUsedAt,
		Last429At: a.Last429At, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
	}
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
	Name   string `json:"name"`   // user-supplied alias for the account
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

	a := store.Account{
		Name:         strings.TrimSpace(req.Name),
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    expiresAt,
		Scopes:       tr.Scope,
		// SubscriptionType / RateLimitTier / OrganizationUUID could be
		// populated from a follow-up call to /api/oauth/profile; left empty
		// for the MVP and discovered passively as the account is used.
	}
	if a.Name == "" {
		a.Name = fmt.Sprintf("Account %d", time.Now().Unix())
	}
	if err := s.Store.Upsert(r.Context(), &a); err != nil {
		http.Error(w, "save account: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"account": toView(a)})
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
	writeJSON(w, http.StatusOK, map[string]any{"account": toView(*a)})
}

// --- install / uninstall scripts -------------------------------------------

func (s *Server) handleInstallScript(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	fmt.Fprint(w, assets.RenderInstallScript(s.Port))
}

func (s *Server) handleUninstallScript(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	fmt.Fprint(w, assets.RenderUninstallScript())
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
