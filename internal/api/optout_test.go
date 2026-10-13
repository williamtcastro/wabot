package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/williamtcastro/wame/internal/auth"
	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
)

func TestOptOutEndpoints(t *testing.T) {
	b := newTestBot(t)
	h := newHandler(b, "s3cret-owner-secret")
	owner := bearer("s3cret-owner-secret")
	sender := bearer(issue(t, b, "promo-bot", []auth.Scope{auth.Send}, nil, 0))

	if w := do(h, "GET", "/optouts", sender); w.Code != 403 {
		t.Fatalf("GET /optouts without admin: %d", w.Code)
	}
	if w := doBody(h, "POST", "/optouts", `{"chat":"5511999999999"}`, sender); w.Code != 403 {
		t.Fatalf("POST /optouts without admin: %d", w.Code)
	}
	if w := do(h, "DELETE", "/optouts/5511999999999", sender); w.Code != 403 {
		t.Fatalf("DELETE /optouts without admin: %d", w.Code)
	}
	if w := doBody(h, "POST", "/optouts", `{"chat":"5511999999999"}`, owner); w.Code != 201 || !strings.Contains(w.Body.String(), `"chat":"`+maria+`"`) {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	if w := doBody(h, "POST", "/optouts", `{"chat":"`+maria+`"}`, owner); w.Code != 200 || !strings.Contains(w.Body.String(), `"added":false`) {
		t.Fatalf("add again: %d %s", w.Code, w.Body)
	}
	if w := doBody(h, "POST", "/optouts", `{"chat":"120363000000000001@g.us"}`, owner); w.Code != 400 {
		t.Fatalf("group: %d %s", w.Code, w.Body)
	}
	var list []store.OptOut
	if w := do(h, "GET", "/optouts", owner); json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list) != 1 || list[0].By != "api" {
		t.Fatalf("list: %s", w.Body)
	}
	if w := do(h, "DELETE", "/optouts/5511999999999", owner); w.Code != 200 {
		t.Fatalf("remove: %d %s", w.Code, w.Body)
	}
	if w := do(h, "DELETE", "/optouts/"+maria, owner); w.Code != 404 {
		t.Fatalf("remove again: %d %s", w.Code, w.Body)
	}
	log, _ := b.Store.AuditLog(20, 0)
	var actions []string
	for _, e := range log {
		if strings.HasPrefix(e.Action, "optout") {
			actions = append(actions, e.Action)
		}
	}
	if strings.Join(actions, ",") != "optout_remove,optout_remove,optout_add,optout_add,optout_add" {
		t.Fatalf("audit: %v", actions)
	}
}

func TestQueueShowsReceipts(t *testing.T) {
	b := newTestBot(t)
	h := newHandler(b, "s3cret-owner-secret")
	owner := bearer("s3cret-owner-secret")
	items, err := b.Enqueue("api", "", "promo", "", []bot.QueueRequest{{To: "5511999999999", Text: "Hi Maria"}, {To: "5511988887777", Text: "Hi John"}})
	if err != nil {
		t.Fatal(err)
	}
	for i, it := range items {
		if _, err := b.Store.ClaimOutbox(it.ID, fmt.Sprintf("MSG%d", i)); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Store.FinishOutbox(it.ID, store.OutboxResult{Status: store.OutboxSent}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.Store.MarkOutboxReceipt([]string{"MSG0"}, true, time.Now()); err != nil {
		t.Fatal(err)
	}

	var it map[string]any
	if w := do(h, "GET", fmt.Sprintf("/queue/%d", items[0].ID), owner); json.Unmarshal(w.Body.Bytes(), &it) != nil || it["delivered_at"] == nil || it["read_at"] == nil {
		t.Fatalf("read item: %s", w.Body)
	}
	if w := do(h, "GET", fmt.Sprintf("/queue/%d", items[1].ID), owner); strings.Contains(w.Body.String(), "delivered_at") || strings.Contains(w.Body.String(), "read_at") {
		t.Fatalf("no receipts yet must leave the fields out: %s", w.Body)
	}
	var list struct {
		Counts store.OutboxCounts `json:"counts"`
	}
	if w := do(h, "GET", "/queue", owner); json.Unmarshal(w.Body.Bytes(), &list) != nil || list.Counts.Sent != 2 || list.Counts.Delivered != 1 || list.Counts.Read != 1 {
		t.Fatalf("counts: %s", w.Body)
	}
	var stats Stats
	if w := do(h, "GET", "/stats", owner); json.Unmarshal(w.Body.Bytes(), &stats) != nil || stats.Queue.Delivered != 1 || stats.Queue.Read != 1 || stats.Queue.Timezone == "" {
		t.Fatalf("stats: %s", w.Body)
	}
}
