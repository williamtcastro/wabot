package tui

import (
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/williamtcastro/wame/internal/termui"
)

func (m *model) key(k tea.KeyMsg) tea.Cmd {
	if m.searching {
		return m.resultsKey(k)
	}
	if m.focus == focusInput && strings.HasPrefix(m.input.Value(), "/") {
		switch k.String() {
		case "tab":
			if len(m.input.MatchedSuggestions()) == 0 {
				return nil
			}
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(k)
			return tea.Batch(cmd, m.suggest())
		case "up", "down":
			if len(m.input.MatchedSuggestions()) > 1 {
				var cmd tea.Cmd
				m.input, cmd = m.input.Update(k)
				return cmd
			}
		case "ctrl+j", "ctrl+k":
			if len(m.input.MatchedSuggestions()) > 0 {
				dir := tea.KeyMsg{Type: tea.KeyDown}
				if k.String() == "ctrl+k" {
					dir.Type = tea.KeyUp
				}
				var cmd tea.Cmd
				m.input, cmd = m.input.Update(dir)
				return cmd
			}
		}
	}
	switch k.String() {
	case "ctrl+c":
		return tea.Quit
	case "ctrl+j", "ctrl+k":
		step := 1
		if k.String() == "ctrl+k" {
			step = -1
		}
		from := m.cursor
		if m.focus != focusChats {
			from = chatIndex(m.filteredChats(), m.active) + 1
		}
		m.cursor = clamp(from+step, 0, len(m.filteredChats()))
		return m.open(m.cursorJID())
	case "tab", "shift+tab":
		step := 1
		if k.String() == "shift+tab" {
			step = 2
		}
		next := (m.focus + focus(step)) % 3
		if next == focusMessages && len(m.visible) == 0 {
			next = (next + focus(step)) % 3
		}
		m.setFocus(next)
		return nil
	case "ctrl+y":
		return m.copyNumber()
	case "ctrl+d":
		m.vp.HalfPageDown()
		return nil
	case "ctrl+u":
		m.vp.HalfPageUp()
		return nil
	case "pgup", "pgdown":
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(k)
		return cmd
	}
	switch m.focus {
	case focusChats:
		return m.chatsKey(k)
	case focusMessages:
		return m.messagesKey(k)
	}
	return m.inputKey(k)
}

func (m *model) setFocus(f focus) {
	if f == focusMessages && len(m.visible) == 0 {
		f = focusInput
	}
	m.focus = f
	m.pendingDelete = ""
	if f == focusInput {
		m.input.Focus()
		m.sel = -1
	} else {
		m.input.Blur()
	}
	if f == focusMessages && m.sel < 0 {
		m.sel = len(m.visible) - 1
	}
	if f == focusChats {
		m.syncCursor()
	}
	m.render(f == focusInput)
}

func (m *model) inputKey(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "enter":
		line := strings.TrimSpace(m.input.Value())
		m.input.SetValue("")
		return tea.Batch(m.submit(line), m.suggest())
	case "esc":
		switch {
		case m.panelTitle != "":
			m.panelTitle = ""
		case m.replyTo != nil || m.editing != nil:
			if m.editing != nil {
				m.input.SetValue("")
			}
			m.replyTo, m.editing = nil, nil
		}
		m.layout()
		m.render(true)
		return nil
	case "up", "down":
		if m.panelTitle != "" {
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(k)
			return cmd
		}
		if k.String() == "up" && m.input.Value() == "" && m.active != "" {
			m.setFocus(focusMessages)
		}
		return nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return tea.Batch(cmd, m.suggest())
}

