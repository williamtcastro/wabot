package admin

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/williamtcastro/wame/internal/api"
	"github.com/williamtcastro/wame/internal/auth"
	"github.com/williamtcastro/wame/internal/bot"
)

func TestQueueTabShowsTypeNotName(t *testing.T) {
	b, c := newServer(t)
	pdf := "%PDF-1.4\n% " + strings.Repeat("x", 3000) + "\n%%EOF\n"
	f, _, err := b.AddQueueFile("api", "", "Cardápio da Maria.pdf", strings.NewReader(pdf), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Enqueue("api", "", "menu", "", []bot.QueueRequest{{To: "5511999999999", File: f.ID, Text: "Our new menu"}, {To: "5511988887777", Text: "Hi John"}}); err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t, Options{Client: c})
	press(m, "7")
	view := ansi.Strip(m.View())
	for _, want := range []string{"TYPE", "document 2 KB", "text"} {
		if !strings.Contains(view, want) {
			t.Errorf("queue view lacks %q:\n%s", want, view)
		}
	}
	for _, never := range []string{"Cardápio", "Our new menu", "Hi John"} {
		if strings.Contains(view, never) {
			t.Fatalf("queue view shows %q:\n%s", never, view)
		}
	}
	press(m, "5")
	if view := ansi.Strip(m.View()); !strings.Contains(view, "Queue files") || !strings.Contains(view, "2 KB") || strings.Contains(view, "Cardápio") {
		t.Fatalf("health:\n%s", view)
	}
}

func addQueueFile(t *testing.T, b *bot.Bot, actor, name string) string {
	t.Helper()
	body := "%PDF-1.4\n% " + actor + strings.Repeat("x", 3000) + "\n%%EOF\n"
	f, _, err := b.AddQueueFile(actor, "", name, strings.NewReader(body), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return f.ID
}

func TestQueueFilesViewNeverShowsNames(t *testing.T) {
	b, c := newServer(t)
	mine := addQueueFile(t, b, "owner", "maria-secret-plan.pdf")
	theirs := addQueueFile(t, b, "ops", "john-private-list.pdf")
	m := newTestModel(t, Options{Client: c})
	press(m, "7", "F")
	raw := m.View()
	view := ansi.Strip(raw)
	for _, want := range []string{"files · 2 of 2 · 2 files, 5 KB in use", "ID", "KIND", "SIZE", "UPLOADED", "EXPIRES", "USED", "BY",
		shortFileID(mine), shortFileID(theirs), "document", "2 KB", "unused", "owner", "ops"} {
		if !strings.Contains(view, want) {
			t.Errorf("files view lacks %q:\n%s", want, view)
		}
	}
	for _, never := range []string{"maria-secret-plan", "john-private-list", "secret", "NAME"} {
		if strings.Contains(view, never) {
			t.Fatalf("files view shows %q:\n%s", never, view)
		}
	}
	for _, f := range m.qfiles.Files {
		if f.Name != "" {
			t.Fatalf("the panel kept a file name: %+v", f)
		}
	}
	for _, line := range strings.Split(view, "\n") {
		if w := ansi.StringWidth(line); w > 120 {
			t.Fatalf("line of %d columns overflows 120: %q", w, line)
		}
	}
	if keys := flat(m.viewKeys()); !strings.Contains(keys, "/ filter") || !strings.Contains(keys, "d delete") || !strings.Contains(keys, "F/esc queue") {
		t.Fatalf("footer: %s", keys)
	}
	press(m, "esc")
	if m.queueFiles || !strings.Contains(flat(m.View()), "F files") {
		t.Fatal("esc must go back to the queue items")
	}
}

func TestQueueFilesFilterAndDelete(t *testing.T) {
	b, c := newServer(t)
	first := addQueueFile(t, b, "owner", "maria-secret-plan.pdf")
	second := addQueueFile(t, b, "ops", "ana.pdf")
	m := newTestModel(t, Options{Client: c})
	press(m, "7", "F", "/", "o", "p", "s", "enter")
	if vis := m.visibleQueueFiles(); len(vis) != 1 || vis[0].ID != second || !strings.Contains(ansi.Strip(m.View()), "filtered by") {
		t.Fatalf("filter by actor: %+v", vis)
	}
	if keys := flat(m.viewKeys()); !strings.Contains(keys, "esc clear") {
		t.Fatalf("footer with a filter: %s", keys)
	}
	press(m, "esc")
	if m.qfileQuery != "" || !m.queueFiles || len(m.visibleQueueFiles()) != 2 {
		t.Fatalf("esc must clear the filter first: %q", m.qfileQuery)
	}
	press(m, "/", "f", "_", "enter")
	if len(m.visibleQueueFiles()) != 2 {
		t.Fatal("filter by id prefix")
	}
	press(m, "esc", "/", "z", "z", "enter")
	if !strings.Contains(ansi.Strip(m.View()), "nothing matches") {
		t.Fatal("no match message")
	}
	press(m, "esc", "d")
	if m.mode != modeConfirmDeleteQueueFile || m.confirmQFile != m.qfiles.Files[0].ID {
		t.Fatalf("mode %v, confirm %q", m.mode, m.confirmQFile)
	}
	captured := m.confirmQFile
	if view := ansi.Strip(m.View()); !strings.Contains(view, "Delete file "+shortFileID(captured)) || strings.Contains(view, "secret") {
		t.Fatalf("confirm dialog:\n%s", view)
	}
	press(m, "n")
	if files, _ := b.QueueFiles("owner", true); len(files) != 2 {
		t.Fatal("deleted without a yes")
	}
	press(m, "d")
	m.Update(queueFilesMsg{seq: m.qfilesReq.start(), files: QueueFiles{Files: []QueueFile{m.qfiles.Files[1]}}})
	press(m, "y")
	files, err := b.QueueFiles("owner", true)
	if err != nil || len(files) != 1 || files[0].ID == captured || m.noticeErr {
		t.Fatalf("delete must act on the file marked when the dialog opened: %+v %v, notice %q", files, err, m.notice)
	}
	if !slices.Contains([]string{first, second}, files[0].ID) || len(m.qfiles.Files) != 1 {
		t.Fatalf("panel after delete: %+v", m.qfiles.Files)
	}
}

func TestQueueFilesDeleteInUse(t *testing.T) {
	b, c := newServer(t)
	id := addQueueFile(t, b, "owner", "ana-menu.pdf")
	if _, err := b.Enqueue("owner", "", "menu", "", []bot.QueueRequest{{To: "5511999999999", File: id}, {To: "5511988887777", File: id}}); err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t, Options{Client: c})
	press(m, "7", "F")
	if view := ansi.Strip(m.View()); !strings.Contains(view, "2, 2 open") {
		t.Fatalf("use column:\n%s", view)
	}
	press(m, "d", "y")
	if !m.noticeErr || !strings.Contains(m.notice, "still used by 2 queued messages; cancel them first") {
		t.Fatalf("409 notice: %q", m.notice)
	}
	if files, _ := b.QueueFiles("owner", true); len(files) != 1 {
		t.Fatal("a file in use was deleted")
	}
}

