package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mdp/qrterminal/v3"
)

const sidebarWidth = 32

var (
	styleHeader  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("28")).Padding(0, 1)
	styleMe      = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleThem    = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	styleDim     = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	styleErr     = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	styleBadge   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("42"))
	styleCursor  = lipgloss.NewStyle().Background(lipgloss.Color("237"))
	styleSidebar = lipgloss.NewStyle().Width(sidebarWidth).BorderStyle(lipgloss.NormalBorder()).BorderRight(true).BorderForeground(lipgloss.Color("238"))
)

const tuiHelp = `Keys:
  Tab                      switch between the chat list and the input
  ↑/↓ + Enter              (in the chat list) pick and open a chat
  Ctrl+N / Ctrl+P          next / previous chat without leaving the input
  Esc                      back to "All activity"
  PgUp/PgDn, ↑/↓           scroll messages
Commands:
  /contacts [search]       list your contacts, optionally filtered by name/number
  /to <number|jid|name>    open a chat (plain text then goes there)
  /all                     show the live feed of every chat
  /send <number|jid> <msg> send a one-off message
  /file <path> [caption]   send a file/image to the open chat
  /groups                  list your groups (use their JID with /to)
  /pair <phone>            log in with a pairing code instead of QR
  /clear  /help  /quit`

type eventMsg struct{ v any }
type lineMsg string
type chatsMsg []Chat
type openedMsg struct {
	jid, name string
	msgs      []Message
}

type model struct {
	bot    *Bot
	sub    chan any
	vp     viewport.Model
	input  textinput.Model
	status Status
	ready  bool
	height int

	chats       []Chat
	unread      map[string]int
	cursor      int
	sideFocused bool

	active     string
	activeName string
	console    []string
	chatLines  []string
}

func RunTUI(b *Bot) error {
	ch, unsub := b.Subscribe()
	defer unsub()
	in := textinput.New()
	in.Placeholder = "type /help · Tab for the chat list"
	in.Focus()
	m := &model{bot: b, sub: ch, input: in, status: b.Status(), unread: map[string]int{}}
	m.console = []string{styleDim.Render(tuiHelp)}
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

func (m *model) wait() tea.Cmd {
	return func() tea.Msg { return eventMsg{<-m.sub} }
}

func (m *model) loadChats() tea.Cmd {
	return func() tea.Msg {
		chats, err := m.bot.Chats(context.Background(), 200)
		if err != nil {
			return lineMsg(styleErr.Render("load chats: " + err.Error()))
		}
		return chatsMsg(chats)
	}
}

func (m *model) open(jid string) tea.Cmd {
	if jid == "" {
		m.active, m.activeName = "", ""
		m.cursor = 0
		m.refresh()
		return nil
	}
	return func() tea.Msg {
		msgs := m.bot.Messages(jid, 300)
		return openedMsg{jid: jid, name: m.bot.chatName(context.Background(), jid, ""), msgs: msgs}
	}
}

func (m *model) Init() tea.Cmd { return tea.Batch(textinput.Blink, m.wait(), m.loadChats()) }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		w, h := msg.Width-sidebarWidth-1, msg.Height-3
		if !m.ready {
			m.vp = viewport.New(w, h-1)
			m.ready = true
		} else {
			m.vp.Width, m.vp.Height = w, h-1
		}
		m.height = h
		m.input.Width = msg.Width - 4
		m.refresh()
		return m, nil

	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC:
			return m, tea.Quit
		case tea.KeyTab:
			m.sideFocused = !m.sideFocused
			if m.sideFocused {
				m.input.Blur()
			} else {
				m.input.Focus()
			}
			return m, nil
		case tea.KeyEsc:
			m.sideFocused = false
			m.input.Focus()
			return m, m.open("")
		case tea.KeyCtrlN, tea.KeyCtrlP:
			step := 1
			if msg.Type == tea.KeyCtrlP {
				step = -1
			}
			m.moveCursor(step)
			return m, m.open(m.cursorJID())
		case tea.KeyPgUp, tea.KeyPgDown:
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			return m, cmd
		}
		if m.sideFocused {
			switch msg.String() {
			case "up", "k":
				m.moveCursor(-1)
			case "down", "j":
				m.moveCursor(1)
			case "enter":
				m.sideFocused = false
				m.input.Focus()
				return m, m.open(m.cursorJID())
			}
			return m, nil
		}
		switch msg.Type {
		case tea.KeyEnter:
			line := strings.TrimSpace(m.input.Value())
			m.input.SetValue("")
			if line == "/quit" || line == "/q" {
				return m, tea.Quit
			}
			return m, m.command(line)
		case tea.KeyUp, tea.KeyDown:
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			return m, cmd
		}

	case lineMsg:
		m.add(string(msg))
		return m, nil

	case chatsMsg:
		m.chats = msg
		m.syncCursor()
		return m, nil

	case openedMsg:
		m.active, m.activeName = msg.jid, msg.name
		for _, c := range m.chats {
			if c.JID == msg.jid {
				m.activeName = c.Name
			}
		}
		delete(m.unread, msg.jid)
		m.chatLines = m.chatLines[:0]
		for i := len(msg.msgs) - 1; i >= 0; i-- {
			m.chatLines = append(m.chatLines, m.formatChatLine(msg.msgs[i]))
		}
		m.syncCursor()
		m.refresh()
		return m, nil

	case eventMsg:
		cmds := []tea.Cmd{m.wait()}
		switch v := msg.v.(type) {
		case Status:
			m.status = v
			if v.LoggedIn {
				cmds = append(cmds, m.loadChats())
			}
		case ChatsChanged:
			cmds = append(cmds, m.loadChats())
		case Message:
			m.console = append(m.console, m.formatFeedLine(v))
			if v.Chat == m.active {
				m.chatLines = append(m.chatLines, m.formatChatLine(v))
			} else if !v.FromMe {
				m.unread[v.Chat]++
			}
			cmds = append(cmds, m.loadChats())
		}
		m.refresh()
		return m, tea.Batch(cmds...)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *model) cursorJID() string {
	if m.cursor == 0 || m.cursor > len(m.chats) {
		return ""
	}
	return m.chats[m.cursor-1].JID
}

