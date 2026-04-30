package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// viewCooldown renders the cooldown preset picker per §4.4 — centered panel
// titled "Cooldown · <name>", 5 preset rows, footer chips.
func (m *model) viewCooldown() string {
	w := m.width
	if w <= 0 {
		return "Loading…"
	}

	panelW := w - 4
	if panelW > 60 {
		panelW = 60
	}
	if panelW < 40 {
		panelW = 40
	}
	innerW := panelW - 4

	a, _ := m.selected()

	var body strings.Builder
	for i, p := range cooldownPresets {
		row := "  " + p.label
		if i == m.cooldownChoice {
			row = cursorStyle.Render("▸ ") + p.label
			row = selectedStyle.Width(innerW).Render(row)
		}
		body.WriteString(row)
		if i != len(cooldownPresets)-1 {
			body.WriteString("\n")
		}
	}

	title := "Cooldown · " + a.Name
	card := panel(title, body.String(), panelW)

	footer := keyChipRow(
		keyChip("↑↓", "pick"),
		keyChip("enter", "apply"),
		keyChip("esc", "cancel"),
	)

	stack := card + "\n" + footer
	return lipgloss.Place(w, m.height, lipgloss.Center, lipgloss.Center, stack)
}
