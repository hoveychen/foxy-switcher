package vault

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/hoveychen/foxy-switcher/server/openrouter"
	"github.com/hoveychen/foxy-switcher/server/selector"
	"github.com/hoveychen/foxy-switcher/server/store"
)

// OpenRouterKeys mints and revokes the per-device runtime keys described by
// store.ProviderOpenRouter. It is vault-internal by construction: it holds the
// management key path and decides *for a given device id* what that device may
// have. Nothing on this type is reachable through vault.Service, so a remote
// agent can never drive derivation on behalf of another device.
type OpenRouterKeys struct {
	st     *store.Store
	logger *log.Logger

	// newClient builds a management client. A field rather than a direct
	// constructor call so tests can substitute a fake upstream without standing
	// up an HTTP server for every case.
	newClient func(managementKey string) OpenRouterAPI
}

// OpenRouterAPI is the slice of the openrouter client this service uses.
type OpenRouterAPI interface {
	DeriveDeviceKey(ctx context.Context, spec openrouter.DeriveSpec) (openrouter.DerivedKey, error)
	RevokeDerivedKey(ctx context.Context, keyHash, guardrailID string) error
}

// NewOpenRouterKeys returns the production service, talking to the real
// OpenRouter management API.
func NewOpenRouterKeys(st *store.Store, logger *log.Logger) *OpenRouterKeys {
	if logger == nil {
		logger = log.Default()
	}
	return &OpenRouterKeys{
		st:     st,
		logger: logger,
		newClient: func(managementKey string) OpenRouterAPI {
			return &openrouter.Client{ManagementKey: managementKey}
		},
	}
}

// SetClientFactory swaps the management-client constructor. Exported for tests
// in other packages that need the real derivation logic against a fake
// upstream; production code never calls it.
func (k *OpenRouterKeys) SetClientFactory(f func(managementKey string) OpenRouterAPI) {
	k.newClient = f
}

// ErrNoOpenRouterAccount means no OpenRouter account is usable right now:
// none exists, none is active, or none has both a management key and a model
// allowlist configured. Distinct from "this device isn't allowed OpenRouter",
// which surfaces as selector.ErrNoAvailable, because the operator fix is
// different (configure an account vs grant the device).
var ErrNoOpenRouterAccount = errors.New("no configured OpenRouter account is available")

