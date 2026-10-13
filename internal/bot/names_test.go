package bot

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	wstore "github.com/williamtcastro/wame/internal/store"
)

const anaWith9 = "5521987654321@s.whatsapp.net"

// fakeBook stands in for whatsmeow's contact store (the phone's address book, push and business names).
type fakeBook map[types.JID]types.ContactInfo

func (f fakeBook) GetContact(_ context.Context, j types.JID) (types.ContactInfo, error) {
	c, ok := f[j]
	c.Found = ok
	return c, nil
}
func (f fakeBook) GetAllContacts(context.Context) (map[types.JID]types.ContactInfo, error) {
	return f, nil
}
func (fakeBook) PutPushName(context.Context, types.JID, string) (bool, string, error) {
	panic("wame must not write to the address book")
}
func (fakeBook) PutBusinessName(context.Context, types.JID, string) (bool, string, error) {
	panic("wame must not write to the address book")
}
func (fakeBook) PutContactName(context.Context, types.JID, string, string) error {
	panic("wame must not write to the address book")
}
func (fakeBook) PutAllContactNames(context.Context, []store.ContactEntry) error {
	panic("wame must not write to the address book")
}
func (fakeBook) PutManyRedactedPhones(context.Context, []store.RedactedPhoneEntry) error {
	panic("wame must not write to the address book")
}

func jidOf(s string) types.JID {
	j, err := types.ParseJID(s)
	if err != nil {
		panic(err)
	}
	return j
}

func withBook(t *testing.T, b *Bot, book fakeBook) {
	t.Helper()
	b.Client.Store.Contacts = book
	t.Cleanup(func() { b.Client.Store.Contacts = nil })
}

