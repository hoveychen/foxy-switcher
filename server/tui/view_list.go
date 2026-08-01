package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

type layoutMode int

const (
	layoutNarrow layoutMode = iota
	layoutRegular
	layoutWide
)

func computeLayout(width int) layoutMode {
	switch {
	case width >= 120:
		return layoutWide
	case width >= 80:
		return layoutRegular
	default:
		return layoutNarrow
	}
}

func (m *model) viewList() string {
	if m.width <= 0 {
		return "Loading…"
	}
	switch computeLayout(m.width) {
	case layoutWide:
		return m.viewListWide()
	case layoutRegular:
		return m.viewListRegular()
	default:
		return m.viewListNarrow()
	}
}

// ============================================================================
// Wide (≥120 cols) — split-pane with detail panel
// ============================================================================

func (m *model) viewListWide() string {
	header := m.renderHeader()

	listW := m.width * 35 / 100
	if listW < 36 {
		listW = 36
	}
	if listW > 56 {
		listW = 56
	}
	detailW := m.width - listW - 1

	bodyH := m.computeBodyHeight()

	var detailBody, listBody string
	if len(m.accounts) == 0 {
		listBody = dimStyle.Render("(no accounts — press 'a' to add)")
		detailBody = renderEmptyState(detailW - 4)
	} else {
		listBody = m.renderAccountList(listW - 4)
		detailBody = m.renderDetailBody(detailW - 4)
	}
	listBody = padBodyToHeight(listBody, bodyH)
	detailBody = padBodyToHeight(detailBody, bodyH)

	listPanel := panel("ACCOUNTS", listBody, listW)
	detailPanel := panel(m.detailTitle(), detailBody, detailW)

	body := lipgloss.JoinHorizontal(lipgloss.Top, listPanel, " ", detailPanel)

	return joinSections(header, body, m.renderStatusLine(), m.renderFooter())
}

// ============================================================================
// Regular (80–119 cols) — single column with inline detail under selection
// ============================================================================

func (m *model) viewListRegular() string {
	header := m.renderHeader()

	listW := m.width
	if listW > 100 {
		listW = 100
	}
	listInner := listW - 4
	var listBody string
	if len(m.accounts) == 0 {
		listBody = renderEmptyState(listInner)
	} else {
		listBody = m.renderAccountListInline(listInner)
	}
	listBody = padBodyToHeight(listBody, m.computeBodyHeight())
	listPanel := panel("ACCOUNTS", listBody, listW)

	return joinSections(header, listPanel, m.renderStatusLine(), m.renderFooter())
}

// ============================================================================
// Narrow (<80 cols) — minimal one-line rows, no detail
// ============================================================================

func (m *model) viewListNarrow() string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("foxy-switcher"))
	sb.WriteString("  ")
	sb.WriteString(m.renderCredText())
	sb.WriteString("\n\n")

	if len(m.accounts) == 0 {
		sb.WriteString(dimStyle.Render("(no accounts — press 'a' to add one)"))
	} else {
		nowMs := time.Now().UnixMilli()
		inUseID := m.inUseAccountID()
		for i, a := range m.accounts {
			isInUse := inUseID != 0 && a.ID == inUseID
			rail := rowRail(i == m.cursor, isInUse)
			dot := statusDot(a, nowMs)
			name := truncate(a.Name, m.width-6)
			if !isInUse && i != m.cursor && a.Lease != nil && !a.Lease.Mine {
				name = dimStyle.Render(name)
			}
			line := rail + dot + " " + name
			if i == m.cursor {
				line = selectedStyle.Width(m.width).Render(line)
			}
			sb.WriteString(line)
			sb.WriteString("\n")
		}
	}
	sb.WriteString("\n")
	sb.WriteString(m.renderStatusLine())
	sb.WriteString("\n\n")
	sb.WriteString(m.renderFooter())
	return sb.String()
}

// ============================================================================
// Section renderers
// ============================================================================

