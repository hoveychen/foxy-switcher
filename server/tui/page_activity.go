package tui

// activityPage is the Phase 1 placeholder. Phase 3 replaces it with a
// `bubbles/table` + `bubbles/viewport` driven event timeline plus filter
// chips.
type activityPage struct {
	width, height int
}

func newActivityPage() *activityPage { return &activityPage{} }

func (p *activityPage) setSize(w, h int) {
	p.width = w
	p.height = h
}

func (p *activityPage) view() string {
	return placeholderView(p.width, p.height, "Activity",
		"Switches, refreshes, errors with filter chips",
		"Coming in Phase 3 — wired to /api/activity")
}
