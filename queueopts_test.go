package main

import (
	"strings"
	"testing"

	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
)

func TestQueueImportOptions(t *testing.T) {
	b, _ := queueServer(t)
	smallChunks(t)
	for to, name := range map[string]string{"5511999999999": "Maria", "5511988887777": "John"} {
		if _, err := b.PatchContact(t.Context(), "ops", to, bot.ContactInput{Name: &name, AddTags: []string{"clients"}}); err != nil {
			t.Fatal(err)
		}
	}
	var body strings.Builder
	body.WriteString("to,text,name\n")
	for range 12 {
		body.WriteString("552187654321,\"Hi {name}\",Ana\n") // the same row 12 times: across chunks too
	}
	body.WriteString("tag:clients,\"Hi {first_name|there}\",\n")
	file := csvFileTo(t, body.String())

	out, err := runQueue(t, "import", "-dry-run", "-template", "-dedupe", "-batch", "promo", file)
	if err != nil || !strings.Contains(out, "2 messages to 1 recipients") || !strings.Contains(out, "tag:clients (expanded by the server)") || !strings.Contains(out, "11 duplicate rows left out") {
		t.Fatalf("%v\n%s", err, out)
	}
	out, err = runQueue(t, "import", "-template", "-dedupe", "-batch", "promo", file)
	if err != nil || !strings.Contains(out, "queued 3 messages, skipped 11 (11 duplicates), batch promo") {
		t.Fatalf("%v\n%s", err, out)
	}
	items, _ := b.Store.ListOutbox(store.OutboxFilter{Batch: "promo"}, 10)
	var texts []string
	for _, it := range items {
		texts = append(texts, it.Text)
	}
	if strings.Join(texts, "|") != "Hi Maria|Hi John|Hi Ana" {
		t.Fatalf("%v", texts)
	}

	out, err = runQueue(t, "import", "-skip-sent", "48h", "-batch", "news", "-text", "News", csvFileTo(t, "to\n5511999999999\n5511988887777\n14155550100\n"))
	if err != nil || !strings.Contains(out, "queued 1 messages, skipped 2 (2 sent in the last 48h), batch news") {
		t.Fatalf("%v\n%s", err, out)
	}
	out, err = runQueue(t, "import", "-skip-sent", "batch:promo", "-batch", "news2", "-text", "News", csvFileTo(t, "to\n552187654321\n14155550100\n"))
	if err != nil || !strings.Contains(out, "skipped 1 (1 already in batch promo)") {
		t.Fatalf("%v\n%s", err, out)
	}

	for _, c := range []struct{ args, body, want string }{
		{"-skip-sent 100d", "to,text\n5511999999999,hi\n", "skip_sent is at most 90 days"},
		{"-template", "to,text\n5511999999999,Hi {nome}\n", "line 2: unknown placeholder {nome}"},
		{"", "to,text,name\ntag:clients,hi,Ana\n", "line 2: a name can't be given for a tag"},
		{"", "to,text\ntag:Not Valid,hi\n", "line 2: tag"},
	} {
		args := append(append([]string{"import", "-dry-run"}, strings.Fields(c.args)...), csvFileTo(t, c.body))
		if _, err := runQueue(t, args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %q: %v, want %q", c.args, c.body, err, c.want)
		}
	}
}

func TestChunkRowsNameColumnOnlyWhenUsed(t *testing.T) {
	chunks, err := chunkRows([]bot.QueueRequest{{To: "5511999999999", Text: "hi", Line: 2}})
	if err != nil || !strings.HasPrefix(string(chunks[0].body), "to,text,send_at,reply_to\n") {
		t.Fatalf("%v %q", err, chunks[0].body)
	}
	chunks, err = chunkRows([]bot.QueueRequest{{To: "5511999999999", Text: "hi", Name: "Maria", Line: 2}})
	if err != nil || string(chunks[0].body) != "to,text,send_at,reply_to,name\n5511999999999,hi,,,Maria\n" {
		t.Fatalf("%v %q", err, chunks[0].body)
	}
}
