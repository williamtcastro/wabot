package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Outbox item states. queued and sending are open; the rest are final.
const (
	OutboxQueued   = "queued"
	OutboxSending  = "sending"
	OutboxSent     = "sent"
	OutboxFailed   = "failed"
	OutboxCanceled = "canceled"
)

// OutboxStates lists every state, in display order.
var OutboxStates = []string{OutboxQueued, OutboxSending, OutboxSent, OutboxFailed, OutboxCanceled}

// OutboxItem is one queued text message. MessageID is chosen before the first attempt and reused on
// retries, so a message that did reach WhatsApp before a crash isn't shown twice.
type OutboxItem struct {
	ID        int64     `json:"id"`
	Batch     string    `json:"batch"`
	Actor     string    `json:"actor"`
	TokenID   string    `json:"-"`
	Key       string    `json:"-"`
	To        string    `json:"to"`
	Text      string    `json:"text,omitempty"`
	ReplyTo   string    `json:"reply_to,omitempty"`
	Status    string    `json:"status"`
	Attempts  int       `json:"attempts"`
	Due       time.Time `json:"due"`
	Created   time.Time `json:"created_at"`
	Updated   time.Time `json:"updated_at"`
	Chat      string    `json:"chat,omitempty"`
	MessageID string    `json:"message_id,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// OutboxFilter selects items; empty fields match everything.
type OutboxFilter struct {
	Actor  string
	Batch  string
	Status string
	Before int64
}

// OutboxCounts is how many items are in each state, plus the oldest still waiting.
type OutboxCounts struct {
	Queued       int       `json:"queued"`
	Sending      int       `json:"sending"`
	Sent         int       `json:"sent"`
	Failed       int       `json:"failed"`
	Canceled     int       `json:"canceled"`
	OldestQueued time.Time `json:"oldest_queued,omitzero"`
	NextDue      time.Time `json:"next_due,omitzero"`
}

func (c *OutboxCounts) of(status string) *int {
	switch status {
	case OutboxQueued:
		return &c.Queued
	case OutboxSending:
		return &c.Sending
	case OutboxSent:
		return &c.Sent
	case OutboxFailed:
		return &c.Failed
	case OutboxCanceled:
		return &c.Canceled
	}
	return nil
}

// Of is the count for one state; "" means all of them.
func (c OutboxCounts) Of(status string) int {
	if status == "" {
		return c.Queued + c.Sending + c.Sent + c.Failed + c.Canceled
	}
	if n := c.of(status); n != nil {
		return *n
	}
	return 0
}

// OutboxResult is the outcome of one attempt. With Status queued the item is retried at Due; Refund
// gives the attempt back because it never reached WhatsApp.
type OutboxResult struct {
	Status string
	Chat   string
	Error  string
	Due    time.Time
	Refund bool
}

func migrateOutbox(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS wame_outbox (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	batch      TEXT NOT NULL,
	actor      TEXT NOT NULL,
	token_id   TEXT NOT NULL DEFAULT '',
	key        TEXT NOT NULL DEFAULT '',
	recipient  TEXT NOT NULL,
	text       TEXT NOT NULL,
	reply_to   TEXT NOT NULL DEFAULT '',
	status     TEXT NOT NULL,
	attempts   INTEGER NOT NULL DEFAULT 0,
	due        INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	chat       TEXT NOT NULL DEFAULT '',
	message_id TEXT NOT NULL DEFAULT '',
	error      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS wame_outbox_due ON wame_outbox (status, due, id);
CREATE INDEX IF NOT EXISTS wame_outbox_batch ON wame_outbox (batch, id);
CREATE UNIQUE INDEX IF NOT EXISTS wame_outbox_key ON wame_outbox (actor, key) WHERE key != '';`)
	return err
}

const outboxColumns = `id, batch, actor, token_id, key, recipient, text, reply_to, status, attempts, due, created_at, updated_at, chat, message_id, error`

func scanOutbox(row interface{ Scan(...any) error }) (OutboxItem, error) {
	var it OutboxItem
	var due, created, updated int64
	err := row.Scan(&it.ID, &it.Batch, &it.Actor, &it.TokenID, &it.Key, &it.To, &it.Text, &it.ReplyTo, &it.Status,
		&it.Attempts, &due, &created, &updated, &it.Chat, &it.MessageID, &it.Error)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	it.Due, it.Created, it.Updated = time.UnixMilli(due), time.UnixMilli(created), time.UnixMilli(updated)
	return it, err
}

