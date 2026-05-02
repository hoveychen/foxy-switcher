package tui

import "github.com/charmbracelet/lipgloss"

// HighContrastTheme — accessibility-oriented palette with maximum foreground /
// background separation. Useful on low-quality terminals or for users who
// need extra legibility. Avoids relying on subtle hue differences.
func HighContrastTheme() *Theme {
	return &Theme{
		Name:    "high-contrast",
		Display: "High Contrast",

		TextPrimary:   lipgloss.AdaptiveColor{Light: "#000000", Dark: "#ffffff"},
		TextSecondary: lipgloss.AdaptiveColor{Light: "#1a1a1a", Dark: "#e0e0e0"},
		TextTertiary:  lipgloss.AdaptiveColor{Light: "#444444", Dark: "#a0a0a0"},

		BgSelected: lipgloss.AdaptiveColor{Light: "#ffe680", Dark: "#665c1f"},
		BgSubtle:   lipgloss.AdaptiveColor{Light: "#f0f0f0", Dark: "#1a1a1a"},

		BorderSubtle: lipgloss.AdaptiveColor{Light: "#000000", Dark: "#ffffff"},

		AccentBrand:  lipgloss.AdaptiveColor{Light: "#cc4400", Dark: "#ffae33"},
		AccentSoft:   lipgloss.AdaptiveColor{Light: "#ffe0b3", Dark: "#3d2814"},
		TextOnAccent: lipgloss.AdaptiveColor{Light: "#000000", Dark: "#000000"},

		TokenOK:         lipgloss.AdaptiveColor{Light: "#006400", Dark: "#7cffa6"},
		TokenOKSoft:     lipgloss.AdaptiveColor{Light: "#ccffcc", Dark: "#003d14"},
		TokenWarn:       lipgloss.AdaptiveColor{Light: "#996600", Dark: "#ffd966"},
		TokenWarnSoft:   lipgloss.AdaptiveColor{Light: "#fff2cc", Dark: "#3d2e00"},
		TokenDanger:     lipgloss.AdaptiveColor{Light: "#b30000", Dark: "#ff6666"},
		TokenDangerSoft: lipgloss.AdaptiveColor{Light: "#ffd6d6", Dark: "#3d0a0a"},
		TokenInfo:       lipgloss.AdaptiveColor{Light: "#003366", Dark: "#80c0ff"},
		TokenInfoSoft:   lipgloss.AdaptiveColor{Light: "#cce0ff", Dark: "#001f3d"},

		GradientFrom: lipgloss.Color("#ffae33"),
		GradientTo:   lipgloss.Color("#cc4400"),
	}
}
