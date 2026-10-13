package api

import (
	"encoding/csv"
	"net/http"
	"strconv"
	"strings"

	"go.mau.fi/whatsmeow/types"

	"github.com/williamtcastro/wame/internal/auth"
	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
)

const (
	defaultContacts = 1000
	maxContacts     = 5000
)

// boolParam reads a 1/true/0/false query parameter; anything else is a 400.
func boolParam(w http.ResponseWriter, r *http.Request, name string) (v, ok bool) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return false, true
	}
	v, err := strconv.ParseBool(s)
	if err != nil {
		writeError(w, bot.BadInput("?%s must be 1, true, 0 or false", name))
		return false, false
	}
	return v, true
}

// tagParams reads a repeated tag parameter; each value may also hold several tags separated by commas.
func tagParams(r *http.Request, name string) []string {
	var tags []string
	for _, v := range r.URL.Query()[name] {
		tags = append(tags, strings.FieldsFunc(v, func(c rune) bool { return c == ',' || c == ' ' })...)
	}
	return tags
}

// contacts lists people from the address book and your own list. A token limited to chats gets only
// those chats (the allowlist's own entries are the only thing ever looked up).
func (a *server) contacts(w http.ResponseWriter, r *http.Request) {
	known, ok := boolParam(w, r, "known")
	if !ok {
		return
	}
	q := bot.ContactQuery{
		Q:     r.URL.Query().Get("q"),
		Tags:  tagParams(r, "tag"),
		Known: known,
		Allow: principal(r).Chats,
		Limit: intParam(r, "limit", defaultContacts, maxContacts),
	}
	contacts, err := a.b.Contacts(r.Context(), q)
	respond(w, contacts, err)
}

func (a *server) contactTags(w http.ResponseWriter, r *http.Request) {
	tags, err := a.b.ContactTags()
	respond(w, tags, err)
}

// visibleContact drops your own fields (notes among them) for callers without admin.
func visibleContact(p auth.Principal, v bot.ContactView) bot.ContactView {
	if !p.Has(auth.Admin) {
		v.Own = nil
	}
	return v
}

// contact is one person, never looked up on WhatsApp. A restricted token gets the fixed 403 for
// anyone outside its allowlist.
func (a *server) contact(w http.ResponseWriter, r *http.Request) {
	to, ok := a.authorizeChat(w, r, r.PathValue("number"))
	if !ok {
		return
	}
	v, err := a.b.ContactOf(r.Context(), to)
	respond(w, visibleContact(principal(r), v), err)
}

// putContact replaces your own fields (PUT) or changes only the ones given (PATCH).
func (a *server) putContact(w http.ResponseWriter, r *http.Request) {
	var in bot.ContactInput
	if !readJSONMax(w, r, &in, bot.MaxContactBody) {
		return
	}
	write := a.b.SetContact
	if r.Method == http.MethodPatch {
		write = a.b.PatchContact
	}
	v, err := write(r.Context(), principal(r).Name, r.PathValue("number"), in)
	respond(w, v, err)
}

func (a *server) deleteContact(w http.ResponseWriter, r *http.Request) {
	err := a.b.DeleteContact(r.Context(), principal(r).Name, r.PathValue("number"))
	respond(w, map[string]bool{"deleted": true}, err)
}

// deleteAllContacts empties your own list; ?all=true guards against a stray DELETE /contacts.
func (a *server) deleteAllContacts(w http.ResponseWriter, r *http.Request) {
	all, ok := boolParam(w, r, "all")
	if !ok {
		return
	}
	if !all {
		writeError(w, bot.BadInput("add ?all=true to delete every contact in your own list"))
		return
	}
	n, err := a.b.DeleteAllContacts(r.Context(), principal(r).Name)
	respond(w, map[string]int64{"deleted": n}, err)
}

// importContacts reads a contacts CSV (bot.ContactImportColumns) of at most
// bot.MaxContactImportBytes, all rows or none.
func (a *server) importContacts(w http.ResponseWriter, r *http.Request) {
	dry, ok := boolParam(w, r, "dry_run")
	if !ok {
		return
	}
	opt := bot.ContactImportOptions{AddTags: tagParams(r, "add_tags"), DryRun: dry}
	res, err := a.b.ImportContacts(r.Context(), principal(r).Name, http.MaxBytesReader(w, r.Body, bot.MaxContactImportBytes), opt)
	if tooLarge(w, err) {
		return
	}
	respond(w, res, err)
}

// exportContacts writes your own list as a CSV that POST /contacts/import reads back unchanged.
func (a *server) exportContacts(w http.ResponseWriter, r *http.Request) {
	list, err := a.b.Store.Contacts(store.ContactFilter{})
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="wame-contacts.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write(bot.ContactImportColumns)
	for _, c := range list {
		number := c.JID
		if jid, err := types.ParseJID(c.JID); err == nil && jid.Server == types.DefaultUserServer {
			number = jid.User
		}
		_ = cw.Write([]string{number, c.Name, strings.Join(c.Tags, " "), c.Notes, c.Language, c.TimeZone})
	}
	cw.Flush()
}
