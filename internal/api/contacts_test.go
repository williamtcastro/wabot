package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/williamtcastro/wame/internal/auth"
	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
)

func contactView(t *testing.T, w *httptest.ResponseRecorder) bot.ContactView {
	t.Helper()
	var v bot.ContactView
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	return v
}

func contactList(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	var list []bot.Contact
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	out := []string{}
	for _, c := range list {
		out = append(out, c.JID)
	}
	return out
}

func TestContactWrites(t *testing.T) {
	b := newTestBot(t)
	h := newHandler(b, "")
	admin := bearer(issue(t, b, "boss", []auth.Scope{auth.Admin}, nil, 0))
	reader := bearer(issue(t, b, "reader", []auth.Scope{auth.Read}, nil, 0))

	v := contactView(t, doJSON(h, "PUT", "/contacts/5511999999999", `{"name":"Maria","tags":["Clients","vip"],"notes":"prefers mornings","language":"pt-BR","time_zone":"America/Sao_Paulo"}`, admin))
	if v.JID != maria || v.Display != "Maria" || v.Source != bot.SourceOwn || v.Own == nil || v.Own.Notes != "prefers mornings" || !slices.Equal(v.Tags, []string{"clients", "vip"}) || v.Own.UpdatedBy != "boss" {
		t.Fatalf("PUT = %+v %+v", v, v.Own)
	}

	v = contactView(t, doJSON(h, "PATCH", "/contacts/"+maria, `{"notes":null,"add_tags":["new"]}`, admin))
	if v.Own.Notes != "" || v.Own.Name != "Maria" || v.Own.Language != "pt-BR" || !slices.Equal(v.Tags, []string{"clients", "new", "vip"}) {
		t.Fatalf("PATCH: null must clear and absent must leave alone: %+v", v.Own)
	}
	v = contactView(t, doJSON(h, "PUT", "/contacts/+5511999999999", `{"name":"Maria"}`, admin))
	if v.Own.Name != "Maria" || len(v.Own.Tags) != 0 || v.Own.Language != "" || v.Own.TimeZone != "" {
		t.Fatalf("PUT: absent fields must be cleared: %+v", v.Own)
	}

	for _, c := range []struct {
		method, body string
		code         int
		want         string
	}{
		{"PATCH", `{"nome":"Maria"}`, 400, "unknown field"},
		{"PATCH", `{"name":"Maria"} {}`, 400, "unexpected data"},
		{"PATCH", `{"tags":["BAD TAG!"]}`, 400, "tag"},
		{"PUT", `{"name":"Maria","add_tags":["x"]}`, 400, "only work in a PATCH"},
		{"PATCH", `{"time_zone":"Mars/Olympus"}`, 400, ""},
		{"PATCH", `{"notes":"` + strings.Repeat("x", bot.MaxContactBody) + `"}`, 413, "larger than"},
	} {
		w := doJSON(h, c.method, "/contacts/5511999999999", c.body, admin)
		if w.Code != c.code || !strings.Contains(errorOf(w), c.want) {
			t.Errorf("%s %.40s = %d %s, want %d %q", c.method, c.body, w.Code, w.Body, c.code, c.want)
		}
	}
	if w := doJSON(h, "PUT", "/contacts/120363012345678901@g.us", `{"name":"Family"}`, admin); w.Code != 400 || !strings.Contains(errorOf(w), "not groups") {
		t.Errorf("group = %d %s", w.Code, w.Body)
	}

	for _, c := range []struct{ method, path string }{
		{"PUT", "/contacts/5511999999999"}, {"PATCH", "/contacts/5511999999999"}, {"DELETE", "/contacts/5511999999999"},
		{"DELETE", "/contacts?all=true"}, {"POST", "/contacts/import"}, {"GET", "/contacts/export"},
	} {
		if w := doJSON(h, c.method, c.path, `{"name":"x"}`, reader); w.Code != http.StatusForbidden || !strings.Contains(errorOf(w), "admin scope") {
			t.Errorf("reader %s %s = %d, want 403", c.method, c.path, w.Code)
		}
	}

	if w := do(h, "DELETE", "/contacts/5511988887777", admin); w.Code != 404 {
		t.Fatalf("DELETE of someone without a row = %d, want 404", w.Code)
	}
	if w := do(h, "DELETE", "/contacts/5511999999999", admin); w.Code != 200 {
		t.Fatalf("DELETE = %d %s", w.Code, w.Body)
	}
	if v := contactView(t, do(h, "GET", "/contacts/5511999999999", admin)); v.Own != nil || v.Source != bot.SourceNumber {
		t.Fatalf("after DELETE = %+v", v)
	}

	entries, _ := b.Store.AuditLog(100, 0)
	actions := map[string]int{}
	for _, e := range entries {
		actions[e.Action]++
		for _, secret := range []string{"Maria", "mornings", "clients", "vip", "pt-BR", "Sao_Paulo", "Family"} {
			if strings.Contains(e.Detail, secret) {
				t.Errorf("audit entry %s carries a value: %+v", e.Action, e)
			}
		}
	}
	if actions["contact_set"] < 3 || actions["contact_delete"] != 2 {
		t.Fatalf("audit actions = %v", actions)
	}
}

