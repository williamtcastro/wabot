package admin

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/williamtcastro/wame/internal/api"
	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
	"github.com/williamtcastro/wame/internal/termui"
)

type tab int

const (
	tabStatus tab = iota
	tabChats
	tabTokens
	tabAudit
	tabHealth
	tabSettings
	tabQueue
)

var tabNames = []string{"Status", "Chats", "Tokens", "Audit", "Health", "Settings", "Queue"}

type mode int

const (
	modeNormal mode = iota
	modePair
	modeConfirmLogout
	modeConfirmRevoke
	modeConfirmRotate
	modeCreate
	modeShowToken
	modeFilter
	modeSearch
	modeCompose
	modeServers
	modeSetting
	modeConfirmCancelBatch
)

const (
	pollFast      = 3 * time.Second
	pollLive      = 15 * time.Second
	auditPage     = 100
	chatsLimit    = 100
	messagesLimit = 60
	unusedAfter   = 30 * 24 * time.Hour
	callTimeout   = 30 * time.Second
)

type (
	// genMsg tags a server answer with the client that produced it, so answers from a server the
	// user has since switched away from are dropped.
	genMsg struct {
		gen int
		msg tea.Msg
	}
	statusMsg struct {
		seq     int
		s       bot.Status
		latency time.Duration
		err     error
	}
	tokensMsg struct {
		tokens []store.Token
		err    error
	}
	auditMsg struct {
		entries []store.AuditEntry
		older   bool
		before  int64
		err     error
	}
	statsMsg struct {
		seq int
		s   api.Stats
		err error
	}
	chatsMsg struct {
		chats []store.Chat
		err   error
	}
	messagesMsg struct {
		chat, query string
		msgs        []store.Message
		err         error
	}
	sentMsg struct {
		chat string
		err  error
	}
	createdMsg struct {
		plain, replaced string
		revokeErr       error
		err             error
	}
	pairMsg struct {
		code string
		err  error
	}
	doneMsg struct {
		notice string
		err    error
	}
	noticeMsg struct {
		text string
		err  bool
	}
	tickMsg struct{}
)

// Options configure the panel. Without a Client it opens on the server picker.
type Options struct {
	Client   *Client
	Profile  string
	Profiles []Profile
	Insecure bool
	Copy     func(string) error
	Bell     func()
}

type model struct {
	c             *Client
	gen           int
	opts          Options
	profile       string
	width, height int
	tab           tab
	mode          mode

	status    bot.Status
	statusReq inflight
	statusErr error
	checked   time.Time
	latency   time.Duration
	pairCode  string

	tokens     []store.Token
	tokensErr  error
	tokSel     int
	confirmTok store.Token

	audit      []store.AuditEntry
	auditErr   error
	auditSel   int
	auditMore  bool
	auditBusy  bool
	auditQuery string

	stats     api.Stats
	statsReq  inflight
	statsErr  error
	statsAt   time.Time
	healthSel int

	settings    []bot.SettingView
	settingsErr error
	setSel      int
	editKey     string

	queue        QueuePage
	queueErr     error
	queueSel     int
	queueStatus  string
	queueReload  termui.Debounce
	confirmBatch string

	chats       []store.Chat
	chatsReload termui.Debounce
	chatsErr    error
	chatSel     int
	msgs        []store.Message
	msgsChat    string
	msgsErr     error
	search      string
	searchHits  []store.Message

	live live

	prompt    textinput.Model
	form      []textinput.Model
	formFocus int
	created   string
	replaced  string
	serverSel int

	notice    string
	noticeErr bool
}

// Run opens the panel.
func Run(o Options) error {
	m := newModel(o)
	defer m.close()
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

func newModel(o Options) *model {
	p := textinput.New()
	p.Prompt = "› "
	p.PromptStyle = styleAccent
	p.CharLimit = 4096
	if o.Copy == nil {
		o.Copy = func(string) error { return nil }
	}
	if o.Bell == nil {
		o.Bell = func() {}
	}
	m := &model{c: o.Client, opts: o, profile: o.Profile, prompt: p, auditMore: true}
	if m.c == nil {
		m.mode = modeServers
	}
	return m
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.loadAll(), m.live.start(m.c, m.gen), tick(m.pollEvery()))
}