func (s *Store) queryOutbox(query string, args ...any) ([]OutboxItem, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OutboxItem{}
	for rows.Next() {
		it, err := scanOutbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Enqueue stores items in one transaction. An item whose (Actor, Key) is already stored isn't added
// again; the stored one is returned in its place, so a retried request doesn't send twice.
func (s *Store) Enqueue(items []OutboxItem) ([]OutboxItem, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	out := make([]OutboxItem, len(items))
	for i, it := range items {
		out[i], err = scanOutbox(tx.QueryRow(`INSERT INTO wame_outbox (batch, actor, token_id, key, recipient, text, reply_to, status, due, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING RETURNING `+outboxColumns,
			it.Batch, it.Actor, it.TokenID, it.Key, it.To, it.Text, it.ReplyTo, OutboxQueued, max(it.Due.UnixMilli(), now), now, now))
		if errors.Is(err, ErrNotFound) {
			out[i], err = scanOutbox(tx.QueryRow(`SELECT `+outboxColumns+` FROM wame_outbox WHERE actor = ? AND key = ?`, it.Actor, it.Key))
		}
		if err != nil {
			return nil, err
		}
	}
	return out, tx.Commit()
}

func (s *Store) OutboxItem(id int64) (OutboxItem, error) {
	return scanOutbox(s.db.QueryRow(`SELECT `+outboxColumns+` FROM wame_outbox WHERE id = ?`, id))
}

func (f OutboxFilter) where() (string, []any) {
	var conds []string
	var args []any
	for _, c := range []struct {
		cond string
		v    any
		set  bool
	}{
		{"actor = ?", f.Actor, f.Actor != ""},
		{"batch = ?", f.Batch, f.Batch != ""},
		{"status = ?", f.Status, f.Status != ""},
		{"id < ?", f.Before, f.Before > 0},
	} {
		if c.set {
			conds, args = append(conds, c.cond), append(args, c.v)
		}
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ListOutbox returns matching items, newest first.
func (s *Store) ListOutbox(f OutboxFilter, limit int) ([]OutboxItem, error) {
	where, args := f.where()
	return s.queryOutbox(`SELECT `+outboxColumns+` FROM wame_outbox`+where+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
}

// CountOutbox counts matching items by state; f.Status and f.Before are ignored.
func (s *Store) CountOutbox(f OutboxFilter) (OutboxCounts, error) {
	f.Status, f.Before = "", 0
	where, args := f.where()
	rows, err := s.db.Query(`SELECT status, COUNT(*), MIN(created_at), MIN(due) FROM wame_outbox`+where+` GROUP BY status`, args...)
	if err != nil {
		return OutboxCounts{}, err
	}
	defer rows.Close()
	var c OutboxCounts
	for rows.Next() {
		var status string
		var n int
		var oldest, due int64
		if err := rows.Scan(&status, &n, &oldest, &due); err != nil {
			return c, err
		}
		if p := c.of(status); p != nil {
			*p = n
		}
		if status == OutboxQueued {
			c.OldestQueued, c.NextDue = time.UnixMilli(oldest), time.UnixMilli(due)
		}
	}
	return c, rows.Err()
}

// DueOutbox returns up to limit queued items whose time has come, oldest due first.
func (s *Store) DueOutbox(now time.Time, limit int) ([]OutboxItem, error) {
	return s.queryOutbox(`SELECT `+outboxColumns+` FROM wame_outbox WHERE status = ? AND due <= ? ORDER BY due, id LIMIT ?`,
		OutboxQueued, now.UnixMilli(), limit)
}

// NextOutboxDue is when the next queued item is due, or zero when nothing is queued.
func (s *Store) NextOutboxDue() (time.Time, error) {
	var due sql.NullInt64
	if err := s.db.QueryRow(`SELECT MIN(due) FROM wame_outbox WHERE status = ?`, OutboxQueued).Scan(&due); err != nil || !due.Valid {
		return time.Time{}, err
	}
	return time.UnixMilli(due.Int64), nil
}

// ClaimOutbox moves a queued item to sending, counts the attempt and fixes its message id.
// It returns ErrNotFound when the item is no longer queued (canceled meanwhile).
func (s *Store) ClaimOutbox(id int64, messageID string) (OutboxItem, error) {
	return scanOutbox(s.db.QueryRow(`UPDATE wame_outbox SET status = ?, attempts = attempts + 1, updated_at = ?,
		message_id = COALESCE(NULLIF(message_id, ''), ?) WHERE id = ? AND status = ? RETURNING `+outboxColumns,
		OutboxSending, time.Now().UnixMilli(), messageID, id, OutboxQueued))
}

// FinishOutbox records an attempt's outcome on an item that is still open. It returns ErrNotFound
// when the item was finished meanwhile (canceled while the attempt ran).
func (s *Store) FinishOutbox(id int64, r OutboxResult) (OutboxItem, error) {
	now := time.Now()
	if r.Due.IsZero() {
		r.Due = now
	}
	refund := 0
	if r.Refund {
		refund = 1
	}
	return scanOutbox(s.db.QueryRow(`UPDATE wame_outbox SET status = ?, updated_at = ?, error = ?, due = ?, attempts = attempts - ?,
		chat = COALESCE(NULLIF(?, ''), chat) WHERE id = ? AND status IN (?, ?) RETURNING `+outboxColumns,
		r.Status, now.UnixMilli(), r.Error, r.Due.UnixMilli(), refund, r.Chat, id, OutboxQueued, OutboxSending))
}

// CancelOutbox cancels the queued items matching f (only id, when id > 0) and returns them.
func (s *Store) CancelOutbox(id int64, f OutboxFilter) ([]OutboxItem, error) {
	f.Status, f.Before = OutboxQueued, 0
	where, args := f.where()
	if id > 0 {
		where += " AND id = ?"
		args = append(args, id)
	}
	return s.queryOutbox(`UPDATE wame_outbox SET status = ?, updated_at = ?`+where+` RETURNING `+outboxColumns,
		append([]any{OutboxCanceled, time.Now().UnixMilli()}, args...)...)
}

// RequeueSending puts items left in sending by a crash or shutdown back in the queue. They keep their
// message id, so WhatsApp shows a message that did go out only once.
func (s *Store) RequeueSending() (int64, error) {
	res, err := s.db.Exec(`UPDATE wame_outbox SET status = ?, updated_at = ? WHERE status = ?`, OutboxQueued, time.Now().UnixMilli(), OutboxSending)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PruneOutbox deletes finished items last changed before t.
func (s *Store) PruneOutbox(t time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM wame_outbox WHERE status IN (?,?,?) AND updated_at < ?`, OutboxSent, OutboxFailed, OutboxCanceled, t.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
