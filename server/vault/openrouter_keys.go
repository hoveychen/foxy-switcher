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
	DeriveDeviceKey(ctx context.Context, spec openrouter.DeriveSpec) (openrouter.Key, error)
	RevokeDerivedKey(ctx context.Context, keyHash string) error
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
			return &openrouter.Client{APIKey: managementKey}
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

	acc, cfg, cred, err := k.pickAccount(ctx)
	if err != nil {
		return OpenRouterGrant{}, err
	}

	// An ordinary (non-provisioning) key can't mint anything, so it is served
	// straight through. That is the whole point of supporting it: for a single
	// machine, deriving a key from a provisioning key buys nothing, and asking for
	// a provisioning key hands the daemon the power to add and delete every key on
	// the account just to solve "one machine needs one key".
	//
	// The cost, stated plainly in the grant and in the admin UI, is that every
	// authorised device shares this key — so revoking one device cannot revoke it.
	if !cred.IsProvisioning {
		return k.sharedGrant(acc, cfg, cred), nil
	}

	// Reuse an existing, unexpired key for the CHOSEN account. Keyed on acc.ID, so
	// rotating to a different account correctly derives a fresh key rather than
	// re-serving the drained account's one.
	//
	// The old account's key is deliberately left alive: it is capped, it can't
	// spend on an account with no money, and keeping it makes rotating back
	// instant if the operator tops that account up. It stays revocable — a device
	// revoke walks every row for the device, whichever account each came from.
	//
	// Config edits don't have to be detected here: changing an account's policy
	// revokes that account's keys up front (see RevokeAccountKeys), so a surviving
	// row is by definition current.
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

	mgmtKey := cred.APIKey
	// The model list is NOT sent upstream: it drives the device's profile files,
	// which is what makes a model appear in the picker. What bounds cost is the
	// key's own spend cap, below.
	derived, err := k.newClient(mgmtKey).DeriveDeviceKey(ctx, openrouter.DeriveSpec{
		KeyName:     openRouterKeyName(deviceID),
		LimitUSD:    cfg.LimitUSD,
		LimitReset:  cfg.LimitReset,
		WorkspaceID: cfg.WorkspaceID,
	})
	if err != nil {
		return OpenRouterGrant{}, fmt.Errorf("derive OpenRouter key for device %s: %w", deviceID, err)
	}

	row := store.DeviceOpenRouterKey{
		DeviceID:  deviceID,
		AccountID: acc.ID,
		KeyHash:   derived.Hash,
		KeySecret: derived.Secret,
		ExpiresAt: derived.ExpiresAt,
	}
	if err := k.st.PutDeviceOpenRouterKey(ctx, row); err != nil {
		// We hold a live upstream key we're about to forget about. Kill it rather
		// than leak an unrevocable credential.
		cleanup := context.WithoutCancel(ctx)
		if rerr := k.newClient(mgmtKey).RevokeDerivedKey(cleanup, derived.Hash); rerr != nil {
			k.logger.Printf("[openrouter] LEAKED key %s for device %s: could not persist (%v) and could not revoke (%v)",
				derived.Hash, deviceID, err, rerr)
		}
		return OpenRouterGrant{}, err
	}
	k.logger.Printf("[openrouter] derived key for device %s from account %d (%s), %d model(s) offered",
		deviceID, acc.ID, acc.Name, len(cfg.AllowedModels))
	return k.grant(acc, cfg, row), nil
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
// account's spend cap changes (every outstanding key was minted with the old
// cap, so they must all go), and when the account is deleted.
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
	cred, err := k.st.OpenRouterCredential(ctx, accountID)
	if errors.Is(err, store.ErrNotFound) {
		// The management key is gone, so we can no longer revoke upstream. Say so
		// loudly and keep the row: dropping it would erase the only record that
		// this key exists, and it would keep working forever.
		return fmt.Errorf("cannot revoke OpenRouter key %s for device %s: account %d has no API key on file "+
			"(re-enter it, then retry)", row.KeyHash, row.DeviceID, accountID)
	}
	if err != nil {
		return err
	}
	if err := k.newClient(cred.APIKey).RevokeDerivedKey(ctx, row.KeyHash); err != nil {
		return fmt.Errorf("revoke OpenRouter key %s for device %s: %w", row.KeyHash, row.DeviceID, err)
	}
	if err := k.st.DeleteDeviceOpenRouterKey(ctx, row.DeviceID, row.AccountID); err != nil {
		return err
	}
	k.logger.Printf("[openrouter] revoked key %s for device %s (account %d)", row.KeyHash, row.DeviceID, accountID)
	return nil
}

