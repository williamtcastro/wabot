package store

import (
	"context"
	"testing"
	"time"
)

func TestSearch(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	for i, m := range []Message{
		{ID: "S1", Chat: maria, Sender: maria, Type: "text", Text: "vamos no café amanhã?"},
		{ID: "S2", Chat: john, Sender: john, Type: "text", Text: "Bom dia, tudo bem?"},
		{ID: "S3", Chat: maria, Sender: john, FromMe: true, Type: "text", Text: "bom demais"},
		{ID: "S4", Chat: ana, Sender: ana, Type: "text", Text: "cafeteria nova"},
	} {
		m.Timestamp = base.Add(time.Duration(i) * time.Minute)
		if _, _, err := s.Save(m); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(f Filter) []string {
		var out []string
		for _, m := range query(t, s, f, 10, true) {
			out = append(out, m.ID)
		}
		return out
	}
	cases := []struct {
		f    Filter
		want []string
	}{
		{Filter{Text: "cafe"}, []string{"S1", "S4"}},
		{Filter{Text: "AMANHA"}, []string{"S1"}},
		{Filter{Text: "bom di"}, []string{"S2"}},
		{Filter{Text: "bom"}, []string{"S2", "S3"}},
		{Filter{Text: "bom", Chat: maria, ChatOnly: true}, []string{"S3"}},
		{Filter{Text: `"; DROP TABLE x --`}, nil},
	}
	for _, c := range cases {
		got := ids(c.f)
		if len(got) != len(c.want) || (len(got) > 0 && got[0] != c.want[0]) {
			t.Errorf("Query(%+v) = %v, want %v", c.f, got, c.want)
		}
		for _, m := range query(t, s, Filter{}, 10, true) {
			inSQL := false
			for _, id := range got {
				inSQL = inSQL || id == m.ID
			}
			if c.f.Match(m) != inSQL {
				t.Errorf("Match(%+v) disagrees with SQL for %s %q", c.f, m.ID, m.Text)
			}
		}
	}

	if err := s.MarkEdited(maria, "S1", "vamos no cinema"); err != nil {
		t.Fatal(err)
	}
	if got := ids(Filter{Text: "cafe"}); len(got) != 1 || got[0] != "S4" {
		t.Errorf("edited text should be reindexed, got %v", got)
	}
	if got := ids(Filter{Text: "cinema"}); len(got) != 1 {
		t.Errorf("new text should be searchable, got %v", got)
	}
	if err := s.MarkDeleted(ana, "S4"); err != nil {
		t.Fatal(err)
	}
	if got := ids(Filter{Text: "cafeteria"}); len(got) != 0 {
		t.Errorf("deleted message should not be found, got %v", got)
	}
}

func unreadOf(t *testing.T, s *Store, chat string) int {
	t.Helper()
	chats, err := s.Chats(context.Background(), 50, func(c, _ string) string { return c })
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chats {
		if c.JID == chat {
			return c.Unread
		}
	}
	return -1
}

func TestUnreadCounts(t *testing.T) {
	s := openStore(t)
	now := time.Now()
	save := func(id string, fromMe bool, typ string) Message {
		m, _, err := s.Save(Message{ID: id, Chat: maria, Sender: maria, FromMe: fromMe, Type: typ, Text: id, Timestamp: now})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	save("U1", false, "text")
	save("U2", false, "text")
	save("U3", false, "reaction")
	if n := unreadOf(t, s, maria); n != 2 {
		t.Fatalf("unread = %d, want 2 (reactions don't count)", n)
	}
	mine := save("U4", true, "text")
	if n := unreadOf(t, s, maria); n != 0 {
		t.Fatalf("sending in a chat should mark it read, unread = %d", n)
	}
	save("U5", false, "text")
	if s.MarkReadUpTo(maria, mine.Seq) {
		t.Fatal("MarkReadUpTo must never move backwards")
	}
	if n := unreadOf(t, s, maria); n != 1 {
		t.Fatalf("unread = %d, want 1", n)
	}
	if !s.MarkChatRead(maria) || unreadOf(t, s, maria) != 0 {
		t.Fatal("MarkChatRead should clear unread")
	}
}

func TestExistingHistoryStartsRead(t *testing.T) {
	db := openDB(t)
	s, err := Open(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	fixtures(t, s)
	if _, err := db.Exec(`DROP TABLE wame_chat_state`); err != nil {
		t.Fatal(err)
	}
	s, err = Open(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	for _, chat := range []string{maria, ana, group} {
		if n := unreadOf(t, s, chat); n != 0 {
			t.Errorf("%s: messages stored before the upgrade should start read, unread = %d", chat, n)
		}
	}
}

func TestGroupsCache(t *testing.T) {
	s := openStore(t)
	in := []Group{{JID: group, Name: "Family", Admins: []string{maria}}}
	if err := s.SaveGroups(in); err != nil {
		t.Fatal(err)
	}
	in[0].Name = "Família"
	if err := s.SaveGroups(in); err != nil {
		t.Fatal(err)
	}
	got := must[[]Group](t)(s.Groups())
	if len(got) != 1 || got[0].Name != "Família" || len(got[0].Admins) != 1 || got[0].Admins[0] != maria {
		t.Fatalf("Groups() = %+v", got)
	}
}

func TestImportOnlyMarksImportedChatsRead(t *testing.T) {
	s := openStore(t)
	now := time.Now()
	if _, _, err := s.Save(Message{ID: "L1", Chat: maria, Sender: maria, Type: "text", Text: "live", Timestamp: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.Import([]Message{{ID: "H1", Chat: john, Sender: john, Type: "text", Text: "history", Timestamp: now.Add(-time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	if n := unreadOf(t, s, maria); n != 1 {
		t.Fatalf("a history sync for John must not mark Maria read, unread = %d", n)
	}
	if n := unreadOf(t, s, john); n != 0 {
		t.Fatalf("imported history should count as read, unread = %d", n)
	}
}
