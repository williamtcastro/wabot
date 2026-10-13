package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
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

// ValidOutboxStatus accepts "" (any) or one of OutboxStates.
func ValidOutboxStatus(s string) error {
	if s == "" || slices.Contains(OutboxStates, s) {
		return nil
	}
	return fmt.Errorf("status must be one of %s", strings.Join(OutboxStates, ", "))
}

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
	Delivered time.Time `json:"delivered_at,omitzero"`
	Read      time.Time `json:"read_at,omitzero"`
}

// Receipt progress of an item WhatsApp accepted, beyond its state.
const (
	OutboxDelivered = "delivered"
	OutboxRead      = "read"
)

// Progress is the item's state or, for a sent item, delivered / read once WhatsApp reported that.
func (it OutboxItem) Progress() string {
	switch {
	case it.Status != OutboxSent:
		return it.Status
	case !it.Read.IsZero():
		return OutboxRead
	case !it.Delivered.IsZero():
		return OutboxDelivered
	}
	return it.Status
}

// OutboxFilter selects items; empty fields match everything.
type OutboxFilter struct {
	Actor  string
	Batch  string
	Status string
	Before int64
}

// For limits f to actor's own items unless the caller is an admin.
func (f OutboxFilter) For(actor string, admin bool) OutboxFilter {
	if !admin {
		f.Actor = actor
	}
	return f
}

// OutboxCounts is how many items are in each state, plus the oldest still waiting.
type OutboxCounts struct {
	Queued       int       `json:"queued"`
	Sending      int       `json:"sending"`
	Sent         int       `json:"sent"`
	Failed       int       `json:"failed"`
	Canceled     int       `json:"canceled"`
	Delivered    int       `json:"delivered"`
	Read         int       `json:"read"`
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
	if err != nil {
		return err
	}
	if err := addColumns(ctx, db, "wame_outbox", outboxReceiptColumns); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS wame_outbox_message ON wame_outbox (message_id) WHERE message_id != ''`)
	return err
}

// outboxReceiptColumns came after the table did, so they're added to older databases on startup.
var outboxReceiptColumns = []string{"delivered_at INTEGER NOT NULL DEFAULT 0", "read_at INTEGER NOT NULL DEFAULT 0"}

// addColumns adds each "name type…" definition that table doesn't have yet.
func addColumns(ctx context.Context, db *sql.DB, table string, defs []string) error {
	have, err := func() (map[string]bool, error) {
		rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		have := map[string]bool{}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return nil, err
			}
			have[name] = true
		}
		return have, rows.Err()
	}()
	if err != nil {
		return err
	}
	for _, def := range defs {
		if name, _, _ := strings.Cut(def, " "); !have[name] {
			if err := addColumn(ctx, db, table, def); err != nil {
				return err
			}
		}
	}
	return nil
}

// addColumn treats a column that is already there as added: another process opening the same old
// database may have added it since the table was read.
func addColumn(ctx context.Context, db *sql.DB, table, def string) error {
	_, err := db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+def)
	if err != nil && strings.Contains(err.Error(), "duplicate column name") {
		return nil
	}
	return err
}

const outboxColumns = `id, batch, actor, token_id, key, recipient, text, reply_to, status, attempts, due, created_at, updated_at, chat, message_id, error, delivered_at, read_at`

// outboxColumnsNoText reads an item without its text, where it isn't needed.
var outboxColumnsNoText = strings.Replace(outboxColumns, " text,", " '',", 1)

func unixMilliOrZero(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func scanOutbox(row interface{ Scan(...any) error }) (OutboxItem, error) {
	var it OutboxItem
	var due, created, updated, delivered, read int64
	err := row.Scan(&it.ID, &it.Batch, &it.Actor, &it.TokenID, &it.Key, &it.To, &it.Text, &it.ReplyTo, &it.Status,
		&it.Attempts, &due, &created, &updated, &it.Chat, &it.MessageID, &it.Error, &delivered, &read)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	it.Due, it.Created, it.Updated = time.UnixMilli(due), time.UnixMilli(created), time.UnixMilli(updated)
	it.Delivered, it.Read = unixMilliOrZero(delivered), unixMilliOrZero(read)
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
	rows, err := s.db.Query(`SELECT status, COUNT(*), MIN(created_at), MIN(due), SUM(delivered_at > 0), SUM(read_at > 0)
		FROM wame_outbox`+where+` GROUP BY status`, args...)
	if err != nil {
		return OutboxCounts{}, err
	}
	defer rows.Close()
	var c OutboxCounts
	for rows.Next() {
		var status string
		var n, delivered, read int
		var oldest, due int64
		if err := rows.Scan(&status, &n, &oldest, &due, &delivered, &read); err != nil {
			return c, err
		}
		if p := c.of(status); p != nil {
			*p = n
		}
		if status == OutboxSent {
			c.Delivered, c.Read = delivered, read
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

// MarkOutboxReceipt records a delivery receipt (or, with read, a read receipt) on the items sent
// with these message ids and returns only the items it changed (without their text), so each change
// is reported once. A read also sets delivered_at when it's missing: WhatsApp doesn't always send
// both. Any status is recorded: a receipt can beat the sent result, or show that an attempt counted
// as failed did reach the phone; callers decide what to report.
func (s *Store) MarkOutboxReceipt(messageIDs []string, read bool, at time.Time) ([]OutboxItem, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	ms := at.UnixMilli()
	set, unchanged, args := `delivered_at = ?`, `delivered_at = 0`, []any{ms}
	if read {
		set, unchanged = `read_at = ?, delivered_at = CASE WHEN delivered_at = 0 THEN ? ELSE delivered_at END`, `read_at = 0`
		args = append(args, ms)
	}
	for _, id := range messageIDs {
		args = append(args, id)
	}
	// message_id != '' lets SQLite use the partial index, and keeps unsent items out.
	return s.queryOutbox(`UPDATE wame_outbox SET `+set+` WHERE message_id != '' AND message_id IN (`+placeholders(len(messageIDs))+`) AND `+
		unchanged+` RETURNING `+outboxColumnsNoText, args...)
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
