package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// renderEmptyState produces the centered foxy logo + headline + CTA used when
// the account pool is empty. innerW is the cell width to center within.
//
// The mascot is the embedded foxy-icon.png rendered as 24×12 truecolor ANSI
// half-blocks (see comp_logo.go). On terminals without 24-bit color the
// glyphs still render but lose the brand colors.
func renderEmptyState(innerW int) string {
	if innerW < 24 {
		innerW = 24
	}

	headline := lipgloss.NewStyle().Foreground(textPrimary).Bold(true).Render("no accounts in the pool yet")
	subline := dimStyle.Render("press ") +
		keyChip("a", "add") +
		dimStyle.Render(" to add your first")

	var sb strings.Builder
	logo := renderFoxLogo(24, 12)
	for _, ln := range strings.Split(logo, "\n") {
		sb.WriteString(centerLine(ln, innerW))
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	sb.WriteString(centerLine(headline, innerW))
	sb.WriteString("\n\n")
	sb.WriteString(centerLine(subline, innerW))
	return sb.String()
}

// centerLine pads s with leading spaces to center within width cells. Width is
// measured visually (lipgloss-aware).
func centerLine(s string, width int) string {
	w := lipgloss.Width(s)
	if w >= width {
		return s
	}
	left := (width - w) / 2
	return strings.Repeat(" ", left) + s
}
