package tui

import "github.com/charmbracelet/lipgloss"

// TokyoNightTheme — deep navy night surfaces with cyan + magenta highlights.
// Inspired by enkia/tokyo-night-vscode-theme; light variant uses Tokyo Night
// Day for parity.
func TokyoNightTheme() *Theme {
	return &Theme{
		Name:    "tokyo",
		Display: "Tokyo Night",

		TextPrimary:   lipgloss.AdaptiveColor{Light: "#3760bf", Dark: "#c0caf5"},
		TextSecondary: lipgloss.AdaptiveColor{Light: "#6172b0", Dark: "#9aa5ce"},
		TextTertiary:  lipgloss.AdaptiveColor{Light: "#9099b0", Dark: "#565f89"},

		BgSelected: lipgloss.AdaptiveColor{Light: "#dde6f7", Dark: "#283457"},
		BgSubtle:   lipgloss.AdaptiveColor{Light: "#e1e6f0", Dark: "#16161e"},

		BorderSubtle: lipgloss.AdaptiveColor{Light: "#a4adc7", Dark: "#414868"},

		AccentBrand:  lipgloss.AdaptiveColor{Light: "#ff9e64", Dark: "#ff9e64"},
		AccentSoft:   lipgloss.AdaptiveColor{Light: "#fbe6d4", Dark: "#3d2f24"},
		TextOnAccent: lipgloss.AdaptiveColor{Light: "#1a1b26", Dark: "#1a1b26"},

		TokenOK:         lipgloss.AdaptiveColor{Light: "#587539", Dark: "#9ece6a"},
		TokenOKSoft:     lipgloss.AdaptiveColor{Light: "#d7e6c4", Dark: "#243329"},
		TokenWarn:       lipgloss.AdaptiveColor{Light: "#8c6c3e", Dark: "#e0af68"},
		TokenWarnSoft:   lipgloss.AdaptiveColor{Light: "#f3e3c5", Dark: "#3d3326"},
		TokenDanger:     lipgloss.AdaptiveColor{Light: "#c64343", Dark: "#f7768e"},
		TokenDangerSoft: lipgloss.AdaptiveColor{Light: "#f3d2d2", Dark: "#3d2528"},
		TokenInfo:       lipgloss.AdaptiveColor{Light: "#34548a", Dark: "#7aa2f7"},
		TokenInfoSoft:   lipgloss.AdaptiveColor{Light: "#d7dff0", Dark: "#1f2c3d"},

		GradientFrom: lipgloss.Color("#7aa2f7"),
		GradientTo:   lipgloss.Color("#bb9af7"),
	}
}
