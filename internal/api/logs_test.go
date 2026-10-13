package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/williamtcastro/wame/internal/auth"
	"github.com/williamtcastro/wame/internal/bot"
)

func TestLogsEndpoint(t *testing.T) {
	const owner = "s3cret-owner-secret"
	logs := bot.NewLogBuffer(100)
	logs.Scrub(owner, "https://hooks.example.com/x?k=hush")
	b, err := bot.New(t.Context(), filepath.Join(t.TempDir(), "wame.db"), waLog.Noop, slog.New(logs.Wrap(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	b.Logs = logs
	h := newHandler(b, owner)
	ownerHdr := bearer(owner)

	b.Log.Info("webhook configured", "url", "https://hooks.example.com/x?k=hush", "chat", maria)
	b.Log.Warn("delivery failed", "err", fmt.Errorf("Post https://hooks.example.com/x?k=hush: refused"), "secret", "hush")
	b.Log.Error("bad owner", "detail", "got "+owner)

	reader := issue(t, b, "reader", []auth.Scope{auth.Read}, nil, 0)
	if w := do(h, "GET", "/logs", bearer(reader)); w.Code != 403 {
		t.Fatalf("read token: %d", w.Code)
	}
	sender := issue(t, b, "sender", []auth.Scope{auth.Read, auth.Send}, []string{"5511988887777"}, 0)
	if w := do(h, "GET", "/logs", bearer(sender)); w.Code != 403 {
		t.Fatalf("send token limited to chats: %d", w.Code)
	}

	get := func(query string) LogPage {
		t.Helper()
		w := do(h, "GET", "/logs"+query, ownerHdr)
		var p LogPage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &p) != nil {
			t.Fatalf("GET /logs%s: %d %s", query, w.Code, w.Body)
		}
		if body := w.Body.String(); strings.Contains(body, "hush") || strings.Contains(body, owner) || strings.Contains(body, reader) {
			t.Fatalf("a secret reached /logs: %s", body)
		}
		return p
	}

	all := get("")
	if len(all.Entries) < 5 || all.Entries[0].ID < all.Entries[1].ID {
		t.Fatalf("entries = %+v", all.Entries)
	}
	for _, e := range all.Entries {
		if e.Msg == "http" && e.Attrs["status"] == "200" {
			t.Fatalf("polling /logs must not fill the buffer with its own requests: %+v", e)
		}
	}
	warn := get("?level=warn")
	var msgs []string
	for _, e := range warn.Entries {
		msgs = append(msgs, e.Msg)
		if e.Level != "warn" && e.Level != "error" {
			t.Fatalf("level=warn returned %+v", e)
		}
	}
	if !strings.Contains(strings.Join(msgs, ","), "bad owner,delivery failed") {
		t.Fatalf("warn entries = %v", msgs)
	}

	newest := all.Entries[0].ID
	if p := get(fmt.Sprintf("?since=%d", newest)); len(p.Entries) != 0 {
		t.Fatalf("nothing new since %d: %+v", newest, p.Entries)
	}
	b.Log.Info("later", "n", 1)
	if p := get(fmt.Sprintf("?since=%d", newest)); len(p.Entries) != 1 || p.Entries[0].Msg != "later" {
		t.Fatalf("tail: %+v", p.Entries)
	}
	if p := get(fmt.Sprintf("?before=%d&limit=1", newest)); len(p.Entries) != 1 || p.Entries[0].ID != newest-1 {
		t.Fatalf("page back: %+v", p.Entries)
	}
	for _, q := range []string{"?level=loud", "?since=x", "?before=-1"} {
		if w := do(h, "GET", "/logs"+q, ownerHdr); w.Code != 400 {
			t.Fatalf("%s: %d", q, w.Code)
		}
	}

	b.Logs = nil
	if p := get(""); p.Entries == nil || len(p.Entries) != 0 {
		t.Fatalf("without a buffer: %+v", p)
	}
}
