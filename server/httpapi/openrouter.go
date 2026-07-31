package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/hoveychen/foxy-switcher/server/activity"
	"github.com/hoveychen/foxy-switcher/server/openrouter"
	"github.com/hoveychen/foxy-switcher/server/store"
)

// openrouter.go is the admin surface for OpenRouter accounts: create one, edit
// its derivation template (the model list each device offers and the spend cap
// each device's key carries), store its management key, and verify that key is
// really a provisioning key.
//
// The management key is write-only across this API. It goes in, but no handler
// ever reads it back out — the UI renders a "configured / not configured" flag
// instead. Anything else would put a mint-and-revoke credential behind a plain
// GET.

// OpenRouterKeyService is the revocation half of vault.OpenRouterKeys that this
// package needs. Declared here (rather than importing the concrete type) so the
// dependency points inward and tests can substitute a recorder.
type OpenRouterKeyService interface {
	RevokeAccountKeys(ctx context.Context, accountID int64) error
}

// SetOpenRouterKeys wires the derivation service so policy edits can invalidate
// outstanding device keys. Leaving it nil is valid for combined-mode desktop
// builds — with no devices there are no derived keys to invalidate.
func (s *Server) SetOpenRouterKeys(k OpenRouterKeyService) { s.openRouterKeys = k }

// openRouterView is the admin-visible shape of an OpenRouter account's config.
// Note the absence of the management key itself.
type openRouterView struct {
	AllowedModels []string `json:"allowed_models"`
	LimitUSD      float64  `json:"limit_usd,omitempty"`
	LimitReset    string   `json:"limit_reset,omitempty"`
	WorkspaceID   string   `json:"workspace_id,omitempty"`
	// HasManagementKey is how the UI shows configured-ness without the secret.
	HasManagementKey bool `json:"has_management_key"`
	// IsProvisioning reports what the stored key can do, as detected at save
	// time. True: each device gets its own revocable key. False: every device
	// shares this one, so revoking a device can't revoke the key.
	IsProvisioning bool `json:"is_provisioning"`
	// DerivedKeyCount is how many devices currently hold a key from this
	// account — the blast radius of a policy edit, which revokes them all.
	DerivedKeyCount int `json:"derived_key_count"`
	// Credit is the account balance as last polled. Nil when never polled — the
	// UI must show "unknown" rather than "$0", which would read as broke.
	Credit *openRouterCreditView `json:"credit,omitempty"`
	// OutOfCredit mirrors the selector's own verdict, so the card's badge and the
	// routing decision can't disagree.
	OutOfCredit bool `json:"out_of_credit"`
}

// openRouterCreditView is an account's balance as the admin UI shows it.
type openRouterCreditView struct {
	Total     float64 `json:"total"`
	Remaining float64 `json:"remaining"`
	CheckedAt int64   `json:"checked_at"`
}

