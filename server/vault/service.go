// Package vault is the boundary between the foxy-switcher account store +
// refresher (the "vault") and the local credential injector (the "agent").
//
// In combined mode (single binary, today's default) both sides share a
// process and Service is the in-process implementation in this package. In
// split mode (planned by docs/cloud-vault.md), the agent talks to a remote
// vault over HTTP — that path will land an httpclient implementation of
// the same interface in a sibling subpackage.
//
// Step 1 of the migration only covers the agent-facing subset: the methods
// credinject.Coordinator needs to choose, inject, mark-used, and reverse-
// sync. The frontend / TUI continue to talk to httpapi over HTTP because
// httpapi *is* the vault's HTTP surface — there's nothing to abstract for
// callers that already cross a network.
package vault

import (
	"context"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
)

// Account is the credential row passed across the boundary. It mirrors
// store.Account exactly today; we re-export via type alias so the agent and
// vault binaries don't both need to import the SQLite-flavoured store
// package once they're separated. When the wire format diverges from the
// schema (e.g. for redaction or version skew) this becomes its own struct.
type Account = store.Account

// AutoSwitch is the daemon-wide auto-rotation toggle. Re-exported for the
// same reason as Account.
type AutoSwitch = store.AutoSwitch

// Lease is the agent's claim on an account: while a lease is live, the
// vault's refresh scheduler skips that account because the agent is
// actively injecting it (Claude Code rotates those tokens itself; parallel
// refreshes would race the one-time-use refresh_token).
//
// Step 2 leases live in memory and are NOT enforced for uniqueness across
// devices. Step 4 adds persistence + uniqueness so two devices can't claim
// the same account.
type Lease struct {
	ID        string // opaque, returned by AcquireLease
	AccountID int64
	ExpiresAt int64 // unix millis
}

// DefaultIdleReclaimThreshold is how long a lease's holder must have gone
// without real local Claude Code activity before the lease becomes reclaimable
// — i.e. an active, pool-starved device may preempt it. Chosen comfortably
// above normal "reading the output / thinking" pauses so a brief idle never
// costs a user their slot (reclaim also only fires under genuine pool
// exhaustion), yet short enough to free machines left running unused. Both the
// vault (reclaim decision) and the agent (its "am I active enough to acquire?"
// gate) key off this single value so the two sides never disagree about which
// leases are idle.
const DefaultIdleReclaimThreshold = 10 * time.Minute

// Service is what credinject.Coordinator depends on. It's intentionally
// small: only the operations the coordinator's choose / reconcile / reverse-
// sync paths perform. Future steps will widen this surface as more agent-
// side concerns get pushed across the boundary (event subscription, etc).
type Service interface {
	// ListAccounts returns every account in the pool. Used by the
	// coordinator's stickiness check (find the current account + spot any
	// LastUsedAt==0 sentinel that means "user pinned this one").
	ListAccounts(ctx context.Context) ([]Account, error)

	// GetAutoSwitch returns the daemon-wide rotation policy. The coordinator
	// branches on Enabled to choose between auto-LRU and manual stickiness.
	GetAutoSwitch(ctx context.Context) (AutoSwitch, error)

	// Pick returns the next account the coordinator should inject, applying
	// the selector's eligibility predicate (status, expiry, threshold).
	// Returns selector.ErrNoAvailable when no account qualifies — the
	// coordinator's caller treats that as the trigger to restore the user's
	// native creds. Pick does NOT mutate any state; the coordinator calls
	// MarkUsed after a successful inject so the LRU clock advances.
	//
	// Lease semantics: filters out every leased account (own + foreign).
	// Most callers should prefer PickForDevice — Pick is kept for
	// back-compat with code paths that don't yet plumb device identity.
	Pick(ctx context.Context, now time.Time) (*Account, error)

	// PickForDevice is Pick with caller-device awareness: foreign leases
	// (held by other devices) are still excluded, but the caller's own
	// lease passes through so a single-account pool can keep re-picking
	// the held account on rotation. deviceID "" behaves as Pick.
	//
	// In agent mode the httpclient impl forwards the request to vault
	// without sending deviceID — the server reads BearerAuth ctx and
	// substitutes the caller's device id automatically.
	PickForDevice(ctx context.Context, now time.Time, deviceID string) (*Account, error)

	// PickProviderForDevice is the provider-aware form used by agents that
	// manage Claude and Codex simultaneously. Legacy Pick/PickForDevice keep
	// selecting Claude so older agents never receive a Codex credential.
	PickProviderForDevice(ctx context.Context, now time.Time, deviceID, provider string) (*Account, error)

	// MarkUsed bumps last_used_at on the account so the LRU tiebreaker
	// reflects real pool usage. Called after a switch lands successfully.
	MarkUsed(ctx context.Context, accountID int64) error

	// UpdateTokens overwrites the access/refresh/expires columns for one
	// account. The coordinator's reverse-sync calls this after observing
	// Claude Code rotate the keychain blob behind us. The pattern matches
	// store.UpdateTokens — keeping the names parallel avoids gratuitous
	// translation between the two surfaces.
	UpdateTokens(ctx context.Context, accountID int64, accessToken, refreshToken string, expiresAt int64) error

	// UpdateProviderCredential also persists the provider-native document
	// (Codex auth JSON) observed after the local CLI rotates credentials.
	UpdateProviderCredential(ctx context.Context, accountID int64, accessToken, refreshToken string, expiresAt int64, credentialJSON string) error

	// AcquireLease records that a device intends to inject accountID. The
	// returned Lease ID is what callers later pass to Renew / Release. A
	// device that already holds a lease silently replaces it — matching the
	// "pick a new account → switch" path where the old lease is now stale.
	AcquireLease(ctx context.Context, accountID int64, deviceID string, ttl time.Duration) (Lease, error)

	// RenewLease bumps the TTL on an existing lease. Returns ErrLeaseNotFound
	// when the lease has already expired or been released — caller should
	// re-AcquireLease in that case. Note: ErrLeaseNotFound now also covers
	// "the vault reclaimed this idle lease under pool pressure", so an idle
	// agent must NOT blindly re-acquire on it (see credinject.refreshLease).
	//
	// idleFor is how long the caller has gone without real local Claude Code
	// activity (0 == active). The vault records it so a live-but-idle lease can
	// be told apart from one in active use and reclaimed under pressure.
	RenewLease(ctx context.Context, leaseID string, ttl, idleFor time.Duration) (Lease, error)

	// ReleaseLease removes the lease early. Idempotent.
	ReleaseLease(ctx context.Context, leaseID string) error
}
