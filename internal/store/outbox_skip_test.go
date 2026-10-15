package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

const (
	mariaJID = "5511999999999@s.whatsapp.net"
	johnJID  = "5511988887777@s.whatsapp.net"
)

func TestOutboxRecipientKeyBackfill(t *testing.T) {
	s := openStore(t)
	if _, err := s.db.Exec(`INSERT INTO wame_outbox (batch, actor, recipient, text, status, due, created_at, updated_at, chat)
		VALUES ('old', 'ops', '+55 11 99999-9999', 'Hi Maria', 'sent', 1, 1, 1, ?), ('old', 'ops', '5511988887777', 'Hi John', 'queued', 1, 1, 1, '')`, mariaJID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateOutbox(context.Background(), s.db); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.ListOutbox(OutboxFilter{}, 10)
	if err != nil || len(items) != 2 || items[1].RecipientKey != mariaJID || items[0].RecipientKey != "" {
		t.Fatalf("an item sent before recipient_key existed is matched by its chat: %v %+v", err, items)
	}
}

func TestEnqueueWithAltKeysAndSkips(t *testing.T) {
	s := openStore(t)
	first, err := s.Enqueue([]OutboxItem{{Batch: "promo", Actor: "ops", Key: "k#0:100000000000001@lid", To: "100000000000001@lid", Text: "Hi", RecipientKey: "100000000000001@lid"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	items := []OutboxItem{
		{Batch: "promo", Actor: "ops", Key: "k#0:" + mariaJID, To: mariaJID, Text: "Hi", RecipientKey: mariaJID},
		{Batch: "promo", Actor: "ops", Key: "k#1", To: johnJID, Text: "Hi", RecipientKey: johnJID},
		{Batch: "promo", Actor: "ops", Key: "k#2", To: "5511988887777", Text: "Hi", RecipientKey: johnJID},
	}
	var replayed []bool
	out, skipped, err := s.EnqueueWith(items, EnqueueOptions{
		Rows:   []EnqueueRow{{AltKeys: []string{"k#0:100000000000001@lid"}, Recipients: []string{mariaJID, "100000000000001@lid"}}},
		Dedupe: true,
		Check:  func(_ []OutboxItem, r []bool, _ int) error { replayed = r; return nil },
	})
	if err != nil || out[0].ID != first[0].ID || !replayed[0] || out[1].ID == 0 || out[2].ID != 0 || !slices.Equal(skipped, []string{"", "", SkipDuplicate}) {
		t.Fatalf("%v %+v %v %v", err, out, replayed, skipped)
	}

	skip := &SkipSent{Since: time.Now().Add(-time.Hour), Actor: "ops"}
	_, skipped, err = s.EnqueueWith([]OutboxItem{
		{Batch: "news", Actor: "ops", To: "5511999999999", Text: "News", RecipientKey: mariaJID},
		{Batch: "news", Actor: "ops", To: "552187654321", Text: "News", RecipientKey: "552187654321@s.whatsapp.net"},
	}, EnqueueOptions{SkipSent: skip, Rows: []EnqueueRow{{Recipients: []string{mariaJID, "100000000000001@lid"}}}})
	if err != nil || !slices.Equal(skipped, []string{SkipRecentlySent, ""}) {
		t.Fatalf("Maria's queued item (stored under her @lid) counts: %v %v", err, skipped)
	}
	skip.Actor = "john-bot"
	if _, skipped, _ = s.EnqueueWith([]OutboxItem{{Batch: "x", Actor: "john-bot", To: "5511988887777", Text: "x", RecipientKey: johnJID}}, EnqueueOptions{SkipSent: skip}); skipped[0] != "" {
		t.Fatal("another actor's items don't count")
	}
}

func TestEnqueueWithTakenKeys(t *testing.T) {
	s := openStore(t)
	if _, err := s.Enqueue([]OutboxItem{{Batch: "promo", Actor: "ops", Key: "k#0", To: "5511999999999", Text: "Hi"}}, nil); err != nil {
		t.Fatal(err)
	}
	member := []OutboxItem{{Batch: "promo", Actor: "ops", Key: "k#0:" + johnJID, To: johnJID, Text: "Hi"}}
	for _, opt := range []EnqueueOptions{
		{Rows: []EnqueueRow{{Taken: []string{"k#0"}}}},
		{Rows: []EnqueueRow{{Taken: []string{"k#0"}, AltKeys: []string{"k#0:x"}}}},
	} {
		if _, _, err := s.EnqueueWith(member, opt); !errors.Is(err, ErrKeyTaken) {
			t.Fatalf("%+v: %v", opt, err)
		}
	}
	if _, _, err := s.EnqueueWith([]OutboxItem{{Batch: "promo", Actor: "ops", Key: "k#1", To: "5511988887777", Text: "Hi"}},
		EnqueueOptions{Rows: []EnqueueRow{{Taken: []string{"k#1:"}}}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnqueueWith(member[:1:1], EnqueueOptions{Rows: []EnqueueRow{{Taken: []string{"k#1"}}}}); !errors.Is(err, ErrKeyTaken) {
		t.Fatal(err)
	}
	if _, _, err := s.EnqueueWith([]OutboxItem{{Batch: "promo", Actor: "ops", Key: "k#2", To: "5511988887777", Text: "Hi"}},
		EnqueueOptions{Rows: []EnqueueRow{{Taken: []string{"k#0:"}}}}); err != nil {
		t.Fatalf("no k#0:… key is stored: %v", err)
	}
	if n, _ := s.CountOutbox(OutboxFilter{}); n.Queued != 3 {
		t.Fatalf("a refused request stores nothing: %+v", n)
	}
}