func patch(t *testing.T, b *Bot, to string, in ContactInput) ContactView {
	t.Helper()
	v, err := b.PatchContact(t.Context(), "ops", to, in)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestPhoneVariants(t *testing.T) {
	for in, want := range map[string][]string{
		anaWith9:                       {ana},
		ana:                            {anaWith9},
		maria:                          {"551199999999@s.whatsapp.net"},
		"552100000000@s.whatsapp.net":  nil, // only mobile numbers (6 to 9) gained the 9
		"5521923456789@s.whatsapp.net": nil, // never the landline 552123456789
		"552123456789@s.whatsapp.net":  nil,
		"5521800000000@s.whatsapp.net": nil,
		"14155550100@s.whatsapp.net":   nil,
		"5500987654321@s.whatsapp.net": nil,
		"100000000000001@lid":          nil,
		group:                          nil,
	} {
		if got := phoneVariants(in); !slices.Equal(got, want) {
			t.Errorf("phoneVariants(%s) = %v, want %v", in, got, want)
		}
	}
}

func TestNamesOrder(t *testing.T) {
	b := newTestBot(t)
	b.mu.Lock()
	b.groups = map[string]groupInfo{group: {name: "Family"}}
	b.mu.Unlock()
	patch(t, b, "5511999999999", ContactInput{Name: ptr("Maria")})
	patch(t, b, "552187654321", ContactInput{Name: ptr("Ana (own)")})

	ctx := t.Context()
	for _, c := range []struct {
		book       fakeBook
		chat, push string
		want       Names
	}{
		{fakeBook{jidOf(maria): {FullName: "Maria Silva", PushName: "Mari"}}, maria, "", Names{Display: "Maria", Source: SourceOwn, Own: "Maria", Book: "Maria Silva", Push: "Mari"}},
		{fakeBook{jidOf(john): {FullName: "John Smith", FirstName: "John", BusinessName: "Shop", PushName: "Johnny"}}, john, "",
			Names{Display: "John Smith", Source: SourceBook, Book: "John Smith", Business: "Shop", Push: "Johnny"}},
		{fakeBook{jidOf(john): {FirstName: "John", BusinessName: "Shop"}}, john, "", Names{Display: "John", Source: SourceBook, Book: "John", Business: "Shop"}},
		{fakeBook{jidOf(anaWith9): {FullName: "Ana Paula"}}, anaWith9, "", Names{Display: "Ana (own)", Source: SourceOwn, Own: "Ana (own)", Book: "Ana Paula"}},
		{fakeBook{jidOf(anaWith9): {FullName: "Ana Paula"}}, ana, "", Names{Display: "Ana (own)", Source: SourceOwn, Own: "Ana (own)", Book: "Ana Paula"}},
		{fakeBook{jidOf(john): {BusinessName: "Shop", PushName: "shop"}}, john, "Johnny", Names{Display: "Shop", Source: SourceBusiness, Business: "Shop", Push: "Johnny"}},
		{fakeBook{jidOf(john): {PushName: "Johnny"}}, john, "", Names{Display: "~Johnny", Source: SourcePush, Push: "Johnny"}},
		{nil, john, "Jo", Names{Display: "~Jo", Source: SourcePush, Push: "Jo"}},
		{nil, john, "", Names{Display: "+5511988887777", Source: SourceNumber}},
		{nil, "100000000000009@lid", "", Names{Display: "100000000000009@lid", Source: SourceNumber}},
		{nil, group, "", Names{Display: "Family", Source: SourceGroup}},
		{nil, "120363000000000002@g.us", "", Names{Display: "group 120363000000000002", Source: SourceGroup}},
	} {
		b.Client.Store.Contacts = c.book
		if c.book == nil {
			b.Client.Store.Contacts = nil
		}
		if got := b.NamesOf(ctx, c.chat, c.push); got != c.want {
			t.Errorf("NamesOf(%s, %q) = %+v, want %+v", c.chat, c.push, got, c.want)
		}
		if got := b.ChatName(ctx, c.chat, c.push); got != c.want.Display {
			t.Errorf("ChatName(%s) = %q", c.chat, got)
		}
	}
	b.Client.Store.Contacts = nil
	b.Client.Store.ID = new(jidOf("5511988887777.0:1@s.whatsapp.net"))
	t.Cleanup(func() { b.Client.Store.ID = nil })
	if got := b.NamesOf(ctx, john, "Johnny"); got != (Names{Display: "Me", Source: SourceMe}) {
		t.Errorf("own chat = %+v", got)
	}
	if n := b.Lookups.Stats().LastMinute; n != 0 {
		t.Fatalf("names must never look anything up, %d lookups", n)
	}
}

func TestOwnNameMapFollowsWrites(t *testing.T) {
	b := newTestBot(t)
	ctx := t.Context()
	name := func(chat string) string { return b.ChatName(ctx, chat, "Mari") }
	if got := name(maria); got != "~Mari" {
		t.Fatal(got)
	}
	patch(t, b, maria, ContactInput{Name: ptr("Maria")})
	if got := name(maria); got != "Maria" {
		t.Fatalf("after a write: %q", got)
	}
	patch(t, b, maria, ContactInput{Name: ptr("")})
	if got := name(maria); got != "~Mari" {
		t.Fatalf("after clearing the name: %q", got)
	}
	if _, err := b.SetContact(ctx, "ops", maria, ContactInput{Name: ptr("Maria"), Tags: &[]string{"vip"}}); err != nil {
		t.Fatal(err)
	}
	if got := name(maria); got != "Maria" {
		t.Fatalf("after a PUT: %q", got)
	}
	if err := b.DeleteContact(ctx, "ops", maria); err != nil {
		t.Fatal(err)
	}
	if got := name(maria); got != "~Mari" {
		t.Fatalf("after a delete: %q", got)
	}
	if _, err := b.ImportContacts(ctx, "ops", stringsReader("number,name\n5511999999999,Maria\n"), ContactImportOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := name(maria); got != "Maria" {
		t.Fatalf("after an import: %q", got)
	}
	if _, err := b.DeleteAllContacts(ctx, "ops"); err != nil {
		t.Fatal(err)
	}
	if got := name(maria); got != "~Mari" {
		t.Fatalf("after deleting all: %q", got)
	}
}

func TestOwnNameUnderLIDFoundByPhone(t *testing.T) {
	b := newTestBot(t)
	withLIDs(t, b)
	const mariaLID = "100000000000001@lid"
	_ = b.ChatName(t.Context(), maria, "") // loads the in-memory names before the @lid row exists
	patch(t, b, mariaLID, ContactInput{Name: ptr("Maria")})
	if err := b.Client.Store.LIDs.PutLIDMapping(t.Context(), jidOf(mariaLID), jidOf(maria)); err != nil {
		t.Fatal(err)
	}
	if got := b.ChatName(t.Context(), maria, ""); got != "Maria" {
		t.Fatalf("a row stored under the @lid before the number was known: %q", got)
	}
	patch(t, b, maria, ContactInput{Name: ptr("")})
	if got := b.ChatName(t.Context(), maria, ""); got != "+5511999999999" {
		t.Fatalf("a name cleared after the row moved from the @lid: %q", got)
	}
	patch(t, b, maria, ContactInput{Name: ptr("Maria")})
	if err := b.DeleteContact(t.Context(), "ops", maria); err != nil {
		t.Fatal(err)
	}
	if got := b.ChatName(t.Context(), maria, ""); got != "+5511999999999" {
		t.Fatalf("after a delete: %q", got)
	}
}

func TestNamesChangedIsDebounced(t *testing.T) {
	b := newTestBot(t)
	ch, cancel := b.Subscribe()
	defer cancel()
	count := func(within time.Duration) int {
		n := 0
		deadline := time.After(within)
		for {
			select {
			case v := <-ch:
				if _, ok := v.(NamesChanged); ok {
					n++
				}
			case <-deadline:
				return n
			}
		}
	}
	for range 5 {
		b.handle(&events.PushName{JID: jidOf(maria), NewPushName: "Maria"})
	}
	if n := count(100 * time.Millisecond); n != 1 {
		t.Fatalf("first burst: %d events, want 1 at once", n)
	}
	if n := count(1500 * time.Millisecond); n != 1 {
		t.Fatalf("the rest of the burst: %d events, want 1 more", n)
	}
	b.handle(&events.Contact{JID: jidOf(john)})
	b.handle(&events.BusinessName{JID: jidOf(john)})
	if n := count(1500 * time.Millisecond); n < 1 || n > 2 {
		t.Fatalf("address-book and business name changes: %d events", n)
	}
}

func TestOwnNameAfterRelinkMerge(t *testing.T) {
	b := newTestBot(t)
	withLIDs(t, b)
	const anaLID = "100000000000003@lid"
	patch(t, b, ana, ContactInput{Tags: &[]string{"vip"}})
	patch(t, b, anaLID, ContactInput{Name: ptr("Ana")})
	if got := b.ChatName(t.Context(), ana, ""); got != "+552187654321" {
		t.Fatalf("before the mapping: %q", got)
	}
	if err := b.Client.Store.LIDs.PutLIDMapping(t.Context(), jidOf(anaLID), jidOf(ana)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Store.Contacts(wstore.ContactFilter{}); err != nil { // relinks
		t.Fatal(err)
	}
	if got := b.ChatName(t.Context(), ana, ""); got != "Ana" {
		t.Fatalf("after the @lid row merged into the tagged phone row: %q", got)
	}
	b.mu.RLock()
	_, stale := b.own[anaLID]
	lids := b.ownLIDs
	b.mu.RUnlock()
	if stale || lids != 0 {
		t.Fatalf("the in-memory rows still hold the @lid row: %v, %d", stale, lids)
	}
}

func TestOwnNameDuringConcurrentWrites(t *testing.T) {
	b := newTestBot(t)
	withLIDs(t, b)
	patch(t, b, maria, ContactInput{Name: ptr("Maria")})
	ctx := t.Context()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 100 {
			if _, err := b.PatchContact(ctx, "ops", "100000000000004@lid", ContactInput{Name: ptr(fmt.Sprint("John ", i))}); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for {
		select {
		case <-done:
			return
		default:
		}
		if got := b.ChatName(ctx, maria, ""); got != "Maria" {
			t.Fatalf("a write to someone else hid Maria's name: %q", got)
		}
	}
}
