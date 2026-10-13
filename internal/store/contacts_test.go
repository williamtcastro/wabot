package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
)

func setContact(t *testing.T, s *Store, e ContactEdit) ContactChange {
	t.Helper()
	ch, err := s.SetContact(e)
	if err != nil {
		t.Fatal(err)
	}
	return ch
}

func TestMigrateContactsIdempotent(t *testing.T) {
	db := openDB(t)
	for range 2 {
		if _, err := Open(context.Background(), db); err != nil {
			t.Fatal(err)
		}
	}
	s := must[*Store](t)(Open(context.Background(), db))
	setContact(t, s, ContactEdit{Keys: []string{maria}, Set: OwnContact{Name: "Maria"}, Fields: []string{FieldName}, Actor: "ops"})
	if _, err := Open(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if c, err := s.Contact(maria); err != nil || c.Name != "Maria" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestSetContact(t *testing.T) {
	s := openStore(t)
	if _, err := s.Contact(maria); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no row yet: %v", err)
	}
	ch := setContact(t, s, ContactEdit{Keys: []string{maria}, Set: OwnContact{Name: "Maria", Tags: []string{"vip", "clients", "vip"}, Notes: "ignored"},
		Fields: []string{FieldName, FieldTags}, Actor: "ops"})
	if !ch.Before.Empty() || !slices.Equal(ch.Changed, []string{FieldName, FieldTags}) || ch.After.Notes != "" ||
		!slices.Equal(ch.After.Tags, []string{"clients", "vip"}) || ch.After.CreatedBy != "ops" || ch.After.CreatedAt.IsZero() {
		t.Fatalf("%+v", ch)
	}
	ch = setContact(t, s, ContactEdit{Keys: []string{maria}, AddTags: []string{"new"}, RemoveTags: []string{"vip", "absent"}, Actor: "tui"})
	if !slices.Equal(ch.After.Tags, []string{"clients", "new"}) || ch.After.Name != "Maria" || ch.After.CreatedBy != "ops" || ch.After.UpdatedBy != "tui" {
		t.Fatalf("tag edit: %+v", ch.After)
	}
	if ch = setContact(t, s, ContactEdit{Keys: []string{maria}, Set: OwnContact{Name: "Maria"}, Fields: []string{FieldName}, Actor: "x"}); len(ch.Changed) != 0 || ch.After.UpdatedBy != "tui" {
		t.Fatalf("no change must not write: %+v", ch)
	}
	ch = setContact(t, s, ContactEdit{Keys: []string{maria}, Fields: ContactFields, Actor: "ops"})
	if !ch.After.Empty() || len(ch.Changed) != 2 {
		t.Fatalf("clearing every field: %+v", ch)
	}
	if _, err := s.Contact(maria); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an empty row is deleted: %v", err)
	}
	if _, err := s.SetContact(ContactEdit{Keys: []string{john}, AddTags: make([]string, MaxContactTags+1)}); err != nil {
		t.Fatalf("empty tags are dropped: %v", err)
	}
	many := make([]string, MaxContactTags+1)
	for i := range many {
		many[i] = fmt.Sprint("t", i)
	}
	if _, err := s.SetContact(ContactEdit{Keys: []string{john}, AddTags: many}); !errors.Is(err, ErrTooManyTags) {
		t.Fatalf("over the tag cap: %v", err)
	}
}

func TestContactUnderVariantKey(t *testing.T) {
	s := openStore(t)
	const anaWith9 = "5521987654321@s.whatsapp.net"
	setContact(t, s, ContactEdit{Keys: []string{ana}, Set: OwnContact{Name: "Ana"}, Fields: []string{FieldName}})
	ch := setContact(t, s, ContactEdit{Keys: []string{anaWith9, ana}, Set: OwnContact{Notes: "n"}, Fields: []string{FieldNotes}})
	if ch.After.JID != ana || ch.After.Name != "Ana" || ch.After.Notes != "n" {
		t.Fatalf("an existing row under the other variant is updated: %+v", ch.After)
	}
	if list := must[[]OwnContact](t)(s.Contacts(ContactFilter{})); len(list) != 1 {
		t.Fatalf("%+v", list)
	}
	if err := s.DeleteContact(anaWith9, ana); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteContact(anaWith9, ana); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}
	if err := s.DeleteContact(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no keys = %v", err)
	}
}

