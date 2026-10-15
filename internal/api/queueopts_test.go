package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/williamtcastro/wame/internal/auth"
	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
)

type queuedWithSkips struct {
	queued
	Skipped     bot.SkipCounts   `json:"skipped"`
	SkippedRows []bot.SkippedRow `json:"skipped_rows"`
}

func TestQueueTagsTemplatesAndSkips(t *testing.T) {
	b := newTestBot(t)
	h := newHandler(b, "s3cret-owner-secret")
	for to, name := range map[string]string{"5511999999999": "Maria", "5511988887777": "John"} {
		if _, err := b.PatchContact(t.Context(), "ops", to, bot.ContactInput{Name: &name, AddTags: []string{"clients"}}); err != nil {
			t.Fatal(err)
		}
	}
	both := bearer(issue(t, b, "promo-bot", []auth.Scope{auth.Read, auth.Send}, nil, 0))
	sendOnly := bearer(issue(t, b, "send-bot", []auth.Scope{auth.Send}, nil, 0))
	scoped := bearer(issue(t, b, "maria-only", []auth.Scope{auth.Read, auth.Send}, []string{maria}, 0))

	body := `{"batch":"promo","template":true,"dedupe":true,"messages":[
		{"to":"tag:clients","text":"Hi {first_name}!"},
		{"to":"5511999999999","text":"Hi {first_name}!"},
		{"to":"552187654321","text":"Hi {name}!","name":"Ana"}]}`
	w := doBody(h, "POST", "/queue", body, both)
	var res queuedWithSkips
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &res) != nil || res.Queued != 3 || res.Skipped.Duplicate != 1 || len(res.SkippedRows) != 1 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "Maria") || strings.Contains(w.Body.String(), "clients") {
		t.Fatalf("the answer carries no names, tags or text: %s", w.Body)
	}
	if it, _ := b.Store.OutboxItem(res.Items[2].ID); it.Text != "Hi Ana!" {
		t.Fatalf("%+v", it)
	}
	if e, _ := b.Store.AuditLog(1, 0); e[0].Detail != "3 messages, 1 skipped" {
		t.Fatalf("audit %+v", e[0])
	}

	w = doBody(h, "POST", "/queue", `{"batch":"promo","skip_sent":"48h","messages":[{"to":"5511999999999","text":"again"}]}`, both)
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &res) != nil || res.Queued != 0 || res.Batch != "promo" || res.Skipped.RecentlySent != 1 || res.SkippedRows[0].Reason != "recently_sent" {
		t.Fatalf("all skipped is a 202: %d %s", w.Code, w.Body)
	}

	for _, c := range []struct {
		body string
		hdr  map[string]string
		code int
		want string
	}{
		{`{"messages":[{"to":"tag:clients","text":"hi"}]}`, sendOnly, 400, "read scope"},
		{`{"template":true,"messages":[{"to":"5511999999999","text":"hi {name}"}]}`, sendOnly, 400, "read scope"},
		{`{"skip_sent":"30d","messages":[{"to":"5511999999999","text":"hi"}]}`, sendOnly, 400, "at most 7 days"},
		{`{"skip_sent":"forever","messages":[{"to":"5511999999999","text":"hi"}]}`, both, 400, "must be a duration"},
		{`{"messages":[{"to":"tag:vip","text":"hi"}]}`, both, 400, "tag vip has no contacts"},
		{`{"messages":[{"to":"tag:vip","text":"hi"}]}`, scoped, 400, "no contacts this token can reach"},
	} {
		if w := doBody(h, "POST", "/queue", c.body, c.hdr); w.Code != c.code || !strings.Contains(errorOf(w), c.want) {
			t.Errorf("%s: %d %s, want %d %q", c.body, w.Code, w.Body, c.code, c.want)
		}
	}

	w = doBody(h, "POST", "/queue", `{"batch":"scoped","messages":[{"to":"tag:clients","text":"hi"}]}`, scoped)
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &res) != nil || res.Queued != 1 || res.Items[0].To != maria {
		t.Fatalf("a restricted token reaches only the allowlisted members: %d %s", w.Code, w.Body)
	}
}

func TestQueueCSVOptions(t *testing.T) {
	b := newTestBot(t)
	h := newHandler(b, "s3cret-owner-secret")
	both := bearer(issue(t, b, "promo-bot", []auth.Scope{auth.Read, auth.Send}, nil, 0))
	body := "to,text,name\n5511999999999,Hi {name},Maria\n5511999999999,Hi {name},Maria\n5511988887777,Hi {name|there},\n"
	w := doCSV(h, "/queue?batch=csv&template=1&dedupe=true&skip_sent=48h", body, both)
	var res queuedWithSkips
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &res) != nil || res.Queued != 2 || res.SkippedRows[0].Line != 3 || res.SkippedRows[0].Index != 1 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	for i, want := range []string{"Hi Maria", "Hi there"} {
		if it, _ := b.Store.OutboxItem(res.Items[i].ID); it.Text != want {
			t.Errorf("%d: %q", i, it.Text)
		}
	}
	if w := doCSV(h, "/queue?dedupe=maybe", body, both); w.Code != 400 || !strings.Contains(errorOf(w), "?dedupe must be") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := doCSV(h, "/queue?template=1", "to,text\n5511999999999,Hi {nome}\n", both); w.Code != 400 || !strings.Contains(errorOf(w), "line 2: unknown placeholder {nome}") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if c, _ := b.Store.CountOutbox(store.OutboxFilter{}); c.Queued != 2 {
		t.Fatalf("counts %+v", c)
	}
}
