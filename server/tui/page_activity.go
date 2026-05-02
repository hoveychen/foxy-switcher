package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// activityFilterKey identifies which chip is active. Server-side filtering is
// the source of truth — switching chips re-issues a fetch with a new
// ActivityFilter so we don't risk drift between client-side rules and the
// daemon's typeMatches() (which understands "error.*" wildcards).
type activityFilterKey int

const (
	actFilterAll activityFilterKey = iota
	actFilterSwitches
	actFilterRefreshes
	actFilterErrors
)

// activityChip is one filter chip. types is what we send on the wire — must
// match the server's typeMatches() taxonomy verbatim, including the "error.*"
// wildcard.
type activityChip struct {
	key   activityFilterKey
	label string
	types []string
}

func activityChips() []activityChip {
	return []activityChip{
		{actFilterAll, "All", nil},
		{actFilterSwitches, "Switches", []string{"cred.injected", "cred.restored", "cred.failed"}},
		{actFilterRefreshes, "Refreshes", []string{"token.refreshed", "usage.polled"}},
		{actFilterErrors, "Errors", []string{"error.*"}},
	}
}

// activityPage renders the daemon's event timeline with a filter chip strip
// and a scrollable table. Polling uses the same 5s cadence as the Accounts
// and Dashboard pages — all three are coalesced by re-issuing on Tick.
type activityPage struct {
	width, height int

	filter   activityFilterKey
	events   []ActivityEvent
	loadedAt time.Time
	loadErr  string

	// accounts is mirrored from the Accounts page so ID → Name resolution
	// doesn't require a second fetch. Empty until first accountsMsg lands.
	accounts map[int64]string

	cursor int // index into events; 0 = newest
	top    int // first visible row index
}

func newActivityPage() *activityPage {
	return &activityPage{accounts: map[int64]string{}}
}

func (p *activityPage) setSize(w, h int) {
	p.width = w
	p.height = h
}

// setAccounts is called by the App after every successful accountsMsg so the
// page can resolve account_id → name without a second round-trip.
func (p *activityPage) setAccounts(accs []Account) {
	m := make(map[int64]string, len(accs))
	for _, a := range accs {
		m[a.ID] = a.Name
	}
	p.accounts = m
}

// activityLoadedMsg is the result of the page's poll cycle.
type activityLoadedMsg struct {
	filter activityFilterKey
	events []ActivityEvent
	err    error
}

type activityTickMsg struct{}

const activityPollInterval = 5 * time.Second

func activityTickCmd() tea.Cmd {
	return tea.Tick(activityPollInterval, func(time.Time) tea.Msg { return activityTickMsg{} })
}

// activityLoadCmd issues GET /api/activity for the given filter. Limit is
// capped at 200 — same default as the server. Stamping the requested filter
// onto the message lets us drop stale responses if the user toggled chips
// while the request was in flight.
func activityLoadCmd(c *Client, key activityFilterKey) tea.Cmd {
	chip := chipFor(key)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		evs, err := c.ListActivity(ctx, ActivityFilter{Limit: 200, Types: chip.types})
		return activityLoadedMsg{filter: key, events: evs, err: err}
	}
}

func chipFor(key activityFilterKey) activityChip {
	for _, c := range activityChips() {
		if c.key == key {
			return c
		}
	}
	return activityChips()[0]
}

func (p *activityPage) onLoaded(msg activityLoadedMsg) {
	if msg.filter != p.filter {
		// Stale — user moved on.
		return
	}
	if msg.err != nil {
		p.loadErr = msg.err.Error()
		return
	}
	p.events = msg.events
	p.loadedAt = time.Now()
	p.loadErr = ""
	if p.cursor >= len(p.events) {
		p.cursor = 0
	}
	p.clampScroll()
}

// Update handles page-local key bindings — filter cycling and table scroll.
// Returns the next command (re-fetch when filter changes) and a `handled`
// flag the App uses to decide whether to fall through to other routing.
func (p *activityPage) Update(msg tea.Msg, c *Client) (tea.Cmd, bool) {
	switch m := msg.(type) {
	case activityLoadedMsg:
		p.onLoaded(m)
		return nil, true
	case activityTickMsg:
		return tea.Batch(activityLoadCmd(c, p.filter), activityTickCmd()), true
	case tea.KeyMsg:
		return p.handleKey(m, c)
	}
	return nil, false
}

