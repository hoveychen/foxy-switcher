package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// mode identifies which screen is currently being rendered. The model carries
// every screen's state inline so transitions are zero-cost — a TUI that
// hot-loops between four trivial views doesn't justify the overhead of
// sub-models with their own Init/Update.
type mode int

const (
	modeList mode = iota
	modeAddName
	modeAddPaste
	modeCooldown
	modeConfirmDelete
	modeError
)

// Run is the package entry point invoked by `foxy-switcher tui`.
func Run(dataDir string) error {
	c, err := NewClient(dataDir)
	if err != nil {
		return err
	}
	m := newModel(c)
	prog := tea.NewProgram(m, tea.WithAltScreen())
	_, err = prog.Run()
	return err
}

type model struct {
	client *Client

	mode mode

	accounts []Account
	hook     HookStatus
	cursor   int

	width  int
	height int

	// Add-account flow state.
	addName        textinput.Model
	addPaste       textinput.Model
	pendingState   string
	pendingURL     string
	pendingNameVal string

	// Cooldown picker state. Indexes into cooldownPresets.
	cooldownChoice int

	// Last user-facing message and any transient error.
	statusMsg string
	statusErr string

	// Latest fatal error (network / api). Cleared on next successful op.
	fatalErr string

	lastRefresh time.Time
}

var cooldownPresets = []struct {
	label string
	d     time.Duration
}{
	{"Clear cooldown", 0},
	{"5 minutes", 5 * time.Minute},
	{"30 minutes", 30 * time.Minute},
	{"1 hour", time.Hour},
	{"6 hours", 6 * time.Hour},
}

func newModel(c *Client) *model {
	name := textinput.New()
	name.Placeholder = "Account alias (optional)"
	name.CharLimit = 64
	name.Prompt = "› "

	paste := textinput.New()
	paste.Placeholder = "Paste code#state from browser"
	paste.CharLimit = 512
	paste.Prompt = "› "

	return &model{
		client:   c,
		mode:     modeList,
		addName:  name,
		addPaste: paste,
	}
}

// ============================================================================
// Bubble Tea contract
// ============================================================================

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.refreshCmd(), tickCmd())
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tickMsg:
		// Periodic refresh — keeps usage / cooldown counters live without the
		// user pressing R. 5s mirrors the daemon's hook reconcile cadence.
		return m, tea.Batch(m.refreshCmd(), tickCmd())

	case accountsMsg:
		if msg.err != nil {
			m.fatalErr = msg.err.Error()
		} else {
			m.fatalErr = ""
			m.accounts = msg.accounts
			m.hook = msg.hook
			if m.cursor >= len(m.accounts) {
				m.cursor = max(0, len(m.accounts)-1)
			}
			m.lastRefresh = time.Now()
		}
		return m, nil

	case opResultMsg:
		if msg.err != nil {
			m.statusErr = msg.err.Error()
			m.statusMsg = ""
		} else {
			m.statusMsg = msg.ok
			m.statusErr = ""
		}
		return m, m.refreshCmd()

	case loginStartMsg:
		if msg.err != nil {
			m.statusErr = "login start: " + msg.err.Error()
			m.mode = modeList
			return m, nil
		}
		m.pendingState = msg.state
		m.pendingURL = msg.url
		m.addPaste.SetValue("")
		m.addPaste.Focus()
		m.mode = modeAddPaste
		return m, textinput.Blink

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	// Forward to whichever input is focused.
	switch m.mode {
	case modeAddName:
		var cmd tea.Cmd
		m.addName, cmd = m.addName.Update(msg)
		return m, cmd
	case modeAddPaste:
		var cmd tea.Cmd
		m.addPaste, cmd = m.addPaste.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeList:
		return m.handleListKey(msg)
	case modeAddName:
		return m.handleAddNameKey(msg)
	case modeAddPaste:
		return m.handleAddPasteKey(msg)
	case modeCooldown:
		return m.handleCooldownKey(msg)
	case modeConfirmDelete:
		return m.handleConfirmDeleteKey(msg)
	case modeError:
		// Any key returns to list.
		m.mode = modeList
		m.fatalErr = ""
		return m, nil
	}
	return m, nil
}

func (m *model) handleListKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.accounts)-1 {
			m.cursor++
		}
	case "g":
		m.cursor = 0
	case "G":
		m.cursor = max(0, len(m.accounts)-1)
	case "R":
		return m, m.refreshCmd()
	case "r":
		if a, ok := m.selected(); ok {
			return m, m.opCmd("Token refreshed for "+a.Name, func(ctx context.Context) error {
				return m.client.RefreshNow(ctx, a.ID)
			})
		}
	case "e":
		if a, ok := m.selected(); ok {
			return m, m.opCmd("Enabled "+a.Name, func(ctx context.Context) error {
				return m.client.Enable(ctx, a.ID)
			})
		}
	case "d":
		if a, ok := m.selected(); ok {
			return m, m.opCmd("Disabled "+a.Name, func(ctx context.Context) error {
				return m.client.Disable(ctx, a.ID)
			})
		}
	case "c":
		if _, ok := m.selected(); ok {
			m.cooldownChoice = 0
			m.mode = modeCooldown
		}
	case "x":
		if _, ok := m.selected(); ok {
			m.mode = modeConfirmDelete
		}
	case "a":
		// Kick off OAuth: ask for an alias first, then hit /login.
		m.addName.SetValue("")
		m.addName.Focus()
		m.mode = modeAddName
		return m, textinput.Blink
	}
	return m, nil
}