// EnsureDeviceKey returns the device's OpenRouter grant, deriving one on first
// call and reusing it afterwards. Idempotent and cheap on the steady-state
// path: a device that already holds a live key gets a pure database read, no
// upstream call. That matters because the design deliberately keeps OpenRouter
// out of the agent's reconcile loop — this is called on authorisation changes,
// not every few seconds.
func (k *OpenRouterKeys) EnsureDeviceKey(ctx context.Context, deviceID string) (OpenRouterGrant, error) {
	if deviceID == "" {
		return OpenRouterGrant{}, fmt.Errorf("device id required")
	}
	allowed, err := k.st.DeviceAllowsProvider(ctx, deviceID, store.ProviderOpenRouter)
	if err != nil {
		return OpenRouterGrant{}, err
	}
	if !allowed {
		// Same signal the Claude/Codex pools use for "not for you" so callers
		// have one not-available branch rather than a per-provider zoo.
		return OpenRouterGrant{}, selector.ErrNoAvailable
	}

	acc, cfg, err := k.pickAccount(ctx)
	if err != nil {
		return OpenRouterGrant{}, err
	}

	// Reuse an existing, unexpired key. Config edits don't have to be detected
	// here: changing an account's allowlist revokes that account's keys up front
	// (see RevokeAccountKeys), so a surviving row is by definition current.
	if existing, err := k.st.DeviceOpenRouterKeyFor(ctx, deviceID, acc.ID); err == nil {
		if !existing.Expired(time.Now()) && existing.KeySecret != "" {
			return k.grant(acc, cfg, *existing), nil
		}
		// Expired or unusable — kill it upstream before minting a replacement so
		// we don't accumulate dead keys on the OpenRouter account.
		if err := k.revokeOne(ctx, acc.ID, *existing); err != nil {
			return OpenRouterGrant{}, err
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return OpenRouterGrant{}, err
	}

	mgmtKey, err := k.st.OpenRouterManagementKey(ctx, acc.ID)
	if err != nil {
		return OpenRouterGrant{}, fmt.Errorf("account %d: %w", acc.ID, err)
	}
	derived, err := k.newClient(mgmtKey).DeriveDeviceKey(ctx, openrouter.DeriveSpec{
		KeyName:       openRouterKeyName(deviceID),
		GuardrailName: openRouterKeyName(deviceID),
		AllowedModels: cfg.AllowedModels,
		LimitUSD:      cfg.LimitUSD,
		LimitReset:    cfg.LimitReset,
		WorkspaceID:   cfg.WorkspaceID,
		// Degrading to an advisory allowlist is a deployment decision, not one
		// this code should make silently. It stays off until an operator has run
		// the capability probe and accepted the trade-off.
		AllowUnenforcedModels: false,
	})
	if err != nil {
		return OpenRouterGrant{}, fmt.Errorf("derive OpenRouter key for device %s: %w", deviceID, err)
	}

	row := store.DeviceOpenRouterKey{
		DeviceID:    deviceID,
		AccountID:   acc.ID,
		KeyHash:     derived.Hash,
		KeySecret:   derived.Secret,
		GuardrailID: derived.GuardrailID,
		ExpiresAt:   derived.ExpiresAt,
	}
	if err := k.st.PutDeviceOpenRouterKey(ctx, row); err != nil {
		// We hold a live upstream key we're about to forget about. Kill it rather
		// than leak an unrevocable credential.
		cleanup := context.WithoutCancel(ctx)
		if rerr := k.newClient(mgmtKey).RevokeDerivedKey(cleanup, derived.Hash, derived.GuardrailID); rerr != nil {
			k.logger.Printf("[openrouter] LEAKED key %s for device %s: could not persist (%v) and could not revoke (%v)",
				derived.Hash, deviceID, err, rerr)
		}
		return OpenRouterGrant{}, err
	}
	k.logger.Printf("[openrouter] derived key for device %s from account %d (%s), %d model(s), guardrail_enforced=%v",
		deviceID, acc.ID, acc.Name, len(cfg.AllowedModels), derived.GuardrailEnforced)

	grant := k.grant(acc, cfg, row)
	grant.GuardrailEnforced = derived.GuardrailEnforced
	return grant, nil
}

// RevokeDeviceKeys kills every runtime key minted for a device. Called when a
// device is revoked, suspended, or has its OpenRouter grant withdrawn.
//
// This is not optional bookkeeping. A derived key talks to OpenRouter directly:
// it never passes through the vault's bearer auth, so suspending or revoking
// the device does nothing to it. Without this call "suspended" would leave a
// fully working credential on the machine.
//
// A row is deleted only after its upstream key is gone, so a failed revoke
// retries on the next attempt instead of being forgotten.
func (k *OpenRouterKeys) RevokeDeviceKeys(ctx context.Context, deviceID string) error {
	rows, err := k.st.ListDeviceOpenRouterKeys(ctx, deviceID)
	if err != nil {
		return err
	}
	var failures []error
	for _, row := range rows {
		if err := k.revokeOne(ctx, row.AccountID, row); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// RevokeAccountKeys kills every key derived from one account. Called when the
// account's allowlist or spend cap changes (every outstanding key encodes the
// old policy in its guardrail, so they must all go), and when the account is
// deleted.
func (k *OpenRouterKeys) RevokeAccountKeys(ctx context.Context, accountID int64) error {
	rows, err := k.st.ListAccountOpenRouterKeys(ctx, accountID)
	if err != nil {
		return err
	}
	var failures []error
	for _, row := range rows {
		if err := k.revokeOne(ctx, accountID, row); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// revokeOne deletes a single key upstream and then locally.
func (k *OpenRouterKeys) revokeOne(ctx context.Context, accountID int64, row store.DeviceOpenRouterKey) error {
	mgmtKey, err := k.st.OpenRouterManagementKey(ctx, accountID)
	if errors.Is(err, store.ErrNotFound) {
		// The management key is gone, so we can no longer revoke upstream. Say so
		// loudly and keep the row: dropping it would erase the only record that
		// this key exists, and it would keep working forever.
		return fmt.Errorf("cannot revoke OpenRouter key %s for device %s: account %d has no management key on file "+
			"(re-enter it, then retry)", row.KeyHash, row.DeviceID, accountID)
	}
	if err != nil {
		return err
	}
	if err := k.newClient(mgmtKey).RevokeDerivedKey(ctx, row.KeyHash, row.GuardrailID); err != nil {
		return fmt.Errorf("revoke OpenRouter key %s for device %s: %w", row.KeyHash, row.DeviceID, err)
	}
	if err := k.st.DeleteDeviceOpenRouterKey(ctx, row.DeviceID, row.AccountID); err != nil {
		return err
	}
	k.logger.Printf("[openrouter] revoked key %s for device %s (account %d)", row.KeyHash, row.DeviceID, accountID)
	return nil
}

// pickAccount chooses which OpenRouter account a device derives from.
//
// Selection is deliberately NOT the LRU selector: OpenRouter accounts aren't a
// scarce rotating resource, so "spread devices across accounts" would buy
// nothing and make a device's key jump between accounts on unrelated pool
// changes. The rule is instead the most boring stable one — lowest id among
// fully configured, active accounts — so a device keeps deriving from the same
// account across restarts. Multi-account OpenRouter pools are therefore
// "several configured, the first one wins", not a load-balanced pool; the
// design contract doesn't ask for more than that.
func (k *OpenRouterKeys) pickAccount(ctx context.Context) (store.Account, store.OpenRouterAccountConfig, error) {
	accs, err := k.st.ListProvider(ctx, store.ProviderOpenRouter)
	if err != nil {
		return store.Account{}, store.OpenRouterAccountConfig{}, err
	}
	sort.Slice(accs, func(i, j int) bool { return accs[i].ID < accs[j].ID })
	for _, a := range accs {
		if a.Status != store.StatusActive {
			continue
		}
		cfg, err := store.ParseOpenRouterConfig(a.CredentialJSON)
		if err != nil {
			k.logger.Printf("[openrouter] account %d (%s): unreadable config, skipping: %v", a.ID, a.Name, err)
			continue
		}
		if len(cfg.AllowedModels) == 0 {
			// An empty allowlist would mint a key with no guardrail and no models
			// to offer — a device would get a working key and an empty dropdown.
			continue
		}
		has, err := k.st.HasOpenRouterManagementKey(ctx, a.ID)
		if err != nil {
			return store.Account{}, store.OpenRouterAccountConfig{}, err
		}
		if !has {
			continue
		}
		return a, cfg, nil
	}
	return store.Account{}, store.OpenRouterAccountConfig{}, ErrNoOpenRouterAccount
}

func (k *OpenRouterKeys) grant(acc store.Account, cfg store.OpenRouterAccountConfig, row store.DeviceOpenRouterKey) OpenRouterGrant {
	return OpenRouterGrant{
		AccountID:     acc.ID,
		AccountName:   acc.Name,
		APIKey:        row.KeySecret,
		BaseURL:       DefaultOpenRouterBaseURL,
		AllowedModels: cfg.AllowedModels,
		ExpiresAt:     row.ExpiresAt,
		// A stored row that carries a guardrail id was enforced when minted.
		// Derivation refuses the unenforced path outright today, so this is
		// belt-and-braces rather than the only signal.
		GuardrailEnforced: row.GuardrailID != "",
	}
}

// openRouterKeyName is the upstream label for a device's key. Includes the
// device id so an operator reading OpenRouter's own key list can tell which
// machine a key belongs to without consulting the vault.
func openRouterKeyName(deviceID string) string {
	return "foxy-" + deviceID
}
