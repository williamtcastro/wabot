package api

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/williamtcastro/wame/internal/auth"
	"github.com/williamtcastro/wame/internal/store"
)

func doUpload(h *Handler, name string, data []byte, contentType string, hdr map[string]string) *httptest.ResponseRecorder {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("note", "ignored")
	part, _ := mw.CreatePart(map[string][]string{
		"Content-Disposition": {`form-data; name="file"; filename="` + name + `"`},
		"Content-Type":        {contentType},
	})
	_, _ = part.Write(data)
	_ = mw.Close()
	r := httptest.NewRequest("POST", "/queue/files", &body)
	r.Host = "127.0.0.1:8080"
	r.RemoteAddr = "127.0.0.1:50000"
	r.Header.Set("Content-Type", mw.FormDataContentType())
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func flyerPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, 6, 6))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type uploaded struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Mime    string `json:"mime"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Expires string `json:"expires_at"`
}

func TestQueueFilesEndpoints(t *testing.T) {
	b := newTestBot(t)
	h := newHandler(b, "s3cret-owner-secret")
	owner := bearer("s3cret-owner-secret")
	sender := bearer(issue(t, b, "promo-bot", []auth.Scope{auth.Send}, nil, 0))
	scoped := bearer(issue(t, b, "maria-only", []auth.Scope{auth.Send}, []string{maria}, 0))
	reader := bearer(issue(t, b, "reader", []auth.Scope{auth.Read}, nil, 0))
	pdf := []byte("%PDF-1.4\n% secret menu of Maria\n%%EOF\n")

	if w := doUpload(h, "menu.pdf", pdf, "application/pdf", reader); w.Code != 403 {
		t.Fatalf("read token uploads: %d", w.Code)
	}
	w := doUpload(h, "Cardápio Maria.pdf", pdf, "image/png", sender)
	var f uploaded
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &f) != nil || f.Mime != "application/pdf" || f.Kind != "document" || f.Name != "Cardápio Maria.pdf" || f.Expires == "" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := doUpload(h, "again.pdf", pdf, "application/pdf", sender); w.Code != 200 || !strings.Contains(w.Body.String(), f.ID) {
		t.Fatalf("the same file again: %d %s", w.Code, w.Body)
	}
	if w := doUpload(h, "page.html", []byte("<html><script>alert(1)</script></html>"), "application/pdf", sender); w.Code != 415 {
		t.Fatalf("html: %d %s", w.Code, w.Body)
	}
	old := maxUpload
	maxUpload = 10
	if w := doUpload(h, "menu2.pdf", append(pdf, 'x'), "application/pdf", sender); w.Code != 413 {
		t.Fatalf("too large: %d %s", w.Code, w.Body)
	}
	maxUpload = old
	mine := doUpload(h, "flyer.png", flyerPNG(t), "image/png", scoped)
	var theirs uploaded
	if mine.Code != 201 || json.Unmarshal(mine.Body.Bytes(), &theirs) != nil || theirs.Kind != "image" {
		t.Fatalf("%d %s", mine.Code, mine.Body)
	}

	// Only own files can be queued; someone else's id is a 404, not a 403.
	for _, c := range []struct {
		hdr  map[string]string
		body string
		code int
	}{
		{scoped, `{"file":"` + f.ID + `","messages":[{"to":"5511999999999"}]}`, 404},
		{owner, `{"file":"` + f.ID + `","messages":[{"to":"5511999999999"}]}`, 404},
		{sender, `{"file":"f_00","messages":[{"to":"5511999999999"}]}`, 400},
		{sender, `{"messages":[{"to":"5511999999999","file":"` + theirs.ID + `"}]}`, 404},
		{scoped, `{"file":"` + theirs.ID + `","messages":[{"to":"5511988887777"}]}`, 403},
	} {
		if w := doJSON(h, "POST", "/queue", c.body, c.hdr); w.Code != c.code {
			t.Errorf("%s: %d %s", c.body, w.Code, w.Body)
		}
	}

	key := map[string]string{"Authorization": sender["Authorization"], "Idempotency-Key": "menu-1"}
	body := `{"batch":"menu","file":"` + f.ID + `","text":"Our new menu","messages":[{"to":"5511999999999"},{"to":"5511988887777","text":"Hi John, the menu"}]}`
	w = doJSON(h, "POST", "/queue", body, key)
	var res queued
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &res) != nil || res.Queued != 2 || res.Items[0].FileID != f.ID || res.Items[0].FileKind != "document" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := doJSON(h, "POST", "/queue", body, key); w.Code != 202 || !strings.Contains(w.Body.String(), `"replayed":2`) {
		t.Fatalf("replay: %d %s", w.Code, w.Body)
	}
	if w := doJSON(h, "POST", "/queue", strings.Replace(body, f.ID, theirs.ID, 1), key); w.Code != 404 {
		t.Fatalf("same key, someone else's file: %d %s", w.Code, w.Body)
	}

	// Names never leave the uploader: not in /queue, not to admins, not in events.
	for _, path := range []string{"/queue", "/queue/" + itoa(res.Items[0].ID)} {
		for _, hdr := range []map[string]string{sender, owner} {
			if w := do(h, "GET", path, hdr); w.Code != 200 || strings.Contains(w.Body.String(), "Cardápio") || strings.Contains(w.Body.String(), "secret menu") {
				t.Fatalf("%s: %d %s", path, w.Code, w.Body)
			}
		}
	}
	w = do(h, "GET", "/queue/files", owner)
	if w.Code != 200 || strings.Contains(w.Body.String(), "Cardápio") || strings.Contains(w.Body.String(), "flyer.png") || !strings.Contains(w.Body.String(), f.ID) {
		t.Fatalf("admin list: %d %s", w.Code, w.Body)
	}
	w = do(h, "GET", "/queue/files", sender)
	var list struct {
		Files []uploaded            `json:"files"`
		Usage store.OutboxFileUsage `json:"usage"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Files) != 1 || list.Files[0].Name != "Cardápio Maria.pdf" || list.Files[0].Expires != "" || list.Usage.Files != 1 {
		t.Fatalf("own list: %d %s", w.Code, w.Body)
	}

	// No route serves the content.
	for _, path := range []string{"/queue/files/" + f.ID, "/media/" + f.ID, "/queue/files/" + f.ID + "/content"} {
		if w := do(h, "GET", path, owner); w.Code == 200 || strings.Contains(w.Body.String(), "secret menu") {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}

	if w := do(h, "DELETE", "/queue/files/"+f.ID, scoped); w.Code != 404 {
		t.Fatalf("someone else's delete: %d", w.Code)
	}
	if w := do(h, "DELETE", "/queue/files/"+f.ID, sender); w.Code != 409 {
		t.Fatalf("delete while queued: %d %s", w.Code, w.Body)
	}
	if w := do(h, "DELETE", "/queue?batch=menu", sender); w.Code != 200 {
		t.Fatalf("cancel: %d", w.Code)
	}
	if w := do(h, "DELETE", "/queue/files/"+f.ID, sender); w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if w := do(h, "DELETE", "/queue/files/..%2F..%2Fwame.db", owner); w.Code != 404 {
		t.Fatalf("traversal: %d", w.Code)
	}
	if w := do(h, "DELETE", "/queue/files/"+theirs.ID, owner); w.Code != 200 {
		t.Fatalf("admin delete: %d %s", w.Code, w.Body)
	}

	if err := b.UpdateSettings("ops", map[string]*string{"queue_files_quota": new("10")}); err != nil {
		t.Fatal(err)
	}
	if w := doUpload(h, "flyer.png", flyerPNG(t), "image/png", sender); w.Code != 413 || !strings.Contains(errorOf(w), "quota") {
		t.Fatalf("quota: %d %s", w.Code, w.Body)
	}
	if w := doJSON(h, "POST", "/queue/files", `{}`, sender); w.Code != 400 {
		t.Fatalf("not multipart: %d", w.Code)
	}

	audit := do(h, "GET", "/audit?limit=200", owner).Body.String()
	if !strings.Contains(audit, "queue_file_delete") || strings.Contains(audit, "Cardápio") || strings.Contains(audit, "flyer") {
		t.Fatalf("audit: %s", audit)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
