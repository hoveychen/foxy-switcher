package vault

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

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
