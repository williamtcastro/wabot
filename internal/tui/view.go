package tui

import (
	"cmp"
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mdp/qrterminal/v3"

	"github.com/williamtcastro/wame/internal/store"
	"github.com/williamtcastro/wame/internal/termui"
)

var (
	colAccent = termui.ColAccent
	colMuted  = termui.ColMuted
	colFaint  = termui.ColFaint
	colErr    = termui.ColErr
	colMe     = lipgloss.Color("114")
	colSel    = termui.ColSel

	styleDim    = lipgloss.NewStyle().Foreground(colMuted)
	styleFaint  = lipgloss.NewStyle().Foreground(colFaint)
	styleErr    = lipgloss.NewStyle().Foreground(colErr)
	styleMe     = lipgloss.NewStyle().Foreground(colMe)
	styleBold   = lipgloss.NewStyle().Bold(true)
	styleAccent = lipgloss.NewStyle().Foreground(colAccent)
	styleBadge  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(colAccent).Padding(0, 1)
	styleKey    = lipgloss.NewStyle().Foreground(termui.ColText).Bold(true)
	styleItalic = lipgloss.NewStyle().Foreground(colMuted).Italic(true)

	senderColors = []lipgloss.Color{"39", "213", "208", "141", "80", "220", "174", "111"}
	quickReacts  = []string{"👍", "❤️", "😂", "😮", "😢", "🙏"}
)

func (m *model) layout() {
	if !m.ready {
		return
	}
	chatW := m.w - sidebarWidth
	m.vp.Width = max(10, chatW-4)
	m.vp.Height = max(3, m.mainHeight()-4)
	m.input.Width = max(10, m.w-8)
}

func (m *model) mainHeight() int {
	h := m.h - 1 - 3
	if m.contextBar() != "" {
		h--
	}
	return h
}

// render rebuilds the message pane; toBottom scrolls to the newest message.
func (m *model) render(toBottom bool) {
	if !m.ready {
		return
	}
	width := m.vp.Width
	var content string
	m.visible, m.starts = nil, nil
	switch {
	case !m.status.LoggedIn && m.status.QR != "":
		var sb strings.Builder
		qrterminal.GenerateWithConfig(m.status.QR, qrterminal.Config{
			Level: qrterminal.L, Writer: &sb, HalfBlocks: true,
			BlackChar: qrterminal.BLACK_BLACK, WhiteChar: qrterminal.WHITE_WHITE,
			BlackWhiteChar: qrterminal.BLACK_WHITE, WhiteBlackChar: qrterminal.WHITE_BLACK,
			QuietZone: 1,
		})
		content = "Scan with WhatsApp → Linked devices → Link a device\n" +
			styleDim.Render("or type /pair <your phone number>") + "\n\n" + sb.String()
	case m.searching:
		content = m.renderResults(width)
	case m.panelTitle != "":
		content = lipgloss.NewStyle().Width(width).Render(m.panelBody)
	case m.active == "":
		content = m.renderFeed(width)
	default:
		content = m.renderChat(width)
	}
	m.vp.SetContent(content)
	switch {
	case m.searching:
		if m.resultSel < m.vp.YOffset {
			m.vp.SetYOffset(m.resultSel)
		} else if m.resultSel >= m.vp.YOffset+m.vp.Height {
			m.vp.SetYOffset(m.resultSel - m.vp.Height + 1)
		}
	case m.sel >= 0 && m.sel < len(m.starts):
		start := m.starts[m.sel]
		end := len(strings.Split(content, "\n"))
		if m.sel+1 < len(m.starts) {
			end = m.starts[m.sel+1]
		}
		if start < m.vp.YOffset {
			m.vp.SetYOffset(start)
		} else if end > m.vp.YOffset+m.vp.Height {
			m.vp.SetYOffset(end - m.vp.Height)
		}
	case toBottom:
		m.vp.GotoBottom()
	}
}

func (m *model) renderFeed(width int) string {
	if len(m.feed) == 0 {
		return styleDim.Render("\n  Nothing yet. Messages from every chat will show up here live.\n\n  Press Tab to pick a chat, or type /to <name>.  /help for everything else.")
	}
	var lines []string
	lastDay := ""
	for _, x := range m.feed {
		if day := dayLabel(x.Timestamp); day != lastDay {
			lines = append(lines, separator(day, width))
			lastDay = day
		}
		who := styleBold.Foreground(colorFor(x.Chat)).Render(m.nameOf(x.Chat))
		if x.FromMe {
			who = styleMe.Render("me") + styleDim.Render(" → ") + who
		} else if x.IsGroup {
			who = styleDim.Render(cmp.Or(x.PushName, userOf(x.Sender))+" in ") + who
		}
		line := styleDim.Render(x.Timestamp.Local().Format("15:04")) + "  " + who + styleDim.Render("  ") + shortText(x)
		lines = append(lines, ansi.Truncate(line, width, "…"))
	}
	return strings.Join(lines, "\n")
}

