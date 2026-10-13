package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestOutboxLifecycle(t *testing.T) {
	s := openStore(t)
	later := time.Now().Add(time.Hour)
	items, err := s.Enqueue([]OutboxItem{
		{Batch: "promo", Actor: "ops", To: "5511999999999", Text: "Hi Maria", Due: later},
		{Batch: "promo", Actor: "ops", To: "5511988887777", Text: "Hi John"},
	}, nil)
	if err != nil || len(items) != 2 {
		t.Fatal(err)
	}
	due, _ := s.DueOutbox(time.Now(), 10)
	if len(due) != 1 || due[0].ID != items[1].ID {
		t.Fatalf("only the unscheduled one is due: %+v", due)
	}
	if next, _ := s.NextOutboxDue(); !next.Equal(time.UnixMilli(items[1].Due.UnixMilli())) {
		t.Fatalf("next due %v", next)
	}

	it, err := s.ClaimOutbox(items[1].ID, "MSG1")
	if err != nil || it.Status != OutboxSending || it.Attempts != 1 || it.MessageID != "MSG1" {
		t.Fatalf("%v %+v", err, it)
	}
	if _, err := s.ClaimOutbox(items[1].ID, "MSG2"); !errors.Is(err, ErrNotFound) {
		t.Fatal("claimed twice")
	}
	it, _ = s.FinishOutbox(it.ID, OutboxResult{Status: OutboxQueued, Error: "offline", Refund: true})
	if it.Attempts != 0 || it.Status != OutboxQueued {
		t.Fatalf("refund: %+v", it)
	}
	if it, _ = s.ClaimOutbox(it.ID, "MSG3"); it.MessageID != "MSG1" {
		t.Fatalf("message id must stay: %s", it.MessageID)
	}
	if it, _ = s.FinishOutbox(it.ID, OutboxResult{Status: OutboxSent, Chat: "5511988887777@s.whatsapp.net"}); it.Chat == "" || it.Error != "" {
		t.Fatalf("sent: %+v", it)
	}

	c, _ := s.CountOutbox(OutboxFilter{Batch: "promo"})
	if c.Queued != 1 || c.Sent != 1 || c.OldestQueued.IsZero() {
		t.Fatalf("%+v", c)
	}
	if n, _ := s.PruneOutbox(time.Now().Add(time.Minute)); n != 1 {
		t.Fatalf("pruned %d, want only the sent one", n)
	}
}

