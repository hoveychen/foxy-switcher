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
	"github.com/hoveychen/foxy-switcher/server/store"
	"github.com/hoveychen/foxy-switcher/server/vault"
	vaultauth "github.com/hoveychen/foxy-switcher/server/vault/auth"
)

// Server is the HTTP entrypoint for vault.Service plus the device-flow
// pairing surface. The store reference is needed for auth lookups and
// pairing rows that don't fit naturally on vault.Service (those operations
// are vault-internal — the agent never invokes them directly other than
// pair-init / pair-poll).
type Server struct {
	svc vault.Service
	st  *store.Store
	// PublicBaseURL is what we hand back to the agent so it can tell the
	// user where to enter the user_code. Empty falls back to the request
	// host, which is fine for combined-mode loopback.
	PublicBaseURL string
}

// New constructs the handler. svc and st are non-nil. PublicBaseURL is set
// by the caller via the field; tests leave it empty.
func New(svc vault.Service, st *store.Store) *Server {
	return &Server{svc: svc, st: st}
}

// Handler returns a *http.ServeMux wired with every agent-facing route.
// Mount it on the same listener as the frontend httpapi (Step 2.6 wires
// both into a single mux so combined / vault mode share one port).
//
// Bearer auth gates every route except pair-init / pair-poll, which the
// agent calls before it has a token.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Public routes — no auth (agent doesn't have a token yet).
	mux.HandleFunc("POST /agent/v1/devices/pair-init", s.handlePairInit)
	mux.HandleFunc("POST /agent/v1/devices/pair-poll", s.handlePairPoll)
	// Protected routes — Bearer token required.
	protected := http.NewServeMux()
	protected.HandleFunc("GET /agent/v1/accounts", s.handleListAccounts)
	protected.HandleFunc("GET /agent/v1/auto-switch", s.handleGetAutoSwitch)
	protected.HandleFunc("POST /agent/v1/pick", s.handlePick)
	protected.HandleFunc("POST /agent/v1/accounts/{id}/used", s.handleMarkUsed)
	protected.HandleFunc("POST /agent/v1/accounts/{id}/tokens", s.handleUpdateTokens)
	protected.HandleFunc("POST /agent/v1/leases", s.handleAcquireLease)
	protected.HandleFunc("POST /agent/v1/leases/{id}/renew", s.handleRenewLease)
	protected.HandleFunc("DELETE /agent/v1/leases/{id}", s.handleReleaseLease)
	mux.Handle("/agent/v1/", s.requireBearer(protected))
	return mux
}

// --- pairing -------------------------------------------------------------

type pairInitReq struct {
	ClientNonce string `json:"client_nonce"`
	DeviceName  string `json:"device_name"`
}

type pairInitResp struct {
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	ExpiresInMillis int64  `json:"expires_in_ms"`
}

func (s *Server) handlePairInit(w http.ResponseWriter, r *http.Request) {
	var req pairInitReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	if req.ClientNonce == "" || req.DeviceName == "" {
		writeError(w, http.StatusBadRequest, errors.New("client_nonce and device_name required"))
		return
	}
	// Sweep before insert so the user_code uniqueness constraint isn't
	// blocked by a stale row.
	_ = s.st.SweepPairings(r.Context())

	code, err := vaultauth.NewUserCode()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	expiresAt := time.Now().Add(PairingTTL).UnixMilli()
	if err := s.st.InsertPairing(r.Context(), store.Pairing{
		ClientNonce: req.ClientNonce,
		UserCode:    code,
		DeviceName:  req.DeviceName,
		ExpiresAt:   expiresAt,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, pairInitResp{
		UserCode:        code,
		VerificationURL: s.verificationURL(r),
		ExpiresInMillis: int64(PairingTTL / time.Millisecond),
	})
}

type pairPollReq struct {
	ClientNonce string `json:"client_nonce"`
}

type pairPollResp struct {
	Status      string `json:"status"`
	DeviceID    string `json:"device_id,omitempty"`
	DeviceToken string `json:"device_token,omitempty"`
}

func (s *Server) handlePairPoll(w http.ResponseWriter, r *http.Request) {
	var req pairPollReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	if req.ClientNonce == "" {
		writeError(w, http.StatusBadRequest, errors.New("client_nonce required"))
		return
	}
	p, err := s.st.FindPairingByNonce(r.Context(), req.ClientNonce)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Expired or unknown — agent restarts the flow.
			writeJSON(w, http.StatusOK, pairPollResp{Status: "expired"})
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	switch p.Status {
	case store.PairingPending:
		writeJSON(w, http.StatusOK, pairPollResp{Status: store.PairingPending})
	case store.PairingDenied:
		// Surface and forget — the agent reads "denied" and gives up. Sweep
		// will GC the row at expiry.
		writeJSON(w, http.StatusOK, pairPollResp{Status: store.PairingDenied})
	case store.PairingApproved:
		// Approved row holds the plaintext token. We hand it back, then
		// promote it into the devices table (hashed) and delete the
		// pairing row so the plaintext doesn't linger.
		token := p.DeviceToken
		deviceID := p.DeviceID
		if token == "" || deviceID == "" {
			writeError(w, http.StatusInternalServerError, errors.New("approved pairing missing token"))
			return
		}
		if err := s.st.InsertDevice(r.Context(), store.Device{
			ID:        deviceID,
			Name:      p.DeviceName,
			TokenHash: vaultauth.HashToken(token),
		}); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		// Drop the pairing row — plaintext token is no longer needed once
		// the device row is in place.
		_ = s.st.SweepPairings(r.Context()) // sweeps expired; we'll explicitly delete this one below.
		// Mark this specific row as denied so the plaintext gets cleared
		// even if the sweeper hasn't fired yet. (The denied state is the
		// terminal disposal; the row is harmless after the device row exists.)
		_ = s.st.DenyPairing(r.Context(), p.UserCode)
		writeJSON(w, http.StatusOK, pairPollResp{
			Status:      store.PairingApproved,
			DeviceID:    deviceID,
			DeviceToken: token,
		})
	default:
		writeError(w, http.StatusInternalServerError, fmt.Errorf("unknown pairing status %q", p.Status))
	}
}

// verificationURL is the absolute URL the agent prints to the user. Built
// from PublicBaseURL when set (vault mode behind a reverse proxy with a
// known FQDN), or from the request host (combined mode loopback).
func (s *Server) verificationURL(r *http.Request) string {
	if s.PublicBaseURL != "" {
		return s.PublicBaseURL + VerificationPath
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host + VerificationPath
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
