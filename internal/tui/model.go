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

	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
	"github.com/williamtcastro/wame/internal/termui"
)

const (
	sidebarWidth  = 34
	actor         = "tui"
	chatsLoad     = 300
	feedLoad      = 200
	feedCap       = 500
	openLimit     = 400
	receiptsMax   = 20
	searchLimit   = 200
	chatsDebounce = 200 * time.Millisecond
)

type focus int

const (
	focusInput focus = iota
	focusChats
	focusMessages
)

type (
	eventMsg struct{ v any }
	chatsMsg struct {
		seq   int
		chats []store.Chat
	}
	chatsStaleMsg  struct{}
	chatsReloadMsg struct{}
	noticeMsg      struct {
		text string
		err  bool
	}
	clearNoticeMsg int
	panelMsg       struct{ title, body string }
	openedMsg      struct {
		seq               int
		jid, name, notice string
		tags              []string
		msgs              []store.Message
		readSeq, jump     int64
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

	chats       []store.Chat
	cursor      int
	filter      string
	chatsSeq    int
	chatsSeen   int
	chatsReload termui.Debounce

	active, activeName string
	activeTags         []string
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

// loadChats reloads the chat list. Answers carry a sequence number so an older list never replaces
// a newer one.
func (m *model) loadChats() tea.Cmd {
	m.chatsSeq++
	seq, b := m.chatsSeq, m.bot
	return func() tea.Msg {
		chats, err := b.Chats(context.Background(), chatsLoad)
		if err != nil {
			return noticeMsg{"load chats: " + err.Error(), true}
		}
		return chatsMsg{seq, chats}
	}
}

// chatsChanged reloads the chat list once per chatsDebounce, however many events ask for it.
func (m *model) chatsChanged() tea.Cmd {
	return m.chatsReload.Schedule(chatsDebounce, chatsReloadMsg{})
}

func (m *model) loadFeed() tea.Cmd {
	return func() tea.Msg {
		msgs, err := m.bot.Store.Messages("", feedLoad)
		if err != nil {
			return noticeMsg{"load activity: " + err.Error(), true}
		}
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
		limit := openLimit
		if jump > 0 {
			limit = max(limit, m.bot.Store.CountFrom(jid, jump)+20)
		}
		msgs, err := m.bot.Store.Messages(jid, limit)
		if err != nil {
			return noticeMsg{"load chat: " + err.Error(), true}
		}
		slices.Reverse(msgs)
		ctx := context.Background()
		opened := openedMsg{seq: seq, jid: jid, name: jid, msgs: msgs, readSeq: m.bot.Store.ReadSeq(jid), jump: jump}
		if !termui.IsPersonChat(jid) {
			opened.name = m.bot.ChatName(ctx, jid, "")
		} else if v, err := m.bot.ContactOf(ctx, jid); err != nil {
			opened.notice = "contact: " + err.Error()
		} else {
			opened.name, opened.tags = v.Display, v.Tags
		}
		return opened
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

	case contactMsg:
		return m, m.applyContact(msg)

	case contactNamesMsg:
		m.contactNames, m.loadingNames = msg, false
		return m, m.suggest()

	case chatsMsg:
		m.applyChats(msg)
		return m, nil

	case chatsStaleMsg:
		return m, m.chatsChanged()

	case chatsReloadMsg:
		m.chatsReload.Done()
		return m, m.loadChats()

	case feedMsg:
		m.feed = safeAll(slices.DeleteFunc(msg, isReaction))
		m.render(true)
		return m, nil

	case openedMsg:
		if msg.seq != m.openSeq {
			return m, nil
		}
		return m, m.applyOpened(msg)

	case eventMsg:
		return m, tea.Batch(m.wait(), m.onEvent(msg.v))
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *model) applyChats(msg chatsMsg) {
	if msg.seq < m.chatsSeen {
		return
	}
	m.chatsSeen = msg.seq
	for i := range msg.chats {
		msg.chats[i].Name, msg.chats[i].LastText = safeLine(msg.chats[i].Name), safeLine(msg.chats[i].LastText)
	}
	keep := m.cursorJID()
	m.chats = msg.chats
	if m.focus == focusChats && keep != "" {
		m.cursor = chatIndex(m.filteredChats(), keep) + 1
	} else {
		m.syncCursor()
	}
	if i := chatIndex(m.chats, m.active); i >= 0 {
		m.activeName = m.chats[i].Name
	}
	m.render(m.sel < 0 && m.vp.AtBottom())
}

func (m *model) applyOpened(msg openedMsg) tea.Cmd {
	if m.editing != nil {
		m.input.SetValue("")
	}
	m.replyTo, m.editing, m.panelTitle = nil, nil, ""
	m.active, m.activeName, m.msgs, m.sel = msg.jid, safeLine(msg.name), safeAll(msg.msgs), -1
	m.activeTags = termui.SafeLines(msg.tags)
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
	if msg.notice != "" {
		return tea.Batch(m.markRead(msg.msgs), m.setNotice(msg.notice, true))
	}
	return m.markRead(msg.msgs)
}

// onEvent applies something the bot published.
func (m *model) onEvent(v any) tea.Cmd {
	switch v := v.(type) {
	case bot.Status:
		m.status = v
		m.render(false)
		if v.LoggedIn {
			return m.chatsChanged()
		}
	case store.ChatRead, bot.NamesChanged:
		return m.chatsChanged()
	case store.ChatsChanged:
		cmds := []tea.Cmd{m.chatsChanged(), m.loadFeed()}
		if m.active != "" {
			cmds = append(cmds, m.open(m.active))
		}
		return tea.Batch(cmds...)
	case store.Message:
		v = safeMessage(v)
		var cmds []tea.Cmd
		if v.Type != "reaction" {
			m.feed = append(m.feed, v)
			if len(m.feed) > feedCap {
				m.feed = m.feed[len(m.feed)-feedCap:]
			}
		}
		if v.Chat == m.active && !slices.ContainsFunc(m.msgs, func(x store.Message) bool { return x.Seq == v.Seq }) {
			m.msgs = append(m.msgs, v)
			cmds = append(cmds, m.markRead([]store.Message{v}))
		}
		m.render(m.sel < 0)
		return tea.Batch(append(cmds, m.chatsChanged())...)
	case store.MessageUpdate:
		replaceMessage(m.msgs, safeMessage(v.Message))
		replaceMessage(m.feed, safeMessage(v.Message))
		m.render(false)
		return m.chatsChanged()
	}
	return nil
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
