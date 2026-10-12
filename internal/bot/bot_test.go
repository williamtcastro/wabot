package bot

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"github.com/williamtcastro/wame/internal/store"
)

const (
	maria = "5511999999999@s.whatsapp.net"
	john  = "5511988887777@s.whatsapp.net"
	ana   = "552187654321@s.whatsapp.net"
	group = "120363000000000001@g.us"
)

func newTestBot(t *testing.T) *Bot {
	t.Helper()
	b, err := New(context.Background(), filepath.Join(t.TempDir(), "wame.db"), waLog.Noop, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLimiterReserveNAllOrNothing(t *testing.T) {
	l := NewLimiter(3, 0, 50*time.Millisecond)
	if _, err := l.ReserveN("lookup", 2); err != nil {
		t.Fatal(err)
	}
	_, err := l.ReserveN("lookup", 2)
	var rl RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter < 50*time.Second {
		t.Fatalf("second ReserveN(2) = %v, want RateLimitError of about a minute", err)
	}
	if at, err := l.Reserve("lookup"); err != nil || time.Until(at) > time.Second {
		t.Fatalf("the failed ReserveN must not take slots: at %v err %v", at, err)
	}
	var in InputError
	if _, err := l.ReserveN("lookup", 4); !errors.As(err, &in) {
		t.Fatalf("n over PerMinute = %v, want InputError", err)
	}
}

func TestLimiterChatGap(t *testing.T) {
	l := NewLimiter(0, time.Second, 5*time.Second)
	first, _ := l.Reserve(maria)
	second, _ := l.Reserve(maria)
	other, _ := l.Reserve(john)
	if d := second.Sub(first); d != time.Second {
		t.Fatalf("same chat gap = %v, want 1s", d)
	}
	if time.Until(other) > 100*time.Millisecond {
		t.Fatal("another chat must not wait for the gap")
	}
}

func TestParseRecipient(t *testing.T) {
	for in, want := range map[string]string{
		"+55 11 99999-9999":            maria,
		"5511988887777":                john,
		" 552187654321 ":               ana,
		"120363000000000001@g.us":      group,
		"5511999999999@s.whatsapp.net": maria,
	} {
		jid, err := ParseRecipient(in)
		if err != nil || jid.String() != want {
			t.Errorf("ParseRecipient(%q) = %v, %v; want %s", in, jid, err, want)
		}
	}
	for _, bad := range []string{"", "1234", "+1 234 567 890 123 456", "@g.us", "Maria"} {
		var in InputError
		if _, err := ParseRecipient(bad); !errors.As(err, &in) {
			t.Errorf("ParseRecipient(%q) err = %v, want InputError", bad, err)
		}
	}
}

func protocolEvent(chat, sender string, fromMe bool, pm *waE2E.ProtocolMessage) *events.Message {
	c, _ := types.ParseJID(chat)
	s, _ := types.ParseJID(sender)
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: c, Sender: s, IsFromMe: fromMe, IsGroup: c.Server == types.GroupServer},
			ID:            "P" + pm.GetKey().GetID(),
			Timestamp:     time.Now(),
		},
		Message: &waE2E.Message{ProtocolMessage: pm},
	}
}

func edit(id, text string) *waE2E.ProtocolMessage {
	return &waE2E.ProtocolMessage{
		Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
		Key:           &waCommon.MessageKey{ID: proto.String(id)},
		EditedMessage: &waE2E.Message{Conversation: proto.String(text)},
	}
}

func revoke(id string) *waE2E.ProtocolMessage {
	return &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String(id)}}
}

func TestHandleProtocolChecksAuthor(t *testing.T) {
	b := newTestBot(t)
	for _, m := range []store.Message{
		{ID: "M1", Chat: maria, Sender: maria, Type: "text", Text: "oi", Timestamp: time.Now()},
		{ID: "G1", Chat: group, Sender: maria, IsGroup: true, Type: "text", Text: "hello group", Timestamp: time.Now()},
	} {
		if _, ok, err := b.Store.Save(m); !ok || err != nil {
			t.Fatal(err)
		}
	}
	ch, unsub := b.Subscribe()
	defer unsub()

	b.handle(protocolEvent(maria, john, false, edit("M1", "forged")))
	b.handle(protocolEvent(group, ana, false, edit("G1", "forged")))
	b.handle(protocolEvent(group, ana, false, revoke("G1")))
	for _, id := range []string{"M1", "G1"} {
		chat := maria
		if id == "G1" {
			chat = group
		}
		m, _ := b.Store.Get(chat, id)
		if m.Edited || m.Deleted || m.Text == "forged" {
			t.Fatalf("%s was changed by someone else: %+v", id, m)
		}
	}
	select {
	case v := <-ch:
		t.Fatalf("ignored actions must not publish, got %#v", v)
	default:
	}

	b.handle(protocolEvent(maria, maria, false, edit("M1", "oi!")))
	if m, _ := b.Store.Get(maria, "M1"); !m.Edited || m.Text != "oi!" {
		t.Fatalf("author's edit not applied: %+v", m)
	}
	if u, ok := (<-ch).(store.MessageUpdate); !ok || u.Text != "oi!" {
		t.Fatalf("want a MessageUpdate after the edit, got %#v", u)
	}
	b.handle(protocolEvent(group, maria, false, revoke("G1")))
	if m, _ := b.Store.Get(group, "G1"); !m.Deleted || m.Text != "" {
		t.Fatalf("author's delete not applied: %+v", m)
	}
}

