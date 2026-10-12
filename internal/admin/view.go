package admin

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mdp/qrterminal/v3"
)

var (
	colAccent = lipgloss.Color("42")
	colMuted  = lipgloss.Color("244")
	colFaint  = lipgloss.Color("238")
	colErr    = lipgloss.Color("203")
	colWarn   = lipgloss.Color("214")
	colSel    = lipgloss.Color("236")

	styleDim    = lipgloss.NewStyle().Foreground(colMuted)
	styleFaint  = lipgloss.NewStyle().Foreground(colFaint)
	styleErr    = lipgloss.NewStyle().Foreground(colErr)
	styleWarn   = lipgloss.NewStyle().Foreground(colWarn)
	styleBold   = lipgloss.NewStyle().Bold(true)
	styleAccent = lipgloss.NewStyle().Foreground(colAccent)
	styleKey    = lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Bold(true)
	styleTab    = lipgloss.NewStyle().Padding(0, 1).Foreground(colMuted)
	styleTabOn  = lipgloss.NewStyle().Padding(0, 1).Bold(true).Foreground(lipgloss.Color("0")).Background(colAccent)
	styleBox    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colAccent).Padding(1, 2)
)

func (m *model) bodyHeight() int { return max(m.height-5, 3) }

func (m *model) View() string {
	if m.width == 0 {
		return ""
	}
	var body string
	switch {
	case m.mode == modeCreate:
		body = m.viewForm()
	case m.mode == modeShowToken:
		body = m.viewCreated()
	case m.tab == tabStatus:
		body = m.viewStatus()
	case m.tab == tabTokens:
		body = m.viewTokens()
	default:
		body = m.viewAudit()
	}
	lines := strings.Split(body, "\n")
	if h := m.bodyHeight(); len(lines) > h {
		lines = lines[:h]
	} else {
		for len(lines) < h {
			lines = append(lines, "")
		}
	}
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, m.width, "…")
	}
	return strings.Join([]string{m.viewHeader(), m.viewTabs(), "", strings.Join(lines, "\n"), m.viewNotice(), m.viewKeys()}, "\n")
}

func (m *model) viewHeader() string {
	dot, state := styleErr.Render("●"), "unreachable"
	switch {
	case m.statusErr != nil:
		state = m.statusErr.Error()
	case m.checked.IsZero():
		dot, state = styleDim.Render("●"), "connecting…"
	case m.status.Connected && m.status.LoggedIn:
		dot, state = styleAccent.Render("●"), "connected"
	case m.status.QR != "":
		dot, state = styleWarn.Render("●"), "waiting for login"
	default:
		dot, state = styleWarn.Render("●"), m.status.State
	}
	return ansi.Truncate(fmt.Sprintf(" %s %s %s  %s %s", styleBold.Render("wame admin"), styleDim.Render("·"), m.c.Server(), dot, state), m.width, "…")
}

func (m *model) viewTabs() string {
	parts := []string{" "}
	for i, name := range tabNames {
		label := fmt.Sprintf("%d %s", i+1, name)
		if tab(i) == m.tab {
			parts = append(parts, styleTabOn.Render(label))
		} else {
			parts = append(parts, styleTab.Render(label))
		}
	}
	return strings.Join(parts, " ")
}

func (m *model) viewNotice() string {
	switch m.mode {
	case modePair, modeFilter:
		return " " + m.prompt.View()
	case modeConfirmLogout:
		return " " + styleWarn.Render("Log out and unlink WhatsApp on this server? The session is deleted and needs a new login. y/N")
	case modeConfirmRevoke:
		if m.tokSel < len(m.tokens) {
			return " " + styleWarn.Render(fmt.Sprintf("Revoke %s and every token it created? Open streams close within 10 s. y/N", m.tokens[m.tokSel].Name))
		}
	}
	if m.notice == "" {
		return ""
	}
	if m.noticeErr {
		return " " + styleErr.Render(m.notice)
	}
	return " " + styleDim.Render(m.notice)
}

