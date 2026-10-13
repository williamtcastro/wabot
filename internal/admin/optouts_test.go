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
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/williamtcastro/wame/internal/api"
	"github.com/williamtcastro/wame/internal/auth"
	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
)

const ana = "5521987654321@s.whatsapp.net"

// flat is the view as plain text with runs of spaces collapsed, so tests don't depend on padding.
func flat(view string) string { return strings.Join(strings.Fields(ansi.Strip(view)), " ") }

func optOutChats(t *testing.T, b *bot.Bot) []string {
	t.Helper()
	list, err := b.OptOuts()
	if err != nil {
		t.Fatal(err)
	}
	var chats []string
	for _, o := range list {
		chats = append(chats, o.Chat)
	}
	return chats
}

func TestQueueOptOuts(t *testing.T) {
	b, c := newServer(t)
	now := time.Now()
	for _, o := range []store.OptOut{
		{Chat: maria, Reason: "STOP", At: now.Add(-2 * time.Hour), By: "whatsapp"},
		{Chat: john, Reason: "manual", At: now.Add(-time.Hour), By: "evil\x1b]52;c;cHduZWQ=\x07"},
	} {
		if _, err := b.Store.AddOptOut(o); err != nil {
			t.Fatal(err)
		}
	}
	m := newTestModel(t, Options{Client: c})
	m.chats = []store.Chat{{JID: maria, Name: "Maria"}}
	press(m, "7")
	view := flat(m.View())
	if !strings.Contains(view, "Opt-outs 2 · o lists them") {
		t.Fatalf("queue view lacks the opt-out count or key:\n%s", view)
	}

	press(m, "o")
	raw := m.View()
	view = flat(raw)
	for _, want := range []string{"+5511999999999", "Maria", "keyword STOP", "+5511988887777", "added by evil", "a add", "x remove", "o/esc queue", "Opt-outs 2 · o back"} {
		if !strings.Contains(view, want) {
			t.Errorf("opt-outs view lacks %q:\n%s", want, view)
		}
	}
	if strings.Contains(raw, "]52;") {
		t.Fatal("the opt-out list isn't sanitized")
	}

	press(m, "a", "5521987654321", "enter")
	if got := optOutChats(t, b); !slices.Contains(got, ana) || m.noticeErr || len(m.optOuts) != 3 {
		t.Fatalf("add: server %v, panel %d, notice %q", got, len(m.optOuts), m.notice)
	}
	press(m, "a", "5521987654321", "enter")
	if m.noticeErr || !strings.Contains(m.notice, "already") {
		t.Fatalf("adding twice: %q", m.notice)
	}
	press(m, "a", "120363000000000000@g.us", "enter")
	if !m.noticeErr || !strings.Contains(m.notice, "not added: only a person's chat") {
		t.Fatalf("adding a group: %q", m.notice)
	}
	m.Update(doneMsg{err: &APIError{Code: http.StatusBadRequest, Msg: "bad\x1b[2Jinput"}})
	if strings.Contains(m.notice, "\x1b") {
		t.Fatalf("server error text not sanitized: %q", m.notice)
	}

	press(m, "/", "m", "a", "r", "i", "a", "enter")
	if vis := m.visibleOptOuts(); len(vis) != 1 || vis[0].Chat != maria || !strings.Contains(ansi.Strip(m.View()), "filtered by") {
		t.Fatalf("filter: %+v", vis)
	}
	if keys := flat(m.viewKeys()); !strings.Contains(keys, "esc clear") || strings.Contains(keys, "o/esc") {
		t.Fatalf("with a filter set esc clears it, the footer must say so: %s", keys)
	}
	press(m, "esc")
	if m.optOutQuery != "" || !m.queueOptOuts || len(m.visibleOptOuts()) != 3 {
		t.Fatalf("esc must clear the filter first: %q, %v", m.optOutQuery, m.queueOptOuts)
	}

	press(m, "G", "x", "n")
	if len(optOutChats(t, b)) != 3 || m.mode != modeNormal {
		t.Fatal("removed without confirmation")
	}
	press(m, "x")
	if m.mode != modeConfirmRemoveOptOut || !strings.Contains(flat(m.View()), "Take Maria (+5511999999999) off") {
		t.Fatalf("confirm dialog:\n%s", ansi.Strip(m.View()))
	}
	m.Update(optOutsMsg{seq: m.optOutsReq.start(), list: []store.OptOut{{Chat: ana}, {Chat: john}}})
	press(m, "y")
	if got := optOutChats(t, b); slices.Contains(got, maria) || len(got) != 2 {
		t.Fatalf("remove must act on the chat marked when the dialog opened: %v, notice %q", got, m.notice)
	}
	if len(m.optOuts) != 2 || m.noticeErr {
		t.Fatalf("after remove: %+v, %q", m.optOuts, m.notice)
	}

	press(m, "esc")
	if m.queueOptOuts || !strings.Contains(flat(m.View()), "f filter") {
		t.Fatal("esc must go back to the queue items")
	}
}