func (m *model) renderChat(width int) string {
	reacts := map[string]map[string]string{}
	byID := map[string]store.Message{}
	for _, x := range m.msgs {
		if x.Type == "reaction" {
			if reacts[x.ReplyTo] == nil {
				reacts[x.ReplyTo] = map[string]string{}
			}
			reacts[x.ReplyTo][x.Sender] = x.Text
			continue
		}
		byID[x.ID] = x
		m.visible = append(m.visible, x)
	}
	if len(m.visible) == 0 {
		return styleDim.Render("\n  No messages yet. Say hi 👋")
	}
	bubbleW := max(20, width*3/4)
	var out []string
	lastDay, lastSender := "", ""
	newCount := 0
	for _, x := range m.visible {
		if x.Seq > m.newAfter && !x.FromMe {
			newCount++
		}
	}
	dividerShown := false
	for i, x := range m.visible {
		if day := dayLabel(x.Timestamp); day != lastDay {
			if len(out) > 0 {
				out = append(out, "")
			}
			out = append(out, separator(day, width), "")
			lastDay, lastSender = day, ""
		} else if x.Sender != lastSender {
			out = append(out, "")
		}
		if newCount > 0 && !dividerShown && x.Seq > m.newAfter && !x.FromMe {
			dividerShown = true
			label := "1 new message"
			if newCount > 1 {
				label = fmt.Sprintf("%d new messages", newCount)
			}
			out = append(out, styleAccent.Render(separator(label, width)), "")
			lastSender = ""
		}
		var block []string
		if x.IsGroup && !x.FromMe && x.Sender != lastSender {
			block = append(block, styleBold.Foreground(colorFor(x.Sender)).Render(cmp.Or(x.PushName, userOf(x.Sender))))
		}
		lastSender = x.Sender
		if q, ok := byID[x.ReplyTo]; ok && x.ReplyTo != "" {
			who := "me"
			if !q.FromMe {
				who = cmp.Or(q.PushName, m.nameOf(q.Sender))
			}
			block = append(block, styleFaint.Render("┃ ")+styleDim.Render(ansi.Truncate(who+": "+shortText(q), bubbleW-2, "…")))
		} else if x.ReplyTo != "" {
			block = append(block, styleFaint.Render("┃ ↩ reply to an earlier message"))
		}
		block = append(block, m.bubble(x, reacts[x.ID], bubbleW)...)

		selected := i == m.sel && m.focus == focusMessages
		m.starts = append(m.starts, len(out))
		for _, line := range block {
			if x.FromMe {
				line = lipgloss.PlaceHorizontal(width-2, lipgloss.Right, line)
			}
			gutter := "  "
			if selected {
				gutter = styleAccent.Render("▌ ")
			}
			out = append(out, gutter+line)
		}
	}
	return strings.Join(out, "\n")
}

// bubble is a message's body wrapped to w, with its time, edit mark, tick and reactions on the last
// line when they fit, or on a line of their own.
func (m *model) bubble(x store.Message, reactions map[string]string, w int) []string {
	meta := x.Timestamp.Local().Format("15:04")
	if x.Edited && !x.Deleted {
		meta = "edited " + meta
	}
	if x.FromMe {
		meta += " ✓"
	}
	if r := joinReactions(reactions); r != "" {
		meta = "│ " + r + "  " + meta
	}
	wrapped := strings.Split(lipgloss.NewStyle().Width(w).Render(m.bodyOf(x)), "\n")
	for j := range wrapped {
		wrapped[j] = strings.TrimRight(wrapped[j], " ")
	}
	last := len(wrapped) - 1
	if lipgloss.Width(wrapped[last])+2+lipgloss.Width(meta) <= w {
		wrapped[last] += "  " + styleDim.Render(meta)
	} else {
		wrapped = append(wrapped, styleDim.Render(meta))
	}
	return wrapped
}

