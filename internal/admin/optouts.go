package admin

import (
	"context"
	"fmt"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/williamtcastro/wame/internal/store"
	"github.com/williamtcastro/wame/internal/termui"
)

type optOutsMsg struct {
	seq  int
	list []store.OptOut
	err  error
}

func (m *model) loadOptOuts() tea.Cmd {
	if m.c == nil {
		return nil
	}
	seq := m.optOutsReq.start()
	return m.call(func(ctx context.Context, c *Client) tea.Msg {
		list, err := c.OptOuts(ctx)
		return optOutsMsg{seq, list, err}
	})
}

func (m *model) applyOptOuts(msg optOutsMsg) {
	if !m.optOutsReq.done(msg.seq) {
		return
	}
	m.optOutsErr = msg.err
	if msg.err != nil {
		return
	}
	selected, ok := m.selectedOptOut()
	m.optOuts, m.optOutsLoaded = msg.list, true
	vis := m.visibleOptOuts()
	if i := slices.IndexFunc(vis, func(o store.OptOut) bool { return o.Chat == selected.Chat }); ok && i >= 0 {
		m.optOutSel = i
	}
	m.optOutSel = clamp(m.optOutSel, len(vis))
}

// optOutsOff reports a server without the route (404) or a token without the admin scope (403):
// asking again won't help, so the tick stops polling until r, o or a tab switch.
func (m *model) optOutsOff() bool {
	return isNotFound(m.optOutsErr) || isForbidden(m.optOutsErr)
}

// pollOptOuts reloads the opt-outs for the Queue tab, the only place that shows them; idle skips
// it while a request is pending (the tick).
func (m *model) pollOptOuts(idle bool) tea.Cmd {
	if m.tab != tabQueue || m.optOutsOff() || (idle && m.optOutsReq.busy()) {
		return nil
	}
	return m.loadOptOuts()
}

// optOutCount is what the Send queue card shows; -1 hides the row (not loaded, an older server or
// a token without the admin scope). A one-off error keeps the last count.
func (m *model) optOutCount() int {
	if !m.optOutsLoaded || m.optOutsOff() {
		return -1
	}
	return len(m.optOuts)
}

// optOutsRefusal is why the list can't be acted on, or "" when it can.
func (m *model) optOutsRefusal() string {
	switch {
	case m.optOutsErr == nil:
		return ""
	case isNotFound(m.optOutsErr):
		return "this server doesn't have opt-outs yet: update wame on it"
	case isForbidden(m.optOutsErr):
		return "this token can't manage opt-outs: it needs the admin scope"
	}
	return "the opt-outs didn't load: " + m.optOutsErr.Error()
}

func (m *model) visibleOptOuts() []store.OptOut {
	return filtered(m.optOuts, m.optOutQuery, func(o store.OptOut) string {
		return strings.ToLower(strings.Join([]string{o.Chat, displayJID(o.Chat), m.chatName(o.Chat), optOutReason(o)}, " "))
	})
}

func (m *model) selectedOptOut() (store.OptOut, bool) {
	if vis := m.visibleOptOuts(); m.optOutSel < len(vis) {
		return vis[m.optOutSel], true
	}
	return store.OptOut{}, false
}

func optOutReason(o store.OptOut) string {
	switch {
	case o.Reason == "manual":
		return "added by " + o.By
	case o.By == "whatsapp":
		return "keyword " + o.Reason
	}
	return strings.Join(nonEmpty(o.Reason, o.By), " by ")
}

// optOutNames is a chat's number and its name, sanitized; name is "" when unknown.
func (m *model) optOutNames(chat string) (num, name string) {
	num, name = displayJID(safe(chat)), m.chatName(chat)
	if name == num {
		name = ""
	}
	return num, name
}

// optOutLabel names a chat as "Maria (+5511999999999)" when its name is known.
func (m *model) optOutLabel(chat string) string {
	if num, name := m.optOutNames(chat); name != "" {
		return name + " (" + num + ")"
	}
	return displayJID(safe(chat))
}

