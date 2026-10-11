package main

import (
	"cmp"
	"context"
	"fmt"
	"hash/fnv"
	"maps"
	"mime"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mdp/qrterminal/v3"
)

const (
	sidebarWidth = 34
	tuiActor     = "tui"
)

type focus int

const (
	focusInput focus = iota
	focusChats
	focusMessages
)

var (
	colAccent = lipgloss.Color("42")
	colMuted  = lipgloss.Color("244")
	colFaint  = lipgloss.Color("238")
	colErr    = lipgloss.Color("203")
	colMe     = lipgloss.Color("114")
	colSel    = lipgloss.Color("236")

	styleDim    = lipgloss.NewStyle().Foreground(colMuted)
	styleFaint  = lipgloss.NewStyle().Foreground(colFaint)
	styleErr    = lipgloss.NewStyle().Foreground(colErr)
	styleMe     = lipgloss.NewStyle().Foreground(colMe)
	styleBold   = lipgloss.NewStyle().Bold(true)
	styleAccent = lipgloss.NewStyle().Foreground(colAccent)
	styleBadge  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(colAccent).Padding(0, 1)
	styleKey    = lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Bold(true)
	styleItalic = lipgloss.NewStyle().Foreground(colMuted).Italic(true)

	senderColors = []lipgloss.Color{"39", "213", "208", "141", "80", "220", "174", "111"}
	quickReacts  = []string{"👍", "❤️", "😂", "😮", "😢", "🙏"}
)

const tuiHelp = `Moving around
  Tab / Shift+Tab     cycle focus: input → chats → messages
  Ctrl+N / Ctrl+P     next / previous chat
  ↑ (empty input)     select messages
  PgUp / PgDn         scroll
  Esc                 cancel / close / back to the input

Chat list (focused)
  type                filter chats by name
  ↑ ↓  Enter          pick and open

Selected message
  r  reply           e  edit (yours, 20 min)
  d d  delete        s  save media to ~/Downloads
  1-6  react 👍 ❤️ 😂 😮 😢 🙏     0  remove reaction
  other letters      back to typing

Commands
  /to <name|number|jid>     open a chat
  /all                      live feed of every chat
  /contacts [search]        your address book
  /groups                   your groups
  /send <number|jid> <msg>  one-off message
  /file <path> [caption]    send a file to the open chat
  /read                     mark the open chat as read
  /pair <phone>             log in with a pairing code
  /help  /quit

Typing a command, Tab completes it (and names after /to, numbers after /send,
paths after /file), ↑ ↓ cycle the matches.`

type (
	eventMsg  struct{ v any }
	chatsMsg  []Chat
	noticeMsg struct {
		text string
		err  bool
	}
	clearNoticeMsg int
	panelMsg       struct{ title, body string }
	openedMsg      struct {
		seq       int
		jid, name string
		msgs      []Message
	}
	feedMsg []Message
)

type model struct {
	bot    *Bot
	sub    chan any
	status Status
	vp     viewport.Model
	input  textinput.Model
	w, h   int
	ready  bool
	focus  focus

	chats  []Chat
	unread map[string]int
	cursor int
	filter string

	active, activeName string
	msgs               []Message
	feed               []Message

	visible []Message
	starts  []int
	sel     int

	replyTo, editing *Message
	pendingDelete    string

	panelTitle, panelBody string

	notice    string
	noticeErr bool
	noticeID  int
	openSeq   int

	contactNames []string
	dirCache     map[string][]string
	dirLoading   string
	loadingNames bool
	barShown     bool
}

type contactNamesMsg []string

var tuiCommands = []struct{ name, args, desc string }{
	{"/to", "<name|number>", "open a chat"},
	{"/all", "", "every chat, live"},
	{"/contacts", "[search]", "address book"},
	{"/groups", "", "your groups"},
	{"/send", "<number> <text>", "one-off message"},
	{"/file", "<path> [caption]", "send a file here"},
	{"/read", "", "mark this chat read"},
	{"/pair", "<phone>", "pairing-code login"},
	{"/help", "", "keys and commands"},
	{"/quit", "", "exit"},
}

