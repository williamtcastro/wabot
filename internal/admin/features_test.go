package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/williamtcastro/wame/internal/api"
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
	m.Update(statusMsg{seq: m.statusReq.start(), s: healthy})
	m.Update(statusMsg{seq: m.statusReq.start(), s: healthy})
	if rang != 0 {
		t.Fatal("rang while healthy")
	}
	m.Update(statusMsg{seq: m.statusReq.start(), s: bot.Status{State: "disconnected", LoggedIn: true}})
	if rang != 1 || !m.noticeErr {
		t.Fatalf("rang %d times, notice %q", rang, m.notice)
	}
	m.Update(statusMsg{seq: m.statusReq.start(), s: bot.Status{State: "disconnected", LoggedIn: true}})
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
	m.Update(statsMsg{seq: m.statsReq.start(), err: &APIError{Code: 404}})
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
		m.Update(statusMsg{seq: m.statusReq.start(), s: bot.Status{State: "connected", Connected: true, LoggedIn: true, JID: "5521987654321@s.whatsapp.net", PushName: "Ana"}})
		cols := 2
		if width >= 150 {
			cols = 3
		}
		for line := range strings.SplitSeq(ansi.Strip(m.View()), "\n") {
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
	if got := safe("a\x1b[2Jb\u202ec\nd\x07"); got != "abc d" {
		t.Fatalf("safe = %q", got)
	}
}

