package store

import (
	"context"
	"testing"
	"time"
)

// TestAcquireLease_OneLeasePerDeviceProvider nails down the invariant that a
// single device_id holds at most one live lease PER PROVIDER. When one physical
// host runs two agent processes that share the same pair-assigned device_id
// (observed 2026-07-20: device "loki" held cws_pool_02 AND cws_pool_05, both
// Claude, both actively renewed), the accounts UI showed the device on two
// cards and two pool accounts were consumed by one host. The coordinator's
// release-old-after-acquire dedup is per-process, so two processes can't see
// each other — the invariant has to be enforced server-side, inside the same
// AcquireLease transaction.
//
// Guard scope is per-provider: a device legitimately managing Claude AND Codex
// at once (PickProviderForDevice) must keep both leases; acquiring a Claude
// account must not drop that device's Codex lease.
func TestAcquireLease_OneLeasePerDeviceProvider(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	mkAcc := func(name, provider string) *Account {
		a := &Account{Provider: provider, Name: name, AccessToken: "at-" + name, RefreshToken: "rt-" + name}
		if err := st.Upsert(ctx, a); err != nil {
			t.Fatalf("upsert %s: %v", name, err)
		}
		return a
	}
	claudeA := mkAcc("claudeA", ProviderClaude)
	claudeB := mkAcc("claudeB", ProviderClaude)
	codexC := mkAcc("codexC", ProviderCodex)
	claudeE := mkAcc("claudeE", ProviderClaude)

	// Same host, one shared device_id, running two agent processes.
	const dev = "loki-device"
	const other = "other-device"

	// Process 1 grabs claudeA.
	if _, err := st.AcquireLease(ctx, "lease-a", claudeA.ID, dev, time.Minute); err != nil {
		t.Fatalf("acquire claudeA: %v", err)
	}
	// A different device grabs claudeE — must never be touched by the guard.
	if _, err := st.AcquireLease(ctx, "lease-e", claudeE.ID, other, time.Minute); err != nil {
		t.Fatalf("acquire claudeE (other device): %v", err)
	}

	// Process 2 (same device_id) grabs claudeB. The guard must release the
	// device's OTHER live Claude lease (claudeA) so the host holds exactly one
	// Claude account.
	if _, err := st.AcquireLease(ctx, "lease-b", claudeB.ID, dev, time.Minute); err != nil {
		t.Fatalf("acquire claudeB: %v", err)
	}
	if st.IsAccountLeased(claudeA.ID) {
		t.Fatalf("claudeA still leased: acquiring claudeB on the same device+provider must have released claudeA")
	}
	if !st.IsAccountLeased(claudeB.ID) {
		t.Fatalf("claudeB not leased after acquire")
	}
	if !st.IsAccountLeased(claudeE.ID) {
		t.Fatalf("claudeE (other device) was released — guard must be scoped to the acquiring device only")
	}

	// The released lease's attribution segment must be closed, not left open,
	// so per-device usage replay stops counting claudeA.
	var open int
	if err := st.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM lease_events WHERE lease_id = 'lease-a' AND ended_at = 0`).
		Scan(&open); err != nil {
		t.Fatalf("count open lease_events for lease-a: %v", err)
	}
	if open != 0 {
		t.Fatalf("lease-a still has %d open lease_events segment(s); guard-released leases must close their segment", open)
	}

	// Cross-provider: the same device grabs a Codex account. Its live Claude
	// lease (claudeB) must survive — the guard is per-provider.
	if _, err := st.AcquireLease(ctx, "lease-c", codexC.ID, dev, time.Minute); err != nil {
		t.Fatalf("acquire codexC: %v", err)
	}
	if !st.IsAccountLeased(claudeB.ID) {
		t.Fatalf("claudeB (Claude) released by acquiring a Codex account — guard must not cross providers")
	}
	if !st.IsAccountLeased(codexC.ID) {
		t.Fatalf("codexC not leased after acquire")
	}

	// Re-acquiring the SAME account this device already holds is a renew, not a
	// self-eviction: claudeB must stay leased.
	if _, err := st.AcquireLease(ctx, "lease-b2", claudeB.ID, dev, time.Minute); err != nil {
		t.Fatalf("re-acquire claudeB: %v", err)
	}
	if !st.IsAccountLeased(claudeB.ID) {
		t.Fatalf("claudeB dropped by re-acquiring itself — the guard must exclude the account being acquired")
	}
}
