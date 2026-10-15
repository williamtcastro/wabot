package admin

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
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

// contactsServer is the real API (api.NewHandler) on a scratch database. With gone set the
// /contacts/ routes answer 404, as on a server older than the contact list (whose GET /contacts
// still answers, from the address book).
func contactsServer(t *testing.T, gone bool) (*bot.Bot, *Client) {
	t.Helper()
	b, err := bot.New(context.Background(), filepath.Join(t.TempDir(), "wame.db"), waLog.Noop, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.InitSettings(bot.Settings{Rate: 20, ChatGap: time.Second, MaxWait: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}
	a := &auth.Authenticator{Store: b.Store, Owner: owner}
	mux := http.NewServeMux()
	mux.Handle("/", api.NewHandler(b, a, "test"))
	if gone {
		mux.HandleFunc("/contacts/", http.NotFound)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, owner, false)
	if err != nil {
		t.Fatal(err)
	}
	return b, c
}

func seedContacts(t *testing.T, b *bot.Bot) {
	t.Helper()
	for _, c := range []struct {
		number, name, notes string
		tags                []string
	}{
		{"5511999999999", "Maria", "prefers mornings", []string{"clients"}},
		{"5511988887777", "John", "", []string{"vip"}},
	} {
		in := bot.ContactInput{Name: &c.name, Tags: &c.tags}
		if c.notes != "" {
			in.Notes = &c.notes
		}
		if _, err := b.PatchContact(t.Context(), "api", c.number, in); err != nil {
			t.Fatal(err)
		}
	}
}

func ownOf(t *testing.T, b *bot.Bot, number string) *store.OwnContact {
	t.Helper()
	v, err := b.ContactOf(t.Context(), number)
	if err != nil {
		t.Fatal(err)
	}
	return v.Own
}

// selectContact marks the contact with that JID in the visible list.
func selectContact(t *testing.T, m *model, jid string) {
	t.Helper()
	i := slices.IndexFunc(m.visibleContacts(), func(c bot.Contact) bool { return c.JID == jid })
	if i < 0 {
		t.Fatalf("%s isn't listed: %+v", jid, m.contacts)
	}
	m.contactSel = i
	run(m, m.contactSelected())
}

func TestContactsTabListsAndEdits(t *testing.T) {
	b, c := contactsServer(t, false)
	seedContacts(t, b)
	m := newTestModel(t, Options{Client: c})
	press(m, "9")
	view := flat(m.View())
	for _, want := range []string{"9 Contacts", "contacts · 2 of 2", "John", "Maria", "own", "+5511999999999", "clients", "vip", "/ filter", "e edit", "x delete", "I/E csv", "W refresh (2-4 lookups)"} {
		if !strings.Contains(view, want) {
			t.Errorf("contacts view lacks %q:\n%s", want, view)
		}
	}

	selectContact(t, m, maria)
	if view := flat(m.View()); !strings.Contains(view, "prefers mornings") || !strings.Contains(view, "Maria (own)") {
		t.Fatalf("the card lacks the notes or names:\n%s", view)
	}

	press(m, "e")
	if m.mode != modeEditContact || len(m.form) != 3 || m.form[0].Value() != "Maria" || m.form[1].Value() != "clients" || m.form[2].Value() != "prefers mornings" {
		t.Fatalf("edit form: mode %v, %d fields", m.mode, len(m.form))
	}
	notes, tags := "call after 10", []string{"late"}
	if _, err := b.PatchContact(t.Context(), "other-panel", "5511999999999", bot.ContactInput{Notes: &notes, AddTags: tags}); err != nil {
		t.Fatal(err)
	}
	m.form[0].SetValue("Maria Silva")
	m.form[1].SetValue("clients VIP")
	press(m, "enter", "enter", "enter")
	o := ownOf(t, b, "5511999999999")
	if m.mode != modeNormal || m.noticeErr || o == nil || o.Name != "Maria Silva" || !slices.Equal(o.Tags, []string{"clients", "late", "vip"}) || o.Notes != "call after 10" {
		t.Fatalf("after the edit: mode %v, notice %q, server %+v", m.mode, m.notice, o)
	}
	if view := flat(m.View()); !strings.Contains(view, "Maria Silva") || !strings.Contains(view, "call after 10") {
		t.Fatalf("the list didn't reload after the edit:\n%s", view)
	}

	selectContact(t, m, maria)
	press(m, "e", "enter", "enter", "enter")
	if m.noticeErr || m.notice != "nothing changed" {
		t.Fatalf("an unchanged form must send nothing: %q", m.notice)
	}

	press(m, "/", "v", "i", "p", "enter")
	if vis := m.visibleContacts(); len(vis) != 2 {
		t.Fatalf("filter by tag vip: %+v", vis)
	}
	press(m, "esc", "/", "j", "o", "h", "n", "enter")
	if vis := m.visibleContacts(); len(vis) != 1 || vis[0].JID != john {
		t.Fatalf("filter: %+v", vis)
	}
	if view := flat(m.View()); !strings.Contains(view, "John (own)") || strings.Contains(view, "loading") {
		t.Fatalf("the card must follow the selection a filter moved:\n%s", view)
	}
	press(m, "esc", "t", "clients", "enter")
	if m.contactQuery != "" || len(m.contacts) != 1 || m.contacts[0].JID != maria || !strings.Contains(flat(m.View()), "tagged clients") {
		t.Fatalf("tag filter: %+v", m.contacts)
	}
	press(m, "esc")
	if m.contactTag != "" || len(m.contacts) != 2 {
		t.Fatalf("esc must clear the tag filter: %q, %d", m.contactTag, len(m.contacts))
	}
}

func TestContactsDeleteActsOnTheCapturedNumber(t *testing.T) {
	b, c := contactsServer(t, false)
	seedContacts(t, b)
	m := newTestModel(t, Options{Client: c})
	press(m, "9")
	selectContact(t, m, maria)
	press(m, "x", "n")
	if ownOf(t, b, "5511999999999") == nil || m.mode != modeNormal {
		t.Fatal("deleted without confirmation")
	}
	press(m, "x")
	if m.mode != modeConfirmDeleteContact || !strings.Contains(flat(m.View()), "Delete your fields for Maria (+5511999999999)?") {
		t.Fatalf("confirm dialog:\n%s", ansi.Strip(m.View()))
	}
	m.Update(contactsMsg{seq: m.contactsReq.start(), list: []bot.Contact{{JID: john, Names: bot.Names{Display: "John"}}, {JID: ana, Names: bot.Names{Display: "Ana"}}}})
	press(m, "y")
	if ownOf(t, b, "5511999999999") != nil || ownOf(t, b, "5511988887777") == nil {
		t.Fatalf("delete must act on the number marked when the dialog opened: notice %q", m.notice)
	}
	if m.noticeErr || len(m.contacts) != 1 || m.contacts[0].JID != john {
		t.Fatalf("after delete: %+v, %q", m.contacts, m.notice)
	}

	m.confirmContact, m.confirmContactLabel, m.mode = maria, "Maria", modeConfirmDeleteContact
	press(m, "y")
	if !m.noticeErr || !strings.Contains(m.notice, "nothing of yours was stored for Maria") {
		t.Fatalf("deleting twice: %q", m.notice)
	}
}

func TestContactsImportDryRunThenImport(t *testing.T) {
	b, c := contactsServer(t, false)
	seedContacts(t, b)
	m := newTestModel(t, Options{Client: c})
	press(m, "9")
	path := filepath.Join(t.TempDir(), "people.csv")
	if err := os.WriteFile(path, []byte("number,name,tags\n5521987654321,Ana,clients\n5511988887777,John,vip clients\n5511999999999,Maria,clients\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	press(m, "I", path, "enter")
	if m.mode != modeConfirmImport || ownOf(t, b, "5521987654321") != nil {
		t.Fatalf("the dry run must ask first and store nothing: mode %v, notice %q", m.mode, m.notice)
	}
	if view := flat(m.View()); !strings.Contains(view, "1 added, 1 updated, 1 unchanged") {
		t.Fatalf("dry run summary:\n%s", view)
	}
	if err := os.WriteFile(path, []byte("number,name\n5521987654321,Changed after the check\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	press(m, "y")
	if o := ownOf(t, b, "5521987654321"); o == nil || o.Name != "Ana" || m.noticeErr {
		t.Fatalf("import must send the bytes that were checked: %+v, %q", o, m.notice)
	}
	if !strings.Contains(m.notice, "1 added, 1 updated, 1 unchanged") || len(m.contacts) != 3 || m.importData != nil {
		t.Fatalf("after import: %q, %d contacts", m.notice, len(m.contacts))
	}

	press(m, "I", path, "enter", "n")
	if o := ownOf(t, b, "5521987654321"); o.Name != "Ana" || m.notice != "nothing imported" {
		t.Fatalf("a declined import changed things: %+v, %q", o, m.notice)
	}
	bad := filepath.Join(t.TempDir(), "bad.csv")
	if err := os.WriteFile(bad, []byte("number,name\n120363000000000000@g.us,Family\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	press(m, "I", bad, "enter")
	if m.mode != modeNormal || !m.noticeErr || !strings.Contains(m.notice, "line 2") {
		t.Fatalf("a bad file: mode %v, %q", m.mode, m.notice)
	}
	press(m, "I", filepath.Join(t.TempDir(), "missing.csv"), "enter")
	if m.mode != modeNormal || !m.noticeErr {
		t.Fatalf("a missing file: %q", m.notice)
	}
}

func TestContactsExportWritesANewFile(t *testing.T) {
	b, c := contactsServer(t, false)
	seedContacts(t, b)
	m := newTestModel(t, Options{Client: c})
	press(m, "9")
	path := filepath.Join(t.TempDir(), "out.csv")
	press(m, "E", path, "enter")
	data, err := os.ReadFile(path)
	if err != nil || m.noticeErr {
		t.Fatalf("export: %v, %q", err, m.notice)
	}
	if got := string(data); !strings.HasPrefix(got, "number,name,tags,notes,language,time_zone\n") || !strings.Contains(got, "5511999999999,Maria,clients,prefers mornings") {
		t.Fatalf("export file:\n%s", got)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("export file mode %v", fi.Mode())
	}
	press(m, "E", path, "enter")
	if !m.noticeErr || !strings.Contains(m.notice, "already exists") {
		t.Fatalf("exporting over a file: %q", m.notice)
	}
}

func TestContactsSanitized(t *testing.T) {
	m := newTestModel(t, Options{})
	m.mode, m.tab = modeNormal, tabContacts
	hostile := "Ma\x1b]52;c;cHduZWQ=\x07ria\u202e"
	m.Update(contactsMsg{seq: m.contactsReq.start(), list: []bot.Contact{
		{JID: maria, Phone: "+5511999999999\x1b[2J", Names: bot.Names{Display: hostile, Source: "push\x1b[31m", Push: hostile}, Tags: []string{"vip\x1b[5m"}},
	}})
	no := false
	m.Update(contactViewMsg{seq: m.contactViewReq.start(), jid: maria, v: bot.Profile{
		JID: maria, LID: "123456789@lid\x1b[3J", Names: bot.Names{Display: hostile, Own: hostile, Push: hostile},
		Own: bot.ProfileOwn{Notes: "line one\nline\x1b[2J two\u2066", UpdatedBy: "evil\x1b[1A", UpdatedAt: time.Now()},
		WhatsApp: bot.ProfileWhatsApp{About: "hi\x1b[4m", VerifiedName: "Shop\u202d", Error: "boom\x1b[6m",
			Picture:  &bot.ProfilePicture{ID: "17\x1b[7m"},
			Business: &bot.BusinessInfo{Address: "Rua\x1b[8m", Options: map[string]string{"k\x1b[9m": "v\u2067"}}},
		Activity: &bot.ProfileActivity{GroupsInCommon: []store.GroupRef{{JID: "120363000000000000@g.us", Name: "Fam\x1b[0Kily"}}},
		OptedOut: &no, Tokens: []string{"bot\x1b[1B"},
	}})
	raw := m.View()
	for _, bad := range []string{"]52;", "[2J", "[31m", "[5m", "[1A", "[3J", "[4m", "[6m", "[7m", "[8m", "[9m", "[0K", "[1B", "\u202e", "\u2066", "\u202d", "\u2067"} {
		if strings.Contains(raw, bad) {
			t.Fatalf("contacts view not sanitized (%q):\n%s", bad, raw)
		}
	}
	if view := flat(raw); !strings.Contains(view, "Maria") || !strings.Contains(view, "line one line two") {
		t.Fatalf("sanitized text is gone:\n%s", view)
	}
	press(m, "e")
	if v := m.form[0].Value(); strings.ContainsAny(v, "\x1b\u202e") {
		t.Fatalf("the edit form shows raw text: %q", v)
	}
	press(m, "esc")

	m.Update(contactTagsMsg{tags: []store.TagCount{{Tag: "v\x1b]52;c;cHduZWQ=\x07ip", Count: 1}}})
	press(m, "t", "v")
	if raw := m.View(); strings.Contains(raw, "]52;") || !strings.Contains(ansi.Strip(raw), "vip 1") {
		t.Fatalf("tag suggestions not sanitized:\n%s", raw)
	}
}

func TestContactsReadOnlyWithoutAdmin(t *testing.T) {
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
	m := newTestModel(t, Options{Client: reader})
	press(m, "9")
	selectContact(t, m, maria)
	view := flat(m.View())
	if !strings.Contains(view, "Maria") || !strings.Contains(view, "read-only") || strings.Contains(view, "prefers mornings") || strings.Contains(view, "e edit") {
		t.Fatalf("read-only view:\n%s", view)
	}
	for _, k := range []string{"e", "x", "I", "E"} {
		m.notice = ""
		press(m, k)
		if m.mode != modeNormal || !m.noticeErr || !strings.Contains(m.notice, "admin scope") {
			t.Fatalf("%s without admin: mode %v, notice %q", k, m.mode, m.notice)
		}
	}

	m.contactDenied, m.tokensErr = false, nil
	m.startContactForm(m.contacts[slices.IndexFunc(m.contacts, func(c bot.Contact) bool { return c.JID == maria })])
	m.form[0].SetValue("Mary")
	press(m, "enter", "enter")
	if !m.contactDenied || !m.noticeErr || !strings.Contains(m.notice, "admin scope") || ownOf(t, b, "5511999999999").Name != "Maria" {
		t.Fatalf("a 403 on a write must turn the tab read-only: %q", m.notice)
	}
}

func TestContactsOnAnOlderServer(t *testing.T) {
	_, c := contactsServer(t, true)
	m := newTestModel(t, Options{Client: c})
	press(m, "9")
	if view := flat(m.View()); !strings.Contains(view, "This server doesn't have your own contact list (GET /contacts/tags) yet") {
		t.Fatalf("no update hint:\n%s", view)
	}
	for _, k := range []string{"e", "x", "I", "j", "/"} {
		press(m, k)
		if m.mode != modeNormal || !m.noticeErr || !strings.Contains(m.notice, "update wame") {
			t.Fatalf("%s on an older server: mode %v, notice %q", k, m.mode, m.notice)
		}
	}
	for _, msg := range []any{contactDoneMsg{err: &APIError{Code: http.StatusNotFound}}, importCheckedMsg{err: &APIError{Code: http.StatusNotFound}}} {
		m.notice = ""
		m.Update(msg)
		if !m.noticeErr || !strings.Contains(m.notice, "update wame") {
			t.Fatalf("a 404 from a write (%T): %q", msg, m.notice)
		}
	}
}

func TestChatsPJumpsToTheContact(t *testing.T) {
	b, c := contactsServer(t, false)
	seedContacts(t, b)
	m := newTestModel(t, Options{Client: c})
	m.tab, m.chats, m.chatSel = tabChats, []store.Chat{{JID: john, Name: "John"}, {JID: ana, Name: "~Ana"}, {JID: "120363000000000000@g.us", Name: "Family"}}, 0
	press(m, "p")
	if got, _ := m.selectedContact(); m.tab != tabContacts || got.JID != john {
		t.Fatalf("p on John: tab %v, selected %+v", m.tab, got)
	}

	m.tab, m.chatSel = tabChats, 1
	press(m, "p")
	got, _ := m.selectedContact()
	if m.tab != tabContacts || got.JID != ana || !strings.Contains(flat(m.View()), "+5521987654321") {
		t.Fatalf("p on someone not in the list yet: tab %v, selected %+v", m.tab, got)
	}
	press(m, "e")
	m.form[0].SetValue("Ana")
	press(m, "enter", "enter", "enter")
	if o := ownOf(t, b, "5521987654321"); o == nil || o.Name != "Ana" {
		t.Fatalf("editing the jumped-to person: %+v, %q", o, m.notice)
	}

	m.tab, m.chatSel = tabChats, 2
	press(m, "p")
	if m.tab != tabChats || !m.noticeErr || !strings.Contains(m.notice, "not groups") {
		t.Fatalf("p on a group: tab %v, %q", m.tab, m.notice)
	}
}

func TestContactsHeaderAndFootersFit(t *testing.T) {
	c, err := NewClient("http://127.0.0.1:1", "t", false)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, width := range []int{100, 120} {
		m := newModel(Options{Client: c, Profile: "maria-server", Profiles: []Profile{{Name: "maria-server"}}})
		m.width, m.height, m.tab = width, 30, tabContacts
		m.status, m.checked, m.latency = bot.Status{Connected: true, LoggedIn: true}, now, 42*time.Millisecond
		m.stats.Queue = bot.QueueStats{OutboxCounts: store.OutboxCounts{Queued: 12}, Paused: true, QuietHours: "21:00-08:00", QuietUntil: now.Add(time.Hour)}
		header := ansi.Strip(m.viewHeader())
		if !strings.Contains(header, "● connected") || !strings.Contains(header, "☾ quiet until") || !strings.Contains(header, "9 Contacts") ||
			strings.Contains(header, "…") || ansi.StringWidth(header) > width {
			t.Fatalf("width %d: %q", width, header)
		}
	}
	for _, width := range []int{100, 120} {
		m := newModel(Options{Client: c, Profiles: []Profile{{Name: "prod"}}})
		m.width, m.height = width, 30
		for _, set := range []func(){
			func() { m.tab = tabContacts },
			func() { m.tokensErr = &APIError{Code: http.StatusForbidden} },
			func() { m.tab = tabChats },
		} {
			set()
			keys := flat(m.viewKeys())
			if !strings.HasSuffix(keys, "r refresh q quit") || strings.Contains(keys, "…") || ansi.StringWidth(keys) > width {
				t.Fatalf("tab %v: the footer is cut at %d columns: %q", m.tab, width, keys)
			}
			if m.tab == tabContacts && (!strings.Contains(keys, "/ filter") || width == 120 && !m.contactsReadOnly() && !strings.Contains(keys, "W refresh (2-4 lookups)") ||
				m.contactsReadOnly() && width == 120 && !strings.Contains(keys, "j/k move")) {
				t.Fatalf("the footer drops keys that fit at %d columns: %q", width, keys)
			}
		}
	}
}

func TestContactsNotesWithLineBreaksKept(t *testing.T) {
	b, c := contactsServer(t, false)
	seedContacts(t, b)
	notes := "a\nb"
	if _, err := b.PatchContact(t.Context(), "api", "5511999999999", bot.ContactInput{Notes: &notes}); err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t, Options{Client: c})
	press(m, "9")
	selectContact(t, m, maria)
	if !strings.Contains(flat(m.View()), "a b") {
		t.Fatalf("the card must fold the notes onto one line:\n%s", flat(m.View()))
	}
	press(m, "e")
	if len(m.form) != 2 || !strings.Contains(flat(m.View()), "line breaks") {
		t.Fatalf("notes with line breaks must not be editable in the one-line form: %d fields\n%s", len(m.form), flat(m.View()))
	}
	m.form[0].SetValue("Maria Silva")
	press(m, "enter", "enter")
	if o := ownOf(t, b, "5511999999999"); o == nil || o.Name != "Maria Silva" || o.Notes != notes {
		t.Fatalf("after the edit: notice %q, server %+v", m.notice, o)
	}
}

func TestContactsTabReentryReloadsTheCard(t *testing.T) {
	b, c := contactsServer(t, false)
	seedContacts(t, b)
	m := newTestModel(t, Options{Client: c})
	press(m, "9")
	selectContact(t, m, maria)
	notes := "call after 10"
	if _, err := b.PatchContact(t.Context(), "other-panel", "5511999999999", bot.ContactInput{Notes: &notes}); err != nil {
		t.Fatal(err)
	}
	press(m, "2", "9")
	if view := flat(m.View()); !strings.Contains(view, "call after 10") {
		t.Fatalf("the card is stale after coming back to the tab:\n%s", view)
	}
	press(m, "e")
	if len(m.form) != 3 || m.form[2].Value() != "call after 10" {
		t.Fatalf("the edit form starts from a stale card: %d fields", len(m.form))
	}
}

func TestContactsTagPromptCompletesEveryTag(t *testing.T) {
	b, c := contactsServer(t, false)
	seedContacts(t, b)
	m := newTestModel(t, Options{Client: c})
	press(m, "9", "t", "c", "tab")
	if v := m.prompt.Value(); v != "clients" {
		t.Fatalf("first tag: %q", v)
	}
	press(m, " ", "v", "tab")
	if v := m.prompt.Value(); v != "clients vip" {
		t.Fatalf("second tag: %q", v)
	}
	press(m, " ")
	if got := m.prompt.MatchedSuggestions(); slices.Contains(got, "clients vip clients") {
		t.Fatalf("a typed tag is suggested again: %q", got)
	}
}

func TestContactsCardDoesNotShowTheLastPersonsError(t *testing.T) {
	b, c := contactsServer(t, false)
	seedContacts(t, b)
	m := newTestModel(t, Options{Client: c})
	press(m, "9")
	m.contactView, m.contactViewErr = bot.Profile{JID: maria}, errors.New("maria's card failed")
	m.contactSel = slices.IndexFunc(m.visibleContacts(), func(c bot.Contact) bool { return c.JID == john })
	if view := flat(m.View()); strings.Contains(view, "maria's card failed") || !strings.Contains(view, "loading") {
		t.Fatalf("John's card shows Maria's error:\n%s", view)
	}
}

func TestContactTagsOlderAnswerDropped(t *testing.T) {
	_, c := contactsServer(t, false)
	m := newTestModel(t, Options{Client: c})
	older, newer := m.contactTagsReq.start(), m.contactTagsReq.start()
	m.Update(contactTagsMsg{seq: newer, tags: []store.TagCount{{Tag: "vip", Count: 1}}})
	m.Update(contactTagsMsg{seq: older, err: &APIError{Code: http.StatusForbidden}})
	if m.contactTagsErr != nil || len(m.contactTags) != 1 || m.contactsReadOnly() {
		t.Fatalf("an older answer overwrote a newer one: %v %+v", m.contactTagsErr, m.contactTags)
	}
}

func TestContactsFilterSearchesTheServerWhenCapped(t *testing.T) {
	b, c := contactsServer(t, false)
	seedContacts(t, b)
	m := newTestModel(t, Options{Client: c})
	press(m, "9")
	m.contactsCapped = true
	if !strings.Contains(flat(m.View()), "searches the server") {
		t.Fatalf("no hint that the list is capped:\n%s", flat(m.View()))
	}
	press(m, "/", "j", "o", "h", "n", "enter")
	if len(m.contacts) != 1 || m.contacts[0].JID != john || m.contactsQ != "john" || !m.contactsCapped {
		t.Fatalf("the filter didn't go to the server: %+v", m.contacts)
	}
	press(m, "esc")
	if len(m.contacts) != 2 || m.contactsQ != "" || m.contactsCapped {
		t.Fatalf("esc must reload everyone: %+v", m.contacts)
	}
}
