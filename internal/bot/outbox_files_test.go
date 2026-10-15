package bot

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"

	"github.com/williamtcastro/wame/internal/auth"
	"github.com/williamtcastro/wame/internal/store"
)

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func pdfBytes(n int) []byte {
	return []byte("%PDF-1.4\n% menu " + strings.Repeat("x", n) + "\n%%EOF\n")
}

func zipBytes(t *testing.T, entry string) []byte {
	t.Helper()
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	w, err := z.Create(entry)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("<Types/>"))
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeUploads swaps the WhatsApp upload for a counter.
func fakeUploads(b *Bot, err error) *atomic.Int64 {
	var n atomic.Int64
	b.uploadMedia = func(ctx context.Context, data []byte, kind whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
		n.Add(1)
		if err != nil {
			return whatsmeow.UploadResponse{}, err
		}
		return whatsmeow.UploadResponse{URL: "https://mmg.example/x", DirectPath: "/v/x", MediaKey: []byte{byte(n.Load())},
			FileEncSHA256: []byte{2}, FileSHA256: []byte{3}, FileLength: uint64(len(data))}, nil
	}
	return &n
}

// setOnline makes the outbox treat WhatsApp as reachable (or not) for this test.
func setOnline(t *testing.T, on bool) {
	old := clientOnline
	clientOnline = func(*Bot) bool { return on }
	t.Cleanup(func() { clientOnline = old })
}

