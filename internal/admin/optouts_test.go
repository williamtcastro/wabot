package admin

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
	if !strings.Contains(view, "Opt-outs 2 · o lists them") || !strings.Contains(view, "o opt-outs") {
		t.Fatalf("queue view lacks the opt-out count or key:\n%s", view)
	}

	press(m, "o")
	raw := m.View()
	view = flat(raw)
	for _, want := range []string{"+5511999999999", "Maria", "keyword STOP", "+5511988887777", "added by evil", "a add", "x remove", "o/esc queue"} {
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