func TestQueueOptOutsOnAnOlderServer(t *testing.T) {
	b, err := bot.New(context.Background(), filepath.Join(t.TempDir(), "wame.db"), waLog.Noop, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	h := api.NewHandler(b, &auth.Authenticator{Store: b.Store, Owner: owner}, "test")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/optouts") {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, owner, false)
	if err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t, Options{Client: c})
	press(m, "7")
	if view := ansi.Strip(m.View()); strings.Contains(view, "o lists them") || m.queueErr != nil {
		t.Fatalf("an older server must hide the count, not the queue:\n%s", view)
	}
	press(m, "o")
	if view := ansi.Strip(m.View()); !strings.Contains(view, "doesn't have the opt-out list (GET /optouts) yet") {
		t.Fatalf("no update hint:\n%s", view)
	}
	press(m, "a")
	if m.mode != modeNormal || !m.noticeErr || !strings.Contains(m.notice, "update wame") {
		t.Fatalf("a on an older server: mode %v, notice %q", m.mode, m.notice)
	}
}

// optOutsServer is a real API that counts GET /optouts; with gone set the route answers 404, as on
// a server older than opt-outs.
func optOutsServer(t *testing.T, gone bool) (*Client, *atomic.Int32) {
	t.Helper()
	b, err := bot.New(context.Background(), filepath.Join(t.TempDir(), "wame.db"), waLog.Noop, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.InitSettings(bot.Settings{Rate: 20, ChatGap: time.Second, MaxWait: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}
	h := api.NewHandler(b, &auth.Authenticator{Store: b.Store, Owner: owner}, "test")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/optouts") && r.Method == http.MethodGet {
			hits.Add(1)
			if gone {
				http.NotFound(w, r)
				return
			}
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, owner, false)
	if err != nil {
		t.Fatal(err)
	}
	return c, &hits
}

// tickNow delivers one poll tick and runs what it asks for, without waiting for the next tick.
func tickNow(m *model) {
	_, cmd := m.Update(tickMsg{})
	run(m, cmd)
}

func TestOptOutsStopPollingAfter404Or403(t *testing.T) {
	for _, c := range []struct {
		name  string
		gone  bool
		scope string
		want  string
	}{
		{"older server", true, "admin", "update wame"},
		{"no admin scope", false, "read", "admin scope"},
	} {
		t.Run(c.name, func(t *testing.T) {
			admin, hits := optOutsServer(t, c.gone)
			plain, _, err := admin.CreateToken(t.Context(), TokenRequest{Name: "agent", Scopes: []string{c.scope}})
			if err != nil {
				t.Fatal(err)
			}
			cl, err := NewClient(admin.Server(), plain, false)
			if err != nil {
				t.Fatal(err)
			}
			m := newTestModel(t, Options{Client: cl})
			if n := hits.Load(); n != 0 {
				t.Fatalf("startup on the Status tab asked for the opt-outs %d times", n)
			}
			press(m, "7")
			if hits.Load() != 1 || !m.optOutsOff() {
				t.Fatalf("opening the Queue tab: %d requests, err %v", hits.Load(), m.optOutsErr)
			}
			tickNow(m)
			tickNow(m)
			m.Update(doneMsg{notice: "ok"})
			if n := hits.Load(); n != 1 {
				t.Fatalf("the tick kept polling /optouts after %v: %d requests", m.optOutsErr, n)
			}
			press(m, "o")
			press(m, "r")
			if n := hits.Load(); n != 3 {
				t.Fatalf("o and r must retry: %d requests", n)
			}
			if view := flat(m.View()); strings.Contains(view, "· o back") {
				t.Fatalf("the card must hide the count:\n%s", view)
			}

			for _, k := range []string{"a", "x", "j", "G"} {
				m.notice = ""
				press(m, k)
				if m.mode != modeNormal || !m.noticeErr || !strings.Contains(m.notice, c.want) {
					t.Fatalf("%s with %v: mode %v, notice %q", k, m.optOutsErr, m.mode, m.notice)
				}
			}
		})
	}
}

func TestOptOutsKeysRefusedWhileTheListFailed(t *testing.T) {
	m := newTestModel(t, Options{})
	m.mode, m.tab, m.queueOptOuts = modeNormal, tabQueue, true
	m.Update(optOutsMsg{seq: m.optOutsReq.start(), list: []store.OptOut{{Chat: maria}, {Chat: john}}})
	m.Update(optOutsMsg{seq: m.optOutsReq.start(), err: errors.New("context deadline exceeded")})
	if view := flat(m.View()); !strings.Contains(view, "Opt-outs 2 · o back") {
		t.Fatalf("a one-off error must keep the last count:\n%s", view)
	}
	for _, k := range []string{"x", "a", "k", "G"} {
		press(m, k)
		if m.mode != modeNormal || m.confirmOptOut != "" || !m.noticeErr || !strings.Contains(m.notice, "didn't load: context deadline exceeded") {
			t.Fatalf("%s on a list that didn't load: mode %v, confirm %q, notice %q", k, m.mode, m.confirmOptOut, m.notice)
		}
	}
	m.Update(optOutsMsg{seq: m.optOutsReq.start(), list: []store.OptOut{{Chat: maria}}})
	press(m, "x")
	if m.mode != modeConfirmRemoveOptOut || m.confirmOptOut != maria {
		t.Fatalf("x after the list loaded again: mode %v, notice %q", m.mode, m.notice)
	}
}

func TestQueueListsKeepTheSelectionVisible(t *testing.T) {
	var opts []store.OptOut
	var items []store.OutboxItem
	for i := range 60 {
		opts = append(opts, store.OptOut{Chat: fmt.Sprintf("5511999999%03d@s.whatsapp.net", i), Reason: "STOP", By: "whatsapp"})
		items = append(items, store.OutboxItem{ID: int64(9000 + i), Chat: maria, Status: store.OutboxQueued})
	}
	for _, w := range []int{100, 120, 180} {
		m := newTestModel(t, Options{})
		m.mode, m.tab = modeNormal, tabQueue
		m.Update(tea.WindowSizeMsg{Width: w, Height: 40})
		m.stats.Queue = bot.QueueStats{OutboxCounts: store.OutboxCounts{Queued: 60, Sent: 3, OldestQueued: time.Now().Add(-time.Hour)}, Running: true, QuietHours: "21:00-08:00", Timezone: "UTC", LastError: "boom"}
		m.queue = QueuePage{Items: items}
		press(m, "G")
		if view := ansi.Strip(m.View()); !strings.Contains(view, "9059") {
			t.Fatalf("width %d: the last queue item is off screen:\n%s", w, view)
		}
		press(m, "o")
		m.Update(optOutsMsg{seq: m.optOutsReq.start(), list: opts})
		press(m, "G")
		if view := ansi.Strip(m.View()); !strings.Contains(view, "+5511999999059") {
			t.Fatalf("width %d: the last opt-out is off screen:\n%s", w, view)
		}
	}
}

func TestQueueFootersFitAt120(t *testing.T) {
	m := newTestModel(t, Options{Profiles: []Profile{{Name: "prod"}}})
	m.mode, m.tab = modeNormal, tabQueue
	m.stats.Queue.Paused = true
	for _, k := range []string{"", "o", "/"} {
		if k == "/" {
			press(m, "/", "m", "enter")
		} else if k != "" {
			press(m, k)
		}
		keys := flat(m.viewKeys())
		if !strings.HasSuffix(keys, "r refresh q quit") || strings.Contains(keys, "…") {
			t.Fatalf("after %q the footer is cut at 120 columns: %q", k, keys)
		}
	}
}
