package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/atotto/clipboard"
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
	modeConfirmDelete
	modeError
	modeSearch
	modeThresholds
)

// Run is the package entry point invoked by `foxy-switcher tui`. mode is a
// short label rendered in the header so the user can tell whether this TUI
// session is talking to a pre-existing daemon ("attached") or running its own
// embedded one ("embedded"); pass "" to hide the chip entirely.
//
// WithMouseCellMotion enables click + wheel events; this means the terminal's
// native click-to-select-text is intercepted while the TUI runs (hold Option
// on macOS terminals to get native selection back).
func Run(dataDir, mode string) error {
	c, err := NewClient(dataDir)
	if err != nil {
		return err
	}
	app := newApp(c, dataDir, mode)
	prog := tea.NewProgram(app, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err = prog.Run()
	return err
}

type model struct {
	client *Client

	mode mode

	// accounts is the *visible* slice (after filter+search). allAccounts is
	// the unfiltered server response — kept around so ops can resolve names
	// and the App can mirror the full pool to Dashboard/Activity.
	accounts    []Account
	allAccounts []Account
	cred        CredStatus
	autoSwitch  AutoSwitch
	filter      accountsFilterKey
	cursor      int

	width  int
	height int

	// Add-account flow state.
	addPaste     textinput.Model
	pendingState string
	pendingURL   string

	// Search input state. searchQuery is the *committed* substring filter;
	// while modeSearch is active, every keystroke recomputes m.accounts so
	// the list shrinks live as the user types. Esc reverts and clears.
	search      textinput.Model
	searchQuery string

	// Threshold editor state. thresholdAccountID is the row that was
	// selected when the modal opened (the cursor may move under it via
	// background refresh). thresholdValues holds 5h / 7d / 7d-Sonnet in
	// that order.
	thresholdField     int
	thresholdValues    [3]float64
	thresholdAccountID int64

	// Last user-facing message and any transient error. Both auto-dismiss
	// after statusToastTTL via statusExpiredMsg.
	statusMsg       string
	statusErr       string
	statusExpiresAt time.Time

	// Latest fatal error (network / api). Cleared on next successful op.
	fatalErr string

	lastRefresh time.Time

	// Async op spinner. pendingOp is the user-facing label shown while a
	// command is in flight; empty means idle. pendingAccountID is the
	// row-level target so renderAccountRow can also surface a busy chip
	// inline — the bottom statusline's spinner is too peripheral to
	// notice for fast local-daemon round-trips.
	spinner          spinner.Model
	pendingOp        string
	pendingAccountID int64

	// daemonMode labels the source of the daemon this TUI is talking to —
	// "attached" (pre-existing) or "embedded" (started by this TUI). Empty
	// hides the header chip.
	daemonMode string

	// disableAdminActions hides destructive vault writes (add / delete /
	// pause / login flow) when the daemon is in agent mode and therefore
	// only has lease access to the upstream vault. Mirrors the desktop's
	// agent-lease-only UI rule. App.Update wires this on every
	// onboardingDecisionMsg.
	disableAdminActions bool
}

// accountsFilterKey identifies which Accounts chip is active. Filter is
// applied client-side because the daemon already returns the full slice and
// "give me a subset" is the user's UI affordance, not a backend optimization.
type accountsFilterKey int

const (
	accFilterAll accountsFilterKey = iota
	accFilterActive
	accFilterPaused
	accFilterCooling
)

func accountsChips() []struct {
	key   accountsFilterKey
	label string
} {
	return []struct {
		key   accountsFilterKey
		label string
	}{
		{accFilterAll, "All"},
		{accFilterActive, "Active"},
		{accFilterPaused, "Paused"},
		{accFilterCooling, "Cooling"},
	}
}

// applyAccountsView is the full Accounts-page predicate: chip filter ∩ name
// substring search. Search is case-insensitive on Account.Name and is
// applied after the chip filter so the filter chip's count semantics still
// match the chips even when search is active.
func applyAccountsView(all []Account, key accountsFilterKey, query string) []Account {
	filtered := applyAccountsFilter(all, key)
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return filtered
	}
	out := make([]Account, 0, len(filtered))
	for _, a := range filtered {
		if strings.Contains(strings.ToLower(a.Name), q) {
			out = append(out, a)
		}
	}
	return out
}

