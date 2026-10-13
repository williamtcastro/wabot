package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"slices"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

const (
	maria = "5511999999999@s.whatsapp.net"
	john  = "5511988887777@s.whatsapp.net"
	ana   = "552187654321@s.whatsapp.net"
	group = "120363000000000001@g.us"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "test.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), openDB(t))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func seqs(msgs []Message) []int64 {
	out := []int64{}
	for _, m := range msgs {
		out = append(out, m.Seq)
	}
	return out
}

func TestMigrateFromV1KeepsRowids(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	_, err := db.Exec(`CREATE TABLE wame_messages (
		chat TEXT NOT NULL, id TEXT NOT NULL, sender TEXT NOT NULL, push_name TEXT NOT NULL DEFAULT '',
		from_me INTEGER NOT NULL, is_group INTEGER NOT NULL, type TEXT NOT NULL, text TEXT NOT NULL,
		ts INTEGER NOT NULL, UNIQUE (chat, id));
		CREATE INDEX wame_messages_ts ON wame_messages (chat, ts);
		INSERT INTO wame_messages VALUES
			('` + maria + `', 'A1', '` + maria + `', 'Maria', 0, 0, 'text', 'oi', 1000),
			('` + john + `', 'A2', '` + john + `', 'John', 0, 0, 'text', 'hello', 2000),
			('` + ana + `', 'A3', '` + ana + `', 'Ana', 0, 0, 'text', 'olá', 3000);
		DELETE FROM wame_messages WHERE id = 'A2';`)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	s := &Store{db: db}
	got := s.Query(Filter{}, 10, true)
	if !slices.Equal(seqs(got), []int64{1, 3}) {
		t.Fatalf("seqs after upgrade = %v, want [1 3]", seqs(got))
	}
	if got[1].ID != "A3" || got[1].Text != "olá" || got[1].PushName != "Ana" {
		t.Fatalf("row 3 = %+v", got[1])
	}
	m, inserted, err := s.Save(Message{ID: "A4", Chat: maria, Sender: maria, Type: "text", Text: "new", Timestamp: time.UnixMilli(4000)})
	if err != nil || !inserted || m.Seq != 4 {
		t.Fatalf("save after upgrade: seq %d inserted %v err %v", m.Seq, inserted, err)
	}
	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if n := len(s.Query(Filter{}, 10, true)); n != 3 {
		t.Fatalf("rows after re-migrate = %d, want 3", n)
	}
}

