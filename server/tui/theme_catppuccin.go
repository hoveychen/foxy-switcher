package tui

import "github.com/charmbracelet/lipgloss"

// CatppuccinTheme uses Catppuccin Mocha (dark) / Latte (light) palettes —
// pastel surfaces, peach accent. https://github.com/catppuccin/catppuccin
func CatppuccinTheme() *Theme {
	return &Theme{
		Name:    "catppuccin",
		Display: "Catppuccin",

		TextPrimary:   lipgloss.AdaptiveColor{Light: "#4c4f69", Dark: "#cdd6f4"},
		TextSecondary: lipgloss.AdaptiveColor{Light: "#6c6f85", Dark: "#a6adc8"},
		TextTertiary:  lipgloss.AdaptiveColor{Light: "#8c8fa1", Dark: "#7f849c"},

		BgSelected: lipgloss.AdaptiveColor{Light: "#fef1c7", Dark: "#313244"},
		BgSubtle:   lipgloss.AdaptiveColor{Light: "#eff1f5", Dark: "#181825"},

		BorderSubtle: lipgloss.AdaptiveColor{Light: "#bcc0cc", Dark: "#585b70"},

		AccentBrand:  lipgloss.AdaptiveColor{Light: "#fe640b", Dark: "#fab387"},
		AccentSoft:   lipgloss.AdaptiveColor{Light: "#fef1c7", Dark: "#45342b"},
		TextOnAccent: lipgloss.AdaptiveColor{Light: "#1e1e2e", Dark: "#1e1e2e"},

		TokenOK:         lipgloss.AdaptiveColor{Light: "#40a02b", Dark: "#a6e3a1"},
		TokenOKSoft:     lipgloss.AdaptiveColor{Light: "#dcefd5", Dark: "#293b2c"},
		TokenWarn:       lipgloss.AdaptiveColor{Light: "#df8e1d", Dark: "#f9e2af"},
		TokenWarnSoft:   lipgloss.AdaptiveColor{Light: "#fef1c7", Dark: "#45402b"},
		TokenDanger:     lipgloss.AdaptiveColor{Light: "#d20f39", Dark: "#f38ba8"},
		TokenDangerSoft: lipgloss.AdaptiveColor{Light: "#fad9e0", Dark: "#452a32"},
		TokenInfo:       lipgloss.AdaptiveColor{Light: "#1e66f5", Dark: "#89b4fa"},
		TokenInfoSoft:   lipgloss.AdaptiveColor{Light: "#dee5fb", Dark: "#283045"},

		GradientFrom: lipgloss.Color("#fab387"),
		GradientTo:   lipgloss.Color("#f38ba8"),
	}
}