func (m *model) handleAddNameKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.addName.Blur()
		m.mode = modeList
		return m, nil
	case "enter":
		m.pendingNameVal = strings.TrimSpace(m.addName.Value())
		m.addName.Blur()
		// Move into a "starting login" state; the loginStartMsg will flip us
		// into modeAddPaste once we have the authorize URL.
		return m, m.loginStartCmd()
	}
	var cmd tea.Cmd
	m.addName, cmd = m.addName.Update(msg)
	return m, cmd
}

func (m *model) handleAddPasteKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.addPaste.Blur()
		m.mode = modeList
		m.pendingState = ""
		m.pendingURL = ""
		return m, nil
	case "enter":
		pasted := strings.TrimSpace(m.addPaste.Value())
		if pasted == "" {
			return m, nil
		}
		state := m.pendingState
		name := m.pendingNameVal
		m.addPaste.Blur()
		m.mode = modeList
		m.pendingState = ""
		m.pendingURL = ""
		return m, m.opCmd("Account added", func(ctx context.Context) error {
			return m.client.LoginCallback(ctx, pasted, state, name)
		})
	}
	var cmd tea.Cmd
	m.addPaste, cmd = m.addPaste.Update(msg)
	return m, cmd
}

func (m *model) handleCooldownKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.mode = modeList
		return m, nil
	case "up", "k":
		if m.cooldownChoice > 0 {
			m.cooldownChoice--
		}
	case "down", "j":
		if m.cooldownChoice < len(cooldownPresets)-1 {
			m.cooldownChoice++
		}
	case "enter":
		choice := cooldownPresets[m.cooldownChoice]
		a, ok := m.selected()
		m.mode = modeList
		if !ok {
			return m, nil
		}
		label := "Cleared cooldown for " + a.Name
		if choice.d > 0 {
			label = fmt.Sprintf("Cooldown %s set on %s", choice.label, a.Name)
		}
		return m, m.opCmd(label, func(ctx context.Context) error {
			return m.client.SetCooldown(ctx, a.ID, choice.d)
		})
	}
	return m, nil
}

func (m *model) handleConfirmDeleteKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		a, ok := m.selected()
		m.mode = modeList
		if !ok {
			return m, nil
		}
		return m, m.opCmd("Deleted "+a.Name, func(ctx context.Context) error {
			return m.client.Delete(ctx, a.ID)
		})
	case "n", "N", "esc", "q":
		m.mode = modeList
	}
	return m, nil
}

func (m *model) selected() (Account, bool) {
	if m.cursor < 0 || m.cursor >= len(m.accounts) {
		return Account{}, false
	}
	return m.accounts[m.cursor], true
}

// ============================================================================
// Commands & messages
// ============================================================================

type tickMsg time.Time

func tickCmd() tea.Cmd {
	return tea.Tick(5*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

type accountsMsg struct {
	accounts []Account
	hook     HookStatus
	err      error
}

func (m *model) refreshCmd() tea.Cmd {
	c := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		accs, err := c.ListAccounts(ctx)
		if err != nil {
			return accountsMsg{err: err}
		}
		hook, err := c.HookStatus(ctx)
		if err != nil {
			return accountsMsg{err: err}
		}
		return accountsMsg{accounts: accs, hook: hook}
	}
}

type opResultMsg struct {
	ok  string
	err error
}

func (m *model) opCmd(okMsg string, fn func(ctx context.Context) error) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := fn(ctx); err != nil {
			return opResultMsg{err: err}
		}
		return opResultMsg{ok: okMsg}
	}
}

type loginStartMsg struct {
	url   string
	state string
	err   error
}

func (m *model) loginStartCmd() tea.Cmd {
	c := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := c.LoginStart(ctx)
		if err != nil {
			return loginStartMsg{err: err}
		}
		return loginStartMsg{url: out.AuthorizeURL, state: out.State}
	}
}

// ============================================================================
// View
// ============================================================================

func (m *model) View() string {
	switch m.mode {
	case modeAddName:
		return m.viewAddName()
	case modeAddPaste:
		return m.viewAddPaste()
	case modeCooldown:
		return m.viewCooldown()
	case modeConfirmDelete:
		return m.viewConfirmDelete()
	}
	return m.viewList()
}

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	headerStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("244"))
	cursorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("212"))
	selectedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("230")).Background(lipgloss.Color("57"))
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	okStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	warnStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	errStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	helpStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	boxStyle      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
)