func (m *model) renderHeader() string {
	parts := []string{titleStyle.Render("foxy-switcher")}
	if m.daemonMode != "" {
		parts = append(parts, dimStyle.Render("("+m.daemonMode+")"))
	}
	parts = append(parts, m.renderCredText())
	parts = append(parts, m.renderAutoSwitchChip())
	parts = append(parts, dimStyle.Render(fmt.Sprintf("%d/%d", len(m.accounts), len(m.allAccounts))))
	if !m.lastRefresh.IsZero() {
		parts = append(parts, dimStyle.Render("refreshed "+humanAge(m.lastRefresh)))
	}
	top := strings.Join(parts, "  ")
	// While typing, the search input takes the second row; otherwise show
	// the filter-chip strip plus a committed-query indicator if any.
	if m.mode == modeSearch {
		return top + "\n" + m.search.View()
	}
	second := m.renderFilterChips()
	if m.searchQuery != "" {
		second += "  " + dimStyle.Render("search: "+m.searchQuery)
	}
	return top + "\n" + second
}

// renderFilterChips builds the All/Active/Paused/Cooldown row. Counts are
// computed against allAccounts so the "Cooldown 3" chip stays meaningful even
// when the user is currently filtered to "Paused".
func (m *model) renderFilterChips() string {
	chips := accountsChips()
	parts := make([]string, 0, len(chips))
	for _, ch := range chips {
		count := len(applyAccountsFilter(m.allAccounts, ch.key))
		label := fmt.Sprintf("%s %d", ch.label, count)
		variant := pillSoft
		tone := pillNeutral
		if ch.key == m.filter {
			variant = pillFilled
			tone = pillAccent
		}
		parts = append(parts, pill(label, tone, variant))
	}
	return strings.Join(parts, " ")
}

// renderAutoSwitchChip shows the daemon's auto-switch state next to the
// in-use indicator. Filled chip = on; soft = off. Policy is the trailing
// caption so a glance answers "is rotation happening, and by what rule".
func (m *model) renderAutoSwitchChip() string {
	policy := strings.ToUpper(m.autoSwitch.Policy)
	if policy == "" {
		policy = "—"
	}
	if m.autoSwitch.Enabled {
		return pill("auto: "+policy, pillAccent, pillFilled)
	}
	return pill("auto: off", pillNeutral, pillSoft)
}

func (m *model) renderCredText() string {
	switch {
	case m.cred.ManagedAccountID != 0:
		name := m.lookupAccountName(m.cred.ManagedAccountID)
		if name == "" {
			name = fmt.Sprintf("acct #%d", m.cred.ManagedAccountID)
		}
		return okStyle.Render("● in use: " + name)
	case m.cred.NativeBackupPresent:
		return okStyle.Render("● idle (native restored)")
	default:
		return warnStyle.Render("○ idle")
	}
}

func (m *model) lookupAccountName(id int64) string {
	for _, a := range m.accounts {
		if a.ID == id {
			return a.Name
		}
	}
	return ""
}

// inUseAccountID returns the id of the account currently injected into Claude
// Code's keychain, or 0 if none.
func (m *model) inUseAccountID() int64 { return m.cred.ManagedAccountID }

// renderAccountList lays out the wide-mode account list; one row per account.
//
//	▶● alice              [ in use ] [max20]
//	▍● bob                          [max5]
func (m *model) renderAccountList(innerW int) string {
	if len(m.accounts) == 0 {
		return dimStyle.Render("(no accounts — press 'a' to add)")
	}
	nowMs := time.Now().UnixMilli()
	inUseID := m.inUseAccountID()
	var lines []string
	for i, a := range m.accounts {
		isInUse := inUseID != 0 && a.ID == inUseID
		row := m.renderAccountRow(a, i == m.cursor, isInUse, innerW, nowMs)
		lines = append(lines, row)
	}
	return strings.Join(lines, "\n")
}

// renderAccountRow renders one list row honoring width constraints. When
// width is tight, the in-use chip wins over the plan badge so the active
// account is always identifiable. A foreign-lease chip ("held by …") takes
// the same emphasis slot when this account is currently leased by another
// device — and the row's name is dimmed so foreign-leased rows visually
// recede, mirroring the desktop AccountsPage's `leased-foreign` greying.
func (m *model) renderAccountRow(a Account, selected, isInUse bool, innerW int, nowMs int64) string {
	rail := rowRail(selected, isInUse)
	dot := statusDot(a, nowMs)
	plan := planBadge(a.Plan)
	chip := ""
	switch {
	case m.pendingAccountID != 0 && a.ID == m.pendingAccountID:
		chip = m.switchingChip()
	case isInUse:
		chip = inUseChip()
	default:
		chip = foreignLeaseChip(a, nowMs)
	}
	suffix, suffixW := composeRowSuffix(chip, plan, innerW-3)
	nameMax := innerW - 3 - suffixW - 1
	if nameMax < 4 {
		nameMax = 4
	}
	name := truncate(a.Name, nameMax)
	if !isInUse && !selected && a.Lease != nil && !a.Lease.Mine {
		name = dimStyle.Render(name)
	}
	nameW := lipgloss.Width(name)
	gap := innerW - 3 - nameW - suffixW
	if gap < 1 {
		gap = 1
	}
	row := rail + dot + " " + name + strings.Repeat(" ", gap) + suffix
	if selected {
		row = selectedStyle.Width(innerW).Render(row)
	}
	return row
}