func (m *model) close() { m.live.stop() }

func (m *model) loadAll() tea.Cmd {
	if m.c == nil {
		return nil
	}
	return tea.Batch(m.loadStatus(), m.loadTokens(), m.loadAudit(false), m.loadStats(), m.loadChats(), m.loadSettings(), m.loadQueue())
}

func (m *model) pollEvery() time.Duration {
	if m.live.on {
		return pollLive
	}
	return pollFast
}

func tick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return tickMsg{} })
}

// inflight tracks one kind of polled request: the tick skips it while one is pending, and an answer
// older than one already applied is dropped, so a slow request can't undo a newer answer.
type inflight struct{ started, applied, pending int }

func (f *inflight) start() int {
	f.started++
	f.pending++
	return f.started
}

func (f *inflight) busy() bool { return f.pending > 0 }

// done reports whether the answer to request seq is the newest so far.
func (f *inflight) done(seq int) bool {
	f.pending = max(f.pending-1, 0)
	if seq < f.applied {
		return false
	}
	f.applied = seq
	return true
}

// call runs f against the current client in the background, tagged with its generation.
func (m *model) call(f func(ctx context.Context, c *Client) tea.Msg) tea.Cmd {
	c, gen := m.c, m.gen
	if c == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		return genMsg{gen, f(ctx, c)}
	}
}

func (m *model) loadStatus() tea.Cmd {
	if m.c == nil {
		return nil
	}
	seq := m.statusReq.start()
	return m.call(func(ctx context.Context, c *Client) tea.Msg {
		start := time.Now()
		s, err := c.Status(ctx)
		return statusMsg{seq, s, time.Since(start), err}
	})
}

func (m *model) loadTokens() tea.Cmd {
	return m.call(func(ctx context.Context, c *Client) tea.Msg {
		t, err := c.Tokens(ctx)
		return tokensMsg{t, err}
	})
}

func (m *model) loadAudit(older bool) tea.Cmd {
	var before int64
	if older && len(m.audit) > 0 {
		before = m.audit[len(m.audit)-1].ID
	}
	m.auditBusy = true
	return m.call(func(ctx context.Context, c *Client) tea.Msg {
		e, err := c.Audit(ctx, auditPage, before)
		return auditMsg{e, older, before, err}
	})
}

func (m *model) loadStats() tea.Cmd {
	if m.c == nil {
		return nil
	}
	seq := m.statsReq.start()
	return m.call(func(ctx context.Context, c *Client) tea.Msg {
		s, err := c.Stats(ctx)
		return statsMsg{seq, s, err}
	})
}

// setNotice shows a line above the keys. It's sanitized: it often ends in an error from the server
// or from a webhook receiver.
func (m *model) setNotice(text string, isErr bool) {
	m.notice, m.noticeErr = safe(text), isErr
}

func (m *model) healthy() bool {
	return m.statusErr == nil && !m.checked.IsZero() && m.status.Connected && m.status.LoggedIn
}

