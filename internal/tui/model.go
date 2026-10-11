// Package tui is the Bubble Tea terminal UI.
package tui

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/williamtcastro/wabot/internal/bot"
	"github.com/williamtcastro/wabot/internal/store"
)

const (
	sidebarWidth = 34
	actor        = "tui"
)

type focus int

const (
	focusInput focus = iota
	focusChats
	focusMessages
)

type (
	eventMsg  struct{ v any }
	chatsMsg  []store.Chat
	noticeMsg struct {
		text string
		err  bool
	}
	clearNoticeMsg int
	panelMsg       struct{ title, body string }
	openedMsg      struct {
		seq           int
		jid, name     string
		msgs          []store.Message
		readSeq, jump int64
	}
	searchMsg struct {
		query   string
		results []store.Message
	}
	feedMsg         []store.Message
	contactNamesMsg []string
	dirMsg          struct {
		dir   string
		names []string
	}
)

type model struct {
	bot    *bot.Bot
	sub    chan any
	status bot.Status
	vp     viewport.Model
	input  textinput.Model
	w, h   int
	ready  bool
	focus  focus

	chats  []store.Chat
	cursor int
	filter string

	active, activeName string
	msgs               []store.Message
	feed               []store.Message

	visible []store.Message
	starts  []int
	sel     int

	replyTo, editing *store.Message
	pendingDelete    string

	panelTitle, panelBody string

	notice    string
	noticeErr bool
	noticeID  int
	openSeq   int

	newAfter int64

	searching   bool
	searchQuery string
	results     []store.Message
	resultSel   int

	contactNames []string
	dirCache     map[string][]string
	dirLoading   string
	loadingNames bool
	barShown     bool
}

// Run shows the TUI until the user quits.
func Run(b *bot.Bot) error {
	ch, unsub := b.Subscribe()
	defer unsub()
	_, err := tea.NewProgram(newModel(b, ch), tea.WithAltScreen()).Run()
	return err
}

func newModel(b *bot.Bot, sub chan any) *model {
	in := textinput.New()
	in.Prompt = "› "
	in.PromptStyle = styleAccent
	in.Placeholder = "Pick a chat with Tab or /to <name>  ·  /help"
	// The defaults also bind ctrl+n/ctrl+p; Ctrl+J/Ctrl+K are translated to down/up in key().
	in.KeyMap.NextSuggestion = key.NewBinding(key.WithKeys("down"))
	in.KeyMap.PrevSuggestion = key.NewBinding(key.WithKeys("up"))
	in.Focus()
	return &model{bot: b, sub: sub, input: in, status: b.Status(), sel: -1}
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
		msgs := m.bot.Store.Messages("", 200)
		slices.Reverse(msgs)
		return feedMsg(msgs)
	}
}

// open loads a chat ("" for All activity). Only the most recent request is applied.
func (m *model) open(jid string) tea.Cmd { return m.openAt(jid, 0) }

// openAt opens a chat and, when jump is a message seq, selects that message.
func (m *model) openAt(jid string, jump int64) tea.Cmd {
	m.openSeq++
	return m.load(m.openSeq, jid, jump)
}

func (m *model) load(seq int, jid string, jump int64) tea.Cmd {
	return func() tea.Msg {
		if jid == "" {
			return openedMsg{seq: seq}
		}
		limit := 400
		if jump > 0 {
			limit = max(limit, m.bot.Store.CountFrom(jid, jump)+20)
		}
		msgs := m.bot.Store.Messages(jid, limit)
		slices.Reverse(msgs)
		return openedMsg{seq: seq, jid: jid, name: m.bot.ChatName(context.Background(), jid, ""), msgs: msgs,
			readSeq: m.bot.Store.ReadSeq(jid), jump: jump}
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

	case searchMsg:
		m.searching, m.searchQuery, m.results, m.resultSel = true, msg.query, safeAll(msg.results), 0
		m.render(false)
		m.vp.GotoTop()
		return m, nil

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
		if i := chatIndex(m.chats, m.active); i >= 0 {
			m.activeName = m.chats[i].Name
		}
		m.render(m.sel < 0 && m.vp.AtBottom())
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
		m.newAfter = msg.readSeq
		m.syncCursor()
		m.layout()
		m.render(true)
		if msg.jump > 0 {
			if i := slices.IndexFunc(m.visible, func(x store.Message) bool { return x.Seq == msg.jump }); i >= 0 {
				m.sel, m.focus = i, focusMessages
				m.input.Blur()
				m.render(false)
			}
		}
		return m, m.markRead(msg.msgs)

	case eventMsg:
		cmds := []tea.Cmd{m.wait()}
		switch v := msg.v.(type) {
		case bot.Status:
			m.status = v
			if v.LoggedIn {
				cmds = append(cmds, m.loadChats())
			}
			m.render(false)
		case store.ChatRead, bot.NamesChanged:
			cmds = append(cmds, m.loadChats())
		case store.ChatsChanged:
			cmds = append(cmds, m.loadChats(), m.loadFeed())
			if m.active != "" {
				cmds = append(cmds, m.open(m.active))
			}
		case store.Message:
			v = safeMessage(v)
			if v.Type != "reaction" {
				m.feed = append(m.feed, v)
				if len(m.feed) > 500 {
					m.feed = m.feed[len(m.feed)-500:]
				}
			}
			if v.Chat == m.active && !slices.ContainsFunc(m.msgs, func(x store.Message) bool { return x.Seq == v.Seq }) {
				m.msgs = append(m.msgs, v)
				cmds = append(cmds, m.markRead([]store.Message{v}))
			}
			cmds = append(cmds, m.loadChats())
			m.render(m.sel < 0)
		case store.MessageUpdate:
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

func (m *model) filteredChats() []store.Chat {
	if m.filter == "" {
		return m.chats
	}
	q := strings.ToLower(m.filter)
	var out []store.Chat
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
