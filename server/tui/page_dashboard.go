package tui

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// dashboardPage owns the Dashboard view's data and polling. The rendering
// pulls live account data from the App-shared accounts model so we don't
// duplicate that fetch — only KPIs / trend / recent activity are dashboard-
// specific.
type dashboardPage struct {
	width, height int

	data       Dashboard
	dataReady  bool
	recent     []ActivityEvent
	loadedAt   time.Time
	loadErr    string

	// External slices the App pushes in via setAccounts before each render so
	// the page doesn't have to round-trip through the daemon for data the
	// Accounts page already polled.
	accounts []Account
	cred     CredStatus
}

func newDashboardPage() *dashboardPage { return &dashboardPage{} }

func (p *dashboardPage) setSize(w, h int) {
	p.width = w
	p.height = h
}

// setAccounts is called by the App after every successful accountsMsg so the
// dashboard reuses the freshest list without a second round-trip.
func (p *dashboardPage) setAccounts(accs []Account, cred CredStatus) {
	p.accounts = accs
	p.cred = cred
}

// dashboardLoadedMsg is the result of the dashboard's own poll cycle —
// /api/dashboard + /api/activity?limit=5.
type dashboardLoadedMsg struct {
	data   Dashboard
	recent []ActivityEvent
	err    error
}

type dashboardTickMsg struct{}

// dashboardPollInterval — pick the same cadence as the accounts poll so the
// two views feel synchronized when the user flips between them. Cheaper than
// a shared single tick, and the per-tick body is two HTTP GETs.
const dashboardPollInterval = 5 * time.Second

func dashboardTickCmd() tea.Cmd {
	return tea.Tick(dashboardPollInterval, func(time.Time) tea.Msg { return dashboardTickMsg{} })
}

// dashboardLoadCmd issues both fetches in parallel inside one command. The
// Dashboard is useless without KPIs, so a failure on either side surfaces a
// single error string rather than a half-empty card grid.
func dashboardLoadCmd(c *Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		d, err := c.GetDashboard(ctx)
		if err != nil {
			return dashboardLoadedMsg{err: err}
		}
		ev, _ := c.ListActivity(ctx, ActivityFilter{Limit: 5})
		return dashboardLoadedMsg{data: d, recent: ev}
	}
}

func (p *dashboardPage) onLoaded(msg dashboardLoadedMsg) {
	if msg.err != nil {
		p.loadErr = msg.err.Error()
		return
	}
	p.data = msg.data
	p.recent = msg.recent
	p.dataReady = true
	p.loadedAt = time.Now()
	p.loadErr = ""
}

// view paints the dashboard. Three-row vertical layout:
//   1. Hero strip — currently-in-use account with its three usage bars.
//   2. KPI row    — three cards side-by-side (or stacked when narrow).
//   3. 24h trend  — three colored sparklines.
//   4. Top-5 + recent activity — two columns when wide, stacked when narrow.
//
// All sections gracefully degrade to a single-line placeholder when their
// data isn't ready yet, so the page is never blank during the first tick.
func (p *dashboardPage) view() string {
	if p.width <= 0 || p.height <= 0 {
		return ""
	}

	hero := p.renderHero()
	kpis := p.renderKPIRow()
	trend := p.renderTrend()
	bottom := p.renderBottomRow()
	footer := p.renderFooter()

	body := strings.Join(
		filterEmpty(hero, "", kpis, "", trend, "", bottom, "", footer),
		"\n",
	)
	// Pad-or-truncate to height; the App already laid out chrome around it.
	bodyH := strings.Count(body, "\n") + 1
	if bodyH < p.height {
		body = body + strings.Repeat("\n", p.height-bodyH)
	}
	return body
}

