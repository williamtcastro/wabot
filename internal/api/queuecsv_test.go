package api

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/williamtcastro/wame/internal/auth"
	"github.com/williamtcastro/wame/internal/store"
)

func doCSV(h *Handler, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", target, strings.NewReader(body))
	r.Host = "127.0.0.1:8080"
	r.RemoteAddr = "127.0.0.1:50000"
	r.Header.Set("Content-Type", "text/csv; charset=utf-8")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func errorOf(w *httptest.ResponseRecorder) string {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	return e.Error
}

type queued struct {
	Batch    string             `json:"batch"`
	Queued   int                `json:"queued"`
	Replayed int                `json:"replayed"`
	Items    []store.OutboxItem `json:"items"`
}

func TestQueueCSV(t *testing.T) {
	b := newTestBot(t)
	h := newHandler(b, "s3cret-owner-secret")
	sender := bearer(issue(t, b, "promo-bot", []auth.Scope{auth.Send}, nil, 0))

	body := "\xef\xbb\xbfreply_to,Text,TO,send_at\r\n" +
		",\"Hi Maria, see you at 9\",5511999999999,2026-10-13T09:00:00-03:00\r\n" +
		"\r\n" +
		",\"Hi John\r\nsecond line\", 5511988887777 ,\r\n" +
		",,552187654321,\r\n"
	w := doCSV(h, "/queue?batch=promo-csv&text=Hello+from+the+store", body, sender)
	var res queued
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &res) != nil || res.Queued != 3 || res.Batch != "promo-csv" || res.Items[0].Text != "" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	for i, want := range []struct{ to, text string }{
		{"5511999999999", "Hi Maria, see you at 9"},
		{"5511988887777", "Hi John\nsecond line"},
		{"552187654321", "Hello from the store"},
	} {
		it, err := b.Store.OutboxItem(res.Items[i].ID)
		if err != nil || it.To != want.to || it.Text != want.text || it.Actor != "promo-bot" {
			t.Errorf("item %d = %+v, %v; want %+v", i, it, err, want)
		}
	}

	for _, c := range []struct {
		name, target, body string
		code               int
		want               string
	}{
		{"unknown column", "/queue", "to,text,nome\n5511999999999,hi,Maria\n", 400, `unknown columns "nome"`},
		{"missing to column", "/queue", "text\nhi\n", 400, `"to" column is required`},
		{"empty to", "/queue", "to,text\n5511999999999,hi\n,hi\n", 400, "line 3: invalid recipient"},
		{"bad number after a multi-line text", "/queue", "to,text\n5511999999999,\"a\nb\nc\"\n12,hi\n", 400, "line 5: invalid recipient"},
		{"no text and no default", "/queue", "to\n5511999999999\n", 400, "line 2: text is empty"},
		{"value outside the columns", "/queue", "to,text\n5511999999999,hi,extra\n", 400, "line 2: field 3 is outside the named columns"},
		{"bad send_at column", "/queue", "to,text,send_at\n5511999999999,hi,soon\n", 400, "line 2: send_at"},
		{"bad send_at default", "/queue?send_at=soon", "to,text\n5511999999999,hi\n", 400, "?send_at"},
		{"bad batch", "/queue?batch=no%20spaces", "to,text\n5511999999999,hi\n", 400, "batch must be"},
		{"header only", "/queue", "to,text\n", 400, "no messages given"},
		{"empty body", "/queue", "", 400, "empty"},
	} {
		if w := doCSV(h, c.target, c.body, sender); w.Code != c.code || !strings.Contains(errorOf(w), c.want) {
			t.Errorf("%s: %d %s, want %d with %q", c.name, w.Code, w.Body, c.code, c.want)
		}
	}

	var many strings.Builder
	many.WriteString("to,text\n")
	for i := range 1001 {
		fmt.Fprintf(&many, "55119%08d,hi\n", i)
	}
	if w := doCSV(h, "/queue", many.String(), sender); w.Code != 400 || !strings.Contains(w.Body.String(), "line 1002: at most 1000 messages") {
		t.Fatalf("1001 rows: %d %s", w.Code, w.Body)
	}
	huge := "to,text\n5511999999999,\"" + strings.Repeat("x", maxQueueBody) + "\"\n"
	if w := doCSV(h, "/queue", huge, sender); w.Code != 413 {
		t.Fatalf("huge body: %d %s", w.Code, w.Body)
	}
}

