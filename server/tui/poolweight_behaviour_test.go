package tui

import "testing"

// poolWindowTotals is where the weighting actually reaches the KPI: a Codex
// account must move neither capacity nor used.
func TestPoolWindowTotals_ExcludesCodex(t *testing.T) {
	claude := Account{
		Provider: "claude", Status: "active",
		RateLimitTier: "default_claude_max_5x",
		FiveHour:      &UsageWindow{Utilization: 50},
	}
	codex := Account{
		Provider: "codex", Status: "active",
		SubscriptionType: "team",
		FiveHour:         &UsageWindow{Utilization: 100},
	}

	soloUsed, soloCap, soloPct := poolWindowTotals([]Account{claude}, "five_hour")
	mixUsed, mixCap, mixPct := poolWindowTotals([]Account{claude, codex}, "five_hour")

	if soloCap != mixCap {
		t.Fatalf("codex changed capacity: %v -> %v", soloCap, mixCap)
	}
	if soloUsed != mixUsed {
		t.Fatalf("codex changed used: %v -> %v", soloUsed, mixUsed)
	}
	if soloPct != mixPct {
		t.Fatalf("codex changed percent: %v -> %v", soloPct, mixPct)
	}
}

// A cooling account's quota is unusable, so it counts as full: a 7d freeze
// fills both windows, a 5h freeze fills only 5h.
func TestPoolWindowTotals_CoolingCountsFull(t *testing.T) {
	base := Account{
		Provider: "claude", Status: "active",
		RateLimitTier:     "default_claude_max_5x",
		FiveHourThreshold: 90, SevenDayThreshold: 90,
	}
	sevenFrozen := base
	sevenFrozen.FiveHour = &UsageWindow{Utilization: 10}
	sevenFrozen.SevenDay = &UsageWindow{Utilization: 92}
	fiveFrozen := base
	fiveFrozen.FiveHour = &UsageWindow{Utilization: 91}
	fiveFrozen.SevenDay = &UsageWindow{Utilization: 40}

	for _, tc := range []struct {
		name         string
		a            Account
		want5, want7 float64
	}{
		{"7d frozen", sevenFrozen, 5, 5},
		{"5h frozen", fiveFrozen, 5, 2},
	} {
		u5, _, _ := poolWindowTotals([]Account{tc.a}, "five_hour")
		u7, _, _ := poolWindowTotals([]Account{tc.a}, "seven_day")
		if u5 != tc.want5 || u7 != tc.want7 {
			t.Errorf("%s: used 5h=%v 7d=%v, want %v/%v", tc.name, u5, u7, tc.want5, tc.want7)
		}
	}
}
