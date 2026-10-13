package admin

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/williamtcastro/wame/internal/bot"
)

func TestLogsTab(t *testing.T) {
	b, c := newServer(t)
	buf := bot.NewLogBuffer(100)
	b.Logs = buf
	log := slog.New(buf.Wrap(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})))
	log.Debug("group refresh", "count", 3)
	log.Info("connection state", "state", "connected")
	log.Warn("auto-reply failed", "chat", maria, "err", "\x1b]52;c;cHduZWQ=\x07boom\x1b[31m")
	log.Error("save message", "id", "M1")

	m := newTestModel(t, Options{Client: c})
	press(m, "8")
	if m.tab != tabLogs || len(m.logs) != 4 || m.logs[0].Msg != "save message" {
		t.Fatalf("logs = %+v, err %v", m.logs, m.logsErr)
	}
	if !m.logsTicking {
		t.Fatal("opening the tab must start following")
	}
	view := m.View()
	if strings.ContainsAny(ansi.Strip(view), "\x1b\x07") || strings.Contains(view, "]52;") {
		t.Fatal("log text must go through safe()")
	}
	plain := ansi.Strip(view)
	for _, want := range []string{"8 Logs", "auto-reply failed", "chat=" + maria, "following", "pause"} {
		if !strings.Contains(plain, want) {
			t.Errorf("view lacks %q:\n%s", want, plain)
		}
	}

	press(m, "f")
	if m.logsLevel != "info" || len(m.logs) != 3 {
		t.Fatalf("f → info: %q, %d entries", m.logsLevel, len(m.logs))
	}
	press(m, "f")
	if m.logsLevel != "warn" || len(m.logs) != 2 {
		t.Fatalf("f → warn: %q, %d entries", m.logsLevel, len(m.logs))
	}
	press(m, "f", "f")
	if m.logsLevel != "" || len(m.logs) != 4 {
		t.Fatalf("f cycles back to all: %q, %d", m.logsLevel, len(m.logs))
	}

	press(m, "/", "s", "t", "a", "t", "e", "enter")
	if got := m.visibleLogs(); len(got) != 1 || got[0].Msg != "connection state" {
		t.Fatalf("filter: %+v", got)
	}
	press(m, "esc")
	if len(m.visibleLogs()) != 4 {
		t.Fatal("esc must clear the filter")
	}

	press(m, "j")
	log.Info("webhook filter chat resolved", "chat", "5511988887777")
	_, cmd := m.Update(genMsg{m.gen, logsTickMsg{}})
	run(m, cmd)
	if len(m.logs) != 5 || m.logs[0].Msg != "webhook filter chat resolved" || m.logsSel != 2 {
		t.Fatalf("follow: %d entries, newest %q, sel %d", len(m.logs), m.logs[0].Msg, m.logsSel)
	}
	if !m.logsTicking {
		t.Fatal("following must keep polling")
	}

	press(m, "F")
	if m.logsFollow || !strings.Contains(ansi.Strip(m.View()), "paused") {
		t.Fatal("F must pause")
	}
	log.Info("while paused")
	_, cmd = m.Update(genMsg{m.gen, logsTickMsg{}})
	run(m, cmd)
	if len(m.logs) != 5 || m.logsTicking {
		t.Fatalf("paused: %d entries, ticking %v", len(m.logs), m.logsTicking)
	}
	press(m, "F")
	if len(m.logs) != 6 || !m.logsTicking {
		t.Fatalf("F again must catch up and follow: %d entries", len(m.logs))
	}

	press(m, "1")
	_, cmd = m.Update(genMsg{m.gen, logsTickMsg{}})
	if cmd != nil || m.logsTicking {
		t.Fatal("polling must stop once the tab is closed")
	}
	if _, cmd = m.Update(genMsg{m.gen - 1, logsTickMsg{}}); cmd != nil {
		t.Fatal("a tick from another server must be dropped")
	}
}

