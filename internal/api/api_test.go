package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/williamtcastro/wabot/docs"
	"github.com/williamtcastro/wabot/internal/bot"
	"github.com/williamtcastro/wabot/internal/store"
)

const (
	maria = "5511999999999@s.whatsapp.net"
	john  = "5511988887777@s.whatsapp.net"
	ana   = "552187654321@s.whatsapp.net"
)

func newTestBot(t *testing.T) *bot.Bot {
	t.Helper()
	b, err := bot.New(context.Background(), filepath.Join(t.TempDir(), "wabot.db"), waLog.Noop, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func seed(t *testing.T, b *bot.Bot) {
	t.Helper()
	base := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	for i, m := range []store.Message{
		{ID: "M1", Chat: maria, Sender: maria, PushName: "Maria", Type: "text", Text: "oi"},
		{ID: "M2", Chat: john, Sender: john, PushName: "John", Type: "text", Text: "hello"},
		{ID: "M3", Chat: maria, Sender: ana, FromMe: true, Type: "text", Text: "tudo bem?"},
	} {
		m.Timestamp = base.Add(time.Duration(i) * time.Minute)
		if _, ok, err := b.Store.Save(m); !ok || err != nil {
			t.Fatal(err)
		}
	}
}

func do(h http.Handler, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.Host = "127.0.0.1:8080"
	for k, v := range hdr {
		if k == "Host" {
			r.Host = v
			continue
		}
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestTokenAuth(t *testing.T) {
	h := NewHandler(newTestBot(t), "s3cret")
	for _, c := range []struct {
		target string
		hdr    map[string]string
		want   int
	}{
		{"/status", nil, 401},
		{"/status", map[string]string{"Authorization": "Bearer wrong"}, 401},
		{"/status", map[string]string{"Authorization": "Bearer s3cret"}, 200},
		{"/status?token=s3cret", nil, 200},
		{"/status?token=s3cret", map[string]string{"Host": "wabot.example"}, 200},
		{"/messages", map[string]string{"Authorization": "Bearer s3cret"}, 200},
		{"/docs", nil, 200},
		{"/openapi.yaml", nil, 200},
	} {
		if w := do(h, "GET", c.target, c.hdr); w.Code != c.want {
			t.Errorf("GET %s %v = %d, want %d", c.target, c.hdr, w.Code, c.want)
		}
	}
	if w := do(h, "GET", "/openapi.yaml", nil); !bytes.Equal(w.Body.Bytes(), docs.OpenAPI) || len(docs.OpenAPI) == 0 {
		t.Error("/openapi.yaml must serve the embedded spec")
	}
}

func TestLocalOnlyWithoutToken(t *testing.T) {
	h := NewHandler(newTestBot(t), "")
	for _, c := range []struct {
		hdr  map[string]string
		want int
	}{
		{nil, 200},
		{map[string]string{"Host": "localhost:8080"}, 200},
		{map[string]string{"Host": "[::1]:8080"}, 200},
		{map[string]string{"Host": "localhost:8080", "Origin": "http://localhost:8080"}, 200},
		{map[string]string{"Host": "wabot.example:8080"}, 403},
		{map[string]string{"Host": "192.168.0.10:8080"}, 403},
		{map[string]string{"Origin": "http://evil.example"}, 403},
		{map[string]string{"Origin": "null"}, 403},
		{map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
	} {
		w := do(h, "GET", "/status", c.hdr)
		if w.Code != c.want {
			t.Errorf("%v = %d, want %d", c.hdr, w.Code, c.want)
		}
		if c.want == 403 && !strings.Contains(w.Body.String(), `"error"`) {
			t.Errorf("403 body = %s", w.Body)
		}
	}
	if w := do(h, "GET", "/docs", map[string]string{"Host": "wabot.example"}); w.Code != 200 {
		t.Errorf("/docs is public, got %d", w.Code)
	}
}

func TestIdempotency(t *testing.T) {
	s := idempotency{entries: map[string]*idemEntry{}}
	ctx := context.Background()
	calls := 0
	send := func() (store.Message, error) {
		calls++
		return store.Message{Seq: int64(calls), ID: fmt.Sprint("ID", calls)}, nil
	}
	first, err := s.do(ctx, "send", "k1", fingerprint(maria, "oi"), send)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.do(ctx, "send", "k1", fingerprint(maria, "oi"), send)
	if err != nil || again.ID != first.ID || calls != 1 {
		t.Fatalf("repeat = %+v, %v (calls %d)", again, err, calls)
	}
	if _, err := s.do(ctx, "send", "k1", fingerprint(maria, "different"), send); !errors.Is(err, errConflict) {
		t.Fatalf("different request with the same key: %v, want errConflict", err)
	}
	if _, err := s.do(ctx, "file", "k1", fingerprint(maria, "different"), send); err != nil || calls != 2 {
		t.Fatalf("keys are scoped per endpoint: %v (calls %d)", err, calls)
	}
	if _, err := s.do(ctx, "send", "", "", send); err != nil || calls != 3 {
		t.Fatal("no key must always run")
	}
	if _, err := s.do(ctx, "send", strings.Repeat("k", 201), "", send); err == nil {
		t.Fatal("overlong key must be rejected")
	}

	_, err = s.do(ctx, "send", "boom", "fp", func() (store.Message, error) { panic("kaboom") })
	if err == nil || !strings.Contains(err.Error(), "internal error: kaboom") {
		t.Fatalf("panic err = %v", err)
	}
	if _, err := s.do(ctx, "send", "boom", "fp", send); err != nil || calls != 4 {
		t.Fatalf("a failed attempt must not be cached: %v (calls %d)", err, calls)
	}
}

func TestWriteErrorCodes(t *testing.T) {
	for _, c := range []struct {
		err  error
		want int
	}{
		{bot.BadInput("nope"), 400},
		{fmt.Errorf("wrapped: %w", store.ErrNotFound), 404},
		{errConflict, 409},
		{bot.RateLimitError{RetryAfter: 2 * time.Second}, 429},
		{context.DeadlineExceeded, 504},
		{errors.New("whatsapp said no"), 502},
	} {
		w := httptest.NewRecorder()
		writeError(w, c.err)
		if w.Code != c.want {
			t.Errorf("%v = %d, want %d", c.err, w.Code, c.want)
		}
		if c.want == 429 && w.Header().Get("Retry-After") != "3" {
			t.Errorf("Retry-After = %q", w.Header().Get("Retry-After"))
		}
	}
}

func messageSeqs(t *testing.T, body []byte) []int64 {
	t.Helper()
	var msgs []store.Message
	if err := json.Unmarshal(body, &msgs); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	out := []int64{}
	for _, m := range msgs {
		out = append(out, m.Seq)
	}
	return out
}

func TestMessagesSinceCursor(t *testing.T) {
	b := newTestBot(t)
	seed(t, b)
	h := NewHandler(b, "")
	for target, want := range map[string][]int64{
		"/messages":                                 {3, 2, 1},
		"/messages?since=1":                         {2, 3},
		"/messages?since=0&limit=2":                 {1, 2},
		"/messages?since=3":                         {},
		"/messages?chat=" + maria:                   {3, 1},
		"/messages?chat=" + maria + "&since=1":      {3},
		"/messages?from_me=false&type=text&since=0": {1, 2},
	} {
		w := do(h, "GET", target, nil)
		if got := messageSeqs(t, w.Body.Bytes()); w.Code != 200 || !slices.Equal(got, want) {
			t.Errorf("%s = %d %v, want %v", target, w.Code, got, want)
		}
	}
	if w := do(h, "GET", "/messages?type=gif", nil); w.Code != 400 {
		t.Errorf("bad filter = %d, want 400", w.Code)
	}
	w := do(h, "GET", "/messages?since=2", nil)
	if strings.Contains(w.Body.String(), "raw") {
		t.Errorf("raw fields leaked into JSON: %s", w.Body)
	}
}

func TestMessagesWaitReturnsBacklog(t *testing.T) {
	b := newTestBot(t)
	seed(t, b)
	w := do(NewHandler(b, ""), "GET", "/messages/wait?since=2&timeout=1", nil)
	var got struct {
		Messages  []store.Message `json:"messages"`
		NextSince int64           `json:"next_since"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got.Messages) != 1 || got.NextSince != 3 {
		t.Fatalf("wait = %s (%v)", w.Body, err)
	}
}

func sseIDs(body string) []string {
	var ids []string
	for _, line := range strings.Split(body, "\n") {
		if id, ok := strings.CutPrefix(line, "id: "); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

func TestEventsLastEventIDReplay(t *testing.T) {
	b := newTestBot(t)
	seed(t, b)
	h := NewHandler(b, "")
	stream := func(target string, hdr map[string]string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		r := httptest.NewRequestWithContext(ctx, "GET", target, nil)
		r.Host = "127.0.0.1:8080"
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
			t.Fatalf("Content-Type = %q", ct)
		}
		return w.Body.String()
	}
	for _, c := range []struct {
		target string
		hdr    map[string]string
		want   []string
	}{
		{"/events", nil, nil},
		{"/events?since=0", nil, []string{"1", "2", "3"}},
		{"/events?since=0", map[string]string{"Last-Event-ID": "1"}, []string{"2", "3"}},
		{"/events", map[string]string{"Last-Event-ID": "2"}, []string{"3"}},
		{"/events?since=0", map[string]string{"Last-Event-ID": "junk"}, []string{"1", "2", "3"}},
		{"/events?since=0&chat=" + maria, map[string]string{"Last-Event-ID": "1"}, []string{"3"}},
		{"/events?since=0&events=status", nil, nil},
	} {
		body := stream(c.target, c.hdr)
		if got := sseIDs(body); !slices.Equal(got, c.want) {
			t.Errorf("%s %v: ids %v, want %v\n%s", c.target, c.hdr, got, c.want, body)
		}
	}
}

func TestAuditEndpoint(t *testing.T) {
	b := newTestBot(t)
	b.Audit("api", "logout", "", "", nil)
	b.Audit("tui", "send", maria, "text, 2 chars", errors.New("not logged in"))
	w := do(NewHandler(b, ""), "GET", "/audit?limit=1", nil)
	var got []store.AuditEntry
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got) != 1 || got[0].Actor != "tui" || got[0].OK {
		t.Fatalf("audit = %s (%v)", w.Body, err)
	}
}