func (m *model) moveCursor(step int) {
	m.cursor = max(0, min(len(m.chats), m.cursor+step))
}

// syncCursor keeps the highlight on the open chat after the list is re-sorted.
func (m *model) syncCursor() {
	if m.sideFocused {
		m.cursor = min(m.cursor, len(m.chats))
		return
	}
	m.cursor = 0
	for i, c := range m.chats {
		if c.JID == m.active {
			m.cursor = i + 1
		}
	}
}

func (m *model) command(line string) tea.Cmd {
	if line == "" {
		return nil
	}
	if !strings.HasPrefix(line, "/") {
		if m.active == "" {
			m.add(styleErr.Render("no chat open: pick one with Tab, or /to <name or number>"))
			return nil
		}
		return m.async(func(ctx context.Context) error {
			_, err := m.bot.SendText(ctx, m.active, line)
			return err
		})
	}
	cmd, rest, _ := strings.Cut(line, " ")
	rest = strings.TrimSpace(rest)
	switch cmd {
	case "/help":
		m.add(styleDim.Render(tuiHelp))
	case "/clear":
		if m.active == "" {
			m.console = nil
		} else {
			m.chatLines = nil
		}
		m.refresh()
	case "/all":
		return m.open("")
	case "/to":
		if _, err := ParseRecipient(rest); err == nil {
			return func() tea.Msg {
				jid, err := m.bot.Resolve(context.Background(), rest)
				if err != nil {
					return lineMsg(styleErr.Render(err.Error()))
				}
				return m.open(jid.String())()
			}
		}
		contacts, err := m.bot.Contacts(context.Background(), rest)
		switch {
		case err != nil:
			m.add(styleErr.Render(err.Error()))
		case len(contacts) == 0:
			m.add(styleErr.Render("no contact matches " + rest))
		case len(contacts) == 1:
			return m.open(contacts[0].JID)
		default:
			m.add(styleErr.Render(fmt.Sprintf("%d contacts match %q, be more specific:", len(contacts), rest)))
			m.add(formatContacts(contacts, 15))
		}
	case "/contacts":
		contacts, err := m.bot.Contacts(context.Background(), rest)
		if err != nil {
			m.add(styleErr.Render(err.Error()))
			return nil
		}
		if len(contacts) == 0 {
			m.add(styleDim.Render("no contacts found (they sync from your phone a few seconds after connecting)"))
			return nil
		}
		m.add(formatContacts(contacts, 0))
		m.add(styleDim.Render(fmt.Sprintf("%d contacts · /to <name or number> to open a chat", len(contacts))))
	case "/send":
		to, text, _ := strings.Cut(rest, " ")
		return m.async(func(ctx context.Context) error {
			_, err := m.bot.SendText(ctx, to, text)
			return err
		})
	case "/file":
		path, caption, _ := strings.Cut(rest, " ")
		if m.active == "" {
			m.add(styleErr.Render("no chat open: pick one with Tab, or /to <name or number>"))
			return nil
		}
		return m.async(func(ctx context.Context) error {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			_, err = m.bot.SendFile(ctx, m.active, data, filepath.Base(path), caption)
			return err
		})
	case "/groups":
		return func() tea.Msg {
			groups, err := m.bot.Client.GetJoinedGroups(context.Background())
			if err != nil {
				return lineMsg(styleErr.Render(err.Error()))
			}
			var sb strings.Builder
			for _, g := range groups {
				fmt.Fprintf(&sb, "%s  %s\n", g.JID, g.Name)
			}
			return lineMsg(strings.TrimRight(sb.String(), "\n"))
		}
	case "/pair":
		return func() tea.Msg {
			code, err := m.bot.PairPhone(context.Background(), rest)
			if err != nil {
				return lineMsg(styleErr.Render(err.Error()))
			}
			return lineMsg("Pairing code: " + code + "  (WhatsApp → Linked devices → Link with phone number)")
		}
	default:
		m.add(styleErr.Render("unknown command " + cmd))
	}
	return nil
}