func TestStaleLogAnswersAreDropped(t *testing.T) {
	m := newModel(Options{})
	m.tab = tabLogs
	full := m.logsReq.start()
	tail := m.logsReq.start()
	m.applyLogs(logsMsg{seq: tail, since: 0, entries: []bot.LogEntry{{ID: 7, Msg: "newer"}}})
	m.applyLogs(logsMsg{seq: full, since: 0, entries: []bot.LogEntry{{ID: 3, Msg: "older"}}})
	if len(m.logs) != 1 || m.logs[0].ID != 7 {
		t.Fatalf("an older answer replaced a newer one: %+v", m.logs)
	}
	m.applyLogs(logsMsg{seq: m.logsReq.start(), since: 5, entries: []bot.LogEntry{{ID: 6}}})
	if len(m.logs) != 1 {
		t.Fatalf("a tail of a list that changed was applied: %+v", m.logs)
	}
	m.applyLogs(logsMsg{seq: m.logsReq.start(), level: "warn", entries: []bot.LogEntry{{ID: 9}}})
	if len(m.logs) != 1 {
		t.Fatal("an answer for another level was applied")
	}
}

func TestLogsTabOnAnOlderServer(t *testing.T) {
	_, c := newServer(t)
	m := newTestModel(t, Options{Client: c})
	press(m, "8")
	m.logs = nil
	m.Update(logsMsg{seq: m.logsReq.start(), err: &APIError{Code: 404}})
	if !strings.Contains(ansi.Strip(m.View()), "doesn't have the server logs") {
		t.Fatal("an older server isn't explained")
	}
	if _, cmd := m.Update(genMsg{m.gen, logsTickMsg{}}); cmd == nil || !m.logsTicking {
		t.Fatal("the tick keeps going (cheaply) while the tab is open")
	}
	if m.logsReq.busy() {
		t.Fatal("a server without /logs must not be polled")
	}
}

func TestLogsFollowAfterAServerRestart(t *testing.T) {
	var mu sync.Mutex
	entries := []bot.LogEntry{{ID: 3, Msg: "three"}, {ID: 2, Msg: "two"}, {ID: 1, Msg: "one"}}
	var asked []int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		asked = append(asked, since)
		out := []bot.LogEntry{}
		for _, e := range entries {
			if e.ID > since {
				out = append(out, e)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"entries": out})
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "t", false)
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(Options{Client: c})
	m.width, m.height, m.tab, m.mode = 120, 40, tabLogs, modeNormal
	m.stats.Started = time.Now().Add(-time.Hour)
	tick := func() {
		_, cmd := m.Update(genMsg{m.gen, logsTickMsg{}})
		run(m, cmd)
	}
	tick()
	if len(m.logs) != 3 {
		t.Fatalf("first load: %+v", m.logs)
	}
	tick()
	if asked[len(asked)-1] != 3 {
		t.Fatalf("follow must tail since the newest id, asked %v", asked)
	}

	mu.Lock()
	entries = []bot.LogEntry{{ID: 2, Msg: "after restart"}, {ID: 1, Msg: "starting"}}
	mu.Unlock()
	m.stats.Started = time.Now()
	tick()
	if len(m.logs) != 2 || m.logs[0].Msg != "after restart" {
		t.Fatalf("after a restart the logs must reload: %+v (asked %v)", m.logs, asked)
	}
	mu.Lock()
	entries = append([]bot.LogEntry{{ID: 3, Msg: "later"}}, entries...)
	mu.Unlock()
	tick()
	if len(m.logs) != 3 || m.logs[0].Msg != "later" || asked[len(asked)-1] != 2 {
		t.Fatalf("follow must tail the new server: %+v (asked %v)", m.logs, asked)
	}
}

