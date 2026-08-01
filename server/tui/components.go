package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// statusDot returns the colored Unicode glyph indicating account state.
// Pair with statusLabel so the status is conveyed by character + text, not
// color alone (matters on NO_COLOR / 8-color terminals).
//
// Priority: org_disabled > needs_reauth > paused > expired > cooling > active.
// org_disabled and needs_reauth win because they can't be recovered by waiting
// or resuming — the org block / dead refresh_token need out-of-band action.
// Paused beats expired because it's an explicit user action; expired beats
// cooling because a dead token blocks injection regardless of how soon the
// limit resets.
func statusDot(a Account, nowMs int64) string {
	switch {
	case a.Status == "org_disabled":
		return lipgloss.NewStyle().Foreground(tokenDanger).Render("⊗")
	case a.Status == "needs_reauth":
		return lipgloss.NewStyle().Foreground(tokenDanger).Render("⚠")
	case a.Status == "paused":
		return lipgloss.NewStyle().Foreground(textTertiary).Render("○")
	case a.TokenExpired:
		return lipgloss.NewStyle().Foreground(tokenDanger).Render("⊘")
	case accountIsCooling(a):
		return lipgloss.NewStyle().Foreground(tokenWarn).Render("◐")
	default:
		return lipgloss.NewStyle().Foreground(tokenOK).Render("●")
	}
}

// statusLabel renders the textual status with the same tone as statusDot.
func statusLabel(a Account, nowMs int64) string {
	if a.Status == "org_disabled" {
		return errStyle.Render("org OAuth disabled")
	}
	if a.Status == "needs_reauth" {
		return errStyle.Render("reauth required")
	}
	if a.Status == "paused" {
		return dimStyle.Render("paused")
	}
	if a.TokenExpired {
		return errStyle.Render("token expired")
	}
	if reset, ok := accountResetAt(a, time.UnixMilli(nowMs)); ok {
		left := time.Until(reset).Round(time.Second)
		return warnStyle.Render("cooling " + left.String())
	}
	if accountIsCooling(a) {
		return warnStyle.Render("cooling")
	}
	return okStyle.Render("active")
}

// accountIsCooling reports whether a HARD window (5h / 7d) has reached its
// threshold. Mirrors selector.hardThreshold so the TUI shows the same
// eligibility state the daemon's selector enforces. The per-model
// weekly-scoped window (SevenDaySonnet slot — Fable/…) is excluded on purpose:
// hitting it only caps that one model, the account stays selectable as a
// degraded fallback, so it must not read as "cooling".
func accountIsCooling(a Account) bool {
	if a.FiveHour != nil && a.FiveHour.Utilization >= a.FiveHourThreshold {
		return true
	}
	if a.SevenDay != nil && a.SevenDay.Utilization >= a.SevenDayThreshold {
		return true
	}
	return false
}

// accountResetAt returns the soonest future reset across the HARD windows
// (5h / 7d) that are currently above their threshold. The scoped window is
// excluded for the same reason as accountIsCooling. Returns ok=false when no
// hard window is throttled, when none of the throttled windows have a
// parseable resets_at, or when every reset has already passed (the next usage
// poll will clear utilization shortly).
func accountResetAt(a Account, now time.Time) (time.Time, bool) {
	candidates := []*UsageWindow{}
	if a.FiveHour != nil && a.FiveHour.Utilization >= a.FiveHourThreshold {
		candidates = append(candidates, a.FiveHour)
	}
	if a.SevenDay != nil && a.SevenDay.Utilization >= a.SevenDayThreshold {
		candidates = append(candidates, a.SevenDay)
	}
	var best time.Time
	found := false
	for _, c := range candidates {
		if c.ResetsAt == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, c.ResetsAt)
		if err != nil || !t.After(now) {
			continue
		}
		if !found || t.Before(best) {
			best = t
			found = true
		}
	}
	return best, found
}

