package admin

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mdp/qrterminal/v3"

	"github.com/williamtcastro/wame/internal/store"
)

var (
	colAccent = lipgloss.Color("42")
	colText   = lipgloss.Color("252")
	colMuted  = lipgloss.Color("244")
	colFaint  = lipgloss.Color("238")
	colErr    = lipgloss.Color("203")
	colWarn   = lipgloss.Color("214")
	colInfo   = lipgloss.Color("39")
	colPurple = lipgloss.Color("141")
	colSel    = lipgloss.Color("236")

	styleDim    = lipgloss.NewStyle().Foreground(colMuted)
	styleFaint  = lipgloss.NewStyle().Foreground(colFaint)
	styleErr    = lipgloss.NewStyle().Foreground(colErr)
	styleWarn   = lipgloss.NewStyle().Foreground(colWarn)
	styleBold   = lipgloss.NewStyle().Bold(true).Foreground(colText)
	styleAccent = lipgloss.NewStyle().Foreground(colAccent)
	styleKey    = lipgloss.NewStyle().Foreground(colText).Background(colSel).Padding(0, 1)
	styleLogo   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(colAccent).Padding(0, 1)
	styleTab    = lipgloss.NewStyle().Padding(0, 1).Foreground(colMuted)
	styleTabOn  = lipgloss.NewStyle().Padding(0, 1).Bold(true).Foreground(colText).Background(colSel)
	styleCard   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colFaint).Padding(0, 1)
	styleModal  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colAccent).Padding(1, 3)
	styleDanger = styleModal.BorderForeground(colErr)
	styleHead   = lipgloss.NewStyle().Foreground(colMuted).Bold(true)

	scopeColors = map[string]lipgloss.Color{"read": colInfo, "send": colWarn, "admin": colErr}
)

func (m *model) bodyHeight() int { return max(m.height-5, 3) }