func (m *model) chatsKey(k tea.KeyMsg) tea.Cmd {
	switch k.Type {
	case tea.KeyUp:
		m.cursor = clamp(m.cursor-1, 0, len(m.filteredChats()))
	case tea.KeyDown:
		m.cursor = clamp(m.cursor+1, 0, len(m.filteredChats()))
	case tea.KeyEnter:
		jid := m.cursorJID()
		m.filter = ""
		m.setFocus(focusInput)
		return m.open(jid)
	case tea.KeyEsc:
		if m.filter != "" {
			m.filter = ""
			m.cursor = chatIndex(m.chats, m.active) + 1
		} else {
			m.setFocus(focusInput)
		}
	case tea.KeyBackspace:
		if m.filter != "" {
			m.filter = string([]rune(m.filter)[:len([]rune(m.filter))-1])
			m.cursor = min(1, len(m.filteredChats()))
		}
	case tea.KeyRunes, tea.KeySpace:
		m.filter += string(k.Runes)
		m.cursor = min(1, len(m.filteredChats()))
	}
	return nil
}

func (m *model) messagesKey(k tea.KeyMsg) tea.Cmd {
	if m.sel < 0 || m.sel >= len(m.visible) {
		m.setFocus(focusInput)
		return nil
	}
	target := m.visible[m.sel]
	key := k.String()
	if key != "d" {
		m.pendingDelete = ""
	}
	switch key {
	case "up", "k":
		m.sel = max(0, m.sel-1)
	case "down", "j":
		if m.sel == len(m.visible)-1 {
			m.setFocus(focusInput)
			return nil
		}
		m.sel++
	case "g":
		m.sel = 0
	case "G":
		m.sel = len(m.visible) - 1
	case "esc":
		m.setFocus(focusInput)
		return nil
	case "r", "enter":
		if target.Deleted {
			return m.setNotice("can't reply to a deleted message", true)
		}
		m.replyTo, m.editing = &target, nil
		m.layout()
		m.setFocus(focusInput)
		return nil
	case "e":
		if !target.FromMe || target.Type != "text" || target.Deleted {
			return m.setNotice("you can only edit your own text messages", true)
		}
		m.editing, m.replyTo = &target, nil
		m.layout()
		m.setFocus(focusInput)
		m.input.SetValue(target.Text)
		m.input.CursorEnd()
		return nil
	case "d":
		if target.Deleted {
			return nil
		}
		if m.pendingDelete != target.ID {
			m.pendingDelete = target.ID
			return m.setNotice("press d again to delete this message for everyone", false)
		}
		m.pendingDelete = ""
		chat := m.active
		return m.do(func(ctx context.Context) (string, error) {
			_, err := m.bot.Delete(ctx, actor, chat, target.ID)
			return "deleted", err
		})
	case "s":
		if target.Media == nil {
			return m.setNotice("this message has no media", true)
		}
		return m.do(func(ctx context.Context) (string, error) { return m.save(ctx, target) })
	case "0", "1", "2", "3", "4", "5", "6":
		emoji := ""
		if key != "0" {
			emoji = quickReacts[key[0]-'1']
		}
		chat := m.active
		return m.do(func(ctx context.Context) (string, error) {
			_, err := m.bot.React(ctx, actor, chat, target.ID, emoji)
			return "", err
		})
	default:
		if k.Type == tea.KeyRunes {
			m.setFocus(focusInput)
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(k)
			return cmd
		}
	}
	m.render(false)
	return nil
}

func (m *model) resultsKey(k tea.KeyMsg) tea.Cmd {
	if len(m.results) == 0 && k.String() != "esc" && k.String() != "q" && k.String() != "ctrl+c" {
		return nil
	}
	key := k.String()
	switch key {
	case "ctrl+j":
		key = "j"
	case "ctrl+k":
		key = "k"
	}
	if sel, ok := termui.ListMove(key, m.resultSel, len(m.results), m.vp.Height/2); ok {
		m.resultSel = sel
		m.render(false)
		return nil
	}
	switch key {
	case "ctrl+c":
		return tea.Quit
	case "enter":
		if m.resultSel < len(m.results) {
			hit := m.results[m.resultSel]
			m.searching = false
			return m.openAt(hit.Chat, hit.Seq)
		}
	case "esc", "q":
		m.searching = false
		m.render(true)
		return nil
	}
	m.render(false)
	return nil
}
