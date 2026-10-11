package main

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

type Message struct {
	Seq       int64     `json:"seq"`
	ID        string    `json:"id"`
	Chat      string    `json:"chat"`
	Sender    string    `json:"sender"`
	PushName  string    `json:"push_name"`
	FromMe    bool      `json:"from_me"`
	IsGroup   bool      `json:"is_group"`
	Type      string    `json:"type"`
	Text      string    `json:"text"`
	ReplyTo   string    `json:"reply_to,omitempty"`
	Media     *Media    `json:"media,omitempty"`
	Edited    bool      `json:"edited,omitempty"`
	Deleted   bool      `json:"deleted,omitempty"`
	Timestamp time.Time `json:"timestamp"`

	raw       []byte
	rawChat   string
	rawSender string
}

type Media struct {
	Mimetype string `json:"mimetype"`
	FileName string `json:"file_name,omitempty"`
	Size     uint64 `json:"size"`
	Path     string `json:"path"`
}

// MessageUpdate is published when a stored message is edited or deleted.
type MessageUpdate struct{ Message }

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

var errNotFound = errors.New("message not found")

const messagesTable = `
CREATE TABLE IF NOT EXISTS wabot_messages (
	seq        INTEGER PRIMARY KEY AUTOINCREMENT,
	chat       TEXT NOT NULL,
	id         TEXT NOT NULL,
	sender     TEXT NOT NULL,
	push_name  TEXT NOT NULL DEFAULT '',
	from_me    INTEGER NOT NULL,
	is_group   INTEGER NOT NULL,
	type       TEXT NOT NULL,
	text       TEXT NOT NULL,
	ts         INTEGER NOT NULL,
	reply_to   TEXT NOT NULL DEFAULT '',
	mimetype   TEXT NOT NULL DEFAULT '',
	file_name  TEXT NOT NULL DEFAULT '',
	file_size  INTEGER NOT NULL DEFAULT 0,
	edited     INTEGER NOT NULL DEFAULT 0,
	deleted    INTEGER NOT NULL DEFAULT 0,
	raw        BLOB,
	raw_chat   TEXT NOT NULL DEFAULT '',
	raw_sender TEXT NOT NULL DEFAULT '',
	UNIQUE (chat, id)
);
CREATE INDEX IF NOT EXISTS wabot_messages_ts ON wabot_messages (chat, ts);`

// migrate creates wabot_messages, upgrading the first schema (implicit rowid, fewer columns) in place.
// seq is AUTOINCREMENT so a number is never reused or renumbered, even after deletes or VACUUM.
func migrate(ctx context.Context, db *sql.DB) error {
	cols := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info('wabot_messages')`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil {
			cols[name] = true
		}
	}
	rows.Close()
	if len(cols) == 0 || cols["seq"] {
		if _, err := db.ExecContext(ctx, messagesTable); err != nil {
			return err
		}
		return addMissingColumns(ctx, db, cols)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	copied := []string{"chat", "id", "sender", "push_name", "from_me", "is_group", "type", "text", "ts"}
	for _, c := range []string{"reply_to", "mimetype", "file_name", "file_size", "edited", "deleted", "raw", "raw_chat", "raw_sender"} {
		if cols[c] {
			copied = append(copied, c)
		}
	}
	list := strings.Join(copied, ", ")
	for _, stmt := range []string{
		`ALTER TABLE wabot_messages RENAME TO wabot_messages_v1`,
		`DROP INDEX IF EXISTS wabot_messages_ts`,
		messagesTable,
		`INSERT INTO wabot_messages (seq, ` + list + `) SELECT rowid, ` + list + ` FROM wabot_messages_v1 ORDER BY rowid`,
		`DROP TABLE wabot_messages_v1`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("upgrade wabot_messages: %w", err)
		}
	}
	return tx.Commit()
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// addMissingColumns adds columns that messagesTable gained after a database was created.
func addMissingColumns(ctx context.Context, db *sql.DB, have map[string]bool) error {
	if len(have) == 0 {
		return nil
	}
	for _, line := range strings.Split(messagesTable, "\n") {
		f := strings.Fields(strings.TrimSuffix(strings.TrimSpace(line), ","))
		if len(f) < 2 || have[f[0]] || f[0] == "CREATE" || f[0] == "UNIQUE" || f[0] == "seq" || f[0] == ");" {
			continue
		}
		if _, err := db.ExecContext(ctx, `ALTER TABLE wabot_messages ADD COLUMN `+strings.Join(f, " ")); err != nil {
			return err
		}
	}
	return nil
}

// saveMessage inserts m unless (chat, id) already exists and returns its seq.
func saveMessage(db execer, m Message) (seq int64, inserted bool, err error) {
	var mime, name string
	var size uint64
	if m.Media != nil {
		mime, name, size = m.Media.Mimetype, m.Media.FileName, m.Media.Size
	}
	res, err := db.Exec(`INSERT OR IGNORE INTO wabot_messages
		(chat, id, sender, push_name, from_me, is_group, type, text, ts, reply_to, mimetype, file_name, file_size, edited, raw, raw_chat, raw_sender)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.Chat, m.ID, m.Sender, m.PushName, m.FromMe, m.IsGroup, m.Type, m.Text, m.Timestamp.UnixMilli(),
		m.ReplyTo, mime, name, int64(size), m.Edited, m.raw, m.rawChat, m.rawSender)
	if err != nil {
		return 0, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, false, nil
	}
	seq, err = res.LastInsertId()
	return seq, err == nil, err
}