// applyStatus records a new status and rings the bell when a healthy server stops being healthy.
func (m *model) applyStatus(s bot.Status, err error) {
	was := m.healthy()
	if err == nil {
		m.status = s
	}
	m.statusErr, m.checked = err, time.Now()
	if s.LoggedIn {
		m.pairCode = ""
	}
	if was && !m.healthy() {
		m.opts.Bell()
		switch {
		case err != nil:
			m.setNotice("lost contact with the server: "+err.Error(), true)
		case !s.LoggedIn:
			m.setNotice("WhatsApp logged this server out", true)
		default:
			m.setNotice("the server lost its WhatsApp connection", true)
		}
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case genMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		return m.Update(msg.msg)

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		cmds := []tea.Cmd{tick(m.pollEvery())}
		if !m.statusReq.busy() {
			cmds = append(cmds, m.loadStatus())
		}
		if !m.statsReq.busy() {
			cmds = append(cmds, m.loadStats())
		}
		if m.mode == modeNormal && m.tab == tabTokens {
			cmds = append(cmds, m.loadTokens())
		}
		if m.mode == modeNormal && m.tab == tabAudit && m.auditSel == 0 && m.auditQuery == "" && !m.auditBusy {
			cmds = append(cmds, m.loadAudit(false))
		}
		if m.tab == tabChats && !m.live.on {
			cmds = append(cmds, m.loadChats())
		}
		if m.tab == tabQueue {
			cmds = append(cmds, m.loadQueue())
		}
		return m, tea.Batch(cmds...)

	case liveMsg, eventMsg:
		return m, m.handleLive(msg)

	case statusMsg:
		if !m.statusReq.done(msg.seq) {
			return m, nil
		}
		m.applyStatus(msg.s, msg.err)
		if msg.err == nil {
			m.latency = msg.latency
		}
		return m, nil

	case tokensMsg:
		m.applyTokens(msg)
		return m, nil

	case auditMsg:
		m.applyAudit(msg)
		return m, nil

	case statsMsg:
		if !m.statsReq.done(msg.seq) {
			return m, nil
		}
		m.statsErr = msg.err
		if msg.err == nil {
			m.stats, m.statsAt = msg.s, time.Now()
			if w := m.stats.Webhook; w != nil {
				m.healthSel = clamp(m.healthSel, len(w.Recent))
			}
		}
		return m, nil

	case queueMsg:
		m.applyQueue(msg)
		return m, nil

	case queueReloadMsg:
		return m, m.reloadQueue()

	case chatsReloadMsg:
		m.chatsReload.Done()
		if m.tab != tabChats {
			return m, nil
		}
		return m, m.loadChats()

	case settingsMsg:
		return m, m.applySettingsMsg(msg)

	case webhookMsg:
		return m, m.applyWebhookMsg(msg)

	case chatsMsg:
		return m, m.applyChats(msg)

	case messagesMsg:
		m.applyMessages(msg)
		return m, nil

	case sentMsg:
		if msg.err != nil {
			m.setNotice("not sent: "+msg.err.Error(), true)
			return m, nil
		}
		m.setNotice("sent", false)
		return m, tea.Batch(m.loadMessages(msg.chat), m.loadChats(), m.loadAudit(false))

	case createdMsg:
		if msg.err != nil {
			m.setNotice("create failed: "+msg.err.Error(), true)
			return m, nil
		}
		m.form, m.created, m.replaced, m.mode, m.notice = nil, msg.plain, msg.replaced, modeShowToken, ""
		if msg.revokeErr != nil {
			m.setNotice("the new token works, but revoking "+msg.replaced+" failed: "+msg.revokeErr.Error(), true)
		}
		return m, tea.Batch(m.loadTokens(), m.loadAudit(false))

	case pairMsg:
		if msg.err != nil {
			m.setNotice("pairing failed: "+msg.err.Error(), true)
			return m, nil
		}
		m.pairCode = msg.code
		m.setNotice("enter the code on your phone: WhatsApp → Linked devices → Link with phone number", false)
		return m, m.loadAudit(false)

	case doneMsg:
		if msg.err != nil {
			m.setNotice(msg.err.Error(), true)
		} else {
			m.setNotice(msg.notice, false)
		}
		return m, tea.Batch(m.loadStatus(), m.loadTokens(), m.loadAudit(false), m.loadQueue(), m.loadStats())

	case noticeMsg:
		m.setNotice(msg.text, msg.err)
		return m, nil

	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m *model) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if k.String() == "ctrl+c" {
		return m, tea.Quit
	}
	switch m.mode {
	case modeServers:
		return m.serversKey(k)
	case modeSetting:
		return m.settingPromptKey(k)
	case modePair:
		return m.promptKey(k, func(v string) tea.Cmd {
			m.setNotice("asking the server for a pairing code…", false)
			return m.call(func(ctx context.Context, c *Client) tea.Msg {
				code, err := c.Pair(ctx, v)
				return pairMsg{code, err}
			})
		})
	case modeCompose:
		chat := m.msgsChat
		return m.promptKey(k, func(v string) tea.Cmd {
			m.setNotice("sending…", false)
			key := "admin-" + strconv.FormatInt(time.Now().UnixNano(), 36)
			return m.call(func(ctx context.Context, c *Client) tea.Msg {
				_, err := c.Send(ctx, chat, v, key)
				return sentMsg{chat, err}
			})
		})
	case modeSearch:
		return m.promptKey(k, func(v string) tea.Cmd {
			m.search = v
			m.setNotice("searching…", false)
			return m.call(func(ctx context.Context, c *Client) tea.Msg {
				msgs, err := c.Messages(ctx, "", v, messagesLimit)
				return messagesMsg{query: v, msgs: msgs, err: err}
			})
		})
	case modeFilter:
		return m, m.auditFilterKey(k)
	case modeConfirmLogout, modeConfirmRevoke, modeConfirmRotate, modeConfirmCancelBatch:
		return m, m.confirmKey(k.String() == "y")
	case modeCreate:
		return m.formKey(k)
	case modeShowToken:
		if k.String() == "c" {
			plain, copyFn := m.created, m.opts.Copy
			return m, func() tea.Msg {
				if err := copyFn(plain); err != nil {
					return noticeMsg{"copy failed: " + err.Error(), true}
				}
				return noticeMsg{"token copied to the clipboard", false}
			}
		}
		m.created, m.replaced, m.mode = "", "", modeNormal
		if !m.noticeErr {
			m.setNotice("token closed; it can't be shown again", false)
		}
		return m, nil
	}

	if n, err := strconv.Atoi(k.String()); err == nil && n >= 1 && n <= len(tabNames) {
		return m, m.setTab(tab(n - 1))
	}
	switch k.String() {
	case "q":
		return m, tea.Quit
	case "tab", "right":
		return m, m.setTab((m.tab + 1) % tab(len(tabNames)))
	case "shift+tab", "left":
		return m, m.setTab((m.tab + tab(len(tabNames)) - 1) % tab(len(tabNames)))
	case "r":
		m.setNotice("refreshing…", false)
		return m, m.loadAll()
	case "S":
		if len(m.opts.Profiles) == 0 {
			m.setNotice("no saved servers: add files to ~/.config/wame/servers/ (see docs/ADMIN.md)", true)
			return m, nil
		}
		m.mode = modeServers
		m.serverSel = 0
		for i, p := range m.opts.Profiles {
			if p.Name == m.profile {
				m.serverSel = i
			}
		}
		return m, nil
	}
	return m, m.tabKey(k.String())
}