func (m *model) bodyOf(x store.Message) string {
	if x.Deleted {
		return styleItalic.Render("🚫 This message was deleted")
	}
	style := lipgloss.NewStyle()
	if x.FromMe {
		style = styleMe
	}
	text := x.Text
	switch x.Type {
	case "image":
		text = "📷 Photo" + prefixed("\n", x.Text)
	case "video":
		text = "🎥 Video" + prefixed("\n", x.Text)
	case "audio":
		text = "🎤 Voice message"
	case "sticker":
		text = "💟 Sticker"
	case "document":
		name := "Document"
		if x.Media != nil && x.Media.FileName != "" {
			name = x.Media.FileName
		}
		text = "📄 " + name
		if x.Media != nil && x.Media.Size > 0 {
			text += styleDim.Render(" · " + humanSize(x.Media.Size))
		}
	case "location":
		text = "📍 " + x.Text
	}
	if x.Media != nil && m.focus == focusMessages && m.sel >= 0 && m.sel < len(m.visible) && m.visible[m.sel].ID == x.ID {
		text += styleDim.Render("  (s to save)")
	}
	return style.Render(text)
}

func (m *model) View() string {
	if !m.ready {
		return "loading…"
	}
	mainH := m.mainHeight()
	sidebar := m.box(m.renderSidebar(mainH-2), sidebarWidth, mainH, m.focus == focusChats)

	title := m.paneTitle()
	pane := lipgloss.JoinVertical(lipgloss.Left, title, styleFaint.Render(strings.Repeat("─", m.vp.Width)), m.vp.View())
	chat := m.box(pane, m.w-sidebarWidth, mainH, m.focus == focusMessages)

	body := lipgloss.JoinHorizontal(lipgloss.Top, sidebar, chat)
	parts := []string{body}
	if bar := m.contextBar(); bar != "" {
		parts = append(parts, bar)
	}
	parts = append(parts, m.box(m.input.View(), m.w, 3, m.focus == focusInput), m.footer())
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

func (m *model) box(content string, w, h int, focused bool) string {
	border := colFaint
	if focused {
		border = colAccent
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).
		Padding(0, 1).Width(w - 2).Height(h - 2).MaxHeight(h).Render(content)
}

func (m *model) paneTitle() string {
	switch {
	case m.searching:
		return styleBold.Render("Search: "+m.searchQuery) + styleDim.Render(fmt.Sprintf("   %d results", len(m.results)))
	case m.panelTitle != "":
		return styleBold.Render(m.panelTitle) + styleDim.Render("   esc to close")
	case m.active == "":
		return styleBold.Render("All activity") + styleDim.Render("   every chat, live")
	}
	sub, _ := copyTarget(m.active)
	hint := styleFaint.Render("   ctrl+y copy")
	tags := ""
	if len(m.activeTags) > 0 {
		tags = styleDim.Render("  " + strings.Join(m.activeTags, " "))
	}
	return ansi.Truncate(styleBold.Render(m.activeName)+tags+styleDim.Render("   "+cmp.Or(sub, m.active))+hint, m.vp.Width, "…")
}

func (m *model) contextBar() string {
	var label string
	switch {
	case m.editing != nil:
		label = styleAccent.Render("✎ Editing") + styleDim.Render("  "+shortText(*m.editing))
	case m.replyTo != nil:
		who := "me"
		if !m.replyTo.FromMe {
			who = cmp.Or(m.replyTo.PushName, m.nameOf(m.replyTo.Sender))
		}
		label = styleAccent.Render("↩ Replying to "+who) + styleDim.Render("  "+shortText(*m.replyTo))
	case strings.HasPrefix(m.input.Value(), "/") && !strings.Contains(m.input.Value(), " "):
		var hints []string
		for _, c := range tuiCommands {
			if strings.HasPrefix(c.name, m.input.Value()) {
				hints = append(hints, styleKey.Render(c.name)+styleDim.Render(prefixed(" ", c.args)+"  "+c.desc))
			}
		}
		if len(hints) == 0 {
			return " " + styleErr.Render("no such command") + styleFaint.Render("  /help lists them")
		}
		tail := styleFaint.Render("  tab complete · ↑↓ ^J/^K cycle")
		return " " + ansi.Truncate(strings.Join(hints, styleFaint.Render("  ·  ")), m.w-1-lipgloss.Width(tail), "…") + tail
	default:
		return ""
	}
	return " " + ansi.Truncate(label, m.w-16, "…") + styleFaint.Render("  esc to cancel")
}

func (m *model) footer() string {
	var left string
	switch {
	case m.notice != "" && m.noticeErr:
		left = styleErr.Render("✗ " + m.notice)
	case m.notice != "":
		left = styleAccent.Render("✓ ") + m.notice
	case m.searching:
		left = hints("↑↓", "move", "enter", "jump to message", "esc", "close")
	case m.focus == focusChats:
		left = hints("type", "filter", "↑↓ ^J/^K", "move", "enter", "open", "esc", "back")
	case m.focus == focusMessages:
		left = hints("j/k g/G", "move", "r", "reply", "1-6", "react", "e", "edit", "dd", "delete", "s", "save", "esc", "back")
	default:
		left = hints("tab", "chats", "↑", "select msg", "^J/^K", "switch", "^D/^U", "scroll", "/help", "commands")
	}
	dot, state := styleErr.Render("●"), m.status.State
	if m.status.Connected && m.status.LoggedIn {
		dot, state = styleAccent.Render("●"), "+"+userOf(m.status.JID)
	}
	right := dot + " " + styleDim.Render(state) + " "
	left = ansi.Truncate(" "+left, m.w-lipgloss.Width(right)-1, "…")
	gap := max(1, m.w-lipgloss.Width(left)-lipgloss.Width(right))
	return left + strings.Repeat(" ", gap) + right
}

func hints(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, styleKey.Render(pairs[i])+" "+styleDim.Render(pairs[i+1]))
	}
	return strings.Join(parts, styleFaint.Render("  ·  "))
}

