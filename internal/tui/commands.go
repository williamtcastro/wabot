package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
)

const tuiHelp = `Moving around
  Tab / Shift+Tab     cycle focus: input → chats → messages
  Ctrl+J / Ctrl+K     next / previous chat (from anywhere)
  ↑ (empty input)     select messages
  Ctrl+D / Ctrl+U     scroll half a page down / up
  Ctrl+Y              copy the open chat's number (group ID for groups)
  PgUp / PgDn         scroll
  Esc                 cancel / close / back to the input

Chat list (focused)
  type                filter chats by name
  ↑ ↓  Enter          pick and open

Selected message
  j k  ↑ ↓           move               g  G   first / last
  r  reply           e  edit (yours, 20 min)
  d d  delete        s  save media to ~/Downloads
  1-6  react 👍 ❤️ 😂 😮 😢 🙏     0  remove reaction
  other letters      back to typing

Commands
  /to <name|number|jid>     open a chat
  /search <words>           search every chat (↑↓ pick, Enter jumps to it, Esc closes)
  /copy                     copy this chat's number (same as Ctrl+Y)
  /all                      live feed of every chat
  /contacts [search]        your address book
  /groups                   your groups
  /send <number|jid> <msg>  one-off message
  /file <path> [caption]    send a file to the open chat
  /read                     mark the open chat as read
  /pair <phone>             log in with a pairing code
  /help  /quit

Typing a command, Tab completes it (and names after /to, numbers after /send,
paths after /file), ↑ ↓ or Ctrl+J / Ctrl+K cycle the matches.`

var tuiCommands = []struct{ name, args, desc string }{
	{"/to", "<name|number>", "open a chat"},
	{"/all", "", "every chat, live"},
	{"/contacts", "[search]", "address book"},
	{"/groups", "", "your groups"},
	{"/send", "<number> <text>", "one-off message"},
	{"/file", "<path> [caption]", "send a file here"},
	{"/search", "<words>", "search all messages"},
	{"/copy", "", "copy this chat's number"},
	{"/read", "", "mark this chat read"},
	{"/pair", "<phone>", "pairing-code login"},
	{"/help", "", "keys and commands"},
	{"/quit", "", "exit"},
}

func (m *model) submit(line string) tea.Cmd {
	if line == "" {
		return nil
	}
	if strings.HasPrefix(line, "/") {
		return m.command(line)
	}
	if m.active == "" {
		return m.setNotice("no chat open: press Tab to pick one, or /to <name>", true)
	}
	chat, reply, editing := m.active, m.replyTo, m.editing
	m.replyTo, m.editing = nil, nil
	m.layout()
	m.render(true)
	if editing != nil {
		return m.do(func(ctx context.Context) (string, error) {
			_, err := m.bot.Edit(ctx, actor, chat, editing.ID, line)
			return "", err
		})
	}
	opt := bot.SendOptions{}
	if reply != nil {
		opt.ReplyTo = reply.ID
	}
	return m.do(func(ctx context.Context) (string, error) {
		_, err := m.bot.SendText(ctx, actor, chat, line, opt)
		return "", err
	})
}

