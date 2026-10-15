package admin

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

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