func (m *model) renderSidebar(height int) string {
	inner := sidebarWidth - 4
	head := styleBold.Render("Chats") + styleDim.Render(fmt.Sprintf("  %d", len(m.chats)))
	if m.filter != "" || m.focus == focusChats {
		head = styleAccent.Render("⌕ ") + m.filter + styleAccent.Render("▏")
	}
	type row struct{ top, bottom string }
	allBottom := "every chat, live"
	unreadChats := 0
	for _, c := range m.chats {
		if c.Unread > 0 {
			unreadChats++
		}
	}
	switch n := unreadChats; {
	case n == 1:
		allBottom = "1 chat with new messages"
	case n > 1:
		allBottom = fmt.Sprintf("%d chats with new messages", n)
	}
	chats := m.filteredChats()
	rows := []row{{"All activity", allBottom}}
	for _, c := range chats {
		name := c.Name
		if c.IsGroup {
			name = "👥 " + name
		}
		right := styleDim.Render(shortTime(c.LastAt))
		nameStyle := lipgloss.NewStyle()
		if n := c.Unread; n > 0 {
			right = styleBadge.Render(fmt.Sprint(n))
			nameStyle = styleBold
		}
		top := nameStyle.Render(ansi.Truncate(name, inner-lipgloss.Width(right)-3, "…"))
		top += strings.Repeat(" ", max(1, inner-2-lipgloss.Width(top)-lipgloss.Width(right))) + right
		preview := shortText(store.Message{Type: c.LastType, Text: c.LastText})
		if c.LastFromMe {
			preview = "me: " + preview
		}
		rows = append(rows, row{top, ansi.Truncate(preview, inner-2, "…")})
	}

	visible := max(1, (height-2)/2)
	offset := 0
	if m.cursor >= visible {
		offset = m.cursor - visible + 1
	}
	lines := []string{head, ""}
	for i := offset; i < len(rows) && i < offset+visible; i++ {
		isActive := (i == 0 && m.active == "") || (i > 0 && chats[i-1].JID == m.active)
		marker := "  "
		if isActive {
			marker = styleAccent.Render("▌ ")
		}
		top, bottom := marker+rows[i].top, "  "+styleDim.Render(rows[i].bottom)
		if i == m.cursor && m.focus == focusChats {
			sel := lipgloss.NewStyle().Background(colSel).Width(inner)
			top, bottom = sel.Render(top), sel.Render(bottom)
		}
		lines = append(lines, top, bottom)
	}
	return strings.Join(lines, "\n")
}

func (m *model) nameOf(jid string) string {
	if i := chatIndex(m.chats, jid); i >= 0 {
		return m.chats[i].Name
	}
	if m.status.JID != "" && jid == m.status.JID {
		return "Me"
	}
	return "+" + userOf(jid)
}

func (m *model) renderResults(width int) string {
	if len(m.results) == 0 {
		return styleDim.Render("\n  No messages match. Words must appear whole, except the last one (\"bom di\" finds \"bom dia\").")
	}
	lines := make([]string, 0, len(m.results))
	for i, x := range m.results {
		who := styleBold.Foreground(colorFor(x.Chat)).Render(m.nameOf(x.Chat))
		switch {
		case x.FromMe:
			who += styleDim.Render(" › ") + styleMe.Render("me:")
		case x.IsGroup:
			who += styleDim.Render(" › " + cmp.Or(x.PushName, userOf(x.Sender)) + ":")
		}
		line := styleDim.Render(x.Timestamp.Local().Format("02/01 15:04")) + "  " + who + " " + shortText(x)
		gutter := "  "
		if i == m.resultSel {
			gutter = styleAccent.Render("▌ ")
		}
		lines = append(lines, gutter+ansi.Truncate(line, width-2, "…"))
	}
	return strings.Join(lines, "\n")
}
