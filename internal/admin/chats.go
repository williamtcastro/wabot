package admin

import (
	"context"
	"fmt"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/williamtcastro/wame/internal/store"
)

const chatListWidth = 34

func (m *model) loadChats() tea.Cmd {
	return m.call(func(ctx context.Context, c *Client) tea.Msg {
		chats, err := c.Chats(ctx, chatsLimit)
		return chatsMsg{chats, err}
	})
}

func (m *model) loadMessages(chat string) tea.Cmd {
	if chat == "" {
		return nil
	}
	return m.call(func(ctx context.Context, c *Client) tea.Msg {
		msgs, err := c.Messages(ctx, chat, "", messagesLimit)
		return messagesMsg{chat: chat, msgs: msgs, err: err}
	})
}

// applyChats keeps the same chat selected when the list reorders, and opens it if it changed.
func (m *model) applyChats(msg chatsMsg) tea.Cmd {
	m.chatsErr = msg.err
	if msg.err != nil {
		return nil
	}
	selected := m.selectedChat()
	m.chats = msg.chats
	if i := slices.IndexFunc(m.chats, func(c store.Chat) bool { return c.JID == selected }); i >= 0 {
		m.chatSel = i
	}
	m.chatSel = clamp(m.chatSel, len(m.chats))
	if chat := m.selectedChat(); chat != m.msgsChat && m.tab == tabChats {
		return m.loadMessages(chat)
	}
	return nil
}

func (m *model) applyMessages(msg messagesMsg) {
	if msg.query != "" {
		if msg.query != m.search {
			return
		}
		m.notice = ""
		m.searchHits, m.msgsErr = msg.msgs, msg.err
		return
	}
	if msg.chat != m.selectedChat() {
		return
	}
	m.msgsChat, m.msgs, m.msgsErr = msg.chat, msg.msgs, msg.err
	slices.Reverse(m.msgs)
}

func (m *model) selectedChat() string {
	if m.chatSel < len(m.chats) {
		return m.chats[m.chatSel].JID
	}
	return ""
}

// onMessage folds a streamed message into the Chats tab.
func (m *model) onMessage(msg store.Message) tea.Cmd {
	if msg.Chat == m.msgsChat && !slices.ContainsFunc(m.msgs, func(x store.Message) bool { return x.Seq == msg.Seq }) {
		m.msgs = append(m.msgs, msg)
		if len(m.msgs) > messagesLimit {
			m.msgs = m.msgs[len(m.msgs)-messagesLimit:]
		}
	}
	if m.tab == tabChats {
		return m.loadChats()
	}
	return nil
}

func (m *model) chatsKey(k string) tea.Cmd {
	move := func(d int) tea.Cmd {
		m.chatSel = clamp(m.chatSel+d, len(m.chats))
		if chat := m.selectedChat(); chat != m.msgsChat {
			return m.loadMessages(chat)
		}
		return nil
	}
	switch k {
	case "j", "down":
		return move(1)
	case "k", "up":
		return move(-1)
	case "g", "home":
		return move(-len(m.chats))
	case "G", "end":
		return move(len(m.chats))
	case "i", "enter":
		if m.msgsChat == "" {
			return nil
		}
		m.startPrompt(modeCompose, "message to "+m.chatName(m.msgsChat)+" (sends as the account)")
	case "/":
		m.startPrompt(modeSearch, "search every chat")
		m.prompt.SetValue(m.search)
		m.prompt.CursorEnd()
	case "esc":
		m.search, m.searchHits = "", nil
	}
	return nil
}

func (m *model) chatName(jid string) string {
	for _, c := range m.chats {
		if c.JID == jid && c.Name != "" {
			return safe(c.Name)
		}
	}
	return displayJID(jid)
}

