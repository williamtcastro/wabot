package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// legacify turns a fresh database into the pre-rename layout: the same schema with wabot_ names,
// which is exactly what older versions created.
func legacify(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	rows, err := db.QueryContext(ctx, `SELECT type, name, sql FROM sqlite_master
		WHERE name LIKE 'wame\_%' ESCAPE '\' AND sql IS NOT NULL
		AND NOT (type = 'table' AND name LIKE 'wame\_messages\_fts\_%' ESCAPE '\')`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	var recreate []string
	for rows.Next() {
		var typ, name, ddl string
		if err := rows.Scan(&typ, &name, &ddl); err != nil {
			t.Fatal(err)
		}
		switch {
		case typ == "table" && name != "wame_messages_fts":
			tables = append(tables, name)
		default:
			recreate = append(recreate, typ+"\x00"+name+"\x00"+ddl)
		}
	}
	rows.Close()
	exec := func(q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, r := range recreate {
		f := strings.SplitN(r, "\x00", 3)
		if f[0] != "table" {
			exec("DROP " + strings.ToUpper(f[0]) + " " + f[1])
		}
	}
	exec("DROP TABLE wame_messages_fts")
	for _, name := range tables {
		exec("ALTER TABLE " + name + " RENAME TO wabot_" + strings.TrimPrefix(name, "wame_"))
	}
	for _, r := range recreate {
		f := strings.SplitN(r, "\x00", 3)
		exec(strings.ReplaceAll(f[2], "wame_", "wabot_"))
	}
	exec("INSERT INTO wabot_messages_fts(wabot_messages_fts) VALUES ('rebuild')")
}

func countPrefix(t *testing.T, db *sql.DB, prefix string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE ? ESCAPE '\'`, strings.ReplaceAll(prefix, "_", `\_`)+"%").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestOldTablesAreRenamed(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	s, err := Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	for i, m := range []Message{
		{ID: "M1", Chat: maria, Sender: maria, PushName: "Maria", Type: "text", Text: "o boleto chegou"},
		{ID: "M2", Chat: john, Sender: john, PushName: "John", Type: "text", Text: "hello"},
		{ID: "M3", Chat: maria, Sender: maria, PushName: "Maria", Type: "text", Text: "e a fatura também"},
	} {
		m.Timestamp = base.Add(time.Duration(i) * time.Minute)
		if _, _, err := s.Save(m); err != nil {
			t.Fatal(err)
		}
	}
	s.MarkChatRead(john)
	if err := s.InsertToken(Token{ID: "abcdefgh", Name: "claude", Hash: []byte("h"), Scopes: []string{"read"}, Created: base}); err != nil {
		t.Fatal(err)
	}
	if err := s.Audit(AuditEntry{At: base, Actor: "api", Action: "send", OK: true}); err != nil {
		t.Fatal(err)
	}
	v := "5"
	if err := s.SetSettings(map[string]*string{"rate": &v}, "ops"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveGroups([]Group{{JID: group, Name: "Release crew"}}); err != nil {
		t.Fatal(err)
	}
	legacify(t, db)
	if countPrefix(t, db, "wame_") != 0 || countPrefix(t, db, "wabot_") == 0 {
		t.Fatal("legacify didn't produce an old layout")
	}

	for range 2 {
		if s, err = Open(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	if n := countPrefix(t, db, "wabot_"); n != 0 {
		t.Fatalf("%d wabot_ objects left", n)
	}
	if got := query(t, s, Filter{Text: "boleto"}, 10, false); len(got) != 1 || got[0].ID != "M1" {
		t.Fatalf("search after the rename = %+v", got)
	}
	if got := query(t, s, Filter{}, 10, false); len(got) != 3 {
		t.Fatalf("messages = %d", len(got))
	}
	chats, err := s.Chats(ctx, 10, func(_, push string) string { return push })
	if err != nil || len(chats) != 2 {
		t.Fatalf("chats = %+v, %v", chats, err)
	}
	for _, c := range chats {
		if want := map[string]int{maria: 2, john: 0}[c.JID]; c.Unread != want {
			t.Errorf("%s unread = %d, want %d (read positions lost?)", c.JID, c.Unread, want)
		}
	}
	if tok, err := s.TokenByID("abcdefgh"); err != nil || tok.Name != "claude" {
		t.Fatalf("token = %+v, %v", tok, err)
	}
	if log, err := s.AuditLog(10, 0); err != nil || len(log) != 1 {
		t.Fatalf("audit = %+v, %v", log, err)
	}
	if st, err := s.Settings(); err != nil || st["rate"].Value != "5" {
		t.Fatalf("settings = %+v, %v", st, err)
	}
	if g := must[[]Group](t)(s.Groups()); len(g) != 1 {
		t.Fatalf("groups = %+v", g)
	}
	if _, _, err := s.Save(Message{ID: "M4", Chat: john, Sender: john, Type: "text", Text: "fatura nova", Timestamp: base.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if got := query(t, s, Filter{Text: "fatura"}, 10, false); len(got) != 2 {
		t.Fatalf("the search index stopped following new messages: %+v", got)
	}
	var ok string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&ok); err != nil || ok != "ok" {
		t.Fatalf("integrity = %q, %v", ok, err)
	}
}

func TestRenameRefusesTwoCopies(t *testing.T) {
	db := openDB(t)
	if _, err := Open(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE wabot_audit (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), db); err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("err = %v", err)
	}
}
