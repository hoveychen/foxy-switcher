package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// sparkBlocks: 9 vertical-block characters from "no data" through full-cell.
// Index 0 (space) is reserved for genuine zero/missing samples — using a thin
// block there would falsely imply non-zero traffic.
var sparkBlocks = []rune{' ', '▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// sparkline renders `values` as a single horizontal row of unicode blocks.
// Each value is normalized against `max`; pass max=0 to use auto-scaling
// (which uses the highest sample, falling back to 1.0 when the series is
// flat-zero so we don't divide by zero).
//
// Color is chosen per-cell from the value's bucket: ≥90 danger, ≥75 warn,
// otherwise OK. Coloring per-cell instead of per-line lets one hot hour
// stand out red against an otherwise calm 24-hour history without us having
// to render two passes.
//
// width=0 means "use len(values)" — caller passes a fixed width to clamp the
// output so the row aligns under a label.
func sparkline(values []float64, max float64, width int) string {
	if len(values) == 0 {
		return ""
	}
	if width <= 0 {
		width = len(values)
	}
	// Sample-and-hold downsample / nearest-pick upsample so a 24-bucket trend
	// fits a 12-cell terminal column without introducing libraries.
	cells := make([]float64, width)
	for i := 0; i < width; i++ {
		idx := i * len(values) / width
		cells[i] = values[idx]
	}

	if max <= 0 {
		for _, v := range cells {
			if v > max {
				max = v
			}
		}
		if max <= 0 {
			max = 1
		}
	}

	var b strings.Builder
	for _, v := range cells {
		if v < 0 {
			v = 0
		}
		ratio := v / max
		if ratio > 1 {
			ratio = 1
		}
		// Map [0,1] to indices [0, len(sparkBlocks)-1]. Tiny non-zero values
		// snap to the smallest visible block so "1% load" doesn't render as
		// "no data".
		idx := int(ratio*float64(len(sparkBlocks)-1) + 0.5)
		if v > 0 && idx == 0 {
			idx = 1
		}
		ch := sparkBlocks[idx]

		var color lipgloss.TerminalColor = tokenOK
		switch {
		case v >= 90:
			color = tokenDanger
		case v >= 75:
			color = tokenWarn
		}
		b.WriteString(lipgloss.NewStyle().Foreground(color).Render(string(ch)))
	}
	return b.String()
}

// sparklineStacked overlays three sparklines on the same row, drawing each
// in its own color so the user can tell which window is hot. The output is
// a `len(rows)`-line block where rows[i] is the labeled sparkline for
// series[i]. Each row is at least `labelW + width + 2` cells wide; if a row
// supplies a non-empty Suffix, it is appended after a single space — used
// for the "cur 25% · peak 38%" tail on the dashboard so users get an actual
// number alongside the silhouette.
func sparklineStacked(width, labelW int, series ...struct {
	Label  string
	Values []float64
	Color  lipgloss.TerminalColor
	Suffix string
}) string {
	if width <= 0 || len(series) == 0 {
		return ""
	}
	var lines []string
	for _, s := range series {
		// Use auto-scaled max per-row: each window has its own ceiling so a
		// quiet 7d-Sonnet doesn't flatten when shown next to a busy 5h.
		spark := sparklineColored(s.Values, 0, width, s.Color)
		label := lipgloss.NewStyle().Foreground(s.Color).Render(s.Label)
		label = padRight(label, labelW)
		row := label + " " + spark
		if s.Suffix != "" {
			row += "  " + dimStyle.Render(s.Suffix)
		}
		lines = append(lines, row)
	}
	return strings.Join(lines, "\n")
}

// sparklineColored is sparkline with a forced color (skipping the per-bucket
// red/yellow/green logic). Used by the stacked variant where the row is
// already labeled by color and per-cell threshold coloring would just be
// noise.
func sparklineColored(values []float64, max float64, width int, color lipgloss.TerminalColor) string {
	if len(values) == 0 || width <= 0 {
		return ""
	}
	cells := make([]float64, width)
	for i := 0; i < width; i++ {
		cells[i] = values[i*len(values)/width]
	}
	if max <= 0 {
		for _, v := range cells {
			if v > max {
				max = v
			}
		}
		if max <= 0 {
			max = 1
		}
	}
	style := lipgloss.NewStyle().Foreground(color)
	var b strings.Builder
	for _, v := range cells {
		if v < 0 {
			v = 0
		}
		ratio := v / max
		if ratio > 1 {
			ratio = 1
		}
		idx := int(ratio*float64(len(sparkBlocks)-1) + 0.5)
		if v > 0 && idx == 0 {
			idx = 1
		}
		b.WriteString(style.Render(string(sparkBlocks[idx])))
	}
	return b.String()
}