// pickAccount chooses which OpenRouter account serves a device.
//
// Ordering is deliberately NOT the LRU selector: spreading devices across
// accounts would buy nothing and make a device's key hop between accounts on
// unrelated pool changes. The rule is the most boring stable one — lowest id
// first — so a device keeps using the same account across restarts.
//
// What the ordering IS combined with is eligibility, which is the selector's
// other job and does matter here: an account with no credit left is skipped, so
// the pool rolls onto the next funded one instead of handing out a key that will
// 402. That is the whole of "rotate when the money runs out" — no lease, no LRU,
// just skip the broke ones and keep the order stable.
//
// An account is skipped when it is paused, has no key on file, has no models
// configured (a working key with an empty picker is no use), has an unreadable
// config, or is out of credit. A never-polled balance counts as funded — see
// store.OpenRouterCredential.HasCredit for why "unknown" must not mean "broke".
func (k *OpenRouterKeys) pickAccount(ctx context.Context) (
	store.Account, store.OpenRouterAccountConfig, store.OpenRouterCredential, error,
) {
	none := func(err error) (store.Account, store.OpenRouterAccountConfig, store.OpenRouterCredential, error) {
		return store.Account{}, store.OpenRouterAccountConfig{}, store.OpenRouterCredential{}, err
	}
	accs, err := k.st.ListProvider(ctx, store.ProviderOpenRouter)
	if err != nil {
		return none(err)
	}
	sort.Slice(accs, func(i, j int) bool { return accs[i].ID < accs[j].ID })
	var skippedBroke int
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
			continue
		}
		cred, err := k.st.OpenRouterCredential(ctx, a.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return none(err)
		}
		if !cred.HasCredit() {
			// Say so per account: "OpenRouter stopped working" is much harder to
			// diagnose than "both accounts are out of money".
			k.logger.Printf("[openrouter] account %d (%s): skipping, $%.2f credit remaining (floor $%.2f)",
				a.ID, a.Name, cred.CreditRemaining, store.MinUsableCredit)
			skippedBroke++
			continue
		}
		return a, cfg, cred, nil
	}
	if skippedBroke > 0 {
		return none(fmt.Errorf("%w: %d account(s) skipped for insufficient credit",
			ErrNoOpenRouterAccount, skippedBroke))
	}
	return none(ErrNoOpenRouterAccount)
}

func (k *OpenRouterKeys) grant(acc store.Account, cfg store.OpenRouterAccountConfig, row store.DeviceOpenRouterKey) OpenRouterGrant {
	return OpenRouterGrant{
		AccountID:     acc.ID,
		AccountName:   acc.Name,
		APIKey:        row.KeySecret,
		BaseURL:       DefaultOpenRouterBaseURL,
		AllowedModels: cfg.AllowedModels,
		ExpiresAt:     row.ExpiresAt,
		DeviceScoped:  true,
	}
}

// sharedGrant hands out the account's own key, unmodified. Used when the stored
// credential can't mint (an ordinary API key), so there is nothing to derive.
//
// No device_openrouter_keys row is written: there is no per-device key to track
// and nothing this vault could revoke. DeviceScoped=false is how that reaches
// the UI, which must not imply a revocability it doesn't have.
func (k *OpenRouterKeys) sharedGrant(acc store.Account, cfg store.OpenRouterAccountConfig, cred store.OpenRouterCredential) OpenRouterGrant {
	return OpenRouterGrant{
		AccountID:     acc.ID,
		AccountName:   acc.Name,
		APIKey:        cred.APIKey,
		BaseURL:       DefaultOpenRouterBaseURL,
		AllowedModels: cfg.AllowedModels,
		DeviceScoped:  false,
	}
}

// openRouterKeyName is the upstream label for a device's key. Includes the
// device id so an operator reading OpenRouter's own key list can tell which
// machine a key belongs to without consulting the vault.
func openRouterKeyName(deviceID string) string {
	return "foxy-" + deviceID
}
