package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// App is the top-level Bubble Tea model. It owns one persistent `*model`
// (the Accounts page — kept as-is so its tests still apply) plus three
// lighter sub-page models for Dashboard/Activity/Settings, and routes input
// between them based on the current `screen`.
//
// Why wrap rather than rewrite: the existing `*model` carries all the live
// state (accounts list, cred status, spinner, polling cmds). Re-implementing
// that inside App would mean duplicating async state and breaking the unit
// tests that assert on the inner type. Wrapping keeps Phase 1 surgical.
type App struct {
	dataDir string

	screen   screen
	helpOpen bool

	// Pages.
	accounts  *model
	dashboard *dashboardPage
	activity  *activityPage
	settings  *settingsPage

	// Outer terminal dimensions (full screen, including chrome).
	width, height int
}

// newApp constructs the router and applies the persisted theme. Returns the
// App and the inner Accounts page so callers (Run) can also invoke the
// existing Init + lifecycle wiring.
func newApp(c *Client, dataDir, daemonMode string) *App {
	cfg := loadTUIConfig(dataDir)
	if cfg.Theme != "" {
		applyTheme(themeByName(cfg.Theme))
	}

	inner := newModel(c)
	inner.daemonMode = daemonMode

	app := &App{
		dataDir:   dataDir,
		screen:    screenDashboard,
		accounts:  inner,
		dashboard: newDashboardPage(),
		activity:  newActivityPage(),
		settings:  newSettingsPage(),
	}
	app.settings.setDaemonMode(daemonMode)
	return app
}

func (a *App) Init() tea.Cmd {
	return tea.Batch(
		a.accounts.Init(),
		dashboardLoadCmd(a.accounts.client),
		dashboardTickCmd(),
		activityLoadCmd(a.accounts.client, a.activity.filter),
		activityTickCmd(),
		settingsLoadCmd(a.accounts.client),
	)
}

func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case dashboardLoadedMsg:
		a.dashboard.onLoaded(msg)
		return a, nil
	case dashboardTickMsg:
		return a, tea.Batch(dashboardLoadCmd(a.accounts.client), dashboardTickCmd())
	case activityLoadedMsg, activityTickMsg:
		cmd, _ := a.activity.Update(msg, a.accounts.client)
		return a, cmd
	case settingsLoadedMsg, settingsSavedMsg:
		cmd, _ := a.settings.Update(msg, a.accounts.client)
		return a, cmd
	case themeChangedMsg:
		_ = saveTUIConfig(a.dataDir, tuiConfig{Theme: msg.name})
		return a, nil
	case accountsMsg:
		// Forward to the inner Accounts model so its existing handler runs,
		// then mirror the fresh slice into the Dashboard / Activity pages so
		// they don't have to re-fetch accounts on their own clock.
		updated, cmd := a.accounts.Update(msg)
		a.accounts = updated.(*model)
		if msg.err == nil {
			// Pass the unfiltered server slice so Dashboard's "Top 5" and
			// Activity's account-name lookup don't lose accounts when the
			// Accounts page is filtered to a subset.
			a.dashboard.setAccounts(msg.accounts, msg.cred)
			a.activity.setAccounts(msg.accounts)
		}
		return a, cmd

	case tea.WindowSizeMsg:
		a.width = msg.Width
		a.height = msg.Height
		a.propagateSize()
		// Forward an adjusted size to the inner Accounts model so its existing
		// layout math operates on the area inside the chrome.
		inner := tea.WindowSizeMsg{Width: a.pageWidth(), Height: a.pageHeight()}
		updated, cmd := a.accounts.Update(inner)
		a.accounts = updated.(*model)
		return a, cmd

	case tea.KeyMsg:
		// Global shortcuts win unless an accounts modal is open (textinput etc.
		// must capture every keystroke). The other pages have no modals in
		// Phase 1, so we always honor globals when the active page isn't
		// Accounts.
		if a.helpOpen {
			if k := msg.String(); k == "?" || k == "esc" || k == "q" {
				a.helpOpen = false
				return a, nil
			}
			return a, nil
		}
		if a.acceptsGlobalKey() {
			if cmd, handled := a.handleGlobalKey(msg); handled {
				return a, cmd
			}
		}
		// Forward to active page. Accounts and Activity have their own input
		// handling; Dashboard/Settings are read-only in Phase 1–3.
		if a.screen == screenAccounts {
			updated, cmd := a.accounts.Update(msg)
			a.accounts = updated.(*model)
			return a, cmd
		}
		if a.screen == screenActivity {
			cmd, _ := a.activity.Update(msg, a.accounts.client)
			return a, cmd
		}
		if a.screen == screenSettings {
			cmd, _ := a.settings.Update(msg, a.accounts.client)
			return a, cmd
		}
		return a, nil

	case tea.MouseMsg:
		if a.screen != screenAccounts {
			// Use top tabs / sidebar nav clicks as page switches.
			if scr, ok := a.sidebarHit(msg); ok {
				a.setScreen(scr)
			}
			return a, nil
		}
		if scr, ok := a.sidebarHit(msg); ok {
			a.setScreen(scr)
			return a, nil
		}
		// Translate the click into the accounts page's coordinate system.
		adjusted := a.translateMouse(msg)
		updated, cmd := a.accounts.Update(adjusted)
		a.accounts = updated.(*model)
		return a, cmd
	}

	// All other messages flow to the accounts model — it owns the polling /
	// spinner / command pipeline that drives the whole TUI.
	updated, cmd := a.accounts.Update(msg)
	a.accounts = updated.(*model)
	return a, cmd
}

