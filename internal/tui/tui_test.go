package tui

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/williamtcastro/wabot/internal/bot"
	"github.com/williamtcastro/wabot/internal/store"
)

const (
	maria = "5511999999999@s.whatsapp.net"
	john  = "5511988887777@s.whatsapp.net"
	ana   = "552187654321@s.whatsapp.net"
)

func newTestModel(t *testing.T, msgs ...store.Message) *model {
	t.Helper()
	b, err := bot.New(context.Background(), filepath.Join(t.TempDir(), "wabot.db"), waLog.Noop, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range msgs {
		if _, ok, err := b.Store.Save(x); !ok || err != nil {
			t.Fatalf("save %s: %v", x.ID, err)
		}
	}
	m := newModel(b, make(chan any))
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 24})
	run(m, m.loadChats())
	run(m, m.loadFeed())
	return m
}

// run executes cmd synchronously and feeds its message back, like the Bubble Tea loop would.
func run(m *model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	if msg := cmd(); msg != nil {
		if _, isBatch := msg.(tea.BatchMsg); !isBatch {
			m.Update(msg)
		}
	}
}

func press(m *model, keys ...string) tea.Cmd {
	var cmd tea.Cmd
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "up":
			msg = tea.KeyMsg{Type: tea.KeyUp}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		case "ctrl+j":
			msg = tea.KeyMsg{Type: tea.KeyCtrlJ}
		case "ctrl+k":
			msg = tea.KeyMsg{Type: tea.KeyCtrlK}
		case "ctrl+d":
			msg = tea.KeyMsg{Type: tea.KeyCtrlD}
		case "ctrl+u":
			msg = tea.KeyMsg{Type: tea.KeyCtrlU}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		if msg.String() != k {
			panic("bad key " + k + " → " + msg.String())
		}
		_, cmd = m.Update(msg)
	}
	return cmd
}

func typeText(m *model, s string) {
	for _, r := range s {
		press(m, string(r))
	}
}

func chatMessages() []store.Message {
	now := time.Now().Add(-time.Hour)
	var out []store.Message
	add := func(chat, sender, id, text string, fromMe bool) {
		out = append(out, store.Message{ID: id, Chat: chat, Sender: sender, FromMe: fromMe, PushName: map[string]string{maria: "Maria", john: "John", ana: "Ana"}[sender],
			Type: "text", Text: text, Timestamp: now.Add(time.Duration(len(out)) * time.Minute)})
	}
	add(ana, ana, "A1", "oi, tudo bem?", false)
	add(john, john, "J1", "hello", false)
	for i := range 30 {
		add(maria, maria, fmt.Sprint("M", i), fmt.Sprintf("message %d from Maria", i), false)
	}
	add(maria, john, "M-me", "see you", true)
	return out
}

func checkScreen(t *testing.T, m *model, label string) {
	t.Helper()
	lines := strings.Split(m.View(), "\n")
	if len(lines) != 24 {
		t.Fatalf("%s: %d lines, want 24", label, len(lines))
	}
	for i, l := range lines {
		if w := lipgloss.Width(l); w > 110 {
			t.Fatalf("%s: line %d is %d wide: %q", label, i, w, l)
		}
	}
}

func TestRenderHeight(t *testing.T) {
	m := newTestModel(t, chatMessages()...)
	checkScreen(t, m, "feed")
	typeText(m, "/")
	if m.contextBar() == "" {
		t.Fatal("typing / should show the hint bar")
	}
	checkScreen(t, m, "hint bar")
	press(m, "esc")
	m.input.SetValue("")
	run(m, m.suggest())
	run(m, m.open(maria))
	checkScreen(t, m, "chat")
	press(m, "up", "r")
	if m.replyTo == nil {
		t.Fatal("r should start a reply")
	}
	checkScreen(t, m, "reply bar")
	m.Update(panelMsg{"Help", tuiHelp})
	checkScreen(t, m, "help panel")
}

func TestAutocomplete(t *testing.T) {
	m := newTestModel(t, chatMessages()...)
	m.Update(contactNamesMsg{"Ana", "John", "Maria"})
	typeText(m, "/t")
	press(m, "tab")
	if v := m.input.Value(); v != "/to " {
		t.Fatalf("/t + tab = %q, want %q", v, "/to ")
	}
	typeText(m, "M")
	if got := m.input.MatchedSuggestions(); len(got) != 1 || got[0] != "/to Maria" {
		t.Fatalf("/to M suggestions = %q", got)
	}
	press(m, "tab")
	if v := m.input.Value(); v != "/to Maria" {
		t.Fatalf("accepted = %q", v)
	}

	m.input.SetValue("")
	run(m, m.suggest())
	typeText(m, "/")
	all := m.input.MatchedSuggestions()
	if len(all) != len(tuiCommands) || all[0] != "/to " {
		t.Fatalf("/ suggestions = %q", all)
	}
	seq := m.openSeq
	press(m, "ctrl+j")
	press(m, "ctrl+j")
	if i := m.input.CurrentSuggestionIndex(); i != 2 {
		t.Fatalf("after two ctrl+j index = %d, want 2", i)
	}
	press(m, "ctrl+k")
	if m.input.CurrentSuggestion() != all[1] {
		t.Fatalf("ctrl+k = %q, want %q", m.input.CurrentSuggestion(), all[1])
	}
	press(m, "down")
	if m.input.CurrentSuggestionIndex() != 2 {
		t.Fatal("↓ should cycle too")
	}
	if m.openSeq != seq || m.input.Value() != "/" {
		t.Fatal("cycling suggestions must not switch chats or edit the input")
	}
}