// confirmKey answers a confirmation dialog. The token or batch it acts on was captured when the
// dialog opened, so lists reloading underneath can't change it.
func (m *model) confirmKey(yes bool) tea.Cmd {
	md := m.mode
	m.mode = modeNormal
	switch md {
	case modeConfirmLogout:
		if !yes {
			m.setNotice("logout cancelled", false)
			return nil
		}
		m.setNotice("logging out…", false)
		return m.call(func(ctx context.Context, c *Client) tea.Msg {
			return doneMsg{"logged out: the session on the server was unlinked", c.Logout(ctx)}
		})
	case modeConfirmRevoke:
		if !yes {
			m.setNotice("revoke cancelled", false)
			return nil
		}
		name := m.confirmTok.Name
		return m.call(func(ctx context.Context, c *Client) tea.Msg {
			n, err := c.RevokeToken(ctx, name)
			return doneMsg{"revoked " + name + " (" + plural(n, "token") + ")", err}
		})
	case modeConfirmRotate:
		if !yes {
			m.setNotice("rotation cancelled", false)
			return nil
		}
		return m.rotate(m.confirmTok)
	case modeConfirmCancelBatch:
		if !yes {
			m.setNotice("nothing canceled", false)
			return nil
		}
		return m.cancelQueued(0, m.confirmBatch, "batch "+m.confirmBatch)
	}
	return nil
}

