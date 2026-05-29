package store

import (
	"context"
	"math"
	"testing"
	"time"
)

// min10 is a 10-minute step in millis; tests lay snapshots on a 10-min grid
// ending well inside UsageHistoryRetention so the since-filter keeps them.
const minMs = int64(60 * 1000)

func insertSnapshot(t *testing.T, st *Store, accountID, ts int64, fh, sd, ss float64) {
	t.Helper()
	if _, err := st.db.ExecContext(context.Background(),
		`INSERT INTO usage_history (account_id, ts, five_hour_util, seven_day_util, seven_day_sonnet_util)
		 VALUES (?, ?, ?, ?, ?)`, accountID, ts, fh, sd, ss); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}
}

func insertSegment(t *testing.T, st *Store, accountID int64, deviceID string, started, ended int64) {
	t.Helper()
	if _, err := st.db.ExecContext(context.Background(),
		`INSERT INTO lease_events (lease_id, account_id, device_id, started_at, ended_at)
		 VALUES (?, ?, ?, ?, ?)`, deviceID+"-l", accountID, deviceID, started, ended); err != nil {
		t.Fatalf("insert segment: %v", err)
	}
}

func approx(a, b float64) bool { return math.Abs(a-b) < 0.01 }

func shareByDevice(at AccountAttribution) map[string]DeviceShare {
	m := make(map[string]DeviceShare, len(at.Devices))
	for _, d := range at.Devices {
		m[d.DeviceID] = d
	}
	return m
}

// TestAttribution_SingleDevice: one device holds the account for the whole
// observed span; every utilization delta accrues to it, none unattributed.
func TestAttribution_SingleDevice(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()
	a := &Account{Name: "alpha", AccessToken: "at", RefreshToken: "rt"}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertDevice(ctx, Device{ID: "dev-1", Name: "MacA", TokenHash: "h1"}); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-60 * time.Minute).UnixMilli()
	insertSnapshot(t, st, a.ID, base+0*minMs, 0, 0, 0)
	insertSnapshot(t, st, a.ID, base+10*minMs, 20, 5, 0)
	insertSnapshot(t, st, a.ID, base+20*minMs, 40, 10, 0)
	insertSnapshot(t, st, a.ID, base+30*minMs, 50, 12, 0)
	// dev-1 holds from base to now (open segment).
	insertSegment(t, st, a.ID, "dev-1", base, 0)

	at, err := st.ComputeAttribution(ctx, a.ID)
	if err != nil {
		t.Fatalf("ComputeAttribution: %v", err)
	}
	if at.SampleCount != 4 {
		t.Fatalf("SampleCount=%d want 4", at.SampleCount)
	}
	if len(at.Devices) != 1 {
		t.Fatalf("got %d devices want 1: %+v", len(at.Devices), at.Devices)
	}
	d := at.Devices[0]
	if d.DeviceID != "dev-1" || d.DeviceName != "MacA" {
		t.Fatalf("device identity: %+v", d)
	}
	if !approx(d.FiveHour, 50) || !approx(d.SevenDay, 12) {
		t.Fatalf("points: 5h=%.3f 7d=%.3f want 50/12", d.FiveHour, d.SevenDay)
	}
	if !approx(at.Unattributed.FiveHour, 0) {
		t.Fatalf("unattributed 5h=%.3f want 0", at.Unattributed.FiveHour)
	}
}

