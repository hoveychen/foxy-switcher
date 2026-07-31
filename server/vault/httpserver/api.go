package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/hoveychen/foxy-switcher/server/store"
	vaultauth "github.com/hoveychen/foxy-switcher/server/vault/auth"
)

// deviceNameMaxLen caps the user-supplied rename payload. The store column
// is TEXT with no DB-level limit, but the Devices table needs to render the
// value in a fixed-width cell; clamping at 64 keeps the UI sane and any
// over-eager paste from filling the DB with megabytes.
const deviceNameMaxLen = 64

// RegisterAPIRoutes mounts the JSON surface that the SPA admin web UI
// (mounted at /admin/* by the embedded React bundle) talks to. The
// shape mirrors the form handlers in web.go but returns JSON and uses
// 401 status (instead of a 302 redirect) when auth is missing — that
// way fetch() in the browser can detect "not signed in" without
// following a redirect into HTML.
//
// Routes live under /admin/api/* (NOT /api/*) so they never collide
// with the frontend httpapi surface that the desktop daemon mounts on
// /api/* in combined mode. ServeMux's "more specific wins" rule would
// otherwise let an /api/devices registered here silently shadow the
// httpapi.Server.handleListDevices the desktop expects to reach.
func (s *Server) RegisterAPIRoutes(mux *http.ServeMux) {
	// Public — no session required.
	mux.HandleFunc("POST /admin/api/login", s.handleAPILogin)
	mux.HandleFunc("POST /admin/api/setup", s.handleAPISetup)
	mux.HandleFunc("GET /admin/api/me", s.handleAPIMe)

	// Protected — session cookie required.
	mux.HandleFunc("POST /admin/api/logout", s.requireSessionJSON(s.handleAPILogout))
	mux.HandleFunc("GET /admin/api/devices", s.requireSessionJSON(s.handleAPIDevicesList))
	mux.HandleFunc("POST /admin/api/devices/revoke", s.requireSessionJSON(s.handleAPIDevicesRevoke))
	mux.HandleFunc("POST /admin/api/devices/suspend", s.requireSessionJSON(s.handleAPIDevicesSuspend))
	mux.HandleFunc("POST /admin/api/devices/resume", s.requireSessionJSON(s.handleAPIDevicesResume))
	mux.HandleFunc("POST /admin/api/devices/rename", s.requireSessionJSON(s.handleAPIDevicesRename))
	mux.HandleFunc("POST /admin/api/devices/providers", s.requireSessionJSON(s.handleAPIDevicesProviders))
	mux.HandleFunc("GET /admin/api/pair", s.requireSessionJSON(s.handleAPIPairLookup))
	mux.HandleFunc("POST /admin/api/pair", s.requireSessionJSON(s.handleAPIPairResolve))
	mux.HandleFunc("POST /admin/api/password", s.requireSessionJSON(s.handleAPIPassword))
}

// requireSessionJSON is the JSON-flavoured sibling of requireSession.
// Distinct from BearerAuth because admin /api/* is cookie-only — agent
// bearer tokens aren't admins and shouldn't be able to revoke peers or
// change the password by holding a device token. On miss: 401 JSON. On
// fresh-vault no-password-yet: 409 JSON `{"error":"setup_required"}` so
// the SPA knows to route to the setup screen.
func (s *Server) requireSessionJSON(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ok, err := s.st.HasPassword(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !ok {
			writeError(w, http.StatusConflict, errors.New("setup_required"))
			return
		}
		if !s.hasSession(r) {
			writeError(w, http.StatusUnauthorized, errors.New("unauthorized"))
			return
		}
		next(w, r)
	}
}

// --- /api/me -------------------------------------------------------------

type apiMeResp struct {
	HasPassword bool `json:"has_password"`
	SignedIn    bool `json:"signed_in"`
}

func (s *Server) handleAPIMe(w http.ResponseWriter, r *http.Request) {
	hasPw, err := s.st.HasPassword(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, apiMeResp{
		HasPassword: hasPw,
		SignedIn:    hasPw && s.hasSession(r),
	})
}

// --- /api/login ----------------------------------------------------------

type apiLoginReq struct {
	Password string `json:"password"`
}