// openRouterConfigFor builds the view for one account. Errors are returned
// rather than swallowed so a corrupt credential_json shows up as a failed
// account list instead of an account that silently appears unconfigured.
func (s *Server) openRouterConfigFor(ctx context.Context, a store.Account) (*openRouterView, error) {
	if a.Provider != store.ProviderOpenRouter {
		return nil, nil
	}
	cfg, err := store.ParseOpenRouterConfig(a.CredentialJSON)
	if err != nil {
		return nil, err
	}
	rows, err := s.Store.ListAccountOpenRouterKeys(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	view := &openRouterView{
		AllowedModels:   cfg.AllowedModels,
		LimitUSD:        cfg.LimitUSD,
		LimitReset:      cfg.LimitReset,
		WorkspaceID:     cfg.WorkspaceID,
		DerivedKeyCount: len(rows),
	}
	cred, err := s.Store.OpenRouterCredential(ctx, a.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// No key yet. Leave Credit nil so the UI says "unknown", not "$0".
	case err != nil:
		return nil, err
	default:
		view.HasManagementKey = true
		view.IsProvisioning = cred.IsProvisioning
		view.OutOfCredit = !cred.HasCredit()
		if cred.CreditCheckedAt != 0 {
			view.Credit = &openRouterCreditView{
				Total:     cred.CreditTotal,
				Remaining: cred.CreditRemaining,
				CheckedAt: cred.CreditCheckedAt,
			}
		}
	}
	return view, nil
}

// openRouterAccountReq is the create/update payload. ManagementKey is optional
// on update ("" = leave the stored one alone) so an admin can retune the model
// list without re-pasting the secret.
type openRouterAccountReq struct {
	Name          string   `json:"name"`
	AllowedModels []string `json:"allowed_models"`
	LimitUSD      float64  `json:"limit_usd"`
	LimitReset    string   `json:"limit_reset"`
	WorkspaceID   string   `json:"workspace_id"`
	ManagementKey string   `json:"management_key"`
}

var validLimitResets = map[string]bool{
	"": true, "daily": true, "weekly": true, "monthly": true,
}

func (r *openRouterAccountReq) normalise() error {
	r.Name = strings.TrimSpace(r.Name)
	r.LimitReset = strings.ToLower(strings.TrimSpace(r.LimitReset))
	if !validLimitResets[r.LimitReset] {
		return fmt.Errorf("limit_reset must be daily, weekly, monthly, or empty (got %q)", r.LimitReset)
	}
	if r.LimitUSD < 0 {
		return errors.New("limit_usd cannot be negative")
	}
	return nil
}

func (r openRouterAccountReq) config() store.OpenRouterAccountConfig {
	cfg := store.OpenRouterAccountConfig{
		AllowedModels: r.AllowedModels,
		LimitUSD:      r.LimitUSD,
		LimitReset:    r.LimitReset,
		WorkspaceID:   strings.TrimSpace(r.WorkspaceID),
	}
	cfg.Normalise()
	return cfg
}

// handleCreateOpenRouterAccount adds an OpenRouter pool account. Unlike Claude
// and Codex there is no OAuth dance: the admin supplies a management key and a
// policy, and devices derive their own keys from it later.
func (s *Server) handleCreateOpenRouterAccount(w http.ResponseWriter, r *http.Request) {
	var req openRouterAccountReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := req.normalise(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.ManagementKey) == "" {
		http.Error(w, "api_key required — the vault needs a key to hand devices",
			http.StatusBadRequest)
		return
	}
	// Ask OpenRouter what sort of key this is rather than making the admin
	// declare it. Also validates the key before we store it, so a typo fails at
	// save time instead of at every device's first request.
	isProvisioning, err := s.detectOpenRouterKeyKind(r.Context(), req.ManagementKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	cfg := req.config()

	acc := &store.Account{
		Provider: store.ProviderOpenRouter,
		Name:     req.Name,
		// Dedup keys must be non-empty or two accounts collapse onto one row via
		// the partial unique indexes. OpenRouter gives us no account identity, so
		// synthesise one from the admin's chosen name.
		AccountUUID:      "openrouter:" + req.Name,
		Email:            "",
		SubscriptionType: "payg",
		Plan:             "OpenRouter",
		Status:           store.StatusActive,
	}
	if err := s.Store.Upsert(r.Context(), acc); err != nil {
		http.Error(w, "save OpenRouter account: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.Store.SetOpenRouterConfig(r.Context(), acc.ID, cfg); err != nil {
		http.Error(w, "save OpenRouter config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.Store.SetOpenRouterCredential(r.Context(), acc.ID, req.ManagementKey, isProvisioning); err != nil {
		http.Error(w, "save API key: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.Bus.EmitInfo(activity.TypeAccountAdded, acc.ID,
		fmt.Sprintf("Added OpenRouter account %s (%d model(s))", acc.Name, len(cfg.AllowedModels)))
	// Re-read: Upsert filled in the id, but the config was written afterwards,
	// so the in-memory row still has an empty credential_json.
	saved, err := s.Store.Get(r.Context(), acc.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeOpenRouterAccount(w, r.Context(), *saved)
}

// handleUpdateOpenRouterAccount retunes an existing account's derivation
// template.
//
// Any policy change revokes every key already derived from this account. That
// is not over-caution: each outstanding key was minted with the OLD spend cap,
// so leaving them alive would mean the admin's edit silently applies to new
// devices only. Re-derivation happens on each device's next config fetch.
func (s *Server) handleUpdateOpenRouterAccount(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	acc, err := s.Store.Get(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if acc.Provider != store.ProviderOpenRouter {
		http.Error(w, "not an OpenRouter account", http.StatusBadRequest)
		return
	}
	var req openRouterAccountReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := req.normalise(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	newCfg := req.config()
	oldCfg, err := store.ParseOpenRouterConfig(acc.CredentialJSON)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Rotating the management key also invalidates outstanding keys: the new key
	// may not even be able to revoke what the old one minted, so we must clear
	// them while the old key still works.
	rotatingKey := strings.TrimSpace(req.ManagementKey) != ""
	if policyChanged(oldCfg, newCfg) || rotatingKey {
		if err := s.revokeOpenRouterAccountKeys(r.Context(), id); err != nil {
			http.Error(w, "revoke outstanding device keys: "+err.Error(), http.StatusBadGateway)
			return
		}
	}
	if err := s.Store.SetOpenRouterConfig(r.Context(), id, newCfg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if rotatingKey {
		isProvisioning, derr := s.detectOpenRouterKeyKind(r.Context(), req.ManagementKey)
		if derr != nil {
			http.Error(w, derr.Error(), http.StatusBadGateway)
			return
		}
		if err := s.Store.SetOpenRouterCredential(r.Context(), id, req.ManagementKey, isProvisioning); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	// Name is create-only: it seeds account_uuid (the dedup key), so renaming
	// would either orphan the identity or collide with another row. Claude and
	// Codex accounts aren't renamable either.
	updated, err := s.Store.Get(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Bus.EmitInfo(activity.TypeAccountUpdated, id,
		fmt.Sprintf("Updated OpenRouter account %s (%d model(s))", updated.Name, len(newCfg.AllowedModels)))
	s.writeOpenRouterAccount(w, r.Context(), *updated)
}

// policyChanged reports whether an edit alters what a derived key is allowed to
// do. Cosmetic fields (workspace aside — it changes which workspace a key is
// minted in, so it counts) don't force a revoke.
func policyChanged(oldCfg, newCfg store.OpenRouterAccountConfig) bool {
	if oldCfg.LimitUSD != newCfg.LimitUSD ||
		oldCfg.LimitReset != newCfg.LimitReset ||
		oldCfg.WorkspaceID != newCfg.WorkspaceID ||
		len(oldCfg.AllowedModels) != len(newCfg.AllowedModels) {
		return true
	}
	// Both slices are Normalise()d (sorted, de-duped) so an element-wise compare
	// is exact — a reordered paste of the same models is not a change.
	for i := range oldCfg.AllowedModels {
		if oldCfg.AllowedModels[i] != newCfg.AllowedModels[i] {
			return true
		}
	}
	return false
}

// handleCheckOpenRouterAccount verifies the stored credential really is a
// provisioning key — the one misconfiguration that silently breaks everything
// (an admin pasting the sk-or- key they use for chat). Read-only: it lists keys
// rather than creating anything on the operator's account.
func (s *Server) handleCheckOpenRouterAccount(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cred, err := s.Store.OpenRouterCredential(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no API key on file for this account", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	caps, err := s.newOpenRouterClient(cred.APIKey).CheckManagementKey(r.Context())
	if err != nil {
		http.Error(w, "probe OpenRouter: "+err.Error(), http.StatusBadGateway)
		return
	}
	// Re-record the detected kind: a key can be upgraded (or the account's plan
	// changed) after it was saved, and leaving a stale flag would mean deriving
	// against a key that can't mint, or vice versa.
	if caps.KeyValid && caps.ManagementKeyValid != cred.IsProvisioning {
		if err := s.Store.SetOpenRouterCredential(r.Context(), id, cred.APIKey, caps.ManagementKeyValid); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	// A fresh balance while we're here — the probe is the admin's "is this
	// working?" button, and a stale figure next to a green tick is confusing.
	if caps.CreditKnown {
		if err := s.Store.SetOpenRouterCredit(r.Context(), id, caps.CreditTotal, caps.CreditRemaining); err != nil &&
			!errors.Is(err, store.ErrNotFound) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"key_valid":            caps.KeyValid,
		"management_key_valid": caps.ManagementKeyValid,
		"is_provisioning":      caps.ManagementKeyValid,
		"credit_known":         caps.CreditKnown,
		"credit_remaining":     caps.CreditRemaining,
		"detail":               caps.Detail,
	})
}

// detectOpenRouterKeyKind asks OpenRouter whether a key can mint other keys.
//
// Detection rather than declaration: the admin pastes a key and OpenRouter says
// what it is (GET /key reports is_provisioning_key). It doubles as validation —
// a rejected key fails here, at save time, instead of at every device's first
// request.
//
// newOpenRouterClient is a field so tests don't need the network.
func (s *Server) detectOpenRouterKeyKind(ctx context.Context, apiKey string) (bool, error) {
	info, err := s.newOpenRouterClient(apiKey).KeySelf(ctx)
	if errors.Is(err, openrouter.ErrUnauthorized) {
		return false, errors.New("OpenRouter rejected this key — check it was copied in full")
	}
	if err != nil {
		return false, fmt.Errorf("verify key with OpenRouter: %w", err)
	}
	return info.IsProvisioning, nil
}

// openRouterKeyReader is the slice of the openrouter client the admin surface
// needs.
type openRouterKeyReader interface {
	KeySelf(ctx context.Context) (openrouter.KeyInfo, error)
	CheckManagementKey(ctx context.Context) (openrouter.Capabilities, error)
}

// newOpenRouterClient builds a client for one key. Overridden in tests.
func (s *Server) newOpenRouterClient(apiKey string) openRouterKeyReader {
	if s.openRouterClientFor != nil {
		return s.openRouterClientFor(apiKey)
	}
	return &openrouter.Client{ManagementKey: apiKey}
}

// SetOpenRouterClientFactory swaps the client constructor. Tests only.
func (s *Server) SetOpenRouterClientFactory(f func(apiKey string) openRouterKeyReader) {
	s.openRouterClientFor = f
}

// revokeOpenRouterAccountKeys kills every derived key for an account. With no
// derivation service wired there can be no derived keys, so it's a no-op.
func (s *Server) revokeOpenRouterAccountKeys(ctx context.Context, accountID int64) error {
	if s.openRouterKeys == nil {
		return nil
	}
	return s.openRouterKeys.RevokeAccountKeys(ctx, accountID)
}

func (s *Server) writeOpenRouterAccount(w http.ResponseWriter, ctx context.Context, acc store.Account) {
	view := toView(acc)
	cfg, err := s.openRouterConfigFor(ctx, acc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	view.OpenRouter = cfg
	writeJSON(w, http.StatusOK, map[string]any{"account": view})
}
