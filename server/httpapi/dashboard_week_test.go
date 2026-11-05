package httpapi

import (
	"math"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
)

// TestSevenDayWeek: the idle line steps down at each account's 7d reset; the
// same-week line replays last week's consumption (a drop counts as a reset,
// its new value as fresh use) onto the lowest-utilization account, and the
// resets wipe it too. Codex has no 7d weight and stays out.
func TestSevenDayWeek(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 30, 0, 0, time.UTC)
	h := time.Hour
	rfc := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	accs := []store.Account{
		{ID: 1, Provider: store.ProviderClaude, RateLimitTier: "default_claude_max_5x", Status: "active",
			SevenDayUtil: 80, SevenDayThreshold: 90, SevenDayResetsAt: rfc(24 * h)},
		{ID: 2, Provider: store.ProviderClaude, RateLimitTier: "default_claude_max_5x", Status: "active",
			SevenDayUtil: 20, SevenDayThreshold: 90, SevenDayResetsAt: rfc(72 * h)},
		{ID: 3, Provider: store.ProviderCodex, Status: "active", SevenDayUtil: 50, SevenDayThreshold: 90},
	}
	wk := now.Add(-weekHours * h)
	ms := func(t time.Time) int64 { return t.UnixMilli() }
	rows := []store.UsageHistoryRow{
		{AccountID: 1, Timestamp: ms(wk.Add(30 * time.Minute)), SevenDayUtil: 0},
		{AccountID: 3, Timestamp: ms(wk.Add(40 * time.Minute)), SevenDayUtil: 0},
		{AccountID: 1, Timestamp: ms(wk.Add(90 * time.Minute)), SevenDayUtil: 10}, // +0.5 PE, hour 2
		{AccountID: 3, Timestamp: ms(wk.Add(100 * time.Minute)), SevenDayUtil: 90}, // Codex: ignored
		{AccountID: 1, Timestamp: ms(wk.Add(5 * h)), SevenDayUtil: 4},              // reset: +0.2 PE, hour 5
	}
	got := sevenDayWeek(accs, rows, now)

	if got.Capacity != 10 {
		t.Fatalf("capacity = %v, want 10", got.Capacity)
	}
	if n := len(got.Points); n != 2*weekHours+1 {
		t.Fatalf("points = %d, want %d", n, 2*weekHours+1)
	}
	near := func(name string, v, want float64) {
		t.Helper()
		if math.Abs(v-want) > 1e-9 {
			t.Errorf("%s = %v, want %v", name, v, want)
		}
	}
	first := got.Points[0]
	near("first actual", first.ActualPct, 10) // A seeded 0, B today's 20
	nowPt := got.Points[weekHours]
	if nowPt.Forecast || nowPt.TS != ms(now) {
		t.Fatalf("now point = %+v", nowPt)
	}
	near("now actual", nowPt.ActualPct, 50)
	near("now same-week", nowPt.SameWeekPct, 50)
	near("now idle", nowPt.IdlePct, 50)

	at := func(k int) DashboardWeekPoint { return got.Points[weekHours+k] }
	if !at(1).Forecast {
		t.Fatal("point after now isn't a forecast")
	}
	near("idle +23h", at(23).IdlePct, 50)
	near("idle +24h", at(24).IdlePct, 10) // A reset
	near("idle +72h", at(72).IdlePct, 0)  // B reset
	near("same-week +1h", at(1).SameWeekPct, 50)
	near("same-week +2h", at(2).SameWeekPct, 55) // B 20 -> 30
	near("same-week +5h", at(5).SameWeekPct, 57) // B 30 -> 34
	near("same-week +24h", at(24).SameWeekPct, 17)
	near("same-week +72h", at(72).SameWeekPct, 0)
}

// TestSevenDayWeek_Saturates: consumption fills the lowest account to its
// threshold (which then reads full), spills into the next, and whatever the
// pool can't absorb is dropped.
func TestSevenDayWeek_Saturates(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 30, 0, 0, time.UTC)
	accs := []store.Account{
		{ID: 1, Provider: store.ProviderClaude, RateLimitTier: "default_claude_pro", Status: "active",
			SevenDayUtil: 0, SevenDayThreshold: 90},
		{ID: 2, Provider: store.ProviderClaude, RateLimitTier: "default_claude_pro", Status: "active",
			SevenDayUtil: 50, SevenDayThreshold: 90},
		{ID: 3, Provider: store.ProviderClaude, RateLimitTier: "default_claude_pro", Status: "paused",
			SevenDayUtil: 10, SevenDayThreshold: 90},
	}
	wk := now.Add(-weekHours * time.Hour).UnixMilli()
	// A 20x account used 25% (2.5 PE) last week — more than the pool can take.
	rows := []store.UsageHistoryRow{
		{AccountID: 4, Timestamp: wk + 1, SevenDayUtil: 0},
		{AccountID: 4, Timestamp: wk + 2, SevenDayUtil: 25},
	}
	accs = append(accs, store.Account{ID: 4, Provider: store.ProviderClaude, RateLimitTier: "default_claude_max_20x",
		Status: "paused", SevenDayUtil: 0, SevenDayThreshold: 90})
	got := sevenDayWeek(accs, rows, now)
	// Capacity 1+1+1+10 = 13. Both active Pros fill to 90 and read full; the
	// paused ones take nothing: (1 + 1 + 0.1 + 0) / 13.
	p := got.Points[weekHours+1].SameWeekPct
	if want := 2.1 / 13 * 100; math.Abs(p-want) > 1e-9 {
		t.Errorf("same-week +1h = %v, want %v", p, want)
	}
}
