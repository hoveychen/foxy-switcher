// Package httpserver wraps a vault.Service in HTTP handlers so a remote
// agent (running credinject on the user's local machine) can drive a
// vault deployed on a different host. The route surface is intentionally
// distinct from the frontend-facing httpapi: handlers here return raw
// store.Account JSON (tokens included — the agent needs them to inject)
// while the frontend surface in httpapi/ redacts tokens.
//
// Step 2 leaves authentication out — Step 3 will fold a Bearer token check
// into the middleware exposed by Handler. Until then, the assumption is
// loopback or VPN.
package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/hoveychen/foxy-switcher/server/selector"
	"github.com/hoveychen/foxy-switcher/server/vault"
)

// Server is the HTTP entrypoint for vault.Service. It owns no state of its
// own — every request resolves to a single Service call.
type Server struct {
	svc vault.Service
}

// New constructs the handler. svc is non-nil; tests pass a fake.
func New(svc vault.Service) *Server {
	return &Server{svc: svc}
}

// Handler returns a *http.ServeMux wired with every agent-facing route.
// Mount it on the same listener as the frontend httpapi (Step 2.6 wires
// both into a single mux so combined / vault mode share one port).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agent/v1/accounts", s.handleListAccounts)
	mux.HandleFunc("GET /agent/v1/auto-switch", s.handleGetAutoSwitch)
	mux.HandleFunc("POST /agent/v1/pick", s.handlePick)
	mux.HandleFunc("POST /agent/v1/accounts/{id}/used", s.handleMarkUsed)
	mux.HandleFunc("POST /agent/v1/accounts/{id}/tokens", s.handleUpdateTokens)
	mux.HandleFunc("POST /agent/v1/leases", s.handleAcquireLease)
	mux.HandleFunc("POST /agent/v1/leases/{id}/renew", s.handleRenewLease)
	mux.HandleFunc("DELETE /agent/v1/leases/{id}", s.handleReleaseLease)
	return mux
}

// --- accounts -------------------------------------------------------------

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	accs, err := s.svc.ListAccounts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accs})
}

func (s *Server) handleGetAutoSwitch(w http.ResponseWriter, r *http.Request) {
	v, err := s.svc.GetAutoSwitch(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

type pickResponse struct {
	Account *vault.Account `json:"account,omitempty"`
}

func (s *Server) handlePick(w http.ResponseWriter, r *http.Request) {
	// Body is empty — vault uses its own clock. The agent doesn't pass a
	// `now` because clocks across hosts may differ; vault is the authority.
	a, err := s.svc.Pick(r.Context(), time.Now())
	if err != nil {
		if errors.Is(err, selector.ErrNoAvailable) {
			// 204 — no account is currently eligible. Agent treats this as
			// the same signal as today's selector.ErrNoAvailable.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, pickResponse{Account: a})
}

func (s *Server) handleMarkUsed(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.svc.MarkUsed(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type updateTokensReq struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"`
}

func (s *Server) handleUpdateTokens(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req updateTokensReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	if err := s.svc.UpdateTokens(r.Context(), id, req.AccessToken, req.RefreshToken, req.ExpiresAt); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- leases ---------------------------------------------------------------

type acquireLeaseReq struct {
	AccountID int64  `json:"account_id"`
	DeviceID  string `json:"device_id"`
	TTLMillis int64  `json:"ttl_ms"`
}

func (s *Server) handleAcquireLease(w http.ResponseWriter, r *http.Request) {
	var req acquireLeaseReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	if req.AccountID == 0 || req.DeviceID == "" || req.TTLMillis <= 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("account_id, device_id, ttl_ms required"))
		return
	}
	lease, err := s.svc.AcquireLease(r.Context(), req.AccountID, req.DeviceID, time.Duration(req.TTLMillis)*time.Millisecond)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, lease)
}

type renewLeaseReq struct {
	TTLMillis int64 `json:"ttl_ms"`
}

func (s *Server) handleRenewLease(w http.ResponseWriter, r *http.Request) {
	leaseID := r.PathValue("id")
	if leaseID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("lease id required"))
		return
	}
	var req renewLeaseReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	if req.TTLMillis <= 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("ttl_ms required"))
		return
	}
	lease, err := s.svc.RenewLease(r.Context(), leaseID, time.Duration(req.TTLMillis)*time.Millisecond)
	if err != nil {
		if errors.Is(err, vault.ErrLeaseNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, lease)
}

func (s *Server) handleReleaseLease(w http.ResponseWriter, r *http.Request) {
	leaseID := r.PathValue("id")
	if leaseID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("lease id required"))
		return
	}
	if err := s.svc.ReleaseLease(r.Context(), leaseID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers -------------------------------------------------------------

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

// writeError emits a JSON body so the agent can decode `{"error":"…"}`
// uniformly without sniffing Content-Type.
func writeError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// Compile-time guard.
var _ = context.Background
