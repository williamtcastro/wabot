package admin

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

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
		q    string
		list []bot.Contact
		err  error
	}
	contactTagsMsg struct {
		seq  int
		tags []store.TagCount
		err  error
	}
	contactViewMsg struct {
		seq int
		jid string
		v   bot.Profile
		err error
	}
	contactRefreshMsg struct {
		seq        int
		jid, label string
		p          bot.Profile
		err        error
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
// notesOK means the notes field is there; notesLocked, that the notes can't be shown on one line as
// stored, so the form leaves them alone.
type contactForm struct {
	jid, label           string
	name, tags, notes    string
	notesOK, notesLocked bool
}

func (m *model) loadContactsTab() tea.Cmd {
	return tea.Batch(m.loadContacts(), m.loadContactTags())
}

func (m *model) loadContacts() tea.Cmd {
	if m.c == nil {
		return nil
	}
	seq, tags, q := m.contactsReq.start(), strings.Fields(m.contactTag), ""
	if m.contactsCapped {
		q = strings.TrimSpace(m.contactQuery)
	}
	return m.call(func(ctx context.Context, c *Client) tea.Msg {
		list, err := c.Contacts(ctx, ContactsQuery{Q: q, Tags: tags, Limit: contactsLimit})
		return contactsMsg{seq, q, list, err}
	})
}

// contactsSearch reports whether the filter has to go to the server: the list holds only the first
// contactsLimit people, and it was loaded for another filter.
func (m *model) contactsSearch() bool {
	return m.contactsCapped && strings.TrimSpace(m.contactQuery) != m.contactsQ
}

func (m *model) loadContactTags() tea.Cmd {
	seq := m.contactTagsReq.start()
	return m.call(func(ctx context.Context, c *Client) tea.Msg {
		tags, err := c.ContactTags(ctx)
		return contactTagsMsg{seq, tags, err}
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
	c.Tags = termui.SafeLines(c.Tags)
	return c
}

// safeProfile cleans everything in a profile that people other than the admin wrote: names, about,
// the business profile, group names, WhatsApp's error text, and the notes (folded onto one line).
func safeProfile(p bot.Profile) bot.Profile {
	p.Phone, p.LID = safe(p.Phone), safe(p.LID)
	n := &p.Names
	n.Display, n.Source, n.Own, n.Book, n.Business, n.Push = safe(n.Display), safe(n.Source), safe(n.Own), safe(n.Book), safe(n.Business), safe(n.Push)
	o := &p.Own
	o.Tags, o.Notes, o.Language, o.TimeZone = termui.SafeLines(o.Tags), safe(o.Notes), safe(o.Language), safe(o.TimeZone)
	o.CreatedBy, o.UpdatedBy = safe(o.CreatedBy), safe(o.UpdatedBy)
	w := &p.WhatsApp
	w.About, w.VerifiedName, w.VerifiedLevel, w.Error = safe(w.About), safe(w.VerifiedName), safe(w.VerifiedLevel), safe(w.Error)
	if w.Picture != nil {
		pic := *w.Picture
		pic.ID, pic.URL = safe(pic.ID), safe(pic.URL)
		w.Picture = &pic
	}
	if w.Business != nil {
		bi := *w.Business
		bi.Address, bi.Email, bi.TimeZone, bi.Categories = safe(bi.Address), safe(bi.Email), safe(bi.TimeZone), termui.SafeLines(bi.Categories)
		bi.Options = make(map[string]string, len(w.Business.Options))
		for k, v := range w.Business.Options {
			bi.Options[safe(k)] = safe(v)
		}
		w.Business = &bi
	}
	if p.Activity != nil {
		a := *p.Activity
		a.GroupsInCommon = make([]store.GroupRef, len(p.Activity.GroupsInCommon))
		for i, g := range p.Activity.GroupsInCommon {
			a.GroupsInCommon[i] = store.GroupRef{JID: safe(g.JID), Name: safe(g.Name)}
		}
		p.Activity = &a
	}
	p.Tokens = termui.SafeLines(p.Tokens)
	return p
}

// fullProfile reports an admin's answer: opted_out comes only with the whole profile.
func fullProfile(p bot.Profile) bool { return p.OptedOut != nil }

func (m *model) applyContactTags(msg contactTagsMsg) {
	if !m.contactTagsReq.done(msg.seq) {
		return
	}
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
	if m.contactsQ = msg.q; msg.q == "" {
		m.contactsCapped = len(msg.list) >= contactsLimit
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
	m.showProfile(msg.jid, msg.v, msg.err)
}

// showProfile puts a profile on the card if that person is still selected, and their names and
// tags on their list row.
func (m *model) showProfile(jid string, p bot.Profile, err error) {
	if c, ok := m.selectedContact(); !ok || c.JID != jid {
		return
	}
	m.contactViewErr, m.contactNotes = err, ""
	if err != nil {
		m.contactView = bot.Profile{JID: jid}
		return
	}
	m.contactNotes = p.Own.Notes
	m.contactView = safeProfile(p)
	m.contactView.JID = jid
	if i := slices.IndexFunc(m.contacts, func(c bot.Contact) bool { return c.JID == jid }); i >= 0 {
		v := m.contactView
		m.contacts[i] = bot.Contact{JID: jid, Phone: v.Phone, Names: v.Names, Tags: v.Own.Tags}
	}
}

// refreshContact asks the server to ask WhatsApp about the person, then shows the answer.
func (m *model) refreshContact(c bot.Contact) tea.Cmd {
	jid, label := c.JID, contactLabel(c)
	seq := m.contactViewReq.start()
	m.setNotice("asking WhatsApp about "+label+" (2-4 lookups)…", false)
	return m.call(func(ctx context.Context, c *Client) tea.Msg {
		p, err := c.RefreshContact(ctx, jid)
		return contactRefreshMsg{seq, jid, label, p, err}
	})
}

const profileRefreshEvery = 5 * time.Minute

func (m *model) applyContactRefresh(msg contactRefreshMsg) tea.Cmd {
	fresh := m.contactViewReq.done(msg.seq)
	next := ""
	if t := msg.p.NextRefreshAt; !t.IsZero() {
		next = "; next refresh at " + stamp(t, "15:04")
	}
	w := msg.p.WhatsApp
	apiErr, _ := errors.AsType[*APIError](msg.err)
	switch {
	case isForbidden(msg.err):
		m.contactDenied = true
		m.setNotice(contactsRefreshScope, true)
		return nil
	case isNotFound(msg.err):
		m.setNotice(contactsUpdate, true)
		return nil
	case apiErr != nil && apiErr.Code == http.StatusTooManyRequests:
		retry := "later"
		if apiErr.RetryAfter > 0 {
			retry = "in " + apiErr.RetryAfter.String()
		}
		m.setNotice("not refreshed: the server's lookup budget is spent; try W again "+retry, true)
		return nil
	case msg.err != nil:
		m.setNotice("not refreshed: "+msg.err.Error(), true)
		return nil
	case msg.p.Refreshed == nil:
		m.setNotice("this server can't ask WhatsApp about contacts yet: update wame on it", true)
	case *msg.p.Refreshed:
		m.setNotice("refreshed "+msg.label+" from WhatsApp"+next, false)
	case w.Error != "" && !msg.p.NextRefreshAt.IsZero() && !w.ErrorAt.Before(msg.p.NextRefreshAt.Add(-profileRefreshEvery-10*time.Second)):
		m.setNotice("WhatsApp answered with an error: "+w.Error+next, true)
	default:
		m.setNotice("not refreshed: one refresh per person every 5 minutes"+next, false)
	}
	if fresh {
		m.showProfile(msg.jid, msg.p, nil)
	}
	return m.loadAudit(false)
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
	// contactsRefreshScope is also what a read-only tab says to W.
	contactsRefreshScope = "this token can't ask WhatsApp about contacts: it needs the admin scope"
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
		if _, ok := termui.ListMove(k, m.contactSel, n, 1); ok || slices.Contains([]string{"/", "t", "e", "x", "I", "E", "W", "enter"}, k) {
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
	if m.contactsReadOnly() && k == "W" {
		m.setNotice(contactsRefreshScope, true)
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
		if m.contactsSearch() {
			return m.loadContacts()
		}
		return m.contactSelected()
	case "t":
		m.startPrompt(modeContactTag, "tags that must all be set, e.g. clients vip; empty shows everyone")
		m.prompt.SetValue(m.contactTag)
		m.prompt.CursorEnd()
		m.suggestTags()
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
	case "W":
		if !ok {
			m.setNotice("no contact marked", true)
			return nil
		}
		return m.refreshContact(c)
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
	case !termui.IsPersonChat(jid):
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
	m.suggestTags()
	return m, cmd
}

// suggestTags completes the tag being typed: each suggestion is the prompt up to its last space
// plus a known tag not typed yet, since the input only completes a whole value.
func (m *model) suggestTags() {
	v := m.prompt.Value()
	head := v[:strings.LastIndex(v, " ")+1]
	typed := strings.Fields(strings.ToLower(head))
	var list []string
	for _, t := range m.contactTags {
		if !slices.Contains(typed, t.Tag) {
			list = append(list, head+t.Tag)
		}
	}
	m.prompt.ShowSuggestions = true
	m.prompt.SetSuggestions(list)
}

func (m *model) startContactForm(c bot.Contact) {
	f := contactForm{jid: c.JID, label: contactLabel(c), name: c.Own, tags: strings.Join(c.Tags, " ")}
	if v := m.contactView; v.JID == c.JID && m.contactViewErr == nil && fullProfile(v) {
		f.name, f.tags, f.notes = v.Names.Own, strings.Join(v.Own.Tags, " "), m.contactNotes
		f.notesOK = true
		if safe(f.notes) != f.notes {
			f.notesOK, f.notesLocked, f.notes = false, true, ""
		}
	}
	f.name = strings.TrimSpace(f.name)
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
	path = termui.ExpandHome(path)
	if fi, err := os.Stat(path); err != nil {
		return nil, err
	} else if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s isn't a regular file", path)
	}
	f, err := os.Open(path)
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
		err = termui.WriteNew(termui.ExpandHome(path), data)
		if errors.Is(err, os.ErrExist) {
			return noticeMsg{"not exported: " + path + " already exists: pick a new name", true}
		}
		if err != nil {
			return noticeMsg{"not exported: " + err.Error(), true}
		}
		return noticeMsg{"exported your contacts to " + path, false}
	})
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
	m.resetContactView()
	return tea.Batch(m.loadContactsTab(), m.loadAudit(false))
}

// resetContactView drops the card, so the next one shown (and the edit form) is loaded fresh.
func (m *model) resetContactView() {
	m.contactView, m.contactViewErr, m.contactNotes = bot.Profile{}, nil, ""
}

// The profile card's width: the list keeps the rest of the screen.
func (m *model) profileWidth() int { return min(max(m.width*2/5, 52), 84) }

func (m *model) viewContacts() string {
	switch {
	case isNotFound(m.contactTagsErr):
		return m.errCard("Contacts", m.contactTagsErr, "your own contact list (GET /contacts/tags)")
	case m.contactsErr != nil:
		return m.errCard("Contacts", m.contactsErr, "the contact list (GET /contacts)")
	}
	vis := m.visibleContacts()
	head := fmt.Sprintf("contacts · %d of %d", len(vis), len(m.contacts))
	if m.contactsCapped {
		head += fmt.Sprintf(" (the first %d; a / filter searches the server on enter)", contactsLimit)
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
	head = "   " + styleDim.Render(fit(head, m.width-4))
	cardW := m.profileWidth()
	listW := m.width - cardW - 1
	cols := []int{min(30, listW/3), 8, 16}
	left := []string{"   " + styleHead.Render(table(cols, "NAME", "SOURCE", "NUMBER", "TAGS"))}
	if len(vis) == 0 {
		msg := "nobody yet: names come from the phone's address book, your edits and imports (I)"
		if m.contactQuery != "" || m.contactTag != "" {
			msg = "nothing matches"
		}
		return strings.Join([]string{head, left[0], "   " + styleDim.Render(msg)}, "\n")
	}
	h := m.bodyHeight() - 1
	start, end := window(m.contactSel, len(vis), h-len(left))
	for i := start; i < end; i++ {
		c := vis[i]
		line := table(cols, c.Display, styleDim.Render(c.Source), cmp.Or(c.Phone, displayJID(safe(c.JID))), styleDim.Render(strings.Join(c.Tags, " ")))
		left = append(left, rowIn(line, i == m.contactSel, listW))
	}
	list := lipgloss.NewStyle().Width(listW).Render(strings.Join(left, "\n"))
	return head + "\n" + lipgloss.JoinHorizontal(lipgloss.Top, list, " ", m.contactDetail(cardW, h))
}

// contactDetail is the selected person's profile card, at most h lines tall.
func (m *model) contactDetail(w, h int) string {
	c, ok := m.selectedContact()
	if !ok {
		return ""
	}
	v := m.contactView
	var rows [][2]string
	switch {
	case v.JID != c.JID:
		rows = [][2]string{{"Number", cmp.Or(c.Phone, displayJID(safe(c.JID)))}, {"", styleDim.Render("loading…")}}
	case isNotFound(m.contactViewErr):
		rows = [][2]string{{"", styleWarn.Render("This server doesn't have GET /contacts/{number} yet: update wame on it.")}}
	case m.contactViewErr != nil:
		rows = [][2]string{{"", errText(m.contactViewErr)}}
	default:
		rows = m.profileRows(v)
	}
	body := strings.Split(kvWrap(rows, w-4), "\n")
	if len(body) > h-3 {
		body = append(body[:max(h-4, 1)], styleDim.Render("…"))
	}
	return card(cmp.Or(c.Display, "Contact"), w, strings.Join(body, "\n"))
}

// profileRows lays out a profile already through safeProfile.
func (m *model) profileRows(v bot.Profile) [][2]string {
	dot := styleFaint.Render(" · ")
	none := styleDim.Render("none")
	var names []string
	for _, n := range [][2]string{{bot.SourceOwn, v.Names.Own}, {bot.SourceBook, v.Names.Book}, {bot.SourceBusiness, v.Names.Business}, {bot.SourcePush, v.Names.Push}} {
		if n[1] != "" {
			names = append(names, n[1]+styleDim.Render(" ("+n[0]+")"))
		}
	}
	rows := [][2]string{
		{"Number", cmp.Or(v.Phone, displayJID(safe(v.JID)))},
		{"LID", cmp.Or(v.LID, styleDim.Render("not known yet"))},
		{"Names", cmp.Or(strings.Join(names, dot), none)},
		{"Tags", cmp.Or(strings.Join(v.Own.Tags, " "), none)},
	}
	full := fullProfile(v)
	o := v.Own
	if full || o.Notes != "" {
		rows = append(rows, [2]string{"Notes", cmp.Or(o.Notes, none)})
	}
	if o.Language != "" || o.TimeZone != "" {
		rows = append(rows, [2]string{"Language", strings.Join(nonEmpty(o.Language, o.TimeZone), dot)})
	}
	if !o.UpdatedAt.IsZero() {
		rows = append(rows, [2]string{"Updated", stamp(o.UpdatedAt, "2006-01-02 15:04") + styleDim.Render(" by "+o.UpdatedBy)})
	} else if full && len(o.Tags) == 0 && o.Notes == "" && v.Names.Own == "" && !m.contactsReadOnly() {
		rows = append(rows, [2]string{"", styleDim.Render("nothing of yours stored; e adds a name, tags or notes")})
	}

	wa := v.WhatsApp
	on := styleDim.Render("unknown")
	if wa.OnWhatsApp != nil {
		on = map[bool]string{true: "yes", false: styleWarn.Render("no")}[*wa.OnWhatsApp]
	}
	if full {
		switch {
		case wa.AsOf.IsZero():
			on += dot + styleDim.Render("never asked; W asks (2-4 lookups)")
		case wa.Stale:
			on += dot + styleWarn.Render("stale") + styleDim.Render(", as of "+stamp(wa.AsOf, "2006-01-02 15:04"))
		default:
			on += dot + styleDim.Render("as of "+stamp(wa.AsOf, "2006-01-02 15:04"))
		}
	}
	rows = append(rows, [2]string{"WhatsApp", on})
	if wa.About != "" {
		rows = append(rows, [2]string{"About", wa.About})
	}
	if wa.VerifiedName != "" {
		rows = append(rows, [2]string{"Verified", strings.Join(nonEmpty(wa.VerifiedName, wa.VerifiedLevel), dot)})
	}
	if p := wa.Picture; p != nil {
		photo := none
		switch {
		case p.Hidden:
			photo = styleDim.Render("hidden from this account")
		case p.ID != "" && p.URL != "":
			photo = "id " + p.ID + dot + styleDim.Render("a link is in the API answer")
		case p.ID != "":
			photo = "id " + p.ID
		}
		rows = append(rows, [2]string{"Photo", photo})
	}
	if b := wa.Business; b != nil {
		var parts []string
		parts = append(parts, nonEmpty(b.Address, b.Email, strings.Join(b.Categories, ", "), b.TimeZone)...)
		for _, k := range slices.Sorted(maps.Keys(b.Options)) {
			parts = append(parts, k+": "+b.Options[k])
		}
		rows = append(rows, [2]string{"Business", cmp.Or(strings.Join(parts, " · "), "a business account")})
	}
	if wa.Error != "" {
		e := styleErr.Render(wa.Error)
		if !wa.ErrorAt.IsZero() {
			e += styleDim.Render(" (" + stamp(wa.ErrorAt, "2006-01-02 15:04") + ")")
		}
		rows = append(rows, [2]string{"Last error", e})
	}

	if a := v.Activity; a != nil {
		msgs := fmt.Sprintf("%d sent%s%d received%s%d unread", a.Sent, dot, a.Received, dot, a.Unread)
		if !a.FirstAt.IsZero() {
			msgs += dot + styleDim.Render("first "+stamp(a.FirstAt, "2006-01-02")+", last "+stamp(a.LastAt, "2006-01-02 15:04"))
		}
		rows = append(rows, [2]string{"Messages", msgs})
		groups := make([]string, len(a.GroupsInCommon))
		for i, g := range a.GroupsInCommon {
			groups[i] = cmp.Or(g.Name, displayJID(g.JID))
		}
		rows = append(rows, [2]string{"Groups", cmp.Or(strings.Join(groups, " · "), none)})
	}
	if q := v.Queue; q != nil {
		text := none
		if q.Total > 0 {
			text = fmt.Sprintf("%d: %d open, %d sent, %d delivered, %d read, %d failed", q.Total, q.Open, q.Sent, q.Delivered, q.Read, q.Failed)
			if !q.LastAt.IsZero() {
				text += styleDim.Render(", last " + stamp(q.LastAt, "2006-01-02 15:04"))
			}
		}
		rows = append(rows, [2]string{"Queue " + safe(cmp.Or(q.Window, "7d")), text})
	}
	if v.OptedOut != nil {
		rows = append(rows, [2]string{"Opted out", map[bool]string{true: styleWarn.Render("yes: queued messages skip them"), false: "no"}[*v.OptedOut]})
	}
	if full {
		rows = append(rows, [2]string{"Tokens", cmp.Or(strings.Join(v.Tokens, ", "), styleDim.Render("none limited to this chat"))})
	} else {
		rows = append(rows, [2]string{"", styleDim.Render("notes, activity and the WhatsApp profile need the admin scope")})
	}
	return rows
}

// kvWrap is kv with each value wrapped to fit w columns, continuation lines under the value.
func kvWrap(rows [][2]string, w int) string {
	lw := 0
	for _, r := range rows {
		lw = max(lw, ansi.StringWidth(r[0]))
	}
	vw := max(w-lw-2, 10)
	var lines []string
	for _, r := range rows {
		for i, l := range strings.Split(clipLines(r[1], vw, notesLines), "\n") {
			label := pad("", lw)
			if i == 0 {
				label = styleDim.Render(pad(r[0], lw))
			}
			lines = append(lines, label+"  "+strings.TrimRight(l, " "))
		}
	}
	return strings.Join(lines, "\n")
}

// clipLines wraps s to w columns and keeps the first n lines.
func clipLines(s string, w, n int) string {
	if s == "" {
		return ""
	}
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
	switch {
	case f.notesLocked:
		lines = append(lines, styleDim.Render("Notes aren't shown: they have line breaks or characters the form can't keep. Edit them through the API or a CSV export and import."))
	case !f.notesOK:
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
