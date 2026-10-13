package bot

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestLogBufferKeepsTheNewest(t *testing.T) {
	var out bytes.Buffer
	buf := NewLogBuffer(3)
	log := slog.New(buf.Wrap(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo})))
	log.Debug("not enabled")
	for i := range 5 {
		log.Info("step", "n", i)
	}
	log.Warn("careful", "count", 2)

	if got := strings.Count(out.String(), "msg=step"); got != 5 {
		t.Fatalf("the wrapped handler wrote %d step lines, want 5:\n%s", got, out.String())
	}
	if strings.Contains(out.String(), "not enabled") {
		t.Fatal("the wrapped handler's level must still apply")
	}
	all := buf.Entries(LogQuery{})
	if len(all) != 3 || all[0].Msg != "careful" || all[0].Level != "warn" || all[0].ID != 6 || all[2].Attrs["n"] != "3" {
		t.Fatalf("entries = %+v", all)
	}
	if got := buf.Entries(LogQuery{Level: slog.LevelWarn}); len(got) != 1 || got[0].Msg != "careful" {
		t.Fatalf("level filter: %+v", got)
	}
	if got := buf.Entries(LogQuery{Since: 5}); len(got) != 1 || got[0].ID != 6 {
		t.Fatalf("since: %+v", got)
	}
	if got := buf.Entries(LogQuery{Before: 6, Limit: 1}); len(got) != 1 || got[0].ID != 5 {
		t.Fatalf("before + limit: %+v", got)
	}
	if got := buf.Entries(LogQuery{Since: 6}); got == nil || len(got) != 0 {
		t.Fatalf("nothing new must be an empty list: %#v", got)
	}
}

func TestLogBufferDropsSecrets(t *testing.T) {
	var out bytes.Buffer
	buf := NewLogBuffer(10)
	hook := "https://hooks.example.com/abc?sig=s3cr3t"
	buf.Scrub(hook, "owner-secret-for-tests", "")
	log := slog.New(buf.Wrap(slog.NewJSONHandler(&out, nil)))

	log.With("component", "webhook", "webhook_url", hook).WithGroup("req").Info("posting",
		"Authorization", "Bearer wame_abc", "api_key", "k1", "password", "p", "status", 500,
		slog.Group("auth", "token", "wame_def", "name", "support-bot"))
	log.Warn("delivery failed", "err", errors.New("Post "+hook+": timeout"), "detail", "used owner-secret-for-tests")
	log.Info("big", "blob", strings.Repeat("€", maxLogValue))

	entries := buf.Entries(LogQuery{})
	kept := fmt.Sprint(entries)
	for _, secret := range []string{"s3cr3t", "hooks.example.com", "wame_abc", "wame_def", "k1", "owner-secret"} {
		if strings.Contains(kept, secret) {
			t.Errorf("%q reached the buffer: %s", secret, kept)
		}
	}
	a := entries[2].Attrs
	if a["component"] != "webhook" || a["req.status"] != "500" || a["req.auth.name"] != "support-bot" || a["req.Authorization"] != redacted || a["webhook_url"] != redacted {
		t.Fatalf("attrs = %v", a)
	}
	if !strings.Contains(entries[1].Attrs["err"], "Post "+redacted+": timeout") {
		t.Fatalf("err = %q", entries[1].Attrs["err"])
	}
	if blob := entries[0].Attrs["blob"]; len(blob) > maxLogValue+len("…") || !strings.HasSuffix(blob, "…") || !utf8.ValidString(blob) {
		t.Fatalf("long values must be clipped on a rune boundary: %d bytes", len(blob))
	}
	if !strings.Contains(out.String(), "s3cr3t") {
		t.Fatal("the wrapped handler must get each record as it was")
	}
}

func TestLogBufferShortSecretsAndPlainKeys(t *testing.T) {
	buf := NewLogBuffer(10)
	buf.Scrub("abc", "1234567", "s3cr3t-long")
	log := slog.New(buf.Wrap(slog.NewTextHandler(io.Discard, nil)))
	log.Warn("ignoring stored setting abc", "key", "queue_timezone", "detail", "1234567 s3cr3t-long",
		"api_key", "k1", "X-Gotify-Key", "k2", "private_key", "k3", "monkey", "fine")
	e := buf.Entries(LogQuery{})[0]
	if e.Msg != "ignoring stored setting abc" || e.Attrs["detail"] != "1234567 "+redacted {
		t.Fatalf("secrets under %d characters must be left alone: %+v", minScrubLen, e)
	}
	if e.Attrs["key"] != "queue_timezone" || e.Attrs["monkey"] != "fine" {
		t.Fatalf("a bare key attribute isn't a secret: %v", e.Attrs)
	}
	for _, k := range []string{"api_key", "X-Gotify-Key", "private_key"} {
		if e.Attrs[k] != redacted {
			t.Errorf("%s = %q", k, e.Attrs[k])
		}
	}
}

func TestLogBufferConcurrent(t *testing.T) {
	buf := NewLogBuffer(50)
	log := slog.New(buf.Wrap(slog.NewTextHandler(io.Discard, nil))).With("component", "test")
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 100 {
				log.Info("tick", "worker", i, "n", j)
				buf.Entries(LogQuery{Since: int64(j)})
			}
		})
	}
	wg.Wait()
	got := buf.Entries(LogQuery{})
	if len(got) != 50 || got[0].ID != 800 || got[49].ID != 751 || got[0].Attrs["component"] != "test" {
		t.Fatalf("got %d entries, newest %+v", len(got), got[0])
	}
}
