package api

import (
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSlowUploadsReleaseTheirSlots(t *testing.T) {
	old := queueUploadTimeout
	queueUploadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { queueUploadTimeout = old })
	b := newTestBot(t)
	h := newHandler(b, "s3cret-owner-secret")
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	stall := func() {
		pr, pw := io.Pipe()
		t.Cleanup(func() { _ = pw.Close() })
		mw := multipart.NewWriter(pw)
		req, _ := http.NewRequestWithContext(t.Context(), "POST", srv.URL+"/queue/files", pr)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Authorization", "Bearer s3cret-owner-secret")
		go func() {
			if resp, err := srv.Client().Do(req); err == nil {
				resp.Body.Close()
			}
		}()
		part, _ := mw.CreateFormFile("file", "menu.pdf")
		_, _ = part.Write([]byte("%PDF-1.4\n")) // then nothing more: a client trickling its upload
	}
	stall()
	stall()
	time.Sleep(600 * time.Millisecond)

	w := doUpload(h, "menu.pdf", []byte("%PDF-1.4\nthe menu\n"), "application/pdf", bearer("s3cret-owner-secret"))
	if w.Code != http.StatusCreated {
		t.Fatalf("after the stalled uploads timed out: %d %s", w.Code, w.Body)
	}
}
