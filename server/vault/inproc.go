package vault

import (
	"context"
	"errors"
	"time"

	"github.com/hoveychen/foxy-switcher/server/selector"
	"github.com/hoveychen/foxy-switcher/server/store"
	vaultauth "github.com/hoveychen/foxy-switcher/server/vault/auth"
)

// ErrLeaseNotFound mirrors store.ErrNotFound for lease lookups so callers
// in non-store code don't have to import store just to errors.Is.
var ErrLeaseNotFound = errors.New("lease not found")

// ErrLeaseLocked is what AcquireLease returns when another device already
// holds a live lease on the account. The httpserver maps this to 409 so
// the agent can distinguish "try a different account" from generic 5xx.
var ErrLeaseLocked = store.ErrLeaseLocked

// InProc is the in-process implementation of Service. It delegates to
// store + selector with a small amount of glue (lease-aware Pick, lease
// id generation). Combined-mode callers pay only the cost of a SQLite
// query; agent-mode callers go through httpserver/httpclient.
type InProc struct {
	st *store.Store
}

// NewInProc returns a Service backed directly by the given store. Step 4
// migrated leases from a separate in-memory store to the same SQLite
// database — the constructor argument shape stays the same.
func NewInProc(st *store.Store) *InProc {
	return &InProc{st: st}
}

// Compile-time assertion that InProc satisfies Service.
var _ Service = (*InProc)(nil)

func (s *InProc) ListAccounts(ctx context.Context) ([]Account, error) {
	return s.st.List(ctx)
}

func (s *InProc) GetAutoSwitch(ctx context.Context) (AutoSwitch, error) {
	return s.st.GetAutoSwitch(ctx)
}

// Pick returns the eligible account with the most weekly runway (lowest
// 7-day utilization, LRU-tiebroken) that no other device currently has
// a live lease on. Same-device-leased accounts are NOT excluded — the
// caller's sticky path (in credinject.choose) wants to keep using its own
// account, and treating its own lease as a disqualifier would force a
// pointless rotation.
//
// Step 4 added the lease filter; the rest matches selector.Pick from
// earlier steps.
func (s *InProc) Pick(ctx context.Context, now time.Time) (*Account, error) {
	return s.PickForDevice(ctx, now, "")
}

// PickForDevice is Pick with caller-device awareness: when deviceID is
// non-empty, the lease filter excludes only OTHER devices' leases so
// the caller can re-pick its own lease (allowing renewal on a single-
// account pool). deviceID == "" preserves Pick's legacy "filter every
// leased account" behaviour for callers that don't yet plumb device
// identity through.
func (s *InProc) PickForDevice(ctx context.Context, now time.Time, deviceID string) (*Account, error) {
	return s.PickProviderForDevice(ctx, now, deviceID, store.ProviderClaude)
}

// PickProviderForDevice picks from one provider pool for one device. Providers
// split into two lease regimes here: exclusive ones (Claude) filter out
// accounts another device holds and fall back to idle-reclaim when that empties
// the pool, while shared ones (Codex) let devices co-hold an account and merely
// balance across the pool by holder count.
func (s *InProc) PickProviderForDevice(ctx context.Context, now time.Time, deviceID, provider string) (*Account, error) {
	// Per-device provider allowlist: a paired device may only lease the
	// providers the admin granted it (at approval or in the devices page).
	// deviceID == "" is combined/local mode, which has no pairing and is not
	// gated. A disallowed provider looks like an empty pool to the caller —
	// its provider manager then restores the local creds and injects nothing.
	if deviceID != "" {
		allowed, err := s.st.DeviceAllowsProvider(ctx, deviceID, provider)
		if err != nil {
			return nil, err
		}
		if !allowed {
			return nil, selector.ErrNoAvailable
		}
	}
	// Shared providers (Codex) let several devices hold one account, so a
	// foreign lease is not a disqualifier — it's just load. Rank by how many
	// OTHER devices are on each account (least crowded first) and skip the
	// idle-reclaim second pass entirely: there is nothing to reclaim when the
	// account can simply be co-held.
	if store.ProviderSharesAccounts(provider) {
		counts, err := s.st.ActiveLeaseCounts(ctx, deviceID)
		if err != nil {
			return nil, err
		}
		return selector.PickProviderWithOptions(ctx, s.st, provider, now,
			selector.PickOptions{DeviceID: deviceID, LeaseCounts: counts})
	}

	acc, err := selector.PickProviderWithFilter(ctx, s.st, provider, now, deviceID, func(a Account) bool {
		if deviceID == "" {
			return s.st.IsAccountLeased(a.ID)
		}
		return s.st.IsAccountLeasedByOther(a.ID, deviceID)
	})
	if err == nil || !errors.Is(err, selector.ErrNoAvailable) {
		return acc, err
	}
	// Pool exhausted — every eligible account is leased. This is the ONLY
	// trigger for idle-reclaim: run a second pass whose filter treats a foreign
	// lease as a disqualifier only while its holder is still active (reported
	// activity within DefaultIdleReclaimThreshold). An account held solely by an
	// idle device therefore becomes pickable, and the selector still ranks by
	// eligibility so a hard-capped account is never handed out. Combined/local
	// mode (deviceID == "") never reclaims — there are no competing devices.
	if deviceID == "" {
		return acc, err
	}
	thresholdMs := DefaultIdleReclaimThreshold.Milliseconds()
	acc, err2 := selector.PickProviderWithFilter(ctx, s.st, provider, now, deviceID, func(a Account) bool {
		return s.st.IsAccountLeasedByOtherActive(a.ID, deviceID, thresholdMs)
	})
	if err2 != nil {
		// Still nothing reclaimable — surface the original ErrNoAvailable so the
		// caller's no-available handling (hold keychain, retry next tick) runs.
		return nil, err
	}
	// Free the idle foreign lease on the chosen account (no-op when the account
	// was simply unleased) so the caller's AcquireLease can take it over.
	if _, rerr := s.st.ReclaimIdleForeignLease(ctx, acc.ID, deviceID, thresholdMs); rerr != nil {
		return nil, rerr
	}
	return acc, nil
}