// composeRowSuffix joins the optional in-use chip and plan badge into one
// trailing string, dropping the plan badge first if the combo would not fit.
func composeRowSuffix(chip, plan string, budget int) (string, int) {
	chipW := lipgloss.Width(chip)
	planW := lipgloss.Width(plan)
	switch {
	case chip != "" && plan != "" && chipW+1+planW <= budget:
		return chip + " " + plan, chipW + 1 + planW
	case chip != "":
		return chip, chipW
	default:
		return plan, planW
	}
}

// renderAccountListInline expands the selected row in place with usage bars
// and meta. Used by the regular layout.
func (m *model) renderAccountListInline(innerW int) string {
	if len(m.accounts) == 0 {
		return dimStyle.Render("(no accounts — press 'a' to add)")
	}
	nowMs := time.Now().UnixMilli()
	inUseID := m.inUseAccountID()
	var lines []string
	for i, a := range m.accounts {
		isInUse := inUseID != 0 && a.ID == inUseID
		row := m.renderAccountRow(a, i == m.cursor, isInUse, innerW, nowMs)
		lines = append(lines, row)
		if i == m.cursor {
			lines = append(lines, m.renderInlineDetail(a, innerW))
		}
	}
	return strings.Join(lines, "\n")
}

func (m *model) renderInlineDetail(a Account, innerW int) string {
	nowMs := time.Now().UnixMilli()
	var sb strings.Builder
	indent := "    "
	sb.WriteString(indent)
	sb.WriteString(dimStyle.Render(ownerLine(a)))
	sb.WriteString("\n")
	for _, r := range usageRows(a) {
		sb.WriteString(indent)
		sb.WriteString(fmt.Sprintf("%-10s", r.Label))
		sb.WriteString(renderUsageWindow(r.Win))
		sb.WriteString("\n")
	}
	sb.WriteString(indent)
	if hasOAuthToken(a) {
		sb.WriteString(dimStyle.Render("token "))
		sb.WriteString(dimStyle.Render(humanRemaining(a.ExpiresAt, nowMs)))
		sb.WriteString(dimStyle.Render(" · "))
	}
	sb.WriteString(dimStyle.Render("last used "))
	sb.WriteString(dimStyle.Render(humanMillis(a.LastUsedAt)))
	_ = innerW
	return sb.String()
}

func (m *model) detailTitle() string {
	if a, ok := m.selected(); ok {
		return a.Name
	}
	return "Detail"
}

