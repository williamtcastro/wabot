package bot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/williamtcastro/wame/internal/store"
)

type fakeNotifier struct {
	mu   sync.Mutex
	got  []Alert
	fail bool
}

func (f *fakeNotifier) Name() string { return "fake" }

func (f *fakeNotifier) Notify(_ context.Context, a Alert) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("push service unavailable")
	}
	f.got = append(f.got, a)
	return nil
}

func (f *fakeNotifier) titles() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, a := range f.got {
		out = append(out, a.Title)
	}
	return out
}

func (f *fakeNotifier) last() Alert {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got[len(f.got)-1]
}

func newAlerter(n Notifier, delay time.Duration) *alerter {
	b := &Bot{}
	b.SetAlerts(n, delay)
	return b.alerts
}

var (
	online       = Status{Connected: true, LoggedIn: true, JID: maria, State: "connected"}
	disconnected = Status{JID: maria, State: "disconnected"}
	loggedOut    = Status{State: "logged_out"}
)

func TestGotifyNotify(t *testing.T) {
	var got struct {
		path, key, query, ctype string
		body                    Alert
	}
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.key, got.query, got.ctype = r.URL.Path, r.Header.Get("X-Gotify-Key"), r.URL.RawQuery, r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		w.WriteHeader(status)
	}))
	defer srv.Close()

	g, err := NewGotify(srv.URL+"/gotify/", "A1b2C3d4")
	if err != nil {
		t.Fatal(err)
	}
	a := Alert{Title: "wame: test", Message: "hello", Priority: priorityHigh}
	if err := g.Notify(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if got.path != "/gotify/message" || got.key != "A1b2C3d4" || got.query != "" || got.ctype != "application/json" || got.body != a {
		t.Fatalf("request = %+v", got)
	}
	status = http.StatusUnauthorized
	if err := g.Notify(t.Context(), a); err == nil || err.Error() != "gotify answered HTTP 401" {
		t.Fatalf("401: %v", err)
	}

	srv.Close()
	err = g.Notify(t.Context(), a)
	host := strings.TrimPrefix(srv.URL, "http://")
	ip, _, _ := strings.Cut(host, ":")
	if err == nil || strings.Contains(err.Error(), ip) || strings.Contains(err.Error(), "A1b2C3d4") || strings.Contains(err.Error(), "/message") {
		t.Fatalf("transport error leaks the address: %v", err)
	}

	named, _ := NewGotify("http://gotify.invalid:8070", "A1b2C3d4")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := named.Notify(ctx, a); err == nil || strings.Contains(err.Error(), "gotify.invalid") {
		t.Fatalf("lookup error leaks the host name: %v", err)
	}
}

func TestNewGotifyChecks(t *testing.T) {
	for _, c := range []struct{ url, token string }{
		{"10.0.0.5:8070", "tok"},
		{"ftp://10.0.0.5", "tok"},
		{"http://10.0.0.5:8070/?token=tok", "tok"},
		{"http://user:pass@10.0.0.5:8070", "tok"},
		{"http://10.0.0.5:8070", ""},
	} {
		if _, err := NewGotify(c.url, c.token); err == nil {
			t.Errorf("accepted %q / %q", c.url, c.token)
		}
	}
	if _, err := NewGotify("https://10.0.0.5:8070", "tok"); err != nil {
		t.Fatal(err)
	}
}

func TestAlertConnection(t *testing.T) {
	n := &fakeNotifier{}
	a := newAlerter(n, 2*time.Minute)
	ctx := t.Context()
	t0 := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	at := func(d time.Duration, s Status) { a.observe(ctx, t0.Add(d), alertSnapshot{status: s}) }

	at(0, online)
	at(time.Second, disconnected)
	at(time.Minute, disconnected)
	at(90*time.Second, online)
	if len(n.titles()) != 0 {
		t.Fatalf("a short blip alerted: %v", n.titles())
	}

	at(2*time.Minute, disconnected)
	at(3*time.Minute, disconnected)
	at(4*time.Minute, disconnected)
	if got := n.titles(); len(got) != 1 || got[0] != "wame: WhatsApp disconnected" {
		t.Fatalf("after the delay: %v", got)
	}
	if l := n.last(); l.Priority != priorityWarn || !strings.Contains(l.Message, "Account: 5511999999999") {
		t.Fatalf("disconnect alert = %+v", l)
	}
	at(5*time.Minute, disconnected)
	at(6*time.Minute, online)
	at(7*time.Minute, online)
	if got := n.titles(); len(got) != 2 || got[1] != "wame: WhatsApp is back" || !strings.Contains(n.last().Message, "after 4m0s") {
		t.Fatalf("recovery: %v %+v", got, n.last())
	}

	at(8*time.Minute, loggedOut)
	if l := n.last(); l.Title != "wame: WhatsApp logged out" || l.Priority != priorityHigh || !strings.Contains(l.Message, "5511999999999") {
		t.Fatalf("a logout must alert at once: %+v", l)
	}
	at(9*time.Minute, loggedOut)
	at(10*time.Minute, Status{State: "waiting_for_qr_scan"})
	if len(n.titles()) != 3 {
		t.Fatalf("repeated: %v", n.titles())
	}
	at(11*time.Minute, online)
	if got := n.titles(); len(got) != 4 || got[3] != "wame: WhatsApp is back" {
		t.Fatalf("after re-linking: %v", got)
	}

	fresh := &fakeNotifier{}
	b := newAlerter(fresh, time.Minute)
	b.observe(ctx, t0, alertSnapshot{status: Status{State: "waiting_for_qr_scan"}})
	b.observe(ctx, t0.Add(time.Hour), alertSnapshot{status: Status{State: "waiting_for_qr_scan"}})
	if len(fresh.titles()) != 0 {
		t.Fatalf("an unpaired server alerted: %v", fresh.titles())
	}
}

