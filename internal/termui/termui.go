// Package termui holds what the local TUI and the admin panel share: sanitizing untrusted text,
// the colour palette, list navigation, debouncing, the clipboard and local files.
package termui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

var (
	ColAccent = lipgloss.Color("42")
	ColText   = lipgloss.Color("252")
	ColMuted  = lipgloss.Color("244")
	ColFaint  = lipgloss.Color("238")
	ColErr    = lipgloss.Color("203")
	ColSel    = lipgloss.Color("236")
)

// Safe strips terminal escape sequences, control characters and bidi marks from text that other
// people control (messages, names, file names, server errors), so it can't move the cursor, recolor
// the screen, set the clipboard or window title, or reorder what's around it. Newlines and tabs stay.
func Safe(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < 0x20, r >= 0x7f && r < 0xa0:
			return -1
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069, r == 0x200e, r == 0x200f:
			return -1
		}
		return r
	}, ansi.Strip(s))
}

// SafeLine is Safe for single-line fields: runs of whitespace, newlines included, become one space.
func SafeLine(s string) string {
	return strings.Join(strings.Fields(Safe(s)), " ")
}

// SafeLines is SafeLine for each string of a list.
func SafeLines(list []string) []string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = SafeLine(s)
	}
	return out
}

// IsPersonChat reports whether a JID is a person (a phone number or a LID), not a group or a list.
func IsPersonChat(jid string) bool {
	return strings.HasSuffix(jid, "@s.whatsapp.net") || strings.HasSuffix(jid, "@lid")
}

// UserOf is the user part of a JID, without the device or agent suffix.
func UserOf(jid string) string {
	user, _, _ := strings.Cut(jid, "@")
	user, _, _ = strings.Cut(user, ":")
	user, _, _ = strings.Cut(user, ".")
	return user
}

// ListMove applies a navigation key to a selection in a list of n rows: j/k and the arrows move one,
// ctrl+d/ctrl+u and page down/up move by page, g/G and home/end jump to the ends. ok is false for
// any other key.
func ListMove(k string, sel, n, page int) (int, bool) {
	switch k {
	case "j", "down":
		sel++
	case "k", "up":
		sel--
	case "ctrl+d", "pgdown":
		sel += max(page, 1)
	case "ctrl+u", "pgup":
		sel -= max(page, 1)
	case "g", "home":
		sel = 0
	case "G", "end":
		sel = n - 1
	default:
		return sel, false
	}
	return max(min(sel, n-1), 0), true
}

// Debounce turns a burst of triggers into one message, delivered after a delay. The receiver of
// that message calls Done.
type Debounce struct{ pending bool }

func (d *Debounce) Schedule(delay time.Duration, msg tea.Msg) tea.Cmd {
	if d.pending {
		return nil
	}
	d.pending = true
	return tea.Tick(delay, func(time.Time) tea.Msg { return msg })
}

func (d *Debounce) Done() { d.pending = false }

// Pending reports whether a message is scheduled.
func (d *Debounce) Pending() bool { return d.pending }

// CopyToClipboard uses the system clipboard tool, or the terminal's OSC 52 clipboard (works over SSH).
func CopyToClipboard(text string) error {
	for _, tool := range [][]string{{"pbcopy"}, {"wl-copy"}, {"xclip", "-selection", "clipboard"}, {"xsel", "--clipboard", "--input"}} {
		if _, err := exec.LookPath(tool[0]); err != nil {
			continue
		}
		cmd := exec.Command(tool[0], tool[1:]...)
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Run(); err == nil {
			return nil
		}
	}
	termenv.NewOutput(os.Stdout).Copy(text)
	return nil
}

// ExpandHome turns a leading "~/" into the home directory.
func ExpandHome(path string) string {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return path
}

// WriteNew writes data to a file that must not exist yet (mode 0600). An existing file is an error
// matching os.ErrExist; a failed write removes the partial file.
func WriteNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}