const msgCols = `seq, chat, id, sender, push_name, from_me, is_group, type, text, ts,
	reply_to, mimetype, file_name, file_size, edited, deleted, raw, raw_chat, raw_sender`

type scanner interface{ Scan(dest ...any) error }

func scanMessage(r scanner) (Message, error) {
	var m Message
	var ts, size int64
	var mime, name string
	err := r.Scan(&m.Seq, &m.Chat, &m.ID, &m.Sender, &m.PushName, &m.FromMe, &m.IsGroup, &m.Type, &m.Text, &ts,
		&m.ReplyTo, &mime, &name, &size, &m.Edited, &m.Deleted, &m.raw, &m.rawChat, &m.rawSender)
	if err != nil {
		return m, err
	}
	m.Timestamp = time.UnixMilli(ts)
	if mime != "" {
		m.Media = &Media{Mimetype: mime, FileName: name, Size: uint64(size), Path: fmt.Sprintf("/media/%d", m.Seq)}
	}
	return m, nil
}

// Filter selects messages for /messages, /messages/wait, /events and the webhook.
type Filter struct {
	Chat     string
	ChatOnly bool
	Types    []string
	FromMe   *bool
	IsGroup  *bool
	Since    int64
}

var messageTypes = map[string]bool{
	"text": true, "image": true, "video": true, "document": true, "audio": true,
	"sticker": true, "reaction": true, "location": true,
}

// ParseFilter reads chat, type, from_me, is_group and since from a query string. resolve turns a
// phone number into the chat JID WhatsApp uses.
func ParseFilter(q url.Values, resolve func(string) (string, error)) (Filter, error) {
	var f Filter
	if c := strings.TrimSpace(q.Get("chat")); c != "" {
		jid, err := resolve(c)
		if err != nil {
			return f, err
		}
		f.Chat = jid
	}
	for _, t := range strings.Split(q.Get("type"), ",") {
		if t = strings.TrimSpace(t); t == "" {
			continue
		}
		if !messageTypes[t] {
			return f, fmt.Errorf("unknown type %q", t)
		}
		f.Types = append(f.Types, t)
	}
	for key, dst := range map[string]**bool{"from_me": &f.FromMe, "is_group": &f.IsGroup} {
		if v := q.Get(key); v != "" {
			bv, err := strconv.ParseBool(v)
			if err != nil {
				return f, fmt.Errorf("%s must be true or false", key)
			}
			*dst = &bv
		}
	}
	if v := q.Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return f, errors.New("since must be a message seq (a non-negative integer)")
		}
		f.Since = n
	}
	return f, nil
}

