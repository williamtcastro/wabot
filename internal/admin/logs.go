package admin

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/termui"
)

const (
	logsLimit = 1000
	logsPoll  = 3 * time.Second
)

type (
	logsMsg struct {
		seq     int
		level   string
		since   int64
		entries []bot.LogEntry
		err     error
	}
	logsTickMsg struct{}
)

// logLevels is the order f cycles through on the Logs tab; "" shows every level.
var logLevels = []string{"", "info", "warn", "error"}

var logStyles = map[string]lipgloss.Style{
	"debug": styleFaint,
	"info":  styleInfo,
	"warn":  styleWarn,
	"error": styleErr,
}

// logEntry is a sanitized entry with its attributes rendered and its filter haystack, worked out
// once as it arrives rather than on every key and frame.
type logEntry struct {
	bot.LogEntry
	attrs, hay string
}

func newLogEntry(e bot.LogEntry) logEntry {
	e.Msg, e.Level = safe(e.Msg), safe(e.Level)
	attrs := make(map[string]string, len(e.Attrs))
	for k, v := range e.Attrs {
		attrs[safe(k)] = safe(v)
	}
	e.Attrs = attrs
	parts := make([]string, 0, len(attrs))
	for _, k := range slices.Sorted(maps.Keys(attrs)) {
		parts = append(parts, k+"="+attrs[k])
	}
	le := logEntry{LogEntry: e, attrs: strings.Join(parts, " ")}
	le.hay = strings.ToLower(e.Msg + " " + le.attrs)
	return le
}

func logHay(e logEntry) string { return e.hay }

func (m *model) newestLog() int64 {
	if len(m.logs) == 0 {
		return 0
	}
	return m.logs[0].ID
}

// loadLogs asks for the entries newer than since, or the newest page when since is 0.
func (m *model) loadLogs(since int64) tea.Cmd {
	level := m.logsLevel
	seq := m.logsReq.start()
	return m.call(func(ctx context.Context, c *Client) tea.Msg {
		e, err := c.Logs(ctx, level, since, logsLimit)
		return logsMsg{seq, level, since, e, err}
	})
}

// refreshLogs tails the loaded entries, or loads the newest page when there are none or the server
// restarted since they loaded: its log ids start again at 1, so a tail would never match.
func (m *model) refreshLogs() tea.Cmd {
	started := m.stats.Started
	if m.logsStarted.IsZero() {
		m.logsStarted = started
	}
	if len(m.logs) == 0 || !started.IsZero() && !started.Equal(m.logsStarted) {
		return m.loadLogs(0)
	}
	return m.loadLogs(m.newestLog())
}

// followLogs starts the poll that tails the logs while the tab is open and follow is on. The tick
// carries the client generation, so one from a server the user switched away from is dropped.
func (m *model) followLogs() tea.Cmd {
	if m.logsTicking || !m.logsFollow || m.tab != tabLogs {
		return nil
	}
	m.logsTicking = true
	gen := m.gen
	return tea.Tick(logsPoll, func(time.Time) tea.Msg { return genMsg{gen, logsTickMsg{}} })
}

func (m *model) logsTick() tea.Cmd {
	m.logsTicking = false
	if m.tab != tabLogs || !m.logsFollow {
		return nil
	}
	var load tea.Cmd
	if !m.logsReq.busy() && !isNotFound(m.logsErr) {
		load = m.refreshLogs()
	}
	return tea.Batch(load, m.followLogs())
}

// applyLogs takes a page: a full one replaces the list, a tail is put on top of it. Answers older
// than one already applied, for a level the user has moved away from, or tails of a list that has
// since changed are dropped.
func (m *model) applyLogs(msg logsMsg) {
	if !m.logsReq.done(msg.seq) || msg.level != m.logsLevel || (msg.since != 0 && msg.since != m.newestLog()) {
		return
	}
	m.logsErr = msg.err
	if msg.err != nil {
		return
	}
	fresh := make([]logEntry, len(msg.entries))
	for i, e := range msg.entries {
		fresh[i] = newLogEntry(e)
	}
	switch {
	case msg.since == 0:
		m.logs, m.logsSel, m.logsStarted = fresh, 0, m.stats.Started
	case len(fresh) >= logsLimit:
		m.logs, m.logsSel = fresh, 0
	default:
		// The selection stays on its entry: it moves down by the new entries the filter shows.
		if m.logsSel > 0 {
			m.logsSel += len(filtered(fresh, m.logsQuery, logHay))
		}
		m.logs = append(fresh, m.logs...)[:min(len(fresh)+len(m.logs), logsLimit)]
	}
	m.logsSel = clamp(m.logsSel, len(m.visibleLogs()))
}

