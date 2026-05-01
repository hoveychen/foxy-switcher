package tui

import (
	"strings"
	"testing"
)

// TestViewListWideFillsHeight verifies the wide layout pads the panel body so
// the rendered output occupies exactly m.height rows. Without this, the
// footer floats up and leaves blank space at the bottom of the terminal.
func TestViewListWideFillsHeight(t *testing.T) {
	m := &model{
		width:  140,
		height: 40,
		accounts: []Account{
			{ID: 1, Name: "alpha@example.com", Plan: "Claude Team Premium", Status: "active"},
			{ID: 2, Name: "beta@example.com", Plan: "Claude Max", Status: "active"},
		},
		cred: CredStatus{ManagedAccountID: 2},
	}
	out := m.viewListWide()
	got := strings.Count(out, "\n") + 1
	if got != m.height {
		t.Fatalf("viewListWide produced %d rows, want %d", got, m.height)
	}
}

// TestRenderCredTextShowsName verifies the header displays the in-use
// account by name rather than just by id.
func TestRenderCredTextShowsName(t *testing.T) {
	m := &model{
		accounts: []Account{
			{ID: 7, Name: "chosen@example.com"},
			{ID: 8, Name: "other@example.com"},
		},
		cred: CredStatus{ManagedAccountID: 7},
	}
	got := m.renderCredText()
	if !strings.Contains(got, "chosen@example.com") {
		t.Fatalf("renderCredText missing account name: %q", got)
	}
	if strings.Contains(got, "acct #") {
		t.Errorf("renderCredText should prefer name over acct id when name is known: %q", got)
	}
}

// TestRenderAccountListMarksInUseAccount verifies the in-use row carries the
// `in use` chip while non-in-use rows do not.
func TestRenderAccountListMarksInUseAccount(t *testing.T) {
	m := &model{
		width:  140,
		height: 40,
		accounts: []Account{
			{ID: 1, Name: "alpha", Plan: "max20", Status: "active"},
			{ID: 2, Name: "beta", Plan: "max5", Status: "active"},
		},
		cred: CredStatus{ManagedAccountID: 2},
	}
	out := m.renderAccountList(48)
	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 list lines, got %d: %q", len(lines), out)
	}
	if !strings.Contains(lines[1], "in use") {
		t.Errorf("in-use account row missing chip: %q", lines[1])
	}
	if strings.Contains(lines[0], "in use") {
		t.Errorf("non-in-use row should not carry chip: %q", lines[0])
	}
}
