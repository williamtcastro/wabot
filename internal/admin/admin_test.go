package admin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/williamtcastro/wame/internal/api"
	"github.com/williamtcastro/wame/internal/auth"
	"github.com/williamtcastro/wame/internal/bot"
)

const owner = "owner-secret-for-tests"

func newServer(t *testing.T) (*bot.Bot, *Client) {
	t.Helper()
	b, err := bot.New(context.Background(), filepath.Join(t.TempDir(), "wame.db"), waLog.Noop, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.InitSettings(bot.Settings{Rate: 20, ChatGap: time.Second, MaxWait: 30 * time.Second, AutoReply: true}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.NewHandler(b, &auth.Authenticator{Store: b.Store, Owner: owner}, "test"))
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, owner, false)
	if err != nil {
		t.Fatal(err)
	}
	return b, c
}

func TestNewClientGuardsTheToken(t *testing.T) {
	for _, c := range []struct {
		server, token string
		insecure      bool
		ok            bool
	}{
		{"https://wame.example.com", "t", false, true},
		{"wame.example.com:8443", "t", false, true},
		{"http://127.0.0.1:8080", "t", false, true},
		{"http://localhost:8080", "t", false, true},
		{"http://[::1]:8080", "t", false, true},
		{"http://10.0.0.5:8080", "t", false, false},
		{"http://10.0.0.5:8080", "t", true, true},
		{"ftp://wame.example.com", "t", false, false},
		{"https://wame.example.com", "", false, false},
	} {
		_, err := NewClient(c.server, c.token, c.insecure)
		if (err == nil) != c.ok {
			t.Errorf("NewClient(%q, insecure=%v): err = %v, want ok=%v", c.server, c.insecure, err, c.ok)
		}
	}
	// wame queue uses this client with a send-scoped token, so the hint can't ask for an admin one.
	_, err := NewClient("https://wame.example.com", "", false)
	if msg := fmt.Sprint(err); !strings.Contains(msg, "-token") || !strings.Contains(msg, "WAME_TOKEN") || strings.Contains(msg, "admin token") || strings.Contains(msg, "owner secret") {
		t.Fatalf("token hint: %q", msg)
	}
}

func TestClientAgainstTheAPI(t *testing.T) {
	_, c := newServer(t)
	ctx := context.Background()

	s, err := c.Status(ctx)
	if err != nil || s.State == "" {
		t.Fatalf("status = %+v, %v", s, err)
	}

	rate := 5
	plain, tok, err := c.CreateToken(ctx, TokenRequest{Name: "support-bot", Scopes: []string{"read", "send"}, Chats: []string{"5511999999999"}, Rate: &rate, ExpiresIn: "24h"})
	if err != nil || !strings.HasPrefix(plain, auth.Prefix) || tok.Name != "support-bot" {
		t.Fatalf("create = %q, %+v, %v", plain, tok, err)
	}
	if _, _, err := c.CreateToken(ctx, TokenRequest{Name: "support-bot", Scopes: []string{"read"}}); err == nil {
		t.Fatal("duplicate name was accepted")
	}

	tokens, err := c.Tokens(ctx)
	if err != nil || len(tokens) != 1 || tokens[0].Chats[0] != "5511999999999@s.whatsapp.net" || tokens[0].Rate != 5 {
		t.Fatalf("tokens = %+v, %v", tokens, err)
	}

	n, err := c.RevokeToken(ctx, "support-bot")
	if err != nil || n != 1 {
		t.Fatalf("revoke = %d, %v", n, err)
	}
	var apiErr *APIError
	if _, err := c.RevokeToken(ctx, "nobody"); !errors.As(err, &apiErr) || apiErr.Code != http.StatusNotFound {
		t.Fatalf("revoking an unknown token: %v", err)
	}

	entries, err := c.Audit(ctx, 10, 0)
	if err != nil || len(entries) < 3 || entries[0].Action != "token_revoke" {
		t.Fatalf("audit = %+v, %v", entries, err)
	}
	older, err := c.Audit(ctx, 10, entries[0].ID)
	if err != nil || len(older) != len(entries)-1 {
		t.Fatalf("paging: %d entries before #%d, want %d (%v)", len(older), entries[0].ID, len(entries)-1, err)
	}
}

func TestClientExplainsAuthErrors(t *testing.T) {
	b, c := newServer(t)
	ctx := context.Background()

	bad, _ := NewClient(c.Server(), "wrong-secret-of-enough-length", false)
	if _, err := bad.Tokens(ctx); err == nil || !strings.Contains(err.Error(), "rejected the token") {
		t.Fatalf("wrong token: %v", err)
	}

	plain, _, err := auth.Issue(b.Store, auth.Request{Name: "reader", Scopes: []auth.Scope{auth.Read}})
	if err != nil {
		t.Fatal(err)
	}
	reader, _ := NewClient(c.Server(), plain, false)
	if _, err := reader.Status(ctx); err != nil {
		t.Fatalf("a read token can see the status: %v", err)
	}
	if _, err := reader.Tokens(ctx); err == nil || !strings.Contains(err.Error(), "admin scope") {
		t.Fatalf("read token listing tokens: %v", err)
	}
}

