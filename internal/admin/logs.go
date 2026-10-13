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
	"info":  lipgloss.NewStyle().Foreground(colInfo),
	"warn":  styleWarn,
	"error": styleErr,
}

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
		load = m.loadLogs(m.newestLog())
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
	for i := range msg.entries {
		e := &msg.entries[i]
		e.Msg = safe(e.Msg)
		attrs := make(map[string]string, len(e.Attrs))
		for k, v := range e.Attrs {
			attrs[safe(k)] = safe(v)
		}
		e.Attrs = attrs
	}
	before := len(m.visibleLogs())
	if msg.since == 0 || len(msg.entries) >= logsLimit {
		m.logs = msg.entries
		m.logsSel = 0
	} else {
		m.logs = append(msg.entries, m.logs...)[:min(len(msg.entries)+len(m.logs), logsLimit)]
		if m.logsSel > 0 {
			m.logsSel += len(m.visibleLogs()) - before
		}
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
		return tea.Batch(m.loadLogs(m.newestLog()), m.followLogs())
	case "/":
		m.startPrompt(modeFilter, "filter by message or attribute")
		m.prompt.SetValue(m.logsQuery)
		m.prompt.CursorEnd()
	case "esc":
		m.logsQuery = ""
		m.logsSel = clamp(m.logsSel, len(m.visibleLogs()))
	}
	return nil
}

func (m *model) visibleLogs() []bot.LogEntry {
	q := strings.ToLower(m.logsQuery)
	if q == "" {
		return m.logs
	}
	var out []bot.LogEntry
	for _, e := range m.logs {
		if strings.Contains(strings.ToLower(e.Msg+" "+logAttrs(e)), q) {
			out = append(out, e)
		}
	}
	return out
}

// logAttrs renders an entry's attributes as k=v pairs in key order.
func logAttrs(e bot.LogEntry) string {
	parts := make([]string, 0, len(e.Attrs))
	for _, k := range slices.Sorted(maps.Keys(e.Attrs)) {
		parts = append(parts, k+"="+e.Attrs[k])
	}
	return strings.Join(parts, " ")
}

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
		line := table(cols, styleDim.Render(stamp(e.Time, "15:04:05")), logLevel(e.Level), e.Msg, styleDim.Render(logAttrs(e)))
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

func (m *model) logDetail(entries []bot.LogEntry) string {
	if m.logsSel >= len(entries) {
		return ""
	}
	e := entries[m.logsSel]
	rows := [][2]string{{"When", stamp(e.Time, "2006-01-02 15:04:05") + styleDim.Render(" ("+relTime(e.Time)+")")}}
	for _, k := range slices.Sorted(maps.Keys(e.Attrs)) {
		rows = append(rows, [2]string{k, e.Attrs[k]})
	}
	return card(fmt.Sprintf("#%d %s", e.ID, logLevel(e.Level)+" "+styleBold.Render(e.Msg)), m.cardWidth(cardDetail), kv(rows))
}
