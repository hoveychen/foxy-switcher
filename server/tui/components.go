package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// statusDot returns the colored Unicode glyph indicating account state.
// Pair with statusLabel so the status is conveyed by character + text, not
// color alone (matters on NO_COLOR / 8-color terminals).
func statusDot(a Account, nowMs int64) string {
	switch {
	case a.Status == "disabled":
		return lipgloss.NewStyle().Foreground(textTertiary).Render("○")
	case a.CooldownUntil > nowMs:
		return lipgloss.NewStyle().Foreground(tokenWarn).Render("◐")
	default:
		return lipgloss.NewStyle().Foreground(tokenOK).Render("●")
	}
}

// statusLabel renders the textual status with the same tone as statusDot.
func statusLabel(a Account, nowMs int64) string {
	if a.Status == "disabled" {
		return dimStyle.Render("disabled")
	}
	if a.CooldownUntil > nowMs {
		left := time.Until(time.UnixMilli(a.CooldownUntil)).Round(time.Second)
		return warnStyle.Render("cooldown " + left.String())
	}
	return okStyle.Render("active")
}

// planBadge renders the plan as `[max20]` with accent colors. Empty plan → "".
func planBadge(plan string) string {
	if plan == "" {
		return ""
	}
	return lipgloss.NewStyle().
		Foreground(accentBrand).
		Background(accentSoft).
		Render("[" + plan + "]")
}

// inUseBadge renders `[ In use ]` with inverted accent colors.
func inUseBadge() string {
	return lipgloss.NewStyle().
		Foreground(textOnAccent).
		Background(accentBrand).
		Render(" In use ")
}

const progressBarDefaultWidth = 10

// progressBar renders `████░░░░░░  42%`. width is the bar cell count; pass 0
// for the default. Tone is auto-selected from utilization (0–100).
func progressBar(util float64, width int) string {
	if width <= 0 {
		width = progressBarDefaultWidth
	}
	if util < 0 {
		util = 0
	}
	if util > 100 {
		util = 100
	}
	filled := int(util*float64(width)/100.0 + 0.5)
	if filled > width {
		filled = width
	}

	tone := tokenOK
	switch {
	case util >= 90:
		tone = tokenDanger
	case util >= 75:
		tone = tokenWarn
	}

	bar := lipgloss.NewStyle().Foreground(tone).Render(strings.Repeat("█", filled)) +
		lipgloss.NewStyle().Foreground(textTertiary).Render(strings.Repeat("░", width-filled))
	return bar + fmt.Sprintf("  %3.0f%%", util)
}

// progressBarEmpty renders the no-data variant: dim track + em-dash.
func progressBarEmpty(width int) string {
	if width <= 0 {
		width = progressBarDefaultWidth
	}
	bar := lipgloss.NewStyle().Foreground(textTertiary).Render(strings.Repeat("░", width))
	return bar + "    —"
}

// keyChip renders `[k] label`: bracketed key in accent colors, then label dim.
func keyChip(key, label string) string {
	chip := lipgloss.NewStyle().
		Foreground(accentBrand).
		Background(accentSoft).
		Render("[" + key + "]")
	return chip + " " + dimStyle.Render(label)
}

// keyChipRow joins chips with three spaces.
func keyChipRow(chips ...string) string {
	return strings.Join(chips, "   ")
}

// accentRail renders the leading column of a list row. Selected rows get a
// colored ▍ glyph; unselected get a single space so columns align.
func accentRail(selected bool) string {
	if selected {
		return lipgloss.NewStyle().Foreground(accentBrand).Render("▍")
	}
	return " "
}

// panel wraps body in a rounded border with title embedded in the top edge:
//
//	╭─ TITLE ─────────╮
//	│ body            │
//	╰─────────────────╯
//
// totalWidth includes both border columns; minimum 6.
func panel(title, body string, totalWidth int) string {
	if totalWidth < 6 {
		totalWidth = 6
	}
	border := lipgloss.NewStyle().Foreground(borderSubtle)

	titleRendered := titleStyle.Render(title)
	titleW := lipgloss.Width(titleRendered)
	// Top edge composition: ╭ + "─ " + title + " " + N×─ + ╮  (totalWidth chars)
	fillN := totalWidth - 5 - titleW
	if fillN < 1 {
		fillN = 1
	}
	top := border.Render("╭─ ") + titleRendered + border.Render(" "+strings.Repeat("─", fillN)+"╮")

	innerW := totalWidth - 4 // -2 borders -2 padding
	if innerW < 1 {
		innerW = 1
	}
	var bodyLines []string
	for _, ln := range strings.Split(body, "\n") {
		lnW := lipgloss.Width(ln)
		pad := innerW - lnW
		if pad < 0 {
			ln = truncateANSI(ln, innerW)
			pad = 0
		}
		bodyLines = append(bodyLines, border.Render("│ ")+ln+strings.Repeat(" ", pad)+border.Render(" │"))
	}

	bottom := border.Render("╰" + strings.Repeat("─", totalWidth-2) + "╯")

	parts := append([]string{top}, bodyLines...)
	parts = append(parts, bottom)
	return strings.Join(parts, "\n")
}

// truncateANSI naively truncates to width cells. Falls back to lipgloss-aware
// width comparison; if cutting mid-ANSI the residual codes leak, but content
// here is short controlled strings so we accept the risk.
func truncateANSI(s string, width int) string {
	if lipgloss.Width(s) <= width {
		return s
	}
	// Walk runes, count visible cells, stop when reaching width-1 (room for …).
	out := make([]rune, 0, len(s))
	cells := 0
	inEsc := false
	for _, r := range s {
		if r == 0x1b {
			inEsc = true
			out = append(out, r)
			continue
		}
		if inEsc {
			out = append(out, r)
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		if cells >= width-1 {
			break
		}
		out = append(out, r)
		cells++
	}
	return string(out) + "…"
}
