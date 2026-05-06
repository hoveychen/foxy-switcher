package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/hoveychen/foxy-switcher/server/vault/httpclient"
)

// Vault pairing proxy.
//
// The Tauri webview can only fetch URLs the http-plugin scope explicitly
// allows. Adding every possible vault host to that allow-list (or
// widening it to all of HTTPS) gives the renderer more network reach
// than the rest of the app needs. Instead, the desktop UI talks only to
// 127.0.0.1 (already in scope) and the daemon — which has no scope
// limits — performs the actual pair-init / pair-poll handshake against
// the user-supplied vault URL. The TUI flow (`foxy-switcher pair`) uses
// the same httpclient, so both surfaces share one implementation.

type pairInitRequest struct {
	VaultURL    string                  `json:"vault_url"`
	DeviceName  string                  `json:"device_name"`
	ClientNonce string                  `json:"client_nonce"`
	DeviceMeta  *httpclient.PairMetadata `json:"device_meta,omitempty"`
}

type pairPollRequest struct {
	VaultURL    string `json:"vault_url"`
	ClientNonce string `json:"client_nonce"`
}

// pairPollResponse mirrors the vault's own /agent/v1/devices/pair-poll
// envelope so the frontend can keep its existing state machine. On
// "approved" device_id + device_token are populated; on "denied" /
// "expired" they're empty and the frontend renders a terminal error.
type pairPollResponse struct {
	Status      string `json:"status"`
	DeviceID    string `json:"device_id,omitempty"`
	DeviceToken string `json:"device_token,omitempty"`
}

func (s *Server) handlePairInit(w http.ResponseWriter, r *http.Request) {
	var req pairInitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	url := strings.TrimRight(strings.TrimSpace(req.VaultURL), "/")
	if url == "" {
		http.Error(w, "vault_url is required", http.StatusBadRequest)
		return
	}
	if req.ClientNonce == "" {
		http.Error(w, "client_nonce is required", http.StatusBadRequest)
		return
	}
	if req.DeviceName == "" {
		http.Error(w, "device_name is required", http.StatusBadRequest)
		return
	}
	client := httpclient.New(url)
	out, err := client.PairInit(r.Context(), req.DeviceName, req.ClientNonce, req.DeviceMeta)
	if err != nil {
		http.Error(w, "pair-init: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlePairPoll(w http.ResponseWriter, r *http.Request) {
	var req pairPollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	url := strings.TrimRight(strings.TrimSpace(req.VaultURL), "/")
	if url == "" {
		http.Error(w, "vault_url is required", http.StatusBadRequest)
		return
	}
	if req.ClientNonce == "" {
		http.Error(w, "client_nonce is required", http.StatusBadRequest)
		return
	}
	client := httpclient.New(url)
	status, res, err := client.PairPollOnce(r.Context(), req.ClientNonce)
	// PairPollOnce returns (PairDenied, nil, ErrPairingDenied) and
	// (PairExpired, nil, ErrPairingExpired) — those are terminal device-flow
	// outcomes, not transport errors, so we surface them as 200 + status
	// instead of 502. Anything else with a non-nil err is a real failure.
	if err != nil && !errors.Is(err, httpclient.ErrPairingDenied) && !errors.Is(err, httpclient.ErrPairingExpired) {
		http.Error(w, "pair-poll: "+err.Error(), http.StatusBadGateway)
		return
	}
	resp := pairPollResponse{Status: string(status)}
	if status == httpclient.PairApproved && res != nil {
		resp.DeviceID = res.DeviceID
		resp.DeviceToken = res.DeviceToken
	}
	writeJSON(w, http.StatusOK, resp)
}
