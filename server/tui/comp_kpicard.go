package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// kpiCard composes a panel with three rows: eyebrow caption, big number, and
// a trailing line that may be a trend hint, a soft-bg pill, or just dim
// metadata. The big number is rendered bold + accent so it dominates the card
// even at a glance.
//
//	╭────────────────╮
//	│ POOL           │
//	│ 12 active      │
//	│ ↑ 2 today      │
//	╰────────────────╯
//
// width is the panel's outer width (≥10). tone selects the eyebrow color so
// the user can scan a row of cards and immediately spot the warning ones.
func kpiCard(eyebrow, big, trail string, tone pillTone, width int) string {
	if width < 10 {
		width = 10
	}
	innerW := width - 4
	if innerW < 4 {
		innerW = 4
	}

	eyebrowStyle := lipgloss.NewStyle().Foreground(textTertiary).Bold(true)
	switch tone {
	case pillOK:
		eyebrowStyle = eyebrowStyle.Foreground(tokenOK)
	case pillWarn:
		eyebrowStyle = eyebrowStyle.Foreground(tokenWarn)
	case pillDanger:
		eyebrowStyle = eyebrowStyle.Foreground(tokenDanger)
	case pillInfo:
		eyebrowStyle = eyebrowStyle.Foreground(tokenInfo)
	case pillAccent:
		eyebrowStyle = eyebrowStyle.Foreground(accentBrand)
	}

	bigStyle := lipgloss.NewStyle().Foreground(textPrimary).Bold(true)

	body := strings.Join([]string{
		eyebrowStyle.Render(strings.ToUpper(eyebrow)),
		bigStyle.Render(big),
		dimStyle.Render(trail),
	}, "\n")

	// Use empty title — the eyebrow already serves that role and we don't want
	// a second header band stealing the visual hierarchy.
	return panel("", body, width)
}
