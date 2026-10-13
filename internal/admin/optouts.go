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

// optOutCount is what the Send queue card shows; -1 hides the row (not loaded, or an older server).
func (m *model) optOutCount() int {
	if !m.optOutsLoaded || m.optOutsErr != nil {
		return -1
	}
	return len(m.optOuts)
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

// optOutLabel names a chat as "Maria (+5511999999999)" when its name is known, sanitized.
func (m *model) optOutLabel(chat string) string {
	num, name := displayJID(safe(chat)), m.chatName(chat)
	if name == num {
		return num
	}
	return name + " (" + num + ")"
}

func (m *model) optOutsKey(k string) tea.Cmd {
	n := len(m.visibleOptOuts())
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
	case "a", "x":
		if isNotFound(m.optOutsErr) {
			m.setNotice("this server doesn't have opt-outs yet: update wame on it", true)
			return nil
		}
		if k == "a" {
			m.startPrompt(modeAddOptOut, "number with country code or JID, e.g. +5511999999999")
			return nil
		}
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
	start, end := window(m.optOutSel, len(vis), max(m.bodyHeight()-len(lines)-1, 3))
	for i := start; i < end; i++ {
		o := vis[i]
		num, name := displayJID(safe(o.Chat)), m.chatName(o.Chat)
		if name == num {
			name = ""
		}
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