func TestSettingsTab(t *testing.T) {
	b, c := newServer(t)
	m := newTestModel(t, Options{Client: c})
	press(m, "6")
	if len(m.settings) != len(bot.SettingKeys) || !strings.Contains(ansi.Strip(m.View()), "Send rate") {
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

func TestQueueTab(t *testing.T) {
	b, c := newServer(t)
	items, err := b.Enqueue("api", "", "promo", "", []bot.QueueRequest{{To: "5511999999999", Text: "Hi Maria"}, {To: "5511988887777", Text: "Hi John"}, {To: "5521987654321", Text: "Hi Ana"}})
	if err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t, Options{Client: c})
	press(m, "7")
	view := ansi.Strip(m.View())
	if len(m.queue.Items) != 3 || !strings.Contains(view, "promo") || !strings.Contains(view, "7 Queue 3") {
		t.Fatalf("queue = %+v\n%s", m.queue, view)
	}
	if strings.Contains(view, "Hi Maria") {
		t.Fatal("the list must not show message text")
	}

	press(m, "x")
	if it, _ := b.Store.OutboxItem(items[2].ID); it.Status != store.OutboxCanceled {
		t.Fatalf("x on the newest: %+v, notice %q", it, m.notice)
	}
	press(m, "x")
	if !m.noticeErr {
		t.Fatal("canceling a canceled item must say no")
	}
	press(m, "X", "y")
	if cnt, _ := b.Store.CountOutbox(store.OutboxFilter{}); cnt.Canceled != 3 {
		t.Fatalf("X y: %+v", cnt)
	}

	press(m, "P")
	if !b.QueueStats().Paused || !m.stats.Queue.Paused {
		t.Fatalf("P didn't pause: notice %q", m.notice)
	}
	press(m, "f")
	if m.queueStatus != store.OutboxQueued || len(m.queue.Items) != 0 {
		t.Fatalf("filter: %q %d", m.queueStatus, len(m.queue.Items))
	}
}

func TestQueueTabReceiptsAndQuietHours(t *testing.T) {
	b, c := newServer(t)
	items, err := b.Enqueue("api", "", "promo", "", []bot.QueueRequest{{To: "5511999999999", Text: "Hi Maria"}, {To: "5511988887777", Text: "Hi John"}, {To: "5521987654321", Text: "Hi Ana"}})
	if err != nil {
		t.Fatal(err)
	}
	for i, it := range items {
		if _, err := b.Store.ClaimOutbox(it.ID, string(rune('A'+i))); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Store.FinishOutbox(it.ID, store.OutboxResult{Status: store.OutboxSent}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.Store.MarkOutboxReceipt([]string{"A", "B"}, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Store.MarkOutboxReceipt([]string{"A"}, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	sp, _ := time.LoadLocation("America/Sao_Paulo")
	now := time.Now().In(sp)
	window := now.Add(-time.Hour).Format("15:04") + "-" + now.Add(time.Hour).Format("15:04")
	if err := b.UpdateSettings("api", map[string]*string{"queue_quiet_hours": &window, "queue_timezone": new("America/Sao_Paulo")}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	b.Go(b.RunOutbox)
	for deadline := time.Now().Add(5 * time.Second); !b.QueueStats().Running && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
	}

	m := newTestModel(t, Options{Client: c})
	press(m, "7")
	view := ansi.Strip(m.View())
	for _, want := range []string{"✓✓ read", "✓✓ delivered", "✓ sent", "2 delivered · 1 read of 3 sent",
		"quiet hours until " + now.Add(time.Hour).Format("15:04") + " America/Sao_Paulo", window} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %q:\n%s", want, view)
		}
	}
}

// spread is one time for each of when's layouts: today, yesterday, earlier this year, a previous year.
func spread() []time.Time {
	now := time.Now()
	thisYear := time.Date(now.Year(), 1, 1, 12, 0, 0, 0, time.Local)
	if now.Sub(thisYear) < 8*24*time.Hour {
		thisYear = now.AddDate(0, 0, -20)
	}
	return []time.Time{now, now.AddDate(0, 0, -1), thisYear, now.AddDate(-1, 0, -3)}
}

func TestWhenFillsItsColumn(t *testing.T) {
	date := func(t time.Time) string {
		if t.Year() == time.Now().Year() {
			return t.Format("Jan 02")
		}
		return t.Format("Jan 2006")
	}
	ats, future := spread(), time.Now().AddDate(0, 0, 3)
	cases := map[time.Time]string{
		ats[0]: ats[0].Format("15:04"), ats[1]: ats[1].Format("Mon 15:04"), ats[2]: date(ats[2]),
		ats[3]: ats[3].Format("Jan 2006"), future: date(future), {}: "",
	}
	for at, want := range cases {
		if got := when(at); got != pad(want, whenWidth) || ansi.StringWidth(got) != whenWidth {
			t.Errorf("when(%v) = %q, want %q padded to %d", at, got, want, whenWidth)
		}
	}
}

// leftCol is where want starts in line, in columns, or -1.
func leftCol(line, want string) int {
	at := strings.Index(line, want)
	if at < 0 {
		return -1
	}
	return ansi.StringWidth(line[:at])
}

func TestChatRowsStayOnOneLine(t *testing.T) {
	b, c := newServer(t)
	people := []struct{ jid, name, label string }{
		{maria, "Maria John Ana Maria John Ana Maria", "~Maria"},
		{john, "John", "~John"},
		{"552187654321@s.whatsapp.net", "Ana", "~Ana"},
		{"5521987654321@s.whatsapp.net", "", "+5521987654321"},
	}
	ats := spread()
	for i, p := range people {
		if _, ok, err := b.Store.Save(store.Message{ID: fmt.Sprint("C", i), Chat: p.jid, Sender: p.jid, PushName: p.name, Type: "text", Text: "oi", Timestamp: ats[i]}); !ok || err != nil {
			t.Fatal(err)
		}
	}
	for _, width := range []int{100, 120, 180} {
		m := newTestModel(t, Options{Client: c})
		m.Update(tea.WindowSizeMsg{Width: width, Height: 30})
		press(m, "2")
		view := ansi.Strip(m.View())
		lines := strings.Split(view, "\n")
		left := func(i int) string { return ansi.Truncate(lines[i], chatListWidth+1, "") }
		first, col := -1, -1
		for i, p := range people {
			row := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(ansi.Truncate(l, chatListWidth+1, ""), p.label) })
			if first < 0 {
				first = row
			}
			if row != first+i {
				t.Fatalf("width %d: %s is on line %d, want %d:\n%s", width, p.label, row, first+i, view)
			}
			at := leftCol(left(row), when(ats[i]))
			switch {
			case at < 0:
				t.Fatalf("width %d: %s's row lacks %q: %q\n%s", width, p.label, when(ats[i]), left(row), view)
			case col < 0:
				col = at
			case at != col:
				t.Fatalf("width %d: %s's time starts at column %d, the first row's at %d:\n%s", width, p.label, at, col, view)
			}
		}
		if after := left(first + len(people)); strings.TrimSpace(after) != "" {
			t.Fatalf("width %d: a chat row spilled onto the next line: %q\n%s", width, after, view)
		}
		for _, l := range lines {
			if ansi.StringWidth(l) > width {
				t.Fatalf("width %d: line too wide: %q", width, l)
			}
		}
		if strings.Contains(view, "─…") {
			t.Fatalf("width %d: the message pane is wider than its room:\n%s", width, view)
		}
	}
}

func TestTimeColumnsLineUp(t *testing.T) {
	_, c := newServer(t)
	ats := spread()
	long := strings.Repeat("a detail long enough to run past the right edge ", 4)
	var audit []store.AuditEntry
	var failures []bot.WebhookFailure
	var tokens []store.Token
	var items []store.OutboxItem
	for i, at := range ats {
		audit = append(audit, store.AuditEntry{ID: int64(10 - i), At: at, Actor: "api", Action: "send", Target: maria, Detail: long, OK: true})
		failures = append(failures, bot.WebhookFailure{At: at, Delivery: fmt.Sprint("d", i), Event: "message", Attempt: 2, Error: "the receiver timed out " + long, GaveUp: i%2 == 0})
		tokens = append(tokens, store.Token{ID: fmt.Sprint("t", i), Name: fmt.Sprint("support-bot-", i), Scopes: []string{"read", "send"}, Created: at, LastUsed: at, Expires: at.AddDate(2, 0, 0), CreatedBy: "a-parent-token-name"})
		items = append(items, store.OutboxItem{ID: int64(i + 1), Batch: "b1", Actor: "api", To: "5511988887777", Status: store.OutboxFailed, Attempts: 3, Due: at, Created: at, Updated: at, Error: long})
	}
	stats := api.Stats{Version: "test", Started: ats[3], Webhook: &bot.WebhookStats{Recent: failures}}

	// check fails on a line wider than the screen, and on rows (the lines with marker) where after
	// doesn't start in the same column. It returns the rows' line numbers.
	check := func(t *testing.T, width int, view, marker, after string) []int {
		t.Helper()
		var rows []int
		col := -1
		for i, l := range strings.Split(view, "\n") {
			if ansi.StringWidth(l) > width {
				t.Fatalf("width %d: line too wide (%d): %q\n%s", width, ansi.StringWidth(l), l, view)
			}
			if marker == "" || !strings.Contains(l, marker) {
				continue
			}
			rows = append(rows, i)
			at := leftCol(l, after)
			switch {
			case at < 0:
				t.Fatalf("width %d: %q lacks %q\n%s", width, l, after, view)
			case col < 0:
				col = at
			case at != col:
				t.Fatalf("width %d: %q starts at %d, want %d:\n%s", width, after, at, col, view)
			}
		}
		return rows
	}
	for _, width := range []int{100, 120, 180} {
		m := newTestModel(t, Options{Client: c})
		m.Update(tea.WindowSizeMsg{Width: width, Height: 40})
		m.Update(statsMsg{seq: m.statsReq.start(), s: stats})
		check(t, width, ansi.Strip(m.View()), "", "")

		press(m, "3")
		m.Update(tokensMsg{tokens: tokens})
		check(t, width, ansi.Strip(m.View()), "", "")

		press(m, "4")
		m.Update(auditMsg{entries: audit})
		check(t, width, ansi.Strip(m.View()), "✓  ", " api ")

		press(m, "5")
		m.Update(statsMsg{seq: m.statsReq.start(), s: stats})
		view := ansi.Strip(m.View())
		rows := check(t, width, view, "try 2", "try 2")
		if len(rows) != len(failures) || rows[len(rows)-1]-rows[0] != len(failures)-1 || !strings.Contains(strings.Split(view, "\n")[rows[len(rows)-1]+1], "R replays") {
			t.Fatalf("width %d: webhook failures aren't one line each:\n%s", width, view)
		}
		more := stats
		more.Webhook = &bot.WebhookStats{Recent: slices.Concat(failures, failures)}
		m.Update(statsMsg{seq: m.statsReq.start(), s: more})
		view = ansi.Strip(m.View())
		lines := strings.Split(view, "\n")
		rows = check(t, width, view, "try 2", "try 2")
		if len(rows) != 6 {
			t.Fatalf("width %d: %d failures listed, want 6:\n%s", width, len(rows), view)
		}
		foot := rows[5] + 1
		next := strings.Split(lines[foot+1], "│")
		if !strings.Contains(lines[foot], "j/k to move") || len(next) > 1 && strings.TrimSpace(next[len(next)-2]) != "" {
			t.Fatalf("width %d: the failures' footer isn't one line:\n%s", width, view)
		}

		press(m, "7")
		m.Update(queueMsg{page: QueuePage{Items: items}})
		check(t, width, ansi.Strip(m.View()), "", "")
	}
}

func TestHeaderKeepsTheStateWithEveryBadge(t *testing.T) {
	c, err := NewClient("http://127.0.0.1:1", "t", false)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, width := range []int{100, 120} {
		for tb := range tabNames {
			m := newModel(Options{Client: c, Profile: "maria-server"})
			m.width, m.height, m.tab = width, 30, tab(tb)
			m.status, m.checked, m.latency = bot.Status{Connected: true, LoggedIn: true}, now, 42*time.Millisecond
			m.chats = []store.Chat{{JID: maria, Unread: 3}, {JID: john, Unread: 1}}
			m.tokens = []store.Token{{Name: "support-bot"}, {Name: "ana-bot"}}
			m.audit = []store.AuditEntry{{At: now, Action: "send", Actor: "ana-bot"}}
			m.statsAt = now
			m.stats.Limits.Send.LastRejected = now
			m.stats.Queue = bot.QueueStats{OutboxCounts: store.OutboxCounts{Queued: 12}, Paused: true}
			m.settings = []bot.SettingView{{Key: "rate", Overridden: true}}
			header := ansi.Strip(m.viewHeader())
			if !strings.Contains(header, "● connected") || ansi.StringWidth(header) > width {
				t.Fatalf("width %d, tab %d: %q (%d wide)", width, tb+1, header, ansi.StringWidth(header))
			}
			if strings.Contains(header, "…") || !strings.Contains(header, fmt.Sprintf("%d %s", tb+1, tabNames[tb])) {
				t.Fatalf("width %d: a tab label is cut or the open one lost its name: %q", width, header)
			}
			for _, badge := range []string{"✗1", "!", "⏸12", "•"} {
				if !strings.Contains(header, badge) {
					t.Fatalf("width %d: badge %q missing: %q", width, badge, header)
				}
			}
		}
	}
	m := newModel(Options{Client: c})
	m.width, m.status, m.checked = 50, bot.Status{Connected: true, LoggedIn: true}, now
	if header := ansi.Strip(m.viewHeader()); !strings.Contains(header, "● connected") || ansi.StringWidth(header) > 50 {
		t.Fatalf("narrow: tabs must be cut, not the state: %q", header)
	}
}

func TestQueueStateFromANewerServer(t *testing.T) {
	got := queueState(store.OutboxItem{Status: "held\x1b]52;c;cHduZWQ=\x07"})
	if !strings.Contains(ansi.Strip(got), "held") || strings.Contains(got, "]52;") {
		t.Fatalf("unknown status: %q", got)
	}
	body := queueBody(bot.QueueStats{Running: true, QuietHours: "21:00\x1b[2J-08:00"}, -1, "")
	if strings.Contains(body, "[2J") || !strings.Contains(ansi.Strip(body), "21:00") {
		t.Fatalf("quiet hours not sanitized: %q", body)
	}
}

func TestChatTitleJIDIsSanitized(t *testing.T) {
	m := newModel(Options{})
	m.width, m.height, m.tab, m.mode = 120, 30, tabChats, modeNormal
	m.chats = []store.Chat{{JID: "120363000000000000\x1b]52;c;cHduZWQ=\x07@g.us"}}
	if strings.Contains(m.View(), "]52;") {
		t.Fatal("the chat's JID reached the screen unsanitized")
	}
}

type fakeNotifier struct{ fail atomic.Bool }

func (f *fakeNotifier) Name() string { return "gotify" }

func (f *fakeNotifier) Notify(context.Context, bot.Alert) error {
	if f.fail.Load() {
		return fmt.Errorf("gotify answered 500 \x1b[31mboom")
	}
	return nil
}

func TestHealthAlerts(t *testing.T) {
	b, c := newServer(t)
	n := &fakeNotifier{}
	b.SetAlerts(n, 30*time.Second)
	m := newTestModel(t, Options{Client: c})
	press(m, "5")
	view := ansi.Strip(m.View())
	for _, want := range []string{"Alerts", "gotify", "30s", "never", "test alert"} {
		if !strings.Contains(view, want) {
			t.Fatalf("health view lacks %q:\n%s", want, view)
		}
	}
	press(m, "a")
	if m.noticeErr || m.notice != "test alert sent" || m.stats.Alerts.Sent != 1 {
		t.Fatalf("test alert: %q, %+v", m.notice, m.stats.Alerts)
	}
	if !strings.Contains(ansi.Strip(m.View()), "wame: test alert") {
		t.Fatalf("last alert not shown:\n%s", ansi.Strip(m.View()))
	}
	n.fail.Store(true)
	press(m, "a")
	view = m.View()
	if !m.noticeErr || !strings.Contains(m.notice, "test alert failed") || strings.Contains(view, "\x1b[31mboom") {
		t.Fatalf("failed alert: %q", m.notice)
	}
	if !strings.Contains(ansi.Strip(view), "Last error") || !strings.Contains(ansi.Strip(view), "1 failed") || !m.healthWarning() {
		t.Fatalf("alert error not shown or not flagged:\n%s", ansi.Strip(view))
	}
	m.Update(alertMsg{&APIError{Code: 404}})
	if !m.noticeErr || !strings.Contains(m.notice, "can't send test alerts yet") {
		t.Fatalf("older server: %q", m.notice)
	}
}

func TestHealthWithoutAlerts(t *testing.T) {
	_, c := newServer(t)
	m := newTestModel(t, Options{Client: c})
	press(m, "5")
	if !strings.Contains(ansi.Strip(m.View()), "No alerts configured") {
		t.Fatalf("missing the not-configured hint:\n%s", ansi.Strip(m.View()))
	}
	press(m, "a")
	if !m.noticeErr || !strings.Contains(m.notice, "no alerts are configured") {
		t.Fatalf("test alert without a backend: %q", m.notice)
	}
}

func TestQueueTabQuietSwitch(t *testing.T) {
	b, c := newServer(t)
	m := newTestModel(t, Options{Client: c})
	press(m, "7", "Q")
	if !m.noticeErr || !strings.Contains(m.notice, "no quiet hours set") {
		t.Fatalf("Q without a window: %q", m.notice)
	}
	if err := b.UpdateSettings("api", map[string]*string{"queue_quiet_hours": new("21:00-08:00")}); err != nil {
		t.Fatal(err)
	}
	press(m, "r", "Q")
	if !b.QueueStats().QuietOff {
		t.Fatalf("Q must switch quiet hours off: %q", m.notice)
	}
	if view := ansi.Strip(m.View()); !strings.Contains(view, "21:00-08:00") || !strings.Contains(view, "off") {
		t.Fatalf("the window must stay visible, marked off:\n%s", view)
	}
	press(m, "Q")
	if b.QueueStats().QuietOff {
		t.Fatal("Q again must switch them back on")
	}
}

func TestHeaderAdvertisesQuietHours(t *testing.T) {
	b, c := newServer(t)
	m := newTestModel(t, Options{Client: c})
	if strings.Contains(ansi.Strip(m.viewHeader()), "quiet") {
		t.Fatal("no window set: nothing to advertise")
	}
	now := time.Now()
	holding := now.Add(-time.Hour).Format("15:04") + "-" + now.Add(time.Hour).Format("15:04")
	later := now.Add(2*time.Hour).Format("15:04") + "-" + now.Add(3*time.Hour).Format("15:04")
	for _, w := range []struct{ window, want string }{
		{holding, "☾ quiet until " + now.Add(time.Hour).Format("15:04")},
		{later, "☾ quiet " + later},
	} {
		if err := b.UpdateSettings("api", map[string]*string{"queue_quiet_hours": &w.window}); err != nil {
			t.Fatal(err)
		}
		press(m, "r")
		for _, width := range []int{100, 120, 180} {
			m.Update(tea.WindowSizeMsg{Width: width, Height: 40})
			h := ansi.Strip(m.viewHeader())
			if !strings.Contains(h, w.want) || ansi.StringWidth(h) > width {
				t.Fatalf("width %d: want %q in the header:\n%s", width, w.want, h)
			}
		}
	}
	press(m, "7", "Q")
	if h := ansi.Strip(m.viewHeader()); strings.Contains(h, "quiet") {
		t.Fatalf("switched off: the header must not advertise it:\n%s", h)
	}
}
