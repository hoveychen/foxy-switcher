package credinject

import (
	"context"
	"testing"
	"time"
)

// idle returns an activityProbe that reports the agent as idle beyond the
// reclaim threshold; active reports it as busy right now.
func idleProbe(c *Coordinator) func() time.Duration {
	return func() time.Duration { return c.idleThreshold + time.Minute }
}
func activeProbe() func() time.Duration { return func() time.Duration { return 0 } }

// TestReconcile_IdleAgentDoesNotAcquire: an agent with no live lease that is
// idle (Claude Code not used lately) must NOT grab a slot — that's the whole
// point of freeing leases from unused machines. It injects nothing and holds no
// lease until real activity resumes.
func TestReconcile_IdleAgentDoesNotAcquire(t *testing.T) {
	c, be, st, _ := newCoord(t)
	seedActive(t, st, "alpha", "sk-ant-oat01-alpha")
	c.activityProbe = idleProbe(c)

	c.reconcile(context.Background())

	if c.CurrentAccountID() != 0 {
		t.Fatalf("idle agent must not inject; currentAccountID=%d", c.CurrentAccountID())
	}
	if be.hasOAuth {
		t.Fatal("idle agent must not write the keychain")
	}
	ls, err := st.ListActiveLeases(context.Background())
	if err != nil {
		t.Fatalf("list leases: %v", err)
	}
	if len(ls) != 0 {
		t.Fatalf("idle agent must hold no lease, got %d", len(ls))
	}
}

// TestReconcile_IdleHolderKeepsLease: an agent that ALREADY holds a lease keeps
// renewing it while idle (zero churn) rather than releasing — but the renew
// reports idleFor, so from another device's view the lease is now reclaimable.
func TestReconcile_IdleHolderKeepsLease(t *testing.T) {
	c, _, st, _ := newCoord(t)
	id := seedActive(t, st, "alpha", "sk-ant-oat01-alpha")
	ctx := context.Background()

	// Active: acquire + inject.
	c.reconcile(ctx)
	if c.CurrentAccountID() != id {
		t.Fatalf("active reconcile should inject alpha; got %d", c.CurrentAccountID())
	}
	leaseID := c.currentLeaseID
	if leaseID == "" {
		t.Fatal("expected a live lease after active reconcile")
	}

	// Go idle. The holder must KEEP the same lease (no churn).
	c.activityProbe = idleProbe(c)
	c.reconcile(ctx)
	if c.currentLeaseID != leaseID {
		t.Fatalf("idle holder must keep its lease, not churn: %q → %q", leaseID, c.currentLeaseID)
	}
	// But the reported idleFor makes it reclaimable from another device.
	if st.IsAccountLeasedByOtherActive(id, "other-dev", c.idleThreshold.Milliseconds()) {
		t.Fatal("an idle holder's lease should be reclaimable (not counted active) after an idle renew")
	}
}

// TestReconcile_ReclaimedIdleHolderParksThenReacquires: after the vault
// reclaims an idle agent's lease under pressure, the agent must stay parked
// (not immediately re-grab a slot) — and then re-acquire the instant real
// activity resumes, with no manual "resume" step.
func TestReconcile_ReclaimedIdleHolderParksThenReacquires(t *testing.T) {
	c, be, st, _ := newCoord(t)
	id := seedActive(t, st, "alpha", "sk-ant-oat01-alpha")
	ctx := context.Background()

	c.reconcile(ctx) // active: acquire
	leaseID := c.currentLeaseID
	if leaseID == "" {
		t.Fatal("expected a live lease after active reconcile")
	}

	// Go idle, then simulate the vault reclaiming our lease under pressure.
	c.activityProbe = idleProbe(c)
	if err := st.ReleaseLease(ctx, leaseID); err != nil {
		t.Fatalf("simulate reclaim: %v", err)
	}

	writesBefore := be.writes
	c.reconcile(ctx) // idle + lease gone → must PARK, not re-acquire
	if c.currentLeaseID != "" {
		t.Fatalf("parked idle agent must hold no lease, got %q", c.currentLeaseID)
	}
	ls, _ := st.ListActiveLeases(ctx)
	if len(ls) != 0 {
		t.Fatalf("idle agent must NOT re-acquire after reclaim, got %d leases", len(ls))
	}
	if be.writes != writesBefore {
		t.Fatalf("parked agent must not rewrite the keychain: %d → %d", writesBefore, be.writes)
	}

	// User returns → active. Next reconcile re-acquires automatically.
	c.activityProbe = activeProbe()
	c.reconcile(ctx)
	if c.currentLeaseID == "" {
		t.Fatal("active agent must re-acquire after returning from idle")
	}
	ls, _ = st.ListActiveLeases(ctx)
	if len(ls) != 1 {
		t.Fatalf("expected exactly one lease after re-acquire, got %d", len(ls))
	}
	if id != c.CurrentAccountID() {
		t.Fatalf("re-acquired account changed unexpectedly: %d", c.CurrentAccountID())
	}
}
