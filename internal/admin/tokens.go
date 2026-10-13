package admin

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/williamtcastro/wame/internal/store"
	"github.com/williamtcastro/wame/internal/termui"
)

func (m *model) tokensKey(k string) tea.Cmd {
	if sel, ok := termui.ListMove(k, m.tokSel, len(m.tokens), m.bodyHeight()/2); ok {
		m.tokSel = sel
		return nil
	}
	switch k {
	case "n":
		m.startForm()
	case "x", "d", "R":
		if m.tokSel >= len(m.tokens) {
			return nil
		}
		t := m.tokens[m.tokSel]
		if t.Revoked {
			m.setNotice(t.Name+" is already revoked", true)
			return nil
		}
		m.confirmTok = t
		if k == "R" {
			m.mode = modeConfirmRotate
		} else {
			m.mode = modeConfirmRevoke
		}
	}
	return nil
}

// unused reports tokens nobody has used for a while: a candidate for revoking.
func unused(t store.Token) bool {
	if !tokenLive(t) {
		return false
	}
	last := t.LastUsed
	if last.IsZero() {
		last = t.Created
	}
	return time.Since(last) > unusedAfter
}

var rotatedSuffix = regexp.MustCompile(`-(\d+)$`)

// nextName picks name-2, name-3… for a rotated token. Revoked names stay reserved, so every
// existing name counts.
func nextName(name string, tokens []store.Token) string {
	base := rotatedSuffix.ReplaceAllString(name, "")
	n := 1
	for _, t := range tokens {
		if t.Name == base {
			continue
		}
		if rest, ok := strings.CutPrefix(t.Name, base+"-"); ok {
			if i, err := strconv.Atoi(rest); err == nil {
				n = max(n, i)
			}
		}
	}
	return fmt.Sprintf("%s-%d", base, n+1)
}

// rotationRequest copies a token's settings. The new token gets the same lifetime the old one was
// created with, counted from now.
func rotationRequest(t store.Token, tokens []store.Token) TokenRequest {
	rate := t.Rate
	r := TokenRequest{Name: nextName(t.Name, tokens), Scopes: t.Scopes, Chats: t.Chats, Rate: &rate}
	if !t.Expires.IsZero() {
		hours := max(int(t.Expires.Sub(t.Created).Round(time.Hour).Hours()), 1)
		r.ExpiresIn = fmt.Sprintf("%dh", hours)
	}
	return r
}

// rotate creates the replacement first and revokes the old token only once that worked, so a
// failure never leaves the client without a working token.
func (m *model) rotate(old store.Token) tea.Cmd {
	req := rotationRequest(old, m.tokens)
	m.setNotice("creating "+req.Name+"…", false)
	return m.call(func(ctx context.Context, c *Client) tea.Msg {
		plain, _, err := c.CreateToken(ctx, req)
		if err != nil {
			return createdMsg{err: err}
		}
		_, revokeErr := c.RevokeToken(ctx, old.Name)
		return createdMsg{plain: plain, replaced: old.Name, revokeErr: revokeErr}
	})
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
		return m, m.call(func(ctx context.Context, c *Client) tea.Msg {
			plain, _, err := c.CreateToken(ctx, req)
			return createdMsg{plain: plain, err: err}
		})
	}
	var cmd tea.Cmd
	m.form[m.formFocus], cmd = m.form[m.formFocus].Update(k)
	return m, cmd
}

func (m *model) formRequest() (TokenRequest, error) {
	v := func(i int) string { return strings.TrimSpace(m.form[i].Value()) }
	r := TokenRequest{Name: v(0), Scopes: splitList(v(1)), Chats: splitList(v(2)), ExpiresIn: v(4)}
	if r.Name == "" {
		return r, errors.New("the token needs a name")
	}
	if len(r.Scopes) == 0 {
		return r, errors.New("pick at least one scope: read, send or admin")
	}
	if s := v(3); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return r, errors.New("rate must be a whole number, 0 or more")
		}
		r.Rate = &n
	}
	return r, nil
}

func splitList(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
}