// applyAccountsFilter returns the subset matching the given chip. "Cooling"
// reflects the live threshold-eligibility check used by the selector, so the
// chip's count matches the dashboard's "Next reset" KPI.
func applyAccountsFilter(all []Account, key accountsFilterKey) []Account {
	if key == accFilterAll {
		return all
	}
	out := make([]Account, 0, len(all))
	for _, a := range all {
		switch key {
		case accFilterActive:
			if a.Status != "paused" && !accountIsCooling(a) {
				out = append(out, a)
			}
		case accFilterPaused:
			if a.Status == "paused" {
				out = append(out, a)
			}
		case accFilterCooling:
			if a.Status != "paused" && accountIsCooling(a) {
				out = append(out, a)
			}
		}
	}
	return out
}

func newModel(c *Client) *model {
	paste := textinput.New()
	paste.Placeholder = "Paste code#state from browser"
	paste.CharLimit = 512
	paste.Prompt = "› "

	search := textinput.New()
	search.Placeholder = "filter accounts by name"
	search.CharLimit = 128
	search.Prompt = "/ "

	sp := spinner.New(
		spinner.WithSpinner(spinner.Dot),
		spinner.WithStyle(lipgloss.NewStyle().Foreground(accentBrand)),
	)

	return &model{
		client:   c,
		mode:     modeList,
		addPaste: paste,
		search:   search,
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
		// Periodic refresh — keeps usage / reset counters live without the
		// user pressing R. 5s mirrors the daemon's hook reconcile cadence.
		return m, tea.Batch(m.refreshCmd(), tickCmd())

	case accountsMsg:
		if msg.err != nil {
			m.fatalErr = msg.err.Error()
		} else {
			m.fatalErr = ""
			m.allAccounts = msg.accounts
			m.accounts = applyAccountsView(msg.accounts, m.filter, m.searchQuery)
			m.cred = msg.cred
			m.autoSwitch = msg.autoSwitch
			if m.cursor >= len(m.accounts) {
				m.cursor = max(0, len(m.accounts)-1)
			}
			m.lastRefresh = time.Now()
		}
		return m, nil

	case opResultMsg:
		m.pendingOp = ""
		m.pendingAccountID = 0
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

	case tea.MouseMsg:
		return m.handleMouse(msg)
	}

	// Forward to whichever input is focused.
	if m.mode == modeAddPaste {
		var cmd tea.Cmd
		m.addPaste, cmd = m.addPaste.Update(msg)
		return m, cmd
	}
	if m.mode == modeSearch {
		var cmd tea.Cmd
		m.search, cmd = m.search.Update(msg)
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
	case modeConfirmDelete:
		return m.handleConfirmDeleteKey(msg)
	case modeSearch:
		return m.handleSearchKey(msg)
	case modeThresholds:
		return m.handleThresholdsKey(msg)
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
	case "u":
		if a, ok := m.selected(); ok {
			// Foreign-leased accounts are unique-indexed at the vault — calling
			// SelectAccount would 409 on leases_account_id_uniq. Short-circuit
			// with an inline error so the user sees who's actually holding it
			// instead of a cryptic API failure.
			if a.Lease != nil && !a.Lease.Mine {
				dev := a.Lease.DeviceName
				if dev == "" {
					dev = a.Lease.DeviceID
				}
				if dev == "" {
					dev = "another device"
				}
				m.statusErr = a.Name + " is held by " + dev
				m.statusMsg = ""
				return m, nil
			}
			return m, m.startOp("Switching to "+a.Name+"…", "Now using "+a.Name, a.ID, func(ctx context.Context) error {
				return m.client.SelectAccount(ctx, a.ID)
			})
		}
	case "r":
		if a, ok := m.selected(); ok {
			return m, m.startOp("Refreshing "+a.Name+"…", "Token refreshed for "+a.Name, a.ID, func(ctx context.Context) error {
				return m.client.RefreshNow(ctx, a.ID)
			})
		}
	case "p":
		// pause/resume is an admin write — vault-side state change. Suppressed
		// in agent mode (lease consumers don't get to flip account status).
		if m.disableAdminActions {
			return m, nil
		}
		if a, ok := m.selected(); ok {
			if a.Status == "paused" {
				return m, m.startOp("Resuming "+a.Name+"…", "Resumed "+a.Name, a.ID, func(ctx context.Context) error {
					return m.client.Resume(ctx, a.ID)
				})
			}
			return m, m.startOp("Pausing "+a.Name+"…", "Paused "+a.Name, a.ID, func(ctx context.Context) error {
				return m.client.Pause(ctx, a.ID)
			})
		}
	case "x":
		if m.disableAdminActions {
			return m, nil
		}
		if _, ok := m.selected(); ok {
			m.mode = modeConfirmDelete
		}
	case "a":
		// Login (add account) is an admin write — agent-mode daemons forward
		// to vault but the vault rejects with 405. Hide the entry point
		// rather than letting users hit a wall.
		if m.disableAdminActions {
			return m, nil
		}
		// Kick off OAuth directly — the account name is derived from the
		// profile fetched after the token exchange, so there's no alias prompt.
		return m, m.loginStartCmd()
	case "/":
		m.search.SetValue(m.searchQuery)
		m.search.Focus()
		m.search.CursorEnd()
		m.mode = modeSearch
		return m, textinput.Blink
	case "t":
		if a, ok := m.selected(); ok {
			m.thresholdAccountID = a.ID
			m.thresholdValues = [3]float64{a.FiveHourThreshold, a.SevenDayThreshold, a.SevenDaySonnetThreshold}
			m.thresholdField = 0
			m.mode = modeThresholds
		}
	case "f", "tab":
		m.cycleFilter(+1)
	case "shift+tab":
		m.cycleFilter(-1)
	case "A":
		v := m.autoSwitch
		v.Enabled = !v.Enabled
		return m, m.opCmd(autoSwitchToggleLabel(v.Enabled), func(ctx context.Context) error {
			_, err := m.client.SetAutoSwitch(ctx, v)
			return err
		})
	case "P":
		v := m.autoSwitch
		v.Policy = cyclePolicy(v.Policy, +1)
		return m, m.opCmd("Auto-switch policy: "+strings.ToUpper(v.Policy), func(ctx context.Context) error {
			_, err := m.client.SetAutoSwitch(ctx, v)
			return err
		})
	}
	return m, nil
}

