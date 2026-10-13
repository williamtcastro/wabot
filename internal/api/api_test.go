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

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/williamtcastro/wame/internal/auth"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/williamtcastro/wame/docs"
	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
)

const (
	maria = "5511999999999@s.whatsapp.net"
	john  = "5511988887777@s.whatsapp.net"
	ana   = "552187654321@s.whatsapp.net"
)

func newTestBot(t *testing.T) *bot.Bot {
	t.Helper()
	b, err := bot.New(context.Background(), filepath.Join(t.TempDir(), "wame.db"), waLog.Noop, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	r.RemoteAddr = "127.0.0.1:50000"
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
	h := newHandler(newTestBot(t), "s3cret")
	for _, c := range []struct {
		target string
		hdr    map[string]string
		want   int
	}{
		{"/status", nil, 401},
		{"/status", map[string]string{"Authorization": "Bearer wrong"}, 401},
		{"/status", map[string]string{"Authorization": "Bearer s3cret"}, 200},
		{"/status", map[string]string{"Authorization": "bearer s3cret"}, 200},
		{"/status", map[string]string{"Authorization": "s3cret"}, 401},
		{"/status?token=s3cret", nil, 401},
		{"/qr?token=s3cret", map[string]string{"Host": "wame.example"}, 404},
		{"/status?token=wrong", map[string]string{"Authorization": "Bearer s3cret"}, 200},
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
	h := newHandler(newTestBot(t), "")
	for _, c := range []struct {
		hdr  map[string]string
		want int
	}{
		{nil, 200},
		{map[string]string{"Host": "localhost:8080"}, 200},
		{map[string]string{"Host": "[::1]:8080"}, 200},
		{map[string]string{"Host": "localhost:8080", "Origin": "http://localhost:8080"}, 200},
		{map[string]string{"Host": "wame.example:8080"}, 403},
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
	if w := do(h, "GET", "/docs", map[string]string{"Host": "wame.example"}); w.Code != 200 {
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
	h := newHandler(b, "")
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
	w := do(newHandler(b, ""), "GET", "/messages/wait?since=2&timeout=1", nil)
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
	h := newHandler(b, "")
	stream := func(target string, hdr map[string]string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		r := httptest.NewRequestWithContext(ctx, "GET", target, nil)
		r.Host = "127.0.0.1:8080"
		r.RemoteAddr = "127.0.0.1:50000"
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
	w := do(newHandler(b, ""), "GET", "/audit?limit=1", nil)
	var got []store.AuditEntry
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got) != 1 || got[0].Actor != "tui" || got[0].OK {
		t.Fatalf("audit = %s (%v)", w.Body, err)
	}
}

func TestMessagesSearch(t *testing.T) {
	b := newTestBot(t)
	seed(t, b)
	h := newHandler(b, "tok")
	rec := do(h, "GET", "/messages?q=TUDO&from_me=true", map[string]string{"Authorization": "Bearer tok"})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got := messageSeqs(t, rec.Body.Bytes()); len(got) != 1 || got[0] != 3 {
		t.Fatalf("q=TUDO&from_me=true -> seqs %v, want [3]", got)
	}
}

func newHandler(b *bot.Bot, owner string) http.Handler {
	return NewHandler(b, &auth.Authenticator{Store: b.Store, Owner: owner}, "test")
}

func issue(t *testing.T, b *bot.Bot, name string, scopes []auth.Scope, chats []string, rate int) string {
	t.Helper()
	plain, _, err := auth.Issue(b.Store, auth.Request{Name: name, Scopes: scopes, Chats: chats, Rate: rate})
	if err != nil {
		t.Fatal(err)
	}
	return plain
}

func bearer(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

func TestScopedTokens(t *testing.T) {
	b := newTestBot(t)
	seed(t, b)
	reader := issue(t, b, "reader", []auth.Scope{auth.Read}, nil, 0)
	mariaBot := issue(t, b, "maria-bot", []auth.Scope{auth.Read, auth.Send}, []string{maria}, 0)
	h := newHandler(b, "")

	if rec := do(h, "GET", "/status", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("once a token exists, no-token requests (even from localhost) must be 401, got %d", rec.Code)
	}
	if rec := do(h, "GET", "/messages", bearer(reader)); rec.Code != 200 {
		t.Fatalf("reader GET /messages = %d", rec.Code)
	}
	for _, c := range []struct{ method, path string }{
		{"POST", "/send"}, {"GET", "/audit"}, {"GET", "/tokens"}, {"POST", "/logout"},
	} {
		if rec := do(h, c.method, c.path, bearer(reader)); rec.Code != http.StatusForbidden {
			t.Errorf("reader %s %s = %d, want 403", c.method, c.path, rec.Code)
		}
	}

	rec := do(h, "GET", "/messages", bearer(mariaBot))
	for _, s := range messageSeqs(t, rec.Body.Bytes()) {
		m, _ := b.Store.GetBySeq(s)
		if m.Chat != maria {
			t.Fatalf("restricted token saw a message from %s", m.Chat)
		}
	}
	if rec := do(h, "GET", "/messages?chat="+john, bearer(mariaBot)); rec.Code != http.StatusForbidden {
		t.Errorf("restricted ?chat=other = %d, want 403", rec.Code)
	}
	for _, path := range []string{"/contacts", "/groups"} {
		if rec := do(h, "GET", path, bearer(mariaBot)); rec.Code != http.StatusForbidden {
			t.Errorf("restricted %s = %d, want 403", path, rec.Code)
		}
	}
	var chats []store.Chat
	_ = json.Unmarshal(do(h, "GET", "/chats", bearer(mariaBot)).Body.Bytes(), &chats)
	if len(chats) != 1 || chats[0].JID != maria {
		t.Errorf("restricted /chats = %+v", chats)
	}
	johnMsg := b.Store.Messages(john, 1)[0]
	if rec := do(h, "GET", fmt.Sprintf("/media/%d", johnMsg.Seq), bearer(mariaBot)); rec.Code != http.StatusNotFound {
		t.Errorf("restricted /media of another chat = %d, want 404", rec.Code)
	}
	if rec := doJSON(h, "POST", "/send", `{"to":"`+john+`","text":"hi"}`, bearer(mariaBot)); rec.Code != http.StatusForbidden {
		t.Errorf("restricted send to another chat = %d, want 403", rec.Code)
	}
	if rec := doJSON(h, "POST", "/send", `{"to":"`+maria+`","text":"hi"}`, bearer(mariaBot)); rec.Code == http.StatusForbidden {
		t.Errorf("send to the allowed chat must pass the token checks, got 403")
	}
	if rec := doJSON(h, "POST", "/react", `{"chat":"`+john+`","id":"M2","emoji":"👍"}`, bearer(mariaBot)); rec.Code != http.StatusForbidden {
		t.Errorf("restricted react in another chat = %d, want 403", rec.Code)
	}
}

func TestTokenAdminEndpointsAndRevocation(t *testing.T) {
	b := newTestBot(t)
	admin := issue(t, b, "boss", []auth.Scope{auth.Admin}, nil, 0)
	h := newHandler(b, "")
	rec := doJSON(h, "POST", "/tokens", `{"name":"helper","scopes":["read"],"expires_in":"24h"}`, bearer(admin))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	var created struct{ Token string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if rec := do(h, "GET", "/status", bearer(created.Token)); rec.Code != 200 {
		t.Fatalf("new token = %d", rec.Code)
	}
	if strings.Contains(do(h, "GET", "/tokens", bearer(admin)).Body.String(), created.Token) {
		t.Fatal("GET /tokens must never return secrets")
	}
	if rec := do(h, "DELETE", "/tokens/helper", bearer(admin)); rec.Code != 200 {
		t.Fatalf("revoke = %d", rec.Code)
	}
	if rec := do(h, "GET", "/status", bearer(created.Token)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token = %d, want 401", rec.Code)
	}
}

func TestPerTokenRate(t *testing.T) {
	b := newTestBot(t)
	slow := issue(t, b, "slow", []auth.Scope{auth.Send}, nil, 1)
	h := newHandler(b, "")
	body := `{"to":"` + maria + `","text":"hi"}`
	if rec := doJSON(h, "POST", "/send", body, bearer(slow)); rec.Code == http.StatusTooManyRequests {
		t.Fatal("first send must not be rate limited")
	}
	if rec := doJSON(h, "POST", "/send", body, bearer(slow)); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second send within a minute at rate 1 = %d, want 429", rec.Code)
	}
}

func doJSON(h http.Handler, method, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Host = "127.0.0.1:8080"
	r.RemoteAddr = "127.0.0.1:50000"
	r.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

type headerTransport struct{ token string }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if h.token != "" {
		r.Header.Set("Authorization", "Bearer "+h.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestMCPOverHTTPUsesTokenScopes(t *testing.T) {
	b := newTestBot(t)
	seed(t, b)
	reader := issue(t, b, "reader", []auth.Scope{auth.Read}, nil, 0)
	srv := httptest.NewServer(newHandler(b, ""))
	defer srv.Close()
	ctx := context.Background()

	connect := func(token string) (*mcp.ClientSession, error) {
		tr := &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: headerTransport{token}}, MaxRetries: -1}
		return mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, tr, nil)
	}
	if _, err := connect(""); err == nil {
		t.Fatal("with tokens configured, /mcp without a token must be refused")
	}
	cs, err := connect(reader)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "read_messages", Arguments: map[string]any{"chat": maria}})
	if err != nil || res.IsError {
		t.Fatalf("reader read_messages: %v %+v", err, res)
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"to": maria, "text": "hi"}})
	if err != nil || !res.IsError {
		t.Fatalf("reader must not send over MCP: %v %+v", err, res)
	}
	entries, _ := b.Store.AuditLog(10, 0)
	for _, e := range entries {
		if e.Action == "send" {
			t.Fatalf("a refused send must not reach the bot: %+v", e)
		}
	}
}

func TestStatsNeedsAdmin(t *testing.T) {
	b := newTestBot(t)
	h := newHandler(b, "s3cret-owner-secret")
	reader := issue(t, b, "reader", []auth.Scope{auth.Read}, nil, 0)
	if w := do(h, "GET", "/stats", map[string]string{"Authorization": "Bearer " + reader}); w.Code != 403 {
		t.Fatalf("read token: %d", w.Code)
	}
	w := do(h, "GET", "/stats", map[string]string{"Authorization": "Bearer s3cret-owner-secret"})
	var s Stats
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &s) != nil || s.Version != "test" || s.Started.IsZero() || s.Webhook != nil || s.Limits.AutoReply.PerMinute != 6 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func doBody(h http.Handler, method, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Host = "127.0.0.1:8080"
	r.RemoteAddr = "127.0.0.1:50000"
	r.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestSettingsEndpoints(t *testing.T) {
	b := newTestBot(t)
	if _, err := b.InitSettings(bot.Settings{Rate: 20, ChatGap: time.Second, MaxWait: 30 * time.Second, AutoReply: true}); err != nil {
		t.Fatal(err)
	}
	h := newHandler(b, "s3cret-owner-secret")
	owner := map[string]string{"Authorization": "Bearer s3cret-owner-secret"}
	reader := issue(t, b, "reader", []auth.Scope{auth.Read}, nil, 0)
	if w := doBody(h, "PATCH", "/settings", `{"rate":5}`, map[string]string{"Authorization": "Bearer " + reader}); w.Code != 403 {
		t.Fatalf("read token: %d", w.Code)
	}

	w := doBody(h, "PATCH", "/settings", `{"rate":5,"auto_reply":false,"chat_gap":"2s"}`, owner)
	var view []bot.SettingView
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil || view[0].Value != "5" || view[3].Value != "false" || view[1].Value != "2s" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if b.AutoReplyOn() || b.Limiter.Stats().PerMinute != 5 {
		t.Fatal("not applied")
	}
	if w := doBody(h, "PATCH", "/settings", `{"rate":"lots"}`, owner); w.Code != 400 {
		t.Fatalf("bad value: %d %s", w.Code, w.Body)
	}
	if w := doBody(h, "PATCH", "/settings", `{"rate":null}`, owner); w.Code != 200 || b.Limiter.Stats().PerMinute != 20 {
		t.Fatalf("reset: %d %s", w.Code, w.Body)
	}
	if w := doBody(h, "POST", "/webhook/test", `{}`, owner); w.Code != 400 {
		t.Fatalf("webhook test without a webhook: %d %s", w.Code, w.Body)
	}
	if w := doBody(h, "POST", "/webhook/replay", `{"delivery":"1-message-abcd"}`, owner); w.Code != 400 {
		t.Fatalf("replay without a webhook: %d %s", w.Code, w.Body)
	}
}

func TestStatsCountsCallers(t *testing.T) {
	b := newTestBot(t)
	h := newHandler(b, "s3cret-owner-secret")
	claude := issue(t, b, "claude", []auth.Scope{auth.Read}, nil, 0)
	for range 3 {
		do(h, "GET", "/status", map[string]string{"Authorization": "Bearer " + claude})
	}
	w := do(h, "GET", "/stats", map[string]string{"Authorization": "Bearer s3cret-owner-secret"})
	var s Stats
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &s) != nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	got := map[string]int64{}
	for _, c := range s.Callers {
		got[c.Name] = c.Requests
	}
	if got["claude"] != 3 || got["api"] != 1 || s.Callers[0].LastIP != "127.0.0.1" {
		t.Fatalf("callers = %+v", s.Callers)
	}
}
