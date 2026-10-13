package admin

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
	"github.com/williamtcastro/wame/internal/termui"
)

const (
	contactsLimit = 5000
	notesLines    = 3
)

type (
	contactsMsg struct {
		seq  int
		list []bot.Contact
		err  error
	}
	contactTagsMsg struct {
		tags []store.TagCount
		err  error
	}
	contactViewMsg struct {
		seq int
		jid string
		v   bot.ContactView
		err error
	}
	// contactDoneMsg ends a write on the Contacts tab; missing is the notice for a 404.
	contactDoneMsg struct {
		notice, missing string
		err             error
	}
	importCheckedMsg struct {
		path string
		data []byte
		res  store.ImportResult
		err  error
	}
)

// contactForm is what the edit form showed when it opened, so only fields the user changed are sent.
type contactForm struct {
	jid, label        string
	name, tags, notes string
	notesOK           bool
}

func (m *model) loadContactsTab() tea.Cmd {
	return tea.Batch(m.loadContacts(), m.loadContactTags())
}

func (m *model) loadContacts() tea.Cmd {
	if m.c == nil {
		return nil
	}
	seq, tags := m.contactsReq.start(), strings.Fields(m.contactTag)
	return m.call(func(ctx context.Context, c *Client) tea.Msg {
		list, err := c.Contacts(ctx, ContactsQuery{Tags: tags, Limit: contactsLimit})
		return contactsMsg{seq, list, err}
	})
}

func (m *model) loadContactTags() tea.Cmd {
	return m.call(func(ctx context.Context, c *Client) tea.Msg {
		tags, err := c.ContactTags(ctx)
		return contactTagsMsg{tags, err}
	})
}

func (m *model) loadContactView(jid string) tea.Cmd {
	if jid == "" || m.c == nil {
		return nil
	}
	seq := m.contactViewReq.start()
	return m.call(func(ctx context.Context, c *Client) tea.Msg {
		v, err := c.Contact(ctx, jid)
		return contactViewMsg{seq, jid, v, err}
	})
}

// safeContact cleans what other people and imports control. The JID stays as the server sent it:
// it's only used in requests, and drawn through displayJID(safe(…)).
func safeContact(c bot.Contact) bot.Contact {
	c.Phone, c.Display, c.Source = safe(c.Phone), safe(c.Display), safe(c.Source)
	c.Own, c.Book, c.Business, c.Push = safe(c.Own), safe(c.Book), safe(c.Business), safe(c.Push)
	c.Tags = safeList(c.Tags)
	return c
}

func safeList(list []string) []string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = safe(s)
	}
	return out
}

func safeView(v bot.ContactView) bot.ContactView {
	v.Contact = safeContact(v.Contact)
	if v.Own != nil {
		o := *v.Own
		o.Name, o.Notes, o.Language, o.TimeZone = safe(o.Name), safe(o.Notes), safe(o.Language), safe(o.TimeZone)
		o.CreatedBy, o.UpdatedBy, o.Tags = safe(o.CreatedBy), safe(o.UpdatedBy), safeList(o.Tags)
		v.Own = &o
	}
	return v
}

func (m *model) applyContactTags(msg contactTagsMsg) {
	m.contactTagsErr = msg.err
	if msg.err != nil {
		return
	}
	for i := range msg.tags {
		msg.tags[i].Tag = safe(msg.tags[i].Tag)
	}
	m.contactTags = msg.tags
}

func (m *model) applyContacts(msg contactsMsg) tea.Cmd {
	if !m.contactsReq.done(msg.seq) {
		return nil
	}
	m.contactsErr = msg.err
	if msg.err != nil {
		return nil
	}
	selected, _ := m.selectedContact()
	for i := range msg.list {
		msg.list[i] = safeContact(msg.list[i])
	}
	m.contacts = msg.list
	if jump := m.contactJump; jump != "" {
		m.contactJump, selected.JID = "", jump
		if !slices.ContainsFunc(m.contacts, func(c bot.Contact) bool { return c.JID == jump }) {
			num := displayJID(safe(jump))
			m.contacts = append([]bot.Contact{{JID: jump, Phone: num, Names: bot.Names{Display: m.chatName(jump), Source: bot.SourceNumber}}}, m.contacts...)
		}
	}
	vis := m.visibleContacts()
	if i := slices.IndexFunc(vis, func(c bot.Contact) bool { return c.JID == selected.JID }); i >= 0 {
		m.contactSel = i
	}
	m.contactSel = clamp(m.contactSel, len(vis))
	return m.contactSelected()
}

