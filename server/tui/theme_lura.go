package tui

import "github.com/charmbracelet/lipgloss"

// LuraTheme is the brand theme — warm orange accent on neutral grays. Matches
// the desktop LURA v1 design system colors as closely as terminal palettes
// allow.
func LuraTheme() *Theme {
	return &Theme{
		Name:    "lura",
		Display: "LURA (default)",

		TextPrimary:   lipgloss.AdaptiveColor{Light: "#0d0d0f", Dark: "#f5f5f7"},
		TextSecondary: lipgloss.AdaptiveColor{Light: "#6e6e76", Dark: "#b0b0b6"},
		TextTertiary:  lipgloss.AdaptiveColor{Light: "#8e8e96", Dark: "#8a8a92"},

		BgSelected: lipgloss.AdaptiveColor{Light: "#fff7ed", Dark: "#3f291c"},
		BgSubtle:   lipgloss.AdaptiveColor{Light: "#f3f3f5", Dark: "#1a1a1c"},

		BorderSubtle: lipgloss.AdaptiveColor{Light: "#b8b8c0", Dark: "#6e6e76"},

		AccentBrand:  lipgloss.AdaptiveColor{Light: "#ff7a1a", Dark: "#ff9233"},
		AccentSoft:   lipgloss.AdaptiveColor{Light: "#ffe8d1", Dark: "#3f291c"},
		TextOnAccent: lipgloss.AdaptiveColor{Light: "#ffffff", Dark: "#ffffff"},

		TokenOK:         lipgloss.AdaptiveColor{Light: "#22c55e", Dark: "#4ade80"},
		TokenOKSoft:     lipgloss.AdaptiveColor{Light: "#dcfce7", Dark: "#14532d"},
		TokenWarn:       lipgloss.AdaptiveColor{Light: "#f59e0b", Dark: "#fbbf24"},
		TokenWarnSoft:   lipgloss.AdaptiveColor{Light: "#fef3c7", Dark: "#451a03"},
		TokenDanger:     lipgloss.AdaptiveColor{Light: "#ef4444", Dark: "#f87171"},
		TokenDangerSoft: lipgloss.AdaptiveColor{Light: "#fee2e2", Dark: "#450a0a"},
		TokenInfo:       lipgloss.AdaptiveColor{Light: "#3b82f6", Dark: "#60a5fa"},
		TokenInfoSoft:   lipgloss.AdaptiveColor{Light: "#dbeafe", Dark: "#172554"},

		GradientFrom: lipgloss.Color("#ffb066"),
		GradientTo:   lipgloss.Color("#e55f00"),
	}
}
