package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// viewThresholds renders the per-account utilization-threshold editor.
// Layout mirrors viewCooldown — centered card titled with the account
// name, three rows for 5h / 7d / 7d-Sonnet, and key chips below.
//
// 100 means "do not skip on this window"; we show it as "off" so the
// semantic ("the selector ignores this dimension") is obvious.
func (m *model) viewThresholds() string {
	w := m.width
	if w <= 0 {
		return "Loading…"
	}

	panelW := w - 4
	if panelW > 60 {
		panelW = 60
	}
	if panelW < 44 {
		panelW = 44
	}
	innerW := panelW - 4

	a := m.accountByID(m.thresholdAccountID)

	labels := [3]string{"5h", "7d", "7d (Sonnet)"}
	var body strings.Builder
	for i, label := range labels {
		row := fmt.Sprintf("  %-12s", label)
		val := m.thresholdValues[i]
		var valStr string
		if val >= 100 {
			valStr = "off"
		} else {
			valStr = fmt.Sprintf("%2.0f%%", val)
		}
		if i == m.thresholdField {
			stepper := "− " + valStr + " +"
			row += stepper
			row = selectedStyle.Width(innerW).Render(row)
		} else {
			row += dimStyle.Render(valStr)
		}
		body.WriteString(row)
		if i != len(labels)-1 {
			body.WriteString("\n")
		}
	}

	hint := dimStyle.Render("threshold = window util at which this account is skipped; 100 = off")
	body.WriteString("\n\n" + hint)

	title := "Thresholds · " + a.Name
	if a.Name == "" {
		title = "Thresholds"
	}
	card := panel(title, body.String(), panelW)

	footer := keyChipRow(
		keyChip("←/→", "field"),
		keyChip("-/+", "step 5%"),
		keyChip("0", "off"),
		keyChip("enter", "save"),
		keyChip("esc", "cancel"),
	)

	stack := card + "\n" + footer
	return lipgloss.Place(w, m.height, lipgloss.Center, lipgloss.Center, stack)
}

// accountByID resolves the row that was selected when the threshold modal
// opened. Background refreshes can change the cursor or remove rows; we
// look up by id so the modal stays bound to the original row.
func (m *model) accountByID(id int64) Account {
	for _, a := range m.allAccounts {
		if a.ID == id {
			return a
		}
	}
	return Account{}
}