func (p *dashboardPage) renderHero() string {
	heading := titleStyle.Render("Currently in use")
	if !p.dataReady {
		if p.loadErr != "" {
			return heading + "\n" + errStyle.Render("✗ "+p.loadErr)
		}
		return heading + "\n" + dimStyle.Render("loading…")
	}

	a, ok := p.inUseAccount()
	if !ok {
		return heading + "\n" + dimStyle.Render("(no account injected)")
	}

	meta := []string{a.Name}
	if a.Plan != "" {
		meta = append(meta, pill(a.Plan, pillAccent, pillSoft))
	}
	if exp := humanRemaining(a.ExpiresAt, time.Now().UnixMilli()); exp != "" {
		meta = append(meta, dimStyle.Render(exp))
	}

	bar5h := p.renderUsageRow("5h    ", a.FiveHour)
	bar7d := p.renderUsageRow("7d    ", a.SevenDay)
	bar7s := p.renderUsageRow("7d-S  ", a.SevenDaySonnet)

	body := strings.Join([]string{
		strings.Join(meta, "  "),
		bar5h,
		bar7d,
		bar7s,
	}, "\n")
	cardW := p.width - 4
	if cardW > 76 {
		cardW = 76
	}
	if cardW < 24 {
		cardW = 24
	}
	return heading + "\n" + panel("", body, cardW)
}

func (p *dashboardPage) renderUsageRow(label string, w *UsageWindow) string {
	if w == nil {
		return label + progressBarEmpty(progressBarDefaultWidth)
	}
	return label + progressBar(w.Utilization, progressBarDefaultWidth)
}

func (p *dashboardPage) renderKPIRow() string {
	if !p.dataReady {
		return dimStyle.Render("KPIs loading…")
	}

	k := p.data.KPIs
	cardW := (p.width - 6) / 4
	if cardW < 16 {
		cardW = 16
	}

	// Available: same buckets as the desktop dashboard — count
	// active+!expired+!cooling as "available", with cooling/paused split out
	// in the sub-line.
	var healthy, cooling, paused, expired int
	for _, a := range p.accounts {
		switch {
		case a.Status != "active":
			paused++
		case a.TokenExpired:
			expired++
		case accountIsCooling(a):
			cooling++
		default:
			healthy++
		}
	}
	availTone := pillOK
	if len(p.accounts) == 0 {
		availTone = pillNeutral
	} else if healthy == 0 {
		availTone = pillWarn
	}
	availValue := fmt.Sprintf("%d", healthy)
	if len(p.accounts) == 0 {
		availValue = "—"
	}
	availSub := fmt.Sprintf("%d total · %d cool · %d paused", len(p.accounts), cooling, paused)
	if len(p.accounts) == 0 {
		availSub = "no accounts yet"
	}
	avail := kpiCard("Available", availValue, availSub, availTone, cardW)

	// 5h / 7d weighted pool — live snapshot from the local accounts slice
	// (matches the frontend's poolWindowTotals call). We don't read the
	// trend bucket's *_pct here because that's hour-aligned historical
	// data; the KPI should reflect the latest poll.
	used5h, cap5h, pct5h := poolWindowTotals(p.accounts, "five_hour")
	used7d, cap7d, pct7d := poolWindowTotals(p.accounts, "seven_day")

	five := poolKPI("5h pool", used5h, cap5h, pct5h, cardW)
	seven := poolKPI("7d pool", used7d, cap7d, pct7d, cardW)

	// Next reset: unchanged semantics — soonest reset across throttled windows.
	rstValue := "—"
	rstSub := "none cooling"
	rstTone := pillNeutral
	if k.NextResetAt > 0 {
		left := time.Until(time.UnixMilli(k.NextResetAt)).Round(time.Second)
		if left < 0 {
			left = 0
		}
		rstValue = humanCountdown(left)
		rstSub = p.resetAccountName(k.NextResetAt)
		rstTone = pillWarn
	} else if k.CoolingCount > 0 {
		rstSub = fmt.Sprintf("%d cooling", k.CoolingCount)
	}
	rst := kpiCard("Next reset", rstValue, rstSub, rstTone, cardW)

	return lipgloss.JoinHorizontal(lipgloss.Top, avail, " ", five, " ", seven, " ", rst)
}