func (f Filter) Match(m Message) bool {
	if f.Chat != "" && m.Chat != f.Chat && (f.ChatOnly || m.Sender != f.Chat) {
		return false
	}
	if len(f.Types) > 0 && !slices.Contains(f.Types, m.Type) {
		return false
	}
	if f.FromMe != nil && *f.FromMe != m.FromMe {
		return false
	}
	if f.IsGroup != nil && *f.IsGroup != m.IsGroup {
		return false
	}
	return m.Seq > f.Since
}

func (f Filter) where() (string, []any) {
	conds := []string{"seq > ?"}
	args := []any{f.Since}
	switch {
	case f.Chat != "" && f.ChatOnly:
		conds = append(conds, "chat = ?")
		args = append(args, f.Chat)
	case f.Chat != "":
		conds = append(conds, "(chat = ? OR sender = ?)")
		args = append(args, f.Chat, f.Chat)
	}
	if len(f.Types) > 0 {
		conds = append(conds, "type IN (?"+strings.Repeat(",?", len(f.Types)-1)+")")
		for _, t := range f.Types {
			args = append(args, t)
		}
	}
	if f.FromMe != nil {
		conds = append(conds, "from_me = ?")
		args = append(args, *f.FromMe)
	}
	if f.IsGroup != nil {
		conds = append(conds, "is_group = ?")
		args = append(args, *f.IsGroup)
	}
	return strings.Join(conds, " AND "), args
}

// Query returns matching messages. oldestFirst orders by seq (for cursors); otherwise newest first by time.
func (b *Bot) Query(f Filter, limit int, oldestFirst bool) []Message {
	b.relinkLIDs()
	where, args := f.where()
	order := "ts DESC, seq DESC"
	if oldestFirst {
		order = "seq ASC"
	}
	rows, err := b.db.Query(`SELECT `+msgCols+` FROM wabot_messages WHERE `+where+` ORDER BY `+order+` LIMIT ?`,
		append(args, limit)...)
	if err != nil {
		return []Message{}
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		if m, err := scanMessage(rows); err == nil {
			out = append(out, m)
		}
	}
	return out
}

func (b *Bot) LastSeq() int64 {
	var seq sql.NullInt64
	_ = b.db.QueryRow(`SELECT MAX(seq) FROM wabot_messages`).Scan(&seq)
	return seq.Int64
}

// Messages returns the newest messages of one chat (or of all chats for ""), newest first.
func (b *Bot) Messages(chat string, limit int) []Message {
	return b.Query(Filter{Chat: chat, ChatOnly: chat != ""}, limit, false)
}

func (b *Bot) getMessage(chat, id string) (Message, error) {
	return b.getMessageWhere(`chat = ? AND id = ?`, chat, id)
}

func (b *Bot) getMessageBySeq(seq int64) (Message, error) { return b.getMessageWhere(`seq = ?`, seq) }