func TestQueueFilesOnAnOlderServer(t *testing.T) {
	b, err := bot.New(context.Background(), filepath.Join(t.TempDir(), "wame.db"), waLog.Noop, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	h := api.NewHandler(b, &auth.Authenticator{Store: b.Store, Owner: owner}, "test")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/queue/files") {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, owner, false)
	if err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t, Options{Client: c})
	press(m, "7", "F")
	if view := ansi.Strip(m.View()); !strings.Contains(view, "doesn't have queue files (GET /queue/files) yet") || !strings.Contains(view, "Update wame") {
		t.Fatalf("no update hint:\n%s", view)
	}
	press(m, "d")
	if m.mode != modeNormal || !m.noticeErr || !strings.Contains(m.notice, "update wame") {
		t.Fatalf("d on an older server: mode %v, notice %q", m.mode, m.notice)
	}
	if m.pollQueueFiles(true) != nil {
		t.Fatal("the tick keeps asking an older server")
	}
}

func TestQueueFilesReadOnlyWhenForbidden(t *testing.T) {
	_, c := newServer(t)
	m := newTestModel(t, Options{Client: c})
	press(m, "7", "F")
	m.Update(queueFilesMsg{seq: m.qfilesReq.start(), err: &APIError{Code: http.StatusForbidden}})
	press(m, "d")
	if m.mode != modeNormal || !m.noticeErr || !strings.Contains(m.notice, "read-only") {
		t.Fatalf("d on a 403: mode %v, notice %q", m.mode, m.notice)
	}
}