func TestHandleStoresAndPublishesOnce(t *testing.T) {
	b := newTestBot(t)
	ch, unsub := b.Subscribe()
	defer unsub()
	c, _ := types.ParseJID(maria)
	evt := &events.Message{
		Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: c, Sender: c}, ID: "IN1", PushName: "Maria", Timestamp: time.Now()},
		Message: &waE2E.Message{Conversation: proto.String("hello")},
	}
	b.handle(evt)
	b.handle(evt)
	got := (<-ch).(store.Message)
	if got.Seq != 1 || got.Text != "hello" || got.Chat != maria || got.PushName != "Maria" || len(got.Raw) == 0 || got.RawSender != maria {
		t.Fatalf("published %+v", got)
	}
	select {
	case v := <-ch:
		t.Fatalf("duplicate published: %#v", v)
	default:
	}
}

func TestAuditedRecordsFailures(t *testing.T) {
	b := newTestBot(t)
	ctx := context.Background()
	var in InputError
	if _, err := b.SendText(ctx, "api", maria, "  ", SendOptions{}); !errors.As(err, &in) {
		t.Fatalf("empty text err = %v", err)
	}
	if _, err := b.SendText(ctx, "cli", maria, "hi", SendOptions{}); err == nil || err.Error() != "not logged in" {
		t.Fatalf("offline send err = %v", err)
	}
	log, err := b.Store.AuditLog(10, 0)
	if err != nil || len(log) != 2 {
		t.Fatalf("audit = %+v, %v", log, err)
	}
	if log[0].Actor != "cli" || log[0].OK || log[1].Error != "text is empty" || log[1].Target != maria {
		t.Fatalf("audit entries = %+v", log)
	}
}

func TestCappedBody(t *testing.T) {
	read := func(body string, left int64) (string, error) {
		data, err := io.ReadAll(&cappedBody{rc: io.NopCloser(strings.NewReader(body)), left: left})
		return string(data), err
	}
	if got, err := read("0123456789", 10); err != nil || got != "0123456789" {
		t.Fatalf("exactly at the cap: %q, %v", got, err)
	}
	if got, err := read("01234", 10); err != nil || got != "01234" {
		t.Fatalf("under the cap: %q, %v", got, err)
	}
	var in InputError
	if _, err := read("0123456789", 5); !errors.As(err, &in) {
		t.Fatalf("over the cap: err = %v, want InputError", err)
	}
}

type fakeTransport struct{}

func (fakeTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("data"))}, nil
}

func TestLimitedTransport(t *testing.T) {
	tr := limitedTransport{fakeTransport{}}
	get, _ := http.NewRequest(http.MethodGet, "https://mmg.example/x", nil)
	resp, _ := tr.RoundTrip(get)
	if c, ok := resp.Body.(*cappedBody); !ok || c.left != maxMediaDownload {
		t.Fatalf("GET body = %T, want a cappedBody at %d", resp.Body, maxMediaDownload)
	}
	post, _ := http.NewRequest(http.MethodPost, "https://mmg.example/x", nil)
	if resp, _ := tr.RoundTrip(post); func() bool { _, ok := resp.Body.(*cappedBody); return ok }() {
		t.Fatal("uploads must not be capped")
	}
}

func TestDownloadMediaChecksBeforeNetwork(t *testing.T) {
	b := newTestBot(t)
	big, _, _ := b.Store.Save(store.Message{ID: "D1", Chat: ana, Sender: ana, Type: "video", Raw: []byte{1}, Timestamp: time.Now(),
		Media: &store.Media{Mimetype: "video/mp4", Size: maxMediaDownload + 1}})
	text, _, _ := b.Store.Save(store.Message{ID: "D2", Chat: ana, Sender: ana, Type: "text", Text: "no media", Timestamp: time.Now()})
	var in InputError
	if _, _, err := b.DownloadMedia(context.Background(), big.Seq); !errors.As(err, &in) {
		t.Fatalf("oversized: %v", err)
	}
	for _, seq := range []int64{text.Seq, 999} {
		if _, _, err := b.DownloadMedia(context.Background(), seq); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("seq %d: %v, want ErrNotFound", seq, err)
		}
	}
}

func TestWebhookSignsAndFilters(t *testing.T) {
	got := make(chan *http.Request, 2)
	bodies := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		got <- r
		bodies <- string(data)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	no := false
	w := NewWebhook(ctx, srv.URL, "s3cret", store.Filter{FromMe: &no}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.Deliver("message", store.Message{Seq: 1, ID: "W0", Chat: john, FromMe: true, Type: "text"})
	w.Deliver("message", store.Message{Seq: 2, ID: "W1", Chat: maria, Sender: maria, Type: "text", Text: "oi"})
	select {
	case r := <-got:
		body := <-bodies
		if !strings.Contains(body, `"id":"W1"`) || strings.Contains(body, "raw") {
			t.Fatalf("body = %s", body)
		}
		want := "sha256=" + Sign("s3cret", r.Header.Get("X-Wame-Timestamp"), []byte(body))
		if r.Header.Get("X-Wame-Signature") != want || r.Header.Get("X-Wame-Event") != "message" {
			t.Fatalf("headers = %v", r.Header)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery")
	}
	select {
	case <-got:
		t.Fatal("the from_me message should have been filtered out")
	case <-time.After(200 * time.Millisecond):
	}
}