func (m *model) View() string {
	if m.width == 0 {
		return ""
	}
	var body string
	switch {
	case m.mode == modeCreate:
		body = m.modal(styleModal, m.viewForm())
	case m.mode == modeShowToken:
		body = m.modal(styleModal, m.viewCreated())
	case m.mode == modePair:
		body = m.modal(styleModal, m.viewPair())
	case m.mode == modeConfirmLogout:
		body = m.modal(styleDanger, m.viewConfirmLogout())
	case m.mode == modeConfirmRevoke:
		body = m.modal(styleDanger, m.viewConfirmRevoke())
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
	return strings.Join([]string{m.viewHeader(), styleFaint.Render(strings.Repeat("─", m.width)), strings.Join(lines, "\n"), m.viewNotice(), m.viewKeys()}, "\n")
}

func (m *model) modal(style lipgloss.Style, content string) string {
	return lipgloss.Place(m.width, m.bodyHeight(), lipgloss.Center, lipgloss.Center, style.Render(content))
}

// connection describes the server's state as a colored dot and a short label.
func (m *model) connection() (string, string) {
	switch {
	case m.statusErr != nil:
		return styleErr.Render("●"), "unreachable"
	case m.checked.IsZero():
		return styleDim.Render("●"), "connecting…"
	case m.status.Connected && m.status.LoggedIn:
		return styleAccent.Render("●"), "connected"
	case m.status.QR != "":
		return styleWarn.Render("●"), "waiting for login"
	}
	return styleWarn.Render("●"), m.status.State
}

func (m *model) viewHeader() string {
	parts := []string{" " + styleLogo.Render("◆ wame admin")}
	for i, name := range tabNames {
		label := fmt.Sprintf("%d %s", i+1, name)
		switch tab(i) {
		case tabTokens:
			if n := m.activeTokens(); n > 0 {
				label += " " + styleDim.Render(fmt.Sprint(n))
			}
		case tabAudit:
			if n := m.recentFailures(); n > 0 {
				label += " " + styleErr.Render(fmt.Sprintf("✗%d", n))
			}
		}
		if tab(i) == m.tab {
			parts = append(parts, styleTabOn.Render(label))
		} else {
			parts = append(parts, styleTab.Render(label))
		}
	}
	left := strings.Join(parts, " ")
	dot, state := m.connection()
	right := dot + " " + state + styleFaint.Render("  ·  ") + styleDim.Render(m.c.base.Host)
	if m.statusErr == nil && m.latency > 0 {
		right += styleFaint.Render("  ·  ") + styleDim.Render(fmt.Sprintf("%d ms", m.latency.Milliseconds()))
	}
	right += " "
	gap := m.width - ansi.StringWidth(left) - ansi.StringWidth(right)
	if gap < 2 {
		return ansi.Truncate(left+"  "+right, m.width, "…")
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m *model) viewNotice() string {
	if m.mode == modeFilter {
		return " " + styleAccent.Render("/") + " " + m.prompt.View()
	}
	if m.notice == "" {
		return ""
	}
	if m.noticeErr {
		return " " + styleErr.Render("✗ "+m.notice)
	}
	icon := styleAccent.Render("✓ ")
	if strings.HasSuffix(m.notice, "…") {
		icon = styleDim.Render("◌ ")
	}
	return " " + icon + styleDim.Render(m.notice)
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
	return ansi.Truncate(" "+strings.Join(parts, "   "), m.width, "…")
}

// card is a bordered box of total width w with a title.
func card(title string, w int, body string) string {
	return styleCard.Width(max(w-2, 10)).Render(styleBold.Render(title) + "\n" + body)
}

func kv(rows [][2]string) string {
	w := 0
	for _, r := range rows {
		w = max(w, ansi.StringWidth(r[0]))
	}
	lines := make([]string, len(rows))
	for i, r := range rows {
		lines[i] = styleDim.Render(pad(r[0], w)) + "  " + r[1]
	}
	return strings.Join(lines, "\n")
}

func (m *model) viewStatus() string {
	if m.statusErr != nil {
		return indent(card("Can't reach the server", min(m.width-2, 90),
			styleErr.Render(m.statusErr.Error())+"\n\n"+
				styleDim.Render("Check the URL and token. Retrying every few seconds.")), " ")
	}
	if m.checked.IsZero() {
		return "  " + styleDim.Render("loading…")
	}
	s := m.status
	colW := max((m.width-3)/2, 36)
	yes := func(b bool, on, off string) string {
		if b {
			return styleAccent.Render("● " + on)
		}
		return styleWarn.Render("○ " + off)
	}
	account := styleDim.Render("not linked")
	if s.JID != "" {
		account = styleBold.Render("+" + userOf(s.JID))
		if s.PushName != "" {
			account += " " + styleDim.Render(s.PushName)
		}
	}
	whatsapp := card("WhatsApp", colW, kv([][2]string{
		{"Account", account},
		{"Session", yes(s.LoggedIn, "linked", "not linked")},
		{"Socket", yes(s.Connected, "connected", "disconnected")},
		{"State", styleDim.Render(s.State)},
	}))
	server := card("Server", colW, kv([][2]string{
		{"URL", m.c.Server()},
		{"Latency", fmt.Sprintf("%d ms", m.latency.Milliseconds())},
		{"Checked", styleDim.Render(m.checked.Format("15:04:05") + ", every " + pollEvery.String())},
	}))
	left := lipgloss.JoinVertical(lipgloss.Left, whatsapp, server)

	var right string
	switch {
	case m.pairCode != "":
		right = card("Pairing code", colW, "\n  "+styleLogo.Render(m.pairCode)+"\n\n"+
			styleDim.Render("On the phone: WhatsApp → Linked devices →\nLink a device → Link with phone number instead."))
	case s.QR != "":
		var qr strings.Builder
		qrterminal.GenerateHalfBlock(s.QR, qrterminal.L, &qr)
		right = card("Scan to log in", colW, styleDim.Render("WhatsApp → Linked devices → Link a device.\nIt refreshes on its own; p pairs by phone number.")+
			"\n"+strings.TrimRight(qr.String(), "\n"))
	default:
		right = lipgloss.JoinVertical(lipgloss.Left, card("Tokens", colW, m.tokenSummary()), card("Activity", colW, m.activitySummary()))
		if !s.LoggedIn {
			right = lipgloss.JoinVertical(lipgloss.Left, card("Not linked", colW,
				styleWarn.Render("No QR yet.")+styleDim.Render(" The server shows one once it\nconnects; or press p to pair with a phone number.")), right)
		}
	}
	return indent(lipgloss.JoinHorizontal(lipgloss.Top, left, " ", right), " ")
}

func (m *model) activeTokens() int {
	n := 0
	for _, t := range m.tokens {
		if tokenLive(t) {
			n++
		}
	}
	return n
}

func tokenLive(t store.Token) bool {
	return !t.Revoked && (t.Expires.IsZero() || t.Expires.After(time.Now()))
}

func (m *model) tokenSummary() string {
	if m.tokensErr != nil {
		return styleErr.Render(m.tokensErr.Error())
	}
	var expiring, revoked int
	for _, t := range m.tokens {
		switch {
		case !tokenLive(t):
			revoked++
		case !t.Expires.IsZero() && time.Until(t.Expires) < 7*24*time.Hour:
			expiring++
		}
	}
	exp := styleDim.Render("0")
	if expiring > 0 {
		exp = styleWarn.Render(fmt.Sprint(expiring))
	}
	return kv([][2]string{
		{"Active", styleBold.Render(fmt.Sprint(m.activeTokens()))},
		{"Expiring in 7d", exp},
		{"Revoked/expired", styleDim.Render(fmt.Sprint(revoked))},
	})
}

func (m *model) recentFailures() int {
	n := 0
	since := time.Now().Add(-24 * time.Hour)
	for _, e := range m.audit {
		if e.At.After(since) && !e.OK {
			n++
		}
	}
	return n
}

func (m *model) activitySummary() string {
	if m.auditErr != nil && len(m.audit) == 0 {
		return styleErr.Render(m.auditErr.Error())
	}
	since := time.Now().Add(-24 * time.Hour)
	var total int
	for _, e := range m.audit {
		if e.At.After(since) {
			total++
		}
	}
	fails := styleDim.Render("0")
	if n := m.recentFailures(); n > 0 {
		fails = styleErr.Render(fmt.Sprint(n))
	}
	last := styleDim.Render("none")
	if len(m.audit) > 0 {
		e := m.audit[0]
		last = actionStyle(e.Action).Render(e.Action) + styleDim.Render(" by "+e.Actor+", "+relTime(e.At))
	}
	return kv([][2]string{
		{"Actions (24h)", styleBold.Render(fmt.Sprint(total))},
		{"Failed (24h)", fails},
		{"Latest", last},
	})
}

func scopeBadges(scopes []string) string {
	parts := make([]string, len(scopes))
	for i, s := range scopes {
		parts[i] = lipgloss.NewStyle().Foreground(scopeColors[s]).Render(s)
	}
	return strings.Join(parts, " ")
}

func (m *model) viewTokens() string {
	if m.tokensErr != nil {
		return indent(card("Tokens", min(m.width-2, 90), styleErr.Render(m.tokensErr.Error())), " ")
	}
	if len(m.tokens) == 0 {
		return indent(card("No tokens yet", min(m.width-2, 70),
			styleDim.Render("Press n to create one for an agent, bot or script.\nEach gets its own scopes, chats, rate and expiry.")), " ")
	}
	cols := []int{20, 18, 10, 8, 12, 12}
	row := func(cells ...string) string {
		var b strings.Builder
		for i, c := range cells {
			if i < len(cols) {
				b.WriteString(pad(fit(c, cols[i]), cols[i]) + " ")
			} else {
				b.WriteString(c)
			}
		}
		return b.String()
	}
	detail := m.tokenDetail()
	lines := []string{"   " + styleHead.Render(row("NAME", "SCOPES", "CHATS", "RATE", "EXPIRES", "LAST USED", "CREATED BY"))}
	start, end := window(m.tokSel, len(m.tokens), m.bodyHeight()-2-lipgloss.Height(detail))
	for i := start; i < end; i++ {
		t := m.tokens[i]
		chats := "all"
		if len(t.Chats) > 0 {
			chats = plural(len(t.Chats), "chat")
		}
		rate := "global"
		if t.Rate > 0 {
			rate = fmt.Sprintf("%d/min", t.Rate)
		}
		expires := "never"
		switch {
		case t.Revoked:
			expires = styleErr.Render("revoked")
		case !t.Expires.IsZero() && !tokenLive(t):
			expires = styleErr.Render("expired")
		case !t.Expires.IsZero() && time.Until(t.Expires) < 7*24*time.Hour:
			expires = styleWarn.Render(relTime(t.Expires))
		case !t.Expires.IsZero():
			expires = relTime(t.Expires)
		}
		lastUsed := styleDim.Render("never")
		if !t.LastUsed.IsZero() {
			lastUsed = relTime(t.LastUsed)
		}
		createdBy := cmpOr(t.CreatedBy, "owner")
		name, scopes := t.Name, scopeBadges(t.Scopes)
		if !tokenLive(t) {
			name, scopes = styleDim.Strikethrough(true).Render(name), styleDim.Render(strings.Join(t.Scopes, " "))
		}
		line := row(name, scopes, chats, rate, expires, lastUsed, styleDim.Render(createdBy))
		if i == m.tokSel {
			lines = append(lines, " "+styleAccent.Render("▌")+" "+lipgloss.NewStyle().Background(colSel).Width(m.width-3).Render(line))
		} else {
			lines = append(lines, "   "+line)
		}
	}
	lines = append(lines, "", indent(detail, " "))
	return strings.Join(lines, "\n")
}

func (m *model) tokenDetail() string {
	if m.tokSel >= len(m.tokens) {
		return ""
	}
	t := m.tokens[m.tokSel]
	chats := "all chats"
	if len(t.Chats) > 0 {
		names := make([]string, len(t.Chats))
		for i, c := range t.Chats {
			names[i] = displayJID(c)
		}
		chats = strings.Join(names, ", ")
	}
	expires := "never"
	if !t.Expires.IsZero() {
		expires = t.Expires.Format("2006-01-02 15:04") + styleDim.Render(" ("+relTime(t.Expires)+")")
	}
	created := t.Created.Format("2006-01-02 15:04") + styleDim.Render(" by "+cmpOr(t.CreatedBy, "the owner"))
	state := styleAccent.Render("● active")
	if t.Revoked {
		state = styleErr.Render("● revoked")
	} else if !tokenLive(t) {
		state = styleErr.Render("● expired")
	}
	left := kv([][2]string{{"ID", styleDim.Render(t.ID)}, {"State", state}, {"Scopes", scopeBadges(t.Scopes)}, {"Chats", chats}})
	right := kv([][2]string{{"Created", created}, {"Expires", expires}, {"Rate", cmpOr(rateText(t.Rate), "global limit only")}})
	w := min(m.width-2, 120)
	return card(t.Name, w, lipgloss.JoinHorizontal(lipgloss.Top, lipgloss.NewStyle().Width((w-4)/2).Render(left), right))
}

func rateText(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("%d sends per minute", n)
}

func actionStyle(action string) lipgloss.Style {
	switch {
	case strings.HasPrefix(action, "token"):
		return lipgloss.NewStyle().Foreground(colPurple)
	case action == "pair" || action == "logout" || action == "logged_out":
		return lipgloss.NewStyle().Foreground(colWarn)
	case action == "delete":
		return lipgloss.NewStyle().Foreground(colErr)
	}
	return lipgloss.NewStyle().Foreground(colInfo)
}

func (m *model) viewAudit() string {
	if m.auditErr != nil && len(m.audit) == 0 {
		return indent(card("Audit log", min(m.width-2, 90), styleErr.Render(m.auditErr.Error())), " ")
	}
	entries := m.visibleAudit()
	if len(entries) == 0 {
		msg := "The audit log is empty."
		if m.auditQuery != "" {
			msg = fmt.Sprintf("Nothing matches %q in the loaded entries. Esc clears the filter.", m.auditQuery)
		}
		return indent(card("Audit log", min(m.width-2, 90), styleDim.Render(msg)), " ")
	}
	cols := []int{2, 13, 16, 13, 22}
	row := func(cells ...string) string {
		var b strings.Builder
		for i, c := range cells {
			if i < len(cols) {
				b.WriteString(pad(fit(c, cols[i]), cols[i]) + " ")
			} else {
				b.WriteString(c)
			}
		}
		return b.String()
	}
	detail := m.auditDetail(entries)
	lines := []string{"   " + styleHead.Render(row("", "WHEN", "ACTOR", "ACTION", "TARGET", "DETAIL"))}
	start, end := window(m.auditSel, len(entries), m.bodyHeight()-3-lipgloss.Height(detail))
	for i := start; i < end; i++ {
		e := entries[i]
		icon, detail := styleAccent.Render("✓"), styleDim.Render(e.Detail)
		if !e.OK {
			icon, detail = styleErr.Render("✗"), styleErr.Render(strings.Join(nonEmpty(e.Detail, e.Error), " · "))
		}
		line := row(icon, styleDim.Render(when(e.At)), e.Actor, actionStyle(e.Action).Render(e.Action), displayJID(e.Target), detail)
		if i == m.auditSel {
			lines = append(lines, " "+styleAccent.Render("▌")+" "+lipgloss.NewStyle().Background(colSel).Width(m.width-3).Render(ansi.Truncate(line, m.width-3, "…")))
		} else {
			lines = append(lines, "   "+line)
		}
	}
	footer := fmt.Sprintf("%d of %d loaded", len(entries), len(m.audit))
	if m.auditQuery != "" {
		footer += fmt.Sprintf(" · filtered by %q (esc clears)", m.auditQuery)
	}
	switch {
	case m.auditBusy:
		footer += " · loading…"
	case m.auditMore && m.auditQuery == "":
		footer += " · move past the last row for older entries"
	}
	lines = append(lines, "   "+styleFaint.Render(footer), indent(detail, " "))
	return strings.Join(lines, "\n")
}

func (m *model) auditDetail(entries []store.AuditEntry) string {
	if m.auditSel >= len(entries) {
		return ""
	}
	e := entries[m.auditSel]
	result := styleAccent.Render("✓ ok")
	if !e.OK {
		result = styleErr.Render("✗ " + e.Error)
	}
	rows := [][2]string{
		{"When", e.At.Format("2006-01-02 15:04:05") + styleDim.Render(" ("+relTime(e.At)+")")},
		{"Who", e.Actor},
		{"Target", cmpOr(displayJID(e.Target), styleDim.Render("none"))},
		{"Detail", cmpOr(e.Detail, styleDim.Render("none"))},
		{"Result", result},
	}
	return card(fmt.Sprintf("#%d %s", e.ID, e.Action), min(m.width-2, 120), kv(rows))
}

func (m *model) viewForm() string {
	lines := []string{styleBold.Render("New token"), styleDim.Render("For an agent, bot or script. Only its hash is stored; you'll see it once."), ""}
	for i, f := range formFields {
		label := styleDim.Render(pad(f.label, 8))
		marker := "  "
		if i == m.formFocus {
			label, marker = styleAccent.Render(pad(f.label, 8)), styleAccent.Render("› ")
		}
		lines = append(lines, marker+label+" "+m.form[i].View())
	}
	lines = append(lines, "",
		styleDim.Render("With an admin token rather than the owner secret, you can't grant"),
		styleDim.Render("the admin scope, and the new token expires no later than yours."))
	return lipgloss.NewStyle().Width(min(m.width-12, 84)).Render(strings.Join(lines, "\n"))
}

func (m *model) viewCreated() string {
	return styleAccent.Render("✓ Token created") + "\n\n" +
		lipgloss.NewStyle().Background(colSel).Padding(0, 1).Render(m.created) + "\n\n" +
		styleWarn.Render("Store it now: it can't be shown again.") + "\n" +
		styleDim.Render("Send it as  Authorization: Bearer <token>")
}

func (m *model) viewPair() string {
	return styleBold.Render("Pair with a phone number") + "\n" +
		styleDim.Render("The server asks WhatsApp for an 8-character code to enter on that phone.") + "\n\n" +
		m.prompt.View()
}

func (m *model) viewConfirmLogout() string {
	return styleErr.Render("Log out of WhatsApp?") + "\n\n" +
		"This unlinks the device and deletes the session on\n" + styleBold.Render(m.c.base.Host) + ". It needs a new QR or pairing code after.\n\n" +
		styleKey.Render("y") + " log out   " + styleKey.Render("any key") + " cancel"
}

func (m *model) viewConfirmRevoke() string {
	if m.tokSel >= len(m.tokens) {
		return ""
	}
	t := m.tokens[m.tokSel]
	return styleErr.Render("Revoke "+t.Name+"?") + "\n\n" +
		"It stops working at once, with every token it created.\nOpen streams close within 10 s. This can't be undone.\n\n" +
		styleKey.Render("y") + " revoke   " + styleKey.Render("any key") + " cancel"
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
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

func pad(s string, w int) string {
	if n := ansi.StringWidth(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

func nonEmpty(parts ...string) []string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// when is a compact timestamp: the time today, the weekday this week, the date otherwise.
func when(t time.Time) string {
	now := time.Now()
	switch {
	case t.YearDay() == now.YearDay() && t.Year() == now.Year():
		return t.Format("15:04")
	case now.Sub(t) < 6*24*time.Hour:
		return t.Format("Mon 15:04")
	}
	return t.Format("Jan 02 15:04")
}

func displayJID(jid string) string {
	if strings.HasSuffix(jid, "@s.whatsapp.net") {
		return "+" + userOf(jid)
	}
	return jid
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
