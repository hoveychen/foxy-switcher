package tui

import "github.com/charmbracelet/lipgloss"

// Theme is a complete palette + style derivation. The TUI keeps a single
// active theme in `currentTheme`, and `applyTheme` re-binds the package-level
// color vars + styles below. Components reach for those vars directly so the
// theme swap costs one assignment per token plus a re-derivation of cached
// styles — no plumbing through every callsite.
type Theme struct {
	// Name is a stable id used for persistence.
	Name string
	// Display is the human-readable label rendered in the Settings picker.
	Display string

	// Text
	TextPrimary   lipgloss.AdaptiveColor
	TextSecondary lipgloss.AdaptiveColor
	TextTertiary  lipgloss.AdaptiveColor

	// Surfaces
	BgSelected   lipgloss.AdaptiveColor // selected list rows / soft accent fill
	BgSubtle     lipgloss.AdaptiveColor // statusbar / chrome subdued bg
	BorderSubtle lipgloss.AdaptiveColor

	// Accent / brand
	AccentBrand  lipgloss.AdaptiveColor
	AccentSoft   lipgloss.AdaptiveColor
	TextOnAccent lipgloss.AdaptiveColor

	// Semantic
	TokenOK         lipgloss.AdaptiveColor
	TokenOKSoft     lipgloss.AdaptiveColor
	TokenWarn       lipgloss.AdaptiveColor
	TokenWarnSoft   lipgloss.AdaptiveColor
	TokenDanger     lipgloss.AdaptiveColor
	TokenDangerSoft lipgloss.AdaptiveColor
	TokenInfo       lipgloss.AdaptiveColor
	TokenInfoSoft   lipgloss.AdaptiveColor

	// GradientFrom / GradientTo anchor the LURA mark gradient. Both are 24-bit
	// colors (not adaptive) — terminals that fall back to 8-color will simply
	// see the closest match per cell.
	GradientFrom lipgloss.Color
	GradientTo   lipgloss.Color
}

// Package-level mutable color vars and styles. Components reference these
// directly. `applyTheme` re-binds them when the user switches theme.
var (
	textPrimary   lipgloss.AdaptiveColor
	textSecondary lipgloss.AdaptiveColor
	textTertiary  lipgloss.AdaptiveColor

	bgSelected lipgloss.AdaptiveColor
	bgSubtle   lipgloss.AdaptiveColor

	borderSubtle lipgloss.AdaptiveColor

	accentBrand  lipgloss.AdaptiveColor
	accentSoft   lipgloss.AdaptiveColor
	textOnAccent lipgloss.AdaptiveColor

	tokenOK         lipgloss.AdaptiveColor
	tokenOKSoft     lipgloss.AdaptiveColor
	tokenWarn       lipgloss.AdaptiveColor
	tokenWarnSoft   lipgloss.AdaptiveColor
	tokenDanger     lipgloss.AdaptiveColor
	tokenDangerSoft lipgloss.AdaptiveColor
	tokenInfo       lipgloss.AdaptiveColor
	tokenInfoSoft   lipgloss.AdaptiveColor

	gradientFrom lipgloss.Color
	gradientTo   lipgloss.Color
)

var (
	titleStyle    lipgloss.Style
	headerStyle   lipgloss.Style
	cursorStyle   lipgloss.Style
	selectedStyle lipgloss.Style
	dimStyle      lipgloss.Style
	okStyle       lipgloss.Style
	warnStyle     lipgloss.Style
	errStyle      lipgloss.Style
	helpStyle     lipgloss.Style
)

// currentTheme is the active theme; nil before first applyTheme call. The init
// below ensures it's always non-nil at runtime.
var currentTheme *Theme

func init() {
	applyTheme(LuraTheme())
}

// applyTheme rebinds all package-level color vars and styles to t. Call this
// once on startup and again on every theme switch. Components that captured
// styles at package init are refreshed here.
func applyTheme(t *Theme) {
	currentTheme = t

	textPrimary = t.TextPrimary
	textSecondary = t.TextSecondary
	textTertiary = t.TextTertiary

	bgSelected = t.BgSelected
	bgSubtle = t.BgSubtle

	borderSubtle = t.BorderSubtle

	accentBrand = t.AccentBrand
	accentSoft = t.AccentSoft
	textOnAccent = t.TextOnAccent

	tokenOK = t.TokenOK
	tokenOKSoft = t.TokenOKSoft
	tokenWarn = t.TokenWarn
	tokenWarnSoft = t.TokenWarnSoft
	tokenDanger = t.TokenDanger
	tokenDangerSoft = t.TokenDangerSoft
	tokenInfo = t.TokenInfo
	tokenInfoSoft = t.TokenInfoSoft

	gradientFrom = t.GradientFrom
	gradientTo = t.GradientTo

	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(accentBrand)
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(textSecondary)
	cursorStyle = lipgloss.NewStyle().Foreground(accentBrand)
	selectedStyle = lipgloss.NewStyle().Foreground(textPrimary).Background(bgSelected)
	dimStyle = lipgloss.NewStyle().Foreground(textSecondary)
	okStyle = lipgloss.NewStyle().Foreground(tokenOK)
	warnStyle = lipgloss.NewStyle().Foreground(tokenWarn)
	errStyle = lipgloss.NewStyle().Foreground(tokenDanger)
	helpStyle = lipgloss.NewStyle().Foreground(textTertiary)
}

// allThemes returns the registered themes in cycle order. The order is also
// the order shown in the Settings picker.
func allThemes() []*Theme {
	return []*Theme{
		LuraTheme(),
		CatppuccinTheme(),
		TokyoNightTheme(),
		HighContrastTheme(),
	}
}

// themeByName looks up a theme by its persistence id. Returns LURA on miss so
// a corrupt config can never leave the user without colors.
func themeByName(name string) *Theme {
	for _, t := range allThemes() {
		if t.Name == name {
			return t
		}
	}
	return LuraTheme()
}

// nextTheme returns the theme after the current one in the cycle, wrapping at
// the end. Used by the global `T` keybinding.
func nextTheme(current *Theme) *Theme {
	themes := allThemes()
	for i, t := range themes {
		if t.Name == current.Name {
			return themes[(i+1)%len(themes)]
		}
	}
	return themes[0]
}
