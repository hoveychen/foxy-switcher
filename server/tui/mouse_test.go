package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestListRowAtWide(t *testing.T) {
	m := &model{
		width:  140,
		height: 40,
		accounts: []Account{
			{ID: 1, Name: "alpha"},
			{ID: 2, Name: "beta"},
			{ID: 3, Name: "gamma"},
		},
	}
	cases := []struct {
		name    string
		y       int
		want    int
		wantOK  bool
		comment string
	}{
		{"above panel", 1, 0, false, "header / blank rows are unmapped"},
		{"panel top border", 2, 0, false, "border is not a row"},
		{"first account", 3, 0, true, ""},
		{"second account", 4, 1, true, ""},
		{"third account", 5, 2, true, ""},
		{"padding row", 6, 0, false, "below last account, in padding"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := m.listRowAt(0, tc.y)
			if ok != tc.wantOK {
				t.Fatalf("listRowAt(_, %d) ok=%v, want %v", tc.y, ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Errorf("listRowAt(_, %d) = %d, want %d (%s)", tc.y, got, tc.want, tc.comment)
			}
		})
	}
}

func TestListRowAtRegularSkipsInlineDetail(t *testing.T) {
	// Width 100 → layoutRegular; cursor=1 means account[1]'s 5 inline detail
	// rows sit between accounts 1 and 2.
	m := &model{
		width:  100,
		height: 40,
		cursor: 1,
		accounts: []Account{
			{ID: 1, Name: "alpha"},
			{ID: 2, Name: "beta"},
			{ID: 3, Name: "gamma"},
		},
	}
	// account 0 → y=3, account 1 (cursor) → y=4, inline detail → y=5..9,
	// account 2 → y=10.
	if idx, ok := m.listRowAt(0, 3); !ok || idx != 0 {
		t.Errorf("y=3 should map to account 0, got idx=%d ok=%v", idx, ok)
	}
	if idx, ok := m.listRowAt(0, 4); !ok || idx != 1 {
		t.Errorf("y=4 should map to account 1 (cursor), got idx=%d ok=%v", idx, ok)
	}
	if idx, ok := m.listRowAt(0, 7); !ok || idx != 1 {
		t.Errorf("y=7 (inside inline detail) should stick to cursor account 1, got idx=%d ok=%v", idx, ok)
	}
	if idx, ok := m.listRowAt(0, 10); !ok || idx != 2 {
		t.Errorf("y=10 should map to account 2 (after the 5-line inline detail), got idx=%d ok=%v", idx, ok)
	}
}

func TestFooterChipAt(t *testing.T) {
	m := &model{width: 140, height: 40}
	// Single-row footer at y = m.height - 1 = 39.
	footerY := m.height - 1

	// Walk x positions to find the [a] add chip start. The first chip is
	// "[↑↓] move" which is informational; the second chip is "[a] add".
	defs := m.footerChipDefs()
	chips := m.footerChipsRendered()
	x := 0
	for i, c := range chips {
		if defs[i].emit == "a" {
			break
		}
		x += lipgloss.Width(c) + 3
	}
	// Click in the middle of the [a] add chip.
	got, ok := m.footerChipAt(x+1, footerY)
	if !ok || got != "a" {
		t.Errorf("clicking [a] add chip should emit \"a\"; got %q ok=%v", got, ok)
	}

	// Clicking outside the footer should miss.
	if _, ok := m.footerChipAt(0, 0); ok {
		t.Errorf("click at (0,0) should not hit footer")
	}

	// Clicking the informational [↑↓] move chip should miss (emit == "").
	if _, ok := m.footerChipAt(0, footerY); ok {
		t.Errorf("click on informational chip should not emit a key")
	}
}

func TestHandleMouseWheelMovesCursor(t *testing.T) {
	m := &model{
		width:  140,
		height: 40,
		cursor: 1,
		accounts: []Account{
			{ID: 1, Name: "alpha"},
			{ID: 2, Name: "beta"},
			{ID: 3, Name: "gamma"},
		},
	}
	if _, _ = m.handleMouse(tea.MouseMsg{
		Button: tea.MouseButtonWheelUp,
		Action: tea.MouseActionPress,
	}); m.cursor != 0 {
		t.Errorf("wheel up: cursor = %d, want 0", m.cursor)
	}
	if _, _ = m.handleMouse(tea.MouseMsg{
		Button: tea.MouseButtonWheelDown,
		Action: tea.MouseActionPress,
	}); m.cursor != 1 {
		t.Errorf("wheel down: cursor = %d, want 1", m.cursor)
	}
}

func TestHandleMouseLeftClickMovesCursor(t *testing.T) {
	m := &model{
		width:  140,
		height: 40,
		cursor: 0,
		accounts: []Account{
			{ID: 1, Name: "alpha"},
			{ID: 2, Name: "beta"},
			{ID: 3, Name: "gamma"},
		},
	}
	// Click at y=5 → account[2] in wide layout.
	m.handleMouse(tea.MouseMsg{
		X:      10,
		Y:      5,
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
	})
	if m.cursor != 2 {
		t.Errorf("left-click on account row: cursor = %d, want 2", m.cursor)
	}
}