func TestContactsFilterAndTags(t *testing.T) {
	s := openStore(t)
	for jid, tags := range map[string][]string{maria: {"clients", "vip"}, john: {"clients"}, ana: {"a_b"}} {
		setContact(t, s, ContactEdit{Keys: []string{jid}, Set: OwnContact{Tags: tags}, Fields: []string{FieldTags}})
	}
	setContact(t, s, ContactEdit{Keys: []string{"100000000000003@lid"}, Set: OwnContact{Tags: []string{"axb"}}, Fields: []string{FieldTags}})
	jids := func(f ContactFilter) []string {
		var out []string
		for _, c := range must[[]OwnContact](t)(s.Contacts(f)) {
			out = append(out, c.JID)
		}
		return out
	}
	if got := jids(ContactFilter{Tags: []string{"clients"}}); !slices.Equal(got, []string{john, maria}) {
		t.Errorf("clients = %v", got)
	}
	if got := jids(ContactFilter{Tags: []string{"clients", "vip"}}); !slices.Equal(got, []string{maria}) {
		t.Errorf("clients AND vip = %v", got)
	}
	if got := jids(ContactFilter{Tags: []string{"a_b"}}); !slices.Equal(got, []string{ana}) {
		t.Errorf("_ must not be a wildcard: %v", got)
	}
	if got := jids(ContactFilter{Tags: []string{"client"}}); len(got) != 0 {
		t.Errorf("a prefix of a tag must not match: %v", got)
	}
	if got := jids(ContactFilter{JIDs: []string{maria, ana}}); !slices.Equal(got, []string{maria, ana}) {
		t.Errorf("JIDs = %v", got)
	}
	if got := jids(ContactFilter{JIDs: []string{}}); len(got) != 0 {
		t.Errorf("an empty JID list matches nothing: %v", got)
	}
	tags := must[[]TagCount](t)(s.ContactTags())
	if want := []TagCount{{"a_b", 1}, {"axb", 1}, {"clients", 2}, {"vip", 1}}; !slices.Equal(tags, want) {
		t.Errorf("tags = %v", tags)
	}
	if n, err := s.DeleteAllContacts(); n != 4 || err != nil {
		t.Fatal(n, err)
	}
	if got := jids(ContactFilter{}); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestImportContactsIsAtomic(t *testing.T) {
	s := openStore(t)
	setContact(t, s, ContactEdit{Keys: []string{john}, Set: OwnContact{Name: "John", Notes: "keep"}, Fields: []string{FieldName, FieldNotes}})
	setContact(t, s, ContactEdit{Keys: []string{ana}, Set: OwnContact{Name: "Ana"}, Fields: []string{FieldName}})
	edits := []ContactEdit{
		{Keys: []string{maria}, Set: OwnContact{Name: "Maria"}, Fields: []string{FieldName}, AddTags: []string{"clients"}},
		{Keys: []string{john}, Set: OwnContact{Name: "Johnny"}, Fields: []string{FieldName}},
		{Keys: []string{ana}, Set: OwnContact{Name: "Ana"}, Fields: []string{FieldName}},
	}
	res, _, err := s.ImportContacts(edits, true)
	if err != nil || res != (ImportResult{Added: 1, Updated: 1, Unchanged: 1}) {
		t.Fatalf("dry run: %+v %v", res, err)
	}
	if _, err := s.Contact(maria); !errors.Is(err, ErrNotFound) {
		t.Fatal("a dry run stores nothing")
	}
	many := make([]string, MaxContactTags+1)
	for i := range many {
		many[i] = fmt.Sprint("t", i)
	}
	bad := append(slices.Clone(edits), ContactEdit{Keys: []string{"100000000000003@lid"}, Set: OwnContact{Tags: many}, Fields: []string{FieldTags}})
	if _, _, err := s.ImportContacts(bad, false); !errors.Is(err, ErrTooManyTags) {
		t.Fatal(err)
	}
	if _, err := s.Contact(maria); !errors.Is(err, ErrNotFound) {
		t.Fatal("a failed import stores nothing")
	}
	res, changes, err := s.ImportContacts(edits, false)
	if err != nil || res != (ImportResult{Added: 1, Updated: 1, Unchanged: 1}) || len(changes) != 3 {
		t.Fatalf("%+v %v", res, err)
	}
	if c, _ := s.Contact(john); c.Name != "Johnny" || c.Notes != "keep" {
		t.Fatalf("absent columns are left alone: %+v", c)
	}
}

func TestContactLimit(t *testing.T) {
	s := openStore(t)
	if _, err := s.db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO wame_contacts (jid, name, created_at, updated_at, created_by, updated_by)
		SELECT i || '@s.whatsapp.net', 'x', 0, 0, 'test', 'test' FROM n`, 50); err != nil {
		t.Fatal(err)
	}
	old := contactCap
	contactCap = 50
	t.Cleanup(func() { contactCap = old })
	if _, err := s.SetContact(ContactEdit{Keys: []string{maria}, Set: OwnContact{Name: "Maria"}, Fields: []string{FieldName}}); !errors.Is(err, ErrContactLimit) {
		t.Fatalf("over the cap: %v", err)
	}
	if _, err := s.SetContact(ContactEdit{Keys: []string{"1@s.whatsapp.net"}, Set: OwnContact{Name: "y"}, Fields: []string{FieldName}}); err != nil {
		t.Fatalf("existing rows can still change: %v", err)
	}
}

func TestConcurrentTagEdits(t *testing.T) {
	s := openStore(t)
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := range 10 {
		wg.Go(func() {
			_, err := s.SetContact(ContactEdit{Keys: []string{maria}, AddTags: []string{fmt.Sprint("t", i)}})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if c, _ := s.Contact(maria); len(c.Tags) != 10 {
		t.Fatalf("a tag edit was lost: %v", c.Tags)
	}
}

func TestContactUnderLIDIsRelinked(t *testing.T) {
	s := openStore(t)
	if _, err := s.db.Exec(`CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT UNIQUE NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	const mariaLID, johnLID = "100000000000001@lid", "100000000000002@lid"
	setContact(t, s, ContactEdit{Keys: []string{maria}, Set: OwnContact{Name: "Maria", Tags: []string{"clients"}}, Fields: []string{FieldName, FieldTags}})
	setContact(t, s, ContactEdit{Keys: []string{mariaLID}, Set: OwnContact{Name: "Other", Tags: []string{"vip", "clients"}, Notes: "from lid", Language: "pt-BR"},
		Fields: ContactFields})
	setContact(t, s, ContactEdit{Keys: []string{johnLID}, Set: OwnContact{Name: "John"}, Fields: []string{FieldName}})
	if _, err := s.db.Exec(`INSERT INTO whatsmeow_lid_map VALUES ('100000000000001', '5511999999999'), ('100000000000002', '5511988887777')`); err != nil {
		t.Fatal(err)
	}
	list := must[[]OwnContact](t)(s.Contacts(ContactFilter{}))
	if len(list) != 2 || list[0].JID != john || list[0].Name != "John" {
		t.Fatalf("%+v", list)
	}
	m := list[1]
	if m.JID != maria || m.Name != "Maria" || m.Notes != "from lid" || m.Language != "pt-BR" || !slices.Equal(m.Tags, []string{"clients", "vip"}) {
		t.Fatalf("merged = %+v", m)
	}
	if tags := must[[]TagCount](t)(s.ContactTags()); !slices.Equal(tags, []TagCount{{"clients", 1}, {"vip", 1}}) {
		t.Fatalf("merged tags count once: %v", tags)
	}
	if got := must[[]OwnContact](t)(s.Contacts(ContactFilter{Tags: []string{"vip"}})); len(got) != 1 {
		t.Fatalf("a merged tag still matches: %+v", got)
	}
	ch := setContact(t, s, ContactEdit{Keys: []string{maria}, AddTags: []string{"x"}})
	if !slices.Equal(ch.After.Tags, []string{"clients", "vip", "x"}) {
		t.Fatalf("the next write normalizes the tags: %v", ch.After.Tags)
	}
	var raw string
	if err := s.db.QueryRow(`SELECT tags FROM wame_contacts WHERE jid = ?`, maria).Scan(&raw); err != nil || raw != ",clients,vip,x," {
		t.Fatal(raw, err)
	}
}

func TestRelinkMergeKeepsBothRows(t *testing.T) {
	s := openStore(t)
	if _, err := s.db.Exec(`CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT UNIQUE NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	const mariaLID, johnLID = "100000000000001@lid", "100000000000002@lid"
	var phoneTags, lidTags []string
	for i := range MaxContactTags {
		phoneTags, lidTags = append(phoneTags, fmt.Sprintf("p%02d", i)), append(lidTags, fmt.Sprintf("a%02d", i))
	}
	setContact(t, s, ContactEdit{Keys: []string{maria}, Set: OwnContact{Name: "Maria", Notes: "phone notes", Tags: phoneTags[:MaxContactTags-2]},
		Fields: ContactFields, Actor: "ops"})
	setContact(t, s, ContactEdit{Keys: []string{mariaLID}, Set: OwnContact{Name: "Other", Notes: "lid notes", TimeZone: "America/Sao_Paulo", Tags: lidTags},
		Fields: ContactFields, Actor: "ops"})
	setContact(t, s, ContactEdit{Keys: []string{john}, Set: OwnContact{Notes: "same"}, Fields: ContactFields, Actor: "ops"})
	setContact(t, s, ContactEdit{Keys: []string{johnLID}, Set: OwnContact{Name: "John", Notes: "same"}, Fields: ContactFields, Actor: "ops"})
	gen := s.ContactsGen()
	if _, err := s.db.Exec(`INSERT INTO whatsmeow_lid_map VALUES ('100000000000001', '5511999999999'), ('100000000000002', '5511988887777')`); err != nil {
		t.Fatal(err)
	}
	list := must[[]OwnContact](t)(s.Contacts(ContactFilter{}))
	if len(list) != 2 {
		t.Fatalf("%+v", list)
	}
	j, m := list[0], list[1]
	if j.JID != john || j.Name != "John" || j.Notes != "same" || j.UpdatedBy != relinkActor {
		t.Fatalf("john = %+v", j)
	}
	if m.JID != maria || m.Name != "Maria" || m.Notes != "phone notes\nlid notes" || m.TimeZone != "America/Sao_Paulo" || m.UpdatedBy != relinkActor {
		t.Fatalf("maria = %+v", m)
	}
	if len(m.Tags) != MaxContactTags || !slices.Contains(m.Tags, "p17") || !slices.Contains(m.Tags, "a01") || slices.Contains(m.Tags, "a02") {
		t.Fatalf("tags are united with the phone row's first, capped: %v", m.Tags)
	}
	if s.ContactsGen() == gen {
		t.Fatal("a relink that moved rows must change ContactsGen")
	}
	gen = s.ContactsGen()
	_ = must[[]OwnContact](t)(s.Contacts(ContactFilter{}))
	if s.ContactsGen() != gen {
		t.Fatal("a read without anything to relink changed ContactsGen")
	}
	if _, err := s.SetContact(ContactEdit{Keys: []string{maria}, Set: OwnContact{Language: "pt-BR"}, Fields: []string{FieldLanguage}}); err != nil {
		t.Fatalf("a PATCH without tags after a merge: %v", err)
	}
}

func TestImportCountsEachPersonOnce(t *testing.T) {
	s := openStore(t)
	setContact(t, s, ContactEdit{Keys: []string{john}, Set: OwnContact{Name: "John"}, Fields: []string{FieldName}})
	edits := []ContactEdit{
		{Keys: []string{maria}, Set: OwnContact{Name: "Maria"}, Fields: []string{FieldName}},
		{Keys: []string{maria}, Set: OwnContact{Notes: "x"}, Fields: []string{FieldNotes}},
		{Keys: []string{john}, Set: OwnContact{Name: "Johnny"}, Fields: []string{FieldName}},
		{Keys: []string{john}, Set: OwnContact{Name: "John"}, Fields: []string{FieldName}},
		{Keys: []string{ana}, Set: OwnContact{Name: "Ana"}, Fields: []string{FieldName}},
		{Keys: []string{ana}, Set: OwnContact{Name: ""}, Fields: []string{FieldName}},
	}
	for _, dry := range []bool{true, false} {
		gen := s.ContactsGen()
		res, _, err := s.ImportContacts(edits, dry)
		if err != nil || res != (ImportResult{Added: 1, Unchanged: 2}) {
			t.Fatalf("dry run %v: %+v %v", dry, res, err)
		}
		if changed := s.ContactsGen() != gen; changed == dry {
			t.Fatalf("dry run %v changed ContactsGen: %v", dry, changed)
		}
	}
}
