package tui

import "github.com/charmbracelet/lipgloss"

// pillTone selects the foreground/background pair for a pill. Each tone reads
// the current theme's semantic tokens, so pills automatically re-skin on
// theme switch — no need to re-render or re-cache.
type pillTone int

const (
	pillNeutral pillTone = iota
	pillAccent
	pillOK
	pillWarn
	pillDanger
	pillInfo
)

// pillVariant chooses between filled (text on solid color) and soft (text on
// muted background). Soft is the LURA default — closer to web design system
// and reads well on both light and dark terminals. Filled is reserved for the
// "in use" emphasis chip where it needs to win attention.
type pillVariant int

const (
	pillSoft pillVariant = iota
	pillFilled
)

// pill renders ` text ` with single-cell horizontal padding. Width-stable —
// lipgloss padding inserts spaces, no horizontal border, so callers can place
// pills inline without surprise wrapping.
func pill(text string, tone pillTone, variant pillVariant) string {
	fg, bg := pillColors(tone, variant)
	style := lipgloss.NewStyle().Foreground(fg).Background(bg).Padding(0, 1)
	if variant == pillFilled {
		style = style.Bold(true)
	}
	return style.Render(text)
}

// utilTone maps a 0–100 utilization percentage to the standard semantic tone
// used across pills, sparklines, KPI cards, and progress bars. ≥90 danger /
// ≥75 warn / otherwise OK. Centralized so a future threshold tweak is one
// edit instead of grepping the codebase.
func utilTone(percent float64) pillTone {
	switch {
	case percent >= 90:
		return pillDanger
	case percent >= 75:
		return pillWarn
	default:
		return pillOK
	}
}

// pillColors maps (tone, variant) → (fg, bg). Returning AdaptiveColor lets the
// terminal pick light/dark automatically; in filled mode the foreground rides
// on the solid token color, in soft mode it rides on the soft variant.
func pillColors(tone pillTone, variant pillVariant) (lipgloss.TerminalColor, lipgloss.TerminalColor) {
	switch variant {
	case pillFilled:
		switch tone {
		case pillAccent:
			return textOnAccent, accentBrand
		case pillOK:
			return textOnAccent, tokenOK
		case pillWarn:
			return textOnAccent, tokenWarn
		case pillDanger:
			return textOnAccent, tokenDanger
		case pillInfo:
			return textOnAccent, tokenInfo
		default:
			return textPrimary, bgSelected
		}
	default: // pillSoft
		switch tone {
		case pillAccent:
			return accentBrand, accentSoft
		case pillOK:
			return tokenOK, tokenOKSoft
		case pillWarn:
			return tokenWarn, tokenWarnSoft
		case pillDanger:
			return tokenDanger, tokenDangerSoft
		case pillInfo:
			return tokenInfo, tokenInfoSoft
		default:
			return textPrimary, bgSelected
		}
	}
}
