package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// Group is the cached name, admins and members of a group, so names show before WhatsApp connects.
// Admins are phone JIDs where known. Members go to wame_group_members as WhatsApp reported them;
// SaveGroups leaves a group's members alone when Members is nil. Groups doesn't read them.
type Group struct {
	JID     string
	Name    string
	Admins  []string
	Members []GroupMember
}

// GroupMember is one participant of a group: a phone JID or an @lid, and its role.
type GroupMember struct {
	JID  string
	Role string
}

// Roles in wame_group_members.
const (
	RoleMember     = "member"
	RoleAdmin      = "admin"
	RoleSuperAdmin = "superadmin"
)

// GroupRef names a group.
type GroupRef struct {
	JID  string `json:"jid"`
	Name string `json:"name"`
}

// ChatRead is published when a chat's read position moves (opened, /read, or read on another device).
type ChatRead struct {
	Chat string
	Seq  int64
}

const stateTables = `
CREATE TABLE IF NOT EXISTS wame_groups (
	jid     TEXT PRIMARY KEY,
	name    TEXT NOT NULL,
	admins  TEXT NOT NULL DEFAULT '[]',
	updated INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS wame_chat_state (
	chat     TEXT PRIMARY KEY,
	read_seq INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS wame_messages_chat_seq ON wame_messages (chat, seq);
CREATE TABLE IF NOT EXISTS wame_group_members (
	group_jid  TEXT NOT NULL,
	member     TEXT NOT NULL,
	role       TEXT NOT NULL DEFAULT 'member',
	first_seen INTEGER NOT NULL,
	last_seen  INTEGER NOT NULL,
	PRIMARY KEY (group_jid, member)
);
CREATE INDEX IF NOT EXISTS wame_group_members_member ON wame_group_members (member);`

// The full-text index mirrors wame_messages.text through triggers. remove_diacritics folds accents,
// so "cafe" finds "café".
const searchTables = `
CREATE VIRTUAL TABLE wame_messages_fts USING fts5(
	text, content='wame_messages', content_rowid='seq', tokenize='unicode61 remove_diacritics 2'
);
CREATE TRIGGER wame_messages_fts_insert AFTER INSERT ON wame_messages BEGIN
	INSERT INTO wame_messages_fts(rowid, text) VALUES (new.seq, new.text);
END;
CREATE TRIGGER wame_messages_fts_delete AFTER DELETE ON wame_messages BEGIN
	INSERT INTO wame_messages_fts(wame_messages_fts, rowid, text) VALUES ('delete', old.seq, old.text);
END;
CREATE TRIGGER wame_messages_fts_update AFTER UPDATE OF text ON wame_messages BEGIN
	INSERT INTO wame_messages_fts(wame_messages_fts, rowid, text) VALUES ('delete', old.seq, old.text);
	INSERT INTO wame_messages_fts(rowid, text) VALUES (new.seq, new.text);
END;
INSERT INTO wame_messages_fts(wame_messages_fts) VALUES ('rebuild');`

func migrateState(ctx context.Context, db *sql.DB) error {
	hadState, err := tableExists(ctx, db, "wame_chat_state")
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, stateTables); err != nil {
		return err
	}
	// Existing history counts as read; only messages that arrive from now on can be unread.
	if !hadState {
		if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO wame_chat_state (chat, read_seq)
			SELECT chat, MAX(seq) FROM wame_messages GROUP BY chat`); err != nil {
			return err
		}
	}
	hasSearch, err := tableExists(ctx, db, "wame_messages_fts")
	if err != nil || hasSearch {
		return err
	}
	// One transaction, so a crash can't leave the index without the triggers that keep it current.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, searchTables); err != nil {
		return err
	}
	return tx.Commit()
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func tableExists(ctx context.Context, db queryRower, name string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, name).Scan(&n)
	return n > 0, err
}

// MarkReadUpTo moves a chat's read position forward (never back) and reports whether it moved.
func (s *Store) MarkReadUpTo(chat string, seq int64) bool {
	res, err := s.db.Exec(`INSERT INTO wame_chat_state (chat, read_seq) VALUES (?, ?)`+upsertReadSeq, chat, seq)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

// MarkChatRead marks everything currently stored in chat as read.
func (s *Store) MarkChatRead(chat string) bool {
	var seq sql.NullInt64
	_ = s.db.QueryRow(`SELECT MAX(seq) FROM wame_messages WHERE chat = ?`, chat).Scan(&seq)
	return seq.Valid && s.MarkReadUpTo(chat, seq.Int64)
}

func (s *Store) ReadSeq(chat string) int64 {
	var seq int64
	_ = s.db.QueryRow(`SELECT read_seq FROM wame_chat_state WHERE chat = ?`, chat).Scan(&seq)
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
	_ = s.db.QueryRow(`SELECT MAX(seq) FROM wame_messages WHERE chat = ? AND id IN (`+placeholders(len(ids))+`)`, args...).Scan(&seq)
	return seq.Int64
}

// CountFrom counts the messages in chat with seq >= from (to know how far back to load for a jump).
func (s *Store) CountFrom(chat string, from int64) int {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM wame_messages WHERE chat = ? AND seq >= ?`, chat, from).Scan(&n)
	return n
}

