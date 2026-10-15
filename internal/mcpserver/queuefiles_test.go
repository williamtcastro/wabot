package mcpserver

import (
	"strings"
	"testing"

	"github.com/williamtcastro/wame/internal/auth"
)

func TestScheduleFile(t *testing.T) {
	b := newBot(t)
	mariaBot := connect(t, b, tokenPrincipal(t, b, "maria-bot", []auth.Scope{auth.Send}, 0))
	pdf := "%PDF-1.4\n% menu\n%%EOF\n"
	mine, _, err := b.AddQueueFile("maria-bot", "", "Cardápio.pdf", strings.NewReader(pdf), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	theirs, _, err := b.AddQueueFile("john-bot", "", "john.pdf", strings.NewReader(pdf+" "), 1<<20)
	if err != nil {
		t.Fatal(err)
	}

	if _, text, isErr := call(t, mariaBot, "schedule_message", map[string]any{"to": maria, "file": theirs.ID}); !isErr || !strings.Contains(text, "not found") {
		t.Fatalf("someone else's file = %q", text)
	}
	out, text, isErr := call(t, mariaBot, "schedule_message", map[string]any{"to": maria, "file": mine.ID, "send_at": "1h"})
	if isErr {
		t.Fatalf("own file without a caption: %q", text)
	}
	if _, text, isErr := call(t, mariaBot, "schedule_message", map[string]any{"to": maria}); !isErr || !strings.Contains(text, "text is empty") {
		t.Fatalf("neither text nor file = %q", text)
	}

	list, _, _ := call(t, mariaBot, "list_scheduled", nil)
	items := list["items"].([]any)
	it := items[0].(map[string]any)
	if len(items) != 1 || it["kind"] != "document" || it["file_size"] != float64(mine.Size) || it["id"] != out["id"] {
		t.Fatalf("%v", items)
	}
	if _, raw, _ := call(t, mariaBot, "list_scheduled", nil); strings.Contains(raw, "Cardápio") || !strings.Contains(raw, "document") {
		t.Fatal("file names never reach MCP")
	}
}
