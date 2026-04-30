package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// viewConfirmDelete renders the destructive-confirm modal per §3.10 — small
// centered panel with danger-toned border and key chips below.
func (m *model) viewConfirmDelete() string {
	w := m.width
	if w <= 0 {
		return "Loading…"
	}

	panelW := w - 4
	if panelW > 50 {
		panelW = 50
	}
	if panelW < 36 {
		panelW = 36
	}

	a, _ := m.selected()

	var body strings.Builder
	body.WriteString("Permanently remove ")
	body.WriteString(a.Name)
	body.WriteString("?\n")
	if a.Email != "" {
		body.WriteString(dimStyle.Render(a.Email))
		body.WriteString("\n")
	}
	body.WriteString("\n")
	body.WriteString(dimStyle.Render("refresh_token will be discarded;"))
	body.WriteString("\n")
	body.WriteString(dimStyle.Render("Anthropic keeps no state to clean."))

	card := panel("Delete account", body.String(), panelW)

	footer := keyChipRow(
		keyChip("y", "confirm"),
		keyChip("n", "cancel"),
	)

	stack := card + "\n" + footer
	return lipgloss.Place(w, m.height, lipgloss.Center, lipgloss.Center, stack)
}
