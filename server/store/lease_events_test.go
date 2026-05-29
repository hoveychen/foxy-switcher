package store

import (
	"context"
	"testing"
	"time"
)

// allEvents reads every lease_events segment for an account (since=0), the
// shape attribution code consumes. Helper for the lifecycle assertions below.
func allEvents(t *testing.T, st *Store, accountID int64) []LeaseEvent {
	t.Helper()
	ev, err := st.LeaseEventsForAccountSince(context.Background(), accountID, 0)
	if err != nil {
		t.Fatalf("LeaseEventsForAccountSince: %v", err)
	}
	return ev
}

// TestLeaseEvents_AcquireOpensSegment: a fresh acquire opens exactly one
// open segment (ended_at == 0) attributed to the acquiring device, and a
// same-device renew-in-place does NOT open a second segment — the held span
// is one continuous segment across renewals.
func TestLeaseEvents_AcquireOpensSegment(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()
	a := &Account{Name: "alpha", AccessToken: "at-a", RefreshToken: "rt-a"}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatal(err)
	}

	if _, err := st.AcquireLease(ctx, "lease-1", a.ID, "dev-1", time.Minute); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	ev := allEvents(t, st, a.ID)
	if len(ev) != 1 {
		t.Fatalf("after acquire: got %d events, want 1", len(ev))
	}
	if ev[0].DeviceID != "dev-1" || ev[0].StartedAt == 0 || ev[0].EndedAt != 0 {
		t.Fatalf("segment: %+v (want dev-1, open)", ev[0])
	}

	// Renew in place (same device, same lease id semantics): no new segment.
	if _, err := st.AcquireLease(ctx, "lease-ignored", a.ID, "dev-1", time.Minute); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if ev := allEvents(t, st, a.ID); len(ev) != 1 || ev[0].EndedAt != 0 {
		t.Fatalf("after renew: want still 1 open segment, got %+v", ev)
	}
}

// TestLeaseEvents_ReleaseClosesSegment: ReleaseLease stamps ended_at on the
// open segment (so attribution stops counting it) while keeping the history
// row, and is idempotent.
func TestLeaseEvents_ReleaseClosesSegment(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()
	a := &Account{Name: "alpha", AccessToken: "at-a", RefreshToken: "rt-a"}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AcquireLease(ctx, "lease-1", a.ID, "dev-1", time.Minute); err != nil {
		t.Fatal(err)
	}
	before := time.Now().UnixMilli()
	if err := st.ReleaseLease(ctx, "lease-1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	ev := allEvents(t, st, a.ID)
	if len(ev) != 1 {
		t.Fatalf("after release: got %d events, want 1 (history kept)", len(ev))
	}
	if ev[0].EndedAt < before {
		t.Fatalf("segment not closed: ended_at=%d before=%d", ev[0].EndedAt, before)
	}
	// Idempotent: releasing again must not error or reopen.
	if err := st.ReleaseLease(ctx, "lease-1"); err != nil {
		t.Fatalf("release again: %v", err)
	}
	if ev := allEvents(t, st, a.ID); len(ev) != 1 || ev[0].EndedAt == 0 {
		t.Fatalf("double release changed state: %+v", ev)
	}
}

// TestLeaseEvents_SweepClosesAtExpiry: an expired lease swept by SweepLeases
// gets its open segment closed at the lease's own expires_at (not "now"),
// because the device could not have used the account past its lease lapse.
func TestLeaseEvents_SweepClosesAtExpiry(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()
	a := &Account{Name: "alpha", AccessToken: "at-a", RefreshToken: "rt-a"}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatal(err)
	}
	// Insert an already-expired lease plus its open event by hand so we can
	// assert the close bounds the segment at expires_at exactly.
	started := time.Now().Add(-10 * time.Minute).UnixMilli()
	expires := time.Now().Add(-2 * time.Minute).UnixMilli()
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO leases (id, account_id, device_id, acquired_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		"lease-1", a.ID, "dev-1", started, expires); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO lease_events (lease_id, account_id, device_id, started_at, ended_at) VALUES (?, ?, ?, ?, 0)`,
		"lease-1", a.ID, "dev-1", started); err != nil {
		t.Fatal(err)
	}

	if err := st.SweepLeases(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	ev := allEvents(t, st, a.ID)
	if len(ev) != 1 {
		t.Fatalf("after sweep: got %d events, want 1", len(ev))
	}
	if ev[0].EndedAt != expires {
		t.Fatalf("segment closed at %d, want exactly expires_at=%d", ev[0].EndedAt, expires)
	}
}

// TestLeaseEvents_HandoffSequence: the "先后流转" case — device X holds the
// account, its lease expires, then device Y acquires the same account. We must
// end up with two segments (X closed at its expiry, Y open), proving the
// history can attribute a single account's window across multiple devices.
func TestLeaseEvents_HandoffSequence(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()
	a := &Account{Name: "alpha", AccessToken: "at-a", RefreshToken: "rt-a"}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatal(err)
	}
	// Device X holds an already-expired lease (so the next acquire sweeps it).
	xStart := time.Now().Add(-30 * time.Minute).UnixMilli()
	xExpire := time.Now().Add(-1 * time.Minute).UnixMilli()
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO leases (id, account_id, device_id, acquired_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		"lease-x", a.ID, "dev-x", xStart, xExpire); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO lease_events (lease_id, account_id, device_id, started_at, ended_at) VALUES (?, ?, ?, ?, 0)`,
		"lease-x", a.ID, "dev-x", xStart); err != nil {
		t.Fatal(err)
	}

	// Device Y acquires — AcquireLease must close X's segment (at xExpire) and
	// open Y's.
	if _, err := st.AcquireLease(ctx, "lease-y", a.ID, "dev-y", time.Minute); err != nil {
		t.Fatalf("Y acquire: %v", err)
	}
	ev := allEvents(t, st, a.ID)
	if len(ev) != 2 {
		t.Fatalf("handoff: got %d segments, want 2 (%+v)", len(ev), ev)
	}
	// Ordered by started_at ASC → X first, Y second.
	if ev[0].DeviceID != "dev-x" || ev[0].EndedAt != xExpire {
		t.Fatalf("X segment: %+v (want dev-x closed at %d)", ev[0], xExpire)
	}
	if ev[1].DeviceID != "dev-y" || ev[1].EndedAt != 0 {
		t.Fatalf("Y segment: %+v (want dev-y open)", ev[1])
	}
}
