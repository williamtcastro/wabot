package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"
)

// Why EnqueueWith skipped an item.
const (
	SkipDuplicate    = "duplicate"
	SkipRecentlySent = "recently_sent"
)

// ErrKeyTaken means a key in an item's EnqueueRow.Taken is already stored: the request that reuses
// these keys differs in shape from the one that stored them.
var ErrKeyTaken = errors.New("idempotency key already used for another kind of row")

// EnqueueOptions are Enqueue's extras. With SkipSent, Dedupe or any AltKeys each keyed item is
// first looked up (a replay is returned as before), then checked against SkipSent, then Dedupe, and
// only then inserted; the transaction takes the write lock before reading, so two requests at once
// can't both pass a check.
type EnqueueOptions struct {
	Check EnqueueCheck
	// Rows[i] are items[i]'s extras; missing ones are empty.
	Rows []EnqueueRow
	// SkipSent skips an item whose recipient already got, or is about to get, a message; nil is off.
	SkipSent *SkipSent
	// Dedupe skips an item whose recipient, file and text were already queued (or replayed) by this call.
	Dedupe bool
}

// EnqueueRow is what EnqueueWith knows about one item beyond the item itself.
type EnqueueRow struct {
	// Recipients are the keys the recipient may be stored under (its RecipientKey, the 9th-digit
	// variant, its @lid); empty means RecipientKey alone.
	Recipients []string
	// AltKeys are other idempotency keys an earlier run may have stored the item under (a tag
	// member keyed by another form of its number). The item's own Key wins.
	AltKeys []string
	// Taken are keys that mustn't be stored already when the item is new (ErrKeyTaken): exact keys,
	// or prefixes when they end in ':'.
	Taken []string
}

// SkipSent says which earlier messages make EnqueueWith skip an item. Items count while queued or
// sending, and once sent when that was since Since; with Batch, every queued, sending or sent item
// of that batch counts instead, whatever its age. Failed and canceled items never count.
type SkipSent struct {
	Since time.Time
	Batch string
	// Actor limits the items that count to this actor's; "" counts everyone's.
	Actor string
	// Messages also counts messages sent to the recipient since Since (wame_messages, from_me),
	// whoever sent them: the phone, /send or the queue.
	Messages bool
}

func (o EnqueueOptions) lookupFirst() bool {
	return o.SkipSent != nil || o.Dedupe || slices.ContainsFunc(o.Rows, func(r EnqueueRow) bool { return len(r.AltKeys) > 0 })
}

func (o EnqueueOptions) row(i int) EnqueueRow {
	if i < len(o.Rows) {
		return o.Rows[i]
	}
	return EnqueueRow{}
}

// taken returns ErrKeyTaken when one of the item's Taken keys is stored for its actor.
func (o EnqueueOptions) taken(tx *sql.Tx, i int, it OutboxItem) error {
	for _, k := range o.row(i).Taken {
		q, args := `SELECT 1 FROM wame_outbox WHERE actor = ? AND key != '' AND key = ?`, []any{it.Actor, k}
		if strings.HasSuffix(k, ":") {
			// key >= 'k#0:' AND key < 'k#0;' is every key with that prefix, through the index.
			q, args = `SELECT 1 FROM wame_outbox WHERE actor = ? AND key != '' AND key >= ? AND key < ?`, []any{it.Actor, k, k[:len(k)-1] + ";"}
		}
		found, err := exists(tx, q+` LIMIT 1`, args...)
		if err != nil {
			return err
		}
		if found {
			return ErrKeyTaken
		}
	}
	return nil
}

// outboxWriteLock takes the database's write lock, so what the transaction reads next can't change
// before it writes.
func outboxWriteLock(tx *sql.Tx) error {
	_, err := tx.Exec(`DELETE FROM wame_outbox WHERE 0`)
	return err
}

func (o EnqueueOptions) recipients(i int, it OutboxItem) []string {
	if r := o.row(i).Recipients; len(r) > 0 {
		return r
	}
	if it.RecipientKey != "" {
		return []string{it.RecipientKey}
	}
	return nil
}

// stored returns the item already stored under it's key (or one of its AltKeys), if any.
func (o EnqueueOptions) stored(tx *sql.Tx, i int, it OutboxItem) (OutboxItem, bool, error) {
	if it.Key == "" {
		return OutboxItem{}, false, nil
	}
	keys := append([]string{it.Key}, o.row(i).AltKeys...)
	stored, err := scanOutbox(tx.QueryRow(`SELECT `+outboxColumns+` FROM wame_outbox WHERE actor = ? AND key != '' AND key IN (`+placeholders(len(keys))+`)
		ORDER BY key = ? DESC LIMIT 1`, append(append([]any{it.Actor}, anys(keys)...), it.Key)...))
	if errors.Is(err, ErrNotFound) {
		return OutboxItem{}, false, nil
	}
	return stored, err == nil, err
}

// skip says why it must not be stored, or "".
func (o EnqueueOptions) skip(tx *sql.Tx, i int, it OutboxItem, seen map[string]bool) (string, error) {
	keys := o.recipients(i, it)
	if len(keys) == 0 {
		return "", nil
	}
	if o.SkipSent != nil {
		sent, err := o.SkipSent.match(tx, keys)
		if err != nil || sent {
			return SkipRecentlySent, err
		}
	}
	if o.Dedupe {
		for _, k := range keys {
			if seen[k+"\x00"+it.FileID+"\x00"+it.Text] {
				return SkipDuplicate, nil
			}
		}
	}
	return "", nil
}

// mark remembers it as queued by this call, for Dedupe.
func (o EnqueueOptions) mark(i int, it OutboxItem, seen map[string]bool) {
	if !o.Dedupe {
		return
	}
	for _, k := range o.recipients(i, it) {
		seen[k+"\x00"+it.FileID+"\x00"+it.Text] = true
	}
}

func (s SkipSent) match(tx *sql.Tx, keys []string) (bool, error) {
	q := `SELECT 1 FROM wame_outbox WHERE recipient_key != '' AND recipient_key IN (` + placeholders(len(keys)) + `)`
	args := anys(keys)
	if s.Batch != "" {
		q += ` AND batch = ? AND status IN (?, ?, ?)`
		args = append(args, s.Batch, OutboxQueued, OutboxSending, OutboxSent)
	} else {
		q += ` AND (status IN (?, ?) OR (status = ? AND updated_at >= ?))`
		args = append(args, OutboxQueued, OutboxSending, OutboxSent, s.Since.UnixMilli())
	}
	if s.Actor != "" {
		q += ` AND actor = ?`
		args = append(args, s.Actor)
	}
	found, err := exists(tx, q+` LIMIT 1`, args...)
	if found || err != nil || !s.Messages || s.Batch != "" {
		return found, err
	}
	return exists(tx, `SELECT 1 FROM wame_messages WHERE chat IN (`+placeholders(len(keys))+`) AND from_me = 1 AND ts >= ? AND type <> 'reaction' LIMIT 1`,
		append(anys(keys), s.Since.UnixMilli())...)
}

func exists(tx *sql.Tx, query string, args ...any) (bool, error) {
	var one int
	err := tx.QueryRow(query, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// migrateOutboxRecipients indexes recipient_key and fills it on items stored before it existed with
// the chat they were sent to (items never sent keep it empty: skip_sent can't match them).
func migrateOutboxRecipients(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE INDEX IF NOT EXISTS wame_outbox_recipient ON wame_outbox (recipient_key, updated_at) WHERE recipient_key != '';
UPDATE wame_outbox SET recipient_key = chat WHERE recipient_key = '' AND chat != '';`)
	return err
}