// acceptsGlobalKey returns true when the App is allowed to intercept its
// global shortcuts. We refuse while the accounts page is in a modal (paste,
// confirm, thresholds, search) so digit/letter keys can't be stolen from
// textinput.
func (a *App) acceptsGlobalKey() bool {
	if a.screen != screenAccounts {
		return true
	}
	return a.accounts.mode == modeList
}

// handleGlobalKey processes the App-level shortcuts (page switch, theme
// cycle, help toggle). Returns handled=true to stop further routing.
func (a *App) handleGlobalKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	switch msg.String() {
	case "1":
		a.setScreen(screenDashboard)
		return nil, true
	case "2":
		a.setScreen(screenAccounts)
		return nil, true
	case "3":
		a.setScreen(screenActivity)
		return nil, true
	case "4":
		a.setScreen(screenSettings)
		return nil, true
	case "T":
		next := nextTheme(currentTheme)
		applyTheme(next)
		_ = saveTUIConfig(a.dataDir, tuiConfig{Theme: next.Name})
		return nil, true
	case "?":
		a.helpOpen = true
		return nil, true
	}
	return nil, false
}

func (a *App) setScreen(s screen) {
	if a.screen == s {
		return
	}
	a.screen = s
	a.propagateSize()
}

// propagateSize hands the per-page content dimensions to whichever sub-page
// is currently visible. The Accounts page receives its size via
// WindowSizeMsg in Update — this method covers the simpler placeholders.
func (a *App) propagateSize() {
	pw, ph := a.pageWidth(), a.pageHeight()
	a.dashboard.setSize(pw, ph)
	a.activity.setSize(pw, ph)
	a.settings.setSize(pw, ph)
}

// pageWidth / pageHeight return the rectangle available to a page, after
// subtracting the sidebar (or tabs) and the bottom statusbar. Callers must
// not assume positive values when the terminal is freshly opened — return
// zero is fine; pages render "Loading…" or empty.
func (a *App) pageWidth() int {
	mode := pickSidebarMode(a.width)
	w := a.width - sidebarWidthFor(mode)
	if w < 0 {
		return 0
	}
	return w
}

func (a *App) pageHeight() int {
	// statusbar(1) + (tabs strip(1) when narrow).
	h := a.height - 1
	if pickSidebarMode(a.width) == sidebarTabs {
		h -= 1
	}
	if h < 0 {
		return 0
	}
	return h
}

func (a *App) View() string {
	if a.width <= 0 || a.height <= 0 {
		return "Loading…"
	}

	mode := pickSidebarMode(a.width)
	pageView := a.activePageView()

	var body string
	if mode == sidebarTabs {
		tabs := renderSidebarTabs(a.screen, a.width)
		body = tabs + "\n" + pageView
	} else {
		sb := renderSidebar(mode, a.screen, a.pageHeight(), a.daemonStatusLine())
		body = lipgloss.JoinHorizontal(lipgloss.Top, sb, pageView)
	}

	bar := renderStatusbar(a.statusbarState(), a.width)
	full := body + "\n" + bar

	if a.helpOpen {
		modal := renderHelpModal(a.screen, a.width, a.height)
		full = centerOverlay(full, modal, a.width, a.height)
	}
	return full
}

