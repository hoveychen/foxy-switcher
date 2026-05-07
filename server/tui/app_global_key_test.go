package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestHandleGlobalKey_QuitFromAnyScreen — q and ctrl+c must yield tea.Quit
// regardless of which screen is active. Previously only Accounts' inner
// model handled q; Dashboard / Activity / Settings dropped it on the floor
// and the user couldn't quit from those tabs.
func TestHandleGlobalKey_QuitFromAnyScreen(t *testing.T) {
	cases := []struct {
		name string
		key  tea.KeyMsg
	}{
		{"q", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}}},
		{"ctrl+c", tea.KeyMsg{Type: tea.KeyCtrlC}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := &App{accounts: &model{mode: modeList}}
			cmd, handled := app.handleGlobalKey(tc.key)
			if !handled {
				t.Fatalf("%s should be handled at the App level", tc.name)
			}
			if cmd == nil {
				t.Fatalf("%s should return a non-nil quit cmd", tc.name)
			}
			if _, ok := cmd().(tea.QuitMsg); !ok {
				t.Fatalf("%s cmd should yield tea.QuitMsg, got %T", tc.name, cmd())
			}
		})
	}
}

// TestHandleGlobalKey_HelpToggle — ? opens the help modal without producing
// a cmd; quitting it is exercised via the existing helpOpen path.
func TestHandleGlobalKey_HelpToggle(t *testing.T) {
	app := &App{accounts: &model{mode: modeList}}
	cmd, handled := app.handleGlobalKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	if !handled {
		t.Fatal("? should be handled")
	}
	if cmd != nil {
		t.Fatalf("? should not return a cmd; got %v", cmd)
	}
	if !app.helpOpen {
		t.Fatal("? should set helpOpen=true")
	}
}

// TestAcceptsGlobalKey_AccountsModalSuppression guards the textinput-stealing
// edge case: when the Accounts page is in a modal (paste / search /
// threshold / confirm), acceptsGlobalKey must return false so the keystroke
// reaches the focused textinput instead of being eaten as a global hotkey.
func TestAcceptsGlobalKey_AccountsModalSuppression(t *testing.T) {
	suppressed := []struct {
		name string
		m    mode
	}{
		{"modeAddPaste", modeAddPaste},
		{"modeConfirmDelete", modeConfirmDelete},
		{"modeSearch", modeSearch},
		{"modeThresholds", modeThresholds},
	}
	for _, tc := range suppressed {
		t.Run(tc.name, func(t *testing.T) {
			app := &App{screen: screenAccounts, accounts: &model{mode: tc.m}}
			if app.acceptsGlobalKey() {
				t.Fatalf("Accounts %s must suppress global keys", tc.name)
			}
		})
	}

	// Non-Accounts tabs must always accept globals so q quits from them.
	tabs := []struct {
		name string
		s    screen
	}{
		{"Dashboard", screenDashboard},
		{"Activity", screenActivity},
		{"Settings", screenSettings},
	}
	for _, tc := range tabs {
		t.Run(tc.name, func(t *testing.T) {
			// Even in a hypothetical accounts-modal state, screen != accounts
			// so acceptsGlobalKey must return true.
			app := &App{screen: tc.s, accounts: &model{mode: modeAddPaste}}
			if !app.acceptsGlobalKey() {
				t.Fatalf("non-Accounts screen %s must always accept global keys", tc.name)
			}
		})
	}
}

// TestStatusbarHint_AdvertisesQuit — every tab gets the bottom statusbar,
// so users only learn about q-to-quit from that hint. Regression guard for
// the original bug where only Accounts' footer chips listed `q quit`.
func TestStatusbarHint_AdvertisesQuit(t *testing.T) {
	app := &App{screen: screenDashboard, accounts: &model{mode: modeList, daemonMode: "attached"}}
	state := app.statusbarState()
	if !strings.Contains(state.NavHint, "q quit") {
		t.Fatalf("statusbar nav hint should advertise `q quit`; got %q", state.NavHint)
	}
	if !strings.Contains(state.NavHint, "? help") {
		t.Fatalf("statusbar nav hint should still advertise `? help`; got %q", state.NavHint)
	}
}
