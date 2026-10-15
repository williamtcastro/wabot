package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/williamtcastro/wame/internal/store"
)

func menuFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "menu.pdf")
	writeFile(t, path, "%PDF-1.4\n% "+content+"\n%%EOF\n")
	return path
}

func TestQueueImportFile(t *testing.T) {
	b, _ := queueServer(t)
	smallChunks(t)
	csv := csvFile(t, 23)
	menu := menuFile(t, "menu")

	out, err := runQueue(t, "import", "-batch", "promo", "-file", menu, csv)
	if err != nil || !strings.Contains(out, "uploaded menu.pdf (document, ") || !strings.Contains(out, "queued 23 messages") {
		t.Fatalf("%v\n%s", err, out)
	}
	files, _ := b.QueueFiles("owner", true)
	if len(files) != 1 {
		t.Fatalf("%d files uploaded, want 1 for 3 chunks", len(files))
	}
	items, _ := b.Store.ListOutbox(store.OutboxFilter{Batch: "promo"}, 100)
	if len(items) != 23 {
		t.Fatalf("%d items", len(items))
	}
	for _, it := range items {
		if it.FileID != files[0].ID {
			t.Fatalf("item %d has file %q, want %q", it.ID, it.FileID, files[0].ID)
		}
	}

	// A rerun uploads the same bytes again (200, same id) and queues nothing twice.
	out, err = runQueue(t, "import", "-batch", "promo", "-file", menu, csv)
	if err != nil || !strings.Contains(out, "menu.pdf: already uploaded ("+files[0].ID+")") || !strings.Contains(out, "queued 0 messages (already queued 23)") {
		t.Fatalf("rerun: %v\n%s", err, out)
	}

	// An existing id skips the upload and gives the same key as the path did.
	if out, err = runQueue(t, "import", "-dry-run", "-file", files[0].ID, csv); err != nil || !strings.Contains(out, "an existing upload") || strings.Contains(out, "would be uploaded") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestQueueImportFileDryRunAndChecks(t *testing.T) {
	b, _ := queueServer(t)
	menu := menuFile(t, "menu")
	csv := csvFile(t, 3)

	out, err := runQueue(t, "import", "-dry-run", "-file", menu, csv)
	if err != nil || !strings.Contains(out, "file menu.pdf (") || !strings.Contains(out, "would be uploaded once") || !strings.Contains(out, "dry run: nothing sent") {
		t.Fatalf("%v\n%s", err, out)
	}
	if files, _ := b.QueueFiles("owner", true); len(files) != 0 {
		t.Fatalf("a dry run uploaded %d files", len(files))
	}

	huge := filepath.Join(t.TempDir(), "huge.pdf")
	writeFile(t, huge, "x")
	if err := os.Truncate(huge, maxImportFile+1); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ path, want string }{
		{filepath.Join(t.TempDir(), "missing.pdf"), "-file:"},
		{t.TempDir(), "not a regular file"},
		{huge, "is larger than 64.0 MiB"},
	} {
		for _, dry := range []bool{true, false} {
			args := []string{"import", "-file", c.path, csv}
			if dry {
				args = append([]string{"import", "-dry-run"}, args[1:]...)
			}
			if _, err := runQueue(t, args...); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s dry=%v: %v, want %q", c.path, dry, err, c.want)
			}
		}
	}
	if n := counts(t, b, ""); n.Queued != 0 {
		t.Fatalf("a failed check queued %+v", n)
	}
}

func TestQueueImportFileAllowsEmptyText(t *testing.T) {
	b, _ := queueServer(t)
	menu := menuFile(t, "menu")
	csv := csvFileTo(t, "to,text\n5511999999999,\n5511988887777,\n")

	if _, err := runQueue(t, "import", "-dry-run", csv); err == nil || !strings.Contains(err.Error(), "text is empty") {
		t.Fatalf("without -file: %v", err)
	}
	if _, err := runQueue(t, "import", "-dry-run", "-file", menu, csv); err != nil {
		t.Fatalf("dry run with -file: %v", err)
	}
	if out, err := runQueue(t, "import", "-batch", "menus", "-dedupe", "-file", menu, csv); err != nil || !strings.Contains(out, "queued 2 messages") {
		t.Fatalf("%v\n%s", err, out)
	}
	items, _ := b.Store.ListOutbox(store.OutboxFilter{Batch: "menus"}, 10)
	if len(items) != 2 || items[0].Text != "" || items[0].FileID == "" {
		t.Fatalf("%+v", items)
	}
}

func TestQueueImportFileChangesKey(t *testing.T) {
	b, _ := queueServer(t)
	csv := csvFileTo(t, "to,text\n5511999999999,Hi Maria\n")
	first, second := menuFile(t, "one"), menuFile(t, "two")

	if out, err := runQueue(t, "import", "-batch", "a", "-file", first, csv); err != nil || !strings.Contains(out, "queued 1 messages") {
		t.Fatalf("%v\n%s", err, out)
	}
	// Same CSV, another file, no -key: a new import, not a 409 against the first one.
	if out, err := runQueue(t, "import", "-batch", "b", "-file", second, csv); err != nil || !strings.Contains(out, "queued 1 messages") {
		t.Fatalf("%v\n%s", err, out)
	}
	if n := counts(t, b, ""); n.Queued != 2 {
		t.Fatalf("%+v", n)
	}
	a, _ := runQueue(t, "import", "-dry-run", "-file", first, csv)
	c, _ := runQueue(t, "import", "-dry-run", csv)
	if keyOf(a) == keyOf(c) || keyOf(a) == "" {
		t.Fatalf("keys: %q vs %q", keyOf(a), keyOf(c))
	}
	// An explicit -key with another file is the user's call: the server answers 409.
	if _, err := runQueue(t, "import", "-batch", "a", "-key", "k1", "-file", first, csv); err != nil {
		t.Fatal(err)
	}
	if _, err := runQueue(t, "import", "-batch", "a", "-key", "k1", "-file", second, csv); err == nil || !strings.Contains(err.Error(), "differ from what was queued") {
		t.Fatalf("%v", err)
	}
}

func keyOf(out string) string {
	_, after, _ := strings.Cut(out, "key ")
	return strings.TrimSpace(after)
}
