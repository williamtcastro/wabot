package admin

import (
	"io"
	"log/slog"
	"strings"
	"testing"

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
