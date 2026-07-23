package store

import (
	"context"
	"testing"
	"time"
)

// grandfatheredLease inserts a leases row WITHOUT a matching lease_events
// segment, simulating a lease first acquired before the lease_events feature
// existed (or first-acquired under code that didn't open a segment). acquired
// is the original acquire time; the lease is live (expires in the future).
func grandfatheredLease(t *testing.T, st *Store, leaseID string, accountID int64, deviceID string, acquired int64) {
	t.Helper()
	if _, err := st.db.ExecContext(context.Background(),
		`INSERT INTO leases (id, account_id, device_id, acquired_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		leaseID, accountID, deviceID, acquired, time.Now().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatalf("insert grandfathered lease: %v", err)
	}
}

// TestRenew_AcquireBackfillsMissingSegment: a grandfathered lease renewed via
// AcquireLease's renew-in-place path must get an open lease_events segment
// opened (anchored at the lease's original acquired_at), so a continuously
// held account becomes attributable without waiting for it to lapse.
func TestRenew_AcquireBackfillsMissingSegment(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()
	a := &Account{Name: "alpha", AccessToken: "at", RefreshToken: "rt"}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatal(err)
	}
	acquired := time.Now().Add(-3 * time.Hour).UnixMilli()
	grandfatheredLease(t, st, "lease-1", a.ID, "dev-1", acquired)

	// Sanity: no event yet (this is the grandfathered state).
	if ev := allEvents(t, st, a.ID); len(ev) != 0 {
		t.Fatalf("precondition: expected 0 events, got %d", len(ev))
	}

	// Renew-in-place: same device re-acquires the live lease.
	if _, err := st.AcquireLease(ctx, "ignored-id", a.ID, "dev-1", time.Minute); err != nil {
		t.Fatalf("renew acquire: %v", err)
	}

	ev := allEvents(t, st, a.ID)
	if len(ev) != 1 {
		t.Fatalf("expected 1 backfilled segment, got %d: %+v", len(ev), ev)
	}
	if ev[0].DeviceID != "dev-1" || ev[0].EndedAt != 0 {
		t.Fatalf("segment: %+v (want dev-1, open)", ev[0])
	}
	if ev[0].StartedAt != acquired {
		t.Fatalf("segment started_at=%d, want lease acquired_at=%d", ev[0].StartedAt, acquired)
	}
}

// TestRenew_AcquireDoesNotDuplicateSegment: when an open segment already
// exists, renew-in-place must NOT open a second one.
func TestRenew_AcquireDoesNotDuplicateSegment(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()
	a := &Account{Name: "alpha", AccessToken: "at", RefreshToken: "rt"}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatal(err)
	}
	// Fresh acquire opens exactly one segment.
	if _, err := st.AcquireLease(ctx, "lease-1", a.ID, "dev-1", time.Minute); err != nil {
		t.Fatal(err)
	}
	// Renew several times.
	for i := 0; i < 3; i++ {
		if _, err := st.AcquireLease(ctx, "ignored", a.ID, "dev-1", time.Minute); err != nil {
			t.Fatalf("renew %d: %v", i, err)
		}
	}
	if ev := allEvents(t, st, a.ID); len(ev) != 1 || ev[0].EndedAt != 0 {
		t.Fatalf("expected exactly 1 open segment after renews, got %+v", ev)
	}
}

// TestRenew_RenewLeaseBackfillsMissingSegment: the same backfill must happen
// when the agent keeps the lease alive via RenewLease (by lease id) rather
// than AcquireLease.
func TestRenew_RenewLeaseBackfillsMissingSegment(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()
	a := &Account{Name: "alpha", AccessToken: "at", RefreshToken: "rt"}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatal(err)
	}
	acquired := time.Now().Add(-2 * time.Hour).UnixMilli()
	grandfatheredLease(t, st, "lease-1", a.ID, "dev-1", acquired)

	if _, err := st.RenewLease(ctx, "lease-1", time.Minute, 0); err != nil {
		t.Fatalf("renew: %v", err)
	}

	ev := allEvents(t, st, a.ID)
	if len(ev) != 1 {
		t.Fatalf("expected 1 backfilled segment, got %d: %+v", len(ev), ev)
	}
	if ev[0].DeviceID != "dev-1" || ev[0].EndedAt != 0 || ev[0].StartedAt != acquired {
		t.Fatalf("segment: %+v (want dev-1 open, started_at=%d)", ev[0], acquired)
	}
}
