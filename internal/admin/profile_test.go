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

	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/williamtcastro/wame/internal/api"
	"github.com/williamtcastro/wame/internal/auth"
	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
)

func TestContactsProfileCard(t *testing.T) {
	b, c := contactsServer(t, false)
	seedContacts(t, b)
	now := time.Now()
	if err := b.Store.PutCachedProfile(store.CachedProfile{
		JID: maria, OnWhatsApp: 1, LID: "123456789012345@lid", InfoAt: now, AboutAt: now,
		About:     "Hey\x1b]52;c;cHduZWQ=\x07 there\u202e\nsecond\x1b[2J line",
		PictureID: "1712345678", PictureURL: "https://example.invalid/p.jpg", PictureAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t, Options{Client: c})
	press(m, "9")
	selectContact(t, m, maria)
	raw := m.View()
	for _, bad := range []string{"]52;", "\x1b[2J", "\u202e"} {
		if strings.Contains(raw, bad) {
			t.Fatalf("the profile card isn't sanitized (%q):\n%s", bad, raw)
		}
	}
	view := flat(raw)
	for _, want := range []string{"LID 123456789012345@lid", "Hey there second line", "WhatsApp yes", "id 1712345678", "Opted out no", "Tokens none", "Notes prefers mornings"} {
		if !strings.Contains(view, want) {
			t.Errorf("the profile card lacks %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "example.invalid") {
		t.Fatalf("the photo's link is drawn:\n%s", view)
	}
}

func TestContactsRefresh(t *testing.T) {
	b, c := contactsServer(t, false)
	seedContacts(t, b)
	m := newTestModel(t, Options{Client: c})
	press(m, "9")
	selectContact(t, m, maria)
	press(m, "W")
	if !m.noticeErr || !strings.Contains(m.notice, "not refreshed") || !strings.Contains(m.notice, "not logged in") {
		t.Fatalf("W while the server isn't logged in: %q", m.notice)
	}

	no, next := false, time.Now().Add(3*time.Minute)
	m.Update(contactRefreshMsg{seq: m.contactViewReq.start(), jid: maria, label: "Maria", p: bot.Profile{
		JID: maria, LID: "123456789012345@lid", Names: bot.Names{Display: "Maria", Own: "Maria", Source: bot.SourceOwn},
		Own: bot.ProfileOwn{Tags: []string{"clients"}}, OptedOut: &no, Refreshed: &no, NextRefreshAt: next,
	}})
	if m.noticeErr || !strings.Contains(m.notice, "next refresh at "+stamp(next, "15:04")) {
		t.Fatalf("refreshed:false: %q", m.notice)
	}
	if !strings.Contains(flat(m.View()), "123456789012345@lid") {
		t.Fatalf("the card didn't take the answer:\n%s", flat(m.View()))
	}

	m.Update(contactRefreshMsg{seq: m.contactViewReq.start(), jid: maria, err: &APIError{Code: http.StatusTooManyRequests, Msg: "rate limited", RetryAfter: 12 * time.Second}})
	if !m.noticeErr || !strings.Contains(m.notice, "in 12s") {
		t.Fatalf("429: %q", m.notice)
	}
	m.Update(contactRefreshMsg{seq: m.contactViewReq.start(), jid: maria, p: bot.Profile{JID: maria}})
	if !m.noticeErr || !strings.Contains(m.notice, "update wame") {
		t.Fatalf("a server that ignores refresh=1: %q", m.notice)
	}
	m.Update(contactRefreshMsg{seq: m.contactViewReq.start(), jid: maria, err: &APIError{Code: http.StatusForbidden}})
	if !m.noticeErr || m.notice != contactsRefreshScope || !m.contactsReadOnly() {
		t.Fatalf("403: %q", m.notice)
	}
	m.notice = ""
	press(m, "W")
	if m.notice != contactsRefreshScope {
		t.Fatalf("W on a read-only tab: %q", m.notice)
	}
}

func TestContactsRefreshNeedsAdmin(t *testing.T) {
	b, c := contactsServer(t, false)
	seedContacts(t, b)
	plain, _, err := auth.Issue(b.Store, auth.Request{Name: "reader", Scopes: []auth.Scope{auth.Read}})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewClient(c.Server(), plain, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.RefreshContact(t.Context(), "5511999999999"); !isForbidden(err) {
		t.Fatalf("a read token refreshing: %v", err)
	}
	m := newTestModel(t, Options{Client: reader})
	press(m, "9")
	selectContact(t, m, maria)
	view := flat(m.View())
	if !strings.Contains(view, "need the admin scope") || strings.Contains(view, "Opted out") || strings.Contains(view, "W refresh") {
		t.Fatalf("a read token's card:\n%s", view)
	}
	press(m, "W")
	if m.notice != contactsRefreshScope {
		t.Fatalf("W with a read token: %q", m.notice)
	}
}

// A server from before profiles: the list and tags answer, GET /contacts/{number} doesn't.
func TestContactsProfileOnAnOlderServer(t *testing.T) {
	b, err := bot.New(context.Background(), filepath.Join(t.TempDir(), "wame.db"), waLog.Noop, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.InitSettings(bot.Settings{Rate: 20, ChatGap: time.Second, MaxWait: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}
	seedContacts(t, b)
	h := api.NewHandler(b, &auth.Authenticator{Store: b.Store, Owner: owner}, "test")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/contacts/") && r.URL.Path != "/contacts/tags" {
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
	press(m, "9")
	selectContact(t, m, maria)
	if view := flat(m.View()); !strings.Contains(view, "update wame on it") {
		t.Fatalf("no update hint on the card:\n%s", view)
	}
	press(m, "W")
	if !m.noticeErr || !strings.Contains(m.notice, "update wame") {
		t.Fatalf("W on an older server: %q", m.notice)
	}
}

func TestClientReadsAProfileFromBeforeProfiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jid":"5511999999999@s.whatsapp.net","phone":"+5511999999999","name":"Maria","name_source":"own","own_name":"Maria","tags":["clients"],
			"own":{"jid":"5511999999999@s.whatsapp.net","name":"Maria","tags":["clients"],"notes":"prefers mornings"}}`)
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, owner, false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.Contact(t.Context(), "5511999999999")
	if err != nil || p.Names.Display != "Maria" || p.Names.Source != bot.SourceOwn || !slices.Equal(p.Own.Tags, []string{"clients"}) || p.Own.Notes != "prefers mornings" {
		t.Fatalf("old answer: %+v, %v", p, err)
	}
}