// cycleFilter advances the chip and resets cursor + visible slice. Server
// data isn't re-fetched — applyAccountsView operates on the cached
// allAccounts so toggling chips is instant.
func (m *model) cycleFilter(dir int) {
	chips := accountsChips()
	n := accountsFilterKey(len(chips))
	m.filter = (m.filter + accountsFilterKey(dir) + n) % n
	m.accounts = applyAccountsView(m.allAccounts, m.filter, m.searchQuery)
	m.cursor = 0
}

func autoSwitchToggleLabel(enabled bool) string {
	if enabled {
		return "Auto-switch enabled"
	}
	return "Auto-switch disabled"
}

func (m *model) handleAddPasteKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.addPaste.Blur()
		m.mode = modeList
		m.pendingState = ""
		m.pendingURL = ""
		return m, nil
	case "ctrl+y":
		if m.pendingURL == "" {
			return m, nil
		}
		if err := clipboard.WriteAll(m.pendingURL); err != nil {
			m.statusErr = "copy URL: " + err.Error()
			m.statusMsg = ""
		} else {
			m.statusMsg = "URL copied to clipboard"
			m.statusErr = ""
		}
		m.statusExpiresAt = time.Now().Add(statusToastTTL)
		return m, statusExpireCmd()
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
		return m, m.startOp("Adding account…", "Account added", 0, func(ctx context.Context) error {
			return m.client.LoginCallback(ctx, pasted, state)
		})
	}
	var cmd tea.Cmd
	m.addPaste, cmd = m.addPaste.Update(msg)
	return m, cmd
}

