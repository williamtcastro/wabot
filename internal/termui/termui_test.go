package termui

import (
	"testing"
	"time"
)

func TestSafe(t *testing.T) {
	for in, want := range map[string]string{
		"\x1b]0;pwned\x07hello":       "hello",
		"\x1b[31mred\x1b[0m":          "red",
		"a\x1b[2Jb‮c\nd\x07":          "abc\nd",
		"Maria‏⁦gnp.exe":              "Mariagnp.exe",
		"line1\nline2\tx\x00\x7f\x9b": "line1\nline2\tx",
	} {
		if got := Safe(in); got != want {
			t.Errorf("Safe(%q) = %q, want %q", in, got, want)
		}
	}
	if got := SafeLine(" Ana\n\tde  \x1b[1mSouza "); got != "Ana de Souza" {
		t.Errorf("SafeLine = %q", got)
	}
}

func TestUserOf(t *testing.T) {
	for jid, want := range map[string]string{
		"5511999999999@s.whatsapp.net":     "5511999999999",
		"5511999999999:12@s.whatsapp.net":  "5511999999999",
		"5511999999999.0:3@s.whatsapp.net": "5511999999999",
		"120363000000000000@g.us":          "120363000000000000",
		"552187654321":                     "552187654321",
	} {
		if got := UserOf(jid); got != want {
			t.Errorf("UserOf(%q) = %q, want %q", jid, got, want)
		}
	}
}

func TestListMove(t *testing.T) {
	for _, c := range []struct {
		k            string
		sel, n, want int
		ok           bool
	}{
		{"j", 0, 5, 1, true},
		{"down", 4, 5, 4, true},
		{"k", 0, 5, 0, true},
		{"ctrl+d", 1, 50, 11, true},
		{"pgup", 3, 50, 0, true},
		{"G", 0, 5, 4, true},
		{"end", 0, 0, 0, true},
		{"home", 3, 5, 0, true},
		{"x", 3, 5, 3, false},
	} {
		if got, ok := ListMove(c.k, c.sel, c.n, 10); got != c.want || ok != c.ok {
			t.Errorf("ListMove(%q, %d, %d) = %d, %v; want %d, %v", c.k, c.sel, c.n, got, ok, c.want, c.ok)
		}
	}
}

func TestDebounce(t *testing.T) {
	var d Debounce
	if d.Schedule(time.Millisecond, "x") == nil || d.Schedule(time.Millisecond, "x") != nil {
		t.Fatal("a second trigger while one is pending must coalesce")
	}
	d.Done()
	if d.Schedule(time.Millisecond, "x") == nil {
		t.Fatal("after Done a trigger schedules again")
	}
}
