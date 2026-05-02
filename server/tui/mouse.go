package tui

import (
	tea "github.com/charmbracelet/bubbletea"
)

// handleMouse routes mouse events. Only modeList consumes them; modal screens
// (add paste / cooldown / confirm) ignore mouse for now.
func (m *model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.mode != modeList {
		return m, nil
	}

	switch msg.Button {
	case tea.MouseButtonWheelUp:
		if msg.Action != tea.MouseActionPress {
			return m, nil
		}
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil
	case tea.MouseButtonWheelDown:
		if msg.Action != tea.MouseActionPress {
			return m, nil
		}
		if m.cursor < len(m.accounts)-1 {
			m.cursor++
		}
		return m, nil
	}

	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return m, nil
	}

	if key, ok := m.footerChipAt(msg.X, msg.Y); ok {
		return m.handleListKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
	}
	if idx, ok := m.listRowAt(msg.X, msg.Y); ok {
		m.cursor = idx
	}
	return m, nil
}