// poolKPI renders one weighted-pool card (5h or 7d). Value is "used / cap x"
// (e.g. "3.2 / 11x"); sub is the percent. Capacity 0 (empty pool) falls
// back to a muted "—".
func poolKPI(label string, used, capacity, percent float64, w int) string {
	if capacity <= 0 {
		return kpiCard(label, "—", "no quota", pillNeutral, w)
	}
	value := fmt.Sprintf("%s / %sx", fmtX(used), fmtX(capacity))
	sub := fmt.Sprintf("%d%% used", int(percent+0.5))
	return kpiCard(label, value, sub, utilTone(percent), w)
}

// fmtX trims trailing zeros from Pro-equivalent counts so "5.0" renders as
// "5" but "3.5" stays "3.5". Mirrors src/pages/DashboardPage.tsx:fmtX so the
// two surfaces format identically.
func fmtX(v float64) string {
	if v == float64(int(v)) {
		return strconv.Itoa(int(v))
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

func (p *dashboardPage) renderTrend() string {
	if !p.dataReady {
		return ""
	}

	heading := headerStyle.Render("24h trend — weighted pool")
	if len(p.data.Trend) == 0 {
		return heading + "\n" + dimStyle.Render("(no samples yet)")
	}

	// Weighted-pool percent: the same lines the desktop dashboard renders.
	// Old daemons (pre rate_limit_tier rollout) leave *_pct as 0 across all
	// buckets — fall back to the legacy max-utilization lines so users on
	// older builds still see a meaningful chart.
	five := make([]float64, len(p.data.Trend))
	seven := make([]float64, len(p.data.Trend))
	hasPct := false
	for i, b := range p.data.Trend {
		five[i] = b.FiveHourPct
		seven[i] = b.SevenDayPct
		if b.FiveHourPct > 0 || b.SevenDayPct > 0 {
			hasPct = true
		}
	}
	if !hasPct {
		for i, b := range p.data.Trend {
			five[i] = b.FiveHour
			seven[i] = b.SevenDay
		}
	}

	chartW := p.width - 12
	if chartW > 48 {
		chartW = 48
	}
	if chartW < 12 {
		chartW = 12
	}

	// fmtSuffix produces the trailing "cur 25% · peak 38%" chip. When we're on
	// the legacy raw-Pro-equivalent fallback path (hasPct=false), the values
	// aren't percentages, so we drop the % and just show the magnitudes —
	// otherwise old daemons would render misleading "cur 9.8%".
	pctTail := func(values []float64) string {
		if len(values) == 0 {
			return ""
		}
		cur := values[len(values)-1]
		peak := 0.0
		for _, v := range values {
			if v > peak {
				peak = v
			}
		}
		fmtV := func(v float64) string {
			if !hasPct {
				return fmt.Sprintf("%.1f", v)
			}
			if v >= 10 || v == 0 {
				return fmt.Sprintf("%.0f%%", v)
			}
			return fmt.Sprintf("%.1f%%", v)
		}
		return fmt.Sprintf("cur %s · peak %s", fmtV(cur), fmtV(peak))
	}

	rows := sparklineStacked(chartW, 8,
		struct {
			Label  string
			Values []float64
			Color  lipgloss.TerminalColor
			Suffix string
		}{"5h", five, accentBrand, pctTail(five)},
		struct {
			Label  string
			Values []float64
			Color  lipgloss.TerminalColor
			Suffix string
		}{"7d", seven, tokenInfo, pctTail(seven)},
	)
	return heading + "\n" + rows
}

func (p *dashboardPage) renderBottomRow() string {
	top := p.renderTopAccounts()
	rec := p.renderRecentActivity()

	if p.width >= 80 {
		colW := (p.width - 4) / 2
		left := padBlock(top, colW)
		right := padBlock(rec, colW)
		return lipgloss.JoinHorizontal(lipgloss.Top, left, "  ", right)
	}
	return strings.Join(filterEmpty(top, "", rec), "\n")
}

func (p *dashboardPage) renderTopAccounts() string {
	heading := headerStyle.Render("Top 5 accounts")
	if len(p.accounts) == 0 {
		return heading + "\n" + dimStyle.Render("(no accounts)")
	}
	type row struct {
		a    Account
		util float64
	}
	rows := make([]row, 0, len(p.accounts))
	now := time.Now().UnixMilli()
	for _, a := range p.accounts {
		rows = append(rows, row{a: a, util: maxUtilization(a)})
		_ = now
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].util > rows[j].util })
	if len(rows) > 5 {
		rows = rows[:5]
	}

	var out []string
	for _, r := range rows {
		dot := statusDot(r.a, time.Now().UnixMilli())
		name := truncate(r.a.Name, 16)
		util := dimStyle.Render("paused")
		if r.a.Status != "paused" {
			text := fmt.Sprintf("%3.0f%%", r.util)
			switch utilTone(r.util) {
			case pillDanger:
				util = errStyle.Render(text + " ⚠")
			case pillWarn:
				util = warnStyle.Render(text)
			default:
				util = okStyle.Render(text)
			}
		}
		out = append(out, fmt.Sprintf("%s %-16s  %s", dot, name, util))
	}
	return heading + "\n" + strings.Join(out, "\n")
}