func (m *model) optOutsKey(k string) tea.Cmd {
	n := len(m.visibleOptOuts())
	if _, ok := termui.ListMove(k, m.optOutSel, n, 1); ok || k == "a" || k == "x" {
		if why := m.optOutsRefusal(); why != "" {
			m.setNotice(why, true)
			return nil
		}
	}
	if sel, ok := termui.ListMove(k, m.optOutSel, n, m.bodyHeight()/2); ok {
		m.optOutSel = sel
		return nil
	}
	switch k {
	case "o":
		m.queueOptOuts = false
	case "esc":
		if m.optOutQuery == "" {
			m.queueOptOuts = false
			return nil
		}
		m.listFilterKey(k, "")
	case "/":
		m.listFilterKey(k, "filter by number, name or reason")
	case "a":
		m.startPrompt(modeAddOptOut, "number with country code or JID, e.g. +5511999999999")
	case "x":
		o, ok := m.selectedOptOut()
		if !ok {
			m.setNotice("no opt-out marked", true)
			return nil
		}
		m.confirmOptOut = o.Chat
		m.mode = modeConfirmRemoveOptOut
	}
	return nil
}

func (m *model) addOptOut(chat string) tea.Cmd {
	m.setNotice("adding "+chat+" to the opt-outs…", false)
	return m.call(func(ctx context.Context, c *Client) tea.Msg {
		o, added, err := c.AddOptOut(ctx, chat)
		switch {
		case err != nil:
			return doneMsg{err: fmt.Errorf("not added: %w", err)}
		case !added:
			return doneMsg{notice: displayJID(o.Chat) + " was already on the opt-outs"}
		}
		return doneMsg{notice: displayJID(o.Chat) + " added to the opt-outs: it gets no queued messages"}
	})
}

func (m *model) removeOptOut(chat string) tea.Cmd {
	return m.call(func(ctx context.Context, c *Client) tea.Msg {
		if err := c.RemoveOptOut(ctx, chat); err != nil {
			return doneMsg{err: fmt.Errorf("not removed: %w", err)}
		}
		return doneMsg{notice: displayJID(chat) + " removed from the opt-outs: queued messages reach it again"}
	})
}

func (m *model) viewOptOuts(lines []string) string {
	if m.optOutsErr != nil {
		return strings.Join(append(lines, m.errCard("Opt-outs", m.optOutsErr, "the opt-out list (GET /optouts)")), "\n")
	}
	vis := m.visibleOptOuts()
	head := fmt.Sprintf("opt-outs · %d of %d · chats that get no queued messages", len(vis), len(m.optOuts))
	if m.optOutQuery != "" {
		head += fmt.Sprintf(" · filtered by %q (esc clears)", safe(m.optOutQuery))
	}
	lines = append(lines, "   "+styleDim.Render(head))
	cols := []int{17, 22, 22}
	lines = append(lines, "   "+styleHead.Render(table(cols, "CHAT", "NAME", "REASON", "WHEN")))
	if len(vis) == 0 {
		msg := "nobody opted out"
		if m.optOutQuery != "" {
			msg = "nothing matches"
		}
		return strings.Join(append(lines, "   "+styleDim.Render(msg)), "\n")
	}
	start, end := window(m.optOutSel, len(vis), m.listRows(lines))
	for i := start; i < end; i++ {
		o := vis[i]
		num, name := m.optOutNames(o.Chat)
		line := table(cols, num, name, safe(optOutReason(o)), styleDim.Render(when(o.At)+"  "+relTime(o.At)))
		lines = append(lines, m.row(line, i == m.optOutSel))
	}
	return strings.Join(lines, "\n")
}

func (m *model) viewAddOptOut() string {
	return styleBold.Render("Add an opt-out") + "\n" +
		styleDim.Render("That chat gets no queued messages; direct sends aren't affected.") + "\n\n" +
		m.prompt.View()
}

func (m *model) viewConfirmRemoveOptOut() string {
	return styleBold.Render("Take "+m.optOutLabel(m.confirmOptOut)+" off the opt-outs?") + "\n\n" +
		styleDim.Render("Queued messages to it go out again.") + "\n\n" +
		styleKey.Render("y") + styleDim.Render(" remove   any other key: keep it")
}
