package httpapi

import (
	"testing"

	"github.com/hoveychen/foxy-switcher/server/store"
)

// TestToViewScopedLabelAndLimits guards the scoped-window surface of the
// account view: the seven_day_sonnet slot now carries a per-model weekly-scoped
// window (e.g. Fable). toView must expose the model label and re-serve the
// window as a limits[] weekly_scoped entry so external consumers (claude-fleet's
// foxy path) can parse it. percent is 0–100, matching the stored util unit.
func TestToViewScopedLabelAndLimits(t *testing.T) {
	a := store.Account{
		ID:                     1,
		Provider:               "claude",
		Name:                   "fable-user",
		SevenDaySonnetUtil:     42.0,
		SevenDaySonnetResetsAt: "2026-05-07T12:00:00Z",
		SevenDayScopedLabel:    "Fable",
	}
	v := toView(a)

	if v.SevenDayScopedLabel != "Fable" {
		t.Errorf("SevenDayScopedLabel = %q, want %q", v.SevenDayScopedLabel, "Fable")
	}
	if v.SevenDaySonnet == nil || v.SevenDaySonnet.Utilization != 42.0 {
		t.Fatalf("seven_day_sonnet window = %+v, want util 42.0", v.SevenDaySonnet)
	}
	if len(v.Limits) != 1 {
		t.Fatalf("expected 1 limits[] entry, got %d", len(v.Limits))
	}
	lim := v.Limits[0]
	if lim.Kind != "weekly_scoped" {
		t.Errorf("limit kind = %q, want weekly_scoped", lim.Kind)
	}
	if lim.Percent != 42.0 {
		t.Errorf("limit percent = %v, want 42.0", lim.Percent)
	}
	if lim.ResetsAt != "2026-05-07T12:00:00Z" {
		t.Errorf("limit resets_at = %q", lim.ResetsAt)
	}
	if lim.Scope == nil || lim.Scope.Model.DisplayName != "Fable" {
		t.Errorf("limit scope model = %+v, want display_name Fable", lim.Scope)
	}
}

// TestToViewNoScopedLabelOmitsLimits: a scoped window present but without a
// model label (legacy row) surfaces the bar locally but must NOT re-serve a
// limits[] entry, since consumers require a model display name.
func TestToViewNoScopedLabelOmitsLimits(t *testing.T) {
	a := store.Account{
		ID:                     2,
		Provider:               "claude",
		Name:                   "legacy",
		SevenDaySonnetUtil:     10.0,
		SevenDaySonnetResetsAt: "2026-05-07T00:00:00Z",
		SevenDayScopedLabel:    "",
	}
	v := toView(a)
	if v.SevenDaySonnet == nil {
		t.Fatal("seven_day_sonnet window should still be present")
	}
	if v.SevenDayScopedLabel != "" {
		t.Errorf("SevenDayScopedLabel = %q, want empty", v.SevenDayScopedLabel)
	}
	if len(v.Limits) != 0 {
		t.Errorf("expected no limits[] entries without a label, got %d", len(v.Limits))
	}
}
