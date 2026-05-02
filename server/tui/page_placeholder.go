package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// placeholderView renders a centered "coming soon" card for pages that
// haven't been wired to real data yet. Phase-by-phase the file count
// shrinks: each replacement page swaps this call for its own real view.
func placeholderView(width, height int, title, subtitle, eta string) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	cardW := width - 6
	if cardW > 60 {
		cardW = 60
	}
	if cardW < 24 {
		cardW = 24
	}

	body := strings.Join([]string{
		titleStyle.Render(title),
		dimStyle.Render(subtitle),
		"",
		pill(eta, pillAccent, pillSoft),
	}, "\n")

	card := panel("", body, cardW)

	cardLines := strings.Count(card, "\n") + 1
	topPad := (height - cardLines) / 2
	if topPad < 0 {
		topPad = 0
	}
	pad := lipgloss.NewStyle().Render(strings.Repeat("\n", topPad))

	leftPad := (width - cardW) / 2
	if leftPad < 0 {
		leftPad = 0
	}
	indent := strings.Repeat(" ", leftPad)
	var indented []string
	for _, ln := range strings.Split(card, "\n") {
		indented = append(indented, indent+ln)
	}
	return pad + strings.Join(indented, "\n")
}