func (s *InProc) MarkUsed(ctx context.Context, accountID int64) error {
	return s.st.MarkUsed(ctx, accountID)
}

func (s *InProc) UpdateTokens(ctx context.Context, accountID int64, accessToken, refreshToken string, expiresAt int64) error {
	return s.st.UpdateTokens(ctx, accountID, accessToken, refreshToken, expiresAt)
}

func (s *InProc) UpdateProviderCredential(ctx context.Context, accountID int64, accessToken, refreshToken string, expiresAt int64, credentialJSON string) error {
	return s.st.UpdateProviderCredential(ctx, accountID, accessToken, refreshToken, expiresAt, credentialJSON)
}

func (s *InProc) AcquireLease(ctx context.Context, accountID int64, deviceID string, ttl time.Duration) (Lease, error) {
	// Enforce the per-device provider allowlist here too, not just in
	// PickProviderForDevice: the agent's sticky-selection path re-acquires a
	// lease on its current account without re-picking, so a device whose
	// provider was revoked could otherwise keep re-leasing it. deviceID == ""
	// (combined/local) and unpaired ids are un-gated (DeviceAllowsProvider
	// returns true on a missing row). ErrNoAvailable is the signal the agent's
	// provider manager treats as "restore local creds, inject nothing".
	if deviceID != "" {
		acc, err := s.st.Get(ctx, accountID)
		if err != nil {
			return Lease{}, err
		}
		allowed, err := s.st.DeviceAllowsProvider(ctx, deviceID, acc.Provider)
		if err != nil {
			return Lease{}, err
		}
		if !allowed {
			return Lease{}, selector.ErrNoAvailable
		}
	}
	id := vaultauth.NewID()
	got, err := s.st.AcquireLease(ctx, id, accountID, deviceID, ttl)
	if err != nil {
		return Lease{}, err
	}
	return Lease{ID: got.ID, AccountID: got.AccountID, ExpiresAt: got.ExpiresAt}, nil
}

func (s *InProc) RenewLease(ctx context.Context, leaseID string, ttl, idleFor time.Duration) (Lease, error) {
	got, err := s.st.RenewLease(ctx, leaseID, ttl, idleFor)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Lease{}, ErrLeaseNotFound
		}
		return Lease{}, err
	}
	return Lease{ID: got.ID, AccountID: got.AccountID, ExpiresAt: got.ExpiresAt}, nil
}

func (s *InProc) ReleaseLease(ctx context.Context, leaseID string) error {
	return s.st.ReleaseLease(ctx, leaseID)
}

// SweepLeases is a vault-internal job — main wires it into a goroutine
// timer so expired rows don't accumulate. Exposed here (not on Service)
// because remote agents have no business sweeping the vault's tables.
func (s *InProc) SweepLeases(ctx context.Context) error {
	return s.st.SweepLeases(ctx)
}
