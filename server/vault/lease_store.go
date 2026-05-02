package vault

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// ErrLeaseNotFound is returned by LeaseStore lookups when the lease ID is
// unknown — either it never existed or the sweeper already retired it.
var ErrLeaseNotFound = errors.New("lease not found")

// LeaseStore is the in-memory record of which device currently has the
// agent-side claim on which account. It exists so the vault's refresh
// scheduler can skip accounts that an agent is actively injecting (Claude
// Code rotates those tokens itself; running our own refresh in parallel
// would race the one-time-use refresh_token).
//
// Step 2 uses it for combined-mode bookkeeping and the HTTP boundary —
// uniqueness across devices is NOT enforced yet (Step 4 will). Persistence
// is also Step 4's job; restarting the daemon clears all leases, which is
// safe because every agent re-acquires on its first reconcile.
type LeaseStore struct {
	mu     sync.Mutex
	byID   map[string]*leaseEntry          // leaseID → entry
	byDev  map[string]string                // deviceID → leaseID (one per device)
	now    func() time.Time                 // overridable in tests
}

type leaseEntry struct {
	ID        string
	AccountID int64
	DeviceID  string
	ExpiresAt time.Time
}

// NewLeaseStore returns an empty store. The sweeper does not run by default;
// callers wire it to their context via Sweep when they want stale-lease GC.
func NewLeaseStore() *LeaseStore {
	return &LeaseStore{
		byID:  make(map[string]*leaseEntry),
		byDev: make(map[string]string),
		now:   time.Now,
	}
}

// Acquire records (or replaces) the lease for deviceID. Returning the new
// Lease handle is the only way callers learn the lease ID, so they hold it
// for later Renew / Release calls.
//
// Acquire is intentionally permissive: a device acquiring while it already
// holds a different lease silently retires the old one. This matches the
// agent's "pick a new account → switch" path, where the previous lease is
// no longer needed.
func (s *LeaseStore) Acquire(accountID int64, deviceID string, ttl time.Duration) Lease {
	id := newLeaseID()
	e := &leaseEntry{
		ID:        id,
		AccountID: accountID,
		DeviceID:  deviceID,
		ExpiresAt: s.now().Add(ttl),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.byDev[deviceID]; ok {
		delete(s.byID, prev)
	}
	s.byID[id] = e
	s.byDev[deviceID] = id
	return Lease{ID: id, AccountID: accountID, ExpiresAt: e.ExpiresAt.UnixMilli()}
}

// Renew bumps the TTL on an existing lease. Returns ErrLeaseNotFound when
// the lease has expired or been released.
func (s *LeaseStore) Renew(leaseID string, ttl time.Duration) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[leaseID]
	if !ok {
		return Lease{}, ErrLeaseNotFound
	}
	if !e.ExpiresAt.After(s.now()) {
		// Expired; treat as gone.
		delete(s.byID, leaseID)
		if s.byDev[e.DeviceID] == leaseID {
			delete(s.byDev, e.DeviceID)
		}
		return Lease{}, ErrLeaseNotFound
	}
	e.ExpiresAt = s.now().Add(ttl)
	return Lease{ID: e.ID, AccountID: e.AccountID, ExpiresAt: e.ExpiresAt.UnixMilli()}, nil
}

// Release removes the lease early. Idempotent — releasing an unknown lease
// is not an error.
func (s *LeaseStore) Release(leaseID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[leaseID]
	if !ok {
		return
	}
	delete(s.byID, leaseID)
	if s.byDev[e.DeviceID] == leaseID {
		delete(s.byDev, e.DeviceID)
	}
}

// IsLeased reports whether accountID currently has any non-expired lease.
// Used by the refresh scheduler to skip in-use accounts.
func (s *LeaseStore) IsLeased(accountID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for _, e := range s.byID {
		if e.AccountID == accountID && e.ExpiresAt.After(now) {
			return true
		}
	}
	return false
}

// Sweep removes all expired leases. Safe to call on a goroutine timer; the
// caller decides cadence.
func (s *LeaseStore) Sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for id, e := range s.byID {
		if !e.ExpiresAt.After(now) {
			delete(s.byID, id)
			if s.byDev[e.DeviceID] == id {
				delete(s.byDev, e.DeviceID)
			}
		}
	}
}

// newLeaseID returns a 128-bit random hex string. Collision probability with
// an in-memory store is comfortably negligible.
func newLeaseID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand failure is fatal-grade; falling back to a degenerate
		// time-based ID would mask the bug. Panic so the daemon crashes
		// loudly rather than silently producing predictable lease IDs.
		panic("vault: rand.Read failed: " + err.Error())
	}
	return hex.EncodeToString(buf[:])
}
