package bot

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/williamtcastro/wame/internal/store"
)

func stringsReader(s string) *strings.Reader { return strings.NewReader(s) }

func withLIDs(t *testing.T, b *Bot) {
	t.Helper()
	b.Client.Store.LIDs = sqlstore.NewWithDB(b.db, "sqlite3", waLog.Noop).LIDMap
}

func TestContactsPreferTheExactKey(t *testing.T) {
	b := newTestBot(t)
	patch(t, b, maria, ContactInput{Name: ptr("Maria"), Tags: &[]string{"vip"}})
	withBook(t, b, fakeBook{jidOf(maria): {FullName: "Maria Silva"}, jidOf("551199999999@s.whatsapp.net"): {FullName: "Maria (old number)"}})
	for range 20 {
		cs, err := b.Contacts(t.Context(), ContactQuery{Tags: []string{"vip"}})
		if err != nil || len(cs) != 1 || cs[0].JID != maria || cs[0].Display != "Maria" {
			t.Fatalf("%+v %v", cs, err)
		}
	}
}

func TestContactInputJSON(t *testing.T) {
	var in ContactInput
	if err := json.Unmarshal([]byte(`{"name":null,"notes":"","tags":null,"add_tags":["vip"]}`), &in); err != nil {
		t.Fatal(err)
	}
	if in.Name == nil || *in.Name != "" || in.Notes == nil || in.Tags == nil || len(*in.Tags) != 0 || in.Language != nil || in.TimeZone != nil ||
		!slices.Equal(in.AddTags, []string{"vip"}) {
		t.Fatalf("null clears, absent is nil: %+v", in)
	}
	for _, bad := range []string{`{"nick":"x"}`, `{"name":1}`, `{"tags":"vip"}`, `[]`, `null`, ` null`, `"x"`, `1`} {
		if err := json.Unmarshal([]byte(bad), &in); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestContactWrites(t *testing.T) {
	b := newTestBot(t)
	ctx := t.Context()
	v := patch(t, b, "+55 11 99999-9999", ContactInput{Name: ptr("  Maria\n‮Silva​ "), Tags: &[]string{"Clients", "vip"}, Notes: ptr("line one\r\nline two‮")})
	if v.JID != maria || v.Phone != "+5511999999999" || v.Display != "Maria Silva" || v.Source != SourceOwn || !slices.Equal(v.Tags, []string{"clients", "vip"}) ||
		v.Own == nil || v.Own.Notes != "line one\nline two" || v.Own.UpdatedBy != "ops" {
		t.Fatalf("%+v %+v", v, v.Own)
	}
	v = patch(t, b, maria, ContactInput{AddTags: []string{"new"}, RemoveTags: []string{"vip"}, TimeZone: ptr("America/Sao_Paulo"), Language: ptr("pt-BR")})
	if !slices.Equal(v.Tags, []string{"clients", "new"}) || v.Own.Notes == "" || v.Own.TimeZone != "America/Sao_Paulo" || v.Own.Language != "pt-BR" {
		t.Fatalf("a PATCH leaves absent fields alone: %+v", v.Own)
	}
	v, err := b.SetContact(ctx, "ops", maria, ContactInput{Name: ptr("Maria")})
	if err != nil || v.Own == nil || v.Own.Notes != "" || len(v.Tags) != 0 || v.Own.TimeZone != "" {
		t.Fatalf("a PUT clears absent fields: %+v %v", v.Own, err)
	}
	if v = patch(t, b, maria, ContactInput{Name: ptr("")}); v.Own != nil || v.Source != SourceNumber {
		t.Fatalf("all fields empty deletes the row: %+v", v)
	}
	if _, err := b.Store.Contact(maria); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	if v, err := b.ContactOf(ctx, "5511988887777"); err != nil || v.JID != john || v.Own != nil || v.Display != "+5511988887777" || v.Tags == nil {
		t.Fatalf("an unknown person is still an answer: %+v %v", v, err)
	}

	many := make([]string, store.MaxContactTags+1)
	for i := range many {
		many[i] = fmt.Sprint("t", i)
	}
	for name, c := range map[string]struct {
		to string
		in ContactInput
	}{
		"group":             {group, ContactInput{Name: ptr("Family")}},
		"bad number":        {"12", ContactInput{Name: ptr("x")}},
		"long name":         {maria, ContactInput{Name: ptr(strings.Repeat("é", MaxContactName+1))}},
		"long notes":        {maria, ContactInput{Notes: ptr(strings.Repeat("n", MaxContactNotes+1))}},
		"bad tag":           {maria, ContactInput{Tags: &[]string{"vip!"}}},
		"long tag":          {maria, ContactInput{AddTags: []string{strings.Repeat("a", MaxContactTag+1)}}},
		"too many tags":     {maria, ContactInput{Tags: &many}},
		"bad language":      {maria, ContactInput{Language: ptr("português")}},
		"long language":     {maria, ContactInput{Language: ptr("pt-BRAZILIAN-extra")}},
		"unknown zone":      {maria, ContactInput{TimeZone: ptr("Mars/Base")}},
		"server zone":       {maria, ContactInput{TimeZone: ptr("Local")}},
		"path zone":         {maria, ContactInput{TimeZone: ptr("../../etc/passwd")}},
		"put with add_tags": {maria, ContactInput{AddTags: []string{"vip"}}},
	} {
		put := name == "put with add_tags"
		var err error
		if put {
			_, err = b.SetContact(ctx, "ops", c.to, c.in)
		} else {
			_, err = b.PatchContact(ctx, "ops", c.to, c.in)
		}
		if !isInputError(err) {
			t.Errorf("%s: %v, want bad input", name, err)
		}
	}
	if err := b.DeleteContact(ctx, "ops", maria); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete without a row = %v", err)
	}
	if err := b.DeleteContact(ctx, "ops", group); !isInputError(err) {
		t.Fatalf("delete of a group = %v", err)
	}

	// Tags added one at a time stop at the cap with bad input, not a database error.
	for i := range store.MaxContactTags {
		patch(t, b, john, ContactInput{AddTags: []string{fmt.Sprint("t", i)}})
	}
	if _, err := b.PatchContact(ctx, "ops", john, ContactInput{AddTags: []string{"one-more"}}); !isInputError(err) {
		t.Fatalf("over the tag cap by adding: %v", err)
	}
	if n := b.Lookups.Stats().LastMinute; n != 0 {
		t.Fatalf("%d lookups", n)
	}
}

func TestContactVariantsNeverLookedUp(t *testing.T) {
	b := newTestBot(t)
	patch(t, b, "552187654321", ContactInput{Name: ptr("Ana")})
	v := patch(t, b, "5521987654321", ContactInput{Notes: ptr("same person")})
	if v.Own == nil || v.Own.JID != ana || v.Own.Name != "Ana" || v.Own.Notes != "same person" {
		t.Fatalf("the row under the other variant is updated: %+v", v.Own)
	}
	if list, _ := b.Store.Contacts(store.ContactFilter{}); len(list) != 1 {
		t.Fatalf("%+v", list)
	}
	if v, err := b.ContactOf(t.Context(), "5521987654321"); err != nil || v.Own == nil || v.Display != "Ana" {
		t.Fatalf("%+v %v", v, err)
	}
	if err := b.DeleteContact(t.Context(), "ops", "5521987654321"); err != nil {
		t.Fatal(err)
	}
	if n := b.Lookups.Stats().LastMinute; n != 0 {
		t.Fatalf("%d lookups", n)
	}
}

func TestContactsList(t *testing.T) {
	b := newTestBot(t)
	ctx := t.Context()
	const unknownLID = "100000000000003@lid"
	patch(t, b, maria, ContactInput{Name: ptr("Maria"), Tags: &[]string{"clients", "vip"}})
	patch(t, b, "552187654321", ContactInput{Tags: &[]string{"clients"}})
	patch(t, b, unknownLID, ContactInput{Notes: ptr("no name")})

	before, err := b.Contacts(ctx, ContactQuery{})
	if err != nil || len(before) != 3 {
		t.Fatalf("before login only your own rows: %+v %v", before, err)
	}

	withBook(t, b, fakeBook{
		jidOf(maria):                 {FullName: "Maria Silva", PushName: "Mari"},
		jidOf(john):                  {PushName: "Johnny"},
		jidOf(anaWith9):              {FullName: "Ana"},
		jidOf("100000000000001@lid"): {PushName: "hidden"},
		jidOf(group):                 {FullName: "not a person"},
	})
	all, err := b.Contacts(ctx, ContactQuery{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range all {
		got = append(got, c.Display+"|"+c.Source+"|"+strings.Join(c.Tags, ","))
	}
	want := []string{unknownLID + "|number|", "Ana|book|clients", "Maria|own|clients,vip", "~Johnny|push|"}
	if !slices.Equal(got, want) {
		t.Fatalf("merged list:\n got %q\nwant %q", got, want)
	}
	if all[1].JID != anaWith9 {
		t.Fatalf("a row under the other variant merges into the address-book entry: %+v", all[1])
	}

	names := func(q ContactQuery) []string {
		t.Helper()
		cs, err := b.Contacts(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, c := range cs {
			out = append(out, c.Display)
		}
		return out
	}
	for _, c := range []struct {
		q    ContactQuery
		want []string
	}{
		{ContactQuery{Tags: []string{"clients"}}, []string{"Ana", "Maria"}},
		{ContactQuery{Tags: []string{"CLIENTS", "vip"}}, []string{"Maria"}},
		{ContactQuery{Known: true}, []string{unknownLID, "Ana", "Maria"}},
		{ContactQuery{Q: "silva"}, []string{"Maria"}},
		{ContactQuery{Q: "+55 21 98765"}, []string{"Ana"}},
		{ContactQuery{Q: "vip"}, []string{"Maria"}},
		{ContactQuery{Limit: 2}, []string{unknownLID, "Ana"}},
		{ContactQuery{Allow: []string{john, anaWith9}}, []string{"Ana", "~Johnny"}},
	} {
		if got := names(c.q); !slices.Equal(got, c.want) {
			t.Errorf("%+v = %q, want %q", c.q, got, c.want)
		}
	}
	if _, err := b.Contacts(ctx, ContactQuery{Tags: []string{"no such!"}}); !isInputError(err) {
		t.Fatalf("a bad tag filter: %v", err)
	}

	raw, _ := json.Marshal(all[2])
	var shape map[string]any
	_ = json.Unmarshal(raw, &shape)
	for _, k := range []string{"jid", "phone", "name", "name_source", "own_name", "book_name", "push_name", "tags"} {
		if _, ok := shape[k]; !ok {
			t.Errorf("%s missing from %s", k, raw)
		}
	}
	if _, ok := shape["notes"]; ok {
		t.Errorf("the list never carries notes: %s", raw)
	}
	b.Client.Store.Contacts = fakeBook{jidOf(john): {}}
	if got := names(ContactQuery{}); slices.Contains(got, "+5511988887777") {
		t.Fatalf("an address-book entry without any name isn't listed: %q", got)
	}
	if n := b.Lookups.Stats().LastMinute; n != 0 {
		t.Fatalf("%d lookups", n)
	}
}

func TestImportContacts(t *testing.T) {
	b := newTestBot(t)
	ctx := t.Context()
	patch(t, b, john, ContactInput{Name: ptr("John"), Notes: ptr("prefers mornings")})
	patch(t, b, ana, ContactInput{Name: ptr("Ana")})

	csv := "\xef\xbb\xbfPhone;Name;tags\r\n5511999999999;Maria;clients vip\r\n5511988887777;Johnny;\r\n5521987654321;Ana;\r\n"
	res, err := b.ImportContacts(ctx, "ops", stringsReader(csv), ContactImportOptions{AddTags: []string{"Imported"}, DryRun: true})
	if err != nil || res != (store.ImportResult{Added: 1, Updated: 2}) {
		t.Fatalf("dry run: %+v %v", res, err)
	}
	if _, err := b.Store.Contact(maria); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a dry run stores nothing")
	}
	res, err = b.ImportContacts(ctx, "ops", stringsReader(csv), ContactImportOptions{AddTags: []string{"imported"}})
	if err != nil || res != (store.ImportResult{Added: 1, Updated: 2}) {
		t.Fatalf("%+v %v", res, err)
	}
	m, _ := b.Store.Contact(maria)
	j, _ := b.Store.Contact(john)
	a, _ := b.Store.Contact(ana, anaWith9)
	if m.Name != "Maria" || !slices.Equal(m.Tags, []string{"clients", "imported", "vip"}) ||
		j.Name != "Johnny" || j.Notes != "prefers mornings" || !slices.Equal(j.Tags, []string{"imported"}) ||
		a.JID != ana || !slices.Equal(a.Tags, []string{"imported"}) {
		t.Fatalf("%+v\n%+v\n%+v", m, j, a)
	}
	if got := b.ChatName(ctx, john, ""); got != "Johnny" {
		t.Fatalf("names follow an import: %q", got)
	}
	if res, err = b.ImportContacts(ctx, "ops", stringsReader("to,notes\n5511988887777,\n"), ContactImportOptions{}); err != nil || res.Updated != 1 {
		t.Fatalf("an empty cell clears: %+v %v", res, err)
	}
	if j, _ = b.Store.Contact(john); j.Notes != "" || j.Name != "Johnny" {
		t.Fatalf("%+v", j)
	}

	for name, c := range map[string]struct{ in, want string }{
		"bad tag on line 3":   {"number,name,tags\n5511999999999,Changed,ok\n5511988887777,x,BAD!\n", "line 3: tag"},
		"group":               {"number,name\n" + group + ",Family\n", "line 2: contacts are people"},
		"bad number":          {"number,name\n12,x\n", "line 2: invalid recipient"},
		"scientific notation": {"number,name\n5.511999E+12,Maria\n", "line 2: \"5.511999E+12\" is a number in scientific notation"},
		"unknown column":      {"number,nickname\n5511999999999,M\n", `unknown columns "nickname"`},
		"no number column":    {"name\nMaria\n", `the "number" column is required`},
		"long name":           {"number,name\n5511999999999," + strings.Repeat("x", MaxContactName+1) + "\n", "line 2: name has"},
		"bad zone":            {"number,time_zone\n5511999999999,Mars/Base\n", "line 2: unknown time zone"},
		"tags over the cap":   {"number,tags\n5511999999999," + manyTags(store.MaxContactTags) + "\n", "line 2: at most"},
		"empty":               {"", "the CSV is empty: the first line must name the columns, like number,name"},
	} {
		_, err := b.ImportContacts(ctx, "ops", stringsReader(c.in), ContactImportOptions{AddTags: []string{"x"}})
		if !isInputError(err) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want bad input with %q", name, err, c.want)
		}
	}
	if m, _ := b.Store.Contact(maria); m.Name != "Maria" || slices.Contains(m.Tags, "ok") {
		t.Fatal("a failed import stores nothing, not even the good lines before the bad one")
	}
	big := "number\n" + strings.Repeat("5511999999999\n", MaxContactImport+1)
	if _, err := b.ImportContacts(ctx, "ops", stringsReader(big), ContactImportOptions{}); !isInputError(err) || !strings.Contains(err.Error(), "at most 5000 contacts") {
		t.Fatalf("over the row limit: %v", err)
	}
	if _, err := b.ImportContacts(ctx, "ops", stringsReader("number\n5511999999999\n"), ContactImportOptions{AddTags: []string{"BAD!"}}); !isInputError(err) {
		t.Fatalf("a bad add_tags: %v", err)
	}
	if n := b.Lookups.Stats().LastMinute; n != 0 {
		t.Fatalf("an import must never look numbers up: %d lookups", n)
	}
}

func manyTags(n int) string {
	var out []string
	for i := range n {
		out = append(out, fmt.Sprint("m", i))
	}
	return strings.Join(out, "|")
}

func TestContactAuditCarriesNoValues(t *testing.T) {
	b := newTestBot(t)
	ctx := t.Context()
	patch(t, b, maria, ContactInput{Name: ptr("Maria"), Tags: &[]string{"secret-tag"}, Notes: ptr("private note")})
	patch(t, b, maria, ContactInput{Name: ptr("Maria")})
	if _, err := b.PatchContact(ctx, "ops", maria, ContactInput{Name: ptr(strings.Repeat("x", 200))}); err == nil {
		t.Fatal("too long")
	}
	if _, err := b.ImportContacts(ctx, "ops", stringsReader("number,name,notes\n5511988887777,John,prefers mornings\n5511999999999,Maria,\n"), ContactImportOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteContact(ctx, "ops", maria); err != nil {
		t.Fatal(err)
	}
	if _, err := b.DeleteAllContacts(ctx, "ops"); err != nil {
		t.Fatal(err)
	}
	log, err := b.Store.AuditLog(20, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range log {
		for _, secret := range []string{"Maria", "John", "secret-tag", "private note", "prefers", "xxxx"} {
			if strings.Contains(e.Detail+e.Error, secret) {
				t.Errorf("audit %s carries %q: %+v", e.Action, secret, e)
			}
		}
		got = append(got, fmt.Sprintf("%s %s %s %v", e.Action, e.Target, e.Detail, e.OK))
	}
	slices.Reverse(got)
	want := []string{
		"contact_set " + maria + " name, tags, notes true",
		"contact_set " + maria + " no change true",
		"contact_set " + maria + "  false",
		"contact_import csv 2 rows: 1 added, 1 updated true",
		"contact_delete " + maria + "  true",
		"contact_delete_all  1 contacts true",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("audit:\n got %q\nwant %q", got, want)
	}
}

func TestContactKeysDropTheDevice(t *testing.T) {
	b := newTestBot(t)
	v := patch(t, b, "5511999999999:3@s.whatsapp.net", ContactInput{Name: ptr("Maria")})
	if v.JID != maria || v.Own == nil || v.Own.JID != maria {
		t.Fatalf("stored under %+v", v.Own)
	}
	if got := b.ChatName(t.Context(), maria, ""); got != "Maria" {
		t.Fatalf("name = %q", got)
	}
	if chat, err := b.LocalChat("5511999999999.0:3@s.whatsapp.net"); err != nil || chat != maria {
		t.Fatalf("LocalChat = %q %v", chat, err)
	}
}

func TestExportContactsIsAudited(t *testing.T) {
	b := newTestBot(t)
	patch(t, b, maria, ContactInput{Name: ptr("Maria"), Notes: ptr("prefers mornings")})
	list, err := b.ExportContacts(t.Context(), "ops")
	if err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	entries, _ := b.Store.AuditLog(1, 0)
	if len(entries) != 1 || entries[0].Action != "contact_export" || entries[0].Actor != "ops" || entries[0].Detail != "1 contacts" {
		t.Fatalf("audit = %+v", entries)
	}
}