func (m *model) settingPromptKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc":
		m.mode = modeNormal
		return m, nil
	case "enter":
		m.mode = modeNormal
		v := strings.TrimSpace(m.prompt.Value())
		label, _ := settingLabel(m.editKey)
		return m, m.changeSetting(m.editKey, &v, label+" set to "+cmp.Or(v, "(none)"))
	}
	var cmd tea.Cmd
	m.prompt, cmd = m.prompt.Update(k)
	return m, cmd
}

func (m *model) applyTokens(msg tokensMsg) {
	m.tokensErr = msg.err
	if msg.err != nil {
		return
	}
	var selected string
	if m.tokSel < len(m.tokens) {
		selected = m.tokens[m.tokSel].Name
	}
	m.tokens = msg.tokens
	if i := slices.IndexFunc(m.tokens, func(t store.Token) bool { return t.Name == selected }); i >= 0 {
		m.tokSel = i
	}
	m.tokSel = clamp(m.tokSel, len(m.tokens))
}

func (m *model) setTab(t tab) tea.Cmd {
	m.tab, m.notice = t, ""
	switch t {
	case tabChats:
		return m.loadChats()
	case tabHealth:
		return m.loadStats()
	case tabTokens:
		return tea.Batch(m.loadTokens(), m.loadStats())
	case tabSettings:
		return m.loadSettings()
	case tabQueue:
		return tea.Batch(m.loadQueue(), m.loadStats())
	}
	return nil
}

func (m *model) tabKey(k string) tea.Cmd {
	switch m.tab {
	case tabStatus:
		switch k {
		case "p":
			if m.status.LoggedIn {
				m.setNotice("already linked; log out first to pair again", true)
				return nil
			}
			m.startPrompt(modePair, "phone number with country code, e.g. +5511999999999")
		case "L":
			if !m.status.LoggedIn {
				m.setNotice("not logged in", true)
				return nil
			}
			m.mode = modeConfirmLogout
		}
	case tabChats:
		return m.chatsKey(k)
	case tabTokens:
		return m.tokensKey(k)
	case tabHealth:
		return m.healthKey(k)
	case tabSettings:
		return m.settingsKey(k)
	case tabQueue:
		return m.queueKey(k)
	case tabAudit:
		return m.auditKey(k)
	}
	return nil
}

// sanitizeAudit cleans what a token holder controls in audit entries (actor names, targets, file
// names in details, error text) once, as they arrive.
func sanitizeAudit(entries []store.AuditEntry) {
	for i := range entries {
		e := &entries[i]
		e.Actor, e.Action, e.Target, e.Detail, e.Error = safe(e.Actor), safe(e.Action), safe(e.Target), safe(e.Detail), safe(e.Error)
	}
}

// applyAudit takes a page of the audit log. An older page is only appended where it was asked for:
// after a refresh the list ends elsewhere and the page is dropped.
func (m *model) applyAudit(msg auditMsg) {
	m.auditBusy = false
	if msg.older && msg.err == nil && (len(m.audit) == 0 || m.audit[len(m.audit)-1].ID != msg.before) {
		return
	}
	m.auditErr = msg.err
	if msg.err != nil {
		return
	}
	sanitizeAudit(msg.entries)
	if msg.older {
		m.audit = append(m.audit, msg.entries...)
	} else {
		m.audit = msg.entries
	}
	m.auditMore = len(msg.entries) == auditPage
	m.auditSel = clamp(m.auditSel, len(m.visibleAudit()))
}

