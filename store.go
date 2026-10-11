package main

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

type Chat struct {
	JID        string    `json:"jid"`
	Name       string    `json:"name"`
	IsGroup    bool      `json:"is_group"`
	LastText   string    `json:"last_text"`
	LastType   string    `json:"last_type"`
	LastFromMe bool      `json:"last_from_me"`
	LastAt     time.Time `json:"last_at"`
}

// ChatsChanged is published after a history sync bulk-imports messages.
type ChatsChanged struct{}

func migrate(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS wabot_messages (
	chat      TEXT NOT NULL,
	id        TEXT NOT NULL,
	sender    TEXT NOT NULL,
	push_name TEXT NOT NULL DEFAULT '',
	from_me   INTEGER NOT NULL,
	is_group  INTEGER NOT NULL,
	type      TEXT NOT NULL,
	text      TEXT NOT NULL,
	ts        INTEGER NOT NULL,
	PRIMARY KEY (chat, id)
);
CREATE INDEX IF NOT EXISTS wabot_messages_ts ON wabot_messages (chat, ts);`)
	return err
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func saveMessage(db execer, m Message) error {
	_, err := db.Exec(`INSERT OR IGNORE INTO wabot_messages
		(chat, id, sender, push_name, from_me, is_group, type, text, ts) VALUES (?,?,?,?,?,?,?,?,?)`,
		m.Chat, m.ID, m.Sender, m.PushName, m.FromMe, m.IsGroup, m.Type, m.Text, m.Timestamp.UnixMilli())
	return err
}

// relinkLIDs moves messages stored under a hidden @lid ID onto the phone-number chat once
// whatsmeow has learned which number that ID belongs to.
func (b *Bot) relinkLIDs() {
	for _, col := range []string{"chat", "sender"} {
		_, _ = b.db.Exec(`UPDATE OR IGNORE wabot_messages
			SET ` + col + ` = (SELECT pn FROM whatsmeow_lid_map WHERE lid = substr(` + col + `, 1, length(` + col + `) - 4)) || '@s.whatsapp.net'
			WHERE ` + col + ` LIKE '%@lid'
			AND substr(` + col + `, 1, length(` + col + `) - 4) IN (SELECT lid FROM whatsmeow_lid_map)`)
	}
}

// Messages returns the newest messages first; chat may be a chat or sender JID, or "" for all.
func (b *Bot) Messages(chat string, limit int) []Message {
	b.relinkLIDs()
	rows, err := b.db.Query(`SELECT chat, id, sender, push_name, from_me, is_group, type, text, ts
		FROM wabot_messages WHERE ? = '' OR chat = ? OR sender = ? ORDER BY ts DESC LIMIT ?`,
		chat, chat, chat, limit)
	if err != nil {
		return []Message{}
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		var ts int64
		if rows.Scan(&m.Chat, &m.ID, &m.Sender, &m.PushName, &m.FromMe, &m.IsGroup, &m.Type, &m.Text, &ts) == nil {
			m.Timestamp = time.UnixMilli(ts)
			out = append(out, m)
		}
	}
	return out
}

// Chats lists conversations by most recent activity.
func (b *Bot) Chats(ctx context.Context, limit int) ([]Chat, error) {
	b.relinkLIDs()
	rows, err := b.db.QueryContext(ctx, `
SELECT chat, is_group, type, text, from_me, MAX(ts),
	(SELECT push_name FROM wabot_messages p WHERE p.chat = m.chat AND p.from_me = 0 AND p.push_name <> '' AND p.is_group = 0
	 ORDER BY ts DESC LIMIT 1)
FROM wabot_messages m GROUP BY chat ORDER BY MAX(ts) DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Chat{}
	for rows.Next() {
		var c Chat
		var ts int64
		var push sql.NullString
		if err := rows.Scan(&c.JID, &c.IsGroup, &c.LastType, &c.LastText, &c.LastFromMe, &ts, &push); err != nil {
			return nil, err
		}
		c.LastAt = time.UnixMilli(ts)
		c.Name = b.chatName(ctx, c.JID, push.String)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (b *Bot) chatName(ctx context.Context, chat, pushName string) string {
	jid, err := types.ParseJID(chat)
	if err != nil {
		return chat
	}
	if jid.Server == types.GroupServer {
		if name := b.groupName(ctx, chat); name != "" {
			return name
		}
		return "group " + jid.User
	}
	if own := b.Client.Store.ID; own != nil && own.User == jid.User {
		return "You"
	}
	if b.Client.Store.Contacts != nil {
		if c, err := b.Client.Store.Contacts.GetContact(ctx, jid); err == nil {
			if name := firstNonEmpty(c.FullName, c.FirstName, c.BusinessName, c.PushName); name != "" {
				return name
			}
		}
	}
	if pushName != "" {
		return "~" + pushName
	}
	if jid.Server == types.DefaultUserServer {
		return "+" + jid.User
	}
	return chat
}

func (b *Bot) groupName(ctx context.Context, chat string) string {
	b.mu.RLock()
	name, ok := b.groups[chat]
	loaded := b.groups != nil
	b.mu.RUnlock()
	if ok || loaded || !b.Client.IsLoggedIn() {
		return name
	}
	groups, err := b.Client.GetJoinedGroups(ctx)
	if err != nil {
		return ""
	}
	names := make(map[string]string, len(groups))
	for _, g := range groups {
		names[g.JID.String()] = g.Name
	}
	b.mu.Lock()
	b.groups = names
	b.mu.Unlock()
	return names[chat]
}

// importHistory stores the recent conversations WhatsApp sends right after a fresh login.
func (b *Bot) importHistory(h *events.HistorySync) {
	tx, err := b.db.Begin()
	if err != nil {
		return
	}
	for _, conv := range h.Data.GetConversations() {
		chat, err := types.ParseJID(conv.GetID())
		if err != nil {
			continue
		}
		for _, hm := range conv.GetMessages() {
			evt, err := b.Client.ParseWebMessage(chat, hm.GetMessage())
			if err != nil {
				continue
			}
			if m := b.toMessage(evt); m.Type != "other" {
				_ = saveMessage(tx, m)
			}
		}
	}
	if tx.Commit() == nil {
		b.publish(ChatsChanged{})
	}
}

func shortText(m Message) string {
	t := strings.ReplaceAll(m.Text, "\n", " ")
	if m.Type != "text" {
		t = "[" + m.Type + "] " + t
	}
	return strings.TrimSpace(t)
}