// suggest refreshes the input's completions for the command being typed and resizes the layout
// when the hint bar appears or disappears.
func (m *model) suggest() tea.Cmd {
	v := m.input.Value()
	var list []string
	var load tea.Cmd
	cmd, arg, hasArg := strings.Cut(v, " ")
	switch {
	case !strings.HasPrefix(v, "/"):
	case !hasArg:
		for _, c := range tuiCommands {
			if c.args != "" {
				c.name += " "
			}
			list = append(list, c.name)
		}
	case cmd == "/to":
		seen := map[string]bool{}
		add := func(target string) {
			if target != "" && !seen[target] {
				seen[target] = true
				list = append(list, "/to "+target)
			}
		}
		for _, c := range m.chats {
			add(chatTarget(c))
		}
		for _, name := range m.contactNames {
			add(name)
		}
		if m.contactNames == nil && !m.loadingNames {
			m.loadingNames = true
			load = func() tea.Msg {
				contacts, _ := m.bot.Contacts(context.Background(), "")
				names := make([]string, 0, len(contacts))
				for _, c := range contacts {
					names = append(names, safeLine(c.Name))
				}
				return contactNamesMsg(names)
			}
		}
	case cmd == "/send":
		for _, c := range m.chats {
			target := userOf(c.JID)
			if c.IsGroup {
				target = c.JID
			}
			list = append(list, "/send "+target+" ")
		}
	case cmd == "/file" && !strings.Contains(arg, " "):
		dir, _ := filepath.Split(arg)
		names, ok := m.dirCache[dir]
		if ok {
			list = pathSuggestions(arg, names)
		} else if m.dirLoading != dir {
			m.dirLoading = dir
			load = func() tea.Msg { return dirMsg{dir, readDirNames(dir)} }
		}
	}
	m.input.ShowSuggestions = true // SetSuggestions only refreshes the matches while this is on
	m.input.SetSuggestions(list)
	m.input.ShowSuggestions = len(list) > 0
	if shown := m.contextBar() != ""; shown != m.barShown {
		m.barShown = shown
		m.layout()
		m.render(m.sel < 0)
	}
	return load
}

// chatTarget is what /to needs to reopen a chat: its contact name if it has one, else its number or group JID.
func chatTarget(c Chat) string {
	switch {
	case c.IsGroup:
		return c.JID
	case strings.HasPrefix(c.Name, "~"), strings.HasPrefix(c.Name, "+"), c.Name == "You":
		return userOf(c.JID)
	}
	return c.Name
}

type dirMsg struct {
	dir   string
	names []string
}

// readDirNames lists a directory for /file completion (directories end in "/"); it runs off the UI loop.
func readDirNames(dir string) []string {
	entries, _ := os.ReadDir(cmp.Or(expandHome(dir), "."))
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name()+"/")
		} else {
			names = append(names, e.Name())
		}
	}
	return names
}

func pathSuggestions(typed string, names []string) []string {
	dir, base := filepath.Split(typed)
	var out []string
	for _, name := range names {
		if strings.Contains(name, " ") || (strings.HasPrefix(name, ".") && !strings.HasPrefix(base, ".")) || !strings.HasPrefix(name, base) {
			continue
		}
		out = append(out, "/file "+dir+name)
		if len(out) == 200 {
			break
		}
	}
	return out
}

func expandHome(path string) string {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return path
}

func RunTUI(b *Bot) error {
	ch, unsub := b.Subscribe()
	defer unsub()
	in := textinput.New()
	in.Prompt = "› "
	in.PromptStyle = styleAccent
	in.Placeholder = "Pick a chat with Tab or /to <name>  ·  /help"
	in.Focus()
	m := &model{bot: b, sub: ch, input: in, status: b.Status(), unread: map[string]int{}, sel: -1}
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, m.wait(), m.loadChats(), m.loadFeed())
}

func (m *model) wait() tea.Cmd {
	return func() tea.Msg { return eventMsg{<-m.sub} }
}

func (m *model) loadChats() tea.Cmd {
	return func() tea.Msg {
		chats, err := m.bot.Chats(context.Background(), 300)
		if err != nil {
			return noticeMsg{"load chats: " + err.Error(), true}
		}
		return chatsMsg(chats)
	}
}

func (m *model) loadFeed() tea.Cmd {
	return func() tea.Msg {
		msgs := m.bot.Messages("", 200)
		slices.Reverse(msgs)
		return feedMsg(msgs)
	}
}

// open loads a chat ("" for All activity). Only the most recent request is applied.
func (m *model) open(jid string) tea.Cmd {
	m.openSeq++
	return m.load(m.openSeq, jid)
}

func (m *model) load(seq int, jid string) tea.Cmd {
	return func() tea.Msg {
		if jid == "" {
			return openedMsg{seq: seq}
		}
		msgs := m.bot.Messages(jid, 400)
		slices.Reverse(msgs)
		return openedMsg{seq: seq, jid: jid, name: m.bot.chatName(context.Background(), jid, ""), msgs: msgs}
	}
}

func (m *model) do(fn func(ctx context.Context) (string, error)) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		ok, err := fn(ctx)
		if err != nil {
			return noticeMsg{err.Error(), true}
		}
		if ok != "" {
			return noticeMsg{ok, false}
		}
		return nil
	}
}

