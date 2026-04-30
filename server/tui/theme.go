package tui

import "github.com/charmbracelet/lipgloss"

// Theme tokens — see docs/TUI_DESIGN_SYSTEM.md for the source of truth.
// The reference palette (§2.1.1) feeds the system tokens (§2.1.2); only the
// system tokens should appear in component code.

const (
	cOrange400 = "#ff9233"
	cOrange500 = "#ff7a1a"

	cGray500  = "#8e8e96"
	cGray600  = "#6e6e76"
	cGray1000 = "#0d0d0f"

	cGreen500 = "#22c55e"
	cGreen400 = "#4ade80"
	cAmber500 = "#f59e0b"
	cAmber400 = "#fbbf24"
	cRed500   = "#ef4444"
	cRed400   = "#f87171"
)

var (
	textPrimary   = lipgloss.AdaptiveColor{Light: cGray1000, Dark: "#f5f5f7"}
	textSecondary = lipgloss.AdaptiveColor{Light: cGray600, Dark: "#b0b0b6"}
	textTertiary  = lipgloss.AdaptiveColor{Light: cGray500, Dark: "#8a8a92"}

	bgSelected = lipgloss.AdaptiveColor{Light: "#fff7ed", Dark: "#3f291c"}

	borderSubtle = lipgloss.AdaptiveColor{Light: "#b8b8c0", Dark: cGray600}

	accentBrand  = lipgloss.AdaptiveColor{Light: cOrange500, Dark: cOrange400}
	accentSoft   = lipgloss.AdaptiveColor{Light: "#ffe8d1", Dark: "#3f291c"}
	textOnAccent = lipgloss.AdaptiveColor{Light: "#ffffff", Dark: "#ffffff"}

	tokenOK     = lipgloss.AdaptiveColor{Light: cGreen500, Dark: cGreen400}
	tokenWarn   = lipgloss.AdaptiveColor{Light: cAmber500, Dark: cAmber400}
	tokenDanger = lipgloss.AdaptiveColor{Light: cRed500, Dark: cRed400}
)

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(accentBrand)
	headerStyle   = lipgloss.NewStyle().Bold(true).Foreground(textSecondary)
	cursorStyle   = lipgloss.NewStyle().Foreground(accentBrand)
	selectedStyle = lipgloss.NewStyle().Foreground(textPrimary).Background(bgSelected)
	dimStyle      = lipgloss.NewStyle().Foreground(textSecondary)
	okStyle       = lipgloss.NewStyle().Foreground(tokenOK)
	warnStyle     = lipgloss.NewStyle().Foreground(tokenWarn)
	errStyle      = lipgloss.NewStyle().Foreground(tokenDanger)
	helpStyle     = lipgloss.NewStyle().Foreground(textTertiary)
)
