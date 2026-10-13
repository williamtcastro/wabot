package bot

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/williamtcastro/wame/internal/store"
)

func ptr(s string) *string { return &s }

func TestRuntimeSettings(t *testing.T) {
	b := newTestBot(t)
	base := Settings{Rate: 20, ChatGap: time.Second, MaxWait: 30 * time.Second, AutoReply: true}
	if _, err := b.InitSettings(base); err != nil {
		t.Fatal(err)
	}
	if err := b.UpdateSettings("ops", map[string]*string{"rate": ptr("5"), "auto_reply": ptr("false"), "chat_gap": ptr("3s")}); err != nil {
		t.Fatal(err)
	}
	if s := b.Limiter.Stats(); s.PerMinute != 5 || s.ChatGapMS != 3000 || b.Lookups.Stats().PerMinute != 15 || b.AutoReplyOn() {
		t.Fatalf("not applied: %+v, auto-reply %v", s, b.AutoReplyOn())
	}
	for _, bad := range []map[string]*string{
		{"rate": ptr("-1")}, {"chat_gap": ptr("1h")}, {"auto_reply": ptr("maybe")}, {"webhook_filter": ptr("from_me=perhaps")}, {"token": ptr("x")},
	} {
		if err := b.UpdateSettings("ops", bad); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
	if b.Limiter.Stats().PerMinute != 5 {
		t.Fatal("a rejected change was applied")
	}

	view, err := b.SettingsView()
	if err != nil || view[0].Key != "rate" || view[0].Value != "5" || view[0].Default != "20" || !view[0].Overridden || view[0].UpdatedBy != "ops" {
		t.Fatalf("view = %+v, %v", view, err)
	}

	// A restart keeps the overrides; a nil value goes back to the flag.
	if _, err := b.InitSettings(base); err != nil || b.Limiter.Stats().PerMinute != 5 {
		t.Fatalf("override lost on restart: %v", err)
	}
	if err := b.UpdateSettings("ops", map[string]*string{"rate": nil}); err != nil || b.Limiter.Stats().PerMinute != 20 {
		t.Fatalf("reset: %v, rate %d", err, b.Limiter.Stats().PerMinute)
	}

	log, _ := b.Store.AuditLog(20, 0)
	var ok, failed int
	for _, e := range log {
		if e.Action == "settings" {
			if e.OK {
				ok++
			} else {
				failed++
			}
		}
	}
	if ok != 2 || failed != 5 || !strings.Contains(log[0].Detail, "rate reset") {
		t.Fatalf("audit: %d ok, %d failed, latest %+v", ok, failed, log[0])
	}
}

func TestWebhookFilterSetting(t *testing.T) {
	b := newTestBot(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Webhook = NewWebhook(ctx, "http://127.0.0.1:1", "", store.Filter{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := b.InitSettings(Settings{}); err != nil {
		t.Fatal(err)
	}
	if err := b.UpdateSettings("ops", map[string]*string{"webhook_filter": ptr("chat=5511999999999&from_me=false")}); err != nil {
		t.Fatal(err)
	}
	b.Webhook.mu.RLock()
	f := b.Webhook.Filter
	b.Webhook.mu.RUnlock()
	if f.Chat != maria || f.FromMe == nil || *f.FromMe {
		t.Fatalf("filter = %+v", f)
	}
	b.Webhook.SetChatIf("5511999999999@s.whatsapp.net-old", john)
	if b.Webhook.Filter.Chat != maria {
		t.Fatal("a stale resolution overwrote the new filter")
	}
}

func TestWebhookTestAndReplay(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	events := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		events <- r.Header.Get("X-Wame-Event") + " " + r.Header.Get("X-Wame-Delivery")
		if fail.Load() && r.Header.Get("X-Wame-Event") != "test" {
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	b := newTestBot(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := b.TestWebhook(ctx, "ops"); err == nil {
		t.Fatal("tested a webhook that isn't configured")
	}
	b.Webhook = NewWebhook(ctx, srv.URL, "s3cret", store.Filter{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r, err := b.TestWebhook(ctx, "ops")
	if err != nil || !r.OK || r.Status != 200 || !strings.HasPrefix(<-events, "test test-") {
		t.Fatalf("test = %+v, %v", r, err)
	}

	m, ok, err := b.Store.Save(store.Message{ID: "R1", Chat: maria, Sender: maria, Type: "text", Text: "oi", Timestamp: time.Now()})
	if !ok || err != nil {
		t.Fatal(err)
	}
	b.Webhook.Deliver("message", m)
	first := <-events
	var s WebhookStats
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if s = b.Webhook.Stats(); len(s.Recent) == 1 {
			break
		}
	}
	if len(s.Recent) != 1 {
		t.Fatalf("no failure recorded: %+v", s)
	}
	id := s.Recent[0].Delivery

	if _, err := b.ReplayWebhook(ctx, "ops", "999-message-ffff"); err == nil {
		t.Fatal("replayed a delivery that never failed")
	}
	fail.Store(false)
	r, err = b.ReplayWebhook(ctx, "ops", id)
	if err != nil || !r.OK || r.Delivery != id || <-events != first {
		t.Fatalf("replay = %+v, %v", r, err)
	}
	if s := b.Webhook.Stats(); !s.Recent[0].Replayed || s.Replayed != 1 {
		t.Fatalf("replay not recorded: %+v", s)
	}
}

func TestUsage(t *testing.T) {
	b := newTestBot(t)
	b.RecordRequest("claude", "10.0.0.7")
	b.RecordRequest("claude", "10.0.0.8")
	b.RecordRequest("", "10.0.0.9")
	_, _ = b.audited("claude", "send", maria, "", func() (store.Message, error) { return store.Message{Chat: maria}, nil })
	_, _ = b.audited("claude", "send", maria, "", func() (store.Message, error) { return store.Message{}, BadInput("nope") })
	_, _ = b.audited("claude", "react", maria, "", func() (store.Message, error) { return store.Message{}, nil })
	u := b.Usage()
	if len(u) != 1 || u[0].Name != "claude" || u[0].Requests != 2 || u[0].Sends != 1 || u[0].Failed != 1 || u[0].LastIP != "10.0.0.8" {
		t.Fatalf("usage = %+v", u)
	}
}
