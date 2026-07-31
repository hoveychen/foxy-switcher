package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// screen identifies which top-level page is rendered in the main pane. Order
// is the cycle order — sidebar nav items, `1`–`4` digit shortcuts, and
// `nextScreen` all walk this list.
type screen int

const (
	screenDashboard screen = iota
	screenAccounts
	screenActivity
	screenSettings
)

// sidebarMode picks the chrome layout based on terminal width. The transitions
// are intentionally aggressive: at narrow widths the sidebar would consume too
// much horizontal real estate from the actual content, so we drop to a single
// top-row tab strip and reclaim the width.
type sidebarMode int

const (
	sidebarExpanded  sidebarMode = iota // ≥120 cols — full label + icon column
	sidebarCollapsed                    // 80–119 cols — icon-only column
	sidebarTabs                         // <80 cols — top-row tab strip, no left column
)

// sidebarMode breakpoints chosen to match `view_list.go`'s layout tiers so the
// chrome and content reflow at the same widths.
func pickSidebarMode(width int) sidebarMode {
	switch {
	case width >= 120:
		return sidebarExpanded
	case width >= 80:
		return sidebarCollapsed
	default:
		return sidebarTabs
	}
}

const (
	sidebarWidthExpanded  = 22
	sidebarWidthCollapsed = 4
)

// sidebarWidthFor returns the column count consumed by the sidebar in a given
// mode. Tabs mode reserves zero columns horizontally (it's a top row instead),
// so callers laying out the main pane can subtract this directly.
func sidebarWidthFor(mode sidebarMode) int {
	switch mode {
	case sidebarExpanded:
		return sidebarWidthExpanded
	case sidebarCollapsed:
		return sidebarWidthCollapsed
	default:
		return 0
	}
}

// sidebarItem describes one nav entry. icon is single-cell unicode; label is
// the long-form name shown in expanded mode and as the tab text in tabs mode;
// shortcut is the digit key the user can press anywhere to jump to it.
type sidebarItem struct {
	id       screen
	icon     string
	label    string
	shortcut rune
}

func sidebarItems() []sidebarItem {
	return []sidebarItem{
		{screenDashboard, "◉", "Dashboard", '1'},
		{screenAccounts, "◐", "Accounts", '2'},
		{screenActivity, "◷", "Activity", '3'},
		{screenSettings, "⚙", "Settings", '4'},
	}
}

// renderSidebar produces the left rail in expanded/collapsed modes. height is
// the total inner height the rail must fill (excluding statusbar). Returns ""
// for sidebarTabs mode — callers should call renderSidebarTabs instead.
//
// Layout (expanded):
//
//	🦊 FOXY              <- gradient mark
//	                     <- spacer
//	▎◉ Dashboard         <- active row: 1-cell rail + soft accent bg
//	 ◐ Accounts
//	 ◷ Activity
//	 ⚙ Settings
//
// Daemon health is rendered by the bottom statusbar instead of duplicated
// here.
func renderSidebar(mode sidebarMode, active screen, height int) string {
	if mode == sidebarTabs {
		return ""
	}
	width := sidebarWidthFor(mode)

	header := renderSidebarHeader(mode, width)
	nav := renderSidebarNav(mode, active, width)

	headerLines := strings.Count(header, "\n") + 1
	if header == "" {
		headerLines = 0
	}
	navLines := strings.Count(nav, "\n") + 1

	used := headerLines + 1 + navLines // +1 spacer after header
	pad := height - used
	if pad < 0 {
		pad = 0
	}

	rail := lipgloss.NewStyle().Foreground(borderSubtle).Render(strings.Repeat(" ", width))
	parts := []string{header, rail, nav}
	for i := 0; i < pad; i++ {
		parts = append(parts, strings.Repeat(" ", width))
	}
	return strings.Join(parts, "\n")
}

// renderSidebarHeader: gradient `🦊 FOXY` (expanded) or just `🦊` (collapsed).
func renderSidebarHeader(mode sidebarMode, width int) string {
	mark := "🦊 FOXY"
	if mode == sidebarCollapsed {
		mark = "🦊"
	}
	g := gradient(mark, gradientFrom, gradientTo)
	return padRight(g, width)
}

// renderSidebarNav lays out the nav rows. The active row gets a 1-cell orange
// rail on the left edge plus a soft-accent background, distinguishing it from
// hover/dim states without relying on color alone.
func renderSidebarNav(mode sidebarMode, active screen, width int) string {
	items := sidebarItems()
	var rows []string
	for _, it := range items {
		row := renderSidebarRow(mode, it, it.id == active, width)
		rows = append(rows, row)
	}
	return strings.Join(rows, "\n")
}

func renderSidebarRow(mode sidebarMode, it sidebarItem, isActive bool, width int) string {
	railChar := " "
	railFG := borderSubtle
	if isActive {
		railChar = "▎"
		railFG = accentBrand
	}
	rail := lipgloss.NewStyle().Foreground(railFG).Render(railChar)

	innerWidth := width - 1 // -1 for rail
	if innerWidth < 1 {
		innerWidth = 1
	}

	bodyStyle := lipgloss.NewStyle().Foreground(textSecondary)
	if isActive {
		bodyStyle = lipgloss.NewStyle().Foreground(textPrimary).Background(accentSoft).Bold(true)
	}

	if mode == sidebarExpanded {
		// `<icon> <label>` left, dim shortcut digit right-aligned. Without
		// the digit hint, keyboard users had no way to discover that 1–4
		// switch pages.
		left := it.icon + " " + it.label
		hint := dimStyle.Render(string(it.shortcut))
		hintW := lipgloss.Width(hint)
		leftW := lipgloss.Width(left)
		gap := innerWidth - leftW - hintW
		if gap < 1 {
			// No room for the hint — fall back to label-only with truncation.
			body := left
			if leftW > innerWidth {
				body = truncateANSI(body, innerWidth)
			} else {
				body = body + strings.Repeat(" ", innerWidth-leftW)
			}
			return rail + bodyStyle.Render(body)
		}
		body := left + strings.Repeat(" ", gap) + hint
		return rail + bodyStyle.Render(body)
	}

	// Collapsed (3 cells inside the rail): show `<icon><digit>` so the
	// digit is visible even without the label column.
	body := it.icon + string(it.shortcut)
	bodyW := lipgloss.Width(body)
	if bodyW < innerWidth {
		body = body + strings.Repeat(" ", innerWidth-bodyW)
	} else if bodyW > innerWidth {
		body = truncateANSI(body, innerWidth)
	}
	return rail + bodyStyle.Render(body)
}

// renderSidebarTabs is the narrow-mode replacement for the left rail — a
// single row of pills. Active tab uses the filled accent pill; the rest use
// the neutral pill so the visual hierarchy still reads at 60 cols.
func renderSidebarTabs(active screen, width int) string {
	items := sidebarItems()
	var chips []string
	for _, it := range items {
		text := string(it.shortcut) + " " + it.label
		var p string
		if it.id == active {
			p = pill(text, pillAccent, pillFilled)
		} else {
			p = pill(text, pillNeutral, pillSoft)
		}
		chips = append(chips, p)
	}
	row := strings.Join(chips, " ")
	return padRight(row, width)
}

// padRight pads or truncates s to exactly width visual cells.
func padRight(s string, width int) string {
	w := lipgloss.Width(s)
	if w == width {
		return s
	}
	if w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return truncateANSI(s, width)
}