func (s *Server) handleAPILogin(w http.ResponseWriter, r *http.Request) {
	var req apiLoginReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	if req.Password == "" {
		writeError(w, http.StatusBadRequest, errors.New("password required"))
		return
	}
	hasPw, err := s.st.HasPassword(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !hasPw {
		writeError(w, http.StatusConflict, errors.New("setup_required"))
		return
	}
	hash, err := s.st.PasswordHash(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !vaultauth.VerifyPassword(hash, req.Password) {
		writeError(w, http.StatusUnauthorized, errors.New("wrong password"))
		return
	}
	if err := s.startSession(r.Context(), w, r); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- /api/setup ----------------------------------------------------------

type apiSetupReq struct {
	Password string `json:"password"`
	Confirm  string `json:"confirm"`
}

func (s *Server) handleAPISetup(w http.ResponseWriter, r *http.Request) {
	hasPw, err := s.st.HasPassword(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if hasPw {
		writeError(w, http.StatusConflict, errors.New("already_set_up"))
		return
	}
	var req apiSetupReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	if req.Password == "" || req.Password != req.Confirm {
		writeError(w, http.StatusBadRequest, errors.New("passwords must match and be non-empty"))
		return
	}
	hash, err := vaultauth.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.st.SetPasswordHash(r.Context(), hash); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.startSession(r.Context(), w, r); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- /api/logout ---------------------------------------------------------

func (s *Server) handleAPILogout(w http.ResponseWriter, r *http.Request) {
	s.endSession(r.Context(), w, r)
	w.WriteHeader(http.StatusNoContent)
}

// --- /api/devices --------------------------------------------------------

type apiDeviceRow struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Hostname   string `json:"hostname,omitempty"`
	OS         string `json:"os,omitempty"`
	OSVersion  string `json:"os_version,omitempty"`
	Arch       string `json:"arch,omitempty"`
	Model      string `json:"model,omitempty"`
	AppVersion string `json:"app_version,omitempty"`
	ClientType string `json:"client_type,omitempty"`
	CreatedAt  int64  `json:"created_at"`
	LastSeenAt int64  `json:"last_seen_at"`
	// DisabledAt is 0 for an active device and the suspend timestamp
	// (UnixMilli) for a suspended one. The DevicesPage renders a
	// "suspended" badge and flips the action button to Resume when != 0.
	DisabledAt int64 `json:"disabled_at"`
	// AllowClaude / AllowCodex / AllowOpenRouter is the per-device provider
	// allowlist the DevicesPage renders as toggles.
	AllowClaude     bool `json:"allow_claude"`
	AllowCodex      bool `json:"allow_codex"`
	AllowOpenRouter bool `json:"allow_openrouter"`
	// CurrentLease names the account this device is currently leasing,
	// joined with the account name so the admin DevicesPage can render
	// "currently using X (12 min left)" without a second query. Nil when
	// the device holds no live lease.
	CurrentLease *apiDeviceLease `json:"current_lease,omitempty"`
}

// apiDeviceLease is the lightweight lease shape used by /admin/api/devices.
// account_name is taken from the accounts table (empty fallback) so the UI
// can render "currently using {name}" directly.
type apiDeviceLease struct {
	AccountID   int64  `json:"account_id"`
	AccountName string `json:"account_name"`
	AcquiredAt  int64  `json:"acquired_at"`
	ExpiresAt   int64  `json:"expires_at"`
}

type apiDevicesResp struct {
	Devices []apiDeviceRow `json:"devices"`
}

func (s *Server) handleAPIDevicesList(w http.ResponseWriter, r *http.Request) {
	devs, err := s.st.ListDevices(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	leases, err := s.st.ListActiveLeasesWithAccounts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	leaseByDevice := make(map[string]store.LeaseWithAccount, len(leases))
	for _, l := range leases {
		leaseByDevice[l.DeviceID] = l
	}
	out := make([]apiDeviceRow, 0, len(devs))
	for _, d := range devs {
		row := apiDeviceRow{
			ID:              d.ID,
			Name:            d.Name,
			Hostname:        d.Hostname,
			OS:              d.OS,
			OSVersion:       d.OSVersion,
			Arch:            d.Arch,
			Model:           d.Model,
			AppVersion:      d.AppVersion,
			ClientType:      d.ClientType,
			CreatedAt:       d.CreatedAt,
			LastSeenAt:      d.LastSeenAt,
			DisabledAt:      d.DisabledAt,
			AllowClaude:     d.AllowClaude,
			AllowCodex:      d.AllowCodex,
			AllowOpenRouter: d.AllowOpenRouter,
		}
		if l, ok := leaseByDevice[d.ID]; ok {
			row.CurrentLease = &apiDeviceLease{
				AccountID:   l.AccountID,
				AccountName: l.AccountName,
				AcquiredAt:  l.AcquiredAt,
				ExpiresAt:   l.ExpiresAt,
			}
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, apiDevicesResp{Devices: out})
}

type apiRevokeReq struct {
	ID string `json:"id"`
}

func (s *Server) handleAPIDevicesRevoke(w http.ResponseWriter, r *http.Request) {
	var req apiRevokeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, errors.New("id required"))
		return
	}
	if err := s.st.DeleteDevice(r.Context(), req.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAPIDevicesSuspend temporarily kicks a device without deleting it:
// it stamps disabled_at (so BearerAuth 401s the token) and releases any
// leases the device holds so the accounts return to the pool immediately.
// The row and token_hash survive, so a later Resume needs no re-pair.
func (s *Server) handleAPIDevicesSuspend(w http.ResponseWriter, r *http.Request) {
	var req apiRevokeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, errors.New("id required"))
		return
	}
	if err := s.st.SetDeviceDisabled(r.Context(), req.ID, true); err != nil {
		if notFoundIs(err) {
			writeError(w, http.StatusNotFound, errors.New("device not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// Free the accounts this device was holding. The device is already
	// disabled (the authoritative kick); a release failure shouldn't undo
	// that, but we surface it as 500 so the operator knows the pool didn't
	// fully free — the lease will still expire on its own TTL.
	if _, err := s.st.ReleaseDeviceLeases(r.Context(), req.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAPIDevicesResume clears disabled_at so the device's existing token
// authenticates again — no re-pair, since the row was never deleted.
func (s *Server) handleAPIDevicesResume(w http.ResponseWriter, r *http.Request) {
	var req apiRevokeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, errors.New("id required"))
		return
	}
	if err := s.st.SetDeviceDisabled(r.Context(), req.ID, false); err != nil {
		if notFoundIs(err) {
			writeError(w, http.StatusNotFound, errors.New("device not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type apiDeviceProvidersReq struct {
	ID          string `json:"id"`
	AllowClaude bool   `json:"allow_claude"`
	AllowCodex  bool   `json:"allow_codex"`
	// AllowOpenRouter is a pointer so a DevicesPage build that predates the
	// OpenRouter toggle (and therefore omits the field) can't silently revoke a
	// grant the admin made — omitted means "leave as-is", not false. Claude /
	// Codex keep their plain-bool shape: every shipped client sends both.
	AllowOpenRouter *bool `json:"allow_openrouter"`
}

// handleAPIDevicesProviders updates a device's provider allowlist (the choice
// made at approval). It then releases the device's live leases so the new
// allowlist takes effect on the device's next reconcile: a revoked provider's
// held lease is dropped (and can't be re-acquired — the vault gates both Pick
// and AcquireLease), and still-allowed providers are simply re-picked.
func (s *Server) handleAPIDevicesProviders(w http.ResponseWriter, r *http.Request) {
	var req apiDeviceProvidersReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, errors.New("id required"))
		return
	}
	// Resolve the omitted-field case against the row's current grant.
	allowOpenRouter := false
	if req.AllowOpenRouter != nil {
		allowOpenRouter = *req.AllowOpenRouter
	} else {
		cur, err := s.st.FindDevice(r.Context(), req.ID)
		if err != nil {
			if notFoundIs(err) {
				writeError(w, http.StatusNotFound, errors.New("device not found"))
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		allowOpenRouter = cur.AllowOpenRouter
	}
	if err := s.st.SetDeviceProviders(r.Context(), req.ID, req.AllowClaude, req.AllowCodex, allowOpenRouter); err != nil {
		if notFoundIs(err) {
			writeError(w, http.StatusNotFound, errors.New("device not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if _, err := s.st.ReleaseDeviceLeases(r.Context(), req.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// OpenRouter holds no lease, so ReleaseDeviceLeases above does nothing for
	// it. Withdrawing the grant instead has to kill the device's derived key
	// upstream — otherwise the key keeps working forever and "revoked" is a lie.
	if !allowOpenRouter {
		if err := s.revokeDeviceOpenRouter(r.Context(), req.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// revokeDeviceOpenRouter kills every OpenRouter runtime key minted for a
// device, both upstream and in the local mapping table. A nil OpenRouter field
// (combined-mode builds, tests that don't exercise the provider) makes this a
// no-op — but only safely so, because with no revoker configured no key could
// have been derived in the first place.
func (s *Server) revokeDeviceOpenRouter(ctx context.Context, deviceID string) error {
	if s.OpenRouter == nil {
		return nil
	}
	return s.OpenRouter.RevokeDeviceKeys(ctx, deviceID)
}

type apiRenameReq struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s *Server) handleAPIDevicesRename(w http.ResponseWriter, r *http.Request) {
	var req apiRenameReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, errors.New("id required"))
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, errors.New("name required"))
		return
	}
	if len(name) > deviceNameMaxLen {
		writeError(w, http.StatusBadRequest, fmt.Errorf("name too long (max %d)", deviceNameMaxLen))
		return
	}
	if err := s.st.UpdateDeviceName(r.Context(), req.ID, name); err != nil {
		if notFoundIs(err) {
			writeError(w, http.StatusNotFound, errors.New("device not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- /api/pair -----------------------------------------------------------

type apiPairLookupResp struct {
	Code       string `json:"code"`
	DeviceName string `json:"device_name"`
	Status     string `json:"status"`
}

func (s *Server) handleAPIPairLookup(w http.ResponseWriter, r *http.Request) {
	rawCode := r.URL.Query().Get("code")
	if rawCode == "" {
		writeError(w, http.StatusBadRequest, errors.New("code required"))
		return
	}
	code := vaultauth.NormaliseUserCode(rawCode)
	p, err := s.st.FindPairingByCode(r.Context(), code)
	if err != nil {
		if notFoundIs(err) {
			writeError(w, http.StatusNotFound, errors.New("code expired or unknown"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, apiPairLookupResp{
		Code:       p.UserCode,
		DeviceName: p.DeviceName,
		Status:     p.Status,
	})
}

type apiPairResolveReq struct {
	Code   string `json:"code"`
	Action string `json:"action"` // "approve" | "deny"
	// Provider allowlist chosen by the admin at approval. Pointers so an
	// omitted field falls back to the default (claude on, codex off) rather
	// than a zero-value false.
	AllowClaude     *bool `json:"allow_claude"`
	AllowCodex      *bool `json:"allow_codex"`
	AllowOpenRouter *bool `json:"allow_openrouter"`
}

type apiPairResolveResp struct {
	Result string `json:"result"`
}

func (s *Server) handleAPIPairResolve(w http.ResponseWriter, r *http.Request) {
	var req apiPairResolveReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	code := vaultauth.NormaliseUserCode(req.Code)
	if code == "" {
		writeError(w, http.StatusBadRequest, errors.New("code required"))
		return
	}
	switch req.Action {
	case "deny":
		if err := s.st.DenyPairing(r.Context(), code); err != nil && !notFoundIs(err) {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, apiPairResolveResp{Result: store.PairingDenied})
	case "approve":
		token := vaultauth.NewToken()
		deviceID := vaultauth.NewID()
		allowClaude := req.AllowClaude == nil || *req.AllowClaude             // default on
		allowCodex := req.AllowCodex != nil && *req.AllowCodex                // default off
		allowOpenRouter := req.AllowOpenRouter != nil && *req.AllowOpenRouter // default off
		if err := s.st.ApprovePairing(r.Context(), code, deviceID, token, allowClaude, allowCodex, allowOpenRouter); err != nil {
			if notFoundIs(err) {
				writeError(w, http.StatusNotFound, errors.New("code expired or already used"))
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, apiPairResolveResp{Result: store.PairingApproved})
	default:
		writeError(w, http.StatusBadRequest, errors.New("action must be approve or deny"))
	}
}

// --- /api/password -------------------------------------------------------

type apiPasswordReq struct {
	Current string `json:"current"`
	Next    string `json:"next"`
	Confirm string `json:"confirm"`
}

func (s *Server) handleAPIPassword(w http.ResponseWriter, r *http.Request) {
	var req apiPasswordReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	if req.Next == "" || req.Next != req.Confirm {
		writeError(w, http.StatusBadRequest, errors.New("new passwords must match and be non-empty"))
		return
	}
	hash, err := s.st.PasswordHash(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !vaultauth.VerifyPassword(hash, req.Current) {
		writeError(w, http.StatusUnauthorized, errors.New("current password is wrong"))
		return
	}
	newHash, err := vaultauth.HashPassword(req.Next)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.st.SetPasswordHash(r.Context(), newHash); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
