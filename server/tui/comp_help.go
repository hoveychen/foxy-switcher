package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// helpEntry is a single key-binding row in the help modal. desc is shown to
// the right of the key chip; keep it short — multi-line help belongs in
// online docs, not this overlay.
type helpEntry struct {
	key  string
	desc string
}

// helpSection groups entries under a heading (e.g. "Global", "Accounts").
type helpSection struct {
	title   string
	entries []helpEntry
}

// helpFor returns the section list relevant to the active screen. Global
// always appears first; per-page sections only show when their page is
// active to avoid drowning new users in shortcuts they can't trigger.
func helpFor(active screen) []helpSection {
	global := helpSection{
		title: "Global",
		entries: []helpEntry{
			{"1-4", "switch page"},
			{"T", "next theme"},
			{"?", "toggle this help"},
			{"q", "quit"},
		},
	}
	switch active {
	case screenAccounts:
		return []helpSection{global, helpAccounts()}
	case screenActivity:
		return []helpSection{global, helpActivity()}
	case screenSettings:
		return []helpSection{global, helpSettings()}
	default:
		return []helpSection{global, helpDashboard()}
	}
}

func helpDashboard() helpSection {
	return helpSection{
		title:   "Dashboard",
		entries: []helpEntry{{"r", "refresh now"}},
	}
}

func helpAccounts() helpSection {
	return helpSection{
		title: "Accounts",
		entries: []helpEntry{
			{"↑/↓", "move cursor"},
			{"enter", "use selected"},
			{"a", "add account"},
			{"u", "pin in-use"},
			{"p", "pause / resume"},
			{"r", "refresh now"},
			{"d", "delete"},
			{"/", "search by name"},
			{"f / ⇥", "cycle filter"},
			{"t", "edit thresholds"},
			{"A", "toggle auto-switch"},
			{"P", "cycle policy"},
		},
	}
}

func helpActivity() helpSection {
	return helpSection{
		title: "Activity",
		entries: []helpEntry{
			{"↑/↓", "scroll"},
			{"g/G", "top / bottom"},
			{"f", "cycle filter"},
		},
	}
}

func helpSettings() helpSection {
	return helpSection{
		title: "Settings",
		entries: []helpEntry{
			{"↑/↓", "move row"},
			{"←/→", "change value"},
			{"enter", "activate"},
		},
	}
}

// renderHelpModal returns a centered overlay panel listing the bindings for
// `active`. termWidth/termHeight are used to size and center; the panel
// auto-shrinks to fit narrow terminals (down to 36×10).
func renderHelpModal(active screen, termWidth, termHeight int) string {
	sections := helpFor(active)

	const targetWidth = 56
	w := targetWidth
	if w > termWidth-4 {
		w = termWidth - 4
	}
	if w < 36 {
		w = 36
	}

	var bodyLines []string
	for i, sec := range sections {
		if i > 0 {
			bodyLines = append(bodyLines, "")
		}
		bodyLines = append(bodyLines, headerStyle.Render(sec.title))
		for _, e := range sec.entries {
			row := fmt.Sprintf("  %s  %s",
				pill(e.key, pillAccent, pillSoft),
				dimStyle.Render(e.desc))
			bodyLines = append(bodyLines, row)
		}
	}
	bodyLines = append(bodyLines, "")
	bodyLines = append(bodyLines, helpStyle.Render("Press ? or esc to close"))

	body := strings.Join(bodyLines, "\n")
	return panel("Keyboard shortcuts", body, w)
}

// helpFooterHint is the single-line hint shown in the chrome footer when the
// modal is closed — points users at `?` and the most-used global keys.
func helpFooterHint() string {
	return strings.Join([]string{
		keyChip("?", "help"),
		keyChip("1-4", "pages"),
		keyChip("T", "theme"),
		keyChip("q", "quit"),
	}, "   ")
}

// centerOverlay places overlay over a background-sized box of (width, height)
// cells. Both inputs may be empty; the result is always exactly width×height
// when background already is. Used by the App view to lay help / confirm /
// thresholds modals on top of the chrome.
func centerOverlay(background, overlay string, width, height int) string {
	if overlay == "" {
		return background
	}
	bgLines := strings.Split(background, "\n")
	for len(bgLines) < height {
		bgLines = append(bgLines, strings.Repeat(" ", width))
	}
	ovLines := strings.Split(overlay, "\n")
	ovH := len(ovLines)
	ovW := 0
	for _, ln := range ovLines {
		if w := lipgloss.Width(ln); w > ovW {
			ovW = w
		}
	}
	top := (height - ovH) / 2
	left := (width - ovW) / 2
	if top < 0 {
		top = 0
	}
	if left < 0 {
		left = 0
	}
	for i, ovLn := range ovLines {
		row := top + i
		if row < 0 || row >= len(bgLines) {
			continue
		}
		bgLn := bgLines[row]
		bgW := lipgloss.Width(bgLn)
		if bgW < width {
			bgLn = bgLn + strings.Repeat(" ", width-bgW)
		}
		// Naive overlay: replace the slice [left, left+ovW) with the overlay
		// line. ANSI-aware splice is hard; for terminal modals the overlay
		// width is short and the background under it is mostly spaces, so the
		// visual artifact is acceptable.
		runes := []rune(stripANSI(bgLn))
		for j := 0; j < ovW && left+j < len(runes); j++ {
			runes[left+j] = ' '
		}
		bgLines[row] = string(runes[:left]) + ovLn + string(runes[left+ovW:])
	}
	return strings.Join(bgLines, "\n")
}

// stripANSI removes CSI sequences. Used by the overlay splicer to operate on
// raw cells without leaking style boundaries across the overlay seam.
func stripANSI(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		if r == 0x1b {
			in = true
			continue
		}
		if in {
			if r == 'm' {
				in = false
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
