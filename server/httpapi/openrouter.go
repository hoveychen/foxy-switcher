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

// openrouter.go is the admin surface for OpenRouter accounts: create one,
// edit its derivation template (the model allowlist and spend cap that drive
// BOTH the upstream guardrail and the device's profile files), store its
// management key, and probe what the account's plan actually supports.
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
	// EnforceModels reports whether an upstream guardrail backs the allowlist.
	// Off by default: a derived key already caps spend, tracks usage and revokes
	// per device on its own; the guardrail only adds server-side model
	// restriction.
	EnforceModels bool `json:"enforce_models"`
	// HasManagementKey is how the UI shows configured-ness without the secret.
	HasManagementKey bool `json:"has_management_key"`
	// DerivedKeyCount is how many devices currently hold a key from this
	// account — the blast radius of an allowlist edit, which revokes them all.
	DerivedKeyCount int `json:"derived_key_count"`
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
	hasKey, err := s.Store.HasOpenRouterManagementKey(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	rows, err := s.Store.ListAccountOpenRouterKeys(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	return &openRouterView{
		AllowedModels:    cfg.AllowedModels,
		LimitUSD:         cfg.LimitUSD,
		LimitReset:       cfg.LimitReset,
		WorkspaceID:      cfg.WorkspaceID,
		EnforceModels:    cfg.EnforceModels,
		HasManagementKey: hasKey,
		DerivedKeyCount:  len(rows),
	}, nil
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
	EnforceModels bool     `json:"enforce_models"`
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
	// Verified against the live API: a GUARDRAIL carrying limit_usd must also
	// carry reset_interval ("Reset interval is required when setting a budget
	// limit"); there is no lifetime budget window on a guardrail. `/keys` has no
	// such rule — a `limit` with no `limit_reset` is a valid lifetime cap on the
	// key — so this only bites when a guardrail is actually going to be created.
	// Reject it at save time rather than letting every device's derivation fail
	// later with a raw upstream validation error.
	if r.EnforceModels && r.LimitUSD > 0 && r.LimitReset == "" {
		return errors.New("a spend cap needs limit_reset (daily, weekly or monthly) — " +
			"OpenRouter has no lifetime budget window")
	}
	return nil
}

func (r openRouterAccountReq) config() store.OpenRouterAccountConfig {
	cfg := store.OpenRouterAccountConfig{
		AllowedModels: r.AllowedModels,
		LimitUSD:      r.LimitUSD,
		LimitReset:    r.LimitReset,
		WorkspaceID:   strings.TrimSpace(r.WorkspaceID),
		EnforceModels: r.EnforceModels,
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
		http.Error(w, "management_key required — without it the vault cannot mint device keys",
			http.StatusBadRequest)
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
	if err := s.Store.SetOpenRouterManagementKey(r.Context(), acc.ID, req.ManagementKey); err != nil {
		http.Error(w, "save management key: "+err.Error(), http.StatusInternalServerError)
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
// is not over-caution: each outstanding key carries the OLD allowlist and cap
// baked into its own upstream guardrail, so leaving them alive would mean the
// admin's edit silently applies to new devices only. Re-derivation happens on
// each device's next config fetch.
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
		if err := s.Store.SetOpenRouterManagementKey(r.Context(), id, req.ManagementKey); err != nil {
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
		oldCfg.EnforceModels != newCfg.EnforceModels ||
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

// handleCheckOpenRouterAccount runs the capability probe against the stored
// management key. This is the design's blocking prerequisite made operable:
// it answers "is the model allowlist actually enforced server-side, or only
// advisory?" against a real account, and catches the common
// inference-key-instead-of-provisioning-key mistake.
func (s *Server) handleCheckOpenRouterAccount(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	key, err := s.Store.OpenRouterManagementKey(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no management key on file for this account", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	caps, err := (&openrouter.Client{ManagementKey: key}).CheckCapabilities(r.Context())
	if err != nil {
		http.Error(w, "probe OpenRouter: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"management_key_valid": caps.ManagementKeyValid,
		"guardrails_available": caps.GuardrailsAvailable,
		"detail":               caps.Detail,
	})
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
