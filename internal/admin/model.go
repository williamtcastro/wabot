package admin

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
)

type tab int

const (
	tabStatus tab = iota
	tabTokens
	tabAudit
)

var tabNames = []string{"Status", "Tokens", "Audit"}

type mode int

const (
	modeNormal mode = iota
	modePair
	modeConfirmLogout
	modeConfirmRevoke
	modeCreate
	modeShowToken
	modeFilter
)

const (
	pollEvery = 3 * time.Second
	auditPage = 100
)

type (
	statusMsg struct {
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
		err     error
	}
	createdMsg struct {
		plain string
		err   error
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

type model struct {
	c             *Client
	copy          func(string) error
	width, height int
	tab           tab
	mode          mode

	status    bot.Status
	statusErr error
	checked   time.Time
	latency   time.Duration
	pairCode  string

	tokens    []store.Token
	tokensErr error
	tokSel    int

	audit      []store.AuditEntry
	auditErr   error
	auditSel   int
	auditMore  bool
	auditBusy  bool
	auditQuery string

	prompt    textinput.Model
	form      []textinput.Model
	formFocus int
	created   string

	notice    string
	noticeErr bool
}

// Run opens the panel. copy puts text on the clipboard (used for a newly created token).
func Run(c *Client, copy func(string) error) error {
	_, err := tea.NewProgram(newModel(c, copy), tea.WithAltScreen()).Run()
	return err
}

func newModel(c *Client, copy func(string) error) *model {
	p := textinput.New()
	p.Prompt = "› "
	p.PromptStyle = styleAccent
	return &model{c: c, copy: copy, prompt: p, auditMore: true}
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.loadStatus(), m.loadTokens(), m.loadAudit(false), tick())
}

func tick() tea.Cmd {
	return tea.Tick(pollEvery, func(time.Time) tea.Msg { return tickMsg{} })
}

func call[T any](f func(ctx context.Context) T) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return f(ctx)
	}
}

func (m *model) loadStatus() tea.Cmd {
	return call(func(ctx context.Context) tea.Msg {
		start := time.Now()
		s, err := m.c.Status(ctx)
		return statusMsg{s, time.Since(start), err}
	})
}

func (m *model) loadTokens() tea.Cmd {
	return call(func(ctx context.Context) tea.Msg {
		t, err := m.c.Tokens(ctx)
		return tokensMsg{t, err}
	})
}

func (m *model) loadAudit(older bool) tea.Cmd {
	var before int64
	if older && len(m.audit) > 0 {
		before = m.audit[len(m.audit)-1].ID
	}
	m.auditBusy = true
	return call(func(ctx context.Context) tea.Msg {
		e, err := m.c.Audit(ctx, auditPage, before)
		return auditMsg{e, older, err}
	})
}