func (m *model) setNotice(text string, isErr bool) tea.Cmd {
	m.notice, m.noticeErr = safe(text), isErr
	m.noticeID++
	id := m.noticeID
	return tea.Tick(5*time.Second, func(time.Time) tea.Msg { return clearNoticeMsg(id) })
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		if !m.ready {
			m.vp = viewport.New(0, 0)
			m.ready = true
		}
		m.layout()
		m.render(true)
		return m, nil

	case tea.KeyMsg:
		return m, m.key(msg)

	case noticeMsg:
		return m, m.setNotice(msg.text, msg.err)

	case clearNoticeMsg:
		if int(msg) == m.noticeID {
			m.notice = ""
		}
		return m, nil

	case panelMsg:
		m.panelTitle, m.panelBody = msg.title, msg.body
		m.render(false)
		m.vp.GotoTop()
		return m, nil

	case dirMsg:
		if m.dirCache == nil {
			m.dirCache = map[string][]string{}
		}
		m.dirCache[msg.dir], m.dirLoading = msg.names, ""
		return m, m.suggest()

	case contactNamesMsg:
		m.contactNames, m.loadingNames = msg, false
		return m, m.suggest()

	case chatsMsg:
		for i := range msg {
			msg[i].Name, msg[i].LastText = safeLine(msg[i].Name), safeLine(msg[i].LastText)
		}
		keep := m.cursorJID()
		m.chats = msg
		if m.focus == focusChats && keep != "" {
			m.cursor = chatIndex(m.filteredChats(), keep) + 1
		} else {
			m.syncCursor()
		}
		return m, nil

	case feedMsg:
		m.feed = safeAll(slices.DeleteFunc(msg, isReaction))
		m.render(true)
		return m, nil

	case openedMsg:
		if msg.seq != m.openSeq {
			return m, nil
		}
		if m.editing != nil {
			m.input.SetValue("")
		}
		m.replyTo, m.editing, m.panelTitle = nil, nil, ""
		m.active, m.activeName, m.msgs, m.sel = msg.jid, safeLine(msg.name), safeAll(msg.msgs), -1
		if i := chatIndex(m.chats, msg.jid); i >= 0 {
			m.activeName = m.chats[i].Name
		}
		if msg.jid != "" {
			last := int64(0)
			if len(m.msgs) > 0 {
				last = m.msgs[len(m.msgs)-1].Seq
			}
			for _, x := range m.feed {
				if x.Chat == msg.jid && x.Seq > last {
					m.msgs = append(m.msgs, x)
				}
			}
		}
		delete(m.unread, msg.jid)
		m.syncCursor()
		m.layout()
		m.render(true)
		return m, m.markRead(msg.msgs)

	case eventMsg:
		cmds := []tea.Cmd{m.wait()}
		switch v := msg.v.(type) {
		case Status:
			m.status = v
			if v.LoggedIn {
				cmds = append(cmds, m.loadChats())
			}
			m.render(false)
		case ChatsChanged:
			cmds = append(cmds, m.loadChats(), m.loadFeed())
			if m.active != "" {
				cmds = append(cmds, m.open(m.active))
			}
		case Message:
			v = safeMessage(v)
			if v.Type != "reaction" {
				m.feed = append(m.feed, v)
				if len(m.feed) > 500 {
					m.feed = m.feed[len(m.feed)-500:]
				}
			}
			if v.Chat == m.active && !slices.ContainsFunc(m.msgs, func(x Message) bool { return x.Seq == v.Seq }) {
				m.msgs = append(m.msgs, v)
				cmds = append(cmds, m.markRead([]Message{v}))
			} else if !v.FromMe && v.Type != "reaction" {
				m.unread[v.Chat]++
			}
			cmds = append(cmds, m.loadChats())
			m.render(m.sel < 0)
		case MessageUpdate:
			replaceMessage(m.msgs, safeMessage(v.Message))
			replaceMessage(m.feed, safeMessage(v.Message))
			cmds = append(cmds, m.loadChats())
			m.render(false)
		}
		return m, tea.Batch(cmds...)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *model) key(k tea.KeyMsg) tea.Cmd {
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
		}
	}
	switch k.String() {
	case "ctrl+c":
		return tea.Quit
	case "ctrl+n", "ctrl+p":
		step := 1
		if k.String() == "ctrl+p" {
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
			_, err := m.bot.Delete(ctx, tuiActor, chat, target.ID)
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
			_, err := m.bot.React(ctx, tuiActor, chat, target.ID, emoji)
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

func (m *model) save(ctx context.Context, target Message) (string, error) {
	data, full, err := m.bot.DownloadMedia(ctx, target.Seq)
	if err != nil {
		return "", err
	}
	dir, err := os.UserHomeDir()
	if err == nil {
		dir = filepath.Join(dir, "Downloads")
	}
	if _, err := os.Stat(dir); err != nil {
		dir = "."
	}
	name := filepath.Base(safe(mediaFileName(full)))
	ext := filepath.Ext(name)
	for i := 0; i < 100; i++ {
		path := filepath.Join(dir, name)
		if i > 0 {
			path = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(name, ext), i, ext))
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = f.Write(data)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return "saved " + path, err
	}
	return "", fmt.Errorf("%s: too many files with that name in %s", name, dir)
}

func (m *model) submit(line string) tea.Cmd {
	if line == "" {
		return nil
	}
	if strings.HasPrefix(line, "/") {
		return m.command(line)
	}
	if m.active == "" {
		return m.setNotice("no chat open: press Tab to pick one, or /to <name>", true)
	}
	chat, reply, editing := m.active, m.replyTo, m.editing
	m.replyTo, m.editing = nil, nil
	m.layout()
	m.render(true)
	if editing != nil {
		return m.do(func(ctx context.Context) (string, error) {
			_, err := m.bot.Edit(ctx, tuiActor, chat, editing.ID, line)
			return "", err
		})
	}
	opt := SendOptions{}
	if reply != nil {
		opt.ReplyTo = reply.ID
	}
	return m.do(func(ctx context.Context) (string, error) {
		_, err := m.bot.SendText(ctx, tuiActor, chat, line, opt)
		return "", err
	})
}

func (m *model) command(line string) tea.Cmd {
	cmd, rest, _ := strings.Cut(line, " ")
	rest = strings.TrimSpace(rest)
	switch cmd {
	case "/quit", "/q":
		return tea.Quit
	case "/help":
		return func() tea.Msg { return panelMsg{"Help", tuiHelp} }
	case "/all":
		return m.open("")
	case "/to":
		return m.openByName(rest)
	case "/contacts":
		return func() tea.Msg {
			contacts, err := m.bot.Contacts(context.Background(), rest)
			if err != nil {
				return noticeMsg{err.Error(), true}
			}
			if len(contacts) == 0 {
				return noticeMsg{"no contacts found (they sync from your phone after connecting)", true}
			}
			return panelMsg{fmt.Sprintf("Contacts (%d) · /to <name> to open", len(contacts)), formatContacts(contacts, 0)}
		}
	case "/groups":
		return func() tea.Msg {
			groups, err := m.bot.Client.GetJoinedGroups(context.Background())
			if err != nil {
				return noticeMsg{err.Error(), true}
			}
			var sb strings.Builder
			for _, g := range groups {
				fmt.Fprintf(&sb, "%s  %s\n", styleDim.Render(g.JID.String()), safeLine(g.Name))
			}
			return panelMsg{fmt.Sprintf("Groups (%d) · /to <jid> to open", len(groups)), sb.String()}
		}
	case "/send":
		to, text, _ := strings.Cut(rest, " ")
		return m.do(func(ctx context.Context) (string, error) {
			_, err := m.bot.SendText(ctx, tuiActor, to, text, SendOptions{})
			return "sent to " + to, err
		})
	case "/file":
		path, caption, _ := strings.Cut(rest, " ")
		if m.active == "" {
			return m.setNotice("no chat open", true)
		}
		chat, path := m.active, expandHome(path)
		return m.do(func(ctx context.Context) (string, error) {
			data, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			_, err = m.bot.SendFile(ctx, tuiActor, chat, data, filepath.Base(path), caption, SendOptions{})
			return "", err
		})
	case "/read":
		if m.active == "" {
			return m.setNotice("no chat open", true)
		}
		chat := m.active
		return m.do(func(ctx context.Context) (string, error) {
			n, err := m.bot.MarkRead(ctx, tuiActor, chat, nil)
			return fmt.Sprintf("marked %d messages as read", n), err
		})
	case "/pair":
		return m.do(func(ctx context.Context) (string, error) {
			code, err := m.bot.PairPhone(ctx, tuiActor, rest)
			return "pairing code " + code + " (WhatsApp → Linked devices → Link with phone number)", err
		})
	}
	return m.setNotice("unknown command "+cmd+" (try /help)", true)
}

func (m *model) openByName(q string) tea.Cmd {
	m.openSeq++
	seq := m.openSeq
	return func() tea.Msg {
		ctx := context.Background()
		if _, err := ParseRecipient(q); err == nil {
			jid, err := m.bot.Resolve(ctx, q)
			if err != nil {
				return noticeMsg{err.Error(), true}
			}
			return m.load(seq, m.bot.phoneJID(jid).String())()
		}
		contacts, err := m.bot.Contacts(ctx, q)
		switch {
		case err != nil:
			return noticeMsg{err.Error(), true}
		case len(contacts) == 0:
			return noticeMsg{"no contact matches " + q, true}
		case len(contacts) == 1:
			return m.load(seq, contacts[0].JID)()
		}
		return panelMsg{fmt.Sprintf("%d contacts match %q · be more specific", len(contacts), q), formatContacts(contacts, 30)}
	}
}

// markRead sends read receipts for incoming messages in the open chat.
func (m *model) markRead(msgs []Message) tea.Cmd {
	var ids []string
	for _, x := range msgs {
		if !x.FromMe && x.Type != "reaction" && x.Chat == m.active {
			ids = append(ids, x.ID)
		}
	}
	if len(ids) > 20 {
		ids = ids[len(ids)-20:]
	}
	if len(ids) == 0 || !m.status.LoggedIn {
		return nil
	}
	chat := m.active
	return func() tea.Msg {
		_, _ = m.bot.MarkRead(context.Background(), tuiActor, chat, ids)
		return nil
	}
}

func (m *model) filteredChats() []Chat {
	if m.filter == "" {
		return m.chats
	}
	q := strings.ToLower(m.filter)
	var out []Chat
	for _, c := range m.chats {
		if strings.Contains(strings.ToLower(c.Name), q) || strings.Contains(c.JID, q) {
			out = append(out, c)
		}
	}
	return out
}

func (m *model) cursorJID() string {
	chats := m.filteredChats()
	if m.cursor <= 0 || m.cursor > len(chats) {
		return ""
	}
	return chats[m.cursor-1].JID
}

func (m *model) syncCursor() {
	if m.focus == focusChats {
		m.cursor = clamp(m.cursor, 0, len(m.filteredChats()))
		return
	}
	m.cursor = chatIndex(m.filteredChats(), m.active) + 1
}

func (m *model) layout() {
	if !m.ready {
		return
	}
	chatW := m.w - sidebarWidth
	m.vp.Width = max(10, chatW-4)
	m.vp.Height = max(3, m.mainHeight()-4)
	m.input.Width = max(10, m.w-8)
}

func (m *model) mainHeight() int {
	h := m.h - 1 - 3
	if m.contextBar() != "" {
		h--
	}
	return h
}

// render rebuilds the message pane; toBottom scrolls to the newest message.
func (m *model) render(toBottom bool) {
	if !m.ready {
		return
	}
	width := m.vp.Width
	var content string
	m.visible, m.starts = nil, nil
	switch {
	case !m.status.LoggedIn && m.status.QR != "":
		var sb strings.Builder
		qrterminal.GenerateWithConfig(m.status.QR, qrterminal.Config{
			Level: qrterminal.L, Writer: &sb, HalfBlocks: true,
			BlackChar: qrterminal.BLACK_BLACK, WhiteChar: qrterminal.WHITE_WHITE,
			BlackWhiteChar: qrterminal.BLACK_WHITE, WhiteBlackChar: qrterminal.WHITE_BLACK,
			QuietZone: 1,
		})
		content = "Scan with WhatsApp → Linked devices → Link a device\n" +
			styleDim.Render("or type /pair <your phone number>") + "\n\n" + sb.String()
	case m.panelTitle != "":
		content = lipgloss.NewStyle().Width(width).Render(m.panelBody)
	case m.active == "":
		content = m.renderFeed(width)
	default:
		content = m.renderChat(width)
	}
	m.vp.SetContent(content)
	switch {
	case m.sel >= 0 && m.sel < len(m.starts):
		start := m.starts[m.sel]
		end := len(strings.Split(content, "\n"))
		if m.sel+1 < len(m.starts) {
			end = m.starts[m.sel+1]
		}
		if start < m.vp.YOffset {
			m.vp.SetYOffset(start)
		} else if end > m.vp.YOffset+m.vp.Height {
			m.vp.SetYOffset(end - m.vp.Height)
		}
	case toBottom:
		m.vp.GotoBottom()
	}
}

func (m *model) renderFeed(width int) string {
	if len(m.feed) == 0 {
		return styleDim.Render("\n  Nothing yet. Messages from every chat will show up here live.\n\n  Press Tab to pick a chat, or type /to <name>.  /help for everything else.")
	}
	var lines []string
	lastDay := ""
	for _, x := range m.feed {
		if day := dayLabel(x.Timestamp); day != lastDay {
			lines = append(lines, separator(day, width))
			lastDay = day
		}
		who := styleBold.Foreground(colorFor(x.Chat)).Render(m.nameOf(x.Chat))
		if x.FromMe {
			who = styleMe.Render("you") + styleDim.Render(" → ") + who
		} else if x.IsGroup {
			who = styleDim.Render(cmp.Or(x.PushName, userOf(x.Sender))+" in ") + who
		}
		line := styleDim.Render(x.Timestamp.Local().Format("15:04")) + "  " + who + styleDim.Render("  ") + shortText(x)
		lines = append(lines, ansi.Truncate(line, width, "…"))
	}
	return strings.Join(lines, "\n")
}

func (m *model) renderChat(width int) string {
	reacts := map[string]map[string]string{}
	byID := map[string]Message{}
	for _, x := range m.msgs {
		if x.Type == "reaction" {
			if reacts[x.ReplyTo] == nil {
				reacts[x.ReplyTo] = map[string]string{}
			}
			reacts[x.ReplyTo][x.Sender] = x.Text
			continue
		}
		byID[x.ID] = x
		m.visible = append(m.visible, x)
	}
	if len(m.visible) == 0 {
		return styleDim.Render("\n  No messages yet. Say hi 👋")
	}
	bubbleW := max(20, width*3/4)
	var out []string
	lastDay, lastSender := "", ""
	for i, x := range m.visible {
		if day := dayLabel(x.Timestamp); day != lastDay {
			if len(out) > 0 {
				out = append(out, "")
			}
			out = append(out, separator(day, width), "")
			lastDay, lastSender = day, ""
		} else if x.Sender != lastSender {
			out = append(out, "")
		}
		var block []string
		if x.IsGroup && !x.FromMe && x.Sender != lastSender {
			block = append(block, styleBold.Foreground(colorFor(x.Sender)).Render(cmp.Or(x.PushName, userOf(x.Sender))))
		}
		lastSender = x.Sender
		if q, ok := byID[x.ReplyTo]; ok && x.ReplyTo != "" {
			who := "you"
			if !q.FromMe {
				who = cmp.Or(q.PushName, m.nameOf(q.Sender))
			}
			block = append(block, styleFaint.Render("┃ ")+styleDim.Render(ansi.Truncate(who+": "+shortText(q), bubbleW-2, "…")))
		} else if x.ReplyTo != "" {
			block = append(block, styleFaint.Render("┃ ↩ reply to an earlier message"))
		}
		body := m.bodyOf(x)
		meta := x.Timestamp.Local().Format("15:04")
		if x.Edited && !x.Deleted {
			meta = "edited " + meta
		}
		if x.FromMe {
			meta += " ✓"
		}
		if r := joinReactions(reacts[x.ID]); r != "" {
			meta = "│ " + r + "  " + meta
		}
		wrapped := strings.Split(lipgloss.NewStyle().Width(bubbleW).Render(body), "\n")
		for j := range wrapped {
			wrapped[j] = strings.TrimRight(wrapped[j], " ")
		}
		last := len(wrapped) - 1
		if lipgloss.Width(wrapped[last])+2+lipgloss.Width(meta) <= bubbleW {
			wrapped[last] += "  " + styleDim.Render(meta)
		} else {
			wrapped = append(wrapped, styleDim.Render(meta))
		}
		block = append(block, wrapped...)

		selected := i == m.sel && m.focus == focusMessages
		m.starts = append(m.starts, len(out))
		for _, line := range block {
			if x.FromMe {
				line = lipgloss.PlaceHorizontal(width-2, lipgloss.Right, line)
			}
			gutter := "  "
			if selected {
				gutter = styleAccent.Render("▌ ")
			}
			out = append(out, gutter+line)
		}
	}
	return strings.Join(out, "\n")
}

func (m *model) bodyOf(x Message) string {
	if x.Deleted {
		return styleItalic.Render("🚫 This message was deleted")
	}
	style := lipgloss.NewStyle()
	if x.FromMe {
		style = styleMe
	}
	text := x.Text
	switch x.Type {
	case "image":
		text = "📷 Photo" + prefixed("\n", x.Text)
	case "video":
		text = "🎥 Video" + prefixed("\n", x.Text)
	case "audio":
		text = "🎤 Voice message"
	case "sticker":
		text = "💟 Sticker"
	case "document":
		name := "Document"
		if x.Media != nil && x.Media.FileName != "" {
			name = x.Media.FileName
		}
		text = "📄 " + name
		if x.Media != nil && x.Media.Size > 0 {
			text += styleDim.Render(" · " + humanSize(x.Media.Size))
		}
	case "location":
		text = "📍 " + x.Text
	}
	if x.Media != nil && m.focus == focusMessages && m.sel >= 0 && m.sel < len(m.visible) && m.visible[m.sel].ID == x.ID {
		text += styleDim.Render("  (s to save)")
	}
	return style.Render(text)
}

func (m *model) View() string {
	if !m.ready {
		return "loading…"
	}
	mainH := m.mainHeight()
	sidebar := m.box(m.renderSidebar(mainH-2), sidebarWidth, mainH, m.focus == focusChats)

	title := m.paneTitle()
	pane := lipgloss.JoinVertical(lipgloss.Left, title, styleFaint.Render(strings.Repeat("─", m.vp.Width)), m.vp.View())
	chat := m.box(pane, m.w-sidebarWidth, mainH, m.focus == focusMessages)

	body := lipgloss.JoinHorizontal(lipgloss.Top, sidebar, chat)
	parts := []string{body}
	if bar := m.contextBar(); bar != "" {
		parts = append(parts, bar)
	}
	parts = append(parts, m.box(m.input.View(), m.w, 3, m.focus == focusInput), m.footer())
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

func (m *model) box(content string, w, h int, focused bool) string {
	border := colFaint
	if focused {
		border = colAccent
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).
		Padding(0, 1).Width(w - 2).Height(h - 2).MaxHeight(h).Render(content)
}

func (m *model) paneTitle() string {
	switch {
	case m.panelTitle != "":
		return styleBold.Render(m.panelTitle) + styleDim.Render("   esc to close")
	case m.active == "":
		return styleBold.Render("All activity") + styleDim.Render("   every chat, live")
	}
	sub := m.active
	if strings.HasSuffix(sub, "@s.whatsapp.net") {
		sub = "+" + userOf(sub)
	}
	return ansi.Truncate(styleBold.Render(m.activeName)+styleDim.Render("   "+sub), m.vp.Width, "…")
}

func (m *model) contextBar() string {
	var label string
	switch {
	case m.editing != nil:
		label = styleAccent.Render("✎ Editing") + styleDim.Render("  "+shortText(*m.editing))
	case m.replyTo != nil:
		who := "yourself"
		if !m.replyTo.FromMe {
			who = cmp.Or(m.replyTo.PushName, m.nameOf(m.replyTo.Sender))
		}
		label = styleAccent.Render("↩ Replying to "+who) + styleDim.Render("  "+shortText(*m.replyTo))
	case strings.HasPrefix(m.input.Value(), "/") && !strings.Contains(m.input.Value(), " "):
		var hints []string
		for _, c := range tuiCommands {
			if strings.HasPrefix(c.name, m.input.Value()) {
				hints = append(hints, styleKey.Render(c.name)+styleDim.Render(prefixed(" ", c.args)+"  "+c.desc))
			}
		}
		if len(hints) == 0 {
			return " " + styleErr.Render("no such command") + styleFaint.Render("  /help lists them")
		}
		return " " + ansi.Truncate(strings.Join(hints, styleFaint.Render("  ·  ")), m.w-24, "…") + styleFaint.Render("  tab complete · ↑↓ cycle")
	default:
		return ""
	}
	return " " + ansi.Truncate(label, m.w-16, "…") + styleFaint.Render("  esc to cancel")
}

func (m *model) footer() string {
	var left string
	switch {
	case m.notice != "" && m.noticeErr:
		left = styleErr.Render("✗ " + m.notice)
	case m.notice != "":
		left = styleAccent.Render("✓ ") + m.notice
	case m.focus == focusChats:
		left = hints("type", "filter", "↑↓", "move", "enter", "open", "esc", "back")
	case m.focus == focusMessages:
		left = hints("r", "reply", "1-6", "react", "e", "edit", "dd", "delete", "s", "save", "esc", "back")
	default:
		left = hints("tab", "chats", "↑", "select msg", "^N/^P", "switch", "/help", "commands")
	}
	dot, state := styleErr.Render("●"), m.status.State
	if m.status.Connected && m.status.LoggedIn {
		dot, state = styleAccent.Render("●"), "+"+userOf(m.status.JID)
	}
	right := dot + " " + styleDim.Render(state) + " "
	left = ansi.Truncate(" "+left, m.w-lipgloss.Width(right)-1, "…")
	gap := max(1, m.w-lipgloss.Width(left)-lipgloss.Width(right))
	return left + strings.Repeat(" ", gap) + right
}

func hints(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, styleKey.Render(pairs[i])+" "+styleDim.Render(pairs[i+1]))
	}
	return strings.Join(parts, styleFaint.Render("  ·  "))
}

