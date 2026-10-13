package bot

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/williamtcastro/wame/internal/auth"
	"github.com/williamtcastro/wame/internal/store"
)

func TestEnqueueValidatesAndDedupes(t *testing.T) {
	b := newTestBot(t)
	ch, unsub := b.Subscribe()
	defer unsub()

	for _, bad := range [][]QueueRequest{
		nil,
		{{To: "5511999999999", Text: " "}},
		{{To: "abc", Text: "hi"}},
		{{To: "5511999999999", Text: "hi", SendAt: time.Now().Add(40 * 24 * time.Hour)}},
	} {
		if _, err := b.Enqueue("ops", "", "", "", bad); !isInputError(err) {
			t.Fatalf("Enqueue(%v) = %v, want InputError", bad, err)
		}
	}
	if _, err := b.Enqueue("ops", "", "no spaces please", "", []QueueRequest{{To: "5511999999999", Text: "hi"}}); err == nil {
		t.Fatal("bad batch name accepted")
	}

	reqs := []QueueRequest{{To: "5511999999999", Text: "Hi Maria"}, {To: "5511988887777", Text: "Hi John"}}
	first, err := b.Enqueue("ops", "", "promo", "k1", reqs)
	if err != nil || len(first) != 2 || first[0].Status != store.OutboxQueued || first[0].Batch != "promo" {
		t.Fatalf("%v %+v", err, first)
	}
	again, err := b.Enqueue("ops", "", "promo", "k1", reqs)
	if err != nil || again[0].ID != first[0].ID || again[1].ID != first[1].ID {
		t.Fatalf("same key queued again: %v %+v", err, again)
	}
	if other, _ := b.Enqueue("john-bot", "", "promo", "k1", reqs[:1]); other[0].ID == first[0].ID {
		t.Fatal("keys must be per actor")
	}
	if _, err := b.Enqueue("ops", "", "promo", "k1", []QueueRequest{{To: "5511999999999", Text: "something else"}}); !errors.Is(err, ErrKeyReused) {
		t.Fatalf("same key, different body = %v", err)
	}
	if c, _ := b.Store.CountOutbox(store.OutboxFilter{}); c.Queued != 3 {
		t.Fatalf("counts %+v", c)
	}

	canceled, err := b.CancelQueued("ops", 0, store.OutboxFilter{Batch: "promo", Actor: "ops"})
	if err != nil || len(canceled) != 2 {
		t.Fatalf("cancel batch: %v %d", err, len(canceled))
	}
	select {
	case v := <-ch:
		if e, ok := v.(QueueEvent); !ok || e.Item.Status != store.OutboxCanceled || e.Item.Text != "" {
			t.Fatalf("event %#v", v)
		}
	case <-time.After(time.Second):
		t.Fatal("no queue event")
	}
	if _, err := b.CancelQueued("ops", first[0].ID, store.OutboxFilter{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cancel twice = %v", err)
	}
}

func TestOutboxRequeuesWhileOffline(t *testing.T) {
	b := newTestBot(t)
	items, err := b.Enqueue("ops", "", "", "", []QueueRequest{{To: "5511999999999", Text: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	b.sendQueued(context.Background(), items[0])
	it, _ := b.Store.OutboxItem(items[0].ID)
	if it.Status != store.OutboxQueued || it.Attempts != 0 || it.MessageID == "" || it.Error == "" {
		t.Fatalf("a send while logged out must go back in the queue without using an attempt: %+v", it)
	}
	id := it.MessageID
	b.sendQueued(context.Background(), it)
	if it, _ = b.Store.OutboxItem(it.ID); it.MessageID != id {
		t.Fatalf("message id changed between attempts: %s → %s", id, it.MessageID)
	}

	if _, err := b.Store.ClaimOutbox(it.ID, "other"); err != nil {
		t.Fatal(err)
	}
	if n, _ := b.Store.RequeueSending(); n != 1 {
		t.Fatalf("requeued %d", n)
	}
}

func TestOutboxDropsRevokedTokens(t *testing.T) {
	b := newTestBot(t)
	_, tok, err := auth.Issue(b.Store, auth.Request{Name: "promo-bot", Scopes: []auth.Scope{auth.Send}, Rate: 1})
	if err != nil {
		t.Fatal(err)
	}
	items, _ := b.Enqueue("promo-bot", tok.ID, "", "", []QueueRequest{{To: "5511999999999", Text: "a"}, {To: "5511988887777", Text: "b"}})
	if _, ok := b.outboxSlot(&tok, maria); !ok {
		t.Fatal("first slot must be free")
	}
	if d, ok := b.outboxSlot(&tok, john); ok || d < 50*time.Second {
		t.Fatalf("the token's rate of 1/min must hold the second back: %v %v", d, ok)
	}
	b.Limiter = NewLimiter(1, 0, 0)
	if _, ok := b.outboxSlot(&tok, john); ok {
		t.Fatal("still held back")
	}
	if s := b.tokenLimiter(tok.ID, 1).Stats(); s.LastMinute != 1 {
		t.Fatalf("a refused slot must not use the token's budget: %+v", s)
	}
	if b.Limiter.Stats().Rejected != 0 {
		t.Fatal("outbox waits must not count as 429s")
	}

	if _, err := b.Store.RevokeToken("promo-bot"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.queueToken(tok.ID, map[string]*store.Token{}); !errors.Is(err, store.ErrTokenRevoked) {
		t.Fatalf("revoked token not noticed: %v", err)
	}
	b.finishQueued(items[0], store.OutboxResult{Status: store.OutboxFailed, Error: "revoked"})
	if _, err := b.CancelQueued("ops", items[1].ID, store.OutboxFilter{}); err != nil {
		t.Fatal(err)
	}
	b.finishQueued(items[1], store.OutboxResult{Status: store.OutboxFailed, Error: "late"})
	if it, _ := b.Store.OutboxItem(items[1].ID); it.Status != store.OutboxCanceled {
		t.Fatalf("a late result must not overwrite a cancel: %+v", it)
	}
}

func TestOutboxBacksOff(t *testing.T) {
	b := newTestBot(t)
	b.outboxResult(false, "boom")
	if !b.QueueStats().CooldownUntil.IsZero() {
		t.Fatal("one error must not pause the queue")
	}
	b.outboxResult(false, "boom")
	if s := b.QueueStats(); time.Until(s.CooldownUntil) < 20*time.Second || s.LastError != "boom" {
		t.Fatalf("%+v", s)
	}
	b.outboxResult(true, "")
	if !b.QueueStats().CooldownUntil.IsZero() {
		t.Fatal("a success must end the cooldown")
	}
	if outboxBackoff(1) != 30*time.Second || outboxBackoff(20) != maxOutboxBackoff {
		t.Fatal("backoff")
	}
}

func TestQueueSettings(t *testing.T) {
	b := newTestBot(t)
	if _, err := b.InitSettings(Settings{Rate: 20, QueueJitter: 2 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if err := b.UpdateSettings("ops", map[string]*string{"queue_paused": new("true"), "queue_jitter": new("5s")}); err != nil {
		t.Fatal(err)
	}
	if s := b.QueueStats(); !s.Paused || s.JitterMS != 5000 {
		t.Fatalf("%+v", s)
	}
	if d := b.outboxStep(context.Background()); d != outboxIdle {
		t.Fatalf("paused step waited %v", d)
	}
}

func TestQuietHoursWindow(t *testing.T) {
	sp, err := ParseTimezone("America/Sao_Paulo")
	if err != nil {
		t.Fatal(err)
	}
	at := func(hm string) time.Time {
		d, _ := time.Parse("15:04", hm)
		return time.Date(2026, 10, 13, d.Hour(), d.Minute(), 0, 0, sp)
	}
	night, _ := ParseQuietHours("21:00-08:00")
	lunch, _ := ParseQuietHours(" 12:00 - 13:30 ")
	for _, c := range []struct {
		q    QuietHours
		now  string
		want time.Time
	}{
		{night, "20:59", time.Time{}},
		{night, "21:00", at("08:00").AddDate(0, 0, 1)},
		{night, "23:59", at("08:00").AddDate(0, 0, 1)},
		{night, "00:00", at("08:00")},
		{night, "07:59", at("08:00")},
		{night, "08:00", time.Time{}},
		{lunch, "12:00", at("13:30")},
		{lunch, "13:30", time.Time{}},
		{lunch, "11:00", time.Time{}},
		{QuietHours{}, "12:00", time.Time{}},
	} {
		if got := c.q.Until(at(c.now), sp); !got.Equal(c.want) {
			t.Errorf("%s at %s: until %v, want %v", c.q, c.now, got, c.want)
		}
	}
	if lunch.String() != "12:00-13:30" || (QuietHours{}).String() != "" {
		t.Fatalf("String: %q", lunch.String())
	}
	if got := night.Until(at("22:00").UTC(), sp); !got.Equal(at("08:00").AddDate(0, 0, 1)) {
		t.Fatalf("the window is read on the zone's clock, not the server's: %v", got)
	}
	for _, bad := range []string{"21:00", "21:00-21:00", "25:00-08:00", "9pm-8am", "21:00-"} {
		if _, err := ParseQuietHours(bad); err == nil {
			t.Errorf("ParseQuietHours(%q) accepted", bad)
		}
	}
	if loc, err := ParseTimezone(""); err != nil || loc != time.Local {
		t.Fatal("empty is the server's zone")
	}
	if _, err := ParseTimezone("Mars/Olympus"); err == nil {
		t.Fatal("unknown zone accepted")
	}
}

func TestQuietHoursHoldTheQueue(t *testing.T) {
	b := newTestBot(t)
	if _, err := b.InitSettings(Settings{Rate: 20}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []map[string]*string{{"queue_quiet_hours": new("soon")}, {"queue_timezone": new("Nowhere/Town")}} {
		if err := b.UpdateSettings("ops", bad); !isInputError(err) {
			t.Fatalf("%v accepted: %v", bad, err)
		}
	}
	sp, _ := time.LoadLocation("America/Sao_Paulo")
	now := time.Now().In(sp)
	window := now.Add(-time.Hour).Format("15:04") + "-" + now.Add(time.Hour).Format("15:04")
	if err := b.UpdateSettings("ops", map[string]*string{"queue_quiet_hours": &window, "queue_timezone": new("America/Sao_Paulo")}); err != nil {
		t.Fatal(err)
	}
	s := b.QueueStats()
	if s.QuietHours != window || s.Timezone != "America/Sao_Paulo" || time.Until(s.QuietUntil) < 50*time.Minute {
		t.Fatalf("%+v", s)
	}
	if d := b.outboxStep(context.Background()); d < 50*time.Minute || d > 61*time.Minute {
		t.Fatalf("a quiet step must sleep until the window ends, waited %v", d)
	}
	view, _ := b.SettingsView()
	for _, v := range view {
		if v.Key == "queue_timezone" && (v.Value != "America/Sao_Paulo" || v.Default != "Local") {
			t.Fatalf("%+v", v)
		}
	}
	if err := b.UpdateSettings("ops", map[string]*string{"queue_quiet_hours": new("")}); err != nil {
		t.Fatal(err)
	}
	if s := b.QueueStats(); !s.QuietUntil.IsZero() || s.QuietHours != "" {
		t.Fatalf("turned off: %+v", s)
	}
	if d := b.outboxStep(context.Background()); d != outboxOffline {
		t.Fatalf("outside quiet hours the offline check applies again, waited %v", d)
	}
}

func TestOutboxReceiptEvents(t *testing.T) {
	b := newTestBot(t)
	items, _ := b.Enqueue("ops", "", "promo", "", []QueueRequest{{To: "5511999999999", Text: "Hi Maria"}})
	if _, err := b.Store.ClaimOutbox(items[0].ID, "MSG1"); err != nil {
		t.Fatal(err)
	}
	b.finishQueued(items[0], store.OutboxResult{Status: store.OutboxSent, Chat: maria})
	ch, unsub := b.Subscribe()
	defer unsub()
	chat, _ := types.ParseJID(maria)
	receipt := func(typ types.ReceiptType, fromMe bool, ids ...types.MessageID) {
		b.handle(&events.Receipt{MessageSource: types.MessageSource{Chat: chat, Sender: chat, IsFromMe: fromMe}, MessageIDs: ids, Type: typ, Timestamp: time.Now()})
	}
	next := func() string {
		select {
		case v := <-ch:
			e, ok := v.(QueueEvent)
			if !ok || e.Item.Text != "" {
				t.Fatalf("event %#v", v)
			}
			return e.Item.Progress()
		default:
			return ""
		}
	}

	receipt(types.ReceiptTypeDelivered, false, "MSG1", "UNRELATED")
	if got := next(); got != store.OutboxDelivered {
		t.Fatalf("delivered event = %q", got)
	}
	receipt(types.ReceiptTypeDelivered, false, "MSG1")
	receipt(types.ReceiptTypeReadSelf, true, "MSG1")
	receipt(types.ReceiptTypeRead, true, "MSG1")
	if got := next(); got != "" {
		t.Fatalf("repeats and our own read receipts must not publish: %q", got)
	}
	receipt(types.ReceiptTypeRead, false, "MSG1")
	if got := next(); got != store.OutboxRead {
		t.Fatalf("read event = %q", got)
	}
	if s := b.QueueStats(); s.Sent != 1 || s.Delivered != 1 || s.Read != 1 {
		t.Fatalf("%+v", s.OutboxCounts)
	}
}
