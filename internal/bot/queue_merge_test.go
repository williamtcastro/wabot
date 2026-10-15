package bot

import (
	"errors"
	"strings"
	"testing"

	"github.com/williamtcastro/wame/internal/store"
)

func TestQueueFilesWithTagsAndTemplates(t *testing.T) {
	b := newTestBot(t)
	tagged(t, b, "5511999999999", "clients")
	tagged(t, b, "5511988887777", "clients")
	menu := addFile(t, b, "ops", "menu.pdf", pdfBytes(100))
	flyer := addFile(t, b, "ops", "flyer.pdf", pdfBytes(200))
	opt := QueueOptions{Actor: "ops", Batch: "promo", Key: "k", Read: true, Template: true}

	res, err := b.Queue(t.Context(), opt, []QueueRequest{{To: "tag:clients", Text: "Hi {name|there}", File: menu.ID}})
	if err != nil || len(res.Items) != 2 {
		t.Fatalf("%v %+v", err, res)
	}
	for _, it := range res.Items {
		if it.FileID != menu.ID || it.FileKind != "document" || it.FileSize != menu.Size {
			t.Fatalf("tag member without the file: %+v", it)
		}
	}

	// The same key with another file is another request, also when replays compare source_hash.
	_, err = b.Queue(t.Context(), opt, []QueueRequest{{To: "tag:clients", Text: "Hi {name|there}", File: flyer.ID}})
	if !errors.Is(err, ErrKeyReused) {
		t.Fatalf("other file under the same key: %v", err)
	}
	if res, err = b.Queue(t.Context(), opt, []QueueRequest{{To: "tag:clients", Text: "Hi {name|there}", File: menu.ID}}); err != nil || res.Replayed != 2 {
		t.Fatalf("replay: %v %+v", err, res)
	}
}

func TestQueueDedupeKeepsDifferentFiles(t *testing.T) {
	b := newTestBot(t)
	menu := addFile(t, b, "ops", "menu.pdf", pdfBytes(100))
	flyer := addFile(t, b, "ops", "flyer.pdf", pdfBytes(200))
	res, err := b.Queue(t.Context(), QueueOptions{Actor: "ops", Dedupe: true}, []QueueRequest{
		{To: "5511999999999", Text: "Our offers", File: menu.ID},
		{To: "5511999999999", Text: "Our offers", File: flyer.ID},
		{To: "5511999999999", Text: "Our offers", File: flyer.ID},
		{To: "5511999999999", Text: "Our offers"},
	})
	if err != nil || len(res.Items) != 3 || res.Skipped.Duplicate != 1 {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestQueueTagFileErrorNamesTheRow(t *testing.T) {
	b := newTestBot(t)
	tagged(t, b, "5511999999999", "clients")
	other := addFile(t, b, "someone-else", "menu.pdf", pdfBytes(100))
	_, err := b.Queue(t.Context(), QueueOptions{Actor: "ops", Read: true}, []QueueRequest{
		{To: "5511988887777", Text: "Hi"}, {To: "tag:clients", Text: "Hi", File: other.ID}})
	if !errors.Is(err, store.ErrNotFound) || !strings.Contains(err.Error(), "messages[1] (member 1)") {
		t.Fatalf("%v", err)
	}
}

func TestQueueTemplateLiteralBracesWithoutPlaceholders(t *testing.T) {
	b := newTestBot(t)
	menu := addFile(t, b, "ops", "menu.pdf", pdfBytes(100))
	res, err := b.Queue(t.Context(), QueueOptions{Actor: "ops", Read: true, Template: true}, []QueueRequest{
		{To: "5511999999999", Text: "Promo {{50% off}}"},
		{To: "5511988887777", Text: "{{x}}", File: menu.ID},
	})
	if err != nil || len(res.Items) != 2 {
		t.Fatalf("%v %+v", err, res)
	}
	if got, _ := b.Store.OutboxItem(res.Items[0].ID); got.Text != "Promo {50% off}" {
		t.Fatalf("text %q", got.Text)
	}
	if got, _ := b.Store.OutboxItem(res.Items[1].ID); got.Text != "{x}" {
		t.Fatalf("caption %q", got.Text)
	}
}
