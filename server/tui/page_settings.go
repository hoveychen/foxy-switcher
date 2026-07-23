package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// settingsRowID is a stable identifier for the focusable rows on the page.
// Adding a row means: add a const, append to settingsRowOrder, render it in
// view, and handle ←/→/enter in handleAction.
type settingsRowID int

const (
	rowThemePicker settingsRowID = iota
	rowPollInterval
	rowDefaultFiveHour
	rowDefaultSevenDay
	rowDefaultSevenDaySonnet
	rowRestoreNative
	rowAutoSwitchEnabled
	rowAutoSwitchPolicy
	rowResetData
)

// rowOrder is the focusable-row sequence for keyboard nav and renderers.
// In agent mode the default-threshold row is filtered out: thresholds are
// vault-side state shared across agents, so a lease-only consumer can't
// edit them and the row would only frustrate the user. Mirrors the
// agent-lease-only desktop UI rule.
func (p *settingsPage) rowOrder() []settingsRowID {
	out := []settingsRowID{
		rowThemePicker,
		rowPollInterval,
	}
	if !p.adminDisabled() {
		out = append(out, rowDefaultFiveHour, rowDefaultSevenDay, rowDefaultSevenDaySonnet)
	}
	out = append(out,
		rowRestoreNative,
		rowAutoSwitchEnabled,
		rowAutoSwitchPolicy,
		rowResetData,
	)
	return out
}

// adminDisabled returns true when the daemon is in agent mode (paired with
// a remote vault). The flag suppresses vault-side admin writes the agent's
// lease boundary would 405 anyway.
func (p *settingsPage) adminDisabled() bool {
	return p.about.Mode == "agent"
}

func (p *settingsPage) indexOfRow(id settingsRowID) int {
	for i, r := range p.rowOrder() {
		if r == id {
			return i
		}
	}
	return 0
}

// autoSwitchPolicies — match the server's allowedPolicies map. Order is the
// cycle order in the picker.
var autoSwitchPolicies = []string{"lru", "lowest", "rr"}

// settingsPage owns Settings + AutoSwitch + About state and the input cursor.
// Three independent loads on first visit; Set* calls are issued inline on key
// presses with optimistic local state — the response echoes the clamped form
// so the row snaps to it.
type settingsPage struct {
	width, height int

	settings   Settings
	autoSwitch AutoSwitch
	about      About
	daemonMode string

	settingsLoaded   bool
	autoSwitchLoaded bool
	aboutLoaded      bool

	loadErr  string
	loadedAt time.Time

	cursor       int
	confirmReset bool   // armed by R, fired by Enter
	flash        string // transient status line (saved/error)
	flashUntil   time.Time
}

func newSettingsPage() *settingsPage { return &settingsPage{} }

func (p *settingsPage) setSize(w, h int) {
	p.width = w
	p.height = h
}

func (p *settingsPage) setDaemonMode(mode string) {
	p.daemonMode = mode
}

// themeChangedMsg flows from Settings → App so the App can persist the choice
// to <dataDir>/tui.json. The page already applied it locally via applyTheme.
type themeChangedMsg struct{ name string }

// settingsLoadedMsg consolidates the three reads. Any field that errored
// surfaces in loadErr; the rest still populate so partial degradation is
// readable.
type settingsLoadedMsg struct {
	settings   *Settings
	autoSwitch *AutoSwitch
	about      *About
	err        error
}

type settingsSavedMsg struct {
	settings   *Settings
	autoSwitch *AutoSwitch
	err        error
}

func settingsLoadCmd(c *Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s, sErr := c.GetSettings(ctx)
		a, aErr := c.GetAutoSwitch(ctx)
		ab, abErr := c.GetAbout(ctx)
		var err error
		switch {
		case sErr != nil:
			err = sErr
		case aErr != nil:
			err = aErr
		case abErr != nil:
			err = abErr
		}
		msg := settingsLoadedMsg{err: err}
		if sErr == nil {
			msg.settings = &s
		}
		if aErr == nil {
			msg.autoSwitch = &a
		}
		if abErr == nil {
			msg.about = &ab
		}
		return msg
	}
}

func settingsSaveCmd(c *Client, s Settings) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := c.SetSettings(ctx, s)
		if err != nil {
			return settingsSavedMsg{err: err}
		}
		return settingsSavedMsg{settings: &out}
	}
}

