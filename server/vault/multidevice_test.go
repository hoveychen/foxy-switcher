package vault

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/selector"
	"github.com/hoveychen/foxy-switcher/server/store"
)

// seedAccount inserts an active, eligible account so selector.PickWithFilter
// has something to return.
func seedAccount(t *testing.T, st *store.Store, name string) *store.Account {
	t.Helper()
	a := &store.Account{
		Name:         name,
		Email:        name + "@x",
		AccessToken:  "at-" + name,
		RefreshToken: "rt-" + name,
		ExpiresAt:    time.Now().Add(2 * time.Hour).UnixMilli(),
		Status:       "active",
	}
	if err := st.Upsert(context.Background(), a); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	return a
}

// TestPick_ExcludesAccountsLeasedByOthers nails down the Step 4 invariant
// that vault.InProc.Pick must skip accounts another device already holds
// a live lease on. Without that, two agents would happily alternate
// injecting the same account and race the one-time-use refresh_token.
func TestPick_ExcludesAccountsLeasedByOthers(t *testing.T) {
	st := openTestStore(t)
	svc := NewInProc(st)
	ctx := context.Background()

	a := seedAccount(t, st, "alpha")
	b := seedAccount(t, st, "beta")

	// Device 1 grabs alpha.
	if _, err := svc.AcquireLease(ctx, a.ID, "device-1", time.Minute); err != nil {
		t.Fatalf("device-1 acquire: %v", err)
	}

	// Device 2's Pick must return beta — alpha is leased by device-1.
	got, err := svc.Pick(ctx, time.Now())
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got == nil || got.ID != b.ID {
		t.Fatalf("Pick: got %+v want id=%d", got, b.ID)
	}
}

// TestPickForDevice_AllowsOwnLeaseSkipsForeign nails down the
// multi-device-lease-visibility refinement: PickForDevice filters out
// leases held by OTHER devices but lets the caller's OWN lease pass
// through, so a single-account pool can keep re-Picking the same
// account on every reconcile tick (no false ErrNoAvailable when the
// caller already holds the only lease). Foreign-held leases stay
// excluded — same guarantee Step 4 added in TestPick_ExcludesAccountsLeasedByOthers.
func TestPickForDevice_AllowsOwnLeaseSkipsForeign(t *testing.T) {
	st := openTestStore(t)
	svc := NewInProc(st)
	ctx := context.Background()

	a := seedAccount(t, st, "alpha")
	b := seedAccount(t, st, "beta")

	// device-self holds alpha; device-other holds beta.
	if _, err := svc.AcquireLease(ctx, a.ID, "device-self", time.Minute); err != nil {
		t.Fatalf("self acquire alpha: %v", err)
	}
	if _, err := svc.AcquireLease(ctx, b.ID, "device-other", time.Minute); err != nil {
		t.Fatalf("other acquire beta: %v", err)
	}

	// PickForDevice("device-self") should be allowed to return alpha
	// (own lease, not a disqualifier) — selector's LRU tiebreak picks
	// the longest-since-used; both are unused so id-asc breaks the tie.
	got, err := svc.PickForDevice(ctx, time.Now(), "device-self")
	if err != nil {
		t.Fatalf("PickForDevice self: %v", err)
	}
	if got == nil || got.ID != a.ID {
		t.Fatalf("PickForDevice self: got %+v want id=%d (own lease should pass)", got, a.ID)
	}

	// PickForDevice("device-other") symmetrically gets beta.
	got, err = svc.PickForDevice(ctx, time.Now(), "device-other")
	if err != nil {
		t.Fatalf("PickForDevice other: %v", err)
	}
	if got == nil || got.ID != b.ID {
		t.Fatalf("PickForDevice other: got %+v want id=%d", got, b.ID)
	}

	// Empty deviceID falls back to legacy Pick semantics — every leased
	// account is excluded, so with only two accounts (both leased) the
	// result must be ErrNoAvailable.
	_, err = svc.PickForDevice(ctx, time.Now(), "")
	if !errors.Is(err, selector.ErrNoAvailable) {
		t.Errorf("PickForDevice(\"\"): got %v want ErrNoAvailable (legacy filter excludes own lease too)", err)
	}
}

// TestAcquireLease_ContestedReturnsErrLeaseLocked covers the "two agents
// race for the same account" scenario directly: device 2 must see
// ErrLeaseLocked rather than silently take over device 1's lease.
func TestAcquireLease_ContestedReturnsErrLeaseLocked(t *testing.T) {
	st := openTestStore(t)
	svc := NewInProc(st)
	ctx := context.Background()
	a := seedAccount(t, st, "alpha")

	if _, err := svc.AcquireLease(ctx, a.ID, "device-1", time.Minute); err != nil {
		t.Fatalf("device-1 acquire: %v", err)
	}
	_, err := svc.AcquireLease(ctx, a.ID, "device-2", time.Minute)
	if !errors.Is(err, ErrLeaseLocked) {
		t.Fatalf("device-2 acquire: got %v want ErrLeaseLocked", err)
	}
}

// TestAcquireLease_SameDeviceRenewsInPlace verifies the agent's reconcile
// loop can call AcquireLease on every tick without leaking lease IDs;
// same device + same account just refreshes the existing row.
func TestAcquireLease_SameDeviceRenewsInPlace(t *testing.T) {
	st := openTestStore(t)
	svc := NewInProc(st)
	ctx := context.Background()
	a := seedAccount(t, st, "alpha")

	first, err := svc.AcquireLease(ctx, a.ID, "device-1", time.Minute)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	second, err := svc.AcquireLease(ctx, a.ID, "device-1", time.Minute)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("same-device re-acquire produced new lease id: %s → %s", first.ID, second.ID)
	}
}

func TestSameDeviceCanLeaseClaudeAndCodexSimultaneously(t *testing.T) {
	st := openTestStore(t)
	svc := NewInProc(st)
	ctx := context.Background()
	claude := seedAccount(t, st, "claude")
	codex := &store.Account{
		Provider: store.ProviderCodex, Name: "codex", AccountUUID: "codex-id",
		AccessToken: "codex-at", RefreshToken: "codex-rt",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: store.StatusActive,
	}
	if err := st.Upsert(ctx, codex); err != nil {
		t.Fatal(err)
	}
	first, err := svc.AcquireLease(ctx, claude.ID, "device-both", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.AcquireLease(ctx, codex.ID, "device-both", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID || first.AccountID == second.AccountID {
		t.Fatalf("provider leases collapsed: %+v %+v", first, second)
	}
}

// TestSweepLeases_ReclaimsExpiredRows confirms the sweeper actually clears
// expired rows so the unique account_id index unblocks the next acquire.
func TestSweepLeases_ReclaimsExpiredRows(t *testing.T) {
	st := openTestStore(t)
	svc := NewInProc(st)
	ctx := context.Background()
	a := seedAccount(t, st, "alpha")

	if _, err := svc.AcquireLease(ctx, a.ID, "device-1", time.Millisecond); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Wait past TTL — short enough that the test stays fast.
	time.Sleep(10 * time.Millisecond)

	// Without sweep, the row is still present (expires_at <= now). After
	// sweep, the unique-index slot is free for a new device to claim.
	if err := svc.SweepLeases(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := svc.AcquireLease(ctx, a.ID, "device-2", time.Minute); err != nil {
		t.Fatalf("post-sweep acquire by another device: %v", err)
	}
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
