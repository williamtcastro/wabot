package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// Group is the cached name and admin list of a group, so names show before WhatsApp connects.
type Group struct {
	JID    string
	Name   string
	Admins []string
}

// ChatRead is published when a chat's read position moves (opened, /read, or read on another device).
type ChatRead struct {
	Chat string
	Seq  int64
}

const stateTables = `
CREATE TABLE IF NOT EXISTS wabot_groups (
	jid     TEXT PRIMARY KEY,
	name    TEXT NOT NULL,
	admins  TEXT NOT NULL DEFAULT '[]',
	updated INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS wabot_chat_state (
	chat     TEXT PRIMARY KEY,
	read_seq INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS wabot_messages_chat_seq ON wabot_messages (chat, seq);`

// The full-text index mirrors wabot_messages.text through triggers. remove_diacritics folds accents,
// so "cafe" finds "café".
const searchTables = `
CREATE VIRTUAL TABLE wabot_messages_fts USING fts5(
	text, content='wabot_messages', content_rowid='seq', tokenize='unicode61 remove_diacritics 2'
);
CREATE TRIGGER wabot_messages_fts_insert AFTER INSERT ON wabot_messages BEGIN
	INSERT INTO wabot_messages_fts(rowid, text) VALUES (new.seq, new.text);
END;
CREATE TRIGGER wabot_messages_fts_delete AFTER DELETE ON wabot_messages BEGIN
	INSERT INTO wabot_messages_fts(wabot_messages_fts, rowid, text) VALUES ('delete', old.seq, old.text);
END;
CREATE TRIGGER wabot_messages_fts_update AFTER UPDATE OF text ON wabot_messages BEGIN
	INSERT INTO wabot_messages_fts(wabot_messages_fts, rowid, text) VALUES ('delete', old.seq, old.text);
	INSERT INTO wabot_messages_fts(rowid, text) VALUES (new.seq, new.text);
END;
INSERT INTO wabot_messages_fts(wabot_messages_fts) VALUES ('rebuild');`

func migrateState(ctx context.Context, db *sql.DB) error {
	hadState := tableExists(ctx, db, "wabot_chat_state")
	if _, err := db.ExecContext(ctx, stateTables); err != nil {
		return err
	}
	// Existing history counts as read; only messages that arrive from now on can be unread.
	if !hadState {
		if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO wabot_chat_state (chat, read_seq)
			SELECT chat, MAX(seq) FROM wabot_messages GROUP BY chat`); err != nil {
			return err
		}
	}
	if !tableExists(ctx, db, "wabot_messages_fts") {
		if _, err := db.ExecContext(ctx, searchTables); err != nil {
			return err
		}
	}
	return nil
}

func tableExists(ctx context.Context, db *sql.DB, name string) bool {
	var n int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, name).Scan(&n)
	return n > 0
}

// MarkReadUpTo moves a chat's read position forward (never back) and reports whether it moved.
func (s *Store) MarkReadUpTo(chat string, seq int64) bool {
	res, err := s.db.Exec(`INSERT INTO wabot_chat_state (chat, read_seq) VALUES (?, ?)
		ON CONFLICT (chat) DO UPDATE SET read_seq = excluded.read_seq WHERE excluded.read_seq > read_seq`, chat, seq)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

// MarkChatRead marks everything currently stored in chat as read.
func (s *Store) MarkChatRead(chat string) bool {
	var seq sql.NullInt64
	_ = s.db.QueryRow(`SELECT MAX(seq) FROM wabot_messages WHERE chat = ?`, chat).Scan(&seq)
	return seq.Valid && s.MarkReadUpTo(chat, seq.Int64)
}

func (s *Store) ReadSeq(chat string) int64 {
	var seq int64
	_ = s.db.QueryRow(`SELECT read_seq FROM wabot_chat_state WHERE chat = ?`, chat).Scan(&seq)
	return seq
}

// MaxSeqOf returns the highest seq among the given message IDs in chat (0 if none are stored).
func (s *Store) MaxSeqOf(chat string, ids []string) int64 {
	if len(ids) == 0 {
		return 0
	}
	args := []any{chat}
	for _, id := range ids {
		args = append(args, id)
	}
	var seq sql.NullInt64
	_ = s.db.QueryRow(`SELECT MAX(seq) FROM wabot_messages WHERE chat = ? AND id IN (?`+strings.Repeat(",?", len(ids)-1)+`)`, args...).Scan(&seq)
	return seq.Int64
}

// CountFrom counts the messages in chat with seq >= from (to know how far back to load for a jump).
func (s *Store) CountFrom(chat string, from int64) int {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM wabot_messages WHERE chat = ? AND seq >= ?`, chat, from).Scan(&n)
	return n
}

func (s *Store) SaveGroups(groups []Group) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	for _, g := range groups {
		admins, _ := json.Marshal(g.Admins)
		if _, err := tx.Exec(`INSERT INTO wabot_groups (jid, name, admins, updated) VALUES (?, ?, ?, ?)
			ON CONFLICT (jid) DO UPDATE SET name = excluded.name, admins = excluded.admins, updated = excluded.updated`,
			g.JID, g.Name, string(admins), now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Groups() []Group {
	rows, err := s.db.Query(`SELECT jid, name, admins FROM wabot_groups`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		var g Group
		var admins string
		if rows.Scan(&g.JID, &g.Name, &admins) == nil {
			_ = json.Unmarshal([]byte(admins), &g.Admins)
			out = append(out, g)
		}
	}
	return out
}

// ftsQuery turns what a person typed into an FTS5 query: every word must appear, the last one as a
// prefix ("bom di" finds "bom dia"). Quoting each word keeps FTS syntax characters literal.
func ftsQuery(text string) string {
	words := strings.Fields(text)
	for i, w := range words {
		words[i] = `"` + strings.ReplaceAll(w, `"`, `""`) + `"`
	}
	if len(words) > 0 {
		words[len(words)-1] += "*"
	}
	return strings.Join(words, " ")
}
