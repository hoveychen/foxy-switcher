package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// renderEmptyState produces the centered fox mascot + headline + CTA used when
// the account pool is empty. innerW is the cell width to center within.
//
//	 ╱\___/╲
//	(  o o  )
//	 \\  v //
//	  ─────
//	 no accounts yet
//	 press [a] to add your first
func renderEmptyState(innerW int) string {
	if innerW < 24 {
		innerW = 24
	}

	mascotStyle := lipgloss.NewStyle().Foreground(accentBrand).Bold(true)
	headline := lipgloss.NewStyle().Foreground(textPrimary).Bold(true).Render("no accounts in the pool yet")
	subline := dimStyle.Render("press ") +
		keyChip("a", "add") +
		dimStyle.Render(" to add your first")

	mascot := []string{
		` ╱\___/╲`,
		`(  o o  )`,
		` \\  v //`,
		`  ─────`,
	}

	var sb strings.Builder
	for _, ln := range mascot {
		sb.WriteString(centerLine(mascotStyle.Render(ln), innerW))
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
