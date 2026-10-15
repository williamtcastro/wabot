package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

const (
	marialid = "100000000000001@lid"
	family   = "120363012345678901@g.us"
	work     = "120363012345678902@g.us"
)

func TestContactCacheMigrationIdempotent(t *testing.T) {
	db := openDB(t)
	for range 2 {
		if _, err := Open(context.Background(), db); err != nil {
			t.Fatal(err)
		}
	}
}

func TestContactCacheEventsTouchExistingRowsOnly(t *testing.T) {
	s := openStore(t)
	for name, f := range map[string]func() (bool, error){
		"about":   func() (bool, error) { return s.CacheAbout(john, "Hey there", time.Now()) },
		"picture": func() (bool, error) { return s.CachePicture(john, "1712345678") },
		"name":    func() (bool, error) { return s.CacheVerifiedName(john, "John Ltd") },
	} {
		if changed, err := f(); err != nil || changed {
			t.Errorf("%s on a person without a row: changed=%v err=%v", name, changed, err)
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM wame_contact_cache`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("events created %d rows (err %v)", n, err)
	}

	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	if err := s.PutCachedProfile(CachedProfile{JID: maria, OnWhatsApp: 1, About: "old", AboutAt: at,
		PictureID: "1", PictureURL: "https://cdn.example/old", InfoAt: at, LID: marialid}); err != nil {
		t.Fatal(err)
	}
	if changed, _ := s.CacheAbout(maria, "older", at.Add(-time.Minute)); changed {
		t.Error("an older about must not replace a newer one")
	}
	if changed, _ := s.CacheAbout(maria, "new", at.Add(time.Minute)); !changed {
		t.Error("about not updated")
	}
	if changed, _ := s.CachePicture(maria, "2"); !changed {
		t.Error("picture not updated")
	}
	if changed, _ := s.CacheVerifiedName(maria, "Maria Ltd"); !changed {
		t.Error("verified name not updated")
	}
	c, err := s.CachedProfileOf(john, maria)
	if err != nil {
		t.Fatal(err)
	}
	if c.About != "new" || c.PictureID != "2" || c.PictureURL != "" || c.VerifiedName != "Maria Ltd" || c.OnWhatsApp != 1 || c.LID != marialid {
		t.Fatalf("cache = %+v", c)
	}
	if !c.InfoAt.Equal(at) || c.BusinessAt != (time.Time{}) {
		t.Fatalf("times = %v %v", c.InfoAt, c.BusinessAt)
	}
	if _, err := s.CachedProfileOf(john); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing row: %v", err)
	}
}

func TestContactCachePruneAndClear(t *testing.T) {
	s := openStore(t)
	for _, jid := range []string{maria, john, ana} {
		if err := s.PutCachedProfile(CachedProfile{JID: jid, OnWhatsApp: -1}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`UPDATE wame_contact_cache SET updated_at = ? WHERE jid = ?`, time.Now().Add(-100*24*time.Hour).UnixMilli(), ana); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneContactCache(time.Now().Add(-90 * 24 * time.Hour)); err != nil || n != 1 {
		t.Fatalf("prune = %d, %v", n, err)
	}
	if _, err := s.SetContact(ContactEdit{Keys: []string{maria}, Set: OwnContact{Name: "Maria"}, Fields: []string{FieldName}, Actor: "ops"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveGroups([]Group{{JID: family, Name: "Family", Members: []GroupMember{{JID: maria}}}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Save(Message{ID: "K1", Chat: maria, Sender: maria, Type: "text", Text: "kept", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddOptOut(OptOut{Chat: john, Reason: "stop", At: time.Now(), By: "ops"}); err != nil {
		t.Fatal(err)
	}
	gen := s.ContactsGen()
	if n, err := s.ClearContactState(); err != nil || n != 1 {
		t.Fatalf("clear = %d, %v", n, err)
	}
	for _, table := range []string{"wame_contacts", "wame_contact_cache", "wame_group_members", "wame_groups"} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s has %d rows (%v)", table, n, err)
		}
	}
	var msgs, optouts int
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM wame_messages), (SELECT COUNT(*) FROM wame_optouts)`).Scan(&msgs, &optouts); err != nil || msgs != 1 || optouts != 1 {
		t.Errorf("messages %d opt-outs %d should stay (%v)", msgs, optouts, err)
	}
	if s.ContactsGen() == gen {
		t.Error("clearing must bump ContactsGen")
	}
}

func TestContactCacheRelink(t *testing.T) {
	s := openStore(t)
	if _, err := s.db.Exec(`CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT UNIQUE NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := s.PutCachedProfile(CachedProfile{JID: marialid, About: "from lid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO whatsmeow_lid_map VALUES ('100000000000001', '5511999999999')`); err != nil {
		t.Fatal(err)
	}
	c, err := s.CachedProfileOf(maria)
	if err != nil || c.About != "from lid" {
		t.Fatalf("relinked row = %+v, %v", c, err)
	}
	if _, err := s.CachedProfileOf(marialid); !errors.Is(err, ErrNotFound) {
		t.Fatal("the @lid row should be gone")
	}
}

func TestGroupMembers(t *testing.T) {
	s := openStore(t)
	save := func(groups ...Group) {
		t.Helper()
		if err := s.SaveGroups(groups); err != nil {
			t.Fatal(err)
		}
	}
	save(Group{JID: family, Name: "Family", Admins: []string{maria}, Members: []GroupMember{
		{JID: marialid, Role: RoleSuperAdmin}, {JID: john}}},
		Group{JID: work, Name: "Work", Members: []GroupMember{{JID: maria, Role: RoleAdmin}}})
	var first int64
	if err := s.db.QueryRow(`SELECT first_seen FROM wame_group_members WHERE group_jid = ? AND member = ?`, family, marialid).Scan(&first); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	save(Group{JID: family, Name: "Family", Members: []GroupMember{{JID: marialid, Role: RoleMember}, {JID: ana}}},
		Group{JID: work, Name: "Work"})

	rows := map[string]string{}
	r, err := s.db.Query(`SELECT group_jid || ' ' || member, role FROM wame_group_members`)
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
		var k, role string
		if err := r.Scan(&k, &role); err != nil {
			t.Fatal(err)
		}
		rows[k] = role
	}
	r.Close()
	want := map[string]string{family + " " + marialid: RoleMember, family + " " + ana: RoleMember, work + " " + maria: RoleAdmin}
	if len(rows) != len(want) {
		t.Fatalf("members = %v, want %v", rows, want)
	}
	for k, v := range want {
		if rows[k] != v {
			t.Errorf("%s = %q, want %q", k, rows[k], v)
		}
	}
	var first2, last int64
	if err := s.db.QueryRow(`SELECT first_seen, last_seen FROM wame_group_members WHERE group_jid = ? AND member = ?`, family, marialid).Scan(&first2, &last); err != nil {
		t.Fatal(err)
	}
	if first2 != first || last <= first {
		t.Errorf("first_seen %d → %d, last_seen %d", first, first2, last)
	}

	groups, err := s.GroupsWith(maria, marialid)
	if err != nil || len(groups) != 2 || groups[0].Name != "Family" || groups[1].Name != "Work" {
		t.Fatalf("GroupsWith = %+v, %v", groups, err)
	}
	if groups, _ := s.GroupsWith(john); len(groups) != 0 {
		t.Fatalf("john left: %+v", groups)
	}
	all, err := s.Groups()
	if err != nil || len(all) != 2 {
		t.Fatalf("Groups = %+v, %v", all, err)
	}
}