func (m *model) async(fn func(ctx context.Context) error) tea.Cmd {
	return func() tea.Msg {
		if err := fn(context.Background()); err != nil {
			return lineMsg(styleErr.Render("error: " + err.Error()))
		}
		return nil
	}
}

func (m *model) add(s string) {
	if m.active == "" {
		m.console = append(m.console, s)
	} else {
		m.chatLines = append(m.chatLines, s)
	}
	m.refresh()
}

func (m *model) refresh() {
	if !m.ready {
		return
	}
	if len(m.console) > 1000 {
		m.console = m.console[len(m.console)-1000:]
	}
	lines := m.console
	if m.active != "" {
		lines = m.chatLines
		if len(lines) == 0 {
			lines = []string{styleDim.Render("no messages yet, say hi 👋")}
		}
	}
	content := lipgloss.NewStyle().Width(m.vp.Width).Render(strings.Join(lines, "\n"))
	if !m.status.LoggedIn && m.status.QR != "" {
		var sb strings.Builder
		qrterminal.GenerateWithConfig(m.status.QR, qrterminal.Config{
			Level: qrterminal.L, Writer: &sb, HalfBlocks: true,
			BlackChar: qrterminal.BLACK_BLACK, WhiteChar: qrterminal.WHITE_WHITE,
			BlackWhiteChar: qrterminal.BLACK_WHITE, WhiteBlackChar: qrterminal.WHITE_BLACK,
			QuietZone: 1,
		})
		content = "Scan with WhatsApp → Linked devices → Link a device\n(or type /pair <your phone number>)\n\n" + sb.String()
	}
	m.vp.SetContent(content)
	m.vp.GotoBottom()
}

func (m *model) View() string {
	if !m.ready {
		return "loading…"
	}
	who := m.status.JID
	if who == "" {
		who = "not logged in"
	}
	chat := "All activity"
	if m.active != "" {
		chat = m.activeName + styleDim.Render(" "+m.active)
	}
	header := styleHeader.Width(sidebarWidth + 1 + m.vp.Width).Render(fmt.Sprintf("wabot · %s · %s", who, m.status.State))
	title := lipgloss.NewStyle().Bold(true).Render(chat)
	main := lipgloss.JoinVertical(lipgloss.Left, title, m.vp.View())
	body := lipgloss.JoinHorizontal(lipgloss.Top, m.sidebar(), main)
	return header + "\n" + body + "\n" + m.input.View()
}