func (m *model) viewList() string {
	var b strings.Builder

	hookText := warnStyle.Render("hook: not installed")
	if m.hook.Installed {
		hookText = okStyle.Render("hook: installed")
	}
	b.WriteString(titleStyle.Render("foxy-switcher"))
	b.WriteString("  ")
	b.WriteString(hookText)
	b.WriteString("  ")
	b.WriteString(dimStyle.Render(fmt.Sprintf("%d account(s)", len(m.accounts))))
	if !m.lastRefresh.IsZero() {
		b.WriteString("  ")
		b.WriteString(dimStyle.Render("refreshed " + humanAge(m.lastRefresh)))
	}
	b.WriteString("\n\n")

	if m.fatalErr != "" {
		b.WriteString(errStyle.Render("error: " + m.fatalErr))
		b.WriteString("\n\n")
	}

	if len(m.accounts) == 0 {
		b.WriteString(dimStyle.Render("(no accounts — press 'a' to add one)\n"))
	} else {
		// Header row.
		b.WriteString(headerStyle.Render(formatRow("", "NAME", "EMAIL", "PLAN", "STATUS", "5H", "7D", "LAST USED")))
		b.WriteString("\n")
		for i, a := range m.accounts {
			cursor := "  "
			row := formatRow("", a.Name, a.Email, a.Plan, statusFor(a), pctOrDash(a.FiveHour), pctOrDash(a.SevenDay), humanMillis(a.LastUsedAt))
			if i == m.cursor {
				cursor = cursorStyle.Render("▸ ")
				row = selectedStyle.Render(row)
			}
			b.WriteString(cursor + row + "\n")
		}
	}

	// Status / message line.
	b.WriteString("\n")
	switch {
	case m.statusErr != "":
		b.WriteString(errStyle.Render("✗ " + m.statusErr))
	case m.statusMsg != "":
		b.WriteString(okStyle.Render("✓ " + m.statusMsg))
	default:
		b.WriteString(dimStyle.Render(" "))
	}
	b.WriteString("\n\n")

	b.WriteString(helpStyle.Render("↑/↓ move · a add · r refresh · e enable · d disable · c cooldown · x delete · R reload · q quit"))
	return b.String()
}

func (m *model) viewAddName() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Add account"))
	b.WriteString("\n\n")
	b.WriteString("Optional alias for this account (leave empty for default):\n")
	b.WriteString(m.addName.View())
	b.WriteString("\n\n")
	b.WriteString(helpStyle.Render("enter continue · esc cancel"))
	return b.String()
}

func (m *model) viewAddPaste() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Add account — authorize"))
	b.WriteString("\n\n")
	b.WriteString("1. Open this URL in a browser and approve:\n\n")
	b.WriteString(boxStyle.Render(m.pendingURL))
	b.WriteString("\n\n")
	b.WriteString("2. Paste the resulting code (looks like ")
	b.WriteString(dimStyle.Render("code#state"))
	b.WriteString(") below:\n")
	b.WriteString(m.addPaste.View())
	b.WriteString("\n\n")
	b.WriteString(helpStyle.Render("enter submit · esc cancel"))
	return b.String()
}

func (m *model) viewCooldown() string {
	a, _ := m.selected()
	var b strings.Builder
	b.WriteString(titleStyle.Render(fmt.Sprintf("Cooldown — %s", a.Name)))
	b.WriteString("\n\n")
	for i, p := range cooldownPresets {
		prefix := "  "
		line := p.label
		if i == m.cooldownChoice {
			prefix = cursorStyle.Render("▸ ")
			line = selectedStyle.Render(line)
		}
		b.WriteString(prefix + line + "\n")
	}
	b.WriteString("\n")
	b.WriteString(helpStyle.Render("↑/↓ pick · enter apply · esc cancel"))
	return b.String()
}

func (m *model) viewConfirmDelete() string {
	a, _ := m.selected()
	var b strings.Builder
	b.WriteString(titleStyle.Render("Delete account"))
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("Permanently remove %s (%s)?\n", a.Name, a.Email))
	b.WriteString(dimStyle.Render("(refresh_token will be discarded; the Anthropic side keeps no state to clean up)\n\n"))
	b.WriteString(helpStyle.Render("y confirm · n/esc cancel"))
	return b.String()
}

// ============================================================================
// Formatting helpers
// ============================================================================

func formatRow(_ string, name, email, plan, status, fivehr, sevenday, lastUsed string) string {
	return fmt.Sprintf("%-20s %-30s %-12s %-18s %5s %5s  %s",
		truncate(name, 20),
		truncate(email, 30),
		truncate(plan, 12),
		truncate(status, 18),
		fivehr, sevenday, lastUsed,
	)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func statusFor(a Account) string {
	if a.Status == "disabled" {
		return "disabled"
	}
	if a.CooldownUntil > time.Now().UnixMilli() {
		left := time.Until(time.UnixMilli(a.CooldownUntil)).Round(time.Second)
		return "cooldown " + left.String()
	}
	return "active"
}

func pctOrDash(w *UsageWindow) string {
	if w == nil {
		return "  —  "
	}
	return fmt.Sprintf("%4.0f%%", w.Utilization)
}

func humanMillis(ms int64) string {
	if ms == 0 {
		return "never"
	}
	return humanAge(time.UnixMilli(ms))
}

func humanAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
