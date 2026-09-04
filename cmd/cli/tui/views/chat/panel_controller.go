package chat

import (
	tea "charm.land/bubbletea/v2"
)

// handlePanelKey routes a keypress while the command panel is open. It returns
// the command to emit (nil for none) and whether the key was consumed. All
// panel state lives on the model; this method exists so the panel's key
// handling is kept out of the main Update switch, which is already large.
func (m *Model) handlePanelKey(k tea.KeyPressMsg) (cmd tea.Cmd, handled bool) {
	// Help panel is dismiss-only: any key closes it.
	if m.panelMode == panelModeHelp {
		m.panelOpen = false
		return nil, true
	}

	// Plugins list is read-only: ↑/↓ navigate, enter/esc close.
	if m.panelMode == panelModePlugins {
		switch k.String() {
		case "ctrl+c", "esc", "enter":
			m.panelOpen = false
			m.panelMode = panelModeCommand
		case "up":
			if m.panelIdx > 0 {
				m.panelIdx--
			}
		case "down":
			if m.panelIdx < len(m.pluginItems)-1 {
				m.panelIdx++
			}
		}
		return nil, true
	}

	// Edit picker: ↑/↓ choose a past user message, enter copies it back into
	// the input for editing.
	if m.panelMode == panelModeEdit {
		switch k.String() {
		case "ctrl+c", "esc":
			m.panelOpen = false
			m.panelMode = panelModeCommand
		case "up":
			if m.panelIdx > 0 {
				m.panelIdx--
			}
		case "down":
			if m.panelIdx < len(m.editableMessages())-1 {
				m.panelIdx++
			}
		case "enter":
			_, cmd = m.execEditSelection()
		}
		return cmd, true
	}

	// Search overlay: typing extends the query, ↑/↓ pick a match, enter jumps
	// the viewport to it.
	if m.panelMode == panelModeSearch {
		switch k.String() {
		case "ctrl+c", "esc":
			m.panelOpen = false
			m.panelFilter = ""
		case "up":
			if len(m.searchResults) > 0 && m.panelIdx > 0 {
				m.panelIdx--
			}
		case "down":
			if m.panelIdx < len(m.searchResults)-1 {
				m.panelIdx++
			}
		case "enter":
			_, cmd = m.execSearchSelection()
		case "backspace":
			if m.panelFilter != "" {
				r := []rune(m.panelFilter)
				m.panelFilter = string(r[:len(r)-1])
				m.panelIdx = 0
				m.refreshSearch()
			}
		case "space":
			m.panelFilter += " "
			m.panelIdx = 0
			m.refreshSearch()
		default:
			if s := k.String(); len(s) == 1 && s[0] > ' ' && s[0] < 0x7f {
				m.panelFilter += s
				m.panelIdx = 0
				m.refreshSearch()
			}
		}
		return cmd, true
	}

	// Command palette / sessions / models: shared navigation, plus filter
	// input for the command palette.
	switch k.String() {
	case "ctrl+c", "esc":
		m.panelOpen = false
		m.panelMode = panelModeCommand
		m.panelFilter = ""
	case "up":
		if n := m.panelItemCount(); n > 0 && m.panelIdx > 0 {
			m.panelIdx--
		}
	case "down":
		if n := m.panelItemCount(); n > 0 && m.panelIdx < n-1 {
			m.panelIdx++
		}
	case "enter":
		_, cmd = m.panelExecute()
	case "backspace":
		if m.panelMode == panelModeCommand && m.panelFilter != "" {
			m.panelFilter = m.panelFilter[:len(m.panelFilter)-1]
			m.panelIdx = 0
		}
	case "space":
		if m.panelMode != panelModeCommand {
			return nil, true
		}
		// Empty filter: space toggles the selected command.
		if m.panelFilter == "" {
			_, cmd = m.panelToggleSelected()
			return cmd, true
		}
		m.panelFilter += " "
		m.panelIdx = 0
	default:
		if m.panelMode != panelModeCommand {
			return nil, true
		}
		// Printable rune → extend the filter.
		if s := k.String(); len(s) == 1 && s[0] > ' ' && s[0] < 0x7f {
			m.panelFilter += s
			m.panelIdx = 0
		}
	}
	return cmd, true
}

// handlePermissionKey routes a keypress while a tool-call permission dialog is
// open. The user's selection is written back through the reply channel; a
// non-nil command (tea.Quit) is returned for Ctrl+C.
func (m *Model) handlePermissionKey(k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "ctrl+c":
		return tea.Quit
	case "esc":
		m.respondPermission(-1)
	case "up", "left":
		if m.permissionSelectedIdx > 0 {
			m.permissionSelectedIdx--
		}
	case "down", "right":
		if m.permissionReq != nil && m.permissionSelectedIdx < len(m.permissionReq.Options)-1 {
			m.permissionSelectedIdx++
		}
	case "enter":
		m.respondPermission(m.permissionSelectedIdx)
	}
	return nil
}
