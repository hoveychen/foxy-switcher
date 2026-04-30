package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// viewAddPaste renders the OAuth add-account flow per §4.3:
//
//	╭─ Add account ────────────────────────────────╮
//	│                                                │
//	│  Step 1   Open this URL...                    │
//	│           ╭───────────────────────────────╮   │
//	│           │ https://...                    │   │
//	│           ╰───────────────────────────────╯   │
//	│                                                │
//	│  Step 2   Paste the resulting code...         │
//	│           › █                                  │
//	│                                                │
//	╰────────────────────────────────────────────────╯
//	  [enter] submit   [esc] cancel
func (m *model) viewAddPaste() string {
	w := m.width
	if w <= 0 {
		return "Loading…"
	}

	panelW := w - 4
	if panelW > 90 {
		panelW = 90
	}
	if panelW < 50 {
		panelW = 50
	}

	innerW := panelW - 4 // panel side borders + side padding
	stepIndent := 11    // "Step 1   " + 1-cell breathing room
	contentW := innerW - stepIndent
	if contentW < 20 {
		contentW = 20
	}

	stepLabel := func(n string) string {
		return dimStyle.Render("Step " + n)
	}

	var body strings.Builder

	body.WriteString(stepLabel("1") + "    Open this URL in a browser and approve:\n")
	body.WriteString("\n")

	urlBoxW := contentW
	if urlBoxW > 70 {
		urlBoxW = 70
	}
	urlPanel := panel("", m.pendingURL, urlBoxW)
	body.WriteString(indent(urlPanel, stepIndent))
	body.WriteString("\n")

	body.WriteString("\n")
	body.WriteString(stepLabel("2") + "    Paste the resulting code (")
	body.WriteString(dimStyle.Render("code#state"))
	body.WriteString("):\n")
	body.WriteString("\n")

	m.addPaste.Width = contentW - 2
	body.WriteString(indent(m.addPaste.View(), stepIndent))

	card := panel("Add account", body.String(), panelW)

	footer := keyChipRow(
		keyChip("enter", "submit"),
		keyChip("esc", "cancel"),
	)

	stack := card + "\n" + footer
	return lipgloss.Place(w, m.height, lipgloss.Center, lipgloss.Center, stack)
}

// indent prefixes each line of s with n spaces. Preserves trailing newlines.
func indent(s string, n int) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = pad + ln
	}
	return strings.Join(lines, "\n")
}
