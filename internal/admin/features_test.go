package admin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/williamtcastro/wame/internal/auth"
	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
)

const (
	maria = "5511999999999@s.whatsapp.net"
	john  = "5511988887777@s.whatsapp.net"
)

func TestRotateKeepsSettingsAndRevokesTheOld(t *testing.T) {
	b, c := newServer(t)
	if _, _, err := auth.Issue(b.Store, auth.Request{Name: "support-bot", Scopes: []auth.Scope{auth.Read, auth.Send}, Chats: []string{maria}, Rate: 5, TTL: 72 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t, Options{Client: c})
	press(m, "3", "R")
	if m.mode != modeConfirmRotate || !strings.Contains(ansi.Strip(m.View()), "support-bot-2") {
		t.Fatalf("no rotate dialog:\n%s", ansi.Strip(m.View()))
	}
	press(m, "y")
	if m.mode != modeShowToken || m.replaced != "support-bot" || !strings.Contains(ansi.Strip(m.View()), "Token rotated") {
		t.Fatalf("mode = %v, notice = %q", m.mode, m.notice)
	}
	if len(m.tokens) != 2 {
		t.Fatalf("tokens = %+v", m.tokens)
	}
	old, rotated := m.tokens[0], m.tokens[1]
	if !old.Revoked || rotated.Name != "support-bot-2" || rotated.Revoked || rotated.Rate != 5 ||
		strings.Join(rotated.Scopes, ",") != "read,send" || len(rotated.Chats) != 1 || rotated.Chats[0] != maria {
		t.Fatalf("old = %+v\nrotated = %+v", old, rotated)
	}
	if life := rotated.Expires.Sub(rotated.Created); life < 71*time.Hour || life > 73*time.Hour {
		t.Fatalf("rotated lifetime = %v, want 72h", life)
	}
	if _, err := (&auth.Authenticator{Store: b.Store, Owner: owner}).Authenticate(m.created); err != nil {
		t.Fatalf("the rotated token doesn't work: %v", err)
	}
}

func TestNextName(t *testing.T) {
	tokens := []store.Token{{Name: "bot"}, {Name: "bot-2"}, {Name: "bot-5"}, {Name: "bottle-9"}}
	for name, want := range map[string]string{"bot": "bot-6", "bot-5": "bot-6", "claude": "claude-2", "bottle-9": "bottle-10"} {
		if got := nextName(name, tokens); got != want {
			t.Errorf("nextName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestUnusedTokens(t *testing.T) {
	old := time.Now().Add(-40 * 24 * time.Hour)
	for _, c := range []struct {
		t    store.Token
		want bool
	}{
		{store.Token{Created: old}, true},
		{store.Token{Created: old, LastUsed: time.Now()}, false},
		{store.Token{Created: time.Now()}, false},
		{store.Token{Created: old, Revoked: true}, false},
	} {
		if got := unused(c.t); got != c.want {
			t.Errorf("unused(%+v) = %v", c.t, got)
		}
	}
}

func TestChatsTab(t *testing.T) {
	b, c := newServer(t)
	base := time.Now().Add(-time.Hour)
	for i, msg := range []store.Message{
		{ID: "M1", Chat: maria, Sender: maria, PushName: "Maria", Type: "text", Text: "oi, tudo bem?"},
		{ID: "M2", Chat: john, Sender: john, PushName: "John", Type: "text", Text: "the boleto \x1b[31mis paid"},
		{ID: "M3", Chat: maria, FromMe: true, Type: "text", Text: "tudo ótimo"},
	} {
		msg.Timestamp = base.Add(time.Duration(i) * time.Minute)
		if _, ok, err := b.Store.Save(msg); !ok || err != nil {
			t.Fatal(err)
		}
	}
	m := newTestModel(t, Options{Client: c})
	press(m, "2")
	if len(m.chats) != 2 || m.msgsChat != m.chats[0].JID {
		t.Fatalf("chats = %+v, open = %q", m.chats, m.msgsChat)
	}
	view := ansi.Strip(m.View())
	if !strings.Contains(view, "tudo ótimo") || !strings.Contains(view, "doesn't mark anything as read") {
		t.Fatalf("messages not shown:\n%s", view)
	}

	press(m, "/", "b", "o", "l", "e", "t", "o", "enter")
	if len(m.searchHits) != 1 || m.searchHits[0].ID != "M2" {
		t.Fatalf("search = %+v", m.searchHits)
	}
	if view := m.View(); strings.Contains(view, "\x1b[31mis") || !strings.Contains(ansi.Strip(view), "is paid") {
		t.Fatal("search results weren't sanitized")
	}
	press(m, "esc")
	if m.search != "" {
		t.Fatal("esc didn't clear the search")
	}

	press(m, "i", "h", "i", "enter")
	if !m.noticeErr || !strings.Contains(m.notice, "not sent") {
		t.Fatalf("sending without a WhatsApp session: notice = %q", m.notice)
	}
}

func TestLiveStreamConnectsAndCounts(t *testing.T) {
	_, c := newServer(t)
	m := newModel(Options{Client: c})
	t.Cleanup(m.close)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	wait := m.live.start(c, m.gen)
	got := make(chan tea.Msg, 1)
	go func() { got <- wait() }()
	select {
	case msg := <-got:
		m.Update(msg)
	case <-time.After(5 * time.Second):
		t.Fatal("the event stream never connected")
	}
	if !m.live.on || !strings.Contains(m.View(), "live") || m.pollEvery() != pollLive {
		t.Fatalf("live = %+v", m.live)
	}

	for _, msg := range []store.Message{
		{Seq: 1, Chat: maria, PushName: "Maria"}, {Seq: 2, Chat: maria, PushName: "Maria"}, {Seq: 3, Chat: john, FromMe: true},
	} {
		data, _ := json.Marshal(msg)
		m.Update(eventMsg{m.gen, Event{Type: "message", Data: data}})
	}
	in, out, top := m.live.counts()
	if in != 2 || out != 1 || top[0].chat != maria || top[0].n != 2 || top[0].name != "Maria" {
		t.Fatalf("counts = %d, %d, %+v", in, out, top)
	}
	m.Update(eventMsg{m.gen + 1, Event{Type: "message", Data: json.RawMessage(`{"seq":9,"chat":"x"}`)}})
	if in, _, _ := m.live.counts(); in != 2 {
		t.Fatal("an event from a previous server was counted")
	}
}

func TestBellWhenTheServerGoesDown(t *testing.T) {
	_, c := newServer(t)
	rang := 0
	m := newTestModel(t, Options{Client: c, Bell: func() { rang++ }})
	healthy := bot.Status{State: "connected", Connected: true, LoggedIn: true, JID: "5521987654321@s.whatsapp.net"}
	m.Update(statusMsg{s: healthy})
	m.Update(statusMsg{s: healthy})
	if rang != 0 {
		t.Fatal("rang while healthy")
	}
	m.Update(statusMsg{s: bot.Status{State: "disconnected", LoggedIn: true}})
	if rang != 1 || !m.noticeErr {
		t.Fatalf("rang %d times, notice %q", rang, m.notice)
	}
	m.Update(statusMsg{s: bot.Status{State: "disconnected", LoggedIn: true}})
	if rang != 1 {
		t.Fatal("rang again while still down")
	}
}

func TestHealthTab(t *testing.T) {
	_, c := newServer(t)
	m := newTestModel(t, Options{Client: c})
	press(m, "5")
	view := ansi.Strip(m.View())
	for _, want := range []string{"Sends", "Number lookups", "Auto-replies", "No webhook configured", "test"} {
		if !strings.Contains(view, want) {
			t.Errorf("health view lacks %q:\n%s", want, view)
		}
	}
	m.Update(statsMsg{err: &APIError{Code: 404}})
	m.statsAt = time.Time{}
	if !strings.Contains(ansi.Strip(m.View()), "doesn't have GET /stats") {
		t.Fatal("an older server isn't explained")
	}
}

func TestStatusCardsLineUp(t *testing.T) {
	_, c := newServer(t)
	for _, width := range []int{120, 180} {
		m := newTestModel(t, Options{Client: c})
		m.Update(tea.WindowSizeMsg{Width: width, Height: 50})
		m.Update(statusMsg{s: bot.Status{State: "connected", Connected: true, LoggedIn: true, JID: "5521987654321@s.whatsapp.net", PushName: "Ana"}})
		cols := 2
		if width >= 150 {
			cols = 3
		}
		for _, line := range strings.Split(ansi.Strip(m.View()), "\n") {
			if ansi.StringWidth(line) > width {
				t.Fatalf("line wider than %d: %q", width, line)
			}
			tops, bottoms := strings.Count(line, "╭"), strings.Count(line, "╰")
			if (tops != 0 && tops != cols && tops != 6%cols) || (bottoms != 0 && bottoms != cols && bottoms != 6%cols) {
				t.Fatalf("width %d: cards don't line up on %q", width, line)
			}
		}
	}
}

func TestSwitchingServers(t *testing.T) {
	_, c1 := newServer(t)
	_, c2 := newServer(t)
	profiles := []Profile{
		{Name: "home", Source: Source{Server: c1.Server(), Token: owner}},
		{Name: "vps", Source: Source{Server: c2.Server(), Token: owner}},
	}
	m := newTestModel(t, Options{Profiles: profiles})
	if m.mode != modeServers || !strings.Contains(ansi.Strip(m.View()), "vps") {
		t.Fatalf("no picker without a client:\n%s", ansi.Strip(m.View()))
	}
	press(m, "j", "enter")
	if m.c == nil || m.profile != "vps" || m.mode != modeNormal || m.c.Server() != c2.Server() {
		t.Fatalf("profile = %q, mode = %v", m.profile, m.mode)
	}
	gen := m.gen
	press(m, "S", "k", "enter")
	if m.profile != "home" || m.gen != gen+1 {
		t.Fatalf("profile = %q, gen = %d", m.profile, m.gen)
	}
	m.Update(genMsg{gen, tokensMsg{tokens: []store.Token{{Name: "from-vps"}}}})
	if len(m.tokens) != 0 {
		t.Fatal("a late answer from the previous server was applied")
	}
}

func TestSafe(t *testing.T) {
	if got := safe("a\x1b[2Jb\u202ec\nd\x07"); got != "a[2Jbc d" {
		t.Fatalf("safe = %q", got)
	}
}

func TestSettingsTab(t *testing.T) {
	b, c := newServer(t)
	m := newTestModel(t, Options{Client: c})
	press(m, "6")
	if len(m.settings) != 5 || !strings.Contains(ansi.Strip(m.View()), "Send rate") {
		t.Fatalf("settings = %+v\n%s", m.settings, ansi.Strip(m.View()))
	}
	press(m, "enter")
	if m.mode != modeSetting || m.prompt.Value() != "20" {
		t.Fatalf("mode = %v, prompt = %q", m.mode, m.prompt.Value())
	}
	m.prompt.SetValue("7")
	press(m, "enter")
	if b.Limiter.Stats().PerMinute != 7 || m.settings[0].Value != "7" || !m.settings[0].Overridden || m.noticeErr {
		t.Fatalf("rate not changed: %+v, notice %q", m.settings[0], m.notice)
	}
	if !strings.Contains(ansi.Strip(m.View()), "changed by api") {
		t.Fatalf("source not shown:\n%s", ansi.Strip(m.View()))
	}

	press(m, "enter")
	m.prompt.SetValue("lots")
	press(m, "enter")
	if !m.noticeErr || b.Limiter.Stats().PerMinute != 7 {
		t.Fatalf("a bad value: notice %q, rate %d", m.notice, b.Limiter.Stats().PerMinute)
	}

	press(m, "j", "j", "j", "enter")
	if b.AutoReplyOn() || m.settings[3].Value != "false" {
		t.Fatal("enter didn't toggle auto-replies")
	}
	press(m, "g", "k", "k", "k", "x")
	if b.Limiter.Stats().PerMinute != 20 || m.settings[0].Overridden {
		t.Fatalf("x didn't reset the rate: %+v", m.settings[0])
	}
}

func TestHealthWebhookTestAndReplay(t *testing.T) {
	b, c := newServer(t)
	var reject atomic.Bool
	reject.Store(true)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reject.Load() && r.Header.Get("X-Wame-Event") != "test" {
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(receiver.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b.Webhook = bot.NewWebhook(ctx, receiver.URL, "", store.Filter{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	msg, _, err := b.Store.Save(store.Message{ID: "W1", Chat: maria, Sender: maria, Type: "text", Text: "oi", Timestamp: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	b.Webhook.Deliver("message", msg)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && len(b.Webhook.Stats().Recent) == 0; {
		time.Sleep(10 * time.Millisecond)
	}

	m := newTestModel(t, Options{Client: c})
	press(m, "5", "t")
	if m.noticeErr || !strings.Contains(m.notice, "HTTP 200") {
		t.Fatalf("test: %q", m.notice)
	}
	if !strings.Contains(ansi.Strip(m.View()), "gave up") {
		t.Fatalf("failure not listed:\n%s", ansi.Strip(m.View()))
	}
	reject.Store(false)
	press(m, "R")
	if m.noticeErr || !strings.Contains(m.notice, "replay of") || !strings.Contains(ansi.Strip(m.View()), "replayed") {
		t.Fatalf("replay: %q\n%s", m.notice, ansi.Strip(m.View()))
	}
	press(m, "R")
	if !m.noticeErr || !strings.Contains(m.notice, "already replayed") {
		t.Fatalf("second replay: %q", m.notice)
	}
}

func TestTokensShowUsage(t *testing.T) {
	b, c := newServer(t)
	plain, _, err := auth.Issue(b.Store, auth.Request{Name: "claude", Scopes: []auth.Scope{auth.Read}})
	if err != nil {
		t.Fatal(err)
	}
	agent, _ := NewClient(c.Server(), plain, false)
	for range 4 {
		if _, err := agent.Status(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	m := newTestModel(t, Options{Client: c})
	press(m, "3")
	if calls, _, _, _, ok := m.callerUsage("claude"); !ok || calls != 4 {
		t.Fatalf("usage = %d, %v", calls, ok)
	}
	view := ansi.Strip(m.View())
	if !strings.Contains(view, "REQ/24H") || !strings.Contains(view, "4 requests") || !strings.Contains(view, "from 127.0.0.1") {
		t.Fatalf("usage not shown:\n%s", view)
	}
}
