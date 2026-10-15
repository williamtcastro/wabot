// Package store keeps wame's message history and audit log in SQLite, next to whatsmeow's own tables.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Store struct {
	db          *sql.DB
	mu          sync.Mutex
	lidKey      string
	contactsGen atomic.Uint64
}

// Open creates or upgrades wame's tables in db.
func Open(ctx context.Context, db *sql.DB) (*Store, error) {
	if err := renameLegacyTables(ctx, db); err != nil {
		return nil, fmt.Errorf("rename wabot_ tables: %w", err)
	}
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
	if err := migrateSettings(ctx, db); err != nil {
		return nil, fmt.Errorf("create settings table: %w", err)
	}
	if err := migrateOutbox(ctx, db); err != nil {
		return nil, fmt.Errorf("create outbox table: %w", err)
	}
	if err := migrateOptOuts(ctx, db); err != nil {
		return nil, fmt.Errorf("create opt-out table: %w", err)
	}
	if err := migrateContacts(ctx, db); err != nil {
		return nil, fmt.Errorf("create contacts table: %w", err)
	}
	if err := migrateContactCache(ctx, db); err != nil {
		return nil, fmt.Errorf("create contact cache table: %w", err)
	}
	return &Store{db: db}, nil
}

const messagesTable = `
CREATE TABLE IF NOT EXISTS wame_messages (
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
CREATE INDEX IF NOT EXISTS wame_messages_ts ON wame_messages (chat, ts);`