// planWeight returns this account's Pro-equivalent (5h, 7d) weights. Mirrors
// src/api.ts:planWeight and server/httpapi:planWeight — keep in sync. The
// primary key is rate_limit_tier (the only field that distinguishes personal
// Max 5x from Max 20x); subscription_type is a fallback for legacy rows that
// haven't been backfilled by the next UsagePoller tick.
func planWeight(provider, rateTier, sub string) (w5h, w7d float64) {
	// The weights are Claude Pro-equivalents, so only a Claude account has one.
	// This guard is load-bearing, not defensive: a Codex account's
	// subscription_type comes from the ChatGPT plan type, and "team" collides
	// with Claude's legacy fallback below. Codex never sets rate_limit_tier, so
	// without this it fell straight through to 5x. Empty provider = legacy row
	// predating the column = Claude.
	if provider != "" && provider != "claude" {
		return 0, 0
	}
	switch rateTier {
	case "default_claude_pro":
		return 1, 1
	case "default_claude_max_5x":
		return 5, 5
	case "default_claude_max_20x":
		return 20, 10
	}
	switch sub {
	case "pro":
		return 1, 1
	case "max":
		return 5, 5
	case "team":
		return 5, 5
	case "team_premium":
		return 5, 5
	default:
		return 0, 0
	}
}

// poolWindowTotals computes the weighted-pool used / capacity / percent for
// one window across all *active* accounts. Used and capacity are in
// Pro-equivalents (Pro=1x, Max5x=5x, Max20x=20x for 5h / 10x for 7d).
// Percent is used/capacity*100, 0 when capacity is 0. Window is "five_hour"
// or "seven_day" — matches the frontend signature so the TUI can show the
// same numbers without round-tripping through /api/dashboard.
func poolWindowTotals(accounts []Account, window string) (used, capacity, percent float64) {
	for _, a := range accounts {
		if a.Status != "active" {
			continue
		}
		w5h, w7d := planWeight(a.Provider, a.RateLimitTier, a.SubscriptionType)
		var w float64
		var u *UsageWindow
		switch window {
		case "five_hour":
			w = w5h
			u = a.FiveHour
		case "seven_day":
			w = w7d
			u = a.SevenDay
		default:
			continue
		}
		if w <= 0 {
			continue
		}
		capacity += w
		if u != nil {
			util := u.Utilization
			if util < 0 {
				util = 0
			}
			if util > 100 {
				util = 100
			}
			used += (util / 100) * w
		}
	}
	if capacity > 0 {
		percent = used / capacity * 100
	}
	return
}

// planBadge renders the plan as a soft accent pill. Empty plan → "".
func planBadge(plan string) string {
	if plan == "" {
		return ""
	}
	return pill(plan, pillAccent, pillSoft)
}

// inUseChip renders `in use` as a filled accent pill — used in list rows next
// to the plan badge to flag the currently-injected account.
func inUseChip() string {
	return pill("in use", pillAccent, pillFilled)
}

// switchingChip renders a row-level busy chip with the spinner glyph followed
// by "switching…". Used by renderAccountRow when pendingAccountID matches the
// row — the bottom statusline's spinner is too peripheral to notice for fast
// local-daemon round-trips.
func (m *model) switchingChip() string {
	return pill(m.spinner.View()+" switching…", pillAccent, pillFilled)
}

// foreignLeaseChip renders the "held by another device" badge: device name
// plus the lease's remaining TTL (e.g. `held by laptop-2 12m`). Returned as a
// soft info pill so it reads as informational, not as an action affordance.
// Empty string when the lease is nil or owned by this device.
func foreignLeaseChip(a Account, nowMs int64) string {
	if a.Lease == nil || a.Lease.Mine {
		return ""
	}
	dev := a.Lease.DeviceName
	if dev == "" {
		dev = a.Lease.DeviceID
	}
	if dev == "" {
		dev = "another device"
	}
	ttl := leaseTTLLabel(a.Lease.ExpiresAt, nowMs)
	label := "held by " + dev
	if ttl != "" {
		label += " " + ttl
	}
	return pill(label, pillInfo, pillSoft)
}

// leaseTTLLabel formats the lease's remaining lifetime as the short suffix
// rendered after the device name. Empty when the lease has already expired
// (the lease should have been GC'd; rendering "expired" would be noise).
func leaseTTLLabel(expiresAtMs, nowMs int64) string {
	d := time.Duration(expiresAtMs-nowMs) * time.Millisecond
	if d <= 0 {
		return ""
	}
	d = d.Round(time.Minute)
	if d < time.Minute {
		return "<1m"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d.Hours())
	mins := int(d.Minutes()) - h*60
	if mins == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, mins)
}

const progressBarDefaultWidth = 10