func logPage(from, to int64, msg func(int64) string) []bot.LogEntry {
	var out []bot.LogEntry
	for id := from; id >= to; id-- {
		out = append(out, bot.LogEntry{ID: id, Level: "info", Msg: msg(id)})
	}
	return out
}

func TestLogSelectionStaysPutAtTheCap(t *testing.T) {
	parity := func(id int64) string { return map[bool]string{true: "even", false: "odd"}[id%2 == 0] }
	for _, query := range []string{"", "even"} {
		m := newModel(Options{})
		m.width, m.height, m.tab, m.mode = 120, 40, tabLogs, modeNormal
		m.applyLogs(logsMsg{seq: m.logsReq.start(), entries: logPage(logsLimit, 1, parity)})
		m.logsQuery = query
		m.logsSel = 250
		want := m.visibleLogs()[m.logsSel].ID
		m.applyLogs(logsMsg{seq: m.logsReq.start(), since: logsLimit, entries: logPage(logsLimit+5, logsLimit+1, parity)})
		if len(m.logs) != logsLimit {
			t.Fatalf("%q: %d entries, want the cap", query, len(m.logs))
		}
		if got := m.visibleLogs()[m.logsSel].ID; got != want {
			t.Fatalf("filter %q: selection moved from #%d to #%d", query, want, got)
		}
	}
}

func TestLogDetailCardFits(t *testing.T) {
	long := strings.Repeat("x", 1024)
	for _, n := range []int{4, 40} {
		m := newModel(Options{})
		m.width, m.height, m.tab, m.mode = 100, 40, tabLogs, modeNormal
		e := bot.LogEntry{ID: 1, Level: "error", Msg: strings.Repeat("long message ", 20), Time: time.Now(), Attrs: map[string]string{}}
		for i := range n {
			e.Attrs[fmt.Sprintf("attr%02d", i)] = long
		}
		m.applyLogs(logsMsg{seq: m.logsReq.start(), entries: []bot.LogEntry{e, {ID: 0, Msg: "older"}}})
		view := ansi.Strip(m.View())
		if !strings.Contains(view, "╰") {
			t.Fatalf("%d attrs: the detail card's bottom border is cut off:\n%s", n, view)
		}
		for _, l := range strings.Split(view, "\n") {
			if ansi.StringWidth(l) > m.width {
				t.Fatalf("line wider than %d: %q", m.width, l)
			}
		}
		if n == 40 && !strings.Contains(view, "more attribute") {
			t.Fatalf("hidden attributes aren't counted:\n%s", view)
		}
	}
}

func TestLogLevelIsSanitized(t *testing.T) {
	m := newModel(Options{})
	m.width, m.height, m.tab, m.mode = 120, 40, tabLogs, modeNormal
	m.applyLogs(logsMsg{seq: m.logsReq.start(), entries: []bot.LogEntry{{ID: 1, Level: "\x1b]52;c;cHduZWQ=\x07warn", Msg: "hi"}}})
	if strings.ContainsAny(m.logs[0].Level, "\x1b\x07") || strings.Contains(m.View(), "]52;") {
		t.Fatalf("level not sanitized: %q", m.logs[0].Level)
	}
}

func TestReturningToLogsKeepsTheSelection(t *testing.T) {
	b, c := newServer(t)
	buf := bot.NewLogBuffer(100)
	b.Logs = buf
	log := slog.New(buf.Wrap(slog.NewTextHandler(io.Discard, nil)))
	for i := range 5 {
		log.Info("entry", "n", i)
	}
	m := newTestModel(t, Options{Client: c})
	press(m, "8", "j", "j")
	want := m.visibleLogs()[m.logsSel].ID
	press(m, "1")
	log.Info("while away")
	press(m, "8")
	if len(m.logs) != 6 || m.visibleLogs()[m.logsSel].ID != want {
		t.Fatalf("back on Logs: %d entries, selected #%d, want #%d", len(m.logs), m.visibleLogs()[m.logsSel].ID, want)
	}
}