func (m *model) logsKey(k string) tea.Cmd {
	if sel, ok := termui.ListMove(k, m.logsSel, len(m.visibleLogs()), m.bodyHeight()/2); ok {
		m.logsSel = sel
		return nil
	}
	switch k {
	case "f":
		i := slices.Index(logLevels, m.logsLevel)
		m.logsLevel = logLevels[(i+1)%len(logLevels)]
		m.logs, m.logsSel = nil, 0
		return m.loadLogs(0)
	case "F":
		m.logsFollow = !m.logsFollow
		if !m.logsFollow {
			m.setNotice("paused: new log entries won't load until F or r", false)
			return nil
		}
		m.setNotice("following new log entries", false)
		return tea.Batch(m.refreshLogs(), m.followLogs())
	}
	m.listFilterKey(k, "filter by message or attribute")
	return nil
}

func (m *model) visibleLogs() []logEntry { return filtered(m.logs, m.logsQuery, logHay) }

func logLevel(level string) string {
	style, ok := logStyles[level]
	if !ok {
		style = styleBold
	}
	return style.Render(level)
}

func (m *model) viewLogs() string {
	if m.logsErr != nil && len(m.logs) == 0 {
		return m.errCard("Logs", m.logsErr, "the server logs (GET /logs)")
	}
	entries := m.visibleLogs()
	status := []string{fmt.Sprintf("showing %s · %d of %d", levelLabel(m.logsLevel), len(entries), len(m.logs))}
	if m.logsQuery != "" {
		status = append(status, fmt.Sprintf("filtered by %q (esc clears)", safe(m.logsQuery)))
	}
	if m.logsFollow {
		status = append(status, styleAccent.Render("following"))
	} else {
		status = append(status, styleWarn.Render("paused"))
	}
	if m.logsErr != nil {
		status = append(status, errText(m.logsErr))
	}
	lines := []string{"   " + styleDim.Render(strings.Join(status, " · "))}
	if len(entries) == 0 {
		msg := "No log entries yet. The server keeps its recent app logs in memory until it restarts."
		if m.logsQuery != "" {
			msg = fmt.Sprintf("Nothing matches %q in the loaded entries. Esc clears the filter.", safe(m.logsQuery))
		}
		return strings.Join(append(lines, indent(card("Logs", m.cardWidth(cardMedium), styleDim.Render(msg)), " ")), "\n")
	}
	cols := []int{8, 5, 34}
	detail := m.logDetail(entries)
	lines = append(lines, "   "+styleHead.Render(table(cols, "TIME", "LEVEL", "MESSAGE", "DETAILS")))
	start, end := window(m.logsSel, len(entries), m.bodyHeight()-3-lipgloss.Height(detail))
	for i := start; i < end; i++ {
		e := entries[i]
		line := table(cols, styleDim.Render(stamp(e.Time, "15:04:05")), logLevel(e.Level), e.Msg, styleDim.Render(e.attrs))
		lines = append(lines, m.row(line, i == m.logsSel))
	}
	return strings.Join(append(lines, indent(detail, " ")), "\n")
}

func levelLabel(level string) string {
	if level == "" {
		return "all levels"
	}
	return level + " and above"
}

// logDetail is the selected entry's card. Values are cut to one line and attributes past half the
// body are counted instead, so the card always fits.
func (m *model) logDetail(entries []logEntry) string {
	if m.logsSel >= len(entries) {
		return ""
	}
	e := entries[m.logsSel]
	w := m.cardWidth(cardDetail)
	keys := slices.Sorted(maps.Keys(e.Attrs))
	kw := len("When")
	for _, k := range keys {
		kw = max(kw, min(ansi.StringWidth(k), 24))
	}
	valW := max(w-4-kw-2, 10)
	room := max(m.bodyHeight()/2-3, 2)
	rows := [][2]string{{"When", stamp(e.Time, "2006-01-02 15:04:05") + styleDim.Render(" ("+relTime(e.Time)+")")}}
	for i, k := range keys {
		if left := len(keys) - i; len(rows) == room-1 && left > 1 {
			rows = append(rows, [2]string{"…", styleDim.Render(plural(left, "more attribute"))})
			break
		}
		rows = append(rows, [2]string{fit(k, 24), fit(e.Attrs[k], valW)})
	}
	title := fit(fmt.Sprintf("#%d %s", e.ID, logLevel(e.Level)+" "+styleBold.Render(e.Msg)), max(w-4, 10))
	return card(title, w, kv(rows))
}
