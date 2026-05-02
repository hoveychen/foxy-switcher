package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// statusbarState bundles every dynamic field the bar needs in one struct so
// the App model can push a single value down to the renderer instead of
// threading 6+ args. Empty strings are treated as "skip this segment".
type statusbarState struct {
	// Left segment.
	DaemonDot   string // pre-rendered status glyph (statusDot output)
	DaemonLabel string // e.g. "owned :8451" / "attached :8451"

	// Center segment.
	Breadcrumb string // e.g. "Accounts · 12 active"

	// Right segment.
	NavHint         string // always-on keyboard hint, e.g. "1-4 pages · ? help"
	AutoSwitchDot   string // pre-rendered status glyph
	AutoSwitchLabel string // e.g. "auto-switch lru"
	Clock           string // e.g. "17:42"
}

// renderStatusbar produces the single-row bottom bar. The layout is three
// segments — left, center, right — separated by a thin separator glyph and
// padded with the bar's own subtle background so the bar looks like one
// continuous strip even when segments are short.
//
// width is the full terminal width. The bar always returns a string of
// exactly `width` cells so it sits flush against the bottom of the screen.
func renderStatusbar(s statusbarState, width int) string {
	if width <= 0 {
		return ""
	}

	bg := lipgloss.NewStyle().Background(bgSubtle).Foreground(textSecondary)

	left := joinSeg(s.DaemonDot, s.DaemonLabel)
	right := joinSeg(s.NavHint, s.AutoSwitchDot, s.AutoSwitchLabel, s.Clock)
	center := s.Breadcrumb

	leftStyled := bg.Render(" " + left + " ")
	rightStyled := bg.Render(" " + right + " ")
	centerStyled := ""
	if center != "" {
		centerStyled = bg.Render(center)
	}

	leftW := lipgloss.Width(leftStyled)
	rightW := lipgloss.Width(rightStyled)
	centerW := lipgloss.Width(centerStyled)

	// Center the breadcrumb within the residual space.
	residual := width - leftW - rightW
	if residual < 0 {
		residual = 0
	}
	if centerW > residual {
		centerStyled = truncateANSI(centerStyled, residual)
		centerW = lipgloss.Width(centerStyled)
	}
	leftPad := (residual - centerW) / 2
	rightPad := residual - centerW - leftPad
	if leftPad < 0 {
		leftPad = 0
	}
	if rightPad < 0 {
		rightPad = 0
	}

	return leftStyled +
		bg.Render(strings.Repeat(" ", leftPad)) +
		centerStyled +
		bg.Render(strings.Repeat(" ", rightPad)) +
		rightStyled
}

// joinSeg concatenates non-empty parts with single spaces — e.g. dot + label
// renders as "● owned :8451" with one space between, no trailing space if
// either part is missing.
func joinSeg(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if p == "" {
			continue
		}
		keep = append(keep, p)
	}
	return strings.Join(keep, " ")
}