func TestReactionDividerHiddenWhenRemoved(t *testing.T) {
	now := time.Now()
	text := store.Message{ID: "M1", Chat: maria, Sender: maria, Type: "text", Text: "oi", Timestamp: now}
	like := store.Message{ID: "R1", Chat: maria, Sender: john, FromMe: true, Type: "reaction", Text: "👍", ReplyTo: "M1", Timestamp: now}
	unlike := store.Message{ID: "R2", Chat: maria, Sender: john, FromMe: true, Type: "reaction", Text: "", ReplyTo: "M1", Timestamp: now}

	m := newTestModel(t)
	m.active, m.msgs = maria, []store.Message{text, like}
	if out := m.renderChat(80); !strings.Contains(out, "│ 👍") {
		t.Fatalf("reaction not shown:\n%s", out)
	}
	m.msgs = []store.Message{text, like, unlike}
	if out := m.renderChat(80); strings.Contains(out, "│") || strings.Contains(out, "👍") {
		t.Fatalf("removed reaction still shown:\n%s", out)
	}
}

func TestCtrlJKSwitchChats(t *testing.T) {
	m := newTestModel(t, chatMessages()...)
	var order []string
	for _, c := range m.chats {
		order = append(order, c.JID)
	}
	if len(order) != 3 || order[0] != maria {
		t.Fatalf("chats = %v", order)
	}
	run(m, press(m, "ctrl+j"))
	if m.active != order[0] {
		t.Fatalf("ctrl+j from All activity opened %q, want %q", m.active, order[0])
	}
	run(m, press(m, "ctrl+j"))
	if m.active != order[1] {
		t.Fatalf("second ctrl+j opened %q, want %q", m.active, order[1])
	}
	run(m, press(m, "ctrl+k"))
	if m.active != order[0] {
		t.Fatalf("ctrl+k opened %q, want %q", m.active, order[0])
	}
	run(m, press(m, "ctrl+k"))
	if m.active != "" {
		t.Fatalf("ctrl+k at the top should open All activity, got %q", m.active)
	}

	press(m, "tab")
	if m.focus != focusChats {
		t.Fatal("tab should focus the chat list")
	}
	press(m, "down", "down", "down")
	run(m, press(m, "ctrl+k"))
	if m.active != order[1] || m.cursor != 2 {
		t.Fatalf("ctrl+k in the chat list steps from the cursor: active %q cursor %d", m.active, m.cursor)
	}
	press(m, "up")
	if m.cursor != 1 {
		t.Fatalf("↑ in the chat list: cursor %d", m.cursor)
	}
	typeText(m, "jo")
	if m.filter != "jo" || len(m.filteredChats()) != 1 {
		t.Fatalf("letters should filter, filter %q", m.filter)
	}
}

func TestMessageSelectVimKeys(t *testing.T) {
	m := newTestModel(t, chatMessages()...)
	run(m, m.open(maria))
	last := len(m.visible) - 1
	press(m, "up")
	if m.focus != focusMessages || m.sel != last {
		t.Fatalf("↑ on empty input: focus %d sel %d", m.focus, m.sel)
	}
	press(m, "g")
	if m.sel != 0 {
		t.Fatalf("g: sel %d", m.sel)
	}
	press(m, "k")
	if m.sel != 0 {
		t.Fatalf("k at the top: sel %d", m.sel)
	}
	press(m, "j", "j")
	if m.sel != 2 {
		t.Fatalf("j j: sel %d", m.sel)
	}
	press(m, "G")
	if m.sel != last {
		t.Fatalf("G: sel %d, want %d", m.sel, last)
	}
	press(m, "j")
	if m.focus != focusInput {
		t.Fatal("j past the last message goes back to typing")
	}
	press(m, "up", "x")
	if m.focus != focusInput || m.input.Value() != "x" {
		t.Fatalf("other letters jump back to typing: focus %d value %q", m.focus, m.input.Value())
	}
}

func TestHalfPageScroll(t *testing.T) {
	m := newTestModel(t, chatMessages()...)
	run(m, m.open(maria))
	typeText(m, "draft")
	bottom := m.vp.YOffset
	if bottom == 0 {
		t.Fatal("expected a scrollable chat")
	}
	press(m, "ctrl+u")
	if m.vp.YOffset != bottom-m.vp.Height/2 {
		t.Fatalf("ctrl+u: offset %d, want %d", m.vp.YOffset, bottom-m.vp.Height/2)
	}
	press(m, "ctrl+d")
	if m.vp.YOffset != bottom {
		t.Fatalf("ctrl+d: offset %d, want %d", m.vp.YOffset, bottom)
	}
	press(m, "ctrl+k")
	if m.input.Value() != "draft" {
		t.Fatalf("textinput's own ctrl+u/ctrl+k must not edit the draft: %q", m.input.Value())
	}
}

func TestSafe(t *testing.T) {
	for in, want := range map[string]string{
		"\x1b]0;pwned\x07hello":       "hello",
		"\x1b[31mred\x1b[0m":          "red",
		"Maria‮gnp.exe":               "Mariagnp.exe",
		"line1\nline2\tx\x00\x7f\x9b": "line1\nline2\tx",
	} {
		if got := safe(in); got != want {
			t.Errorf("safe(%q) = %q, want %q", in, got, want)
		}
	}
	if got := safeLine(" Ana\n\tde  Souza "); got != "Ana de Souza" {
		t.Errorf("safeLine = %q", got)
	}
	x := safeMessage(store.Message{Text: "\x1b[2Jhi", PushName: "Jo\nhn", Media: &store.Media{FileName: "a\x1b]8;;x\x07.pdf"}})
	if x.Text != "hi" || x.PushName != "Jo hn" || x.Media.FileName != "a.pdf" {
		t.Errorf("safeMessage = %+v %+v", x, x.Media)
	}
}