func (b *Bot) getMessageWhere(where string, args ...any) (Message, error) {
	b.relinkLIDs()
	m, err := scanMessage(b.db.QueryRow(`SELECT `+msgCols+` FROM wabot_messages WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return m, errNotFound
	}
	return m, err
}

func (b *Bot) applyEdit(chat, id, text string) {
	b.updateMessage(chat, id, `text = ?, edited = 1`, text)
}

func (b *Bot) applyDelete(chat, id string) {
	b.updateMessage(chat, id, `text = '', deleted = 1, raw = NULL`)
}

func (b *Bot) updateMessage(chat, id, set string, args ...any) {
	if _, err := b.db.Exec(`UPDATE wabot_messages SET `+set+` WHERE chat = ? AND id = ?`, append(args, chat, id)...); err == nil {
		b.publishUpdate(chat, id)
	}
}

func (b *Bot) publishUpdate(chat, id string) {
	m, err := b.getMessage(chat, id)
	if err != nil {
		return
	}
	b.publish(MessageUpdate{m})
	if b.Webhook != nil {
		b.Webhook.Deliver("update", m)
	}
}

// relinkLIDs moves messages stored under a hidden @lid ID onto the phone-number chat once
// whatsmeow has learned which number that ID belongs to.
func (b *Bot) relinkLIDs() {
	var count, maxRow int64
	if b.db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(rowid), 0) FROM whatsmeow_lid_map`).Scan(&count, &maxRow) != nil {
		return
	}
	key := fmt.Sprint(count, "/", maxRow)
	b.mu.RLock()
	same := key == b.lidKey
	b.mu.RUnlock()
	if same {
		return
	}
	mapped := `replace(%s, '@lid', '') IN (SELECT lid FROM whatsmeow_lid_map)`
	for _, col := range []string{"chat", "sender"} {
		if _, err := b.db.Exec(`UPDATE OR IGNORE wabot_messages
			SET ` + col + ` = (SELECT pn FROM whatsmeow_lid_map WHERE lid = replace(` + col + `, '@lid', '')) || '@s.whatsapp.net'
			WHERE ` + col + ` LIKE '%@lid' AND ` + fmt.Sprintf(mapped, col)); err != nil {
			return
		}
	}
	// Rows left under @lid are copies of messages already stored under the phone number.
	if _, err := b.db.Exec(`DELETE FROM wabot_messages WHERE chat LIKE '%@lid' AND ` + fmt.Sprintf(mapped, "chat")); err != nil {
		return
	}
	b.mu.Lock()
	b.lidKey = key
	b.mu.Unlock()
}

// Chats lists conversations by most recent activity. Reactions don't count as activity.
func (b *Bot) Chats(ctx context.Context, limit int) ([]Chat, error) {
	b.relinkLIDs()
	rows, err := b.db.QueryContext(ctx, `
SELECT chat, is_group, type, CASE WHEN deleted THEN '' ELSE text END, from_me, MAX(ts),
	(SELECT push_name FROM wabot_messages p WHERE p.chat = m.chat AND p.from_me = 0 AND p.push_name <> '' AND p.is_group = 0
	 ORDER BY ts DESC LIMIT 1)
FROM wabot_messages m WHERE type <> 'reaction' GROUP BY chat ORDER BY MAX(ts) DESC LIMIT ?`, limit)
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
			if name := cmp.Or(c.FullName, c.FirstName, c.BusinessName, c.PushName); name != "" {
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

type groupInfo struct {
	name   string
	admins map[string]bool
}

func (b *Bot) groupName(ctx context.Context, chat string) string { return b.group(ctx, chat).name }

func (b *Bot) isGroupAdmin(ctx context.Context, chat, user string) bool {
	return b.group(ctx, chat).admins[user]
}

// group returns cached group info, refreshing at most every 10 minutes.
func (b *Bot) group(ctx context.Context, chat string) groupInfo {
	b.mu.RLock()
	g, ok := b.groups[chat]
	fresh := time.Since(b.groupsAt) < 10*time.Minute
	b.mu.RUnlock()
	if ok || fresh || !b.Client.IsLoggedIn() {
		return g
	}
	joined, err := b.Client.GetJoinedGroups(ctx)
	if err != nil {
		return g
	}
	all := make(map[string]groupInfo, len(joined))
	for _, j := range joined {
		info := groupInfo{name: j.Name, admins: map[string]bool{}}
		for _, p := range j.Participants {
			if p.IsAdmin || p.IsSuperAdmin {
				for _, id := range []types.JID{p.JID, p.PhoneNumber, p.LID} {
					if !id.IsEmpty() {
						info.admins[b.phoneJID(id.ToNonAD()).String()] = true
					}
				}
			}
		}
		all[j.JID.String()] = info
	}
	b.mu.Lock()
	b.groups, b.groupsAt = all, time.Now()
	b.mu.Unlock()
	return all[chat]
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
			if err != nil || evt.Message.GetProtocolMessage() != nil {
				continue
			}
			m := b.toMessage(evt)
			if m.Type == "other" {
				continue
			}
			// ParseWebMessage turns an edit into the new content under the original ID.
			if m.Edited = evt.IsEdit; m.Edited {
				res, err := tx.Exec(`UPDATE wabot_messages SET text = ?, edited = 1 WHERE chat = ? AND id = ?`, m.Text, m.Chat, m.ID)
				if err == nil {
					if n, _ := res.RowsAffected(); n > 0 {
						continue
					}
				}
			}
			_, _, _ = saveMessage(tx, m)
		}
	}
	if tx.Commit() == nil {
		b.publish(ChatsChanged{})
	}
}