func (m *model) sidebar() string {
	inner := sidebarWidth - 3
	type entry struct{ top, bottom string }
	entries := []entry{{"All activity", styleDim.Render(fmt.Sprintf("%d chats", len(m.chats)))}}
	for _, c := range m.chats {
		when := c.LastAt.Local().Format("15:04")
		if time.Since(c.LastAt) > 24*time.Hour {
			when = c.LastAt.Local().Format("Jan 2")
		}
		name := c.Name
		if c.IsGroup {
			name = "👥 " + name
		}
		right := styleDim.Render(when)
		if n := m.unread[c.JID]; n > 0 {
			right = styleBadge.Render(fmt.Sprintf(" %d ", n))
		}
		top := ansi.Truncate(name, inner-lipgloss.Width(right)-1, "…")
		top += strings.Repeat(" ", max(1, inner-lipgloss.Width(top)-lipgloss.Width(right))) + right
		prefix := ""
		if c.LastFromMe {
			prefix = "you: "
		}
		text := shortText(Message{Type: c.LastType, Text: c.LastText})
		entries = append(entries, entry{top, styleDim.Render(ansi.Truncate(prefix+text, inner, "…"))})
	}

	visible := max(1, m.height/2)
	offset := 0
	if m.cursor >= visible {
		offset = m.cursor - visible + 1
	}
	var sb strings.Builder
	for i := offset; i < len(entries) && i < offset+visible; i++ {
		marker := "  "
		if (i == 0 && m.active == "") || (i > 0 && m.chats[i-1].JID == m.active) {
			marker = styleMe.Render("▌ ")
		}
		block := marker + entries[i].top + "\n  " + entries[i].bottom
		if i == m.cursor && m.sideFocused {
			block = styleCursor.Width(sidebarWidth).Render(block)
		}
		sb.WriteString(block + "\n")
	}
	return styleSidebar.Height(m.height).Render(strings.TrimRight(sb.String(), "\n"))
}

func (m *model) nameOf(jid string) string {
	for _, c := range m.chats {
		if c.JID == jid {
			return c.Name
		}
	}
	return strings.Split(jid, "@")[0]
}

func (m *model) formatChatLine(v Message) string {
	ts := styleDim.Render(v.Timestamp.Local().Format("Jan 2 15:04"))
	if v.FromMe {
		return fmt.Sprintf("%s %s %s", ts, styleMe.Render("me:"), shortText(v))
	}
	from := v.PushName
	if from == "" || !v.IsGroup {
		from = m.nameOf(v.Chat)
		if v.IsGroup {
			from = strings.Split(v.Sender, "@")[0]
		}
	}
	return fmt.Sprintf("%s %s %s", ts, styleThem.Render(from+":"), shortText(v))
}

func (m *model) formatFeedLine(v Message) string {
	ts := styleDim.Render(v.Timestamp.Local().Format("15:04"))
	if v.FromMe {
		return fmt.Sprintf("%s %s %s", ts, styleMe.Render("me → "+m.nameOf(v.Chat)+":"), shortText(v))
	}
	from := v.PushName
	if from == "" {
		from = strings.Split(v.Sender, "@")[0]
	}
	if v.IsGroup {
		from += " in " + m.nameOf(v.Chat)
	}
	return fmt.Sprintf("%s %s %s", ts, styleThem.Render(from+":"), shortText(v))
}

func formatContacts(cs []Contact, max int) string {
	var sb strings.Builder
	for i, c := range cs {
		if max > 0 && i == max {
			fmt.Fprintf(&sb, "  … and %d more\n", len(cs)-max)
			break
		}
		name := c.Name
		if c.PushName != "" && c.PushName != c.Name {
			name += styleDim.Render(" ~" + c.PushName)
		}
		fmt.Fprintf(&sb, "  %s  %s\n", styleThem.Render(fmt.Sprintf("%-16s", c.Phone)), name)
	}
	return strings.TrimRight(sb.String(), "\n")
}