// renderDetailBody is for the wide-mode right pane.
func (m *model) renderDetailBody(innerW int) string {
	a, ok := m.selected()
	if !ok {
		return dimStyle.Render("(no accounts — press 'a' to add)")
	}
	nowMs := time.Now().UnixMilli()
	var sb strings.Builder

	owner := ownerLine(a)
	if owner != "" {
		sb.WriteString(dimStyle.Render(owner))
		sb.WriteString("\n")
	}
	if a.OrganizationName != "" {
		sb.WriteString(dimStyle.Render(a.OrganizationName))
		sb.WriteString("\n")
	}
	sb.WriteString("\n")

	// OpenRouter has no subscription windows at all, so the panel is omitted
	// rather than drawn empty — three "no data" bars read as a broken poller.
	if rows := usageRows(a); len(rows) > 0 {
		lines := make([]string, 0, len(rows))
		for _, r := range rows {
			lines = append(lines, fmt.Sprintf("%-10s", r.Label)+renderUsageWindow(r.Win))
		}
		usageW := innerW
		if usageW > 44 {
			usageW = 44
		}
		sb.WriteString(panel("Usage", strings.Join(lines, "\n"), usageW))
		sb.WriteString("\n\n")
	}

	dl := []struct {
		k, v string
	}{
		{"Status", statusDot(a, nowMs) + " " + statusLabel(a, nowMs)},
		{"Last used", humanMillis(a.LastUsedAt)},
	}
	// Both are Claude-shaped: an API-key account has no token lifetime, and
	// nothing polls a usage window it doesn't have.
	if hasOAuthToken(a) {
		dl = append(dl, struct{ k, v string }{"Token", humanRemaining(a.ExpiresAt, nowMs)})
	}
	if len(usageRows(a)) > 0 {
		dl = append(dl, struct{ k, v string }{"Updated", usageFetchedLabel(a.UsageFetchedAt, nowMs)})
	}
	for _, row := range dl {
		sb.WriteString(dimStyle.Render(fmt.Sprintf("%-11s ", row.k)))
		sb.WriteString(row.v)
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

func (m *model) renderStatusLine() string {
	switch {
	case m.pendingOp != "":
		return m.spinner.View() + " " + dimStyle.Render(m.pendingOp)
	case m.statusErr != "":
		return errStyle.Render("✗ " + m.statusErr)
	case m.statusMsg != "":
		return okStyle.Render("✓ " + m.statusMsg)
	case m.fatalErr != "":
		return errStyle.Render("✗ " + m.fatalErr)
	default:
		return " "
	}
}

// footerChipDef pairs the chip's display key/label with the runtime key it
// emits when clicked. emit == "" means the chip is informational only and
// does not respond to mouse clicks (e.g. "↑↓ move").
type footerChipDef struct {
	key, label, emit string
}

// footerChipDefs lists the Accounts-page-specific shortcuts. Global keys
// (1-4 page switch, ? help, q quit) live in the bottom statusbar so every
// tab shares them — including this Accounts page, where they used to be
// duplicated as chips.
//
// Admin-write chips (add/pause-resume/delete) are filtered out in agent
// mode where the vault enforces a lease-only API surface and those writes
// would 405 anyway; better to hide the affordance than to dangle it.
func (m *model) footerChipDefs() []footerChipDef {
	defs := []footerChipDef{
		{"↑↓", "move", ""},
		{"u", "use now", "u"},
	}
	if !m.disableAdminActions {
		defs = append(defs, footerChipDef{"a", "add", "a"})
	}
	defs = append(defs, footerChipDef{"r", "refresh", "r"})
	if !m.disableAdminActions {
		defs = append(defs,
			footerChipDef{"p", "pause/resume", "p"},
			footerChipDef{"x", "delete", "x"},
		)
	}
	defs = append(defs, footerChipDef{"R", "reload", "R"})
	return defs
}

func (m *model) footerChipsRendered() []string {
	defs := m.footerChipDefs()
	out := make([]string, len(defs))
	for i, d := range defs {
		out[i] = keyChip(d.key, d.label)
	}
	return out
}

func (m *model) renderFooter() string {
	chips := m.footerChipsRendered()
	row := keyChipRow(chips...)
	if lipgloss.Width(row) <= m.width {
		return row
	}
	mid := (len(chips) + 1) / 2
	return keyChipRow(chips[:mid]...) + "\n" + keyChipRow(chips[mid:]...)
}

func (m *model) footerHeight() int {
	row := keyChipRow(m.footerChipsRendered()...)
	if lipgloss.Width(row) <= m.width {
		return 1
	}
	return 2
}

// ============================================================================
// Small helpers
// ============================================================================

func renderUsageWindow(w *UsageWindow) string {
	if w == nil {
		return progressBarEmpty(progressBarDefaultWidth)
	}
	return progressBar(w.Utilization, progressBarDefaultWidth)
}

func ownerLine(a Account) string {
	switch {
	case a.FullName != "" && a.Email != "":
		return a.FullName + " · " + a.Email
	case a.Email != "":
		return a.Email
	case a.FullName != "":
		return a.FullName
	default:
		return ""
	}
}

func humanRemaining(targetMs, nowMs int64) string {
	d := time.Duration(targetMs-nowMs) * time.Millisecond
	if d <= 0 {
		return "expired"
	}
	d = d.Round(time.Second)
	if d < time.Hour {
		return fmt.Sprintf("expires in %dm", int(d.Minutes()))
	}
	h := int(d.Hours())
	mins := int(d.Minutes()) - h*60
	return fmt.Sprintf("expires in %dh %dm", h, mins)
}

func usageFetchedLabel(ms, nowMs int64) string {
	if ms == 0 {
		return "pending"
	}
	d := time.Duration(nowMs-ms) * time.Millisecond
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%s ago", humanDuration(d))
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func joinSections(parts ...string) string {
	out := make([]string, 0, len(parts)*2)
	for i, p := range parts {
		if i > 0 {
			out = append(out, "")
		}
		out = append(out, p)
	}
	return strings.Join(out, "\n")
}

// padBodyToHeight appends blank lines so a panel body fills target rows. Used
// to push the bottom border down to the available height of the screen.
func padBodyToHeight(body string, target int) string {
	if target <= 0 {
		return body
	}
	cur := 0
	if body != "" {
		cur = strings.Count(body, "\n") + 1
	}
	if cur >= target {
		return body
	}
	return body + strings.Repeat("\n", target-cur)
}

// listRowAt maps a click coordinate to an account index. Returns false if the
// click is outside the list rows (panel borders, padding, header/footer area).
//
// The wide layout draws account[i] at y = 3 + i (header(1) + blank(1) +
// panel top(1)). The regular layout is the same except the cursor row has 5
// inline detail rows tucked under it, so accounts after the cursor get pushed
// down by 5. The narrow layout is intentionally left unmapped — small windows
// usually have other concerns and one line of slop isn't worth the math.
func (m *model) listRowAt(x, y int) (int, bool) {
	_ = x
	if len(m.accounts) == 0 {
		return 0, false
	}
	switch computeLayout(m.width) {
	case layoutWide:
		idx := y - 3
		if idx < 0 || idx >= len(m.accounts) {
			return 0, false
		}
		return idx, true
	case layoutRegular:
		rel := y - 3
		if rel < 0 {
			return 0, false
		}
		for i := range m.accounts {
			if rel == 0 {
				return i, true
			}
			rel--
			if i == m.cursor {
				if rel < inlineDetailRows {
					return i, true
				}
				rel -= inlineDetailRows
			}
		}
		return 0, false
	default:
		return 0, false
	}
}

// inlineDetailRows is the number of rows renderInlineDetail produces below
// the cursor row in the regular layout (owner + 5h + 7d Opus + 7d Sonnet +
// token meta).
const inlineDetailRows = 5

// footerChipAt maps a click coordinate to the key the chip should emit.
// Returns ("", false) when the click is outside the footer or lands on an
// informational chip (one with emit == "").
func (m *model) footerChipAt(x, y int) (string, bool) {
	defs := m.footerChipDefs()
	rendered := m.footerChipsRendered()
	rowH := 1
	if lipgloss.Width(keyChipRow(rendered...)) > m.width {
		rowH = 2
	}
	footerY := m.height - rowH
	if y < footerY || y >= footerY+rowH {
		return "", false
	}

	var rowChips []string
	var rowDefs []footerChipDef
	if rowH == 1 {
		rowChips = rendered
		rowDefs = defs
	} else {
		mid := (len(rendered) + 1) / 2
		if y == footerY {
			rowChips = rendered[:mid]
			rowDefs = defs[:mid]
		} else {
			rowChips = rendered[mid:]
			rowDefs = defs[mid:]
		}
	}

	cur := 0
	for i, c := range rowChips {
		w := lipgloss.Width(c)
		if x >= cur && x < cur+w {
			if rowDefs[i].emit == "" {
				return "", false
			}
			return rowDefs[i].emit, true
		}
		cur += w + 3 // keyChipRow joins chips with three spaces
	}
	return "", false
}

// computeBodyHeight returns the number of body rows a panel can occupy so the
// list view fills the screen exactly: header(2) + blank + panel + blank +
// statusLine + blank + footer == m.height. panel rows = bodyRows + 2 borders.
func (m *model) computeBodyHeight() int {
	footerH := m.footerHeight()
	// 6 = header(2 — title row + filter chips) + 3 blank separators + statusLine(1).
	// Subtract 2 for the panel's top + bottom border.
	bodyH := m.height - 6 - footerH - 2
	if bodyH < 4 {
		bodyH = 4
	}
	return bodyH
}
