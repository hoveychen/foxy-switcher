package tui

// settingsPage is the Phase 1 placeholder. Phase 4 replaces it with theme
// picker, polling/cooldown steppers, daemon info, and the danger-zone reset
// confirm.
type settingsPage struct {
	width, height int
}

func newSettingsPage() *settingsPage { return &settingsPage{} }

func (p *settingsPage) setSize(w, h int) {
	p.width = w
	p.height = h
}

func (p *settingsPage) view() string {
	return placeholderView(p.width, p.height, "Settings",
		"Theme · polling · thresholds · daemon info · reset",
		"Coming in Phase 4 — wired to /api/settings")
}
