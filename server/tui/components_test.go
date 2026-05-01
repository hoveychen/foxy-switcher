package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
)

func TestStatusDot(t *testing.T) {
	now := time.Now().UnixMilli()
	cases := []struct {
		name string
		acc  Account
		want string
	}{
		{"active", Account{Status: "active"}, "●"},
		{"disabled", Account{Status: "disabled"}, "○"},
		{"cooldown", Account{Status: "active", CooldownUntil: now + 60_000}, "◐"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := statusDot(tc.acc, now)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("statusDot %s: want char %q in %q", tc.name, tc.want, got)
			}
			if w := lipgloss.Width(got); w != 1 {
				t.Errorf("statusDot %s: visual width = %d, want 1", tc.name, w)
			}
		})
	}
}

func TestPlanBadge(t *testing.T) {
	got := planBadge("max20")
	if !strings.Contains(got, "[max20]") {
		t.Fatalf("planBadge missing literal: %q", got)
	}
	if w := lipgloss.Width(got); w != 7 {
		t.Errorf("planBadge visual width = %d, want 7", w)
	}
	if planBadge("") != "" {
		t.Error("planBadge(\"\") should produce empty string")
	}
}

func TestProgressBar(t *testing.T) {
	cases := []struct {
		name string
		util float64
	}{
		{"low", 42},
		{"mid", 80},
		{"high", 95},
		{"zero", 0},
		{"full", 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := progressBar(tc.util, 10)
			if w := lipgloss.Width(got); w != 16 {
				t.Errorf("progressBar(%v): visual width = %d, want 16 (10 bar + 2 gap + 4 pct)", tc.util, w)
			}
		})
	}
}

func TestProgressBarEmpty(t *testing.T) {
	got := progressBarEmpty(10)
	if !strings.Contains(got, "—") {
		t.Errorf("progressBarEmpty missing em-dash: %q", got)
	}
	if w := lipgloss.Width(got); w != 15 {
		t.Errorf("progressBarEmpty visual width = %d, want 15", w)
	}
}

func TestKeyChip(t *testing.T) {
	got := keyChip("a", "add")
	if !strings.Contains(got, "[a]") {
		t.Fatalf("keyChip missing bracketed key: %q", got)
	}
	if !strings.Contains(got, "add") {
		t.Fatalf("keyChip missing label: %q", got)
	}
	if w := lipgloss.Width(got); w != 7 {
		t.Errorf("keyChip visual width = %d, want 7 ([a] add)", w)
	}
}

func TestRowRail(t *testing.T) {
	cases := []struct {
		name     string
		selected bool
		inUse    bool
		want     string
	}{
		{"plain", false, false, " "},
		{"cursor", true, false, "▍"},
		{"in-use", false, true, "▶"},
		{"in-use+cursor", true, true, "▶"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rowRail(tc.selected, tc.inUse)
			if !strings.Contains(got, tc.want) {
				t.Errorf("rowRail(%v,%v) = %q, want %q", tc.selected, tc.inUse, got, tc.want)
			}
			if w := lipgloss.Width(got); w != 1 {
				t.Errorf("rowRail(%v,%v) width = %d, want 1", tc.selected, tc.inUse, w)
			}
		})
	}
}

func TestPanel(t *testing.T) {
	body := "line one\nline two"
	got := panel("USAGE", body, 30)

	lines := strings.Split(got, "\n")
	if len(lines) != 4 {
		t.Fatalf("panel produced %d lines, want 4 (top + 2 body + bottom): %q", len(lines), got)
	}
	if !strings.Contains(lines[0], "USAGE") {
		t.Errorf("top line missing title: %q", lines[0])
	}
	if !strings.Contains(lines[0], "╭") || !strings.Contains(lines[0], "╮") {
		t.Errorf("top line missing rounded corners: %q", lines[0])
	}
	if !strings.Contains(lines[3], "╰") || !strings.Contains(lines[3], "╯") {
		t.Errorf("bottom line missing rounded corners: %q", lines[3])
	}
	for i, ln := range lines {
		if w := lipgloss.Width(ln); w != 30 {
			t.Errorf("line %d width = %d, want 30: %q", i, w, ln)
		}
	}
}

func TestPanelHandlesNarrowWidth(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panel panicked on narrow width: %v", r)
		}
	}()
	got := panel("X", "hi", 4)
	if got == "" {
		t.Error("panel returned empty for narrow width")
	}
}
