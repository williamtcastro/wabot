package tui

import (
	"errors"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/williamtcastro/wame/internal/termui"
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
		if err := CopyToClipboard(text); err != nil {
			return noticeMsg{"copy failed: " + err.Error(), true}
		}
		return noticeMsg{"copied " + text, false}
	}
}

// CopyToClipboard copies text to the system or terminal clipboard (see termui.CopyToClipboard).
func CopyToClipboard(text string) error { return termui.CopyToClipboard(text) }