// handleSearchKey runs while the substring filter is being typed. Every
// keystroke updates m.searchQuery so the list shrinks live; Enter blurs
// (the query persists), Esc reverts to whatever was committed before this
// search session opened.
func (m *model) handleSearchKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		m.searchQuery = strings.TrimSpace(m.search.Value())
		m.search.Blur()
		m.mode = modeList
		m.accounts = applyAccountsView(m.allAccounts, m.filter, m.searchQuery)
		m.cursor = 0
		return m, nil
	case "esc":
		// Esc clears the in-flight edit AND any previously committed query.
		// "Cancel" matches user expectation more than "revert to last commit".
		m.search.SetValue("")
		m.searchQuery = ""
		m.search.Blur()
		m.mode = modeList
		m.accounts = applyAccountsView(m.allAccounts, m.filter, m.searchQuery)
		m.cursor = 0
		return m, nil
	}
	var cmd tea.Cmd
	m.search, cmd = m.search.Update(msg)
	m.searchQuery = m.search.Value()
	m.accounts = applyAccountsView(m.allAccounts, m.filter, m.searchQuery)
	if m.cursor >= len(m.accounts) {
		m.cursor = max(0, len(m.accounts)-1)
	}
	return m, cmd
}

// handleThresholdsKey drives the per-account threshold editor. Layout:
// `[`/`]` (or h/l) move between 5h / 7d / 7d-Sonnet; `-`/`+` step ±5%
// clamped to 0..100 (100 = "do not skip on this window"). Enter commits
// via SetThresholds; Esc cancels without writing.
func (m *model) handleThresholdsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.mode = modeList
		return m, nil
	case "left", "h", "[":
		if m.thresholdField > 0 {
			m.thresholdField--
		}
	case "right", "l", "]", "tab":
		if m.thresholdField < 2 {
			m.thresholdField++
		}
	case "-", "_":
		m.stepThreshold(-5)
	case "+", "=":
		m.stepThreshold(+5)
	case "0":
		// Quick "disable this window" — set threshold to 100 (selector
		// won't skip on it).
		m.thresholdValues[m.thresholdField] = 100
	case "enter":
		id := m.thresholdAccountID
		v := m.thresholdValues
		m.mode = modeList
		return m, m.startOp("Updating thresholds…", "Thresholds saved", id, func(ctx context.Context) error {
			return m.client.SetThresholds(ctx, id, v[0], v[1], v[2])
		})
	}
	return m, nil
}

func (m *model) stepThreshold(delta float64) {
	v := m.thresholdValues[m.thresholdField] + delta
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	m.thresholdValues[m.thresholdField] = v
}

func (m *model) handleConfirmDeleteKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		a, ok := m.selected()
		m.mode = modeList
		if !ok {
			return m, nil
		}
		return m, m.startOp("Deleting "+a.Name+"…", "Deleted "+a.Name, a.ID, func(ctx context.Context) error {
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
	accounts   []Account
	cred       CredStatus
	autoSwitch AutoSwitch
	err        error
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
		// Auto-switch is a non-critical side panel — degrade gracefully so a
		// transient daemon hiccup on this endpoint doesn't blank the whole list.
		as, _ := c.GetAutoSwitch(ctx)
		return accountsMsg{accounts: accs, cred: cred, autoSwitch: as}
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
// accountID is the row this op targets (0 for non-row ops); renderAccountRow
// reads it to draw the inline busy chip.
func (m *model) startOp(pendingLabel, okMsg string, accountID int64, fn func(ctx context.Context) error) tea.Cmd {
	m.pendingOp = pendingLabel
	m.pendingAccountID = accountID
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
	case modeConfirmDelete:
		return m.viewConfirmDelete()
	case modeThresholds:
		return m.viewThresholds()
	}
	// modeSearch falls through to viewList — the list still renders, with a
	// focused search strip swapped into the header.
	return m.viewList()
}

// Style variables live in theme.go. View functions live in view_list.go,
// view_add_paste.go, view_confirm_delete.go.

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