func (m *model) setNotice(text string, isErr bool) {
	m.notice, m.noticeErr = text, isErr
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		cmds := []tea.Cmd{m.loadStatus(), tick()}
		if m.mode == modeNormal && m.tab == tabTokens {
			cmds = append(cmds, m.loadTokens())
		}
		if m.mode == modeNormal && m.tab == tabAudit && m.auditSel == 0 && m.auditQuery == "" && !m.auditBusy {
			cmds = append(cmds, m.loadAudit(false))
		}
		return m, tea.Batch(cmds...)

	case statusMsg:
		m.status, m.statusErr, m.checked, m.latency = msg.s, msg.err, time.Now(), msg.latency
		if msg.s.LoggedIn {
			m.pairCode = ""
		}
		return m, nil

	case tokensMsg:
		m.tokens, m.tokensErr = msg.tokens, msg.err
		m.tokSel = clamp(m.tokSel, len(m.tokens))
		return m, nil

	case auditMsg:
		m.auditBusy = false
		m.auditErr = msg.err
		if msg.err != nil {
			return m, nil
		}
		if msg.older {
			m.audit = append(m.audit, msg.entries...)
		} else {
			m.audit = msg.entries
		}
		m.auditMore = len(msg.entries) == auditPage
		m.auditSel = clamp(m.auditSel, len(m.visibleAudit()))
		return m, nil

	case createdMsg:
		if msg.err != nil {
			m.setNotice("create failed: "+msg.err.Error(), true)
			return m, nil
		}
		m.form, m.created, m.mode, m.notice = nil, msg.plain, modeShowToken, ""
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
		return m, tea.Batch(m.loadStatus(), m.loadTokens(), m.loadAudit(false))

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
	case modePair:
		return m.promptKey(k, func(v string) tea.Cmd {
			m.setNotice("asking the server for a pairing code…", false)
			return call(func(ctx context.Context) tea.Msg {
				code, err := m.c.Pair(ctx, v)
				return pairMsg{code, err}
			})
		})
	case modeFilter:
		if k.String() == "esc" {
			m.auditQuery = ""
		}
		cmd := m.promptKeyLive(k)
		m.auditQuery = m.prompt.Value()
		m.auditSel = clamp(m.auditSel, len(m.visibleAudit()))
		return m, cmd
	case modeConfirmLogout:
		m.mode = modeNormal
		if k.String() != "y" {
			m.setNotice("logout cancelled", false)
			return m, nil
		}
		m.setNotice("logging out…", false)
		return m, call(func(ctx context.Context) tea.Msg {
			return doneMsg{"logged out: the session on the server was unlinked", m.c.Logout(ctx)}
		})
	case modeConfirmRevoke:
		m.mode = modeNormal
		if k.String() != "y" || m.tokSel >= len(m.tokens) {
			m.setNotice("revoke cancelled", false)
			return m, nil
		}
		name := m.tokens[m.tokSel].Name
		return m, call(func(ctx context.Context) tea.Msg {
			n, err := m.c.RevokeToken(ctx, name)
			return doneMsg{"revoked " + name + " (" + plural(n, "token") + ")", err}
		})
	case modeCreate:
		return m.formKey(k)
	case modeShowToken:
		if k.String() == "c" {
			plain := m.created
			return m, func() tea.Msg {
				if err := m.copy(plain); err != nil {
					return noticeMsg{"copy failed: " + err.Error(), true}
				}
				return noticeMsg{"token copied to the clipboard", false}
			}
		}
		m.created, m.mode = "", modeNormal
		m.setNotice("token closed; it can't be shown again", false)
		return m, nil
	}

	switch k.String() {
	case "q":
		return m, tea.Quit
	case "tab", "l", "right":
		m.tab = (m.tab + 1) % tab(len(tabNames))
	case "shift+tab", "h", "left":
		m.tab = (m.tab + tab(len(tabNames)) - 1) % tab(len(tabNames))
	case "1", "2", "3":
		m.tab = tab(k.String()[0] - '1')
	case "r":
		m.setNotice("refreshing…", false)
		return m, tea.Batch(m.loadStatus(), m.loadTokens(), m.loadAudit(false))
	default:
		return m, m.tabKey(k.String())
	}
	m.notice = ""
	return m, nil
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
	case tabTokens:
		switch k {
		case "j", "down":
			m.tokSel = clamp(m.tokSel+1, len(m.tokens))
		case "k", "up":
			m.tokSel = clamp(m.tokSel-1, len(m.tokens))
		case "n":
			m.startForm()
		case "x", "d":
			if m.tokSel >= len(m.tokens) {
				return nil
			}
			if m.tokens[m.tokSel].Revoked {
				m.setNotice(m.tokens[m.tokSel].Name+" is already revoked", true)
				return nil
			}
			m.mode = modeConfirmRevoke
		}
	case tabAudit:
		n := len(m.visibleAudit())
		switch k {
		case "j", "down":
			m.auditSel = clamp(m.auditSel+1, n)
		case "k", "up":
			m.auditSel = clamp(m.auditSel-1, n)
		case "ctrl+d", "pgdown":
			m.auditSel = clamp(m.auditSel+m.bodyHeight()/2, n)
		case "ctrl+u", "pgup":
			m.auditSel = clamp(m.auditSel-m.bodyHeight()/2, n)
		case "g", "home":
			m.auditSel = 0
		case "G", "end":
			m.auditSel = clamp(n-1, n)
		case "/":
			m.startPrompt(modeFilter, "filter by actor, action, target, detail or error")
			m.prompt.SetValue(m.auditQuery)
			m.prompt.CursorEnd()
			return nil
		case "esc":
			m.auditQuery = ""
			return nil
		}
		if m.auditSel >= n-1 && m.auditMore && !m.auditBusy && m.auditQuery == "" && n > 0 {
			return m.loadAudit(true)
		}
	}
	return nil
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

