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
	res, err := s.db.Exec(`INSERT INTO wame_optouts (chat, reason, at, by) VALUES (?,?,?,?) ON CONFLICT (chat) DO NOTHING`,
		o.Chat, o.Reason, o.At.UnixMilli(), o.By)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// RemoveOptOut deletes chat from the list, or returns ErrNotFound.
func (s *Store) RemoveOptOut(chat string) error {
	res, err := s.db.Exec(`DELETE FROM wame_optouts WHERE chat = ?`, chat)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		err = ErrNotFound
	}
	return err
}

// OptedOut reports whether chat is on the list.
func (s *Store) OptedOut(chat string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM wame_optouts WHERE chat = ?`, chat).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// OptOuts lists every entry, newest first.
func (s *Store) OptOuts() ([]OptOut, error) {
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