func (m *model) renderSidebar(height int) string {
	inner := sidebarWidth - 4
	head := styleBold.Render("Chats") + styleDim.Render(fmt.Sprintf("  %d", len(m.chats)))
	if m.filter != "" || m.focus == focusChats {
		head = styleAccent.Render("⌕ ") + m.filter + styleAccent.Render("▏")
	}
	type row struct{ top, bottom string }
	allBottom := "every chat, live"
	switch n := len(m.unread); {
	case n == 1:
		allBottom = "1 chat with new messages"
	case n > 1:
		allBottom = fmt.Sprintf("%d chats with new messages", n)
	}
	chats := m.filteredChats()
	rows := []row{{"All activity", allBottom}}
	for _, c := range chats {
		name := c.Name
		if c.IsGroup {
			name = "👥 " + name
		}
		right := styleDim.Render(shortTime(c.LastAt))
		nameStyle := lipgloss.NewStyle()
		if n := m.unread[c.JID]; n > 0 {
			right = styleBadge.Render(fmt.Sprint(n))
			nameStyle = styleBold
		}
		top := nameStyle.Render(ansi.Truncate(name, inner-lipgloss.Width(right)-3, "…"))
		top += strings.Repeat(" ", max(1, inner-2-lipgloss.Width(top)-lipgloss.Width(right))) + right
		preview := shortText(Message{Type: c.LastType, Text: c.LastText})
		if c.LastFromMe {
			preview = "you: " + preview
		}
		rows = append(rows, row{top, ansi.Truncate(preview, inner-2, "…")})
	}

	visible := max(1, (height-2)/2)
	offset := 0
	if m.cursor >= visible {
		offset = m.cursor - visible + 1
	}
	lines := []string{head, ""}
	for i := offset; i < len(rows) && i < offset+visible; i++ {
		isActive := (i == 0 && m.active == "") || (i > 0 && chats[i-1].JID == m.active)
		marker := "  "
		if isActive {
			marker = styleAccent.Render("▌ ")
		}
		top, bottom := marker+rows[i].top, "  "+styleDim.Render(rows[i].bottom)
		if i == m.cursor && m.focus == focusChats {
			sel := lipgloss.NewStyle().Background(colSel).Width(inner)
			top, bottom = sel.Render(top), sel.Render(bottom)
		}
		lines = append(lines, top, bottom)
	}
	return strings.Join(lines, "\n")
}