func TestAlertRetriesAfterFailure(t *testing.T) {
	n := &fakeNotifier{fail: true}
	b := &Bot{}
	b.SetAlerts(n, 0)
	a := b.alerts
	ctx := t.Context()
	t0 := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	a.observe(ctx, t0, alertSnapshot{status: loggedOut})
	a.observe(ctx, t0.Add(30*time.Second), alertSnapshot{status: loggedOut})
	if s := b.AlertStats(); s.Failed != 1 || s.LastError != "push service unavailable" || s.Sent != 0 {
		t.Fatalf("stats = %+v (a failure must back off before retrying)", s)
	}
	n.mu.Lock()
	n.fail = false
	n.mu.Unlock()
	a.observe(ctx, t0.Add(2*time.Minute), alertSnapshot{status: loggedOut})
	if s := b.AlertStats(); s.Sent != 1 || s.LastTitle != "wame: WhatsApp logged out" || !s.Configured || s.Backend != "fake" {
		t.Fatalf("retry: %+v", s)
	}
}

func TestAlertEvents(t *testing.T) {
	n := &fakeNotifier{}
	a := newAlerter(n, time.Minute)
	ctx := t.Context()
	t0 := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	hook := func(failed, dropped int64) *WebhookStats { return &WebhookStats{Failed: failed, Dropped: dropped} }
	at := func(d time.Duration, s alertSnapshot) {
		s.status = online
		a.observe(ctx, t0.Add(d), s)
	}

	at(0, alertSnapshot{webhook: hook(3, 0)})
	if len(n.titles()) != 0 {
		t.Fatalf("losses from before the alerter started alerted: %v", n.titles())
	}
	at(time.Minute, alertSnapshot{webhook: hook(4, 1)})
	if l := n.last(); l.Title != "wame: webhook deliveries lost" || !strings.HasPrefix(l.Message, "2 webhook") {
		t.Fatalf("webhook = %+v", l)
	}
	at(2*time.Minute, alertSnapshot{webhook: hook(9, 1)})
	if len(n.titles()) != 1 {
		t.Fatal("webhook alerts must be rate-limited")
	}
	at(17*time.Minute, alertSnapshot{webhook: hook(9, 1)})
	if l := n.last(); len(n.titles()) != 2 || !strings.HasPrefix(l.Message, "5 webhook") {
		t.Fatalf("after the window: %v %+v", n.titles(), l)
	}

	cool := t0.Add(20 * time.Minute)
	at(19*time.Minute, alertSnapshot{queue: QueueStats{CooldownUntil: cool}, queueFailed: 2})
	got := n.titles()
	if len(got) != 4 || got[2] != "wame: send queue backing off" || got[3] != "wame: queued messages failed" || !strings.HasPrefix(n.last().Message, "2 queued") {
		t.Fatalf("queue: %v", got)
	}
	at(19*time.Minute+5*time.Second, alertSnapshot{queue: QueueStats{CooldownUntil: cool}, queueFailed: 1})
	at(20*time.Minute, alertSnapshot{queue: QueueStats{CooldownUntil: cool.Add(time.Minute)}, queueFailed: 1})
	if len(n.titles()) != 4 {
		t.Fatalf("queue alerts must be rate-limited: %v", n.titles())
	}
	at(35*time.Minute, alertSnapshot{})
	if l := n.last(); len(n.titles()) != 5 || !strings.HasPrefix(l.Message, "2 queued") {
		t.Fatalf("held-back failures: %v %+v", n.titles(), l)
	}
	for _, al := range n.got {
		if strings.Contains(al.Message, "5511988887777") || strings.Contains(al.Message, "http") {
			t.Fatalf("alert leaks: %+v", al)
		}
	}
}

func TestRunAlertsAndTestAlert(t *testing.T) {
	b := newTestBot(t)
	if err := b.TestAlert(t.Context(), "ops"); err == nil || b.AlertStats().Configured {
		t.Fatal("tested alerts that aren't configured")
	}
	n := &fakeNotifier{}
	b.SetAlerts(n, time.Minute)
	if err := b.TestAlert(t.Context(), "ops"); err != nil || n.last().Title != "wame: test alert" {
		t.Fatalf("test alert: %v", err)
	}
	entries, _ := b.Store.AuditLog(5, 0)
	if len(entries) < 2 || entries[0].Action != "alert_test" || !entries[0].OK || entries[1].OK {
		t.Fatalf("audit = %+v", entries)
	}

	b.Go(b.RunAlerts)
	for subscribed := false; !subscribed; time.Sleep(time.Millisecond) {
		b.mu.RLock()
		subscribed = len(b.subs) > 0
		b.mu.RUnlock()
	}
	b.publish(QueueEvent{Item: store.OutboxItem{ID: 1, Status: store.OutboxFailed}})
	b.setState("logged_out", "")
	for deadline := time.Now().Add(5 * time.Second); len(n.titles()) < 3 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
	}
	got := n.titles()
	if len(got) != 3 || got[1] != "wame: WhatsApp logged out" || got[2] != "wame: queued messages failed" {
		t.Fatalf("alerts = %v", got)
	}
}
