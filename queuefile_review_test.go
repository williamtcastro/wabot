package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/williamtcastro/wame/internal/admin"
)

// uploadAgainst runs uploadAttachment against a server that answers POST /queue/files with body.
func uploadAgainst(t *testing.T, a attachment, code int, body string) error {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	c, err := admin.NewClient(srv.URL, "wame_test", false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = uploadAttachment(t.Context(), c, a, io.Discard)
	return err
}

func TestUploadAttachmentChecksTheAnswer(t *testing.T) {
	menu := menuFile(t, "menu")
	a, err := checkAttachment(menu)
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimPrefix(a.identity, "sha256:")
	const id = "f_0123456789abcdef0123456789abcdef"

	// A proxy that turned the POST into a GET answers 200 with the file list: not an upload.
	if err := uploadAgainst(t, a, 200, `{"files":[],"usage":{}}`); err == nil || !strings.Contains(err.Error(), "didn't answer with a file id") {
		t.Fatalf("answer without an id: %v", err)
	}
	if err := uploadAgainst(t, a, 201, `{"id":"`+id+`","sha256":"`+sha+`","kind":"document","size":10}`); err != nil {
		t.Fatalf("good answer: %v", err)
	}

	// The file changed between the local check (which chose the key) and the upload.
	if err := os.WriteFile(menu, []byte("%PDF-1.4\n% another menu\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := strings.Repeat("ab", 32)
	if err := uploadAgainst(t, a, 201, `{"id":"`+id+`","sha256":"`+other+`","kind":"document","size":10}`); err == nil || !strings.Contains(err.Error(), "changed while it was being imported") {
		t.Fatalf("changed file: %v", err)
	}
}

// The default key of an import without -file must stay what it was before -file existed, so
// reruns of older imports still replay.
func TestImportKeyWithoutFileUnchanged(t *testing.T) {
	got, err := importKey("", "/data/imports/october.csv", "")
	if err != nil || got != "csv-49207b24918cb86d" {
		t.Fatalf("%q %v", got, err)
	}
}