// contactSelected loads the selected person's card unless it's the one shown.
func (m *model) contactSelected() tea.Cmd {
	c, ok := m.selectedContact()
	if !ok || (c.JID == m.contactView.JID && m.contactViewErr == nil) {
		return nil
	}
	return m.loadContactView(c.JID)
}

func (m *model) applyContactView(msg contactViewMsg) {
	if !m.contactViewReq.done(msg.seq) {
		return
	}
	if c, ok := m.selectedContact(); !ok || c.JID != msg.jid {
		return
	}
	m.contactViewErr = msg.err
	if msg.err != nil {
		m.contactView = bot.ContactView{Contact: bot.Contact{JID: msg.jid}}
		return
	}
	m.contactView = safeView(msg.v)
	m.contactView.JID = msg.jid
	if i := slices.IndexFunc(m.contacts, func(c bot.Contact) bool { return c.JID == msg.jid }); i >= 0 {
		m.contacts[i] = m.contactView.Contact
	}
}

func (m *model) visibleContacts() []bot.Contact {
	return filtered(m.contacts, m.contactQuery, func(c bot.Contact) string {
		return strings.ToLower(strings.Join([]string{c.Display, c.Own, c.Book, c.Business, c.Push, c.Phone, c.JID, strings.Join(c.Tags, " ")}, " "))
	})
}

func (m *model) selectedContact() (bot.Contact, bool) {
	if vis := m.visibleContacts(); m.contactSel < len(vis) {
		return vis[m.contactSel], true
	}
	return bot.Contact{}, false
}

// contactsRefusal is why the tab can't be used at all, or "" when it can.
func (m *model) contactsRefusal() string {
	switch {
	case isNotFound(m.contactTagsErr), isNotFound(m.contactsErr):
		return contactsUpdate
	case m.contactsErr != nil:
		return "the contacts didn't load: " + m.contactsErr.Error()
	}
	return ""
}

// contactsReadOnly reports a token that can list contacts but not change them: writes and the full
// card (notes) need the admin scope, and a token limited to chats can't use tags.
func (m *model) contactsReadOnly() bool {
	return m.contactDenied || isForbidden(m.tokensErr) || isForbidden(m.contactTagsErr)
}

const (
	contactsScopeHint = "this token can't change contacts: it needs the admin scope"
	contactsUpdate    = "this server doesn't have contacts yet: update wame on it"
)

// contactsErrText explains a failed contacts call; a 404 there means a server older than the routes.
func contactsErrText(err error) string {
	if isNotFound(err) {
		return contactsUpdate
	}
	return err.Error()
}

func (m *model) contactsKey(k string) tea.Cmd {
	n := len(m.visibleContacts())
	if why := m.contactsRefusal(); why != "" && k != "esc" {
		if _, ok := termui.ListMove(k, m.contactSel, n, 1); ok || slices.Contains([]string{"/", "t", "e", "x", "I", "E", "enter"}, k) {
			m.setNotice(why, true)
			return nil
		}
	}
	if sel, ok := termui.ListMove(k, m.contactSel, n, m.bodyHeight()/2); ok {
		m.contactSel = sel
		return m.contactSelected()
	}
	if m.contactsReadOnly() && slices.Contains([]string{"e", "x", "I", "E"}, k) || isForbidden(m.contactTagsErr) && k == "t" {
		m.setNotice(contactsScopeHint, true)
		return nil
	}
	c, ok := m.selectedContact()
	switch k {
	case "/":
		m.listFilterKey(k, "filter by name, number or tag")
	case "esc":
		if m.contactQuery == "" && m.contactTag != "" {
			m.contactTag = ""
			return m.loadContacts()
		}
		m.listFilterKey(k, "")
		return m.contactSelected()
	case "t":
		m.startPrompt(modeContactTag, "tags that must all be set, e.g. clients vip; empty shows everyone")
		m.prompt.SetValue(m.contactTag)
		m.prompt.CursorEnd()
		tags := make([]string, len(m.contactTags))
		for i, t := range m.contactTags {
			tags[i] = t.Tag
		}
		m.prompt.ShowSuggestions = true
		m.prompt.SetSuggestions(tags)
	case "e":
		if !ok {
			m.setNotice("no contact marked", true)
			return nil
		}
		m.startContactForm(c)
	case "x":
		if !ok {
			m.setNotice("no contact marked", true)
			return nil
		}
		m.confirmContact = c.JID
		m.confirmContactLabel = contactLabel(c)
		m.mode = modeConfirmDeleteContact
	case "I":
		m.startPrompt(modeImportContacts, "path to a CSV file: number,name,tags,notes,language,time_zone")
	case "E":
		m.startPrompt(modeExportContacts, "path of a new CSV file, e.g. ~/wame-contacts.csv")
	case "enter":
		if !ok {
			return nil
		}
		i := slices.IndexFunc(m.chats, func(ch store.Chat) bool { return ch.JID == c.JID })
		if i < 0 {
			m.setNotice("no chat with "+contactLabel(c)+" is stored on the server", true)
			return nil
		}
		m.chatSel = i
		return m.setTab(tabChats)
	}
	return nil
}

