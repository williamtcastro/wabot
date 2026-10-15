package bot

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/williamtcastro/wame/internal/store"
)

func tagged(t *testing.T, b *Bot, to string, tags ...string) {
	t.Helper()
	patch(t, b, to, ContactInput{AddTags: tags})
}

func keysOf(items []store.OutboxItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Key
	}
	return out
}

// markSent finishes an item as sent, as the worker would.
func markSent(t *testing.T, b *Bot, id int64) {
	t.Helper()
	if _, err := b.Store.ClaimOutbox(id, "MSG"+time.Now().Format("150405.000000000")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Store.FinishOutbox(id, store.OutboxResult{Status: store.OutboxSent}); err != nil {
		t.Fatal(err)
	}
}

func TestQueueTagExpansion(t *testing.T) {
	b := newTestBot(t)
	tagged(t, b, "5511999999999", "clients")
	tagged(t, b, "5511988887777", "clients", "vip")
	tagged(t, b, "552187654321", "other")
	opt := QueueOptions{Actor: "ops", Batch: "promo", Key: "k", Read: true}
	reqs := []QueueRequest{{To: "5521987654321", Text: "Hi Ana"}, {To: "tag:Clients", Text: "Hi!"}}

	res, err := b.Queue(t.Context(), opt, reqs)
	if err != nil || len(res.Items) != 3 {
		t.Fatalf("%v %+v", err, res)
	}
	if got, want := keysOf(res.Items), []string{"k#0", "k#1:" + john, "k#1:" + maria}; !slices.Equal(got, want) {
		t.Fatalf("keys %v, want %v", got, want)
	}
	if res.Items[1].To != john || res.Items[1].RecipientKey != john {
		t.Fatalf("member item %+v", res.Items[1])
	}
	if e := lastAudit(t, b); e.Detail != "3 messages" || strings.Contains(e.Detail+e.Error, "clients") {
		t.Fatalf("audit %+v", e)
	}

	tagged(t, b, "552187654321", "clients")
	again, err := b.Queue(t.Context(), opt, reqs)
	if err != nil || len(again.Items) != 4 || again.Replayed != 3 {
		t.Fatalf("a retry after the tag grew must replay the members queued and add the new one: %v %+v", err, again)
	}

	for _, c := range []struct {
		opt  QueueOptions
		to   string
		want string
	}{
		{QueueOptions{Actor: "ops"}, "tag:clients", "need a token with the read scope"},
		{QueueOptions{Actor: "ops", Read: true}, "tag:nobody", "tag nobody has no contacts"},
		{QueueOptions{Actor: "ops", Read: true, Allow: []string{"14155550100@s.whatsapp.net"}}, "tag:clients", "tag clients has no contacts this token can reach"},
		{QueueOptions{Actor: "ops", Read: true}, "tag:Bad Tag", "must be 1 to 32"},
	} {
		if _, err := b.Queue(t.Context(), c.opt, []QueueRequest{{To: c.to, Text: "hi"}}); !isInputError(err) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s with %+v = %v, want %q", c.to, c.opt, err, c.want)
		}
	}
	if _, err := b.Queue(t.Context(), QueueOptions{Actor: "ops", Read: true}, []QueueRequest{{To: "tag:clients", Text: "hi", Name: "Ana"}}); !isInputError(err) {
		t.Fatalf("a name on a tag row = %v", err)
	}

	restricted := QueueOptions{Actor: "support", Read: true, Allow: []string{maria}}
	res, err = b.Queue(t.Context(), restricted, []QueueRequest{{To: "tag:clients", Text: "hi"}})
	if err != nil || len(res.Items) != 1 || res.Items[0].To != maria || res.Skipped.Total() != 0 || len(res.SkippedRows) != 0 {
		t.Fatalf("members outside the allowlist must be dropped without a trace: %v %+v", err, res)
	}
}

