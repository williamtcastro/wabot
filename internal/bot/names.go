package bot

import (
	"cmp"
	"context"
	"strings"
	"time"
	"unicode"

	"go.mau.fi/whatsmeow/types"

	"github.com/williamtcastro/wame/internal/store"
)

// Names is every name wame knows for a chat and the one views show.
type Names struct {
	Display  string `json:"name"`
	Source   string `json:"name_source"`
	Own      string `json:"own_name,omitempty"`
	Book     string `json:"book_name,omitempty"`
	Business string `json:"business_name,omitempty"`
	Push     string `json:"push_name,omitempty"`
}

// Where a display name came from.
const (
	SourceOwn      = "own"
	SourceBook     = "book"
	SourceBusiness = "business"
	SourcePush     = "push"
	SourceNumber   = "number"
	SourceGroup    = "group"
	SourceMe       = "me"
)

// NamesOf names a chat: group subject, "Me", your own name, the phone's address book (full, then
// first name), the business name, ~push name, then +number. It never asks WhatsApp: own names come
// from memory, the rest from whatsmeow's store (a group's subject may be refreshed in the background).
func (b *Bot) NamesOf(ctx context.Context, chat, pushName string) Names {
	jid, err := types.ParseJID(chat)
	if err != nil {
		return Names{Display: chat, Source: SourceNumber}
	}
	if jid.Server == types.GroupServer {
		return Names{Display: cmp.Or(b.group(ctx, chat).name, "group "+jid.User), Source: SourceGroup}
	}
	if self := b.Client.Store.ID; self != nil && self.User == jid.User {
		return Names{Display: "Me", Source: SourceMe}
	}
	pn := b.PhoneJID(jid)
	own, _ := b.ownEntry(append([]string{chat}, b.nameKeys(pn)...)...)
	return namesFrom(pn, own.name, b.bookEntry(ctx, pn), pushName)
}

// ChatName is NamesOf's display name.
func (b *Bot) ChatName(ctx context.Context, chat, pushName string) string {
	return b.NamesOf(ctx, chat, pushName).Display
}

// namesFrom picks the display name of a person from what is known about them.
func namesFrom(jid types.JID, own string, book types.ContactInfo, pushName string) Names {
	n := Names{
		Own:      own,
		Book:     cmp.Or(book.FullName, book.FirstName),
		Business: book.BusinessName,
		Push:     cmp.Or(pushName, book.PushName),
	}
	switch {
	case n.Own != "":
		n.Display, n.Source = n.Own, SourceOwn
	case n.Book != "":
		n.Display, n.Source = n.Book, SourceBook
	case n.Business != "":
		n.Display, n.Source = n.Business, SourceBusiness
	case n.Push != "":
		n.Display, n.Source = "~"+n.Push, SourcePush
	case jid.Server == types.DefaultUserServer:
		n.Display, n.Source = "+"+jid.User, SourceNumber
	default:
		n.Display, n.Source = jid.String(), SourceNumber
	}
	return n
}

// bookEntry is what whatsmeow knows about a person (address book, push and business names), under
// the JID or its 9th-digit variant. Before login there is nothing.
func (b *Bot) bookEntry(ctx context.Context, jid types.JID) types.ContactInfo {
	cs := b.Client.Store.Contacts
	if cs == nil {
		return types.ContactInfo{}
	}
	if c, err := cs.GetContact(ctx, jid); err == nil && c.Found {
		return c
	}
	for _, v := range phoneVariants(jid.String()) {
		vj, _ := types.ParseJID(v)
		if c, err := cs.GetContact(ctx, vj); err == nil && c.Found {
			return c
		}
	}
	return types.ContactInfo{}
}

// phoneVariants lists the other form of a Brazilian mobile number: 55 + area + 9 + 8 digits also
// as 55 + area + 8 digits, and the reverse for a number starting with 6 to 9. Only the forms are
// computed; nothing is asked of WhatsApp.
func phoneVariants(jid string) []string {
	user, ok := strings.CutSuffix(jid, "@"+types.DefaultUserServer)
	if !ok || !strings.HasPrefix(user, "55") || digits(user) != user {
		return nil
	}
	area, local := user[2:min(4, len(user))], user[min(4, len(user)):]
	if len(area) != 2 || area[0] == '0' || area[1] == '0' {
		return nil
	}
	switch {
	case len(local) == 9 && local[0] == '9':
		return []string{"55" + area + local[1:] + "@" + types.DefaultUserServer}
	case len(local) == 8 && local[0] >= '6':
		return []string{"55" + area + "9" + local + "@" + types.DefaultUserServer}
	}
	return nil
}

