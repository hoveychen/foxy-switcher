package openai

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
	"github.com/hoveychen/foxy-switcher/server/vault"
)

// newIdleTestManager builds a RemoteManager over a one-account Codex pool and
// returns it alongside the store and the account, with a settable activity
// probe so the test drives "am I idle?" deterministically.
func newIdleTestManager(t *testing.T) (*RemoteManager, *store.Store, *store.Account) {
	t.Helper()
	dir := t.TempDir()
	storage := &fileCredentialStorage{authPath: filepath.Join(dir, "auth.json")}
	if err := storage.Save(authJSON(t, "native", "")); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	managedAuth, _ := ParseAuthFile(authJSON(t, "pooled", ""))
	managed, _ := managedAuth.Account()
	if err := st.Upsert(context.Background(), managed); err != nil {
		t.Fatal(err)
	}
	return NewRemoteManager(vault.NewInProc(st), storage, "device-a", nil), st, managed
}

// An idle device holding no lease must not take a Codex slot: a machine that
// never runs codex used to grab a pool account at startup and squat it until
// the process exited.
func TestRemoteManagerIdleDeviceDoesNotAcquire(t *testing.T) {
	m, st, managed := newIdleTestManager(t)
	m.activityProbe = func() time.Duration { return vault.DefaultIdleReclaimThreshold + time.Minute }

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if st.IsAccountLeased(managed.ID) || m.ManagedAccountID() != 0 {
		t.Fatalf("idle device took a slot: leased=%v managed=%d",
			st.IsAccountLeased(managed.ID), m.ManagedAccountID())
	}

	// Activity resumes — the same manager now acquires on the next tick.
	m.activityProbe = func() time.Duration { return 0 }
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile after activity: %v", err)
	}
	if !st.IsAccountLeased(managed.ID) || m.ManagedAccountID() != managed.ID {
		t.Fatalf("active device did not acquire: leased=%v managed=%d",
			st.IsAccountLeased(managed.ID), m.ManagedAccountID())
	}
}

// The whole point of reporting a real idleFor: another device that finds the
// pool exhausted can reclaim an idle holder's slot, and the reclaimed holder
// parks instead of grabbing the account straight back.
func TestRemoteManagerIdleLeaseIsReclaimableAndParks(t *testing.T) {
	ctx := context.Background()
	m, st, managed := newIdleTestManager(t)
	m.activityProbe = func() time.Duration { return 0 }
	if err := m.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	svc := vault.NewInProc(st)

	// While we're active the pool looks empty to everyone else — no reclaim.
	if _, err := svc.PickProviderForDevice(ctx, time.Now(), "device-b", store.ProviderCodex); err == nil {
		t.Fatal("an active holder's lease must not be reclaimable")
	}

	// Go idle and renew: the vault now sees a live-but-idle lease.
	m.activityProbe = func() time.Duration { return vault.DefaultIdleReclaimThreshold + time.Minute }
	if err := m.Reconcile(ctx); err != nil {
		t.Fatalf("idle renew reconcile: %v", err)
	}
	if !st.IsAccountLeased(managed.ID) {
		t.Fatal("an idle holder should keep its slot until someone actually needs it")
	}

	got, err := svc.PickProviderForDevice(ctx, time.Now(), "device-b", store.ProviderCodex)
	if err != nil {
		t.Fatalf("pool-starved device-b should reclaim the idle lease: %v", err)
	}
	if got.ID != managed.ID {
		t.Fatalf("reclaimed account = %d, want %d", got.ID, managed.ID)
	}
	if _, err := svc.AcquireLease(ctx, managed.ID, "device-b", time.Minute); err != nil {
		t.Fatalf("device-b acquire after reclaim: %v", err)
	}

	// Our next tick finds the lease gone. Still idle → park, don't re-grab.
	if err := m.Reconcile(ctx); err != nil {
		t.Fatalf("park reconcile: %v", err)
	}
	if m.currentLeaseID != "" {
		t.Fatal("reclaimed manager should have dropped its lease id")
	}
	if !st.IsAccountLeasedByOther(managed.ID, "device-a") {
		t.Fatal("parked manager stole the account back from device-b")
	}
}

// Losing the account to another device must clear stickiness, otherwise
// chooseStickyCodex keeps returning the now-foreign account and the device
// never recovers to a free one.
func TestRemoteManagerLostLeaseClearsStickiness(t *testing.T) {
	ctx := context.Background()
	m, st, managed := newIdleTestManager(t)
	m.activityProbe = func() time.Duration { return 0 }
	if err := m.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Simulate "the vault handed our account to device-b while we blinked":
	// drop our lease row, let device-b take it, keep our sticky pointer.
	svc := vault.NewInProc(st)
	if err := svc.ReleaseLease(ctx, m.currentLeaseID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := svc.AcquireLease(ctx, managed.ID, "device-b", time.Minute); err != nil {
		t.Fatalf("device-b acquire: %v", err)
	}

	// Active, so we try to re-acquire — and get refused.
	if err := m.Reconcile(ctx); err == nil {
		t.Fatal("re-acquiring a foreign-held account should fail")
	}
	if m.ManagedAccountID() != 0 || m.currentLeaseID != "" {
		t.Fatalf("stickiness survived the loss: managed=%d lease=%q",
			m.ManagedAccountID(), m.currentLeaseID)
	}
}