func autoSwitchSaveCmd(c *Client, v AutoSwitch) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := c.SetAutoSwitch(ctx, v)
		if err != nil {
			return settingsSavedMsg{err: err}
		}
		return settingsSavedMsg{autoSwitch: &out}
	}
}

func resetDataCmd(c *Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := c.ResetData(ctx)
		return settingsSavedMsg{err: err}
	}
}

func (p *settingsPage) onLoaded(msg settingsLoadedMsg) {
	if msg.settings != nil {
		p.settings = *msg.settings
		p.settingsLoaded = true
	}
	if msg.autoSwitch != nil {
		p.autoSwitch = *msg.autoSwitch
		p.autoSwitchLoaded = true
	}
	if msg.about != nil {
		p.about = *msg.about
		p.aboutLoaded = true
	}
	if msg.err != nil {
		p.loadErr = msg.err.Error()
	} else {
		p.loadErr = ""
	}
	p.loadedAt = time.Now()
}

func (p *settingsPage) onSaved(msg settingsSavedMsg) {
	if msg.err != nil {
		p.flash = "✗ " + msg.err.Error()
		p.flashUntil = time.Now().Add(4 * time.Second)
		return
	}
	if msg.settings != nil {
		p.settings = *msg.settings
	}
	if msg.autoSwitch != nil {
		p.autoSwitch = *msg.autoSwitch
	}
	p.flash = "✓ saved"
	p.flashUntil = time.Now().Add(2 * time.Second)
}

// Update handles page-local keys. Returns the next cmd and a handled flag.
func (p *settingsPage) Update(msg tea.Msg, c *Client) (tea.Cmd, bool) {
	switch m := msg.(type) {
	case settingsLoadedMsg:
		p.onLoaded(m)
		return nil, true
	case settingsSavedMsg:
		p.onSaved(m)
		return nil, true
	case tea.KeyMsg:
		return p.handleKey(m, c)
	}
	return nil, false
}

func (p *settingsPage) handleKey(msg tea.KeyMsg, c *Client) (tea.Cmd, bool) {
	if p.confirmReset {
		switch msg.String() {
		case "enter", "y":
			p.confirmReset = false
			return resetDataCmd(c), true
		case "esc", "n", "q":
			p.confirmReset = false
			return nil, true
		}
		return nil, true
	}

	switch msg.String() {
	case "up", "k":
		if p.cursor > 0 {
			p.cursor--
		}
		return nil, true
	case "down", "j":
		if p.cursor < len(p.rowOrder())-1 {
			p.cursor++
		}
		return nil, true
	case "left", "h":
		return p.handleAction(c, -1)
	case "right", "l":
		return p.handleAction(c, +1)
	case "enter", " ":
		return p.handleAction(c, 0)
	case "r":
		// Quick path: place cursor on Reset row and arm the confirm.
		p.cursor = p.indexOfRow(rowResetData)
		p.confirmReset = true
		return nil, true
	case "R":
		p.cursor = p.indexOfRow(rowResetData)
		p.confirmReset = true
		return nil, true
	}
	return nil, false
}

// handleAction applies a direction (-1, 0=enter, +1) to the focused row. The
// 0 case is "activate" — toggles bool, fires reset, etc.
func (p *settingsPage) handleAction(c *Client, dir int) (tea.Cmd, bool) {
	row := p.rowOrder()[p.cursor]
	switch row {
	case rowThemePicker:
		if dir == 0 {
			return nil, true
		}
		t := nextThemeBy(currentTheme, dir)
		applyTheme(t)
		name := t.Name
		return func() tea.Msg { return themeChangedMsg{name: name} }, true
	case rowPollInterval:
		if dir == 0 {
			return nil, true
		}
		s := p.settings
		s.UsagePollIntervalSec = clampInt(s.UsagePollIntervalSec+dir*30, 30, 300)
		return settingsSaveCmd(c, s), true
	case rowDefaultFiveHour:
		if dir == 0 {
			return nil, true
		}
		s := p.settings
		s.DefaultFiveHourThreshold = clampFloat(s.DefaultFiveHourThreshold+float64(dir*5), 50, 100)
		return settingsSaveCmd(c, s), true
	case rowDefaultSevenDay:
		if dir == 0 {
			return nil, true
		}
		s := p.settings
		s.DefaultSevenDayThreshold = clampFloat(s.DefaultSevenDayThreshold+float64(dir*5), 50, 100)
		return settingsSaveCmd(c, s), true
	case rowDefaultSevenDaySonnet:
		if dir == 0 {
			return nil, true
		}
		s := p.settings
		s.DefaultSevenDaySonnetThreshold = clampFloat(s.DefaultSevenDaySonnetThreshold+float64(dir*5), 50, 100)
		return settingsSaveCmd(c, s), true
	case rowRestoreNative:
		s := p.settings
		s.RestoreNativeOnQuit = !s.RestoreNativeOnQuit
		return settingsSaveCmd(c, s), true
	case rowAutoSwitchEnabled:
		v := p.autoSwitch
		v.Enabled = !v.Enabled
		return autoSwitchSaveCmd(c, v), true
	case rowAutoSwitchPolicy:
		if dir == 0 {
			return nil, true
		}
		v := p.autoSwitch
		v.Policy = cyclePolicy(v.Policy, dir)
		return autoSwitchSaveCmd(c, v), true
	case rowResetData:
		if dir == 0 {
			p.confirmReset = true
		}
		return nil, true
	}
	return nil, false
}

