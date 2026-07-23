package store

import (
	"context"
	"testing"
	"time"
)

// TestReclaim_IdleForeignLeaseVsActive pins the two predicates idle-reclaim is
// built on: a freshly-held (active) lease is protected, an idle-beyond-threshold
// one is reclaimable, and a device never reclaims its own lease.
func TestReclaim_IdleForeignLeaseVsActive(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	acc := &Account{Provider: ProviderClaude, Name: "acc", AccessToken: "at", RefreshToken: "rt"}
	if err := st.Upsert(ctx, acc); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	const holder = "holder-dev"
	const seeker = "seeker-dev"
	const thresholdMs = int64(10 * 60 * 1000) // 10m

	lease, err := st.AcquireLease(ctx, "lease-1", acc.ID, holder, time.Minute)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Fresh acquire is active: excluded from the seeker's second-pass pick, and
	// not reclaimable.
	if !st.IsAccountLeasedByOtherActive(acc.ID, seeker, thresholdMs) {
		t.Fatal("a fresh lease must count as active (non-reclaimable)")
	}
	if ok, err := st.ReclaimIdleForeignLease(ctx, acc.ID, seeker, thresholdMs); err != nil || ok {
		t.Fatalf("must not reclaim an active lease: ok=%v err=%v", ok, err)
	}

	// Report idle beyond threshold via renew. Now it's reclaimable but still a
	// live lease held by the holder.
	if _, err := st.RenewLease(ctx, lease.ID, time.Minute, 15*time.Minute); err != nil {
		t.Fatalf("renew idle: %v", err)
	}
	if st.IsAccountLeasedByOtherActive(acc.ID, seeker, thresholdMs) {
		t.Fatal("an idle-beyond-threshold lease must NOT count as active")
	}
	if !st.IsAccountLeasedByOther(acc.ID, seeker) {
		t.Fatal("idle lease is still a live foreign lease until reclaimed")
	}

	// A device never reclaims its own idle lease (device_id guard).
	if ok, err := st.ReclaimIdleForeignLease(ctx, acc.ID, holder, thresholdMs); err != nil || ok {
		t.Fatalf("must not reclaim own lease: ok=%v err=%v", ok, err)
	}
	if !st.IsAccountLeased(acc.ID) {
		t.Fatal("own-lease reclaim attempt must have left the lease intact")
	}

	// The seeker reclaims it, freeing the account.
	ok, err := st.ReclaimIdleForeignLease(ctx, acc.ID, seeker, thresholdMs)
	if err != nil || !ok {
		t.Fatalf("seeker should reclaim idle foreign lease: ok=%v err=%v", ok, err)
	}
	if st.IsAccountLeased(acc.ID) {
		t.Fatal("account must be free after reclaim")
	}

	// The reclaimed lease's attribution segment is closed (not left open).
	evs, err := st.LeaseEventsForAccountSince(ctx, acc.ID, 0)
	if err != nil {
		t.Fatalf("lease events: %v", err)
	}
	for _, e := range evs {
		if e.LeaseID == lease.ID && e.EndedAt == 0 {
			t.Fatal("reclaimed lease left an open attribution segment")
		}
	}
}

// TestReclaim_DisabledWhenThresholdZero guards the local/combined-mode escape:
// a non-positive threshold collapses to the plain leased-by-other check and
// never reclaims (there are no competing devices to reclaim from).
func TestReclaim_DisabledWhenThresholdZero(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	acc := &Account{Provider: ProviderClaude, Name: "acc", AccessToken: "at", RefreshToken: "rt"}
	if err := st.Upsert(ctx, acc); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	lease, err := st.AcquireLease(ctx, "lease-1", acc.ID, "holder", time.Minute)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := st.RenewLease(ctx, lease.ID, time.Minute, time.Hour); err != nil {
		t.Fatalf("renew idle: %v", err)
	}
	// Even wildly idle, threshold 0 means "never reclaim".
	if !st.IsAccountLeasedByOtherActive(acc.ID, "seeker", 0) {
		t.Fatal("threshold 0 must fall back to leased-by-other (treat as active)")
	}
	if ok, err := st.ReclaimIdleForeignLease(ctx, acc.ID, "seeker", 0); err != nil || ok {
		t.Fatalf("threshold 0 must never reclaim: ok=%v err=%v", ok, err)
	}
}
