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

// Pick returns the LRU eligible account that no other device currently has
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
	return selector.PickWithFilter(ctx, s.st, now, func(a Account) bool {
		if deviceID == "" {
			return s.st.IsAccountLeased(a.ID)
		}
		return s.st.IsAccountLeasedByOther(a.ID, deviceID)
	})
}

func (s *InProc) MarkUsed(ctx context.Context, accountID int64) error {
	return s.st.MarkUsed(ctx, accountID)
}

func (s *InProc) UpdateTokens(ctx context.Context, accountID int64, accessToken, refreshToken string, expiresAt int64) error {
	return s.st.UpdateTokens(ctx, accountID, accessToken, refreshToken, expiresAt)
}

func (s *InProc) AcquireLease(ctx context.Context, accountID int64, deviceID string, ttl time.Duration) (Lease, error) {
	id := vaultauth.NewID()
	got, err := s.st.AcquireLease(ctx, id, accountID, deviceID, ttl)
	if err != nil {
		return Lease{}, err
	}
	return Lease{ID: got.ID, AccountID: got.AccountID, ExpiresAt: got.ExpiresAt}, nil
}

func (s *InProc) RenewLease(ctx context.Context, leaseID string, ttl time.Duration) (Lease, error) {
	got, err := s.st.RenewLease(ctx, leaseID, ttl)
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