// migrate creates wame_messages, upgrading the first schema (implicit rowid, fewer columns) in place.
// seq is AUTOINCREMENT so a number is never reused or renumbered, even after deletes or VACUUM.
func migrate(ctx context.Context, db *sql.DB) error {
	cols := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info('wame_messages')`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		cols[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
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
		`ALTER TABLE wame_messages RENAME TO wame_messages_v1`,
		`DROP INDEX IF EXISTS wame_messages_ts`,
		messagesTable,
		`INSERT INTO wame_messages (seq, ` + list + `) SELECT rowid, ` + list + ` FROM wame_messages_v1 ORDER BY rowid`,
		`DROP TABLE wame_messages_v1`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("upgrade wame_messages: %w", err)
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
	for line := range strings.SplitSeq(messagesTable, "\n") {
		f := strings.Fields(strings.TrimSuffix(strings.TrimSpace(line), ","))
		if len(f) < 2 || have[f[0]] || f[0] == "CREATE" || f[0] == "UNIQUE" || f[0] == "seq" || f[0] == ");" {
			continue
		}
		if _, err := db.ExecContext(ctx, `ALTER TABLE wame_messages ADD COLUMN `+strings.Join(f, " ")); err != nil {
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
	res, err := db.Exec(`INSERT OR IGNORE INTO wame_messages
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
	s.relinkLater(m)
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
	defer tx.Rollback()
	imported := map[string]bool{}
	for _, m := range msgs {
		imported[m.Chat] = true
		if m.Edited {
			res, err := tx.Exec(`UPDATE wame_messages SET `+editSet+` WHERE chat = ? AND id = ? AND deleted = 0`, m.Text, m.Chat, m.ID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				continue
			}
		}
		if _, _, err := save(tx, m); err != nil {
			return err
		}
	}
	for chat := range imported {
		if _, err := tx.Exec(`INSERT INTO wame_chat_state (chat, read_seq) SELECT ?, MAX(seq) FROM wame_messages WHERE chat = ?`+upsertReadSeq, chat, chat); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, m := range msgs {
		s.relinkLater(m)
	}
	return nil
}

// upsertReadSeq ends an INSERT into wame_chat_state: the read position only moves forward.
const upsertReadSeq = `
	ON CONFLICT (chat) DO UPDATE SET read_seq = excluded.read_seq WHERE excluded.read_seq > read_seq`

const editSet = `text = ?, edited = 1`

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
func (s *Store) Query(f Filter, limit int, oldestFirst bool) ([]Message, error) {
	s.relinkLIDs()
	where, args := f.where()
	order := "ts DESC, seq DESC"
	if oldestFirst {
		order = "seq ASC"
	}
	rows, err := s.db.Query(`SELECT `+msgCols+` FROM wame_messages WHERE `+where+` ORDER BY `+order+` LIMIT ?`,
		append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) LastSeq() (int64, error) {
	var seq sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(seq) FROM wame_messages`).Scan(&seq)
	return seq.Int64, err
}

// Messages returns the newest messages of one chat (or of all chats for ""), newest first.
func (s *Store) Messages(chat string, limit int) ([]Message, error) {
	return s.Query(Filter{Chat: chat, ChatOnly: chat != ""}, limit, false)
}

func (s *Store) Get(chat, id string) (Message, error) {
	return s.getWhere(`chat = ? AND id = ?`, chat, id)
}

func (s *Store) GetBySeq(seq int64) (Message, error) { return s.getWhere(`seq = ?`, seq) }

func (s *Store) getWhere(where string, args ...any) (Message, error) {
	s.relinkLIDs()
	m, err := scanMessage(s.db.QueryRow(`SELECT `+msgCols+` FROM wame_messages WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}

// MarkEdited changes the text unless the message was deleted: a late edit mustn't bring it back.
func (s *Store) MarkEdited(chat, id, text string) error {
	_, err := s.db.Exec(`UPDATE wame_messages SET `+editSet+` WHERE chat = ? AND id = ? AND deleted = 0`, text, chat, id)
	return err
}

// MarkDeleted clears the text and the raw protobuf, so a deleted message can't be quoted or downloaded.
func (s *Store) MarkDeleted(chat, id string) error {
	_, err := s.db.Exec(`UPDATE wame_messages SET text = '', deleted = 1, raw = NULL WHERE chat = ? AND id = ?`, chat, id)
	return err
}

// relinkLater makes the next read relink when m was stored under a hidden @lid ID.
func (s *Store) relinkLater(m Message) {
	if strings.HasSuffix(m.Chat, "@lid") || strings.HasSuffix(m.Sender, "@lid") {
		s.mu.Lock()
		s.lidKey = ""
		s.mu.Unlock()
	}
}

// relinkLIDs moves messages stored under a hidden @lid ID onto the phone-number chat once whatsmeow
// has learned which number that ID belongs to. It runs in one transaction, one at a time, so a
// message saved meanwhile can't be taken for a copy and deleted.
func (s *Store) relinkLIDs() {
	var count, maxRow int64
	if s.db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(rowid), 0) FROM whatsmeow_lid_map`).Scan(&count, &maxRow) != nil {
		return
	}
	key := fmt.Sprint(count, "/", maxRow)
	s.mu.Lock()
	defer s.mu.Unlock()
	if key == s.lidKey {
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	const mapped = ` LIKE '%@lid' AND replace(chat, '@lid', '') IN (SELECT lid FROM whatsmeow_lid_map)`
	const pn = `(SELECT pn FROM whatsmeow_lid_map WHERE lid = replace(chat, '@lid', '')) || '@s.whatsapp.net'`
	for _, stmt := range []string{
		`UPDATE OR IGNORE wame_messages SET chat = ` + pn + ` WHERE chat` + mapped,
		strings.ReplaceAll(`UPDATE OR IGNORE wame_messages SET chat = `+pn+` WHERE chat`+mapped, "chat", "sender"),
		// Rows left under @lid are copies of messages already stored under the phone number.
		`DELETE FROM wame_messages WHERE chat` + mapped,
		`INSERT INTO wame_chat_state (chat, read_seq) SELECT ` + pn + `, read_seq FROM wame_chat_state WHERE chat` + mapped + `
			ON CONFLICT (chat) DO UPDATE SET read_seq = MAX(read_seq, excluded.read_seq)`,
		`DELETE FROM wame_chat_state WHERE chat` + mapped,
		`UPDATE OR IGNORE wame_optouts SET chat = ` + pn + ` WHERE chat` + mapped,
		`DELETE FROM wame_optouts WHERE chat` + mapped,
		// A cache row is a copy of what WhatsApp said; when the phone JID already has one, the @lid copy goes.
		strings.ReplaceAll(`UPDATE OR IGNORE wame_contact_cache SET chat = `+pn+` WHERE chat`+mapped, "chat", "jid"),
		strings.ReplaceAll(`DELETE FROM wame_contact_cache WHERE chat`+mapped, "chat", "jid"),
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return
		}
	}
	moved, err := relinkContacts(tx)
	if err != nil {
		return
	}
	if tx.Commit() == nil {
		s.lidKey = key
		if moved > 0 {
			s.contactsGen.Add(1)
		}
	}
}

// Chats lists conversations by most recent activity. Reactions don't count as activity. name turns a
// chat JID and the latest push name seen in it into a display name.
func (s *Store) Chats(ctx context.Context, limit int, name func(chat, pushName string) string) ([]Chat, error) {
	s.relinkLIDs()
	rows, err := s.db.QueryContext(ctx, `
SELECT chat, is_group, type, CASE WHEN deleted THEN '' ELSE text END, from_me, MAX(ts),
	(SELECT push_name FROM wame_messages p WHERE p.chat = m.chat AND p.from_me = 0 AND p.push_name <> '' AND p.is_group = 0
	 ORDER BY ts DESC LIMIT 1),
	(SELECT COUNT(*) FROM wame_messages u WHERE u.chat = m.chat AND u.from_me = 0 AND u.type <> 'reaction' AND u.deleted = 0
	 AND u.seq > COALESCE((SELECT read_seq FROM wame_chat_state r WHERE r.chat = m.chat), 0))
FROM wame_messages m WHERE type <> 'reaction' GROUP BY chat ORDER BY MAX(ts) DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Chat{}
	var push []string
	for rows.Next() {
		var c Chat
		var ts int64
		var p sql.NullString
		if err := rows.Scan(&c.JID, &c.IsGroup, &c.LastType, &c.LastText, &c.LastFromMe, &ts, &p, &c.Unread); err != nil {
			return nil, err
		}
		c.LastAt = time.UnixMilli(ts)
		out, push = append(out, c), append(push, p.String)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	// name may hit the network (group names), so it runs after the cursor is closed.
	for i := range out {
		out[i].Name = name(out[i].JID, push[i])
	}
	return out, nil
}

// legacyPrefix is the table prefix from before the rename to wame.
const legacyPrefix = "wabot_"

// renameLegacyTables moves a database from before the rename to the wame_ names, in one
// transaction. Tables are renamed with their rows; indexes, triggers and the search index are
// dropped, and the migrations after this one recreate them (rebuilding the search index).
func renameLegacyTables(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT type, name FROM sqlite_master WHERE name LIKE 'wabot\_%' ESCAPE '\'`)
	if err != nil {
		return err
	}
	var tables, other [][2]string
	for rows.Next() {
		var typ, name string
		if err := rows.Scan(&typ, &name); err != nil {
			rows.Close()
			return err
		}
		if typ == "table" {
			tables = append(tables, [2]string{typ, name})
		} else {
			other = append(other, [2]string{typ, name})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(tables)+len(other) == 0 {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, o := range other {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DROP %s IF EXISTS "%s"`, strings.ToUpper(o[0]), o[1])); err != nil {
			return err
		}
	}
	// Dropping the search index also drops its shadow tables, so skip those below.
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS wabot_messages_fts`); err != nil {
		return err
	}
	for _, t := range tables {
		name := t[1]
		if strings.HasPrefix(name, "wabot_messages_fts") {
			continue
		}
		newName := "wame_" + strings.TrimPrefix(name, legacyPrefix)
		exists, err := tableExists(ctx, tx, newName)
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("both %s and %s exist; keep one and remove the other", name, newName)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE "%s" RENAME TO "%s"`, name, newName)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