// TestAttribution_Handoff: device X holds the first half, device Y the second.
// Each window's deltas split by who held the lease during each interval — the
// "先后流转" case the whole feature exists for.
func TestAttribution_Handoff(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()
	a := &Account{Name: "alpha", AccessToken: "at", RefreshToken: "rt"}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"dev-x", "dev-y"} {
		if err := st.InsertDevice(ctx, Device{ID: id, Name: id, TokenHash: "h-" + id}); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().Add(-60 * time.Minute).UnixMilli()
	// 5h util: 0,10,20,35,50 → deltas 10,10,15,15 across [t0..t4].
	insertSnapshot(t, st, a.ID, base+0*minMs, 0, 0, 0)
	insertSnapshot(t, st, a.ID, base+10*minMs, 10, 0, 0)
	insertSnapshot(t, st, a.ID, base+20*minMs, 20, 0, 0)
	insertSnapshot(t, st, a.ID, base+30*minMs, 35, 0, 0)
	insertSnapshot(t, st, a.ID, base+40*minMs, 50, 0, 0)
	// X holds [t0, t2]; Y holds [t2, now]. The handoff is exactly at t2.
	insertSegment(t, st, a.ID, "dev-x", base+0*minMs, base+20*minMs)
	insertSegment(t, st, a.ID, "dev-y", base+20*minMs, 0)

	at, err := st.ComputeAttribution(ctx, a.ID)
	if err != nil {
		t.Fatalf("ComputeAttribution: %v", err)
	}
	by := shareByDevice(at)
	// [t0,t1]+[t1,t2] = 20 to X; [t2,t3]+[t3,t4] = 30 to Y.
	if !approx(by["dev-x"].FiveHour, 20) {
		t.Fatalf("dev-x 5h=%.3f want 20", by["dev-x"].FiveHour)
	}
	if !approx(by["dev-y"].FiveHour, 30) {
		t.Fatalf("dev-y 5h=%.3f want 30", by["dev-y"].FiveHour)
	}
	if !approx(at.Unattributed.FiveHour, 0) {
		t.Fatalf("unattributed=%.3f want 0", at.Unattributed.FiveHour)
	}
	// Most-contributing device sorts first → dev-y.
	if at.Devices[0].DeviceID != "dev-y" {
		t.Fatalf("sort order: %+v", at.Devices)
	}
}

// TestAttribution_WindowReset: a utilization drop is a window rollover. Only
// consumption AFTER the last reset counts toward the current window; the
// pre-reset spike is discarded.
func TestAttribution_WindowReset(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()
	a := &Account{Name: "alpha", AccessToken: "at", RefreshToken: "rt"}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertDevice(ctx, Device{ID: "dev-1", Name: "MacA", TokenHash: "h1"}); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-60 * time.Minute).UnixMilli()
	// 5h util: 0 → 80 → 10 (RESET) → 30. Current window = 10 then +20 = 30.
	insertSnapshot(t, st, a.ID, base+0*minMs, 0, 0, 0)
	insertSnapshot(t, st, a.ID, base+10*minMs, 80, 0, 0)
	insertSnapshot(t, st, a.ID, base+20*minMs, 10, 0, 0)
	insertSnapshot(t, st, a.ID, base+30*minMs, 30, 0, 0)
	insertSegment(t, st, a.ID, "dev-1", base, 0)

	at, err := st.ComputeAttribution(ctx, a.ID)
	if err != nil {
		t.Fatalf("ComputeAttribution: %v", err)
	}
	by := shareByDevice(at)
	if !approx(by["dev-1"].FiveHour, 30) {
		t.Fatalf("dev-1 5h=%.3f want 30 (pre-reset 80 discarded)", by["dev-1"].FiveHour)
	}
}

// TestAttribution_NoLeaseHolder: consumption observed in an interval with no
// lease segment is unattributable, not silently dropped.
func TestAttribution_NoLeaseHolder(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()
	a := &Account{Name: "alpha", AccessToken: "at", RefreshToken: "rt"}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-60 * time.Minute).UnixMilli()
	insertSnapshot(t, st, a.ID, base+0*minMs, 0, 0, 0)
	insertSnapshot(t, st, a.ID, base+10*minMs, 25, 0, 0)
	// No lease segments at all.

	at, err := st.ComputeAttribution(ctx, a.ID)
	if err != nil {
		t.Fatalf("ComputeAttribution: %v", err)
	}
	if len(at.Devices) != 0 {
		t.Fatalf("want 0 devices, got %+v", at.Devices)
	}
	if !approx(at.Unattributed.FiveHour, 25) {
		t.Fatalf("unattributed 5h=%.3f want 25", at.Unattributed.FiveHour)
	}
}

// TestAttribution_ThinData: fewer than two snapshots yields no deltas and an
// empty breakdown (the UI uses SampleCount to show a "just started" hint).
func TestAttribution_ThinData(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()
	a := &Account{Name: "alpha", AccessToken: "at", RefreshToken: "rt"}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatal(err)
	}
	insertSnapshot(t, st, a.ID, time.Now().Add(-5*time.Minute).UnixMilli(), 42, 0, 0)
	at, err := st.ComputeAttribution(ctx, a.ID)
	if err != nil {
		t.Fatalf("ComputeAttribution: %v", err)
	}
	if at.SampleCount != 1 || len(at.Devices) != 0 {
		t.Fatalf("thin data: SampleCount=%d devices=%d want 1/0", at.SampleCount, len(at.Devices))
	}
}