func (a *App) activePageView() string {
	switch a.screen {
	case screenDashboard:
		return a.dashboard.view()
	case screenAccounts:
		// The accounts model already paints its full view (header + body +
		// status + footer). It already accounts for its own width/height
		// because we forwarded an adjusted WindowSizeMsg.
		return a.accounts.View()
	case screenActivity:
		return a.activity.view()
	case screenSettings:
		return a.settings.view()
	}
	return ""
}

// daemonStatusLine renders the bottom-of-sidebar daemon health row. Format:
// `<dot> <mode>` — `mode` is the same label as in the inner header chip.
func (a *App) daemonStatusLine() string {
	mode := a.accounts.daemonMode
	if mode == "" {
		mode = "daemon"
	}
	var dot string
	switch {
	case a.accounts.fatalErr != "":
		dot = lipgloss.NewStyle().Foreground(tokenDanger).Render("●")
	case a.accounts.cred.ManagedAccountID != 0 || a.accounts.cred.NativeBackupPresent:
		dot = lipgloss.NewStyle().Foreground(tokenOK).Render("●")
	default:
		dot = lipgloss.NewStyle().Foreground(tokenWarn).Render("○")
	}
	return dot + " " + dimStyle.Render(mode)
}

func (a *App) statusbarState() statusbarState {
	mode := a.accounts.daemonMode
	if mode == "" {
		mode = "daemon"
	}
	dot := lipgloss.NewStyle().Foreground(tokenOK).Render("●")
	if a.accounts.fatalErr != "" {
		dot = lipgloss.NewStyle().Foreground(tokenDanger).Render("●")
	}

	page := pageLabel(a.screen)
	// Pure-keyboard users had no on-screen cue that 1-4 switched pages; the
	// nav hint sits to the right of the bar so it's always visible.
	hint := dimStyle.Render("1-4 pages · ? help")
	return statusbarState{
		DaemonDot:   dot,
		DaemonLabel: mode,
		Breadcrumb:  page,
		NavHint:     hint,
		Clock:       "",
	}
}

func pageLabel(s screen) string {
	switch s {
	case screenDashboard:
		return "Dashboard"
	case screenAccounts:
		return "Accounts"
	case screenActivity:
		return "Activity"
	case screenSettings:
		return "Settings"
	}
	return ""
}

// sidebarHit reports which screen (if any) the click landed on. Expanded /
// collapsed modes use the left-rail rows; tabs mode uses the top strip.
func (a *App) sidebarHit(msg tea.MouseMsg) (screen, bool) {
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return 0, false
	}
	mode := pickSidebarMode(a.width)
	if mode == sidebarTabs {
		if msg.Y != 0 {
			return 0, false
		}
		return tabHitX(msg.X)
	}
	w := sidebarWidthFor(mode)
	if msg.X >= w {
		return 0, false
	}
	// Layout: header(1) + spacer(1) + 4 nav rows.
	row := msg.Y - 2
	if row < 0 || row >= 4 {
		return 0, false
	}
	return sidebarItems()[row].id, true
}

// tabHitX walks the rendered tabs row left-to-right summing pill widths to
// find which tab a given X column belongs to. Pills are joined by single
// spaces in renderSidebarTabs.
func tabHitX(x int) (screen, bool) {
	items := sidebarItems()
	cur := 0
	for _, it := range items {
		text := string(it.shortcut) + " " + it.label
		// Soft + filled both use 1-cell horizontal padding, so width = len+2.
		w := len(text) + 2
		if x >= cur && x < cur+w {
			return it.id, true
		}
		cur += w + 1
	}
	return 0, false
}

// translateMouse subtracts the sidebar width (or top tab height) from a mouse
// event so the inner accounts model receives coordinates in its own frame.
func (a *App) translateMouse(msg tea.MouseMsg) tea.MouseMsg {
	mode := pickSidebarMode(a.width)
	if mode == sidebarTabs {
		msg.Y -= 1
		return msg
	}
	msg.X -= sidebarWidthFor(mode)
	return msg
}

var _ tea.Model = (*App)(nil)