func (m *model) nameOf(jid string) string {
	if i := chatIndex(m.chats, jid); i >= 0 {
		return m.chats[i].Name
	}
	if m.status.JID != "" && jid == m.status.JID {
		return "You"
	}
	return "+" + userOf(jid)
}

func formatContacts(cs []Contact, limit int) string {
	var sb strings.Builder
	for i, c := range cs {
		if limit > 0 && i == limit {
			fmt.Fprintf(&sb, "… and %d more\n", len(cs)-limit)
			break
		}
		name := safeLine(c.Name)
		if c.PushName != "" && c.PushName != c.Name {
			name += styleDim.Render("  ~" + safeLine(c.PushName))
		}
		fmt.Fprintf(&sb, "%s  %s\n", styleDim.Render(fmt.Sprintf("%-16s", c.Phone)), name)
	}
	return strings.TrimRight(sb.String(), "\n")
}

func separator(label string, width int) string {
	label = " " + label + " "
	side := max(2, (width-lipgloss.Width(label))/2)
	return styleFaint.Render(strings.Repeat("─", side)) + styleDim.Render(label) + styleFaint.Render(strings.Repeat("─", side))
}

func dayLabel(t time.Time) string {
	t = t.Local()
	now := time.Now()
	y, mo, d := t.Date()
	switch ny, nm, nd := now.Date(); {
	case y == ny && mo == nm && d == nd:
		return "Today"
	case now.Sub(t) < 48*time.Hour && now.AddDate(0, 0, -1).Day() == d:
		return "Yesterday"
	case y == ny:
		return t.Format("Mon, Jan 2")
	}
	return t.Format("Jan 2, 2006")
}