func TestContactStats(t *testing.T) {
	s := openStore(t)
	anaAlt := "5521987654321@s.whatsapp.net" // 9th-digit variant
	base := time.Now().Add(-48 * time.Hour).Truncate(time.Millisecond)
	for i, m := range []Message{
		{ID: "A1", Chat: ana, Sender: ana, Type: "text", Text: "oi"},
		{ID: "A2", Chat: ana, Sender: ana, FromMe: true, Type: "text", Text: "olá"},
		{ID: "A3", Chat: anaAlt, Sender: anaAlt, Type: "text", Text: "old number"},
		{ID: "A4", Chat: marialid, Sender: marialid, Type: "text", Text: "via lid"},
		{ID: "A5", Chat: ana, Sender: ana, Type: "reaction", Text: "👍"},
		{ID: "A6", Chat: john, Sender: john, Type: "text", Text: "not ana"},
		{ID: "A7", Chat: family, Sender: ana, IsGroup: true, Type: "text", Text: "in a group"},
	} {
		m.Timestamp = base.Add(time.Duration(i) * time.Minute)
		if _, _, err := s.Save(m); err != nil {
			t.Fatal(err)
		}
	}
	s.MarkReadUpTo(ana, 1)
	if err := s.SaveGroups([]Group{{JID: family, Name: "Family", Members: []GroupMember{{JID: marialid}}}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	old := time.Now().Add(-8 * 24 * time.Hour).UnixMilli()
	for _, row := range []struct {
		to, chat, status string
		updated          int64
		delivered, read  int64
	}{
		{"552187654321", ana, OutboxSent, now, now, now},
		{"+552187654321", "", OutboxQueued, old, 0, 0},
		{ana, ana, OutboxFailed, now, 0, 0},
		{"552187654321", ana, OutboxSent, old, now, 0},
		{"5511988887777", john, OutboxSent, now, 0, 0},
	} {
		if _, err := s.db.Exec(`INSERT INTO wame_outbox (batch, actor, recipient, text, status, due, created_at, updated_at, chat, delivered_at, read_at)
			VALUES ('b', 'ops', ?, 'Hi', ?, 0, 0, ?, ?, ?, ?)`, row.to, row.status, row.updated, row.chat, row.delivered, row.read); err != nil {
			t.Fatal(err)
		}
	}
	st, err := s.ContactStats([]string{ana, marialid, anaAlt})
	if err != nil {
		t.Fatal(err)
	}
	if st.Sent != 1 || st.Received != 3 || st.Unread != 2 {
		t.Errorf("sent %d received %d unread %d", st.Sent, st.Received, st.Unread)
	}
	if !st.FirstAt.Equal(base) || !st.LastAt.Equal(base.Add(3*time.Minute)) {
		t.Errorf("first %v last %v", st.FirstAt, st.LastAt)
	}
	if len(st.Groups) != 1 || st.Groups[0].Name != "Family" {
		t.Errorf("groups = %+v", st.Groups)
	}
	q := st.Queue
	if q.Total != 3 || q.Open != 1 || q.Sent != 1 || q.Delivered != 1 || q.Read != 1 || q.Failed != 1 {
		t.Errorf("queue = %+v", q)
	}
	empty, err := s.ContactStats([]string{maria})
	if err != nil || empty.Sent+empty.Received+empty.Queue.Total != 0 || !empty.FirstAt.IsZero() || empty.Groups == nil {
		t.Fatalf("empty = %+v, %v", empty, err)
	}
}

func TestGroupMembersRelink(t *testing.T) {
	s := openStore(t)
	if _, err := s.db.Exec(`CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT UNIQUE NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveGroups([]Group{
		{JID: family, Name: "Family", Members: []GroupMember{{JID: marialid, Role: RoleAdmin}}},
		{JID: work, Name: "Work", Members: []GroupMember{{JID: marialid}, {JID: maria, Role: RoleSuperAdmin}}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE wame_group_members SET first_seen = 5 WHERE group_jid = ? AND member = ?`, work, marialid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO whatsmeow_lid_map VALUES ('100000000000001', '5511999999999')`); err != nil {
		t.Fatal(err)
	}
	groups, err := s.GroupsWith(maria)
	if err != nil || len(groups) != 2 {
		t.Fatalf("after relink maria is in %+v (%v)", groups, err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM wame_group_members WHERE member = ?`, marialid).Scan(&n); err != nil || n != 0 {
		t.Fatalf("@lid rows left: %d (%v)", n, err)
	}
	var first int64
	if err := s.db.QueryRow(`SELECT first_seen FROM wame_group_members WHERE group_jid = ? AND member = ?`, work, maria).Scan(&first); err != nil || first != 5 {
		t.Errorf("merged first_seen = %d (%v), want the older 5", first, err)
	}
	var role string
	if err := s.db.QueryRow(`SELECT role FROM wame_group_members WHERE group_jid = ? AND member = ?`, family, maria).Scan(&role); err != nil || role != RoleAdmin {
		t.Errorf("moved role = %q, %v", role, err)
	}
}

func TestSaveJoinedGroupsDropsGroupsLeft(t *testing.T) {
	s := openStore(t)
	if err := s.SaveJoinedGroups([]Group{
		{JID: family, Name: "Family", Members: []GroupMember{{JID: maria}}},
		{JID: work, Name: "Work", Members: []GroupMember{{JID: john}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveJoinedGroups([]Group{{JID: family, Name: "Family", Members: []GroupMember{{JID: maria}}}}); err != nil {
		t.Fatal(err)
	}
	if g, _ := s.GroupsWith(john); len(g) != 0 {
		t.Fatalf("a group the account left still counts: %+v", g)
	}
	if all, _ := s.Groups(); len(all) != 2 {
		t.Fatal("group names stay for the messages")
	}
	if err := s.SaveGroups([]Group{{JID: work, Name: "Work", Members: []GroupMember{{JID: john}}}}); err != nil {
		t.Fatal(err)
	}
	if g, _ := s.GroupsWith(maria); len(g) != 1 {
		t.Fatal("SaveGroups with some groups must leave the others alone")
	}
}

func TestPutCachedProfileReplacesOtherKeys(t *testing.T) {
	s := openStore(t)
	alt := "5521987654321@s.whatsapp.net"
	if err := s.PutCachedProfile(CachedProfile{JID: ana, OnWhatsApp: 0}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutCachedProfile(CachedProfile{JID: alt, OnWhatsApp: 1}, ana, alt); err != nil {
		t.Fatal(err)
	}
	if c, err := s.CachedProfileOf(ana, alt); err != nil || c.JID != alt || c.OnWhatsApp != 1 {
		t.Fatalf("read = %+v, %v", c, err)
	}
}