func (m *model) viewKeys() string {
	var keys [][2]string
	switch m.mode {
	case modeCreate:
		keys = [][2]string{{"tab", "next field"}, {"enter", "next / create"}, {"esc", "cancel"}}
	case modeShowToken:
		keys = [][2]string{{"c", "copy"}, {"any key", "close"}}
	case modePair, modeFilter:
		keys = [][2]string{{"enter", "ok"}, {"esc", "cancel"}}
	case modeConfirmLogout, modeConfirmRevoke:
		keys = [][2]string{{"y", "confirm"}, {"any key", "cancel"}}
	default:
		switch m.tab {
		case tabStatus:
			if m.status.LoggedIn {
				keys = append(keys, [2]string{"L", "log out"})
			} else {
				keys = append(keys, [2]string{"p", "pair with phone number"})
			}
		case tabTokens:
			keys = [][2]string{{"j/k", "move"}, {"n", "new"}, {"x", "revoke"}}
		case tabAudit:
			keys = [][2]string{{"j/k", "move"}, {"g/G", "top/bottom"}, {"/", "filter"}}
		}
		keys = append(keys, [2]string{"1-3", "tabs"}, [2]string{"r", "refresh"}, [2]string{"q", "quit"})
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = styleKey.Render(k[0]) + " " + styleDim.Render(k[1])
	}
	return ansi.Truncate(" "+strings.Join(parts, styleFaint.Render("  ·  ")), m.width, "…")
}