func shortTime(t time.Time) string {
	switch label := dayLabel(t); label {
	case "Today":
		return t.Local().Format("15:04")
	case "Yesterday":
		return "Yesterday"
	}
	if time.Since(t) < 7*24*time.Hour {
		return t.Local().Format("Mon")
	}
	return t.Local().Format("02/01/06")
}

func joinReactions(r map[string]string) string {
	counts := map[string]int{}
	for _, e := range r {
		if e != "" {
			counts[e]++
		}
	}
	var parts []string
	for _, e := range slices.Sorted(maps.Keys(counts)) {
		if counts[e] > 1 {
			parts = append(parts, fmt.Sprintf("%s%d", e, counts[e]))
		} else {
			parts = append(parts, e)
		}
	}
	return strings.Join(parts, " ")
}

func colorFor(s string) lipgloss.Color {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return senderColors[h.Sum32()%uint32(len(senderColors))]
}

func userOf(jid string) string { return strings.SplitN(jid, "@", 2)[0] }

func prefixed(prefix, s string) string {
	if s == "" {
		return ""
	}
	return prefix + s
}

func humanSize(n uint64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}

// mediaFileName prefers our table because mime.ExtensionsByType sorts alphabetically (.jfif for JPEG).
func mediaFileName(m Message) string {
	if m.Media.FileName != "" {
		return m.Media.FileName
	}
	ext := extByType[strings.SplitN(m.Media.Mimetype, ";", 2)[0]]
	if ext == "" {
		if exts, _ := mime.ExtensionsByType(m.Media.Mimetype); len(exts) > 0 {
			ext = exts[0]
		}
	}
	return fmt.Sprintf("wabot-%s-%d%s", m.Type, m.Seq, ext)
}

