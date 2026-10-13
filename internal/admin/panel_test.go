package admin

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/williamtcastro/wame/internal/auth"
	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
)

func TestAuditEntriesAreSanitized(t *testing.T) {
	_, c := newServer(t)
	m := newTestModel(t, Options{Client: c})
	press(m, "4")
	m.Update(auditMsg{entries: []store.AuditEntry{{ID: 1, At: time.Now(), Actor: "bot\x1b[31m", Action: "send",
		Target: "\x1b]0;pwned\x07+5511999999999", Detail: "file ‮gnp.exe", Error: "boom\x1b[2J", OK: false}}})
	e := m.audit[0]
	if e.Actor != "bot" || e.Target != "+5511999999999" || e.Detail != "file gnp.exe" || e.Error != "boom" {
		t.Fatalf("entry not sanitized: %+v", e)
	}
	press(m, "1")
	for _, view := range []string{m.View(), func() string { press(m, "4"); return m.View() }()} {
		if strings.Contains(view, "pwned") || strings.Contains(view, "\x1b[2J") || strings.Contains(view, "‮") {
			t.Fatalf("raw audit text on screen:\n%q", view)
		}
	}
}

func TestNoticesAndErrorsAreSanitized(t *testing.T) {
	_, c := newServer(t)
	m := newTestModel(t, Options{Client: c})
	m.setNotice("webhook said \x1b]52;c;cHduZWQ=\x07no", true)
	if m.notice != "webhook said no" {
		t.Fatalf("notice = %q", m.notice)
	}
	press(m, "3")
	m.Update(tokensMsg{err: &APIError{Code: 500, Msg: "bad \x1b[2Jthing"}})
	if v := m.View(); strings.Contains(v, "\x1b[2J") || !strings.Contains(ansi.Strip(v), "bad thing") {
		t.Fatalf("error card not sanitized:\n%q", v)
	}
}

func TestConfirmActsOnTheTokenItOpenedFor(t *testing.T) {
	b, c := newServer(t)
	for _, name := range []string{"maria-bot", "john-bot"} {
		if _, _, err := auth.Issue(b.Store, auth.Request{Name: name, Scopes: []auth.Scope{auth.Read}}); err != nil {
			t.Fatal(err)
		}
	}
	m := newTestModel(t, Options{Client: c})
	press(m, "3")
	target := m.tokens[0].Name
	press(m, "x")
	if m.mode != modeConfirmRevoke {
		t.Fatal("x didn't ask")
	}
	reordered := []store.Token{m.tokens[1], m.tokens[0]}
	m.Update(tokensMsg{tokens: reordered})
	if m.tokens[m.tokSel].Name != target {
		t.Fatalf("selection didn't follow %s across a reload", target)
	}
	m.Update(tokensMsg{tokens: reordered[:1]})
	if !strings.Contains(ansi.Strip(m.View()), "Revoke "+target+"?") {
		t.Fatalf("dialog lost its token:\n%s", ansi.Strip(m.View()))
	}
	press(m, "y")
	tokens, _ := b.Store.Tokens()
	for _, tok := range tokens {
		if tok.Revoked != (tok.Name == target) {
			t.Fatalf("%s revoked = %v, want only %s revoked", tok.Name, tok.Revoked, target)
		}
	}
}

