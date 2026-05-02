package tui

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
)

// gradient renders text with a per-character RGB interpolation between from
// and to. Both anchors must be 24-bit `lipgloss.Color` (e.g. "#ff7a1a") —
// AdaptiveColor pairs are rejected because the interpolation needs concrete
// channels, not late-bound terminal-aware values. On 8-color terminals the
// output is still rendered: lipgloss falls back to the closest indexed color.
//
// Whitespace runes are preserved verbatim (no escape codes around spaces) so
// the caller can pad the string without disturbing the gradient cadence.
func gradient(text string, from, to lipgloss.Color) string {
	r1, g1, b1, ok1 := hexRGB(string(from))
	r2, g2, b2, ok2 := hexRGB(string(to))
	if !ok1 || !ok2 {
		return text
	}

	n := utf8.RuneCountInString(text)
	if n <= 0 {
		return ""
	}

	var b strings.Builder
	i := 0
	for _, r := range text {
		if r == ' ' || r == '\t' || r == '\n' {
			b.WriteRune(r)
			i++
			continue
		}
		var t float64
		if n == 1 {
			t = 0
		} else {
			t = float64(i) / float64(n-1)
		}
		rr := lerp(r1, r2, t)
		gg := lerp(g1, g2, t)
		bb := lerp(b1, b2, t)
		hex := fmt.Sprintf("#%02x%02x%02x", rr, gg, bb)
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(hex)).Render(string(r)))
		i++
	}
	return b.String()
}

// hexRGB parses `#rrggbb` into channel ints. Anything else returns ok=false so
// the caller falls back to plain text — no panics on themes that supply a
// named color.
func hexRGB(s string) (r, g, b int, ok bool) {
	if len(s) != 7 || s[0] != '#' {
		return 0, 0, 0, false
	}
	rv, err := strconv.ParseUint(s[1:3], 16, 8)
	if err != nil {
		return 0, 0, 0, false
	}
	gv, err := strconv.ParseUint(s[3:5], 16, 8)
	if err != nil {
		return 0, 0, 0, false
	}
	bv, err := strconv.ParseUint(s[5:7], 16, 8)
	if err != nil {
		return 0, 0, 0, false
	}
	return int(rv), int(gv), int(bv), true
}

// lerp interpolates a single 0–255 channel. Rounded to nearest int so two
// adjacent characters never produce the same hex unless they're at the same
// fractional position.
func lerp(a, c int, t float64) int {
	v := float64(a) + (float64(c)-float64(a))*t
	if v < 0 {
		v = 0
	}
	if v > 255 {
		v = 255
	}
	return int(v + 0.5)
}