// progressBar renders `████░░░░░░  42%`. width is the bar cell count; pass 0
// for the default. Tone is auto-selected from utilization (0–100).
func progressBar(util float64, width int) string {
	if width <= 0 {
		width = progressBarDefaultWidth
	}
	if util < 0 {
		util = 0
	}
	if util > 100 {
		util = 100
	}
	filled := int(util*float64(width)/100.0 + 0.5)
	if filled > width {
		filled = width
	}

	tone := tokenOK
	switch {
	case util >= 90:
		tone = tokenDanger
	case util >= 75:
		tone = tokenWarn
	}

	bar := lipgloss.NewStyle().Foreground(tone).Render(strings.Repeat("█", filled)) +
		lipgloss.NewStyle().Foreground(textTertiary).Render(strings.Repeat("░", width-filled))
	return bar + fmt.Sprintf("  %3.0f%%", util)
}

// progressBarEmpty renders the no-data variant: dim track + em-dash.
func progressBarEmpty(width int) string {
	if width <= 0 {
		width = progressBarDefaultWidth
	}
	bar := lipgloss.NewStyle().Foreground(textTertiary).Render(strings.Repeat("░", width))
	return bar + "    —"
}

// keyChip renders `k label`: key in a soft accent pill, label dim.
func keyChip(key, label string) string {
	return pill(key, pillAccent, pillSoft) + " " + dimStyle.Render(label)
}

// keyChipRow joins chips with three spaces.
func keyChipRow(chips ...string) string {
	return strings.Join(chips, "   ")
}

// rowRail renders the leading column of a list row. The in-use account always
// wins so the marker stays visible when the cursor moves away.
//
//	in-use  → ▶ (orange bold)
//	cursor  → ▍ (orange)
//	neither → space
func rowRail(selected, inUse bool) string {
	style := lipgloss.NewStyle().Foreground(accentBrand)
	switch {
	case inUse:
		return style.Bold(true).Render("▶")
	case selected:
		return style.Render("▍")
	default:
		return " "
	}
}

// panel wraps body in a rounded border with title embedded in the top edge:
//
//	╭─ TITLE ─────────╮
//	│ body            │
//	╰─────────────────╯
//
// totalWidth includes both border columns; minimum 6.
func panel(title, body string, totalWidth int) string {
	if totalWidth < 6 {
		totalWidth = 6
	}
	border := lipgloss.NewStyle().Foreground(borderSubtle)

	var top string
	if title == "" {
		// Borderless title: full-width top edge, no caption gap.
		top = border.Render("╭" + strings.Repeat("─", totalWidth-2) + "╮")
	} else {
		titleRendered := titleStyle.Render(title)
		titleW := lipgloss.Width(titleRendered)
		// Top edge composition: ╭ + "─ " + title + " " + N×─ + ╮  (totalWidth chars)
		fillN := totalWidth - 5 - titleW
		if fillN < 1 {
			fillN = 1
		}
		top = border.Render("╭─ ") + titleRendered + border.Render(" "+strings.Repeat("─", fillN)+"╮")
	}

	innerW := totalWidth - 4 // -2 borders -2 padding
	if innerW < 1 {
		innerW = 1
	}
	var bodyLines []string
	for _, ln := range strings.Split(body, "\n") {
		lnW := lipgloss.Width(ln)
		pad := innerW - lnW
		if pad < 0 {
			ln = truncateANSI(ln, innerW)
			pad = 0
		}
		bodyLines = append(bodyLines, border.Render("│ ")+ln+strings.Repeat(" ", pad)+border.Render(" │"))
	}

	bottom := border.Render("╰" + strings.Repeat("─", totalWidth-2) + "╯")

	parts := append([]string{top}, bodyLines...)
	parts = append(parts, bottom)
	return strings.Join(parts, "\n")
}

// truncateANSI naively truncates to width cells. Falls back to lipgloss-aware
// width comparison; if cutting mid-ANSI the residual codes leak, but content
// here is short controlled strings so we accept the risk.
func truncateANSI(s string, width int) string {
	if lipgloss.Width(s) <= width {
		return s
	}
	// Walk runes, count visible cells, stop when reaching width-1 (room for …).
	out := make([]rune, 0, len(s))
	cells := 0
	inEsc := false
	for _, r := range s {
		if r == 0x1b {
			inEsc = true
			out = append(out, r)
			continue
		}
		if inEsc {
			out = append(out, r)
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		if cells >= width-1 {
			break
		}
		out = append(out, r)
		cells++
	}
	return string(out) + "…"
}
