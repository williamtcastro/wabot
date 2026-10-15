package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

var bigQuota = OutboxFileQuota{ActorFiles: 50, ActorBytes: 500 << 20, TotalBytes: 2 << 30}

func testFile(id, actor, sha string, size int64) OutboxFile {
	return OutboxFile{ID: id, Actor: actor, SHA256: sha, Size: size, Mime: "application/pdf", Kind: FileDocument, Name: "menu.pdf"}
}

func TestOutboxFileDedupePerActor(t *testing.T) {
	s := openStore(t)
	a, existed, err := s.InsertOutboxFile(testFile("f_1", "promo-bot", "abc", 10), bigQuota)
	if err != nil || existed || a.ID != "f_1" {
		t.Fatalf("%v %v %+v", err, existed, a)
	}
	again, existed, err := s.InsertOutboxFile(testFile("f_2", "promo-bot", "abc", 10), bigQuota)
	if err != nil || !existed || again.ID != "f_1" {
		t.Fatalf("same bytes, same actor must return the first: %v %v %+v", err, existed, again)
	}
	other, existed, err := s.InsertOutboxFile(testFile("f_3", "maria-bot", "abc", 10), bigQuota)
	if err != nil || existed || other.ID != "f_3" {
		t.Fatalf("another actor gets its own row: %v %v %+v", err, existed, other)
	}
	if err := s.DeleteOutboxFile("f_1", "promo-bot"); err != nil {
		t.Fatal(err)
	}
	if f, existed, err := s.InsertOutboxFile(testFile("f_4", "promo-bot", "abc", 10), bigQuota); err != nil || existed || f.ID != "f_4" {
		t.Fatalf("a deleted file doesn't count for dedupe: %v %v %+v", err, existed, f)
	}
	if f, _ := s.OutboxFile("f_1"); f.Deleted.IsZero() || f.Name != "" || f.Size != 10 || f.SHA256 != "abc" {
		t.Fatalf("a deleted row keeps size and hash only: %+v", f)
	}
}