func (p *dashboardPage) renderRecentActivity() string {
	heading := headerStyle.Render("Recent activity")
	if len(p.recent) == 0 {
		return heading + "\n" + dimStyle.Render("(no events yet)")
	}
	var rows []string
	for _, e := range p.recent {
		ts := time.UnixMilli(e.Timestamp).Format("15:04")
		mark := severityGlyph(e.Severity)
		msg := truncate(flattenWhitespace(e.Message), 40)
		rows = append(rows, fmt.Sprintf("%s %s %s", dimStyle.Render(ts), mark, msg))
	}
	return heading + "\n" + strings.Join(rows, "\n")
}

func severityGlyph(sev string) string {
	switch sev {
	case "warn":
		return warnStyle.Render("◐")
	case "error":
		return errStyle.Render("⊘")
	default:
		return okStyle.Render("●")
	}
}

func (p *dashboardPage) renderFooter() string {
	if !p.loadedAt.IsZero() {
		return helpStyle.Render("refreshed " + humanAge(p.loadedAt))
	}
	return ""
}

func (p *dashboardPage) inUseAccount() (Account, bool) {
	if p.cred.ManagedAccountID == 0 {
		return Account{}, false
	}
	for _, a := range p.accounts {
		if a.ID == p.cred.ManagedAccountID {
			return a, true
		}
	}
	return Account{}, false
}

// resetAccountName finds the account whose soonest throttled-window reset
// matches the given timestamp (within one second, since the daemon and the
// TUI both round to seconds). Empty when no match — the KPI then just shows
// the countdown without a name.
func (p *dashboardPage) resetAccountName(at int64) string {
	target := time.UnixMilli(at)
	now := time.Now()
	for _, a := range p.accounts {
		t, ok := accountResetAt(a, now)
		if !ok {
			continue
		}
		if delta := t.Sub(target); delta > -time.Second && delta < time.Second {
			return a.Name
		}
	}
	return ""
}

func maxUtilization(a Account) float64 {
	var max float64
	for _, w := range []*UsageWindow{a.FiveHour, a.SevenDay, a.SevenDaySonnet} {
		if w == nil {
			continue
		}
		if w.Utilization > max {
			max = w.Utilization
		}
	}
	return max
}

// humanCountdown formats a forward-looking duration with two-component
// precision ("14m 22s", "1h 03m"). Distinct from humanDuration in
// view_list.go, which returns single-unit "ago" labels.
func humanCountdown(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		s := int(d.Seconds()) % 60
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), s)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) - h*60
	return fmt.Sprintf("%dh %02dm", h, m)
}

// filterEmpty drops empty strings — used to avoid double-blanks when an
// optional section returns "".
func filterEmpty(parts ...string) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" && len(out) > 0 && out[len(out)-1] == "" {
			continue
		}
		out = append(out, p)
	}
	// Trim trailing blank.
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// padBlock pads each line of a multi-line block to width and stacks them so
// JoinHorizontal lines them up cleanly. The block's line count is preserved.
func padBlock(s string, width int) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		w := lipgloss.Width(ln)
		if w < width {
			lines[i] = ln + strings.Repeat(" ", width-w)
		} else if w > width {
			lines[i] = truncateANSI(ln, width)
		}
	}
	return strings.Join(lines, "\n")
}
