package bot

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.mau.fi/whatsmeow/types"

	"github.com/williamtcastro/wame/internal/store"
)

// Size limits for your own contact fields and their writes.
const (
	MaxContactName        = 100 // runes
	MaxContactNotes       = 2000
	MaxContactLanguage    = 16
	MaxContactTimeZone    = 64
	MaxContactTag         = 32
	MaxContactBody        = 64 << 10 // PUT and PATCH bodies
	MaxContactImport      = 5000     // rows per CSV import
	MaxContactImportBytes = 4 << 20
)

// ContactImportColumns are the columns a contacts CSV may have; "to" and "phone" are accepted for
// "number". GET /contacts/export writes them in this order.
var ContactImportColumns = append([]string{"number"}, store.ContactFields...)

var (
	tagPattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	languagePattern = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{1,8})*$`)
)

// Contact is one person in the list: the unified names and your tags.
type Contact struct {
	JID   string `json:"jid"`
	Phone string `json:"phone"`
	Names
	Tags []string `json:"tags"`
}

// ContactView is one person with your own fields (nil when you wrote nothing about them). Own holds
// the notes, so only admins may see it whole.
type ContactView struct {
	Contact
	Own *store.OwnContact `json:"own,omitempty"`
}

// ContactQuery filters Contacts. Q matches names, tags and the number; every tag in Tags must be
// set; Known drops people known only by push name or number; a non-empty Allow (a restricted
// token's allowlist) keeps only those chats; Limit > 0 caps the answer.
type ContactQuery struct {
	Q     string
	Tags  []string
	Known bool
	Allow []string
	Limit int
}

// ContactInput is the body of PUT and PATCH /contacts/{number}. In a PATCH an absent field is left
// alone and null or "" clears it; in a PUT an absent field is cleared too. AddTags and RemoveTags
// edit the tags after Tags is applied (PATCH only).
type ContactInput struct {
	Name       *string   `json:"name,omitempty"`
	Tags       *[]string `json:"tags,omitempty"`
	Notes      *string   `json:"notes,omitempty"`
	Language   *string   `json:"language,omitempty"`
	TimeZone   *string   `json:"time_zone,omitempty"`
	AddTags    []string  `json:"add_tags,omitempty"`
	RemoveTags []string  `json:"remove_tags,omitempty"`
}

// UnmarshalJSON tells a null field (clear it) from an absent one (leave it) and refuses unknown fields
// and a body that isn't an object (a null body would otherwise clear everything in a PUT).
func (in *ContactInput) UnmarshalJSON(data []byte) error {
	if d := bytes.TrimSpace(data); len(d) == 0 || d[0] != '{' {
		return BadInput("body must be a JSON object")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*in = ContactInput{}
	str := func(v json.RawMessage) (*string, error) {
		s := ""
		if bytes.Equal(v, []byte("null")) {
			return &s, nil
		}
		return &s, json.Unmarshal(v, &s)
	}
	list := func(v json.RawMessage) ([]string, error) {
		l := []string{}
		if bytes.Equal(v, []byte("null")) {
			return l, nil
		}
		return l, json.Unmarshal(v, &l)
	}
	for k, v := range raw {
		var err error
		switch k {
		case "name":
			in.Name, err = str(v)
		case "notes":
			in.Notes, err = str(v)
		case "language":
			in.Language, err = str(v)
		case "time_zone":
			in.TimeZone, err = str(v)
		case "tags":
			var l []string
			l, err = list(v)
			in.Tags = &l
		case "add_tags":
			in.AddTags, err = list(v)
		case "remove_tags":
			in.RemoveTags, err = list(v)
		default:
			return fmt.Errorf("unknown field %.40q (use name, tags, notes, language, time_zone, add_tags, remove_tags)", k)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}
	return nil
}

// edit turns the input into a store edit, cleaning and checking every value. With all, absent
// fields are cleared (PUT).
func (in ContactInput) edit(all bool) (store.ContactEdit, error) {
	var e store.ContactEdit
	var err error
	field := func(name string, v *string, check func(string) (string, error), dst *string) {
		if err != nil || (v == nil && !all) {
			return
		}
		e.Fields = append(e.Fields, name)
		if v != nil {
			*dst, err = check(*v)
		}
	}
	field(store.FieldName, in.Name, checkContactName, &e.Set.Name)
	field(store.FieldNotes, in.Notes, checkContactNotes, &e.Set.Notes)
	field(store.FieldLanguage, in.Language, checkContactLanguage, &e.Set.Language)
	field(store.FieldTimeZone, in.TimeZone, checkContactTimeZone, &e.Set.TimeZone)
	if err != nil {
		return e, err
	}
	if in.Tags != nil || all {
		e.Fields = append(e.Fields, store.FieldTags)
		if in.Tags != nil {
			if e.Set.Tags, err = checkTags(*in.Tags); err != nil {
				return e, err
			}
		}
	}
	if all && (len(in.AddTags) > 0 || len(in.RemoveTags) > 0) {
		return e, BadInput("add_tags and remove_tags only work in a PATCH")
	}
	if e.AddTags, err = checkTags(in.AddTags); err != nil {
		return e, err
	}
	if e.RemoveTags, err = checkTags(in.RemoveTags); err != nil {
		return e, err
	}
	return e, nil
}

func checkContactName(s string) (string, error) {
	s = cleanName(s)
	if n := utf8.RuneCountInString(s); n > MaxContactName {
		return "", BadInput("name has %d characters; at most %d", n, MaxContactName)
	}
	return s, nil
}

func checkContactNotes(s string) (string, error) {
	s = cleanNotes(s)
	if n := utf8.RuneCountInString(s); n > MaxContactNotes {
		return "", BadInput("notes have %d characters; at most %d", n, MaxContactNotes)
	}
	return s, nil
}

func checkContactLanguage(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s != "" && (len(s) > MaxContactLanguage || !languagePattern.MatchString(s)) {
		return "", BadInput("language %.20q isn't a language tag like pt-BR", s)
	}
	return s, nil
}

func checkContactTimeZone(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if len(s) > MaxContactTimeZone {
		return "", BadInput("time zone is longer than %d characters", MaxContactTimeZone)
	}
	loc, err := ParseTimezone(s)
	if err == nil && loc == time.Local {
		err = errors.New("use an IANA name like America/Sao_Paulo, not the server's zone")
	}
	if err != nil {
		return "", BadInput("%v", err)
	}
	return loc.String(), nil
}

// checkTags lowercases tags and checks each one and how many there are.
func checkTags(tags []string) ([]string, error) {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if !tagPattern.MatchString(t) {
			return nil, BadInput("tag %.40q must be 1 to %d of a-z, 0-9, _ and -, starting with a letter or digit", t, MaxContactTag)
		}
		out = append(out, t)
	}
	if out = store.NormalizeTags(out); len(out) > store.MaxContactTags {
		return nil, BadInput("%d tags; at most %d per person", len(out), store.MaxContactTags)
	}
	return out, nil
}

// splitTags reads a CSV tags cell: tags separated by spaces, commas, ';' or '|'.
func splitTags(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' || r == ';' || r == '|' || r == '\t' })
}

// personJID parses a number or JID into the phone JID a person's row is kept under, without
// asking WhatsApp. Groups are refused.
func (b *Bot) personJID(to string) (types.JID, error) {
	jid, err := b.localJID(to)
	if err == nil && !isPersonChat(jid) {
		err = BadInput("contacts are people, not groups")
	}
	return jid, err
}

// contactKeys lists every key a person's own row may be under: the phone JID, its @lid and its
// 9th-digit variant. The first is where a new row goes.
func (b *Bot) contactKeys(jid types.JID) []string {
	keys := b.optOutKeys(jid)
	return append(keys, phoneVariants(keys[0])...)
}

func phoneOf(jid types.JID) string {
	if jid.Server == types.DefaultUserServer {
		return "+" + jid.User
	}
	return ""
}

func tagsOf(o store.OwnContact) []string {
	if o.Tags == nil {
		return []string{}
	}
	return o.Tags
}

func contactAudit(changed []string) string {
	if len(changed) == 0 {
		return "no change"
	}
	return strings.Join(changed, ", ")
}

// Contacts lists the people in the phone's address book and in your own list, merged (a person in
// either), with the unified name. It never asks WhatsApp anything (a restricted allowlist is expanded
// like ExpandAllowlist does for every other route) and works before login with your own rows only.
func (b *Bot) Contacts(ctx context.Context, q ContactQuery) ([]Contact, error) {
	tags, err := checkTags(q.Tags)
	if err != nil {
		return nil, err
	}
	own, err := b.Store.Contacts(store.ContactFilter{Tags: tags})
	if err != nil {
		return nil, err
	}
	var book map[types.JID]types.ContactInfo
	if cs := b.Client.Store.Contacts; cs != nil {
		if book, err = cs.GetAllContacts(ctx); err != nil {
			return nil, err
		}
	}
	ownBy := make(map[string]store.OwnContact, len(own))
	for _, c := range own {
		ownBy[c.JID] = c
	}
	out := []Contact{}
	listed := map[string]bool{}
	add := func(jid types.JID, info types.ContactInfo, o store.OwnContact) {
		listed[jid.String()] = o.JID != ""
		out = append(out, Contact{JID: jid.String(), Phone: phoneOf(jid), Names: namesFrom(jid, o.Name, info, ""), Tags: tagsOf(o)})
	}
	// A row under the address book's own JID goes with it first; only then may a row under the
	// 9th-digit variant join an entry that has none.
	exact := map[types.JID]store.OwnContact{}
	for jid := range book {
		if o, ok := ownBy[jid.String()]; ok && jid.Server == types.DefaultUserServer {
			exact[jid] = o
			delete(ownBy, jid.String())
		}
	}
	for jid, info := range book {
		if jid.Server != types.DefaultUserServer {
			continue
		}
		o, ok := exact[jid]
		for _, k := range phoneVariants(jid.String()) {
			if c, found := ownBy[k]; found && !ok {
				o, ok = c, true
				delete(ownBy, k)
			}
		}
		if o.JID == "" && (len(tags) > 0 || cmp.Or(info.FullName, info.FirstName, info.BusinessName, info.PushName) == "") {
			continue
		}
		add(jid, info, o)
	}
	for _, o := range own {
		if _, left := ownBy[o.JID]; !left {
			continue
		}
		jid, err := types.ParseJID(o.JID)
		if err != nil {
			continue
		}
		add(jid, types.ContactInfo{}, o)
	}
	var allowed []string
	if len(q.Allow) > 0 {
		allowed = b.ExpandAllowlist(ctx, q.Allow)
	}
	needle := strings.ToLower(strings.TrimSpace(q.Q))
	num := digits(needle)
	out = slices.DeleteFunc(out, func(c Contact) bool {
		switch {
		case q.Known && !listed[c.JID] && (c.Source == SourcePush || c.Source == SourceNumber):
			return true
		case len(q.Allow) > 0 && !slices.Contains(allowed, c.JID):
			return true
		case needle == "":
			return false
		}
		hay := strings.ToLower(strings.Join([]string{c.Display, c.Own, c.Book, c.Business, c.Push, strings.Join(c.Tags, " ")}, "\n"))
		return !strings.Contains(hay, needle) && (num == "" || !strings.Contains(c.Phone, num))
	})
	slices.SortFunc(out, func(x, y Contact) int {
		return cmp.Or(cmp.Compare(strings.ToLower(x.Display), strings.ToLower(y.Display)), cmp.Compare(x.JID, y.JID))
	})
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// ContactTags counts the people per tag in your own list.
func (b *Bot) ContactTags() ([]store.TagCount, error) { return b.Store.ContactTags() }

// ContactOf is one person by number or JID, without asking WhatsApp. A person wame knows nothing
// about is still an answer, with only the number.
func (b *Bot) ContactOf(ctx context.Context, to string) (ContactView, error) {
	jid, err := b.personJID(to)
	if err != nil {
		return ContactView{}, err
	}
	o, err := b.Store.Contact(b.contactKeys(jid)...)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return ContactView{}, err
	}
	return b.contactView(ctx, jid, o), nil
}

func (b *Bot) contactView(ctx context.Context, jid types.JID, o store.OwnContact) ContactView {
	v := ContactView{Contact: Contact{JID: jid.String(), Phone: phoneOf(jid), Tags: tagsOf(o)}}
	v.Names = namesFrom(jid, o.Name, b.bookEntry(ctx, jid), "")
	if self := b.Client.Store.ID; self != nil && self.User == jid.User {
		v.Display, v.Source = "Me", SourceMe
	}
	if !o.Empty() {
		v.Own = &o
	}
	return v
}

// SetContact replaces your own fields for a person (PUT): absent fields are cleared, and all empty
// deletes the row. While logged in, a number is resolved once on WhatsApp so the row lands under
// the JID messages use (like an opt-out).
func (b *Bot) SetContact(ctx context.Context, actor, to string, in ContactInput) (ContactView, error) {
	return b.writeContact(ctx, actor, to, in, true)
}

// PatchContact changes only the fields given (PATCH); it never asks WhatsApp.
func (b *Bot) PatchContact(ctx context.Context, actor, to string, in ContactInput) (ContactView, error) {
	return b.writeContact(ctx, actor, to, in, false)
}

func (b *Bot) writeContact(ctx context.Context, actor, to string, in ContactInput, put bool) (v ContactView, err error) {
	target, detail := to, ""
	defer func() { b.Audit(actor, "contact_set", target, detail, err) }()
	jid, err := b.personJID(to)
	if err != nil {
		return v, err
	}
	e, err := in.edit(put)
	if err != nil {
		return v, err
	}
	keys := b.contactKeys(jid)
	if put && b.Client.IsLoggedIn() && !strings.Contains(to, "@") {
		// A number WhatsApp doesn't know is kept as typed, as PATCH and import do.
		canon, cerr := b.CanonicalChat(ctx, to)
		if cerr == nil && canon != jid.String() {
			cj, _ := types.ParseJID(canon)
			jid, keys = cj, append(b.contactKeys(cj), keys...)
		}
	}
	target = jid.String()
	e.Keys, e.Actor = keys, actor
	b.contactsMu.Lock()
	gen := b.Store.ContactsGen()
	ch, err := b.Store.SetContact(e)
	b.syncOwn(gen, err == nil, func() { b.applyOwnLocked([]store.ContactChange{ch}) })
	b.contactsMu.Unlock()
	if err != nil {
		return v, storeContactError(err)
	}
	detail = contactAudit(ch.Changed)
	if len(ch.Changed) > 0 {
		b.namesChanged()
	}
	return b.contactView(ctx, jid, ch.After), nil
}

// storeContactError turns the store's limits into bad input.
func storeContactError(err error) error {
	switch {
	case errors.Is(err, store.ErrTooManyTags):
		return BadInput("at most %d tags per person", store.MaxContactTags)
	case errors.Is(err, store.ErrContactLimit):
		return BadInput("at most %d contacts", store.MaxContacts)
	}
	return err
}

// DeleteContact removes your own fields for a person under every key they may be stored under, or
// returns store.ErrNotFound when there were none.
func (b *Bot) DeleteContact(ctx context.Context, actor, to string) (err error) {
	target := to
	defer func() { b.Audit(actor, "contact_delete", target, "", err) }()
	jid, err := b.personJID(to)
	if err != nil {
		return err
	}
	target = jid.String()
	keys := b.contactKeys(jid)
	b.contactsMu.Lock()
	gen := b.Store.ContactsGen()
	err = b.Store.DeleteContact(keys...)
	b.syncOwn(gen, err == nil, func() {
		for _, k := range keys {
			b.dropOwnLocked(k)
		}
	})
	b.contactsMu.Unlock()
	if err == nil {
		b.namesChanged()
	}
	return err
}

// DeleteAllContacts empties your own list and returns how many people were in it. WhatsApp's
// address book isn't touched.
func (b *Bot) DeleteAllContacts(ctx context.Context, actor string) (n int64, err error) {
	defer func() { b.Audit(actor, "contact_delete_all", "", fmt.Sprintf("%d contacts", n), err) }()
	b.contactsMu.Lock()
	gen := b.Store.ContactsGen()
	n, err = b.Store.DeleteAllContacts()
	b.syncOwn(gen, err == nil, func() { b.own, b.ownLIDs = map[string]ownContact{}, 0 })
	b.contactsMu.Unlock()
	if err == nil && n > 0 {
		b.namesChanged()
	}
	return n, err
}

// ExportContacts returns your own list for GET /contacts/export, audited as contact_export (notes
// leave the server with it).
func (b *Bot) ExportContacts(ctx context.Context, actor string) (list []store.OwnContact, err error) {
	defer func() { b.Audit(actor, "contact_export", "csv", fmt.Sprintf("%d contacts", len(list)), err) }()
	return b.Store.Contacts(store.ContactFilter{})
}

// ContactImportOptions are an import's request options: tags added to every row, and a dry run
// that only counts.
type ContactImportOptions struct {
	AddTags []string
	DryRun  bool
}

// ImportContacts reads a contacts CSV (ContactImportColumns, see readCSVTable) and stores it in one
// transaction: any bad line stores nothing. A column present overwrites that field (an empty cell
// clears it); absent columns are left alone. Numbers are never looked up: a row updates the person
// stored under the number or its 9th-digit variant, otherwise it's stored as typed.
func (b *Bot) ImportContacts(ctx context.Context, actor string, r io.Reader, opt ContactImportOptions) (res store.ImportResult, err error) {
	rows := 0
	defer func() {
		detail := fmt.Sprintf("%d rows: %d added, %d updated", rows, res.Added, res.Updated)
		if opt.DryRun {
			detail += " (dry run)"
		}
		b.Audit(actor, "contact_import", "csv", detail, err)
	}()
	addTags, err := checkTags(opt.AddTags)
	if err != nil {
		return res, err
	}
	table, err := readCSVTable(r, csvTable{
		columns:  ContactImportColumns,
		aliases:  map[string]string{"to": "number", "phone": "number"},
		required: "number",
		limit:    MaxContactImport,
		noun:     "contacts",
	})
	if err != nil {
		return res, err
	}
	rows = len(table)
	edits := make([]store.ContactEdit, len(table))
	for i, row := range table {
		if err := numberCell(row.line, row.cells["number"]); err != nil {
			return res, err
		}
		if edits[i], err = b.importEdit(row, addTags, actor); err != nil {
			return res, BadInput("line %d: %v", row.line, err)
		}
	}
	b.contactsMu.Lock()
	gen := b.Store.ContactsGen()
	res, changes, err := b.Store.ImportContacts(edits, opt.DryRun)
	b.syncOwn(gen, err == nil && !opt.DryRun, func() { b.applyOwnLocked(changes) })
	b.contactsMu.Unlock()
	if ee, ok := errors.AsType[*store.EditError](err); ok && ee.Index < len(table) {
		return res, BadInput("line %d: %v", table[ee.Index].line, storeContactError(ee.Err))
	}
	if err != nil {
		return res, storeContactError(err)
	}
	if !opt.DryRun && res.Added+res.Updated > 0 {
		b.namesChanged()
	}
	return res, nil
}

func (b *Bot) importEdit(row csvRow, addTags []string, actor string) (store.ContactEdit, error) {
	jid, err := b.personJID(row.cells["number"])
	if err != nil {
		return store.ContactEdit{}, err
	}
	var in ContactInput
	for _, f := range store.ContactFields {
		v, ok := row.cells[f]
		if !ok {
			continue
		}
		switch f {
		case store.FieldName:
			in.Name = &v
		case store.FieldNotes:
			in.Notes = &v
		case store.FieldLanguage:
			in.Language = &v
		case store.FieldTimeZone:
			in.TimeZone = &v
		case store.FieldTags:
			t := splitTags(v)
			in.Tags = &t
		}
	}
	in.AddTags = addTags
	e, err := in.edit(false)
	e.Keys, e.Actor = b.contactKeys(jid), actor
	return e, err
}
