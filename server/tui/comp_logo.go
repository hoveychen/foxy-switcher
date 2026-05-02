package tui

import (
	"bytes"
	_ "embed"
	"fmt"
	"image"
	_ "image/png"
	"strings"
	"sync"
)

//go:embed assets/foxy-icon.png
var foxyIconPNG []byte

var (
	foxLogoOnce sync.Once
	foxLogoSrc  image.Image
	foxLogoErr  error

	foxLogoCacheMu sync.Mutex
	foxLogoCache   = map[[2]int]string{}
)

// renderFoxLogo renders the embedded foxy icon as truecolor ANSI half-blocks
// at cellsW × cellsH terminal cells. Each cell holds two stacked source
// pixels via the upper-half-block glyph "▀": foreground = top pixel,
// background = bottom pixel. The result is cached per (W,H) — the PNG decode
// and downsample run once on first use.
//
// Output width measured by lipgloss.Width is exactly cellsW; height is cellsH
// rows separated by '\n' (no trailing newline).
//
// Truecolor is required for the brand colors to read correctly. On 16/256
// color terminals the raw 24-bit escapes are typically dropped or downgraded
// per terminal; the glyphs still render but the fox loses its color identity.
func renderFoxLogo(cellsW, cellsH int) string {
	if cellsW <= 0 || cellsH <= 0 {
		return ""
	}
	foxLogoOnce.Do(func() {
		foxLogoSrc, _, foxLogoErr = image.Decode(bytes.NewReader(foxyIconPNG))
	})
	if foxLogoErr != nil {
		return ""
	}

	key := [2]int{cellsW, cellsH}
	foxLogoCacheMu.Lock()
	if s, ok := foxLogoCache[key]; ok {
		foxLogoCacheMu.Unlock()
		return s
	}
	foxLogoCacheMu.Unlock()

	pxW := cellsW
	pxH := cellsH * 2
	b := foxLogoSrc.Bounds()
	dx, dy := b.Dx(), b.Dy()

	get := func(x, y int) (int, int, int) {
		sx := b.Min.X + x*dx/pxW
		sy := b.Min.Y + y*dy/pxH
		r, g, bl, _ := foxLogoSrc.At(sx, sy).RGBA()
		return int(r >> 8), int(g >> 8), int(bl >> 8)
	}

	var sb strings.Builder
	for y := 0; y < pxH; y += 2 {
		for x := 0; x < pxW; x++ {
			tr, tg, tb := get(x, y)
			br, bg, bb := get(x, y+1)
			fmt.Fprintf(&sb, "\x1b[38;2;%d;%d;%dm\x1b[48;2;%d;%d;%dm▀", tr, tg, tb, br, bg, bb)
		}
		sb.WriteString("\x1b[0m")
		if y+2 < pxH {
			sb.WriteByte('\n')
		}
	}

	out := sb.String()
	foxLogoCacheMu.Lock()
	foxLogoCache[key] = out
	foxLogoCacheMu.Unlock()
	return out
}