func TestCancelBatchActsOnTheBatchItOpenedFor(t *testing.T) {
	b, c := newServer(t)
	if _, err := b.Enqueue("api", "", "promo", "", []bot.QueueRequest{{To: "5511999999999", Text: "Hi Maria"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Enqueue("api", "", "news", "", []bot.QueueRequest{{To: "5511988887777", Text: "Hi John"}}); err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t, Options{Client: c})
	press(m, "7")
	batch := m.queue.Items[0].Batch
	press(m, "X")
	m.queue.Items = []store.OutboxItem{m.queue.Items[1], m.queue.Items[0]}
	press(m, "y")
	cnt, _ := b.Store.CountOutbox(store.OutboxFilter{Batch: batch})
	other, _ := b.Store.CountOutbox(store.OutboxFilter{})
	if cnt.Canceled != 1 || other.Canceled != 1 {
		t.Fatalf("canceled %d in %s, %d overall; want only %s", cnt.Canceled, batch, other.Canceled, batch)
	}
}

func TestEscClearsTheAuditFilter(t *testing.T) {
	_, c := newServer(t)
	m := newTestModel(t, Options{Client: c})
	press(m, "4", "/", "r", "e", "v")
	if m.auditQuery != "rev" {
		t.Fatalf("filter while typing = %q", m.auditQuery)
	}
	press(m, "esc")
	if m.auditQuery != "" || m.mode != modeNormal {
		t.Fatalf("esc left the filter at %q", m.auditQuery)
	}
}

func TestPollsDontPileUpOrGoBackwards(t *testing.T) {
	_, c := newServer(t)
	rang := 0
	m := newTestModel(t, Options{Client: c, Bell: func() { rang++ }})
	slow := m.statusReq.start()
	started := m.statusReq.started
	m.Update(tickMsg{})
	if m.statusReq.started != started {
		t.Fatal("the tick started a status request while one was pending")
	}
	healthy := bot.Status{State: "connected", Connected: true, LoggedIn: true}
	m.Update(statusMsg{seq: m.statusReq.start(), s: healthy})
	m.Update(statusMsg{seq: slow, err: context.DeadlineExceeded})
	if rang != 0 || m.statusErr != nil {
		t.Fatalf("an old timeout after a newer success: rang %d, err %v", rang, m.statusErr)
	}
	if m.statusReq.busy() {
		t.Fatal("answers didn't clear the pending count")
	}
}

func TestStaleOlderAuditPageIsDropped(t *testing.T) {
	_, c := newServer(t)
	m := newTestModel(t, Options{Client: c})
	m.Update(auditMsg{entries: []store.AuditEntry{{ID: 9}, {ID: 8}}})
	m.Update(auditMsg{entries: []store.AuditEntry{{ID: 3}}, older: true, before: 5})
	if len(m.audit) != 2 {
		t.Fatalf("an older page for a list that was since reloaded was appended: %+v", m.audit)
	}
	m.Update(auditMsg{entries: []store.AuditEntry{{ID: 7}}, older: true, before: 8})
	if len(m.audit) != 3 || m.audit[2].ID != 7 {
		t.Fatalf("the matching older page wasn't appended: %+v", m.audit)
	}
}

func TestUnknownSettingFromANewerServer(t *testing.T) {
	_, c := newServer(t)
	m := newTestModel(t, Options{Client: c})
	press(m, "6")
	m.Update(settingsMsg{settings: []bot.SettingView{{Key: "future_knob", Value: "5"}}})
	if !strings.Contains(ansi.Strip(m.View()), "future_knob") {
		t.Fatalf("unknown setting not shown by key:\n%s", ansi.Strip(m.View()))
	}
	press(m, "enter")
	if m.mode != modeSetting || !strings.Contains(ansi.Strip(m.View()), "✎ future_knob") {
		t.Fatalf("editing an unknown setting:\n%s", ansi.Strip(m.View()))
	}
}

func TestTimesShowInTheLocalZone(t *testing.T) {
	_, offset := time.Now().Zone()
	far := time.FixedZone("far", offset+5*3600)
	at := time.Now().Add(-time.Minute).In(far)
	if got, want := stamp(at, "15:04"), at.Local().Format("15:04"); got != want {
		t.Fatalf("stamp = %s, want %s", got, want)
	}
	if got := strings.TrimSpace(when(at)); got != at.Local().Format("15:04") && got != at.Local().Format("Mon 15:04") {
		t.Fatalf("when = %s, want local %s", got, at.Local().Format("15:04"))
	}
}

func TestStreamedMessagesReloadChatsOnce(t *testing.T) {
	_, c := newServer(t)
	m := newTestModel(t, Options{Client: c})
	press(m, "2")
	scheduled := 0
	for i := range 5 {
		if m.onMessage(store.Message{Seq: int64(i + 1), Chat: maria}) != nil {
			scheduled++
		}
	}
	if scheduled != 1 || !m.chatsReload.Pending() {
		t.Fatalf("5 messages scheduled %d reloads", scheduled)
	}
	m.Update(chatsReloadMsg{})
	if m.chatsReload.Pending() {
		t.Fatal("the reload didn't clear the debounce")
	}
}

func TestListKeysAndTabNumbers(t *testing.T) {
	b, c := newServer(t)
	for i := range 3 {
		if _, _, err := auth.Issue(b.Store, auth.Request{Name: fmt.Sprintf("bot-%d", i), Scopes: []auth.Scope{auth.Read}}); err != nil {
			t.Fatal(err)
		}
	}
	m := newTestModel(t, Options{Client: c})
	press(m, "3", "G")
	if m.tokSel != 2 {
		t.Fatalf("G in tokens: sel %d", m.tokSel)
	}
	press(m, "g")
	if m.tokSel != 0 {
		t.Fatalf("g in tokens: sel %d", m.tokSel)
	}
	press(m, "9")
	if m.tab != tabTokens {
		t.Fatal("a digit past the last tab switched tabs")
	}
	if !strings.Contains(ansi.Strip(m.View()), fmt.Sprintf(" 1-%d ", len(tabNames))) {
		t.Fatal("tab hint missing")
	}
}

func TestIsNotFoundSeesWrappedErrors(t *testing.T) {
	if !isNotFound(fmt.Errorf("stats: %w", &APIError{Code: 404})) || isNotFound(&APIError{Code: 500}) {
		t.Fatal("isNotFound")
	}
}