func TestOutboxFileQuota(t *testing.T) {
	s := openStore(t)
	q := OutboxFileQuota{ActorFiles: 2, ActorBytes: 100, TotalBytes: 150}
	if _, _, err := s.InsertOutboxFile(testFile("f_1", "promo-bot", "a", 60), q); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.InsertOutboxFile(testFile("f_2", "promo-bot", "b", 41), q); !errors.Is(err, ErrOutboxFileQuota) {
		t.Fatalf("over the actor's bytes: %v", err)
	}
	if _, _, err := s.InsertOutboxFile(testFile("f_3", "promo-bot", "c", 40), q); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.InsertOutboxFile(testFile("f_4", "promo-bot", "d", 1), q); !errors.Is(err, ErrOutboxFileQuota) {
		t.Fatalf("over the actor's file count: %v", err)
	}
	if _, _, err := s.InsertOutboxFile(testFile("f_5", "maria-bot", "e", 51), q); !errors.Is(err, ErrOutboxFileQuota) {
		t.Fatalf("over the total: %v", err)
	}

	// Parallel uploads can't both squeeze under the quota.
	s2 := openStore(t)
	q = OutboxFileQuota{ActorFiles: 50, ActorBytes: 1000, TotalBytes: 100}
	var wg sync.WaitGroup
	var mu sync.Mutex
	stored := 0
	for i := range 8 {
		wg.Go(func() {
			_, _, err := s2.InsertOutboxFile(testFile("f_p"+string(rune('a'+i)), "promo-bot", string(rune('a'+i)), 30), q)
			if err == nil {
				mu.Lock()
				stored++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if u, _ := s2.OutboxFileUsage(""); stored != 3 || u.Bytes != 90 {
		t.Fatalf("stored %d, usage %+v", stored, u)
	}
}

func TestOutboxFileTriggerAndDelete(t *testing.T) {
	s := openStore(t)
	if _, _, err := s.InsertOutboxFile(testFile("f_1", "promo-bot", "a", 10), bigQuota); err != nil {
		t.Fatal(err)
	}
	item := OutboxItem{Batch: "menu", Actor: "promo-bot", To: "5511999999999", Key: "k#0", FileID: "f_1", FileKind: FileDocument, FileSize: 10}
	items, err := s.Enqueue([]OutboxItem{item}, nil)
	if err != nil || items[0].FileID != "f_1" || items[0].FileKind != FileDocument || items[0].FileSize != 10 {
		t.Fatalf("%v %+v", err, items)
	}
	if _, err := s.Enqueue([]OutboxItem{{Batch: "x", Actor: "maria-bot", To: "5511988887777", FileID: "f_1"}}, nil); !IsOutboxFileGone(err) {
		t.Fatalf("another actor's file must be refused by the database too: %v", err)
	}
	if err := s.DeleteOutboxFile("f_1", "maria-bot"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("someone else's file: %v", err)
	}
	if err := s.DeleteOutboxFile("f_1", "promo-bot"); !errors.Is(err, ErrOutboxFileInUse) {
		t.Fatalf("a file a queued item needs: %v", err)
	}
	if _, err := s.CancelOutbox(items[0].ID, OutboxFilter{}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteOutboxFile("f_1", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteOutboxFile("f_1", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("twice: %v", err)
	}
	if _, err := s.Enqueue([]OutboxItem{{Batch: "menu", Actor: "promo-bot", To: "5511988887777", FileID: "f_1"}}, nil); !IsOutboxFileGone(err) {
		t.Fatalf("a deleted file can't be queued: %v", err)
	}
	if again, err := s.Enqueue([]OutboxItem{item}, nil); err != nil || again[0].ID != items[0].ID {
		t.Fatalf("a replay of the same key still answers with the stored item: %v %+v", err, again)
	}
	if n, _ := s.PruneOutboxFiles(); n != 0 {
		t.Fatal("the row stays while an item refers to it")
	}
	if _, err := s.PruneOutbox(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.PruneOutboxFiles(); n != 1 {
		t.Fatalf("pruned %d rows", n)
	}
}

func TestExpireOutboxFiles(t *testing.T) {
	s := openStore(t)
	for _, f := range []OutboxFile{testFile("f_unused", "a", "1", 1), testFile("f_open", "a", "2", 1), testFile("f_done", "a", "3", 1), testFile("f_recent", "a", "4", 1)} {
		if _, _, err := s.InsertOutboxFile(f, bigQuota); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.Enqueue([]OutboxItem{
		{Batch: "b", Actor: "a", To: "5511999999999", FileID: "f_open"},
		{Batch: "b", Actor: "a", To: "5511999999999", FileID: "f_done"},
		{Batch: "b", Actor: "a", To: "5511999999999", FileID: "f_recent"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items[1:] {
		if _, err := s.CancelOutbox(it.ID, OutboxFilter{}); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour).UnixMilli()
	if _, err := s.db.Exec(`UPDATE wame_outbox_files SET created_at = ?`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE wame_outbox SET updated_at = ? WHERE file_id IN ('f_open', 'f_done')`, old); err != nil {
		t.Fatal(err)
	}
	ids, err := s.ExpireOutboxFiles(time.Now().Add(-time.Hour), time.Now().Add(-24*time.Hour))
	if err != nil || len(ids) != 2 || !(ids[0] == "f_unused" || ids[1] == "f_unused") || !(ids[0] == "f_done" || ids[1] == "f_done") {
		t.Fatalf("expired %v %v", ids, err)
	}
	if live, _ := s.LiveOutboxFiles(); len(live) != 2 {
		t.Fatalf("live %v", live)
	}
}

func TestOutboxFileMigrationKeepsOldItems(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	if _, err := db.Exec(`CREATE TABLE wame_outbox (
		id INTEGER PRIMARY KEY AUTOINCREMENT, batch TEXT NOT NULL, actor TEXT NOT NULL, token_id TEXT NOT NULL DEFAULT '',
		key TEXT NOT NULL DEFAULT '', recipient TEXT NOT NULL, text TEXT NOT NULL, reply_to TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, due INTEGER NOT NULL, created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL, chat TEXT NOT NULL DEFAULT '', message_id TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '');
		INSERT INTO wame_outbox (batch, actor, recipient, text, status, due, created_at, updated_at)
		VALUES ('old', 'ops', '5511999999999', 'Hi Maria', 'queued', 1, 1, 1);`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		s, err := Open(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		items, err := s.ListOutbox(OutboxFilter{}, 10)
		if err != nil || len(items) != 1 || items[0].FileID != "" || items[0].Text != "Hi Maria" {
			t.Fatalf("%v %+v", err, items)
		}
	}
}