func (m *model) viewStatus() string {
	if m.statusErr != nil {
		return "  " + styleErr.Render("Can't reach the server: "+m.statusErr.Error()) + "\n\n  " +
			styleDim.Render("Check the URL and token. Retrying every few seconds.")
	}
	if m.checked.IsZero() {
		return "  " + styleDim.Render("loading…")
	}
	s := m.status
	yes := func(b bool) string {
		if b {
			return styleAccent.Render("yes")
		}
		return styleWarn.Render("no")
	}
	account := styleDim.Render("not linked")
	if s.JID != "" {
		account = "+" + userOf(s.JID)
		if s.PushName != "" {
			account += "  " + styleDim.Render("("+s.PushName+")")
		}
	}
	rows := [][2]string{
		{"State", s.State},
		{"Connected", yes(s.Connected)},
		{"Logged in", yes(s.LoggedIn)},
		{"Account", account},
		{"Checked", styleDim.Render(m.checked.Format("15:04:05"))},
	}
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "  %s %s\n", styleDim.Render(fmt.Sprintf("%-10s", r[0])), r[1])
	}
	switch {
	case m.pairCode != "":
		fmt.Fprintf(&b, "\n  Pairing code  %s\n  %s\n", styleBox.Padding(0, 2).Render(styleBold.Render(m.pairCode)),
			styleDim.Render("On the phone: WhatsApp → Linked devices → Link a device → Link with phone number"))
	case s.QR != "":
		b.WriteString("\n  " + styleBold.Render("Scan to log in") + styleDim.Render("  WhatsApp → Linked devices → Link a device. It refreshes on its own; or press p to use a phone number.") + "\n\n")
		var qr strings.Builder
		qrterminal.GenerateHalfBlock(s.QR, qrterminal.L, &qr)
		for _, l := range strings.Split(strings.TrimRight(qr.String(), "\n"), "\n") {
			b.WriteString("  " + l + "\n")
		}
	case !s.LoggedIn:
		b.WriteString("\n  " + styleDim.Render("Not linked and no QR yet. The server shows one once it connects; or press p to pair with a phone number.") + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m *model) viewTokens() string {
	if m.tokensErr != nil {
		return "  " + styleErr.Render(m.tokensErr.Error())
	}
	if len(m.tokens) == 0 {
		return "  " + styleDim.Render("No tokens yet. Press n to create one for an agent or bot.")
	}
	header := fmt.Sprintf("  %-18s %-16s %-8s %-6s %-12s %-12s %s", "NAME", "SCOPES", "CHATS", "RATE", "EXPIRES", "LAST USED", "CREATED BY")
	lines := []string{styleDim.Render(header)}
	start, end := window(m.tokSel, len(m.tokens), m.bodyHeight()-3)
	for i := start; i < end; i++ {
		t := m.tokens[i]
		chats := "all"
		if len(t.Chats) > 0 {
			chats = fmt.Sprint(len(t.Chats))
		}
		rate := "-"
		if t.Rate > 0 {
			rate = fmt.Sprintf("%d/m", t.Rate)
		}
		expires := "never"
		switch {
		case t.Revoked:
			expires = "revoked"
		case !t.Expires.IsZero():
			expires = relTime(t.Expires)
		}
		lastUsed := "never"
		if !t.LastUsed.IsZero() {
			lastUsed = relTime(t.LastUsed)
		}
		createdBy := t.CreatedBy
		if createdBy == "" {
			createdBy = "owner"
		}
		row := fmt.Sprintf("  %-18s %-16s %-8s %-6s %-12s %-12s %s", fit(t.Name, 18), fit(strings.Join(t.Scopes, ","), 16), chats, rate, expires, lastUsed, createdBy)
		style := lipgloss.NewStyle()
		if t.Revoked || (!t.Expires.IsZero() && t.Expires.Before(time.Now())) {
			style = styleFaint
		}
		if i == m.tokSel {
			style = style.Background(colSel).Width(m.width)
		}
		lines = append(lines, style.Render(row))
	}
	if m.tokSel < len(m.tokens) {
		if t := m.tokens[m.tokSel]; len(t.Chats) > 0 {
			lines = append(lines, "", "  "+styleDim.Render("Chats: ")+strings.Join(t.Chats, ", "))
		}
	}
	return strings.Join(lines, "\n")
}

func (m *model) viewAudit() string {
	if m.auditErr != nil && len(m.audit) == 0 {
		return "  " + styleErr.Render(m.auditErr.Error())
	}
	entries := m.visibleAudit()
	if len(entries) == 0 {
		if m.auditQuery != "" {
			return "  " + styleDim.Render("Nothing matches "+fmt.Sprintf("%q", m.auditQuery)+" in the loaded entries.")
		}
		return "  " + styleDim.Render("The audit log is empty.")
	}
	header := fmt.Sprintf("  %-14s %-14s %-12s %-28s %s", "WHEN", "ACTOR", "ACTION", "TARGET", "DETAIL")
	lines := []string{styleDim.Render(header)}
	start, end := window(m.auditSel, len(entries), m.bodyHeight()-4)
	for i := start; i < end; i++ {
		e := entries[i]
		detail := e.Detail
		style := lipgloss.NewStyle()
		if !e.OK {
			style = styleErr
			detail = strings.TrimSpace(detail + "  ✗ " + e.Error)
		}
		row := fmt.Sprintf("  %-14s %-14s %-12s %-28s %s", e.At.Format("Jan 02 15:04"), fit(e.Actor, 14), fit(e.Action, 12), fit(e.Target, 28), detail)
		if i == m.auditSel {
			style = style.Background(colSel).Width(m.width)
		}
		lines = append(lines, style.Render(ansi.Truncate(row, m.width, "…")))
	}
	footer := fmt.Sprintf("%d of %d loaded", len(entries), len(m.audit))
	if m.auditQuery != "" {
		footer += fmt.Sprintf(", filtered by %q (esc clears)", m.auditQuery)
	}
	switch {
	case m.auditBusy:
		footer += " · loading…"
	case m.auditMore && m.auditQuery == "":
		footer += " · move past the last row for older entries"
	}
	if m.auditSel < len(entries) && !entries[m.auditSel].OK {
		lines = append(lines, "", "  "+styleErr.Render("Error: "+entries[m.auditSel].Error))
	} else {
		lines = append(lines, "", "")
	}
	lines = append(lines, "  "+styleDim.Render(footer))
	return strings.Join(lines, "\n")
}

func (m *model) viewForm() string {
	lines := []string{"  " + styleBold.Render("New token") + styleDim.Render("  for an agent, bot or script. Only its hash is stored; you'll see it once."), ""}
	for i, f := range formFields {
		label := fmt.Sprintf("%-8s", f.label)
		if i == m.formFocus {
			label = styleAccent.Render("› " + label)
		} else {
			label = "  " + styleDim.Render(label)
		}
		lines = append(lines, "  "+label+" "+m.form[i].View())
	}
	lines = append(lines, "",
		"  "+styleDim.Render("Signed in with an admin token rather than the owner secret, you can't grant the admin"),
		"  "+styleDim.Render("scope, and the new token expires no later than yours."))
	return strings.Join(lines, "\n")
}

func (m *model) viewCreated() string {
	box := styleBox.Render(styleBold.Render("Token created") + "\n\n" + m.created + "\n\n" +
		styleDim.Render("Store it now: it can't be shown again. Use it as\nAuthorization: Bearer <token>"))
	return indent(box, "  ")
}

func indent(s, pad string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n")
}

// window returns the slice [start, end) of n rows to show in h lines, keeping sel visible.
func window(sel, n, h int) (int, int) {
	h = max(h, 1)
	if n <= h {
		return 0, n
	}
	start := min(max(sel-h/2, 0), n-h)
	return start, start + h
}

func fit(s string, w int) string {
	if ansi.StringWidth(s) <= w {
		return s
	}
	return ansi.Truncate(s, w, "…")
}

func relTime(t time.Time) string {
	d := time.Until(t)
	future := d > 0
	if !future {
		d = -d
	}
	var s string
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		s = fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		s = fmt.Sprintf("%dh", int(d.Hours()))
	default:
		s = fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	if future {
		return "in " + s
	}
	return s + " ago"
}

func userOf(jid string) string {
	user, _, _ := strings.Cut(jid, "@")
	user, _, _ = strings.Cut(user, ":")
	user, _, _ = strings.Cut(user, ".")
	return user
}