func (p *activityPage) handleKey(msg tea.KeyMsg, c *Client) (tea.Cmd, bool) {
	switch msg.String() {
	case "f", "tab":
		p.filter = (p.filter + 1) % activityFilterKey(len(activityChips()))
		p.cursor = 0
		p.top = 0
		return activityLoadCmd(c, p.filter), true
	case "shift+tab":
		n := activityFilterKey(len(activityChips()))
		p.filter = (p.filter + n - 1) % n
		p.cursor = 0
		p.top = 0
		return activityLoadCmd(c, p.filter), true
	case "1", "2", "3", "4":
		// Reserved for global page switching — let App handle it.
		return nil, false
	case "up", "k":
		if p.cursor > 0 {
			p.cursor--
		}
		p.clampScroll()
		return nil, true
	case "down", "j":
		if p.cursor < len(p.events)-1 {
			p.cursor++
		}
		p.clampScroll()
		return nil, true
	case "g", "home":
		p.cursor = 0
		p.top = 0
		return nil, true
	case "G", "end":
		if n := len(p.events); n > 0 {
			p.cursor = n - 1
		}
		p.clampScroll()
		return nil, true
	case "pgup":
		step := p.visibleRows()
		if step < 1 {
			step = 1
		}
		p.cursor -= step
		if p.cursor < 0 {
			p.cursor = 0
		}
		p.clampScroll()
		return nil, true
	case "pgdown":
		step := p.visibleRows()
		if step < 1 {
			step = 1
		}
		p.cursor += step
		if n := len(p.events); p.cursor >= n {
			p.cursor = n - 1
		}
		if p.cursor < 0 {
			p.cursor = 0
		}
		p.clampScroll()
		return nil, true
	case "r":
		return activityLoadCmd(c, p.filter), true
	}
	return nil, false
}

func (p *activityPage) clampScroll() {
	rows := p.visibleRows()
	if rows < 1 {
		p.top = 0
		return
	}
	if p.cursor < p.top {
		p.top = p.cursor
	}
	if p.cursor >= p.top+rows {
		p.top = p.cursor - rows + 1
	}
	if p.top < 0 {
		p.top = 0
	}
}

// visibleRows is the number of event rows that fit in the current page area
// after accounting for the filter strip (1), spacer (1), table header (1),
// and footer (1). Returns 0 when the page is too short to render.
func (p *activityPage) visibleRows() int {
	const chrome = 4
	r := p.height - chrome
	if r < 0 {
		return 0
	}
	return r
}

func (p *activityPage) view() string {
	if p.width <= 0 || p.height <= 0 {
		return ""
	}

	chips := p.renderChips()
	header := p.renderTableHeader()
	rows := p.renderRows()
	footer := p.renderFooter()

	body := strings.Join([]string{chips, "", header, rows}, "\n")
	bodyLines := strings.Count(body, "\n") + 1
	pad := p.height - bodyLines - 1 // -1 for footer line
	if pad > 0 {
		body = body + strings.Repeat("\n", pad)
	}
	return body + "\n" + footer
}

func (p *activityPage) renderChips() string {
	chips := activityChips()
	parts := make([]string, 0, len(chips)+1)
	for _, ch := range chips {
		variant := pillSoft
		tone := pillNeutral
		if ch.key == p.filter {
			variant = pillFilled
			tone = pillAccent
		}
		parts = append(parts, pill(ch.label, tone, variant))
	}
	count := dimStyle.Render(fmt.Sprintf("%d events", len(p.events)))
	return strings.Join(parts, " ") + "   " + count
}

func (p *activityPage) renderTableHeader() string {
	tw := p.tableColumns()
	cols := []string{
		padRightPlain("TIME", tw.time),
		padRightPlain("LV", tw.lv),
		padRightPlain("TYPE", tw.typ),
		padRightPlain("ACCOUNT", tw.account),
		padRightPlain("MESSAGE", tw.message),
	}
	return headerStyle.Render(strings.Join(cols, " "))
}

