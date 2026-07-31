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
	stepIndent := 11     // "Step 1   " + 1-cell breathing room
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
	urlPanel := panel("", wrapToWidth(m.pendingURL, urlBoxW-4), urlBoxW)
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
		keyChip("ctrl+y", "copy URL"),
		keyChip("esc", "cancel"),
	)

	stack := card
	if line := m.addPasteStatusLine(); line != "" {
		stack += "\n" + line
	}
	stack += "\n" + footer
	return lipgloss.Place(w, m.height, lipgloss.Center, lipgloss.Center, stack)
}

// addPasteStatusLine renders the modal-local feedback row sitting between the
// card and the footer chips. ctrl+y writes a toast into m.statusMsg/statusErr;
// without surfacing it here the modal's full-screen Place would swallow it.
func (m *model) addPasteStatusLine() string {
	switch {
	case m.statusErr != "":
		return errStyle.Render("✗ " + m.statusErr)
	case m.statusMsg != "":
		return okStyle.Render("✓ " + m.statusMsg)
	}
	return ""
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

// wrapToWidth hard-wraps s into chunks of at most width runes per line. OAuth
// URLs are ASCII so a rune-count split matches visible-cell width; this avoids
// the panel's truncateANSI fallback which would chop the tail with `…`.
func wrapToWidth(s string, width int) string {
	if width < 1 || len(s) <= width {
		return s
	}
	runes := []rune(s)
	var b strings.Builder
	for i := 0; i < len(runes); i += width {
		if i > 0 {
			b.WriteByte('\n')
		}
		end := i + width
		if end > len(runes) {
			end = len(runes)
		}
		b.WriteString(string(runes[i:end]))
	}
	return b.String()
}