// SaveGroups stores groups' names, admins and members.
func (s *Store) SaveGroups(groups []Group) error { return s.saveGroups(groups, false) }

// SaveJoinedGroups is SaveGroups for the full list of groups the account is in: members of any other
// group (one the account left) are deleted too. Group names stay, for the messages.
func (s *Store) SaveJoinedGroups(groups []Group) error { return s.saveGroups(groups, true) }

func (s *Store) saveGroups(groups []Group, all bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	for _, g := range groups {
		admins, _ := json.Marshal(g.Admins)
		if _, err := tx.Exec(`INSERT INTO wame_groups (jid, name, admins, updated) VALUES (?, ?, ?, ?)
			ON CONFLICT (jid) DO UPDATE SET name = excluded.name, admins = excluded.admins, updated = excluded.updated`,
			g.JID, g.Name, string(admins), now); err != nil {
			return err
		}
		if g.Members == nil {
			continue
		}
		for _, m := range g.Members {
			if _, err := tx.Exec(`INSERT INTO wame_group_members (group_jid, member, role, first_seen, last_seen)
				VALUES (?, ?, ?, ?, ?)
				ON CONFLICT (group_jid, member) DO UPDATE SET role = excluded.role, last_seen = excluded.last_seen`,
				g.JID, m.JID, cmp.Or(m.Role, RoleMember), now, now); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`DELETE FROM wame_group_members WHERE group_jid = ? AND last_seen <> ?`, g.JID, now); err != nil {
			return err
		}
	}
	if all {
		jids := make([]string, len(groups))
		for i, g := range groups {
			jids[i] = g.JID
		}
		if _, err := tx.Exec(`DELETE FROM wame_group_members WHERE group_jid NOT IN (SELECT value FROM json_each(?))`, jsonList(jids)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func jsonList(ss []string) string {
	data, _ := json.Marshal(ss)
	if ss == nil {
		return "[]"
	}
	return string(data)
}

// GroupsWith lists the cached groups any of keys is a member of, by name.
func (s *Store) GroupsWith(keys ...string) ([]GroupRef, error) {
	s.relinkLIDs()
	return groupsWith(s.db, keys)
}

func groupsWith(q querier, keys []string) ([]GroupRef, error) {
	out := []GroupRef{}
	if len(keys) == 0 {
		return out, nil
	}
	rows, err := q.Query(`SELECT DISTINCT g.jid, g.name FROM wame_group_members m JOIN wame_groups g ON g.jid = m.group_jid
		WHERE m.member IN (`+placeholders(len(keys))+`) ORDER BY g.name, g.jid`, anys(keys)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var g GroupRef
		if err := rows.Scan(&g.JID, &g.Name); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) Groups() ([]Group, error) {
	rows, err := s.db.Query(`SELECT jid, name, admins FROM wame_groups`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		var g Group
		var admins string
		if err := rows.Scan(&g.JID, &g.Name, &admins); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(admins), &g.Admins)
		out = append(out, g)
	}
	return out, rows.Err()
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