func addFile(t *testing.T, b *Bot, actor, name string, data []byte) store.OutboxFile {
	t.Helper()
	f, _, err := b.AddQueueFile(actor, "", name, bytes.NewReader(data), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestQueueFileTypes(t *testing.T) {
	b := newTestBot(t)
	for _, c := range []struct {
		name     string
		data     []byte
		mime     string
		kind     string
		display  string
		wantType bool // 415
	}{
		{"../../x.png", pngBytes(t, 4, 4), "image/png", store.FileImage, "x.png", false},
		{`C:\Users\Maria\flyer`, pngBytes(t, 4, 5), "image/png", store.FileImage, "flyer.png", false},
		{"huge.png", pngBytes(t, 20000, 1), "image/png", store.FileDocument, "huge.png", false},
		{"menu\u202e.pdf", pdfBytes(1), "application/pdf", store.FileDocument, "menu.pdf", false},
		{"list.csv", []byte("to,text\n5511999999999,hi\n"), "text/csv", store.FileDocument, "list.csv", false},
		{"", []byte("plain notes\n"), "text/plain", store.FileDocument, "file.txt", false},
		{"report.docx", zipBytes(t, "[Content_Types].xml"), "application/vnd.openxmlformats-officedocument.wordprocessingml.document", store.FileDocument, "report.docx", false},
		{"photo.pdf", pngBytes(t, 3, 3), "", "", "", true},
		{"page.html", []byte("<!DOCTYPE html><html><script>alert(1)</script></html>"), "", "", "", true},
		{"page.txt", []byte("<html><body>hi</body></html>"), "", "", "", true},
		{"logo.txt", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), "", "", "", true},
		{"logo.svg", []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"/>`), "", "", "", true},
		{"run.exe", append([]byte("MZ\x90\x00\x03\x00\x00\x00"), make([]byte, 64)...), "", "", "", true},
		{"tool", append([]byte("\x7fELF\x02\x01\x01"), make([]byte, 64)...), "", "", "", true},
		{"archive.zip", zipBytes(t, "a.txt"), "", "", "", true},
		{"fake.docx", zipBytes(t, "a.txt"), "", "", "", true},
		{"broken.png", append(pngBytes(t, 2, 2)[:20], 0, 0, 0), "", "", "", true},
	} {
		f, _, err := b.AddQueueFile("promo-bot", "", c.name, bytes.NewReader(c.data), 64<<20)
		if _, isType := errors.AsType[FileTypeError](err); isType != c.wantType {
			t.Errorf("%q: err %v, want a type error %v", c.name, err, c.wantType)
			continue
		}
		if c.wantType {
			continue
		}
		if f.Mime != c.mime || f.Kind != c.kind || f.Name != c.display {
			t.Errorf("%q: %+v", c.name, f)
		}
		if !ValidQueueFileID(f.ID) || strings.Contains(f.ID, ".") {
			t.Errorf("disk name %q", f.ID)
		}
	}
	dir := b.queueDir()
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("outbox dir: %v %v", info.Mode(), err)
	}
	entries, _ := os.ReadDir(dir)
	files, _ := b.Store.ListOutboxFiles("")
	if len(entries) != len(files) || len(files) != 7 {
		t.Fatalf("refused uploads must leave nothing behind: %d on disk, %d rows", len(entries), len(files))
	}
	for _, e := range entries {
		info, _ := e.Info()
		if info.Mode().Perm() != 0o600 || !ValidQueueFileID(e.Name()) {
			t.Fatalf("%s: %v", e.Name(), info.Mode())
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(b.queueDir()), "..", "x.png")); err == nil {
		t.Fatal("a client name was used as a path")
	}
}

func TestQueueFileLimits(t *testing.T) {
	b := newTestBot(t)
	if _, _, err := b.AddQueueFile("promo-bot", "", "big.pdf", bytes.NewReader(pdfBytes(2000)), 1000); !errorsAs[FileTooLargeError](err) {
		t.Fatalf("over the size: %v", err)
	}
	if _, _, err := b.AddQueueFile("promo-bot", "", "empty.pdf", bytes.NewReader(nil), 1000); !isInputError(err) {
		t.Fatalf("empty: %v", err)
	}
	first := addFile(t, b, "promo-bot", "menu.pdf", pdfBytes(10))
	again, existed, err := b.AddQueueFile("promo-bot", "", "other-name.pdf", bytes.NewReader(pdfBytes(10)), 1000)
	if err != nil || !existed || again.ID != first.ID {
		t.Fatalf("same bytes again: %v %v %+v", err, existed, again)
	}
	if theirs := addFile(t, b, "maria-bot", "menu.pdf", pdfBytes(10)); theirs.ID == first.ID {
		t.Fatal("dedupe must be per actor")
	}

	old := maxQueueFiles
	maxQueueFiles = 2
	t.Cleanup(func() { maxQueueFiles = old })
	addFile(t, b, "promo-bot", "b.pdf", pdfBytes(11))
	_, _, err = b.AddQueueFile("promo-bot", "", "c.pdf", bytes.NewReader(pdfBytes(12)), 1000)
	if qe, ok := errors.AsType[FileQuotaError](err); !ok || qe.What != "files" || qe.Used != 2 {
		t.Fatalf("file count quota: %v", err)
	}
	maxQueueFiles = old

	if err := b.UpdateSettings("ops", map[string]*string{"queue_files_quota": new("100")}); err != nil {
		t.Fatal(err)
	}
	_, _, err = b.AddQueueFile("ana-bot", "", "d.pdf", bytes.NewReader(pdfBytes(13)), 1000)
	if qe, ok := errors.AsType[FileQuotaError](err); !ok || qe.What != "all files" {
		t.Fatalf("total quota: %v", err)
	}
	if s := b.QueueStats(); s.Files != 3 || s.FilesQuota != 100 || s.FileBytes == 0 {
		t.Fatalf("stats %+v", s)
	}
	entries, _ := os.ReadDir(b.queueDir())
	if len(entries) != 3 {
		t.Fatalf("%d files on disk", len(entries))
	}
}

func errorsAs[T error](err error) bool {
	_, ok := errors.AsType[T](err)
	return ok
}

func TestQueueFileSettings(t *testing.T) {
	b := newTestBot(t)
	if _, err := b.InitSettings(Settings{Rate: 20}); err != nil {
		t.Fatal(err)
	}
	s := b.currentSettings()
	if s.QueueMediaTTL != 24*time.Hour || s.QueueFilesQuota != 2<<30 {
		t.Fatalf("defaults %+v", s)
	}
	for _, bad := range []map[string]*string{{"queue_media_ttl": new("8d")}, {"queue_media_ttl": new("-1h")}, {"queue_files_quota": new("lots")}, {"queue_files_quota": new("-5MB")}} {
		if err := b.UpdateSettings("ops", bad); !isInputError(err) {
			t.Fatalf("%v accepted: %v", bad, err)
		}
	}
	if err := b.UpdateSettings("ops", map[string]*string{"queue_media_ttl": new("off"), "queue_files_quota": new("500MB")}); err != nil {
		t.Fatal(err)
	}
	view, _ := b.SettingsView()
	got := map[string]SettingView{}
	for _, v := range view {
		got[v.Key] = v
	}
	if got["queue_media_ttl"].Value != "off" || got["queue_media_ttl"].Default != "24h0m0s" || got["queue_files_quota"].Value != "500MB" || got["queue_files_quota"].Default != "2GB" {
		t.Fatalf("%+v %+v", got["queue_media_ttl"], got["queue_files_quota"])
	}
	if err := b.UpdateSettings("ops", map[string]*string{"queue_media_ttl": nil}); err != nil || b.currentSettings().QueueMediaTTL != 24*time.Hour {
		t.Fatalf("reset: %v", err)
	}
}

func TestEnqueueWithFiles(t *testing.T) {
	b := newTestBot(t)
	f := addFile(t, b, "promo-bot", "menu.pdf", pdfBytes(10))
	theirs := addFile(t, b, "maria-bot", "other.pdf", pdfBytes(20))
	reqs := []QueueRequest{{To: "5511999999999", File: f.ID}, {To: "5511988887777", Text: "Hi John, the menu", File: f.ID}}
	items, err := b.Enqueue("promo-bot", "", "menu", "k1", reqs)
	if err != nil || items[0].FileID != f.ID || items[0].FileKind != store.FileDocument || items[0].FileSize != f.Size || items[0].Text != "" {
		t.Fatalf("%v %+v", err, items)
	}
	if again, err := b.QueueMessages("promo-bot", "", "menu", "k1", reqs); err != nil || again.Replayed != 2 {
		t.Fatalf("replay: %v %+v", err, again)
	}
	other := addFile(t, b, "promo-bot", "b.pdf", pdfBytes(11))
	if _, err := b.Enqueue("promo-bot", "", "menu", "k1", []QueueRequest{{To: "5511999999999", File: other.ID}, reqs[1]}); !errors.Is(err, ErrKeyReused) {
		t.Fatalf("same key, another file: %v", err)
	}
	for _, id := range []string{theirs.ID, "f_" + strings.Repeat("0", 32)} {
		if _, err := b.Enqueue("promo-bot", "", "", "", []QueueRequest{{To: "5511999999999", File: id}}); !errors.Is(err, store.ErrNotFound) || isInputError(err) {
			t.Fatalf("file %s: %v", id, err)
		}
	}
	if _, err := b.Enqueue("promo-bot", "", "", "", []QueueRequest{{To: "5511999999999", File: "../../etc/passwd"}}); !isInputError(err) {
		t.Fatalf("bad id: %v", err)
	}
	if err := b.DeleteQueueFile("promo-bot", f.ID, false); !errorsAs[FileInUseError](err) {
		t.Fatalf("delete while queued: %v", err)
	}
	if err := b.DeleteQueueFile("maria-bot", f.ID, false); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("someone else's: %v", err)
	}
	if _, err := b.CancelQueued("promo-bot", 0, store.OutboxFilter{Batch: "menu"}); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteQueueFile("ops", f.ID, true); err != nil {
		t.Fatalf("admin delete: %v", err)
	}
	if _, err := os.Stat(b.queueFilePath(f.ID)); !os.IsNotExist(err) {
		t.Fatal("deleted file still on disk")
	}
	if _, err := b.Enqueue("promo-bot", "", "", "", []QueueRequest{{To: "5511999999999", File: f.ID}}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted file: %v", err)
	}
	log, _ := b.Store.AuditLog(50, 0)
	for _, e := range log {
		if strings.Contains(e.Detail, "menu") || strings.Contains(e.Detail, ".pdf") {
			t.Fatalf("a file name reached the audit log: %+v", e)
		}
	}
}