func TestQueueCSVAllowlist(t *testing.T) {
	b := newTestBot(t)
	h := newHandler(b, "s3cret-owner-secret")
	scoped := bearer(issue(t, b, "maria-only", []auth.Scope{auth.Send}, []string{maria}, 0))
	body := "to,text\n5511999999999,hi Maria\n5511988887777,hi John\n"
	if w := doCSV(h, "/queue", body, scoped); w.Code != 403 || !strings.Contains(w.Body.String(), "line 3: "+errChatForbidden) {
		t.Fatalf("allowlist: %d %s", w.Code, w.Body)
	}
	if n, _ := b.Store.CountOutbox(store.OutboxFilter{}); n.Queued != 0 {
		t.Fatalf("nothing may be queued when one row is refused: %+v", n)
	}
	var res queued
	w := doCSV(h, "/queue", "to,text\n5511999999999,hi Maria\n", scoped)
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &res) != nil || res.Items[0].To != maria {
		t.Fatalf("allowed chat: %d %s", w.Code, w.Body)
	}
}

func TestQueueCSVIdempotency(t *testing.T) {
	b := newTestBot(t)
	h := newHandler(b, "s3cret-owner-secret")
	keyed := bearer(issue(t, b, "promo-bot", []auth.Scope{auth.Send}, nil, 0))
	keyed["Idempotency-Key"] = "import-1"
	body := "to,text\n5511999999999,hi Maria\n5511988887777,hi John\n"
	var first, again queued
	if w := doCSV(h, "/queue?batch=promo", body, keyed); w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &first) != nil {
		t.Fatalf("first: %d %s", w.Code, w.Body)
	}
	if w := doCSV(h, "/queue?batch=promo", body, keyed); w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &again) != nil ||
		again.Items[0].ID != first.Items[0].ID || again.Items[1].ID != first.Items[1].ID || first.Replayed != 0 || again.Replayed != 2 {
		t.Fatalf("a retry must return the same items, counted as replayed: %d %s", w.Code, w.Body)
	}
	if n, _ := b.Store.CountOutbox(store.OutboxFilter{}); n.Queued != 2 {
		t.Fatalf("a retry queued again: %+v", n)
	}
	if w := doCSV(h, "/queue?batch=promo", "to,text\n5511999999999,changed\n5511988887777,hi John\n", keyed); w.Code != 409 {
		t.Fatalf("same key, other rows: %d %s", w.Code, w.Body)
	}
	if w := doCSV(h, "/queue?batch=other", body, keyed); w.Code != 409 {
		t.Fatalf("same key, other batch: %d %s", w.Code, w.Body)
	}
	longer := body + "552187654321,hi Ana\n"
	if w := doCSV(h, "/queue?batch=promo", strings.Replace(longer, "hi John", "hi again", 1), keyed); w.Code != 409 {
		t.Fatalf("same key, a changed row and a new one: %d %s", w.Code, w.Body)
	}
	if n, _ := b.Store.CountOutbox(store.OutboxFilter{}); n.Queued != 2 {
		t.Fatalf("a 409 must not keep the new row: %+v", n)
	}
	if w := doCSV(h, "/queue?batch=promo&send_at="+url.QueryEscape(time.Now().Add(48*time.Hour).Format(time.RFC3339)), body, keyed); w.Code != 409 {
		t.Fatalf("same key, other send_at: %d %s", w.Code, w.Body)
	}
}

func TestQueueCSVFormats(t *testing.T) {
	b := newTestBot(t)
	h := newHandler(b, "s3cret-owner-secret")
	sender := bearer(issue(t, b, "promo-bot", []auth.Scope{auth.Send}, nil, 0))
	sender["Content-Type"] = "application/csv"
	var res queued
	w := doCSV(h, "/queue", "to;text;;\n5511999999999;Olá Maria, tudo bem?;;\n   \n5511988887777;Hi John\n", sender)
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &res) != nil || res.Queued != 2 {
		t.Fatalf("application/csv with ';' and empty columns: %d %s", w.Code, w.Body)
	}
	if it, _ := b.Store.OutboxItem(res.Items[0].ID); it.Text != "Olá Maria, tudo bem?" {
		t.Fatalf("text = %q", it.Text)
	}
}