func TestContactReads(t *testing.T) {
	b := newTestBot(t)
	h := newHandler(b, "")
	admin := bearer(issue(t, b, "boss", []auth.Scope{auth.Admin}, nil, 0))
	reader := bearer(issue(t, b, "reader", []auth.Scope{auth.Read}, nil, 0))
	if v := contactView(t, do(h, "GET", "/contacts/5521987654321", reader)); v.Own != nil || v.Source != bot.SourceNumber || v.Phone != "+5521987654321" {
		t.Fatalf("an unknown person is still a 200: %+v", v)
	}
	for _, c := range []struct{ to, body string }{
		{"5511999999999", `{"name":"Maria","tags":["clients","vip"],"notes":"prefers mornings","language":"pt-BR"}`},
		{"5511988887777", `{"tags":["clients"],"notes":"call after 6"}`},
		{"552187654321", `{"name":"Ana"}`},
	} {
		if w := doJSON(h, "PUT", "/contacts/"+c.to, c.body, admin); w.Code != 200 {
			t.Fatalf("PUT %s = %d %s", c.to, w.Code, w.Body)
		}
	}

	w := do(h, "GET", "/contacts/5511999999999", reader)
	if v := contactView(t, w); v.Own != nil || v.Display != "Maria" || !slices.Equal(v.Tags, []string{"clients", "vip"}) || strings.Contains(w.Body.String(), "mornings") || strings.Contains(w.Body.String(), "pt-BR") {
		t.Fatalf("a read token sees names and tags only: %s", w.Body)
	}
	if v := contactView(t, do(h, "GET", "/contacts/"+maria, admin)); v.Own == nil || v.Own.Notes != "prefers mornings" || v.Own.Language != "pt-BR" {
		t.Fatalf("admin sees the own fields: %+v", v)
	}
	if v := contactView(t, do(h, "GET", "/contacts/5521987654321", admin)); v.Own == nil || v.Display != "Ana" || v.Phone != "+5521987654321" {
		t.Fatalf("the 9th-digit variant finds Ana's row: %+v", v)
	}
	if w := do(h, "GET", "/contacts/12", reader); w.Code != 400 {
		t.Fatalf("bad number = %d", w.Code)
	}

	list := do(h, "GET", "/contacts", reader)
	if got := contactList(t, list); !slices.Equal(got, []string{john, ana, maria}) || strings.Contains(list.Body.String(), "mornings") || strings.Contains(list.Body.String(), `"own":`) {
		t.Fatalf("GET /contacts = %v %s", got, list.Body)
	}
	for target, want := range map[string][]string{
		"/contacts?tag=clients":           {john, maria},
		"/contacts?tag=clients&tag=vip":   {maria},
		"/contacts?tag=clients,vip":       {maria},
		"/contacts?q=MAR":                 {maria},
		"/contacts?q=887777":              {john},
		"/contacts?q=vip":                 {maria},
		"/contacts?known=1&tag=clients":   {john, maria},
		"/contacts?limit=1":               {john},
		"/contacts?tag=nobody":            {},
		"/contacts?limit=99999&q=5521876": {ana},
	} {
		if got := contactList(t, do(h, "GET", target, reader)); !slices.Equal(got, want) {
			t.Errorf("%s = %v, want %v", target, got, want)
		}
	}
	for _, target := range []string{"/contacts?known=maybe", "/contacts?tag=BAD!"} {
		if w := do(h, "GET", target, reader); w.Code != 400 {
			t.Errorf("%s = %d, want 400", target, w.Code)
		}
	}

	var tags []store.TagCount
	if w := do(h, "GET", "/contacts/tags", reader); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &tags) != nil || len(tags) != 2 || tags[0] != (store.TagCount{Tag: "clients", Count: 2}) {
		t.Fatalf("GET /contacts/tags = %d %s", w.Code, w.Body)
	}

	if w := do(h, "DELETE", "/contacts", admin); w.Code != 400 || !strings.Contains(errorOf(w), "all=true") {
		t.Fatalf("DELETE /contacts without ?all=true = %d %s", w.Code, w.Body)
	}
	if got := contactList(t, do(h, "GET", "/contacts", reader)); len(got) != 3 {
		t.Fatalf("an unconfirmed delete-all deleted: %v", got)
	}
	if w := do(h, "DELETE", "/contacts?all=true", admin); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"deleted":3}` {
		t.Fatalf("DELETE /contacts?all=true = %d %s", w.Code, w.Body)
	}
	if got := contactList(t, do(h, "GET", "/contacts", reader)); len(got) != 0 {
		t.Fatalf("after delete all: %v", got)
	}
	entries, _ := b.Store.AuditLog(1, 0)
	if len(entries) != 1 || entries[0].Action != "contact_delete_all" || entries[0].Detail != "3 contacts" {
		t.Fatalf("audit = %+v", entries)
	}
}

func TestContactImportExport(t *testing.T) {
	b := newTestBot(t)
	h := newHandler(b, "")
	admin := bearer(issue(t, b, "boss", []auth.Scope{auth.Admin}, nil, 0))
	count := func() int {
		list, err := b.Store.Contacts(store.ContactFilter{})
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}

	bad := "number,name\n5511999999999,Maria\n12,John\n"
	if w := doCSV(h, "/contacts/import", bad, admin); w.Code != 400 || !strings.Contains(errorOf(w), "line 3") || count() != 0 {
		t.Fatalf("a bad line must store nothing: %d %s, %d stored", w.Code, w.Body, count())
	}
	if w := doCSV(h, "/contacts/import", "number,nome\n5511999999999,Maria\n", admin); w.Code != 400 || !strings.Contains(errorOf(w), "nome") {
		t.Fatalf("unknown column = %d %s", w.Code, w.Body)
	}

	good := "Phone;Name;Tags;Notes\r\n5511999999999;Maria;clients vip;\r\n5511988887777;John;clients;\"prefers mornings, after 9\nand never on Sundays\"\r\n552187654321;;;\r\n"
	var res store.ImportResult
	w := doCSV(h, "/contacts/import?dry_run=1&add_tags=imported", good, admin)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &res) != nil || res != (store.ImportResult{Added: 3}) || count() != 0 {
		t.Fatalf("dry run = %d %s, %d stored", w.Code, w.Body, count())
	}
	w = doCSV(h, "/contacts/import?add_tags=imported", good, admin)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &res) != nil || res != (store.ImportResult{Added: 3}) || count() != 3 {
		t.Fatalf("import = %d %s, %d stored", w.Code, w.Body, count())
	}
	if v := contactView(t, do(h, "GET", "/contacts/552187654321", admin)); v.Own == nil || !slices.Equal(v.Tags, []string{"imported"}) {
		t.Fatalf("add_tags = %+v", v)
	}
	if w := doCSV(h, "/contacts/import?dry_run=perhaps", good, admin); w.Code != 400 {
		t.Fatalf("bad dry_run = %d", w.Code)
	}

	huge := "number,notes\n5511999999999,\"" + strings.Repeat("x", bot.MaxContactImportBytes) + "\"\n"
	if w := doCSV(h, "/contacts/import", huge, admin); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over %d bytes = %d %s", bot.MaxContactImportBytes, w.Code, w.Body)
	}

	exported := do(h, "GET", "/contacts/export", admin)
	if exported.Code != 200 || !strings.HasPrefix(exported.Header().Get("Content-Type"), "text/csv") ||
		!strings.HasPrefix(exported.Body.String(), strings.Join(bot.ContactImportColumns, ",")+"\n") {
		t.Fatalf("export = %d %q", exported.Code, exported.Body)
	}
	before, _ := b.Store.Contacts(store.ContactFilter{})
	if w := do(h, "DELETE", "/contacts?all=true", admin); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := doCSV(h, "/contacts/import", exported.Body.String(), admin); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &res) != nil || res.Added != 3 {
		t.Fatalf("re-import = %d %s", w.Code, w.Body)
	}
	after, _ := b.Store.Contacts(store.ContactFilter{})
	if len(after) != len(before) {
		t.Fatalf("round trip: %d → %d", len(before), len(after))
	}
	for i := range before {
		x, y := before[i], after[i]
		if x.JID != y.JID || x.Name != y.Name || !slices.Equal(x.Tags, y.Tags) || x.Notes != y.Notes || x.Language != y.Language || x.TimeZone != y.TimeZone {
			t.Errorf("round trip changed %+v into %+v", x, y)
		}
	}
	if again := do(h, "GET", "/contacts/export", admin); again.Body.String() != exported.Body.String() {
		t.Fatalf("export after a round trip differs:\n%s\n%s", exported.Body, again.Body)
	}

	entries, _ := b.Store.AuditLog(100, 0)
	imports := 0
	for _, e := range entries {
		if e.Action == "contact_import" {
			imports++
			if e.Target != "csv" || strings.Contains(e.Detail, "Maria") || strings.Contains(e.Detail, "mornings") || strings.Contains(e.Detail, "clients") {
				t.Errorf("import audit = %+v", e)
			}
		}
	}
	if imports < 5 {
		t.Fatalf("%d contact_import entries", imports)
	}
}
