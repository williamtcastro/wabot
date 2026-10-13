package store

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestOutboxLifecycle(t *testing.T) {
	s := openStore(t)
	later := time.Now().Add(time.Hour)
	items, err := s.Enqueue([]OutboxItem{
		{Batch: "promo", Actor: "ops", To: "5511999999999", Text: "Hi Maria", Due: later},
		{Batch: "promo", Actor: "ops", To: "5511988887777", Text: "Hi John"},
	})
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
				if _, err := s.Enqueue(batch); err != nil {
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
		if _, err := s.Enqueue(batch); err != nil {
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
