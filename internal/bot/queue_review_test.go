package bot

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/williamtcastro/wame/internal/store"
)

func TestWipeKeepsFilesWameDidntWrite(t *testing.T) {
	b := newTestBot(t)
	dir := b.queueDir()
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(keep, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	addFile(t, b, "ops", "a.pdf", pdfBytes(1))
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o755 {
		t.Fatalf("mode of a directory wame didn't create changed to %v", info.Mode())
	}
	b.wipeQueueFiles("ops")
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("wipe removed a file wame didn't write: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("left %d entries", len(entries))
	}
}

func TestUploadRacingWipeOrSweep(t *testing.T) {
	for _, c := range []struct {
		name  string
		clear func(b *Bot)
	}{
		{"wipe", func(b *Bot) { b.wipeQueueFiles("ops") }},
		{"sweep", func(b *Bot) { b.sweepQueueFiles(time.Now().Add(time.Hour)) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newTestBot(t)
			pr, pw := io.Pipe()
			done := make(chan error, 1)
			go func() {
				_, _, err := b.AddQueueFile("ops", "", "menu.pdf", pr, 64<<20)
				done <- err
			}()
			data := pdfBytes(4096)
			if _, err := pw.Write(data[:100]); err != nil { // returns once the file is open and being written
				t.Fatal(err)
			}
			c.clear(b)
			_, _ = pw.Write(data[100:])
			_ = pw.Close()
			if err := <-done; !errors.Is(err, ErrFileStorage) {
				t.Fatalf("upload whose file was removed: %v", err)
			}
			if live, _ := b.Store.LiveOutboxFiles(); len(live) != 0 {
				t.Fatalf("live rows without a file: %v", live)
			}
		})
	}
}

func TestFailAttemptNeverUploads(t *testing.T) {
	b := newTestBot(t)
	uploads := fakeUploads(b, nil)
	f := addFile(t, b, "ops", "menu.pdf", pdfBytes(10))
	it := queueOne(t, b, f, 1)[0]
	b.failAttempt(t.Context(), it, errors.New("lookup failed"))
	if uploads.Load() != 0 {
		t.Fatal("recording a failed lookup uploaded the file")
	}
	got, err := b.Store.OutboxItem(it.ID)
	if err != nil || got.Status != store.OutboxQueued || !strings.Contains(got.Error, "lookup failed") {
		t.Fatalf("%v %+v", err, got)
	}
}

func TestDeleteQueueFileJunkIDNotAudited(t *testing.T) {
	b := newTestBot(t)
	addFile(t, b, "ops", "a.pdf", pdfBytes(1)) // the last audit entry before the delete
	if err := b.DeleteQueueFile("ops", strings.Repeat("x", 10<<10), false); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	if e := lastAudit(t, b); e.Action != "queue_file" {
		t.Fatalf("junk id audited: %s %.40q", e.Action, e.Target)
	}
}

func TestTokenLookupErrorIsNotAGoneToken(t *testing.T) {
	b := newTestBot(t)
	f, _, err := b.AddQueueFile("ops", "tok12345", "a.pdf", strings.NewReader(string(pdfBytes(1))), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.Exec(`DROP TABLE wame_tokens`); err != nil { // every lookup now fails, not "not found"
		t.Fatal(err)
	}
	if _, err := b.queueToken("tok12345", map[string]*store.Token{}); !errors.Is(err, errTokenLookup) {
		t.Fatalf("%v", err)
	}
	b.pruneQueueFiles()
	if got, err := b.Store.OutboxFile(f.ID); err != nil || !got.Deleted.IsZero() {
		t.Fatal("a database error deleted a token's files")
	}
}
