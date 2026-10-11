package tui

import (
	"errors"
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/muesli/termenv"
)

// copyTarget is what Ctrl+Y copies for a chat: "+<number>" for people, the JID for groups.
func copyTarget(jid string) (string, error) {
	switch {
	case strings.HasSuffix(jid, "@s.whatsapp.net"):
		return "+" + userOf(jid), nil
	case strings.HasSuffix(jid, "@g.us"):
		return jid, nil
	}
	return "", errors.New("WhatsApp hasn't shared this person's number yet")
}

func (m *model) copyNumber() tea.Cmd {
	if m.active == "" {
		return m.setNotice("no chat open", true)
	}
	text, err := copyTarget(m.active)
	if err != nil {
		return m.setNotice(err.Error(), true)
	}
	return func() tea.Msg {
		if err := copyToClipboard(text); err != nil {
			return noticeMsg{"copy failed: " + err.Error(), true}
		}
		return noticeMsg{"copied " + text, false}
	}
}

// copyToClipboard uses the system clipboard tool, or the terminal's OSC 52 clipboard (works over SSH).
func copyToClipboard(text string) error {
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