// jumpToContact opens the Contacts tab on a chat from the Chats tab.
func (m *model) jumpToContact(jid string) tea.Cmd {
	switch {
	case jid == "":
		return nil
	case !strings.HasSuffix(jid, "@s.whatsapp.net") && !strings.HasSuffix(jid, "@lid"):
		m.setNotice("contacts are people, not groups", true)
		return nil
	}
	m.contactJump, m.contactQuery, m.contactTag = jid, "", ""
	return m.setTab(tabContacts)
}

// contactLabel names a person as "Maria (+5511999999999)", or just the number.
func contactLabel(c bot.Contact) string {
	num := cmp.Or(c.Phone, displayJID(safe(c.JID)))
	if c.Display == "" || c.Display == num {
		return num
	}
	return c.Display + " (" + num + ")"
}

// contactTagKey edits the tag filter; enter applies it (empty clears it), esc keeps the old one.
func (m *model) contactTagKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc":
		m.mode = modeNormal
		return m, nil
	case "enter":
		m.mode = modeNormal
		m.contactTag = strings.ToLower(strings.Join(strings.Fields(m.prompt.Value()), " "))
		return m, m.loadContacts()
	}
	var cmd tea.Cmd
	m.prompt, cmd = m.prompt.Update(k)
	return m, cmd
}

func (m *model) startContactForm(c bot.Contact) {
	f := contactForm{jid: c.JID, label: contactLabel(c), name: c.Own, tags: strings.Join(c.Tags, " ")}
	if v := m.contactView; v.JID == c.JID && m.contactViewErr == nil {
		if v.Own != nil {
			f.name, f.tags, f.notes = v.Own.Name, strings.Join(v.Own.Tags, " "), v.Own.Notes
		}
		f.notesOK = true
	}
	f.name, f.notes = strings.TrimSpace(f.name), strings.TrimSpace(f.notes)
	values := []string{f.name, f.tags}
	limits := []int{bot.MaxContactName, store.MaxContactTags * (bot.MaxContactTag + 1)}
	if f.notesOK {
		values, limits = append(values, f.notes), append(limits, bot.MaxContactNotes)
	}
	m.form = make([]textinput.Model, len(values))
	for i, v := range values {
		in := textinput.New()
		in.Prompt, in.CharLimit = "", limits[i]
		in.SetValue(v)
		m.form[i] = in
	}
	m.contactForm, m.formFocus, m.mode, m.notice = f, 0, modeEditContact, ""
	m.form[0].Focus()
	m.form[0].CursorEnd()
}

func (m *model) contactFormKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc":
		m.form, m.mode = nil, modeNormal
		m.setNotice("nothing changed", false)
		return m, nil
	case "tab", "down":
		m.focusField(m.formFocus + 1)
		return m, nil
	case "shift+tab", "up":
		m.focusField(m.formFocus - 1)
		return m, nil
	case "enter":
		if m.formFocus < len(m.form)-1 {
			m.focusField(m.formFocus + 1)
			return m, nil
		}
		f, in := m.contactForm, m.contactInput()
		m.form, m.mode = nil, modeNormal
		if in.Name == nil && in.Notes == nil && len(in.AddTags)+len(in.RemoveTags) == 0 {
			m.setNotice("nothing changed", false)
			return m, nil
		}
		m.setNotice("saving "+f.label+"…", false)
		return m, m.call(func(ctx context.Context, c *Client) tea.Msg {
			_, err := c.PatchContact(ctx, f.jid, in)
			return contactDoneMsg{notice: "saved " + f.label, err: err}
		})
	}
	var cmd tea.Cmd
	m.form[m.formFocus], cmd = m.form[m.formFocus].Update(k)
	return m, cmd
}