// run feeds a command's messages back into the model. Timers (cursor blink, the poll tick) are
// dropped: anything that doesn't answer within 100 ms is one.
func run(m *model, cmd tea.Cmd) {
	for cmd != nil {
		out := make(chan tea.Msg, 1)
		go func() { out <- cmd() }()
		msg, ok := wait(out)
		if !ok {
			return
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				run(m, c)
			}
			return
		}
		if _, ok := msg.(tickMsg); ok || msg == nil {
			return
		}
		_, cmd = m.Update(msg)
	}
}

func press(m *model, keys ...string) {
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		_, cmd := m.Update(msg)
		run(m, cmd)
	}
}

func newTestModel(t *testing.T, o Options) *model {
	t.Helper()
	m := newModel(o)
	t.Cleanup(m.close)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	run(m, m.Init())
	return m
}

func TestPanelCreatesAndRevokesTokens(t *testing.T) {
	_, c := newServer(t)
	var copied string
	m := newTestModel(t, Options{Client: c, Copy: func(s string) error { copied = s; return nil }})

	if !strings.Contains(m.View(), "1 Status") || m.statusErr != nil {
		t.Fatalf("status not loaded: %v\n%s", m.statusErr, m.View())
	}

	press(m, "3", "n")
	if m.mode != modeCreate {
		t.Fatal("n didn't open the form")
	}
	m.form[0].SetValue("claude")
	press(m, "enter", "enter", "enter", "enter", "enter")
	if m.mode != modeShowToken || !strings.HasPrefix(m.created, auth.Prefix) {
		t.Fatalf("mode = %v, created = %q, notice = %q", m.mode, m.created, m.notice)
	}
	if !strings.Contains(m.View(), m.created) {
		t.Fatal("the new token isn't shown")
	}
	press(m, "c")
	if copied != m.created {
		t.Fatalf("copied %q", copied)
	}
	press(m, "esc")
	if m.created != "" || len(m.tokens) != 1 || m.tokens[0].Name != "claude" || m.tokens[0].Rate != 10 || m.tokens[0].Expires.IsZero() {
		t.Fatalf("tokens = %+v", m.tokens)
	}
	if strings.Contains(m.View(), copied) {
		t.Fatal("the token is still on screen after closing")
	}

	press(m, "x", "n")
	if m.tokens[0].Revoked {
		t.Fatal("revoked without confirmation")
	}
	press(m, "x", "y")
	if !m.tokens[0].Revoked {
		t.Fatalf("not revoked: %q", m.notice)
	}

	press(m, "4")
	view := m.View()
	for _, want := range []string{"token_create", "token_revoke", "claude"} {
		if !strings.Contains(view, want) {
			t.Errorf("audit view lacks %q:\n%s", want, view)
		}
	}
	press(m, "/", "r", "e", "v", "enter")
	if got := m.visibleAudit(); len(got) != 1 || got[0].Action != "token_revoke" {
		t.Fatalf("filter: %+v", got)
	}
}

func TestPanelFormValidation(t *testing.T) {
	_, c := newServer(t)
	m := newTestModel(t, Options{Client: c})
	press(m, "3", "n")
	m.form[3].SetValue("lots")
	m.form[0].SetValue("bot")
	press(m, "enter", "enter", "enter", "enter", "enter")
	if m.mode != modeCreate || !m.noticeErr || !strings.Contains(m.notice, "rate") {
		t.Fatalf("mode = %v, notice = %q", m.mode, m.notice)
	}
	m.form[3].SetValue("5")
	press(m, "enter")
	if m.mode != modeShowToken || len(m.tokens) != 1 || m.tokens[0].Rate != 5 {
		t.Fatalf("after fixing the rate: mode = %v, tokens = %+v, notice = %q", m.mode, m.tokens, m.notice)
	}
}

func TestPanelShowsTheQR(t *testing.T) {
	_, c := newServer(t)
	m := newTestModel(t, Options{Client: c})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 50})
	m.Update(statusMsg{seq: m.statusReq.start(), s: bot.Status{State: "waiting for QR scan", QR: "2@fake-qr-payload"}})
	view := m.View()
	if !strings.Contains(view, "Scan to log in") || !strings.Contains(view, "▀") {
		t.Fatalf("no QR:\n%s", view)
	}
	press(m, "L")
	if m.mode == modeConfirmLogout {
		t.Fatal("offered logout while not logged in")
	}
}

// wait returns cmd's message. A timer (cursor blink, poll tick) is dropped after 100 ms, but a server
// call is waited for: on a slow CI runner one can take longer than that.
func wait(out <-chan tea.Msg) (tea.Msg, bool) {
	deadline := time.After(10 * time.Second)
	for {
		select {
		case msg := <-out:
			return msg, true
		case <-time.After(100 * time.Millisecond):
			if callsInFlight.Load() == 0 {
				return nil, false
			}
		case <-deadline:
			return nil, false
		}
	}
}

func TestLogoutConfirmSaysWhatIsDeleted(t *testing.T) {
	_, c := newServer(t)
	m := newTestModel(t, Options{Client: c})
	text := m.viewConfirmLogout()
	for _, want := range []string{"contacts", "group", "opt-outs stay"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the logout confirm doesn't mention %q:\n%s", want, text)
		}
	}
}