func (m *model) command(line string) tea.Cmd {
	cmd, rest, _ := strings.Cut(line, " ")
	rest = strings.TrimSpace(rest)
	switch cmd {
	case "/quit", "/q":
		return tea.Quit
	case "/help":
		return func() tea.Msg { return panelMsg{"Help", tuiHelp} }
	case "/all":
		return m.open("")
	case "/to":
		return m.openByName(rest)
	case "/contacts":
		return func() tea.Msg {
			contacts, err := m.bot.Contacts(context.Background(), rest)
			if err != nil {
				return noticeMsg{err.Error(), true}
			}
			if len(contacts) == 0 {
				return noticeMsg{"no contacts found (they sync from your phone after connecting)", true}
			}
			return panelMsg{fmt.Sprintf("Contacts (%d) · /to <name> to open", len(contacts)), formatContacts(contacts, 0)}
		}
	case "/groups":
		return func() tea.Msg {
			groups, err := m.bot.Client.GetJoinedGroups(context.Background())
			if err != nil {
				return noticeMsg{err.Error(), true}
			}
			var sb strings.Builder
			for _, g := range groups {
				fmt.Fprintf(&sb, "%s  %s\n", styleDim.Render(g.JID.String()), safeLine(g.Name))
			}
			return panelMsg{fmt.Sprintf("Groups (%d) · /to <jid> to open", len(groups)), sb.String()}
		}
	case "/send":
		to, text, _ := strings.Cut(rest, " ")
		return m.do(func(ctx context.Context) (string, error) {
			_, err := m.bot.SendText(ctx, actor, to, text, bot.SendOptions{})
			return "sent to " + to, err
		})
	case "/file":
		path, caption, _ := strings.Cut(rest, " ")
		if m.active == "" {
			return m.setNotice("no chat open", true)
		}
		chat, path := m.active, expandHome(path)
		return m.do(func(ctx context.Context) (string, error) {
			data, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			_, err = m.bot.SendFile(ctx, actor, chat, data, filepath.Base(path), caption, bot.SendOptions{})
			return "", err
		})
	case "/search":
		if rest == "" {
			return m.setNotice("type what to search for, e.g. /search bom dia", true)
		}
		return func() tea.Msg {
			return searchMsg{rest, m.bot.Store.Query(store.Filter{Text: rest}, 200, false)}
		}
	case "/copy":
		return m.copyNumber()
	case "/read":
		if m.active == "" {
			return m.setNotice("no chat open", true)
		}
		chat := m.active
		return m.do(func(ctx context.Context) (string, error) {
			n, err := m.bot.MarkRead(ctx, actor, chat, nil)
			return fmt.Sprintf("marked %d messages as read", n), err
		})
	case "/pair":
		return m.do(func(ctx context.Context) (string, error) {
			code, err := m.bot.PairPhone(ctx, actor, rest)
			return "pairing code " + code + " (WhatsApp → Linked devices → Link with phone number)", err
		})
	}
	return m.setNotice("unknown command "+cmd+" (try /help)", true)
}

func (m *model) openByName(q string) tea.Cmd {
	m.openSeq++
	seq := m.openSeq
	return func() tea.Msg {
		ctx := context.Background()
		if _, err := bot.ParseRecipient(q); err == nil {
			jid, err := m.bot.Resolve(ctx, q)
			if err != nil {
				return noticeMsg{err.Error(), true}
			}
			return m.load(seq, m.bot.PhoneJID(jid).String(), 0)()
		}
		contacts, err := m.bot.Contacts(ctx, q)
		switch {
		case err != nil:
			return noticeMsg{err.Error(), true}
		case len(contacts) == 0:
			return noticeMsg{"no contact matches " + q, true}
		case len(contacts) == 1:
			return m.load(seq, contacts[0].JID, 0)()
		}
		return panelMsg{fmt.Sprintf("%d contacts match %q · be more specific", len(contacts), q), formatContacts(contacts, 30)}
	}
}

// markRead sends read receipts for incoming messages in the open chat.
// markRead marks what's on screen as read: locally right away (so badges clear even offline) and,
// when connected, with read receipts to the senders.
func (m *model) markRead(msgs []store.Message) tea.Cmd {
	var ids []string
	var last int64
	for _, x := range msgs {
		if x.Chat != m.active {
			continue
		}
		last = max(last, x.Seq)
		if !x.FromMe && x.Type != "reaction" {
			ids = append(ids, x.ID)
		}
	}
	if len(ids) > 20 {
		ids = ids[len(ids)-20:]
	}
	if last == 0 {
		return nil
	}
	chat, online := m.active, m.status.LoggedIn
	return func() tea.Msg {
		moved := m.bot.Store.MarkReadUpTo(chat, last)
		if online && len(ids) > 0 {
			_, _ = m.bot.MarkRead(context.Background(), actor, chat, ids)
		}
		if moved {
			return m.loadChats()()
		}
		return nil
	}
}

func (m *model) save(ctx context.Context, target store.Message) (string, error) {
	data, full, err := m.bot.DownloadMedia(ctx, target.Seq)
	if err != nil {
		return "", err
	}
	dir, err := os.UserHomeDir()
	if err == nil {
		dir = filepath.Join(dir, "Downloads")
	}
	if _, err := os.Stat(dir); err != nil {
		dir = "."
	}
	name := filepath.Base(safe(full.MediaFileName()))
	ext := filepath.Ext(name)
	for i := 0; i < 100; i++ {
		path := filepath.Join(dir, name)
		if i > 0 {
			path = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(name, ext), i, ext))
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = f.Write(data)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return "saved " + path, err
	}
	return "", fmt.Errorf("%s: too many files with that name in %s", name, dir)
}
