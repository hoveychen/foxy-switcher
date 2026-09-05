package vault

import (
	"context"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
)

// TestPickCodex_SharesAndBalances covers the Codex sharing contract end to end
// at the vault boundary: a Codex account held by another device stays pickable
// (it is not excluded the way a Claude one is), and while free accounts remain
// the picker hands each new device the least-crowded one.
func TestPickCodex_SharesAndBalances(t *testing.T) {
	st := openTestStore(t)
	svc := NewInProc(st)
	ctx := context.Background()
	now := time.Now()

	a := seedCodexAccount(t, st, "codex-a")
	b := seedCodexAccount(t, st, "codex-b")

	pickAndHold := func(device string) int64 {
		t.Helper()
		got, err := svc.PickProviderForDevice(ctx, now, device, store.ProviderCodex)
		if err != nil {
			t.Fatalf("%s pick: %v", device, err)
		}
		if _, err := svc.AcquireLease(ctx, got.ID, device, time.Minute); err != nil {
			t.Fatalf("%s acquire %d: %v", device, got.ID, err)
		}
		return got.ID
	}

	first := pickAndHold("device-1")
	second := pickAndHold("device-2")
	if first == second {
		t.Fatalf("device-2 got the same account %d as device-1; free accounts must be preferred", first)
	}

	// Pool is now one holder per account. A third device must still get one
	// (sharing) rather than ErrNoAvailable.
	third := pickAndHold("device-3")
	if third != a.ID && third != b.ID {
		t.Fatalf("device-3 got unexpected account %d", third)
	}

	// A device re-picking must stick to the account it already holds: its own
	// lease must not read as load against it.
	again, err := svc.PickProviderForDevice(ctx, now, "device-1", store.ProviderCodex)
	if err != nil {
		t.Fatalf("device-1 re-pick: %v", err)
	}
	if again.ID != first {
		t.Fatalf("device-1 re-pick moved from %d to %d; the caller's own lease must not count as load", first, again.ID)
	}
}

// TestPickCodex_AllHeldStillPicks is the starved-pool case: with a single Codex
// account already held by another device, a new device must still be handed it
// instead of being told the pool is empty.
func TestPickCodex_AllHeldStillPicks(t *testing.T) {
	st := openTestStore(t)
	svc := NewInProc(st)
	ctx := context.Background()

	only := seedCodexAccount(t, st, "codex-only")
	if _, err := svc.AcquireLease(ctx, only.ID, "device-1", time.Minute); err != nil {
		t.Fatalf("device-1 acquire: %v", err)
	}
	got, err := svc.PickProviderForDevice(ctx, time.Now(), "device-2", store.ProviderCodex)
	if err != nil {
		t.Fatalf("device-2 pick: %v", err)
	}
	if got.ID != only.ID {
		t.Fatalf("device-2 got %d, want the shared account %d", got.ID, only.ID)
	}
	if _, err := svc.AcquireLease(ctx, only.ID, "device-2", time.Minute); err != nil {
		t.Fatalf("device-2 acquire shared account: %v", err)
	}
}
