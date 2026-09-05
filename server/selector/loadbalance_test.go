package selector

import (
	"context"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
)

// TestPickBalancesSharedAccountsByHolderCount covers the shared-provider load
// balancing: when several devices may hold the same Codex account, the picker
// must prefer the least-crowded eligible account so devices spread across the
// pool instead of all piling onto whichever one has the most weekly runway.
func TestPickBalancesSharedAccountsByHolderCount(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	now := time.Now()

	mk := func(name string, sevenDay float64) *store.Account {
		a := &store.Account{
			Provider: store.ProviderCodex, Name: name, Email: name + "@x",
			AccessToken: "at-" + name, ExpiresAt: now.Add(2 * time.Hour).UnixMilli(),
		}
		if err := st.Upsert(ctx, a); err != nil {
			t.Fatalf("upsert %s: %v", name, err)
		}
		if err := st.SetUsage(ctx, a.ID,
			0, "2026-04-30T05:00:00Z",
			sevenDay, "2026-05-07T00:00:00Z",
			0, "2026-05-07T00:00:00Z", "",
		); err != nil {
			t.Fatalf("set usage %s: %v", name, err)
		}
		return a
	}
	// roomy has the most weekly runway, so plain ranking would always pick it.
	roomy := mk("roomy", 10)
	tight := mk("tight", 40)

	counts := map[int64]int{roomy.ID: 2}
	got, err := PickProviderWithOptions(ctx, st, store.ProviderCodex, now, PickOptions{LeaseCounts: counts})
	if err != nil {
		t.Fatalf("PickProviderWithOptions: %v", err)
	}
	if got.ID != tight.ID {
		t.Fatalf("picked account %d, want the unheld %d (load balancing must beat runway)", got.ID, tight.ID)
	}

	// Equal holder counts fall back to the existing runway ordering.
	counts = map[int64]int{roomy.ID: 1, tight.ID: 1}
	got, err = PickProviderWithOptions(ctx, st, store.ProviderCodex, now, PickOptions{LeaseCounts: counts})
	if err != nil {
		t.Fatalf("PickProviderWithOptions (tied): %v", err)
	}
	if got.ID != roomy.ID {
		t.Fatalf("picked account %d, want %d (equal load must fall through to runway)", got.ID, roomy.ID)
	}

	// The caller's own lease must not count as load — otherwise a device that
	// already holds `roomy` would be pushed off it on the next reconcile tick.
	// Callers supply foreign-only counts (see Store.ActiveLeaseCounts's
	// excludeDeviceID), so a self-held account shows up here as 0.
	counts = map[int64]int{}
	got, err = PickProviderWithOptions(ctx, st, store.ProviderCodex, now,
		PickOptions{DeviceID: "dev1", LeaseCounts: counts})
	if err != nil {
		t.Fatalf("PickProviderWithOptions (self-held): %v", err)
	}
	if got.ID != roomy.ID {
		t.Fatalf("picked account %d, want %d — the caller's own lease must not count as load", got.ID, roomy.ID)
	}
}

// TestPickWithoutLeaseCountsIsUnchanged pins that the Claude path, which passes
// no counts, keeps ranking purely by runway.
func TestPickWithoutLeaseCountsIsUnchanged(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	now := time.Now()

	mk := func(name string, sevenDay float64) *store.Account {
		a := &store.Account{Name: name, Email: name + "@x", AccessToken: "at-" + name,
			ExpiresAt: now.Add(2 * time.Hour).UnixMilli()}
		if err := st.Upsert(ctx, a); err != nil {
			t.Fatalf("upsert %s: %v", name, err)
		}
		if err := st.SetUsage(ctx, a.ID,
			0, "2026-04-30T05:00:00Z",
			sevenDay, "2026-05-07T00:00:00Z",
			0, "2026-05-07T00:00:00Z", "",
		); err != nil {
			t.Fatalf("set usage %s: %v", name, err)
		}
		return a
	}
	roomy := mk("roomy", 10)
	mk("tight", 40)

	got, err := Pick(ctx, st, now)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.ID != roomy.ID {
		t.Fatalf("picked account %d, want %d", got.ID, roomy.ID)
	}
}