func (m *model) promptKeyLive(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "esc", "enter":
		m.mode = modeNormal
		return nil
	}
	var cmd tea.Cmd
	m.prompt, cmd = m.prompt.Update(k)
	return cmd
}

var formFields = []struct{ label, value, hint string }{
	{"Name", "", "a unique name, e.g. support-bot"},
	{"Scopes", "read", "read, send and/or admin"},
	{"Chats", "", "optional allowlist: numbers or JIDs, comma-separated"},
	{"Rate", "10", "sends per minute for this token, 0 = only the global limit"},
	{"Expires", "720h", "optional, e.g. 24h or 720h; empty = never"},
}

func (m *model) startForm() {
	m.form = make([]textinput.Model, len(formFields))
	for i, f := range formFields {
		in := textinput.New()
		in.Prompt = ""
		in.Placeholder = f.hint
		in.SetValue(f.value)
		m.form[i] = in
	}
	m.formFocus = 0
	m.form[0].Focus()
	m.mode = modeCreate
	m.notice = ""
}

func (m *model) focusField(i int) {
	m.form[m.formFocus].Blur()
	m.formFocus = (i + len(m.form)) % len(m.form)
	m.form[m.formFocus].Focus()
	m.form[m.formFocus].CursorEnd()
}

func (m *model) formKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc":
		m.form, m.mode = nil, modeNormal
		m.setNotice("token not created", false)
		return m, nil
	case "tab", "down":
		m.focusField(m.formFocus + 1)
		return m, nil
	case "shift+tab", "up":
		m.focusField(m.formFocus - 1)
		return m, nil
	case "enter":
		if m.formFocus < len(m.form)-1 {
			m.focusField(m.formFocus + 1)
			return m, nil
		}
		req, err := m.formRequest()
		if err != nil {
			m.setNotice(err.Error(), true)
			return m, nil
		}
		m.setNotice("creating "+req.Name+"…", false)
		return m, call(func(ctx context.Context) tea.Msg {
			plain, _, err := m.c.CreateToken(ctx, req)
			return createdMsg{plain, err}
		})
	}
	var cmd tea.Cmd
	m.form[m.formFocus], cmd = m.form[m.formFocus].Update(k)
	return m, cmd
}

type formError string

func (e formError) Error() string { return string(e) }

func (m *model) formRequest() (TokenRequest, error) {
	v := func(i int) string { return strings.TrimSpace(m.form[i].Value()) }
	r := TokenRequest{Name: v(0), Scopes: splitList(v(1)), Chats: splitList(v(2)), ExpiresIn: v(4)}
	if r.Name == "" {
		return r, formError("the token needs a name")
	}
	if len(r.Scopes) == 0 {
		return r, formError("pick at least one scope: read, send or admin")
	}
	if s := v(3); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return r, formError("rate must be a whole number, 0 or more")
		}
		r.Rate = &n
	}
	return r, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		out = append(out, p)
	}
	return out
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

func clamp(i, n int) int {
	if i >= n {
		i = n - 1
	}
	return max(i, 0)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