// contactInput is a PATCH with only what the form changed; tags go as additions and removals, so
// another panel's tag edit in the meantime isn't undone.
func (m *model) contactInput() bot.ContactInput {
	f := m.contactForm
	var in bot.ContactInput
	v := func(i int) string { return strings.TrimSpace(m.form[i].Value()) }
	if name := v(0); name != f.name {
		in.Name = &name
	}
	before, after := tagSet(f.tags), tagSet(v(1))
	for _, t := range after {
		if !slices.Contains(before, t) {
			in.AddTags = append(in.AddTags, t)
		}
	}
	for _, t := range before {
		if !slices.Contains(after, t) {
			in.RemoveTags = append(in.RemoveTags, t)
		}
	}
	if f.notesOK {
		if notes := v(2); notes != f.notes {
			in.Notes = &notes
		}
	}
	return in
}

func tagSet(s string) []string {
	return store.NormalizeTags(splitList(strings.ToLower(s)))
}

func (m *model) removeContact(jid, label string) tea.Cmd {
	m.setNotice("deleting your fields for "+label+"…", false)
	return m.call(func(ctx context.Context, c *Client) tea.Msg {
		err := c.DeleteContact(ctx, jid)
		return contactDoneMsg{notice: "deleted your name, tags and notes for " + label, missing: "nothing of yours was stored for " + label, err: err}
	})
}

// checkImport reads the file and asks the server for a dry run; the import itself waits for a confirm
// and sends the same bytes.
func (m *model) checkImport(path string) tea.Cmd {
	m.setNotice("checking "+path+"…", false)
	return m.call(func(ctx context.Context, c *Client) tea.Msg {
		data, err := readImport(path)
		if err != nil {
			return importCheckedMsg{path: path, err: err}
		}
		res, err := c.ImportContacts(ctx, data, true)
		return importCheckedMsg{path, data, res, err}
	})
}

func readImport(path string) ([]byte, error) {
	f, err := os.Open(expandHome(path))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, bot.MaxContactImportBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > bot.MaxContactImportBytes {
		return nil, fmt.Errorf("%s is larger than %d MB: split it", path, bot.MaxContactImportBytes>>20)
	}
	return data, nil
}

func (m *model) applyImportChecked(msg importCheckedMsg) {
	switch {
	case msg.err != nil:
		m.setNotice("not imported: "+contactsErrText(msg.err), true)
	case msg.res.Added+msg.res.Updated == 0:
		m.setNotice(fmt.Sprintf("%s: nothing to change (%s already as in the file)", msg.path, plural(msg.res.Unchanged, "contact")), false)
	case m.mode != modeNormal:
		m.setNotice("import of "+msg.path+" dropped: another dialog was open", true)
	default:
		m.importPath, m.importData, m.importDry = msg.path, msg.data, msg.res
		m.mode, m.notice = modeConfirmImport, ""
	}
}

func (m *model) runImport() tea.Cmd {
	path, data := m.importPath, m.importData
	m.importData = nil
	m.setNotice("importing "+path+"…", false)
	return m.call(func(ctx context.Context, c *Client) tea.Msg {
		res, err := c.ImportContacts(ctx, data, false)
		if err != nil {
			return contactDoneMsg{err: fmt.Errorf("not imported: %w", err)}
		}
		return contactDoneMsg{notice: fmt.Sprintf("imported %s: %d added, %d updated, %d unchanged", path, res.Added, res.Updated, res.Unchanged)}
	})
}

// exportContacts saves the server's own list to a new file; it never overwrites one.
func (m *model) exportContacts(path string) tea.Cmd {
	m.setNotice("exporting…", false)
	return m.call(func(ctx context.Context, c *Client) tea.Msg {
		data, err := c.ExportContacts(ctx)
		if err != nil {
			return noticeMsg{"not exported: " + contactsErrText(err), true}
		}
		if err := writeNew(expandHome(path), data); err != nil {
			return noticeMsg{"not exported: " + err.Error(), true}
		}
		return noticeMsg{"exported your contacts to " + path, false}
	})
}

