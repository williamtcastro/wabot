// Package store keeps wame's message history and audit log in SQLite, next to whatsmeow's own tables.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

type Store struct {
	db     *sql.DB
	mu     sync.RWMutex
	lidKey string
}

// Open creates or upgrades wame's tables in db.
func Open(ctx context.Context, db *sql.DB) (*Store, error) {
	if err := migrate(ctx, db); err != nil {
		return nil, fmt.Errorf("create message tables: %w", err)
	}
	if err := migrateAudit(ctx, db); err != nil {
		return nil, fmt.Errorf("create audit table: %w", err)
	}
	if err := migrateTokens(ctx, db); err != nil {
		return nil, fmt.Errorf("create tokens table: %w", err)
	}
	if err := migrateState(ctx, db); err != nil {
		return nil, fmt.Errorf("create state and search tables: %w", err)
	}
	return &Store{db: db}, nil
}

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

// save inserts m unless (chat, id) already exists and returns its seq.
func save(db execer, m Message) (seq int64, inserted bool, err error) {
	var mime, name string
	var size uint64
	if m.Media != nil {
		mime, name, size = m.Media.Mimetype, m.Media.FileName, m.Media.Size
	}
	res, err := db.Exec(`INSERT OR IGNORE INTO wabot_messages
		(chat, id, sender, push_name, from_me, is_group, type, text, ts, reply_to, mimetype, file_name, file_size, edited, raw, raw_chat, raw_sender)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.Chat, m.ID, m.Sender, m.PushName, m.FromMe, m.IsGroup, m.Type, m.Text, m.Timestamp.UnixMilli(),
		m.ReplyTo, mime, name, int64(size), m.Edited, m.Raw, m.RawChat, m.RawSender)
	if err != nil {
		return 0, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, false, nil
	}
	seq, err = res.LastInsertId()
	return seq, err == nil, err
}

// Save inserts m unless (chat, id) already exists. The returned message has Seq and Media.Path filled in.
func (s *Store) Save(m Message) (Message, bool, error) {
	seq, inserted, err := save(s.db, m)
	if err != nil || !inserted {
		return m, false, err
	}
	m.Seq = seq
	if m.FromMe {
		s.MarkReadUpTo(m.Chat, seq)
	}
	if strings.HasSuffix(m.Chat, "@lid") || strings.HasSuffix(m.Sender, "@lid") {
		s.mu.Lock()
		s.lidKey = ""
		s.mu.Unlock()
	}
	if m.Media != nil {
		media := *m.Media
		media.Path = mediaPath(seq)
		m.Media = &media
	}
	return m, true, nil
}

// Import stores a history-sync batch in one transaction. An edited message updates the stored
// original when there is one.
func (s *Store) Import(msgs []Message) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, m := range msgs {
		if m.Edited {
			res, err := tx.Exec(`UPDATE wabot_messages SET text = ?, edited = 1 WHERE chat = ? AND id = ?`, m.Text, m.Chat, m.ID)
			if err == nil {
				if n, _ := res.RowsAffected(); n > 0 {
					continue
				}
			}
		}
		_, _, _ = save(tx, m)
	}
	imported := map[string]bool{}
	for _, m := range msgs {
		imported[m.Chat] = true
	}
	for chat := range imported {
		if _, err := tx.Exec(`INSERT INTO wabot_chat_state (chat, read_seq) SELECT ?, MAX(seq) FROM wabot_messages WHERE chat = ?
			ON CONFLICT (chat) DO UPDATE SET read_seq = excluded.read_seq WHERE excluded.read_seq > read_seq`, chat, chat); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const msgCols = `seq, chat, id, sender, push_name, from_me, is_group, type, text, ts,
	reply_to, mimetype, file_name, file_size, edited, deleted, raw, raw_chat, raw_sender`

type scanner interface{ Scan(dest ...any) error }

func scanMessage(r scanner) (Message, error) {
	var m Message
	var ts, size int64
	var mime, name string
	err := r.Scan(&m.Seq, &m.Chat, &m.ID, &m.Sender, &m.PushName, &m.FromMe, &m.IsGroup, &m.Type, &m.Text, &ts,
		&m.ReplyTo, &mime, &name, &size, &m.Edited, &m.Deleted, &m.Raw, &m.RawChat, &m.RawSender)
	if err != nil {
		return m, err
	}
	m.Timestamp = time.UnixMilli(ts)
	if mime != "" {
		m.Media = &Media{Mimetype: mime, FileName: name, Size: uint64(size), Path: mediaPath(m.Seq)}
	}
	return m, nil
}

// Query returns matching messages. oldestFirst orders by seq (for cursors); otherwise newest first by time.
func (s *Store) Query(f Filter, limit int, oldestFirst bool) []Message {
	s.RelinkLIDs()
	where, args := f.where()
	order := "ts DESC, seq DESC"
	if oldestFirst {
		order = "seq ASC"
	}
	rows, err := s.db.Query(`SELECT `+msgCols+` FROM wabot_messages WHERE `+where+` ORDER BY `+order+` LIMIT ?`,
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

func (s *Store) LastSeq() int64 {
	var seq sql.NullInt64
	_ = s.db.QueryRow(`SELECT MAX(seq) FROM wabot_messages`).Scan(&seq)
	return seq.Int64
}

// Messages returns the newest messages of one chat (or of all chats for ""), newest first.
func (s *Store) Messages(chat string, limit int) []Message {
	return s.Query(Filter{Chat: chat, ChatOnly: chat != ""}, limit, false)
}

func (s *Store) Get(chat, id string) (Message, error) {
	return s.getWhere(`chat = ? AND id = ?`, chat, id)
}

func (s *Store) GetBySeq(seq int64) (Message, error) { return s.getWhere(`seq = ?`, seq) }

func (s *Store) getWhere(where string, args ...any) (Message, error) {
	s.RelinkLIDs()
	m, err := scanMessage(s.db.QueryRow(`SELECT `+msgCols+` FROM wabot_messages WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}

func (s *Store) MarkEdited(chat, id, text string) error {
	return s.update(chat, id, `text = ?, edited = 1`, text)
}

// MarkDeleted clears the text and the raw protobuf, so a deleted message can't be quoted or downloaded.
func (s *Store) MarkDeleted(chat, id string) error {
	return s.update(chat, id, `text = '', deleted = 1, raw = NULL`)
}

func (s *Store) update(chat, id, set string, args ...any) error {
	_, err := s.db.Exec(`UPDATE wabot_messages SET `+set+` WHERE chat = ? AND id = ?`, append(args, chat, id)...)
	return err
}

// RelinkLIDs moves messages stored under a hidden @lid ID onto the phone-number chat once
// whatsmeow has learned which number that ID belongs to.
func (s *Store) RelinkLIDs() {
	var count, maxRow int64
	if s.db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(rowid), 0) FROM whatsmeow_lid_map`).Scan(&count, &maxRow) != nil {
		return
	}
	key := fmt.Sprint(count, "/", maxRow)
	s.mu.RLock()
	same := key == s.lidKey
	s.mu.RUnlock()
	if same {
		return
	}
	mapped := `replace(%s, '@lid', '') IN (SELECT lid FROM whatsmeow_lid_map)`
	for _, col := range []string{"chat", "sender"} {
		if _, err := s.db.Exec(`UPDATE OR IGNORE wabot_messages
			SET ` + col + ` = (SELECT pn FROM whatsmeow_lid_map WHERE lid = replace(` + col + `, '@lid', '')) || '@s.whatsapp.net'
			WHERE ` + col + ` LIKE '%@lid' AND ` + fmt.Sprintf(mapped, col)); err != nil {
			return
		}
	}
	// Rows left under @lid are copies of messages already stored under the phone number.
	if _, err := s.db.Exec(`DELETE FROM wabot_messages WHERE chat LIKE '%@lid' AND ` + fmt.Sprintf(mapped, "chat")); err != nil {
		return
	}
	_, _ = s.db.Exec(`INSERT INTO wabot_chat_state (chat, read_seq)
		SELECT (SELECT pn FROM whatsmeow_lid_map WHERE lid = replace(chat, '@lid', '')) || '@s.whatsapp.net', read_seq
		FROM wabot_chat_state WHERE chat LIKE '%@lid' AND ` + fmt.Sprintf(mapped, "chat") + `
		ON CONFLICT (chat) DO UPDATE SET read_seq = MAX(read_seq, excluded.read_seq)`)
	_, _ = s.db.Exec(`DELETE FROM wabot_chat_state WHERE chat LIKE '%@lid' AND ` + fmt.Sprintf(mapped, "chat"))
	s.mu.Lock()
	s.lidKey = key
	s.mu.Unlock()
}

// Chats lists conversations by most recent activity. Reactions don't count as activity. name turns a
// chat JID and the latest push name seen in it into a display name.
func (s *Store) Chats(ctx context.Context, limit int, name func(chat, pushName string) string) ([]Chat, error) {
	s.RelinkLIDs()
	rows, err := s.db.QueryContext(ctx, `
SELECT chat, is_group, type, CASE WHEN deleted THEN '' ELSE text END, from_me, MAX(ts),
	(SELECT push_name FROM wabot_messages p WHERE p.chat = m.chat AND p.from_me = 0 AND p.push_name <> '' AND p.is_group = 0
	 ORDER BY ts DESC LIMIT 1),
	(SELECT COUNT(*) FROM wabot_messages u WHERE u.chat = m.chat AND u.from_me = 0 AND u.type <> 'reaction' AND u.deleted = 0
	 AND u.seq > COALESCE((SELECT read_seq FROM wabot_chat_state r WHERE r.chat = m.chat), 0))
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
		if err := rows.Scan(&c.JID, &c.IsGroup, &c.LastType, &c.LastText, &c.LastFromMe, &ts, &push, &c.Unread); err != nil {
			return nil, err
		}
		c.LastAt = time.UnixMilli(ts)
		c.Name = name(c.JID, push.String)
		out = append(out, c)
	}
	return out, rows.Err()
}
