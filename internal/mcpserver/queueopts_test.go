package mcpserver

import (
	"strings"
	"testing"

	"github.com/williamtcastro/wame/internal/auth"
	"github.com/williamtcastro/wame/internal/bot"
)

func TestScheduleTagAndTemplate(t *testing.T) {
	b := newBot(t)
	for to, name := range map[string]string{"5511999999999": "Maria", "5511988887777": "John"} {
		if _, err := b.PatchContact(t.Context(), "ops", to, bot.ContactInput{Name: &name, AddTags: []string{"clients"}}); err != nil {
			t.Fatal(err)
		}
	}
	sendOnly := connect(t, b, tokenPrincipal(t, b, "send-bot", []auth.Scope{auth.Send}, 0))
	if _, text, isErr := call(t, sendOnly, "schedule_message", map[string]any{"to": "tag:clients", "text": "oi"}); !isErr || !strings.Contains(text, "read scope") {
		t.Fatalf("a tag without the read scope = %q", text)
	}
	cs := connect(t, b, tokenPrincipal(t, b, "promo-bot", []auth.Scope{auth.Read, auth.Send}, 0))
	out, text, isErr := call(t, cs, "schedule_message", map[string]any{"to": "tag:clients", "text": "Oi {first_name}!", "template": true, "batch": "promo"})
	if isErr || out["queued"] != float64(2) || out["to"] != "tag:clients" || out["id"] != nil {
		t.Fatalf("%v %q", out, text)
	}
	list, _, _ := call(t, cs, "list_scheduled", map[string]any{"batch": "promo"})
	var texts []string
	for _, it := range list["items"].([]any) {
		texts = append(texts, it.(map[string]any)["untrusted"].(map[string]any)["text"].(string))
	}
	if strings.Join(texts, "|") != "Oi Maria!|Oi John!" {
		t.Fatalf("%v", texts)
	}

	restricted := connect(t, b, restrictedPrincipal(t, b, "maria-bot", maria))
	if _, text, isErr := call(t, restricted, "schedule_message", map[string]any{"to": "tag:clients", "text": "oi"}); !isErr || !strings.Contains(text, "read scope") {
		t.Fatalf("a restricted send-only token = %q", text)
	}
}