// nameKeys lists the keys a person's own row may be under, without a query when there is no row
// under an @lid: the phone JID, its 9th-digit variant and (only then) its @lid.
func (b *Bot) nameKeys(pn types.JID) []string {
	keys := append([]string{pn.String()}, phoneVariants(pn.String())...)
	b.mu.RLock()
	lids := b.ownLIDs
	b.mu.RUnlock()
	if lids > 0 && pn.Server == types.DefaultUserServer && b.Client.Store.LIDs != nil {
		if lid, err := b.Client.Store.LIDs.GetLIDForPN(context.Background(), pn); err == nil && !lid.IsEmpty() {
			keys = append(keys, lid.String())
		}
	}
	return keys
}

// ownContact is the part of an own row kept in memory: what names (and later quiet hours) need,
// never the notes.
type ownContact struct{ name, timeZone string }

func ownOf(c store.OwnContact) ownContact { return ownContact{c.Name, c.TimeZone} }

// ownEntry is the own row under the first of keys that has one, from memory.
func (b *Bot) ownEntry(keys ...string) (ownContact, bool) {
	if err := b.loadOwn(); err != nil {
		b.Log.Warn("load contacts", "err", err)
		return ownContact{}, false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, k := range keys {
		if c, ok := b.own[k]; ok {
			return c, true
		}
	}
	return ownContact{}, false
}

// loadOwn reads every own row into memory when it isn't there: the first time, and after a write
// while some row was under an @lid. Writes go through Bot, which keeps the map in step (applyOwn).
func (b *Bot) loadOwn() error {
	loaded := func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.own != nil
	}
	if loaded() {
		return nil
	}
	b.contactsMu.Lock()
	defer b.contactsMu.Unlock()
	if loaded() {
		return nil
	}
	rows, err := b.Store.Contacts(store.ContactFilter{})
	if err != nil {
		return err
	}
	own := make(map[string]ownContact, len(rows))
	lids := 0
	for _, c := range rows {
		own[c.JID] = ownOf(c)
		if strings.HasSuffix(c.JID, "@"+types.HiddenUserServer) {
			lids++
		}
	}
	b.mu.Lock()
	b.own, b.ownLIDs = own, lids
	b.mu.Unlock()
	return nil
}

// applyOwn brings the in-memory rows in step with changes just stored. contactsMu is held.
func (b *Bot) applyOwn(changes []store.ContactChange) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.own == nil {
		return
	}
	for _, ch := range changes {
		if len(ch.Changed) == 0 {
			continue
		}
		b.dropOwnLocked(ch.Before.JID)
		if !ch.After.Empty() {
			b.own[ch.After.JID] = ownOf(ch.After)
			if strings.HasSuffix(ch.After.JID, "@"+types.HiddenUserServer) {
				b.ownLIDs++
			}
		}
	}
	b.forgetLIDsLocked()
}

// forgetLIDsLocked drops the map while any row is under an @lid: the store may have moved it to the
// phone JID since (relinkLIDs), and the next read loads it again from there.
func (b *Bot) forgetLIDsLocked() {
	if b.ownLIDs > 0 {
		b.own, b.ownLIDs = nil, 0
	}
}

func (b *Bot) dropOwnLocked(jid string) {
	if _, ok := b.own[jid]; ok {
		delete(b.own, jid)
		if strings.HasSuffix(jid, "@"+types.HiddenUserServer) {
			b.ownLIDs--
		}
	}
}

// cleanName makes a name one plain line: invisible runes (control, format such as bidi and
// zero-width, variation selectors, tags) removed, line breaks and tabs folded to spaces, spaces
// collapsed. It doesn't cap the length; callers check it.
func cleanName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case invisibleRune(r):
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// cleanNotes keeps line breaks and tabs but drops other invisible runes.
func cleanNotes(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && invisibleRune(r) {
			return -1
		}
		return r
	}, s))
}

// invisibleRune is mcpserver.invisible's rule: runes that can hide or reorder text.
func invisibleRune(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Variation_Selector, r) || (r >= 0xe0000 && r <= 0xe007f)
}

// namesChangedEvery is the most often NamesChanged goes out: a history sync brings many push names.
const namesChangedEvery = time.Second

// namesChanged publishes NamesChanged now, or once at the end of the current second when one just
// went out. It never blocks.
func (b *Bot) namesChanged() {
	b.mu.Lock()
	if b.namesPending {
		b.mu.Unlock()
		return
	}
	wait := namesChangedEvery - time.Since(b.namesAt)
	if wait <= 0 {
		b.namesAt = time.Now()
		b.mu.Unlock()
		b.publish(NamesChanged{})
		return
	}
	b.namesPending = true
	b.mu.Unlock()
	b.Go(func(ctx context.Context) {
		err := SleepUntil(ctx, time.Now().Add(wait))
		b.mu.Lock()
		b.namesPending, b.namesAt = false, time.Now()
		b.mu.Unlock()
		if err == nil {
			b.publish(NamesChanged{})
		}
	})
}