func (m *model) viewChats() string {
	if m.chatsErr != nil {
		return indent(card("Chats", min(m.width-2, 90), styleErr.Render(m.chatsErr.Error())), " ")
	}
	if len(m.chats) == 0 {
		return indent(card("Chats", min(m.width-2, 70), styleDim.Render("No chats stored on the server yet.")), " ")
	}
	h := m.bodyHeight()
	listW := chatListWidth
	var list []string
	start, end := window(m.chatSel, len(m.chats), h)
	for i := start; i < end; i++ {
		c := m.chats[i]
		badge := "   "
		if c.Unread > 0 {
			badge = styleAccent.Render(fmt.Sprintf("%2d ", min(c.Unread, 99)))
		}
		name := fit(safe(cmpOr(c.Name, displayJID(c.JID))), listW-11)
		line := badge + pad(name, listW-11) + " " + styleDim.Render(pad(when(c.LastAt), 6))
		if i == m.chatSel {
			line = lipgloss.NewStyle().Background(colSel).Width(listW).Render(styleAccent.Render("▌") + line)
		} else {
			line = " " + line
		}
		list = append(list, line)
	}
	left := lipgloss.NewStyle().Width(listW).Height(h).Render(strings.Join(list, "\n"))
	sep := styleFaint.Render(strings.Repeat("│\n", h-1) + "│")
	paneW := max(m.width-listW-4, 20)
	var right string
	if m.search != "" {
		right = m.viewSearch(paneW, h)
	} else {
		right = m.viewMessages(paneW, h)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, left, " ", sep, " ", right)
}

func (m *model) viewMessages(w, h int) string {
	chat := m.selectedChat()
	title := styleBold.Render(m.chatName(chat)) + "  " + styleDim.Render(chat)
	if m.msgsErr != nil {
		return title + "\n\n" + styleErr.Render(m.msgsErr.Error())
	}
	if m.msgsChat != chat {
		return title + "\n\n" + styleDim.Render("loading…")
	}
	var lines []string
	for _, msg := range m.msgs {
		lines = append(lines, m.messageLines(msg, w)...)
	}
	room := h - 3
	if len(lines) > room {
		lines = lines[len(lines)-room:]
	}
	if len(lines) == 0 {
		lines = []string{styleDim.Render("No messages stored for this chat.")}
	}
	foot := styleFaint.Render("Reading here doesn't mark anything as read on the phone.")
	return title + "\n" + styleFaint.Render(strings.Repeat("─", w)) + "\n" + strings.Join(lines, "\n") + strings.Repeat("\n", max(room-len(lines), 0)) + "\n" + foot
}

func (m *model) messageLines(msg store.Message, w int) []string {
	who := styleAccent.Render("me")
	if !msg.FromMe {
		who = lipgloss.NewStyle().Foreground(colInfo).Render(fit(safe(cmpOr(msg.PushName, displayJID(msg.Sender))), 18))
	}
	prefix := styleDim.Render(msg.Timestamp.Format("15:04")) + " " + who + " "
	body := messageText(msg)
	indentW := ansi.StringWidth(prefix)
	wrapped := strings.Split(lipgloss.NewStyle().Width(max(w-indentW, 10)).Render(body), "\n")
	out := []string{prefix + wrapped[0]}
	for _, l := range wrapped[1:] {
		out = append(out, strings.Repeat(" ", indentW)+l)
	}
	return out
}

func messageText(msg store.Message) string {
	text := safe(msg.Text)
	if msg.Type != "text" {
		label := "[" + msg.Type
		if msg.Media != nil && msg.Media.FileName != "" {
			label += " " + safe(msg.Media.FileName)
		}
		label += "]"
		text = strings.TrimSpace(styleDim.Render(label) + " " + text)
	}
	switch {
	case msg.Deleted:
		return styleFaint.Render("message deleted")
	case msg.Edited:
		text += styleFaint.Render(" (edited)")
	}
	return text
}

func (m *model) viewSearch(w, h int) string {
	title := styleBold.Render("Search: "+safe(m.search)) + "  " + styleDim.Render(plural(len(m.searchHits), "match")+" · esc clears")
	if m.msgsErr != nil {
		return title + "\n\n" + styleErr.Render(m.msgsErr.Error())
	}
	lines := []string{title, styleFaint.Render(strings.Repeat("─", w))}
	for _, msg := range m.searchHits {
		if len(lines) >= h {
			break
		}
		head := styleDim.Render(when(msg.Timestamp)) + " " + lipgloss.NewStyle().Foreground(colInfo).Render(fit(m.chatName(msg.Chat), 20)) + " "
		lines = append(lines, ansi.Truncate(head+messageText(msg), w, "…"))
	}
	return strings.Join(lines, "\n")
}

// safe drops control characters and bidi overrides from text other people wrote, so it can't move
// the cursor, recolor the screen or reorder what's around it.
func safe(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return ' '
		case r < 0x20, r == 0x7f, r >= 0x80 && r < 0xa0:
			return -1
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069, r == 0x200e, r == 0x200f:
			return -1
		}
		return r
	}, s)
}