func TestOutboxReceipts(t *testing.T) {
	s := openStore(t)
	items, _ := s.Enqueue([]OutboxItem{
		{Batch: "promo", Actor: "ops", To: "5511999999999", Text: "Hi Maria"},
		{Batch: "promo", Actor: "ops", To: "5511988887777", Text: "Hi John"},
	}, nil)
	for i, it := range items {
		if _, err := s.ClaimOutbox(it.ID, fmt.Sprintf("MSG%d", i)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FinishOutbox(it.ID, OutboxResult{Status: OutboxSent}); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Now()
	changed, err := s.MarkOutboxReceipt([]string{"MSG0", "MSG1", "OTHER"}, false, at)
	if err != nil || len(changed) != 2 || changed[0].Progress() != OutboxDelivered || !changed[0].Read.IsZero() {
		t.Fatalf("delivered: %v %+v", err, changed)
	}
	if changed[0].Text != "" || changed[0].To != "5511999999999" {
		t.Fatalf("receipts must not read the text back: %+v", changed[0])
	}
	if again, _ := s.MarkOutboxReceipt([]string{"MSG0"}, false, at); len(again) != 0 {
		t.Fatal("a repeated receipt must not report a change")
	}
	read, _ := s.MarkOutboxReceipt([]string{"MSG1"}, true, at.Add(time.Second))
	if len(read) != 1 || read[0].Progress() != OutboxRead || !read[0].Delivered.Equal(time.UnixMilli(at.UnixMilli())) {
		t.Fatalf("read must keep the delivery time: %+v", read)
	}
	if again, _ := s.MarkOutboxReceipt([]string{"MSG1"}, false, at); len(again) != 0 {
		t.Fatal("a late delivery receipt after read must not report a change")
	}
	c, _ := s.CountOutbox(OutboxFilter{})
	if c.Sent != 2 || c.Delivered != 2 || c.Read != 1 {
		t.Fatalf("counts %+v", c)
	}
	if none, err := s.MarkOutboxReceipt(nil, true, at); none != nil || err != nil {
		t.Fatal("no ids, no query")
	}
	if _, err := s.Enqueue([]OutboxItem{{Batch: "promo", Actor: "ops", To: "552187654321", Text: "Hi Ana"}}, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.MarkOutboxReceipt([]string{""}, true, at); len(got) != 0 {
		t.Fatalf("an empty id must not match items not sent yet: %+v", got)
	}
	var detail string
	var id, parent, unused int
	if err := s.db.QueryRow(`EXPLAIN QUERY PLAN UPDATE wame_outbox SET read_at = 1 WHERE message_id != '' AND message_id IN (?, ?) AND read_at = 0`,
		"A", "B").Scan(&id, &parent, &unused, &detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "wame_outbox_message") {
		t.Fatalf("receipts must use the message id index, plan: %s", detail)
	}
}

func TestOutboxReceiptColumnsAddedToOldTables(t *testing.T) {
	db := openDB(t)
	if _, err := db.Exec(`CREATE TABLE wame_outbox (id INTEGER PRIMARY KEY AUTOINCREMENT, batch TEXT NOT NULL, actor TEXT NOT NULL,
		token_id TEXT NOT NULL DEFAULT '', key TEXT NOT NULL DEFAULT '', recipient TEXT NOT NULL, text TEXT NOT NULL,
		reply_to TEXT NOT NULL DEFAULT '', status TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, due INTEGER NOT NULL,
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, chat TEXT NOT NULL DEFAULT '',
		message_id TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '');
		INSERT INTO wame_outbox (batch, actor, recipient, text, status, due, created_at, updated_at, message_id)
		VALUES ('old', 'ops', '5511999999999', 'Hi Maria', 'sent', 1, 1, 1, 'OLD1')`); err != nil {
		t.Fatal(err)
	}
	var s *Store
	for range 2 {
		var err error
		if s, err = Open(context.Background(), db); err != nil {
			t.Fatal(err)
		}
		if it, err := s.OutboxItem(1); err != nil || !it.Delivered.IsZero() || it.Batch != "old" {
			t.Fatalf("%v %+v", err, it)
		}
	}
	if got, _ := s.MarkOutboxReceipt([]string{"OLD1"}, true, time.Now()); len(got) != 1 {
		t.Fatal("receipts must work on an upgraded table")
	}
}

func TestAddColumnAlreadyAddedByAnotherProcess(t *testing.T) {
	db := openDB(t)
	if _, err := db.Exec(`CREATE TABLE t (a TEXT)`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := addColumn(context.Background(), db, "t", "b INTEGER NOT NULL DEFAULT 0"); err != nil {
			t.Fatalf("a column added meanwhile must count as added: %v", err)
		}
	}
	if err := addColumn(context.Background(), db, "missing", "b INTEGER"); err == nil {
		t.Fatal("other errors must still fail")
	}
}

func TestOutboxFilterFor(t *testing.T) {
	f := OutboxFilter{Actor: "someone", Batch: "promo"}
	if got := f.For("support-bot", false); got.Actor != "support-bot" || got.Batch != "promo" {
		t.Fatalf("non-admin: %+v", got)
	}
	if got := f.For("ops", true); got.Actor != "someone" {
		t.Fatalf("admin keeps the asked actor: %+v", got)
	}
}

// BenchmarkEnqueue measures queue writes: thousands per second, far above any safe WhatsApp send
// rate, which is why the queue lives in SQLite and not in Redis (docs/QUEUE.md).
func BenchmarkEnqueue(b *testing.B) {
	for _, size := range []int{1, 100} {
		b.Run(fmt.Sprintf("batch=%d", size), func(b *testing.B) {
			s := openStore(b)
			batch := make([]OutboxItem, size)
			for i := range batch {
				batch[i] = OutboxItem{Batch: "bench", Actor: "ops", To: "5511999999999", Text: "Hello from the benchmark"}
			}
			for b.Loop() {
				if _, err := s.Enqueue(batch, nil); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(b.N*size)/b.Elapsed().Seconds(), "msgs/s")
		})
	}
}

func BenchmarkDueAndClaim(b *testing.B) {
	s := openStore(b)
	batch := make([]OutboxItem, 1000)
	for i := range batch {
		batch[i] = OutboxItem{Batch: "bench", Actor: "ops", To: "5511999999999", Text: "Hello"}
	}
	for range 20 {
		if _, err := s.Enqueue(batch, nil); err != nil {
			b.Fatal(err)
		}
	}
	for b.Loop() {
		due, err := s.DueOutbox(time.Now(), 50)
		if err != nil || len(due) == 0 {
			b.Fatal(err, len(due))
		}
		if _, err := s.ClaimOutbox(due[0].ID, "X"); err != nil {
			b.Fatal(err)
		}
		if _, err := s.FinishOutbox(due[0].ID, OutboxResult{Status: OutboxSent}); err != nil {
			b.Fatal(err)
		}
	}
}

func TestEnqueueCheckRollsBack(t *testing.T) {
	s := openStore(t)
	at := time.Now().Add(time.Hour)
	first, err := s.Enqueue([]OutboxItem{{Batch: "promo", Actor: "ops", Key: "k#0", To: "5511999999999", Text: "Hi Maria", Due: at}}, nil)
	if err != nil || first[0].SendAt != at.UnixMilli() {
		t.Fatalf("%v %+v", err, first)
	}
	refuse := errors.New("refused")
	var seen []bool
	var open int
	_, err = s.Enqueue([]OutboxItem{
		{Batch: "promo", Actor: "ops", Key: "k#0", To: "5511999999999", Text: "Hi Maria", Due: at},
		{Batch: "promo", Actor: "ops", Key: "k#1", To: "5511988887777", Text: "Hi John"},
	}, func(out []OutboxItem, replayed []bool, n int) error {
		seen, open = replayed, n
		return refuse
	})
	if !errors.Is(err, refuse) || len(seen) != 2 || !seen[0] || seen[1] || open != 2 {
		t.Fatalf("%v %v %d", err, seen, open)
	}
	if c, _ := s.CountOutbox(OutboxFilter{}); c.Queued != 1 {
		t.Fatalf("a refused check must roll back: %+v", c)
	}
	if it, _ := s.Enqueue([]OutboxItem{{Batch: "promo", Actor: "ops", To: "5511988887777", Text: "Hi John"}}, nil); it[0].SendAt != 0 || it[0].Due.IsZero() {
		t.Fatalf("no send_at: %+v", it[0])
	}
}
