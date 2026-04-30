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

	var detailBody string
	if len(m.accounts) == 0 {
		detailBody = renderEmptyState(detailW - 4)
	} else {
		detailBody = m.renderDetailBody(detailW - 4)
	}

	listPanel := panel("ACCOUNTS", m.renderAccountList(listW-4), listW)
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
		for i, a := range m.accounts {
			rail := accentRail(i == m.cursor)
			dot := statusDot(a, nowMs)
			line := rail + dot + " " + truncate(a.Name, m.width-6)
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
	parts := []string{titleStyle.Render("foxy-switcher"), m.renderCredText()}
	parts = append(parts, dimStyle.Render(fmt.Sprintf("%d account(s)", len(m.accounts))))
	if !m.lastRefresh.IsZero() {
		parts = append(parts, dimStyle.Render("refreshed "+humanAge(m.lastRefresh)))
	}
	return strings.Join(parts, "  ")
}

func (m *model) renderCredText() string {
	switch {
	case m.cred.ManagedAccountID != 0:
		return okStyle.Render(fmt.Sprintf("● managing acct #%d", m.cred.ManagedAccountID))
	case m.cred.NativeBackupPresent:
		return okStyle.Render("● idle (native restored)")
	default:
		return warnStyle.Render("○ idle")
	}
}

// renderAccountList lays out the wide-mode account list; one row per account.
//
//	▍● alice              [max20]
func (m *model) renderAccountList(innerW int) string {
	if len(m.accounts) == 0 {
		return dimStyle.Render("(no accounts — press 'a' to add)")
	}
	nowMs := time.Now().UnixMilli()
	var lines []string
	for i, a := range m.accounts {
		rail := accentRail(i == m.cursor)
		dot := statusDot(a, nowMs)
		badge := planBadge(a.Plan)
		// Visible widths: rail(1) + dot(1) + space(1) + name(?) + … + badge(W)
		fixed := 1 + 1 + 1 + lipgloss.Width(badge)
		nameMax := innerW - fixed - 1 // 1 space before badge
		if nameMax < 4 {
			nameMax = 4
		}
		name := truncate(a.Name, nameMax)
		nameW := lipgloss.Width(name)
		gap := innerW - fixed - nameW
		if gap < 1 {
			gap = 1
		}
		row := rail + dot + " " + name + strings.Repeat(" ", gap) + badge
		if i == m.cursor {
			row = selectedStyle.Width(innerW).Render(row)
		}
		lines = append(lines, row)
	}
	return strings.Join(lines, "\n")
}

// renderAccountListInline expands the selected row in place with usage bars
// and meta. Used by the regular layout.
func (m *model) renderAccountListInline(innerW int) string {
	if len(m.accounts) == 0 {
		return dimStyle.Render("(no accounts — press 'a' to add)")
	}
	nowMs := time.Now().UnixMilli()
	var lines []string
	for i, a := range m.accounts {
		rail := accentRail(i == m.cursor)
		dot := statusDot(a, nowMs)
		badge := planBadge(a.Plan)
		fixed := 1 + 1 + 1 + lipgloss.Width(badge)
		nameMax := innerW - fixed - 1
		if nameMax < 4 {
			nameMax = 4
		}
		name := truncate(a.Name, nameMax)
		nameW := lipgloss.Width(name)
		gap := innerW - fixed - nameW
		if gap < 1 {
			gap = 1
		}
		row := rail + dot + " " + name + strings.Repeat(" ", gap) + badge
		if i == m.cursor {
			row = selectedStyle.Width(innerW).Render(row)
		}
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
	sb.WriteString(indent)
	sb.WriteString("5h        ")
	sb.WriteString(renderUsageWindow(a.FiveHour))
	sb.WriteString("\n")
	sb.WriteString(indent)
	sb.WriteString("7d Opus   ")
	sb.WriteString(renderUsageWindow(a.SevenDay))
	sb.WriteString("\n")
	sb.WriteString(indent)
	sb.WriteString("7d Sonnet ")
	sb.WriteString(renderUsageWindow(a.SevenDaySonnet))
	sb.WriteString("\n")
	sb.WriteString(indent)
	sb.WriteString(dimStyle.Render("token "))
	sb.WriteString(dimStyle.Render(humanRemaining(a.ExpiresAt, nowMs)))
	sb.WriteString(dimStyle.Render(" · last used "))
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

	usageBody := strings.Join([]string{
		"5h        " + renderUsageWindow(a.FiveHour),
		"7d Opus   " + renderUsageWindow(a.SevenDay),
		"7d Sonnet " + renderUsageWindow(a.SevenDaySonnet),
	}, "\n")
	usageW := innerW
	if usageW > 44 {
		usageW = 44
	}
	sb.WriteString(panel("Usage", usageBody, usageW))
	sb.WriteString("\n\n")

	dl := []struct {
		k, v string
	}{
		{"Status", statusDot(a, nowMs) + " " + statusLabel(a, nowMs)},
		{"Last used", humanMillis(a.LastUsedAt)},
		{"Token", humanRemaining(a.ExpiresAt, nowMs)},
		{"Updated", usageFetchedLabel(a.UsageFetchedAt, nowMs)},
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

func (m *model) renderFooter() string {
	chips := []string{
		keyChip("↑↓", "move"),
		keyChip("a", "add"),
		keyChip("r", "refresh"),
		keyChip("e", "enable"),
		keyChip("d", "disable"),
		keyChip("c", "cooldown"),
		keyChip("x", "delete"),
		keyChip("R", "reload"),
		keyChip("q", "quit"),
	}
	row := keyChipRow(chips...)
	if lipgloss.Width(row) <= m.width {
		return row
	}
	mid := (len(chips) + 1) / 2
	return keyChipRow(chips[:mid]...) + "\n" + keyChipRow(chips[mid:]...)
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
