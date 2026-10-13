package admin

import (
	"cmp"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/williamtcastro/wame/internal/bot"
)

// bar draws used out of total as a meter, amber past 75% and red when full.
func bar(used, total, w int) string {
	if total <= 0 {
		return styleDim.Render("unlimited")
	}
	filled := min(used*w/total, w)
	style := styleAccent
	switch {
	case used >= total:
		style = styleErr
	case used*4 >= total*3:
		style = styleWarn
	}
	return style.Render(strings.Repeat("█", filled)) + styleFaint.Render(strings.Repeat("░", w-filled)) +
		fmt.Sprintf(" %d/%d", used, total)
}

func limiterBody(s bot.LimiterStats, unit string) string {
	rejected := styleDim.Render("0")
	if s.Rejected > 0 {
		rejected = styleErr.Render(fmt.Sprint(s.Rejected)) + styleDim.Render(", last "+relTime(s.LastRejected))
	}
	rows := [][2]string{{"Last minute", bar(s.LastMinute, s.PerMinute, 20)}}
	if !s.NextFree.IsZero() {
		rows = append(rows, [2]string{"Next free", styleWarn.Render(relTime(s.NextFree))})
	}
	if s.ChatGapMS > 0 {
		rows = append(rows, [2]string{"Per-chat gap", (time.Duration(s.ChatGapMS) * time.Millisecond).String()})
	}
	rows = append(rows, [2]string{unit, rejected})
	return kv(rows)
}

func (m *model) webhookBody() string {
	w := m.stats.Webhook
	if w == nil {
		return styleDim.Render("No webhook configured (-webhook / WAME_WEBHOOK).")
	}
	failed := styleDim.Render("0")
	if w.Failed > 0 {
		failed = styleErr.Render(fmt.Sprint(w.Failed))
	}
	dropped := styleDim.Render("0")
	if w.Dropped > 0 {
		dropped = styleErr.Render(fmt.Sprint(w.Dropped))
	}
	last := func(t time.Time) string {
		if t.IsZero() {
			return styleDim.Render("never")
		}
		return relTime(t)
	}
	return kv([][2]string{
		{"Queue", bar(w.Queued, w.Capacity, 20)},
		{"Delivered", styleBold.Render(fmt.Sprint(w.Delivered)) + styleDim.Render(fmt.Sprintf(" (%d after a retry, %d replayed)", w.Retried, w.Replayed))},
		{"Given up", failed},
		{"Dropped", dropped},
		{"Last success", last(w.LastSuccess)},
		{"Last failure", last(w.LastFailure)},
	})
}

func (m *model) viewHealth() string {
	switch {
	case m.statsErr != nil && (m.statsAt.IsZero() || isNotFound(m.statsErr)):
		return m.errCard("Health", m.statsErr, "GET /stats")
	case m.statsAt.IsZero():
		return "  " + styleDim.Render("loading…")
	}
	s := m.stats
	auto := styleAccent.Render("on")
	if !s.AutoReply {
		auto = styleDim.Render("off")
	}
	server := panel{"Server", kv([][2]string{
		{"Version", cmp.Or(safe(s.Version), "?")},
		{"Up since", stamp(s.Started, "2006-01-02 15:04") + styleDim.Render(" ("+strings.TrimSuffix(relTime(s.Started), " ago")+")")},
		{"Auto-replies", auto},
		{"Updated", styleDim.Render(stamp(m.statsAt, "15:04:05"))},
	})}
	return m.grid([]panel{
		server,
		{"Sends", limiterBody(s.Limits.Send, "429s")},
		{"Number lookups", limiterBody(s.Limits.Lookups, "429s")},
		{"Auto-replies", limiterBody(s.Limits.AutoReply, "Dropped")},
		{"Webhook", m.webhookBody()},
		{"Recent webhook failures", m.webhookFailures()},
	})
}

func (m *model) webhookFailures() string {
	w := m.stats.Webhook
	switch {
	case w == nil:
		return styleDim.Render("No webhook configured.")
	case len(w.Recent) == 0:
		return styleDim.Render("None since the server started.") + "\n" + styleDim.Render("t sends a test delivery")
	}
	var lines []string
	start, end := window(m.healthSel, len(w.Recent), 6)
	for i := start; i < end; i++ {
		f := w.Recent[i]
		what := cmp.Or(safe(f.Error), fmt.Sprintf("HTTP %d", f.Status))
		state := styleWarn.Render("retrying")
		switch {
		case f.Replayed:
			state = styleAccent.Render("replayed")
		case f.GaveUp:
			state = styleErr.Render("gave up ")
		}
		mark := "  "
		if i == m.healthSel {
			mark = styleAccent.Render("▌ ")
		}
		lines = append(lines, mark+styleDim.Render(when(f.At))+"  "+state+"  "+styleDim.Render(fmt.Sprintf("try %d", f.Attempt))+"  "+fit(what, 34))
	}
	if len(w.Recent) > end-start {
		lines = append(lines, styleDim.Render(fmt.Sprintf("  %d of %d · j/k to move · R replays the marked one", m.healthSel+1, len(w.Recent))))
	} else {
		lines = append(lines, styleDim.Render("  R replays the marked one · t sends a test"))
	}
	return strings.Join(lines, "\n")
}

// panel is a card's title and body before it's laid out.
type panel struct{ title, body string }

// grid lays panels out in rows of equal height: two columns, or three on wide terminals.
func (m *model) grid(ps []panel) string {
	cols := 2
	if m.width >= 150 {
		cols = 3
	}
	w := (m.width - 1 - (cols - 1)) / cols
	var rows []string
	for i := 0; i < len(ps); i += cols {
		row := ps[i:min(i+cols, len(ps))]
		h := 0
		for _, p := range row {
			h = max(h, lipgloss.Height(card(p.title, w, p.body)))
		}
		var cells []string
		for j, p := range row {
			if j > 0 {
				cells = append(cells, " ")
			}
			cells = append(cells, cardSized(p.title, w, h, p.body))
		}
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, cells...))
	}
	return indent(strings.Join(rows, "\n"), " ")
}