var extByType = map[string]string{
	"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp", "video/mp4": ".mp4",
	"audio/ogg": ".ogg", "audio/mpeg": ".mp3", "application/pdf": ".pdf",
}

func isReaction(m Message) bool { return m.Type == "reaction" }

func chatIndex(chats []Chat, jid string) int {
	return slices.IndexFunc(chats, func(c Chat) bool { return c.JID == jid })
}

func replaceMessage(msgs []Message, updated Message) {
	for i := range msgs {
		if msgs[i].Chat == updated.Chat && msgs[i].ID == updated.ID {
			msgs[i] = updated
		}
	}
}

func clamp(v, lo, hi int) int { return max(lo, min(hi, v)) }

func shortText(m Message) string {
	if m.Deleted {
		return "🚫 deleted"
	}
	t := strings.Join(strings.Fields(m.Text), " ")
	if label, ok := mediaLabels[m.Type]; ok {
		t = strings.TrimSpace(label + " " + t)
	}
	return t
}

var mediaLabels = map[string]string{
	"image": "📷", "video": "🎥", "audio": "🎤 voice message", "document": "📄", "sticker": "💟 sticker",
	"location": "📍", "reaction": "reacted",
}

// safe strips terminal control sequences and bidi overrides from text that strangers control
// (messages, names, file names) so it can't rewrite the screen, clipboard or window title.
func safe(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < 0x20, r >= 0x7f && r < 0xa0, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return -1
		}
		return r
	}, ansi.Strip(s))
}

// safeLine is safe for names and other single-line fields: newlines and tabs become spaces.
func safeLine(s string) string {
	return strings.Join(strings.Fields(safe(s)), " ")
}

func safeMessage(x Message) Message {
	x.Text, x.PushName = safe(x.Text), safeLine(x.PushName)
	if x.Media != nil {
		media := *x.Media
		media.FileName = safeLine(media.FileName)
		x.Media = &media
	}
	return x
}

func safeAll(msgs []Message) []Message {
	for i := range msgs {
		msgs[i] = safeMessage(msgs[i])
	}
	return msgs
}
