package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// viewOnboarding renders whichever onboarding phase is active. Caller
// (App.View) should bypass the regular sidebar/page chrome when the
// onboarding overlay is up so the user can't see-but-not-interact-with
// stale page state behind it.
func (a *App) viewOnboarding() string {
	const targetWidth = 64
	w := targetWidth
	if a.width > 0 && w > a.width-4 {
		w = a.width - 4
	}
	if w < 36 {
		w = 36
	}

	var body string
	var title string
	switch a.onboarding.phase {
	case onboardingChoose:
		title = "Where should your accounts live?"
		body = a.viewOnboardingChoose(w)
	case onboardingCloudInput:
		title = "Connect to your vault"
		body = a.viewOnboardingCloudInput(w)
	case onboardingCloudPair:
		title = "Approve this device on your vault"
		body = a.viewOnboardingCloudPair(w)
	case onboardingDone:
		title = "Pairing saved"
		body = a.viewOnboardingDone(w)
	default:
		return ""
	}

	modal := panel(title, body, w)
	bg := strings.Repeat(strings.Repeat(" ", a.width)+"\n", a.height)
	return centerOverlay(bg, modal, a.width, a.height)
}

func (a *App) viewOnboardingChoose(w int) string {
	innerW := w - 4

	intro := wrapLine(
		"Foxy stores your Claude tokens in a vault. The vault can run on "+
			"this device, or on a server you own that other devices share.",
		innerW,
	)

	cards := []struct {
		idx   int
		title string
		desc  string
	}{
		{0, "1) Local vault", "Vault and agent both run on this device. Quickest way to start."},
		{1, "2) Cloud vault", "Pair this device with a vault you've deployed on a server."},
	}

	var lines []string
	lines = append(lines, intro...)
	lines = append(lines, "")
	for _, c := range cards {
		marker := "  "
		titleRendered := c.title
		if a.onboarding.chooseCursor == c.idx {
			marker = cursorStyle.Render("▶ ")
			titleRendered = titleStyle.Render(c.title)
		}
		lines = append(lines, marker+titleRendered)
		for _, l := range wrapLine("    "+c.desc, innerW) {
			lines = append(lines, dimStyle.Render(l))
		}
		lines = append(lines, "")
	}
	lines = append(lines, dimStyle.Render("←/→ to move · enter to confirm · 1/2 shortcut · ctrl+c quit"))
	return strings.Join(lines, "\n")
}

func (a *App) viewOnboardingCloudInput(w int) string {
	innerW := w - 4

	docHint := wrapLine(
		"How to deploy a vault → "+onboardingDeployURL,
		innerW,
	)

	var lines []string
	for _, l := range docHint {
		lines = append(lines, dimStyle.Render(l))
	}
	lines = append(lines, "")
	lines = append(lines, headerStyle.Render("Vault URL"))
	lines = append(lines, a.onboarding.urlInput.View())
	lines = append(lines, "")
	for _, l := range wrapLine("Reachable HTTPS URL of the vault you've deployed.", innerW) {
		lines = append(lines, dimStyle.Render(l))
	}
	if a.onboarding.errMsg != "" {
		lines = append(lines, "")
		errStyle := lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#c33", Dark: "#f87171"})
		for _, l := range wrapLine(a.onboarding.errMsg, innerW) {
			lines = append(lines, errStyle.Render(l))
		}
	}
	lines = append(lines, "")
	lines = append(lines, dimStyle.Render("enter to continue · esc to go back · ctrl+c quit"))
	return strings.Join(lines, "\n")
}

func (a *App) viewOnboardingCloudPair(w int) string {
	innerW := w - 4

	var lines []string
	lines = append(lines, wrapLine("Open the verification URL in a browser, sign in to the vault, and enter this code:", innerW)...)
	lines = append(lines, "")
	if a.onboarding.userCode != "" {
		codeStyle := lipgloss.NewStyle().Bold(true).Foreground(accentBrand)
		lines = append(lines, "    "+codeStyle.Render(a.onboarding.userCode))
		lines = append(lines, "")
	}
	if a.onboarding.verifURL != "" {
		lines = append(lines, dimStyle.Render("URL: ")+a.onboarding.verifURL)
		lines = append(lines, "")
	}
	if a.onboarding.errMsg != "" {
		for _, l := range wrapLine(a.onboarding.errMsg, innerW) {
			lines = append(lines, dimStyle.Render(l))
		}
	} else {
		lines = append(lines, dimStyle.Render("Waiting for approval — this view will update automatically."))
	}
	lines = append(lines, "")
	lines = append(lines, dimStyle.Render("esc to cancel · ctrl+c quit"))
	return strings.Join(lines, "\n")
}

func (a *App) viewOnboardingDone(w int) string {
	innerW := w - 4

	var lines []string
	lines = append(lines, wrapLine("This device is paired and the bearer token is saved.", innerW)...)
	lines = append(lines, "")
	lines = append(lines, wrapLine("Quit and re-run `foxy-switcher tui` so the daemon boots in agent mode and starts using the cloud vault.", innerW)...)
	lines = append(lines, "")
	lines = append(lines, dimStyle.Render("q / enter / esc to quit"))
	return strings.Join(lines, "\n")
}

// wrapLine wraps a single string at word boundaries to fit `width`. The
// TUI elsewhere relies on lipgloss for sophisticated text layout, but
// onboarding bodies are short prose so a hand-rolled greedy wrapper
// keeps the dependency footprint small.
func wrapLine(s string, width int) []string {
	if width <= 0 {
		return []string{s}
	}
	words := strings.Fields(s)
	if len(words) == 0 {
		return []string{""}
	}
	var lines []string
	current := ""
	for _, w := range words {
		if current == "" {
			current = w
			continue
		}
		if lipgloss.Width(current)+1+lipgloss.Width(w) > width {
			lines = append(lines, current)
			current = w
		} else {
			current += " " + w
		}
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}