func TestMigrateAddsMissingColumns(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	if _, err := db.Exec(`CREATE TABLE wame_messages (seq INTEGER PRIMARY KEY AUTOINCREMENT, chat TEXT NOT NULL,
		id TEXT NOT NULL, sender TEXT NOT NULL, push_name TEXT NOT NULL DEFAULT '', from_me INTEGER NOT NULL,
		is_group INTEGER NOT NULL, type TEXT NOT NULL, text TEXT NOT NULL, ts INTEGER NOT NULL, UNIQUE (chat, id))`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('wame_messages') WHERE name IN ('raw_sender', 'deleted', 'file_size')`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("added columns = %d (%v), want 3", n, err)
	}
}

func fixtures(t *testing.T, s *Store) []Message {
	t.Helper()
	base := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	in := []Message{
		{ID: "M1", Chat: maria, Sender: maria, PushName: "Maria", Type: "text", Text: "oi"},
		{ID: "M2", Chat: maria, Sender: john, FromMe: true, Type: "text", Text: "hi Maria"},
		{ID: "M3", Chat: group, Sender: maria, PushName: "Maria", IsGroup: true, Type: "image", Media: &Media{Mimetype: "image/jpeg", Size: 10}},
		{ID: "M4", Chat: group, Sender: ana, PushName: "Ana", IsGroup: true, Type: "reaction", Text: "👍", ReplyTo: "M3"},
		{ID: "M5", Chat: ana, Sender: ana, PushName: "Ana", Type: "document", Text: "a.pdf", Media: &Media{Mimetype: "application/pdf", FileName: "a.pdf"}},
		{ID: "M6", Chat: john, Sender: john, FromMe: true, Type: "text", Text: "note to self"},
	}
	var out []Message
	for i, m := range in {
		m.Timestamp = base.Add(time.Duration(i) * time.Minute)
		saved, inserted, err := s.Save(m)
		if err != nil || !inserted {
			t.Fatalf("save %s: %v", m.ID, err)
		}
		out = append(out, saved)
	}
	return out
}

func TestFilterSQLAgreesWithMatch(t *testing.T) {
	s := openStore(t)
	all := fixtures(t, s)
	yes, no := true, false
	filters := []Filter{
		{},
		{Chat: maria},
		{Chat: maria, ChatOnly: true},
		{Chat: group},
		{Types: []string{"text"}},
		{Types: []string{"image", "document"}},
		{FromMe: &yes},
		{FromMe: &no, IsGroup: &no},
		{IsGroup: &yes},
		{Since: 3},
		{Chat: ana, Types: []string{"reaction"}},
		{Chat: john, FromMe: &yes, Since: 1},
	}
	for _, f := range filters {
		var want []int64
		for _, m := range all {
			if f.Match(m) {
				want = append(want, m.Seq)
			}
		}
		got := seqs(s.Query(f, 100, true))
		if !slices.Equal(got, append([]int64{}, want...)) {
			t.Errorf("filter %+v: SQL %v, Match %v", f, got, want)
		}
	}
}

func TestChatOnly(t *testing.T) {
	s := openStore(t)
	fixtures(t, s)
	withSender := seqs(s.Query(Filter{Chat: maria}, 100, true))
	if !slices.Equal(withSender, []int64{1, 2, 3}) {
		t.Fatalf("chat=maria = %v, want [1 2 3] (incl. her group message)", withSender)
	}
	only := seqs(s.Messages(maria, 100))
	if !slices.Equal(only, []int64{2, 1}) {
		t.Fatalf("Messages(maria) = %v, want [2 1] (newest first, no group)", only)
	}
}

func TestParseFilter(t *testing.T) {
	q, _ := url.ParseQuery("chat=5511999999999&type=text,image&from_me=false&since=7")
	f, err := ParseFilter(q, func(s string) (string, error) { return s + "@s.whatsapp.net", nil })
	if err != nil {
		t.Fatal(err)
	}
	if f.Chat != maria || !slices.Equal(f.Types, []string{"text", "image"}) || f.FromMe == nil || *f.FromMe || f.Since != 7 {
		t.Fatalf("got %+v", f)
	}
	for _, bad := range []string{"type=gif", "from_me=maybe", "since=-1", "since=x"} {
		q, _ := url.ParseQuery(bad)
		if _, err := ParseFilter(q, nil); err == nil {
			t.Errorf("%s: want error", bad)
		}
	}
}

func TestSaveEditDeleteImport(t *testing.T) {
	s := openStore(t)
	all := fixtures(t, s)
	if _, inserted, _ := s.Save(all[0]); inserted {
		t.Fatal("duplicate (chat, id) was inserted again")
	}
	if all[2].Media.Path != "/media/3" {
		t.Fatalf("media path = %q", all[2].Media.Path)
	}
	if err := s.MarkEdited(maria, "M1", "oi!"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDeleted(ana, "M5"); err != nil {
		t.Fatal(err)
	}
	m, _ := s.Get(maria, "M1")
	if !m.Edited || m.Text != "oi!" {
		t.Fatalf("edited = %+v", m)
	}
	m, _ = s.GetBySeq(5)
	if !m.Deleted || m.Text != "" || m.Raw != nil {
		t.Fatalf("deleted = %+v", m)
	}
	if _, err := s.Get(maria, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing message err = %v", err)
	}

	err := s.Import([]Message{
		{ID: "M2", Chat: maria, Sender: john, FromMe: true, Type: "text", Text: "hi Maria (edited)", Edited: true},
		{ID: "H1", Chat: ana, Sender: ana, Type: "text", Text: "from history", Timestamp: time.UnixMilli(1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	m, _ = s.Get(maria, "M2")
	if !m.Edited || m.Text != "hi Maria (edited)" || m.Seq != 2 {
		t.Fatalf("history edit = %+v", m)
	}
	h, _ := s.Get(ana, "H1")
	if h.Seq <= 6 || s.LastSeq() != h.Seq {
		t.Fatalf("history row seq %d, LastSeq %d", h.Seq, s.LastSeq())
	}
}

func TestChatsAndAudit(t *testing.T) {
	s := openStore(t)
	fixtures(t, s)
	chats, err := s.Chats(context.Background(), 10, func(chat, push string) string { return push })
	if err != nil {
		t.Fatal(err)
	}
	var jids []string
	for _, c := range chats {
		jids = append(jids, c.JID)
	}
	if !slices.Equal(jids, []string{john, ana, group, maria}) {
		t.Fatalf("chats = %v", jids)
	}
	if chats[1].Name != "Ana" || chats[2].LastType != "image" {
		t.Fatalf("chat rows = %+v", chats)
	}

	for i, action := range []string{"send", "react", "edit"} {
		e := AuditEntry{At: time.Now(), Actor: "api", Action: action, Target: maria, OK: i != 1}
		if err := s.Audit(e); err != nil {
			t.Fatal(err)
		}
	}
	log, err := s.AuditLog(2, 0)
	if err != nil || len(log) != 2 || log[0].Action != "edit" || log[1].OK {
		t.Fatalf("audit log = %+v, %v", log, err)
	}
	older, _ := s.AuditLog(10, log[1].ID)
	if len(older) != 1 || older[0].Action != "send" {
		t.Fatalf("before cursor = %+v", older)
	}
}