func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s already exists: pick a new name", path)
	}
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func expandHome(path string) string {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return path
}

func (m *model) applyContactDone(msg contactDoneMsg) tea.Cmd {
	switch {
	case isForbidden(msg.err):
		m.contactDenied = true
		m.setNotice(contactsScopeHint, true)
		return nil
	case isNotFound(msg.err) && msg.missing != "":
		m.setNotice(msg.missing, true)
	case msg.err != nil:
		m.setNotice(contactsErrText(msg.err), true)
		return nil
	default:
		m.setNotice(msg.notice, false)
	}
	m.contactView, m.contactViewErr = bot.ContactView{}, nil
	return tea.Batch(m.loadContactsTab(), m.loadAudit(false))
}

func (m *model) viewContacts() string {
	switch {
	case isNotFound(m.contactTagsErr):
		return m.errCard("Contacts", m.contactTagsErr, "your own contact list (GET /contacts/tags)")
	case m.contactsErr != nil:
		return m.errCard("Contacts", m.contactsErr, "the contact list (GET /contacts)")
	}
	vis := m.visibleContacts()
	head := fmt.Sprintf("contacts · %d of %d", len(vis), len(m.contacts))
	if len(m.contacts) >= contactsLimit {
		head += fmt.Sprintf(" (the first %d)", contactsLimit)
	}
	if m.contactTag != "" {
		head += fmt.Sprintf(" · tagged %s (esc clears)", safe(m.contactTag))
	}
	if m.contactQuery != "" {
		head += fmt.Sprintf(" · filtered by %q (esc clears)", safe(m.contactQuery))
	}
	if m.contactsReadOnly() {
		head += " · read-only: changes need the admin scope"
	}
	detail := m.contactDetail()
	lines := []string{"   " + styleDim.Render(head)}
	cols := []int{30, 9, 17}
	lines = append(lines, "   "+styleHead.Render(table(cols, "NAME", "SOURCE", "NUMBER", "TAGS")))
	if len(vis) == 0 {
		msg := "nobody yet: names come from the phone's address book, your edits and imports (I)"
		if m.contactQuery != "" || m.contactTag != "" {
			msg = "nothing matches"
		}
		return strings.Join(append(lines, "   "+styleDim.Render(msg)), "\n")
	}
	start, end := window(m.contactSel, len(vis), m.bodyHeight()-len(lines)-1-lipgloss.Height(detail))
	for i := start; i < end; i++ {
		c := vis[i]
		line := table(cols, c.Display, styleDim.Render(c.Source), cmp.Or(c.Phone, displayJID(safe(c.JID))), styleDim.Render(strings.Join(c.Tags, " ")))
		lines = append(lines, m.row(line, i == m.contactSel))
	}
	lines = append(lines, "", indent(detail, " "))
	return strings.Join(lines, "\n")
}

// contactDetail is the selected person's card: every name, your tags and, for an admin, your notes.
func (m *model) contactDetail() string {
	c, ok := m.selectedContact()
	if !ok {
		return ""
	}
	w := m.cardWidth(cardDetail)
	v := m.contactView
	var rows [][2]string
	switch {
	case v.JID != c.JID && m.contactViewErr == nil:
		rows = [][2]string{{"Number", cmp.Or(c.Phone, displayJID(safe(c.JID)))}, {"", styleDim.Render("loading…")}}
	case isNotFound(m.contactViewErr):
		rows = [][2]string{{"", styleWarn.Render("This server doesn't have GET /contacts/{number} yet: update wame on it.")}}
	case m.contactViewErr != nil:
		rows = [][2]string{{"", errText(m.contactViewErr)}}
	default:
		var names []string
		for _, n := range [][2]string{{bot.SourceOwn, v.Names.Own}, {bot.SourceBook, v.Book}, {bot.SourceBusiness, v.Business}, {bot.SourcePush, v.Push}} {
			if n[1] != "" {
				names = append(names, n[1]+styleDim.Render(" ("+n[0]+")"))
			}
		}
		rows = [][2]string{
			{"Number", cmp.Or(v.Phone, displayJID(safe(v.JID)))},
			{"Names", cmp.Or(strings.Join(names, styleFaint.Render(" · ")), styleDim.Render("none"))},
			{"Tags", cmp.Or(strings.Join(v.Tags, " "), styleDim.Render("none"))},
		}
		if o := v.Own; o != nil {
			rows = append(rows, [2]string{"Notes", cmp.Or(clipLines(o.Notes, w-14, notesLines), styleDim.Render("none"))})
			if o.Language != "" || o.TimeZone != "" {
				rows = append(rows, [2]string{"Language", strings.Join(nonEmpty(o.Language, o.TimeZone), styleFaint.Render(" · "))})
			}
			rows = append(rows, [2]string{"Updated", stamp(o.UpdatedAt, "2006-01-02 15:04") + styleDim.Render(" by "+o.UpdatedBy)})
		} else if !m.contactsReadOnly() {
			rows = append(rows, [2]string{"Notes", styleDim.Render("nothing of yours stored; e adds a name, tags or notes")})
		}
	}
	return card(cmp.Or(c.Display, "Contact"), w, kv(rows))
}

