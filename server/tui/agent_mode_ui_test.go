package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestAccountsFooter_AgentMode_HidesAdminChips: in agent mode the footer
// should not advertise vault-write shortcuts (a/p/x). Mirrors the desktop's
// agent-lease-only UI rule.
func TestAccountsFooter_AgentMode_HidesAdminChips(t *testing.T) {
	m := &model{disableAdminActions: true}
	defs := m.footerChipDefs()

	hidden := []string{"a", "p", "x"}
	for _, def := range defs {
		for _, h := range hidden {
			if def.key == h {
				t.Errorf("agent mode should hide chip %q (label=%q)", h, def.label)
			}
		}
	}

	// Lease-friendly chips must still be present.
	wantPresent := []string{"u", "r", "R"}
	for _, want := range wantPresent {
		found := false
		for _, def := range defs {
			if def.key == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("agent mode should keep chip %q", want)
		}
	}
}

// TestAccountsFooter_LocalMode_KeepsAdminChips: combined mode (or any
// non-agent mode) must still expose a / p / x.
func TestAccountsFooter_LocalMode_KeepsAdminChips(t *testing.T) {
	m := &model{disableAdminActions: false}
	defs := m.footerChipDefs()
	for _, want := range []string{"a", "p", "x"} {
		found := false
		for _, def := range defs {
			if def.key == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("local mode should expose chip %q", want)
		}
	}
}

// TestHandleListKey_AgentMode_IgnoresAdminKeys: pressing a/p/x in agent
// mode must not trigger any cmd. Otherwise the user would hit a 405
// from the upstream vault and see a confusing error toast.
func TestHandleListKey_AgentMode_IgnoresAdminKeys(t *testing.T) {
	keys := []string{"a", "p", "x"}
	for _, k := range keys {
		t.Run(k, func(t *testing.T) {
			m := &model{
				disableAdminActions: true,
				accounts:            []Account{{ID: 1, Name: "alpha"}},
				cursor:              0,
				mode:                modeList,
			}
			km := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
			_, cmd := m.handleListKey(km)
			if cmd != nil {
				t.Fatalf("agent mode should ignore %q, but got non-nil cmd", k)
			}
			if m.mode != modeList {
				t.Fatalf("agent mode should keep modeList, got %v", m.mode)
			}
		})
	}
}

// TestSettingsRowOrder_AgentMode_DropsThreshold: the default-threshold rows
// are vault-side state shared across agents — a lease consumer can't edit
// them, so they must vanish from keyboard-nav order in agent mode.
func TestSettingsRowOrder_AgentMode_DropsThreshold(t *testing.T) {
	thresholdRows := map[settingsRowID]bool{
		rowDefaultFiveHour:       true,
		rowDefaultSevenDay:       true,
		rowDefaultSevenDaySonnet: true,
	}
	p := &settingsPage{about: About{Mode: "agent"}}
	for _, id := range p.rowOrder() {
		if thresholdRows[id] {
			t.Fatal("default-threshold rows must not appear in agent-mode rowOrder")
		}
	}
	// Sanity: combined mode keeps all three rows.
	pCombined := &settingsPage{about: About{Mode: "combined"}}
	found := 0
	for _, id := range pCombined.rowOrder() {
		if thresholdRows[id] {
			found++
		}
	}
	if found != 3 {
		t.Fatalf("combined-mode rowOrder must keep all three default-threshold rows; found %d", found)
	}
}

// TestSettingsView_AgentMode_HidesThresholdRow: e2e check that the row
// is also gone from the rendered string, not just nav order.
func TestSettingsView_AgentMode_HidesThresholdRow(t *testing.T) {
	p := &settingsPage{
		width:            120,
		height:           40,
		settings:         Settings{UsagePollIntervalSec: 60, DefaultFiveHourThreshold: 90, DefaultSevenDayThreshold: 90, DefaultSevenDaySonnetThreshold: 90, RestoreNativeOnQuit: true},
		autoSwitch:       AutoSwitch{Enabled: false, Policy: "lru"},
		about:            About{Mode: "agent", VaultURL: "https://x"},
		settingsLoaded:   true,
		autoSwitchLoaded: true,
		aboutLoaded:      true,
	}
	out := p.view()
	if strings.Contains(out, "Default 5h threshold") {
		t.Fatalf("agent-mode settings view should not render default threshold rows; got\n%s", out)
	}

	pCombined := &settingsPage{
		width:            120,
		height:           40,
		settings:         Settings{UsagePollIntervalSec: 60, DefaultFiveHourThreshold: 90, DefaultSevenDayThreshold: 90, DefaultSevenDaySonnetThreshold: 90, RestoreNativeOnQuit: true},
		autoSwitch:       AutoSwitch{Enabled: false, Policy: "lru"},
		about:            About{Mode: "combined"},
		settingsLoaded:   true,
		autoSwitchLoaded: true,
		aboutLoaded:      true,
	}
	outCombined := pCombined.view()
	if !strings.Contains(outCombined, "Default 5h threshold") {
		t.Fatalf("combined-mode settings view must render default threshold rows; got\n%s", outCombined)
	}
}