func TestQueueRowChangedBetweenNumberAndTag(t *testing.T) {
	b := newTestBot(t)
	tagged(t, b, "5511999999999", "clients")
	tagged(t, b, "5511988887777", "clients")
	opt := QueueOptions{Actor: "ops", Key: "k", Read: true}
	if _, err := b.Queue(t.Context(), opt, []QueueRequest{{To: "5511999999999", Text: "Hi"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Queue(t.Context(), opt, []QueueRequest{{To: "tag:clients", Text: "Hi"}}); !errors.Is(err, ErrKeyReused) {
		t.Fatalf("a row changed from a number to a tag under the same key must be a 409, not a second message to Maria: %v", err)
	}
	opt.Dedupe = true // the lookup-first path
	if _, err := b.Queue(t.Context(), opt, []QueueRequest{{To: "tag:clients", Text: "Hi"}}); !errors.Is(err, ErrKeyReused) {
		t.Fatalf("same with dedupe: %v", err)
	}
	opt.Key = "k2"
	if _, err := b.Queue(t.Context(), opt, []QueueRequest{{To: "tag:clients", Text: "Hi"}}); err != nil {
		t.Fatal(err)
	}
	opt.Dedupe = false
	if _, err := b.Queue(t.Context(), opt, []QueueRequest{{To: "5511999999999", Text: "Hi"}}); !errors.Is(err, ErrKeyReused) {
		t.Fatalf("and back from a tag to a number: %v", err)
	}
	if c, _ := b.Store.CountOutbox(store.OutboxFilter{}); c.Queued != 3 {
		t.Fatalf("counts %+v", c)
	}
}

func TestQueueReviewFindings(t *testing.T) {
	b := newTestBot(t)
	tagged(t, b, "5511999999999", "a")
	tagged(t, b, "5511988887777", "b")

	// An unnamed batch: a retry answers the batch the items are in, not a new name.
	first, err := b.Queue(t.Context(), QueueOptions{Actor: "ops", Key: "k1"}, []QueueRequest{{To: "552187654321", Text: "Hi"}})
	if err != nil {
		t.Fatal(err)
	}
	again, err := b.Queue(t.Context(), QueueOptions{Actor: "ops", Key: "k1"}, []QueueRequest{{To: "552187654321", Text: "Hi"}})
	if err != nil || again.Batch != first.Batch || lastAudit(t, b).Target != first.Batch {
		t.Fatalf("retry batch %q, first %q: %v", again.Batch, first.Batch, err)
	}

	// Another name column under the same key is another request.
	opt := QueueOptions{Actor: "ops", Key: "k2", Template: true, Read: true}
	if _, err := b.Queue(t.Context(), opt, []QueueRequest{{To: "552187654321", Text: "Hi {name}", Name: "Ana"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Queue(t.Context(), opt, []QueueRequest{{To: "552187654321", Text: "Hi {name}", Name: "Maria"}}); !errors.Is(err, ErrKeyReused) {
		t.Fatalf("a changed name column = %v", err)
	}

	// Another tag under the same key is another request.
	opt = QueueOptions{Actor: "ops", Key: "k3", Read: true}
	if _, err := b.Queue(t.Context(), opt, []QueueRequest{{To: "tag:a", Text: "Hi"}}); err != nil {
		t.Fatal(err)
	}
	tagged(t, b, "5511999999999", "b")
	if _, err := b.Queue(t.Context(), opt, []QueueRequest{{To: "tag:b", Text: "Hi"}}); !errors.Is(err, ErrKeyReused) {
		t.Fatalf("a changed tag = %v", err)
	}

	// Errors about a member name its place, not its number (errors are audited).
	_, err = b.Queue(t.Context(), QueueOptions{Actor: "ops", Template: true, Read: true}, []QueueRequest{{To: "tag:b", Text: "Hi {name}"}})
	if !isInputError(err) || strings.Contains(err.Error(), "5511") || !strings.Contains(err.Error(), "member 1") {
		t.Fatalf("%v", err)
	}
	if e := lastAudit(t, b); strings.Contains(e.Error, "5511") {
		t.Fatalf("audit %+v", e)
	}
}

func TestQueueTagOverTheBatchLimit(t *testing.T) {
	b := newTestBot(t)
	tagged(t, b, "5511999999999", "clients")
	tagged(t, b, "5511988887777", "clients")
	reqs := make([]QueueRequest, MaxQueueBatch-1)
	for i := range reqs {
		reqs[i] = QueueRequest{To: "552187654321", Text: "hi"}
	}
	reqs = append(reqs, QueueRequest{To: "tag:clients", Text: "hi"})
	if _, err := b.Queue(t.Context(), QueueOptions{Actor: "ops", Read: true}, reqs); !isInputError(err) || !strings.Contains(err.Error(), "once tags are expanded") {
		t.Fatalf("%v", err)
	}
}

func TestQueueTagMemberReplayAfterRelink(t *testing.T) {
	b := newTestBot(t)
	withLIDs(t, b)
	const mariaLID = "100000000000001@lid"
	tagged(t, b, mariaLID, "clients")
	opt := QueueOptions{Actor: "ops", Key: "k", Read: true}
	reqs := []QueueRequest{{To: "tag:clients", Text: "Hi"}}
	first, err := b.Queue(t.Context(), opt, reqs)
	if err != nil || len(first.Items) != 1 || first.Items[0].Key != "k#0:"+mariaLID {
		t.Fatalf("%v %+v", err, first)
	}
	if err := b.Client.Store.LIDs.PutLIDMapping(t.Context(), jidOf(mariaLID), jidOf(maria)); err != nil {
		t.Fatal(err)
	}
	b.Store.RelinkLater(mariaLID)
	again, err := b.Queue(t.Context(), opt, reqs)
	if err != nil || again.Replayed != 1 || len(again.Items) != 1 || again.Items[0].ID != first.Items[0].ID {
		t.Fatalf("a member now known by number must replay the item queued under its @lid, not queue a second one: %v %+v", err, again)
	}
	if c, _ := b.Store.CountOutbox(store.OutboxFilter{}); c.Queued != 1 {
		t.Fatalf("counts %+v", c)
	}
}

func TestQueueTemplates(t *testing.T) {
	b := newTestBot(t)
	withBook(t, b, fakeBook{
		jidOf(john): {PushName: "Johnny"},
		jidOf(ana):  {FullName: "Ana Paula Souza"},
	})
	patch(t, b, maria, ContactInput{Name: ptr("Maria")})
	opt := QueueOptions{Actor: "ops", Key: "k", Template: true, Read: true}

	res, err := b.Queue(t.Context(), opt, []QueueRequest{
		{To: "5511999999999", Text: "Hi {name}, {{literal}} and {first_name|friend}!"},
		{To: "552187654321", Text: "Hi {first_name}"},
		{To: "5511988887777", Text: "Hi {name|there} ({push_name})"},
		{To: "5511988887777", Text: "Hi {name}", Name: "Jo‮hn\n Smith​"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"Hi Maria, {literal} and Maria!", "Hi Ana", "Hi there (Johnny)", "Hi John Smith"} {
		if res.Items[i].Text != want || res.Items[i].SourceHash == "" {
			t.Errorf("item %d: %q, want %q", i, res.Items[i].Text, want)
		}
	}

	_, err = b.Queue(t.Context(), QueueOptions{Actor: "ops", Template: true, Read: true}, []QueueRequest{
		{To: "5511988887777", Text: "Hi {name}", Line: 2},
		{To: "5511999999999", Text: "Hi {name}", Line: 3},
		{To: "120363012345678901@g.us", Text: "Hi {name}", Line: 4},
	})
	if !isInputError(err) || !strings.Contains(err.Error(), "no name for line 2, line 4:") {
		t.Fatalf("a push name must never fill {name}, nor a group's subject: %v", err)
	}
	for _, text := range []string{"Hi {nmae}", "Hi {name", "Hi }", "Hi {secret: the vault code is 1234}"} {
		_, err := b.Queue(t.Context(), opt, []QueueRequest{{To: "5511999999999", Text: text}})
		if !isInputError(err) {
			t.Errorf("%q = %v", text, err)
		}
		if strings.Contains(err.Error(), "vault") {
			t.Errorf("an error quoted the message text: %v", err)
		}
	}
	if _, err := b.Queue(t.Context(), QueueOptions{Actor: "ops", Template: true}, []QueueRequest{{To: "5511999999999", Text: "Hi {name}"}}); !isInputError(err) {
		t.Fatalf("templates without the read scope = %v", err)
	}
	if res, err := b.Queue(t.Context(), QueueOptions{Actor: "ops"}, []QueueRequest{{To: "5511999999999", Text: "Hi {name}"}}); err != nil || res.Items[0].Text != "Hi {name}" || res.Items[0].SourceHash != "" {
		t.Fatalf("without template the braces are text: %v %+v", err, res)
	}
	if e := lastAudit(t, b); strings.Contains(e.Detail, "Maria") {
		t.Fatalf("audit %+v", e)
	}
}

func TestQueueTemplateReplayAfterRename(t *testing.T) {
	b := newTestBot(t)
	patch(t, b, maria, ContactInput{Name: ptr("Maria")})
	opt := QueueOptions{Actor: "ops", Key: "k", Template: true, Read: true}
	reqs := []QueueRequest{{To: "5511999999999", Text: "Hi {name}"}}
	first, err := b.Queue(t.Context(), opt, reqs)
	if err != nil || first.Items[0].Text != "Hi Maria" {
		t.Fatalf("%v %+v", err, first)
	}
	patch(t, b, maria, ContactInput{Name: ptr("Maria Silva")})
	again, err := b.Queue(t.Context(), opt, reqs)
	if err != nil || again.Replayed != 1 || again.Items[0].Text != "Hi Maria" {
		t.Fatalf("a rename must not turn a retry into a 409, and the queued text stays: %v %+v", err, again)
	}
	patch(t, b, maria, ContactInput{Name: ptr("")})
	if again, err := b.Queue(t.Context(), opt, reqs); err != nil || again.Replayed != 1 {
		t.Fatalf("a retry after the name was removed replays what was queued: %v %+v", err, again)
	}
	if _, err := b.Queue(t.Context(), opt, append(slices.Clone(reqs), reqs[0])); !isInputError(err) || !strings.Contains(err.Error(), "no name for messages[1]") {
		t.Fatalf("a new row without a name is still refused: %v", err)
	}
	if _, err := b.Queue(t.Context(), opt, []QueueRequest{{To: "5511999999999", Text: "Hello {name}"}}); !errors.Is(err, ErrKeyReused) {
		t.Fatalf("another template under the same key = %v", err)
	}
	opt.Template = false
	if _, err := b.Queue(t.Context(), opt, []QueueRequest{{To: "5511999999999", Text: "Hi Maria"}}); !errors.Is(err, ErrKeyReused) {
		t.Fatalf("the rendered text without template under the same key = %v", err)
	}
}

func TestTemplateNameCleaned(t *testing.T) {
	long := strings.Repeat("a", 100)
	if got := templateNameOf(long); len([]rune(got)) != maxTemplateName {
		t.Fatalf("%d runes", len([]rune(got)))
	}
	if got := templateNameOf("Ana\r\n\tPaula⁦️"); got != "Ana Paula" {
		t.Fatalf("%q", got)
	}
	parts, err := parseTemplate("{{name}} {first_name|} x}}")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := renderTemplate(parts, templateNames{}); err != nil || got != "{name}  x}" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestQueueDedupe(t *testing.T) {
	b := newTestBot(t)
	opt := QueueOptions{Actor: "ops", Key: "k", Dedupe: true}
	reqs := []QueueRequest{
		{To: "5511999999999", Text: "Hi", Line: 2},
		{To: "+55 11 99999-9999", Text: "Hi", Line: 3},
		{To: "5511988887777", Text: "Hi", Line: 4},
		{To: "5511999999999", Text: "Other", Line: 5},
		{To: "551199999999", Text: "Hi", Line: 6}, // Maria's other 9th-digit form
	}
	res, err := b.Queue(t.Context(), opt, reqs)
	if err != nil || len(res.Items) != 3 || res.Skipped != (SkipCounts{Duplicate: 2}) {
		t.Fatalf("%v %+v", err, res)
	}
	if got := keysOf(res.Items); !slices.Equal(got, []string{"k#0", "k#2", "k#3"}) {
		t.Fatalf("keys keep the rows' own index: %v", got)
	}
	if res.SkippedRows[0] != (SkippedRow{Line: 3, Index: 1, To: "+55 11 99999-9999", Reason: store.SkipDuplicate}) {
		t.Fatalf("%+v", res.SkippedRows)
	}
	if e := lastAudit(t, b); e.Detail != "3 messages, 2 skipped" {
		t.Fatalf("audit %+v", e)
	}
	again, err := b.Queue(t.Context(), opt, reqs)
	if err != nil || again.Replayed != 3 || again.Skipped.Duplicate != 2 {
		t.Fatalf("a replay skips the same rows again: %v %+v", err, again)
	}
	if c, _ := b.Store.CountOutbox(store.OutboxFilter{}); c.Queued != 3 {
		t.Fatalf("counts %+v", c)
	}
}

func TestQueueSkipSent(t *testing.T) {
	b := newTestBot(t)
	earlier, err := b.Queue(t.Context(), QueueOptions{Actor: "ops", Batch: "promo-october"}, []QueueRequest{
		{To: "5511999999999", Text: "Hi Maria"},
		{To: "5511988887777", Text: "Hi John"},
		{To: "552187654321", Text: "Hi Ana"},
	})
	if err != nil {
		t.Fatal(err)
	}
	markSent(t, b, earlier.Items[0].ID)
	if _, err := b.Store.FinishOutbox(earlier.Items[1].ID, store.OutboxResult{Status: store.OutboxFailed, Error: "x"}); err != nil {
		t.Fatal(err)
	}
	// Ana's item stays queued: it counts whatever its age.

	reqs := []QueueRequest{{To: "+5511999999999", Text: "Hi again"}, {To: "5511988887777", Text: "Hi again"}, {To: "552187654321", Text: "Hi again"}}
	opt := QueueOptions{Actor: "ops", Key: "k2", SkipSent: "48h"}
	res, err := b.Queue(t.Context(), opt, reqs)
	if err != nil || len(res.Items) != 1 || res.Items[0].To != "5511988887777" || res.Skipped.RecentlySent != 2 {
		t.Fatalf("sent and queued count, failed doesn't: %v %+v", err, res)
	}
	if got := keysOf(res.Items); !slices.Equal(got, []string{"k2#1"}) {
		t.Fatalf("keys %v", got)
	}
	again, err := b.Queue(t.Context(), opt, reqs)
	if err != nil || again.Replayed != 1 || again.Skipped.RecentlySent != 2 || len(again.Items) != 1 {
		t.Fatalf("a replay skips again and replays the rest: %v %+v", err, again)
	}

	other := QueueOptions{Actor: "john-bot", SkipSent: "48h"}
	if res, err := b.Queue(t.Context(), other, reqs[:1]); err != nil || len(res.Items) != 1 {
		t.Fatalf("another token's sends don't count without admin: %v %+v", err, res)
	}
	other.Admin = true
	if res, err := b.Queue(t.Context(), other, reqs[2:]); err != nil || len(res.Items) != 0 || res.Skipped.RecentlySent != 1 {
		t.Fatalf("an admin's skip counts every token's items: %v %+v", err, res)
	}

	if res, err := b.Queue(t.Context(), QueueOptions{Actor: "ops", SkipSent: "batch:promo-october"}, reqs); err != nil || len(res.Items) != 1 || res.Items[0].To != "5511988887777" {
		t.Fatalf("batch form: %v %+v", err, res)
	}
	if res, err := b.Queue(t.Context(), QueueOptions{Actor: "ops", SkipSent: "batch:news"}, reqs[:1]); err != nil || len(res.Items) != 1 {
		t.Fatalf("another batch doesn't count: %v %+v", err, res)
	}
}

func TestQueueSkipSentCountsMessagesWithRead(t *testing.T) {
	b := newTestBot(t)
	if _, _, err := b.Store.Save(store.Message{Chat: ana, ID: "M1", Sender: "me", FromMe: true, Type: "text", Text: "hi", Timestamp: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Store.Save(store.Message{Chat: john, ID: "M2", Sender: john, Type: "text", Text: "hi", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	reqs := []QueueRequest{{To: "5521987654321", Text: "Hi Ana"}, {To: "5511988887777", Text: "Hi John"}}
	res, err := b.Queue(t.Context(), QueueOptions{Actor: "ops", SkipSent: "30d", Read: true}, reqs)
	if err != nil || len(res.Items) != 1 || res.Items[0].To != "5511988887777" {
		t.Fatalf("a message sent from the phone counts for a reader (Ana's other 9th-digit form too): %v %+v", err, res)
	}
	if res, err := b.Queue(t.Context(), QueueOptions{Actor: "ops2", SkipSent: "48h"}, reqs[:1]); err != nil || len(res.Items) != 1 {
		t.Fatalf("without read only the token's own queue counts: %v %+v", err, res)
	}
	for _, c := range []struct {
		opt  QueueOptions
		want string
	}{
		{QueueOptions{Actor: "ops", SkipSent: "8d"}, "at most 7 days for a token without the read scope"},
		{QueueOptions{Actor: "ops", SkipSent: "91d", Read: true}, "at most 90 days"},
		{QueueOptions{Actor: "ops", SkipSent: "soon", Read: true}, "must be a duration"},
		{QueueOptions{Actor: "ops", SkipSent: "-1h", Read: true}, "must be a duration"},
		{QueueOptions{Actor: "ops", SkipSent: "batch:no/slash", Read: true}, "batch must be"},
	} {
		if _, err := b.Queue(t.Context(), c.opt, reqs); !isInputError(err) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q = %v, want %q", c.opt.SkipSent, err, c.want)
		}
	}
}

func TestQueueSkipSentConcurrent(t *testing.T) {
	b := newTestBot(t)
	reqs := []QueueRequest{{To: "5511999999999", Text: "Hi"}, {To: "5511988887777", Text: "Hi"}, {To: "552187654321", Text: "Hi"}}
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = b.Queue(t.Context(), QueueOptions{Actor: "ops", SkipSent: "1h"}, reqs)
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if c, _ := b.Store.CountOutbox(store.OutboxFilter{}); c.Queued != 3 {
		t.Fatalf("imports at once must not both pass the skip check: %+v", c)
	}
}

func TestQueueAllSkipped(t *testing.T) {
	b := newTestBot(t)
	if _, err := b.Queue(t.Context(), QueueOptions{Actor: "ops", Batch: "promo"}, []QueueRequest{{To: "5511999999999", Text: "Hi"}}); err != nil {
		t.Fatal(err)
	}
	res, err := b.Queue(t.Context(), QueueOptions{Actor: "ops", Batch: "promo", SkipSent: "1h"}, []QueueRequest{{To: "5511999999999", Text: "Hi"}})
	if err != nil || len(res.Items) != 0 || res.Batch != "promo" || res.Skipped.RecentlySent != 1 {
		t.Fatalf("a rerun of a finished campaign is not an error: %v %+v", err, res)
	}
}

func TestParseSkipSent(t *testing.T) {
	for in, want := range map[string]SkipSentRule{
		"48h":                 {Within: 48 * time.Hour},
		" 3d ":                {Within: 72 * time.Hour},
		"90m":                 {Within: 90 * time.Minute},
		"90d":                 {Within: MaxSkipSent},
		"batch:promo-october": {Batch: "promo-october"},
	} {
		if got, err := ParseSkipSent(in); err != nil || got != want {
			t.Errorf("%q = %+v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "0h", "1000000d", "batch:", "2 days"} {
		if _, err := ParseSkipSent(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
