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