func queueOne(t *testing.T, b *Bot, f store.OutboxFile, n int) []store.OutboxItem {
	t.Helper()
	reqs := make([]QueueRequest, n)
	for i := range reqs {
		reqs[i] = QueueRequest{To: []string{"5511999999999", "5511988887777", "552187654321"}[i%3], File: f.ID, Text: "the menu"}
	}
	items, err := b.Enqueue(f.Actor, f.TokenID, "", "", reqs)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func TestQueueUploadReuse(t *testing.T) {
	b := newTestBot(t)
	if _, err := b.InitSettings(Settings{}); err != nil {
		t.Fatal(err)
	}
	uploads := fakeUploads(b, nil)
	f := addFile(t, b, "promo-bot", "flyer.png", pngBytes(t, 8, 8))
	items := queueOne(t, b, f, 3)
	ctx := t.Context()
	for _, it := range items {
		m, err := b.queueMediaFor(ctx, it)
		if err != nil || m.up.DirectPath != "/v/x" || m.file.ID != f.ID {
			t.Fatalf("%v %+v", err, m)
		}
		b.clearReady()
	}
	if uploads.Load() != 1 {
		t.Fatalf("%d uploads for three items within the TTL", uploads.Load())
	}

	if _, err := b.db.Exec(`UPDATE wame_outbox_files SET uploaded_at = ?`, time.Now().Add(-25*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if m, _ := b.queueMediaFor(ctx, items[0]); m.reused || uploads.Load() != 2 {
		t.Fatalf("an upload older than the TTL must be redone: %d", uploads.Load())
	}
	b.clearReady()
	if m, _ := b.queueMediaFor(ctx, items[1]); !m.reused || uploads.Load() != 2 {
		t.Fatal("the fresh upload is reused")
	}

	// A send error after a reused upload drops the cache, so the retry uploads again.
	b.clearReady()
	m, _ := b.queueMediaFor(ctx, items[2])
	b.dropReusedUpload(m, errors.New("boom"))
	b.clearReady()
	if m, _ := b.queueMediaFor(ctx, items[2]); m.reused || uploads.Load() != 3 {
		t.Fatalf("after a send error the next attempt must upload: %d", uploads.Load())
	}

	// The upload made for an item before its slot is kept for its send.
	if m, _ := b.queueMediaFor(ctx, items[2]); uploads.Load() != 3 || m == nil {
		t.Fatal("ready upload not kept")
	}

	if err := b.UpdateSettings("ops", map[string]*string{"queue_media_ttl": new("off")}); err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		b.clearReady()
		if m, _ := b.queueMediaFor(ctx, it); m.reused {
			t.Fatal("ttl off reuses nothing")
		}
	}
	if uploads.Load() != 6 {
		t.Fatalf("ttl off: %d uploads", uploads.Load())
	}
}

func TestQueueUploadErrorsAndGoneFiles(t *testing.T) {
	b := newTestBot(t)
	uploads := fakeUploads(b, errors.New(`Post "https://mmg.whatsapp.net/mms/image/abc?auth=SECRETAUTH&token=T0K": dial tcp: timeout`))
	f := addFile(t, b, "promo-bot", "flyer.png", pngBytes(t, 8, 8))
	items := queueOne(t, b, f, 2)
	ctx := t.Context()

	m, wait, err := b.queuedFile(ctx, items[0], nil, maria)
	if err == nil || m != nil || wait != 0 || uploads.Load() != 1 {
		t.Fatalf("%v %v %v", m, wait, err)
	}
	// Offline (the connection dropped during the upload): the attempt is given back, no cooldown.
	b.queuedFileFailed(ctx, items[0], err)
	if it, _ := b.Store.OutboxItem(items[0].ID); it.Status != store.OutboxQueued || it.Attempts != 0 || !b.QueueStats().CooldownUntil.IsZero() {
		t.Fatalf("an upload error while offline must be refunded: %+v", it)
	}
	setOnline(t, true)
	b.queuedFileFailed(ctx, items[0], err)
	it, _ := b.Store.OutboxItem(items[0].ID)
	if it.Status != store.OutboxQueued || it.Attempts != 1 || time.Until(it.Due) < 20*time.Second || !strings.Contains(it.Error, "upload") {
		t.Fatalf("an upload error is an attempt with backoff: %+v", it)
	}
	if strings.Contains(it.Error, "SECRETAUTH") || strings.Contains(it.Error, "T0K") || !strings.Contains(it.Error, "mmg.whatsapp.net") {
		t.Fatalf("the upload URL's query must not reach the item: %q", it.Error)
	}
	if s := b.QueueStats(); strings.Contains(s.LastError, "SECRETAUTH") || s.LastError == "" {
		t.Fatalf("last error %q", s.LastError)
	}

	// Canceled while the upload ran: the late failure doesn't touch it.
	b.uploadMedia = func(ctx context.Context, data []byte, kind whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
		if _, err := b.CancelQueued("promo-bot", items[1].ID, store.OutboxFilter{}); err != nil {
			t.Error(err)
		}
		return whatsmeow.UploadResponse{}, errors.New("late")
	}
	_, _, err = b.queuedFile(ctx, items[1], nil, john)
	b.queuedFileFailed(ctx, items[1], err)
	if it, _ := b.Store.OutboxItem(items[1].ID); it.Status != store.OutboxCanceled || it.Attempts != 0 {
		t.Fatalf("a late upload result must not overwrite a cancel: %+v", it)
	}
	// And a late success doesn't send it either.
	b.sendQueuedWith(ctx, items[1], &queueMedia{file: f})
	if it, _ := b.Store.OutboxItem(items[1].ID); it.Status != store.OutboxCanceled {
		t.Fatalf("%+v", it)
	}

	// The file removed from disk: the item fails at once, no retry.
	fakeUploads(b, nil)
	more := queueOne(t, b, f, 1)
	if err := os.Remove(b.queueFilePath(f.ID)); err != nil {
		t.Fatal(err)
	}
	b.clearReady()
	_, _, err = b.queuedFile(ctx, more[0], nil, maria)
	b.queuedFileFailed(ctx, more[0], err)
	if it, _ := b.Store.OutboxItem(more[0].ID); it.Status != store.OutboxFailed || it.Error != "file is gone" {
		t.Fatalf("%+v", it)
	}

	// Changed on disk: same.
	g := addFile(t, b, "promo-bot", "menu.pdf", pdfBytes(5))
	last := queueOne(t, b, g, 1)
	if err := os.WriteFile(b.queueFilePath(g.ID), pdfBytes(6), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = b.queuedFile(ctx, last[0], nil, maria)
	if !errors.Is(err, errFileGone) {
		t.Fatalf("changed file: %v", err)
	}
}

func TestUploadErrorsHideTheAuthToken(t *testing.T) {
	b := newTestBot(t)
	b.uploadMedia = func(ctx context.Context, data []byte, kind whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
		return whatsmeow.UploadResponse{}, fmt.Errorf("failed to execute request: %w",
			&url.Error{Op: "Post", URL: "https://mmg.whatsapp.net/mms/image/T0K?auth=SECRETAUTH&token=T0K", Err: context.DeadlineExceeded})
	}
	_, err := b.sendFile(t.Context(), types.NewJID("5511999999999", types.DefaultUserServer), nil, pngBytes(t, 2, 2), "x.png", "", SendOptions{})
	if err == nil || strings.Contains(err.Error(), "SECRETAUTH") || strings.Contains(err.Error(), "T0K") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("POST /send/file upload error: %v", err)
	}

	f := addFile(t, b, "promo-bot", "flyer.png", pngBytes(t, 8, 8))
	items := queueOne(t, b, f, 1)
	if err := os.Remove(b.queueFilePath(f.ID)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(b.queueFilePath(f.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = b.queueMediaFor(t.Context(), items[0])
	if err == nil || strings.Contains(err.Error(), b.dataDir) || strings.Contains(err.Error(), f.ID) {
		t.Fatalf("a read error must not show the server's paths: %v", err)
	}
}

func TestQueueUploadWaitsForTheSlot(t *testing.T) {
	b := newTestBot(t)
	uploads := fakeUploads(b, nil)
	b.Limiter = NewLimiter(1, 0, 0)
	f := addFile(t, b, "promo-bot", "flyer.png", pngBytes(t, 8, 8))
	items := queueOne(t, b, f, 1)
	if _, ok := b.Limiter.TryReserve(maria, time.Second); !ok {
		t.Fatal("first slot")
	}
	m, wait, err := b.queuedFile(t.Context(), items[0], nil, maria)
	if err != nil || m != nil || wait < time.Second || uploads.Load() != 0 {
		t.Fatalf("no upload while the slot is far: %v %v %v %d", m, wait, err, uploads.Load())
	}
}

func TestPruneQueueFiles(t *testing.T) {
	b := newTestBot(t)
	_, tok, err := auth.Issue(b.Store, auth.Request{Name: "promo-bot", Scopes: []auth.Scope{auth.Send}})
	if err != nil {
		t.Fatal(err)
	}
	unused := addFile(t, b, "ops", "a.pdf", pdfBytes(1))
	done := addFile(t, b, "ops", "b.pdf", pdfBytes(2))
	open := addFile(t, b, "ops", "c.pdf", pdfBytes(3))
	fresh := addFile(t, b, "ops", "d.pdf", pdfBytes(4))
	revoked, _, err := b.AddQueueFile("promo-bot", tok.ID, "e.pdf", bytes.NewReader(pdfBytes(5)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	doneItems := queueOne(t, b, done, 1)
	queueOne(t, b, open, 1)
	revokedItems := queueOne(t, b, revoked, 1)
	if _, err := b.CancelQueued("ops", doneItems[0].ID, store.OutboxFilter{}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour).UnixMilli()
	if _, err := b.db.Exec(`UPDATE wame_outbox_files SET created_at = ? WHERE id != ?`, old, fresh.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.Exec(`UPDATE wame_outbox SET updated_at = ?`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Store.RevokeToken("promo-bot"); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(b.queueDir(), "f_"+strings.Repeat("a", 32))
	newStray := filepath.Join(b.queueDir(), "f_"+strings.Repeat("b", 32))
	other := filepath.Join(b.queueDir(), "notes.txt")
	for _, p := range []string{stray, newStray, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(stray, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	b.pruneOutbox()

	onDisk := func(id string) bool { _, err := os.Stat(b.queueFilePath(id)); return err == nil }
	for _, c := range []struct {
		f    store.OutboxFile
		keep bool
	}{{unused, false}, {done, false}, {open, true}, {fresh, true}, {revoked, false}} {
		if onDisk(c.f.ID) != c.keep {
			t.Errorf("%s on disk = %v, want %v", c.f.Name, !c.keep, c.keep)
		}
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Error("an old file without a row must be swept")
	}
	for _, p := range []string{newStray, other} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: a fresh upload in progress or a foreign file must stay", filepath.Base(p))
		}
	}
	if it, err := b.queueFile(revokedItems[0]); !errors.Is(err, errFileGone) {
		t.Fatalf("revoked token's file: %+v %v", it, err)
	}
}

func TestLogoutRemovesQueueFiles(t *testing.T) {
	b := newTestBot(t)
	f := addFile(t, b, "ops", "a.pdf", pdfBytes(1))
	if _, err := b.db.Exec(`UPDATE wame_outbox_files SET upload = x'01', uploaded_at = 1`); err != nil {
		t.Fatal(err)
	}
	b.logout = func(context.Context) error { return nil }
	if err := b.Logout(t.Context(), "ops"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(b.queueDir()); !os.IsNotExist(err) {
		t.Fatal("outbox/ must be gone after logout")
	}
	if got, _ := b.Store.OutboxFile(f.ID); got.Deleted.IsZero() || got.Name != "" || got.Upload != nil {
		t.Fatalf("%+v", got)
	}
}

func TestCleanFileName(t *testing.T) {
	for in, want := range map[string]string{
		"../../etc/passwd":                "passwd",
		`..\..\menu.pdf`:                  "menu.pdf",
		"me\x00nu\u200b.pdf":              "menu.pdf",
		"..":                              "file",
		".pdf":                            "file.pdf",
		strings.Repeat("é", 200) + ".pdf": strings.Repeat("é", 125) + ".pdf",
	} {
		if got := CleanFileName(in); got != want {
			t.Errorf("CleanFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFreshUploadEndsTheStep(t *testing.T) {
	b := newTestBot(t)
	uploads := fakeUploads(b, nil)
	f := addFile(t, b, "promo-bot", "flyer.png", pngBytes(t, 8, 8))
	items := queueOne(t, b, f, 1)
	// A long upload: pause, quiet hours or a revoked token must be seen before the send.
	m, d, err := b.queuedFile(t.Context(), items[0], nil, maria)
	if err != nil || m != nil || d != redoStep || uploads.Load() != 1 {
		t.Fatalf("a fresh upload must end the step: %v %v %v", m, d, err)
	}
	m, d, err = b.queuedFile(t.Context(), items[0], nil, maria)
	if err != nil || m == nil || d != 0 || uploads.Load() != 1 {
		t.Fatalf("the next step must use the kept upload: %v %v %v %d", m, d, err, uploads.Load())
	}
}

func TestUploadLimitsBeforeDisk(t *testing.T) {
	b := newTestBot(t)
	old := maxQueueFiles
	maxQueueFiles = 1
	t.Cleanup(func() { maxQueueFiles = old })
	addFile(t, b, "promo-bot", "a.pdf", pdfBytes(1))
	seen := false
	r := readerFunc(func(p []byte) (int, error) {
		entries, _ := os.ReadDir(b.queueDir())
		seen = len(entries) > 1
		return copy(p, pdfBytes(2)), io.EOF
	})
	if _, _, err := b.AddQueueFile("promo-bot", "", "b.pdf", r, 1<<20); !errorsAs[FileQuotaError](err) || seen {
		t.Fatalf("a caller over its quota must not write to disk: %v, wrote %v", err, seen)
	}

	var releases []func()
	for range maxUploadsPerActor {
		release, err := b.startUpload("maria-bot")
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	if _, err := b.startUpload("maria-bot"); !errorsAs[RateLimitError](err) {
		t.Fatalf("a third parallel upload: %v", err)
	}
	for _, r := range releases {
		r()
	}
	if _, err := b.startUpload("maria-bot"); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func TestSniffingHoles(t *testing.T) {
	b := newTestBot(t)
	var many bytes.Buffer
	z := zip.NewWriter(&many)
	for i := range maxZipEntries + 5 {
		if _, err := z.CreateHeader(&zip.FileHeader{Name: fmt.Sprint(i), Method: zip.Store}); err != nil {
			t.Fatal(err)
		}
	}
	w, _ := z.Create("[Content_Types].xml")
	_, _ = w.Write([]byte("<Types/>"))
	_ = z.Close()
	for name, data := range map[string][]byte{
		"many.docx":          many.Bytes(),
		"a.txt":              []byte("\f<svg xmlns=\"http://www.w3.org/2000/svg\"><script>alert(1)</script></svg>"),
		"b.txt":              []byte("\xef\xbb\xbf\f<html><script>alert(1)</script></html>"),
		"c.txt":              []byte("\xff\xfe<\x00h\x00t\x00m\x00l\x00>\x00"),
		"photo.Maria Secret": pngBytes(t, 2, 2),
	} {
		_, _, err := b.AddQueueFile("promo-bot", "", name, bytes.NewReader(data), 64<<20)
		if !errorsAs[FileTypeError](err) || strings.Contains(err.Error(), "Secret") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, _, _, err := sniffQueueFile(pngBytes(t, 2, 2), filepath.Join(b.dataDir, "gone"), 10, "x.png"); !errors.Is(err, ErrFileStorage) {
		t.Fatalf("a vanished file must not show its path: %v", err)
	}
	log, _ := b.Store.AuditLog(50, 0)
	for _, e := range log {
		if strings.Contains(e.Error, "Secret") {
			t.Fatalf("a piece of the name reached the audit log: %+v", e)
		}
	}
}

func TestDedupeRestoresAMissingCopy(t *testing.T) {
	b := newTestBot(t)
	f := addFile(t, b, "promo-bot", "menu.pdf", pdfBytes(3))
	if err := os.Remove(b.queueFilePath(f.ID)); err != nil {
		t.Fatal(err)
	}
	again, existed, err := b.AddQueueFile("promo-bot", "", "menu.pdf", bytes.NewReader(pdfBytes(3)), 1<<20)
	if err != nil || !existed || again.ID != f.ID {
		t.Fatalf("%v %v %+v", err, existed, again)
	}
	if _, err := os.Stat(b.queueFilePath(f.ID)); err != nil {
		t.Fatal("the same bytes uploaded again must take the missing copy's place")
	}
	if FormatBytes(1<<20+1) != "1048577B" || FormatBytes(2<<30) != "2GB" {
		t.Fatal("FormatBytes must read back with ParseBytes")
	}
}

func TestWebPSize(t *testing.T) {
	head := func(chunk string, data []byte) []byte {
		b := append([]byte("RIFF\x00\x00\x00\x00WEBP"+chunk+"\x00\x00\x00\x00"), data...)
		return append(b, make([]byte, 40)...)
	}
	vp8x := head("VP8X", []byte{0, 0, 0, 0, 99, 0, 0, 49, 0, 0})
	if w, h, err := webpSize(vp8x); err != nil || w != 100 || h != 50 {
		t.Fatalf("VP8X %d %d %v", w, h, err)
	}
	vp8 := head("VP8 ", []byte{0, 0, 0, 0x9d, 0x01, 0x2a, 64, 0, 32, 0})
	if w, h, err := webpSize(vp8); err != nil || w != 64 || h != 32 {
		t.Fatalf("VP8 %d %d %v", w, h, err)
	}
	if _, _, err := webpSize([]byte("RIFF")); err == nil {
		t.Fatal("short header accepted")
	}
}