func cyclePolicy(cur string, dir int) string {
	idx := 0
	for i, p := range autoSwitchPolicies {
		if p == cur {
			idx = i
			break
		}
	}
	n := len(autoSwitchPolicies)
	idx = (idx + dir + n) % n
	return autoSwitchPolicies[idx]
}

func nextThemeBy(cur *Theme, dir int) *Theme {
	all := allThemes()
	idx := 0
	for i, t := range all {
		if cur != nil && t.Name == cur.Name {
			idx = i
			break
		}
	}
	n := len(all)
	idx = (idx + dir + n) % n
	return all[idx]
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (p *settingsPage) view() string {
	if p.width <= 0 || p.height <= 0 {
		return ""
	}

	if !p.settingsLoaded && !p.autoSwitchLoaded && !p.aboutLoaded {
		if p.loadErr != "" {
			return errStyle.Render("✗ " + p.loadErr)
		}
		return dimStyle.Render("loading…")
	}

	behaviorRows := []string{
		p.renderRow(rowPollInterval, "Poll interval", fmt.Sprintf("%ds", p.settings.UsagePollIntervalSec)),
	}
	// Default thresholds live on the vault side; in agent mode the lease
	// boundary blocks the write and the rows' adjustments would silently
	// no-op. Hide them in lock-step with rowOrder.
	if !p.adminDisabled() {
		behaviorRows = append(behaviorRows,
			p.renderRow(rowDefaultFiveHour, "Default 5h threshold", fmt.Sprintf("%.0f%%", p.settings.DefaultFiveHourThreshold)),
			p.renderRow(rowDefaultSevenDay, "Default 7d threshold", fmt.Sprintf("%.0f%%", p.settings.DefaultSevenDayThreshold)),
			p.renderRow(rowDefaultSevenDaySonnet, "Default 7d scoped-model threshold", fmt.Sprintf("%.0f%%", p.settings.DefaultSevenDaySonnetThreshold)),
		)
	}
	behaviorRows = append(behaviorRows, p.renderRow(rowRestoreNative, "Restore native on quit", boolValue(p.settings.RestoreNativeOnQuit)))
	rows := []string{
		p.section("Appearance",
			p.renderRow(rowThemePicker, "Theme", themeName(currentTheme)),
		),
		p.section("Behavior", behaviorRows...),
		p.section("Auto-switch",
			p.renderRow(rowAutoSwitchEnabled, "Enabled", boolValue(p.autoSwitch.Enabled)),
			p.renderRow(rowAutoSwitchPolicy, "Policy", strings.ToUpper(p.autoSwitch.Policy)),
		),
		p.daemonInfoSection(),
		p.aboutSection(),
		p.section("Danger zone",
			p.renderResetRow(),
		),
	}

	body := strings.Join(filterEmpty(rows...), "\n\n")
	footer := p.renderFooter()
	bodyLines := strings.Count(body, "\n") + 1
	pad := p.height - bodyLines - 1
	if pad > 0 {
		body = body + strings.Repeat("\n", pad)
	}
	return body + "\n" + footer
}

func (p *settingsPage) section(title string, rows ...string) string {
	heading := headerStyle.Render(title)
	return heading + "\n" + strings.Join(rows, "\n")
}

// renderRow lays out "  Label … [< value >]" with the value right-padded to a
// consistent column. Focused row shows a cursor caret + selectedStyle bg so
// the user can see which row keys apply to.
func (p *settingsPage) renderRow(id settingsRowID, label, value string) string {
	const labelW = 26
	const valueW = 22

	caret := "  "
	if id == p.rowOrder()[p.cursor] {
		caret = cursorStyle.Render("▸ ")
	}
	lab := padRightPlain(label, labelW)
	val := padRightPlain(formatValue(id, value), valueW)
	row := caret + lab + val
	if id == p.rowOrder()[p.cursor] {
		row = selectedStyle.Render(row)
	}
	return row
}

// formatValue wraps the raw value with affordance hints — picker rows get
// `< value >`, toggles get `[×]`/`[ ]`, plain steppers get `−value+`. Style is
// kept terse so the row stays readable in narrow terminals.
func formatValue(id settingsRowID, value string) string {
	switch id {
	case rowThemePicker, rowAutoSwitchPolicy:
		return dimStyle.Render("◀ ") + value + dimStyle.Render(" ▶")
	case rowPollInterval, rowDefaultFiveHour, rowDefaultSevenDay, rowDefaultSevenDaySonnet:
		return dimStyle.Render("− ") + value + dimStyle.Render(" +")
	case rowRestoreNative, rowAutoSwitchEnabled:
		return value
	default:
		return value
	}
}

func boolValue(b bool) string {
	if b {
		return okStyle.Render("[×] on")
	}
	return dimStyle.Render("[ ] off")
}

func (p *settingsPage) daemonInfoSection() string {
	if !p.aboutLoaded {
		return p.section("Daemon info", dimStyle.Render("loading…"))
	}
	a := p.about
	uptime := time.Duration(a.UptimeSeconds) * time.Second
	dirCol := p.width - 30
	if dirCol < 16 {
		dirCol = 16
	}
	rows := []string{
		settingsKV("Mode", fmt.Sprintf("%s :%d", p.daemonModeFromAccounts(), a.Port)),
		settingsKV("PID", fmt.Sprintf("%d   uptime %s", a.PID, humanCountdown(uptime))),
		settingsKV("Data dir", truncate(a.DataDir, dirCol)),
		settingsKV("State.db", humanBytes(a.SQLiteSizeB)),
	}
	return p.section("Daemon info", rows...)
}

func (p *settingsPage) daemonModeFromAccounts() string {
	if p.daemonMode == "" {
		return "daemon"
	}
	return p.daemonMode
}

func (p *settingsPage) aboutSection() string {
	if !p.aboutLoaded {
		return ""
	}
	a := p.about
	commit := a.Commit
	if a.CommitDirty {
		commit = commit + "+dirty"
	}
	value := a.Version
	if commit != "" {
		value = value + dimStyle.Render(" · "+commit)
	}
	return p.section("About",
		settingsKV("foxy-switcher", value),
		settingsKV("Build", fmt.Sprintf("%s %s/%s", a.GoVersion, a.OS, a.Arch)),
	)
}

func (p *settingsPage) renderResetRow() string {
	if p.confirmReset {
		return cursorStyle.Render("▸ ") + errStyle.Render("Reset all data?  ") +
			dimStyle.Render("Enter = wipe + exit daemon · Esc = cancel")
	}
	caret := "  "
	if rowResetData == p.rowOrder()[p.cursor] {
		caret = cursorStyle.Render("▸ ")
	}
	row := caret + padRightPlain("Reset all data", 26) +
		dimStyle.Render("press R or enter to arm")
	if rowResetData == p.rowOrder()[p.cursor] {
		row = selectedStyle.Render(row)
	}
	return row
}

func (p *settingsPage) renderFooter() string {
	if p.flash != "" && time.Now().Before(p.flashUntil) {
		return helpStyle.Render(p.flash)
	}
	hint := "↑/↓ row · ←/→ change · enter activate · R reset"
	if p.loadedAt.IsZero() {
		return helpStyle.Render(hint)
	}
	return helpStyle.Render(hint + " · refreshed " + humanAge(p.loadedAt))
}

// settingsKV renders a "label  value" row aligned to the same labelW column
// as renderRow. Used for read-only sections (Daemon info / About).
func settingsKV(label, value string) string {
	const labelW = 26
	return "  " + padRightPlain(label, labelW) + value
}

// humanBytes formats a byte count as "4.2 MB" / "812 KB" — purely for the
// state.db size display.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// themeName returns a friendly label for the active theme. The Theme struct's
// Name field doubles as the persistence key, so we display it verbatim.
func themeName(t *Theme) string {
	if t == nil {
		return "—"
	}
	return t.Name
}