func (m *model) auditKey(k string) tea.Cmd {
	n := len(m.visibleAudit())
	if sel, ok := termui.ListMove(k, m.auditSel, n, m.bodyHeight()/2); ok {
		m.auditSel = sel
		if m.auditSel >= n-1 && m.auditMore && !m.auditBusy && m.auditQuery == "" && n > 0 {
			return m.loadAudit(true)
		}
		return nil
	}
	switch k {
	case "/":
		m.startPrompt(modeFilter, "filter by actor, action, target, detail or error")
		m.prompt.SetValue(m.auditQuery)
		m.prompt.CursorEnd()
	case "esc":
		m.auditQuery = ""
	}
	return nil
}

// auditFilterKey edits the audit filter as it's typed; esc clears it, enter keeps it.
func (m *model) auditFilterKey(k tea.KeyMsg) tea.Cmd {
	var cmd tea.Cmd
	switch k.String() {
	case "esc":
		m.mode = modeNormal
		m.auditQuery = ""
	case "enter":
		m.mode = modeNormal
	default:
		m.prompt, cmd = m.prompt.Update(k)
		m.auditQuery = m.prompt.Value()
	}
	m.auditSel = clamp(m.auditSel, len(m.visibleAudit()))
	return cmd
}

func (m *model) serversKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := len(m.opts.Profiles)
	if sel, ok := termui.ListMove(k.String(), m.serverSel, n, n); ok {
		m.serverSel = sel
		return m, nil
	}
	switch k.String() {
	case "esc", "q":
		if m.c == nil {
			return m, tea.Quit
		}
		m.mode = modeNormal
	case "enter":
		if m.serverSel < n {
			return m, m.switchTo(m.opts.Profiles[m.serverSel])
		}
	}
	return m, nil
}

// switchTo points the panel at another saved server, dropping everything loaded from the old one.
func (m *model) switchTo(p Profile) tea.Cmd {
	c, err := NewClient(p.Server, p.Token, p.Insecure || m.opts.Insecure)
	if err != nil {
		m.setNotice(p.Name+": "+err.Error(), true)
		return nil
	}
	m.live.stop()
	fresh := newModel(m.opts)
	fresh.c, fresh.profile, fresh.gen = c, p.Name, m.gen+1
	fresh.width, fresh.height, fresh.tab = m.width, m.height, m.tab
	fresh.mode = modeNormal
	*m = *fresh
	m.setNotice("switched to "+p.Name, false)
	return tea.Batch(m.loadAll(), m.live.start(m.c, m.gen))
}

func (m *model) startPrompt(md mode, placeholder string) {
	m.mode = md
	m.prompt.Reset()
	m.prompt.Placeholder = placeholder
	m.prompt.Focus()
}

func (m *model) promptKey(k tea.KeyMsg, submit func(string) tea.Cmd) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc":
		m.mode = modeNormal
		return m, nil
	case "enter":
		m.mode = modeNormal
		v := strings.TrimSpace(m.prompt.Value())
		if v == "" {
			return m, nil
		}
		return m, submit(v)
	}
	var cmd tea.Cmd
	m.prompt, cmd = m.prompt.Update(k)
	return m, cmd
}

func (m *model) visibleAudit() []store.AuditEntry {
	q := strings.ToLower(m.auditQuery)
	if q == "" {
		return m.audit
	}
	var out []store.AuditEntry
	for _, e := range m.audit {
		hay := strings.ToLower(strings.Join([]string{e.Actor, e.Action, e.Target, e.Detail, e.Error}, " "))
		if strings.Contains(hay, q) {
			out = append(out, e)
		}
	}
	return out
}

func decode[T any](data json.RawMessage) (T, bool) {
	var v T
	return v, json.Unmarshal(data, &v) == nil
}

func clamp(i, n int) int {
	if i >= n {
		i = n - 1
	}
	return max(i, 0)
}

// row renders a table row, highlighted when it's the selected one.
func (m *model) row(line string, selected bool) string {
	if selected {
		return m.selectedRow(line)
	}
	return "   " + line
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