// clipLines wraps s to w columns and keeps the first n lines.
func clipLines(s string, w, n int) string {
	lines := strings.Split(lipgloss.NewStyle().Width(max(w, 10)).Render(s), "\n")
	if len(lines) > n {
		lines = append(lines[:n-1], fit(lines[n-1], max(w-1, 1))+"…")
	}
	return strings.Join(lines, "\n")
}

func (m *model) viewContactForm() string {
	f := m.contactForm
	labels := []string{"Name", "Tags", "Notes"}
	lines := []string{styleBold.Render("Edit " + f.label), styleDim.Render("Your own fields; WhatsApp and the phone's address book aren't changed."), ""}
	for i := range m.form {
		label, marker := styleDim.Render(pad(labels[i], 6)), "  "
		if i == m.formFocus {
			label, marker = styleAccent.Render(pad(labels[i], 6)), styleAccent.Render("› ")
		}
		lines = append(lines, marker+label+" "+m.form[i].View())
	}
	lines = append(lines, "", styleDim.Render("Empty clears a field. Tags: separated by spaces, like clients vip."))
	if !f.notesOK {
		lines = append(lines, styleDim.Render("Notes aren't shown: the card didn't load, or this token can't read them."))
	}
	return lipgloss.NewStyle().Width(min(m.width-12, 84)).Render(strings.Join(lines, "\n"))
}

func (m *model) viewConfirmDeleteContact() string {
	return styleErr.Render("Delete your fields for "+m.confirmContactLabel+"?") + "\n\n" +
		"Your name, tags, notes, language and time zone for them go.\nThe phone's address book and WhatsApp names stay.\n\n" +
		styleKey.Render("y") + " delete   " + styleKey.Render("any key") + " keep"
}

func (m *model) viewConfirmImport() string {
	r := m.importDry
	return styleBold.Render("Import "+safe(m.importPath)+"?") + "\n\n" +
		fmt.Sprintf("The server checked it: %s added, %s updated, %d unchanged.\n", styleBold.Render(fmt.Sprint(r.Added)), styleBold.Render(fmt.Sprint(r.Updated)), r.Unchanged) +
		styleDim.Render("Columns in the file overwrite those fields; an empty cell clears one.") + "\n\n" +
		styleKey.Render("y") + " import   " + styleKey.Render("any key") + " cancel"
}

func (m *model) viewContactPath() string {
	title, sub := "Import contacts", "A CSV with a number column; the server checks it first and shows what would change."
	if m.mode == modeExportContacts {
		title, sub = "Export contacts", "Your own fields as CSV, in the import's columns. An existing file is never overwritten."
	}
	return styleBold.Render(title) + "\n" + styleDim.Render(sub) + "\n\n" + m.prompt.View()
}

func (m *model) viewContactTagPrompt() string {
	known := make([]string, 0, len(m.contactTags))
	for _, t := range m.contactTags {
		known = append(known, fmt.Sprintf("%s %d", safe(t.Tag), t.Count))
	}
	hint := ""
	if len(known) > 0 {
		hint = styleFaint.Render("  " + strings.Join(known, " · "))
	}
	return " " + styleAccent.Render("tag") + " " + m.prompt.View() + hint
}