func (p *activityPage) renderRows() string {
	if p.loadErr != "" {
		return errStyle.Render("✗ " + p.loadErr)
	}
	if len(p.events) == 0 {
		if !p.loadedAt.IsZero() {
			return dimStyle.Render("(no events match filter)")
		}
		return dimStyle.Render("loading…")
	}

	rows := p.visibleRows()
	if rows < 1 {
		return ""
	}
	end := p.top + rows
	if end > len(p.events) {
		end = len(p.events)
	}

	tw := p.tableColumns()
	out := make([]string, 0, end-p.top)
	for i := p.top; i < end; i++ {
		ev := p.events[i]
		row := p.renderRow(ev, tw)
		if i == p.cursor {
			row = selectedStyle.Render(row)
		}
		out = append(out, row)
	}
	return strings.Join(out, "\n")
}

type activityCols struct {
	time, lv, typ, account, message int
}

func (p *activityPage) tableColumns() activityCols {
	c := activityCols{
		time: 8, // "17:42:15"
		lv:   2,
		typ:  18,
	}
	if p.width < 80 {
		c.typ = 14
		c.account = 8
	} else {
		c.account = 14
	}
	used := c.time + c.lv + c.typ + c.account + 4 // 4 single-cell gutters
	c.message = p.width - used
	if c.message < 8 {
		c.message = 8
	}
	return c
}

func (p *activityPage) renderRow(ev ActivityEvent, tw activityCols) string {
	ts := time.UnixMilli(ev.Timestamp).Format("15:04:05")
	lv := severityGlyph(string(ev.Severity))
	acct := p.accountNameFor(ev.AccountID)
	if acct == "" {
		acct = "—"
	}
	acct = truncate(acct, tw.account)
	msg := truncate(ev.Message, tw.message)

	return strings.Join([]string{
		dimStyle.Render(padRightPlain(ts, tw.time)),
		padRightPlain(lv, tw.lv),
		padRightPlain(eventTypeStyled(ev), tw.typ),
		padRightPlain(acct, tw.account),
		padRightPlain(msg, tw.message),
	}, " ")
}

// eventTypeStyled colors the type column by event family so the timeline is
// scannable at a glance even without re-reading every row.
func eventTypeStyled(ev ActivityEvent) string {
	switch {
	case strings.HasPrefix(ev.Type, "error."), ev.Severity == "error":
		return errStyle.Render(ev.Type)
	case strings.HasPrefix(ev.Type, "cred."):
		return lipgloss.NewStyle().Foreground(accentBrand).Render(ev.Type)
	case strings.HasPrefix(ev.Type, "token."), strings.HasPrefix(ev.Type, "usage."):
		return lipgloss.NewStyle().Foreground(tokenInfo).Render(ev.Type)
	case ev.Severity == "warn":
		return warnStyle.Render(ev.Type)
	default:
		return ev.Type
	}
}

func (p *activityPage) accountNameFor(id int64) string {
	if id == 0 {
		return ""
	}
	if name, ok := p.accounts[id]; ok {
		return name
	}
	return fmt.Sprintf("#%d", id)
}

func (p *activityPage) renderFooter() string {
	if p.loadedAt.IsZero() {
		return helpStyle.Render("waiting for first poll…")
	}
	hint := "f filter · ↑/↓ scroll · g/G top/bottom · r refresh"
	stamp := "refreshed " + humanAge(p.loadedAt)
	return helpStyle.Render(hint+" · "+stamp)
}

// padRightPlain pads a possibly-styled string on the right to width cells.
// Truncates with the ANSI-safe helper if it's already too wide. Used by the
// table header and rows so columns line up regardless of style content.
func padRightPlain(s string, width int) string {
	w := lipgloss.Width(s)
	if w == width {
		return s
	}
	if w > width {
		return truncateANSI(s, width)
	}
	return s + strings.Repeat(" ", width-w)
}
