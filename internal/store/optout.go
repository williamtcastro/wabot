package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// OptOut is a chat that asked not to get queued messages.
type OptOut struct {
	Chat   string    `json:"chat"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
	By     string    `json:"by"`
}

func migrateOptOuts(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS wame_optouts (
	chat   TEXT PRIMARY KEY,
	reason TEXT NOT NULL DEFAULT '',
	at     INTEGER NOT NULL,
	by     TEXT NOT NULL
);`)
	return err
}

// AddOptOut stores o unless the chat is already listed; the first entry is kept.
func (s *Store) AddOptOut(o OptOut) (added bool, err error) {
	s.relinkLater(Message{Chat: o.Chat})
	res, err := s.db.Exec(`INSERT INTO wame_optouts (chat, reason, at, by) VALUES (?,?,?,?) ON CONFLICT (chat) DO NOTHING`,
		o.Chat, o.Reason, o.At.UnixMilli(), o.By)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// RemoveOptOut deletes every given key of a chat (its phone JID, its @lid) from the list, or returns
// ErrNotFound when none was listed.
func (s *Store) RemoveOptOut(chats ...string) error {
	if len(chats) == 0 {
		return ErrNotFound
	}
	res, err := s.db.Exec(`DELETE FROM wame_optouts WHERE chat IN (`+placeholders(len(chats))+`)`, anys(chats)...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		err = ErrNotFound
	}
	return err
}

// OptedOut reports whether any of the keys a chat may be stored under is on the list.
func (s *Store) OptedOut(chats ...string) (bool, error) {
	if len(chats) == 0 {
		return false, nil
	}
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM wame_optouts WHERE chat IN (`+placeholders(len(chats))+`) LIMIT 1`, anys(chats)...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// OptOuts lists every entry, newest first.
func (s *Store) OptOuts() ([]OptOut, error) {
	s.relinkLIDs()
	rows, err := s.db.Query(`SELECT chat, reason, at, by FROM wame_optouts ORDER BY at DESC, chat`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OptOut{}
	for rows.Next() {
		var o OptOut
		var at int64
		if err := rows.Scan(&o.Chat, &o.Reason, &at, &o.By); err != nil {
			return nil, err
		}
		o.At = time.UnixMilli(at)
		out = append(out, o)
	}
	return out, rows.Err()
}

func anys(ss []string) []any {
	out := make([]any, len(ss))
	for i, v := range ss {
		out[i] = v
	}
	return out
}
