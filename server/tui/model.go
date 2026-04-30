package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
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
	cred     CredStatus
	cursor   int

	width  int
	height int

	// Add-account flow state.
	addPaste     textinput.Model
	pendingState string
	pendingURL   string

	// Cooldown picker state. Indexes into cooldownPresets.
	cooldownChoice int

	// Last user-facing message and any transient error. Both auto-dismiss
	// after statusToastTTL via statusExpiredMsg.
	statusMsg       string
	statusErr       string
	statusExpiresAt time.Time

	// Latest fatal error (network / api). Cleared on next successful op.
	fatalErr string

	lastRefresh time.Time

	// Async op spinner. pendingOp is the user-facing label shown while a
	// command is in flight; empty means idle.
	spinner   spinner.Model
	pendingOp string
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
	paste := textinput.New()
	paste.Placeholder = "Paste code#state from browser"
	paste.CharLimit = 512
	paste.Prompt = "› "

	sp := spinner.New(
		spinner.WithSpinner(spinner.Dot),
		spinner.WithStyle(lipgloss.NewStyle().Foreground(accentBrand)),
	)

	return &model{
		client:   c,
		mode:     modeList,
		addPaste: paste,
		spinner:  sp,
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
			m.cred = msg.cred
			if m.cursor >= len(m.accounts) {
				m.cursor = max(0, len(m.accounts)-1)
			}
			m.lastRefresh = time.Now()
		}
		return m, nil

	case opResultMsg:
		m.pendingOp = ""
		if msg.err != nil {
			m.statusErr = msg.err.Error()
			m.statusMsg = ""
		} else {
			m.statusMsg = msg.ok
			m.statusErr = ""
		}
		m.statusExpiresAt = time.Now().Add(statusToastTTL)
		return m, tea.Batch(m.refreshCmd(), statusExpireCmd())

	case statusExpiredMsg:
		if !m.statusExpiresAt.IsZero() && !time.Now().Before(m.statusExpiresAt) {
			m.statusMsg = ""
			m.statusErr = ""
			m.statusExpiresAt = time.Time{}
		}
		return m, nil

	case spinner.TickMsg:
		if m.pendingOp == "" {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

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
	if m.mode == modeAddPaste {
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
			return m, m.startOp("Refreshing "+a.Name+"…", "Token refreshed for "+a.Name, func(ctx context.Context) error {
				return m.client.RefreshNow(ctx, a.ID)
			})
		}
	case "e":
		if a, ok := m.selected(); ok {
			return m, m.startOp("Enabling "+a.Name+"…", "Enabled "+a.Name, func(ctx context.Context) error {
				return m.client.Enable(ctx, a.ID)
			})
		}
	case "d":
		if a, ok := m.selected(); ok {
			return m, m.startOp("Disabling "+a.Name+"…", "Disabled "+a.Name, func(ctx context.Context) error {
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
		// Kick off OAuth directly — the account name is derived from the
		// profile fetched after the token exchange, so there's no alias prompt.
		return m, m.loginStartCmd()
	}
	return m, nil
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
		m.addPaste.Blur()
		m.mode = modeList
		m.pendingState = ""
		m.pendingURL = ""
		return m, m.startOp("Adding account…", "Account added", func(ctx context.Context) error {
			return m.client.LoginCallback(ctx, pasted, state)
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
		return m, m.startOp("Updating cooldown…", label, func(ctx context.Context) error {
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
		return m, m.startOp("Deleting "+a.Name+"…", "Deleted "+a.Name, func(ctx context.Context) error {
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

const statusToastTTL = 5 * time.Second

// statusExpiredMsg signals that a status toast scheduled at op-completion has
// reached its TTL. The handler only clears if the recorded expiry time has
// actually elapsed, so a newer toast scheduled in between safely supersedes
// older ticks.
type statusExpiredMsg struct{}

func statusExpireCmd() tea.Cmd {
	return tea.Tick(statusToastTTL, func(time.Time) tea.Msg { return statusExpiredMsg{} })
}

type accountsMsg struct {
	accounts []Account
	cred     CredStatus
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
		cred, err := c.CredStatus(ctx)
		if err != nil {
			return accountsMsg{err: err}
		}
		return accountsMsg{accounts: accs, cred: cred}
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

// startOp begins an async op: stamps pendingOp so the spinner+label render in
// the status line, then batches the spinner tick with the actual command.
func (m *model) startOp(pendingLabel, okMsg string, fn func(ctx context.Context) error) tea.Cmd {
	m.pendingOp = pendingLabel
	m.statusErr = ""
	m.statusMsg = ""
	return tea.Batch(m.spinner.Tick, m.opCmd(okMsg, fn))
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
	case modeAddPaste:
		return m.viewAddPaste()
	case modeCooldown:
		return m.viewCooldown()
	case modeConfirmDelete:
		return m.viewConfirmDelete()
	}
	return m.viewList()
}

// Style variables live in theme.go. View functions live in view_list.go,
// view_add_paste.go, view_cooldown.go, view_confirm_delete.go.

// ============================================================================
// Formatting helpers
// ============================================================================

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
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
