package store

import (
	"context"
	"database/sql"
	"time"
)

// AuditEntry is one control action: who did what to which chat, and whether it worked.
type AuditEntry struct {
	ID     int64     `json:"id"`
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Target string    `json:"target,omitempty"`
	Detail string    `json:"detail,omitempty"`
	OK     bool      `json:"ok"`
	Error  string    `json:"error,omitempty"`
}

func migrateAudit(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS wabot_audit (
	id     INTEGER PRIMARY KEY AUTOINCREMENT,
	at     INTEGER NOT NULL,
	actor  TEXT NOT NULL,
	action TEXT NOT NULL,
	target TEXT NOT NULL DEFAULT '',
	detail TEXT NOT NULL DEFAULT '',
	ok     INTEGER NOT NULL,
	error  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS wabot_audit_at ON wabot_audit (at);`)
	return err
}

// Audit inserts e; its ID is ignored.
func (s *Store) Audit(e AuditEntry) error {
	_, err := s.db.Exec(`INSERT INTO wabot_audit (at, actor, action, target, detail, ok, error) VALUES (?,?,?,?,?,?,?)`,
		e.At.UnixMilli(), e.Actor, e.Action, e.Target, e.Detail, e.OK, e.Error)
	return err
}

// AuditLog returns the newest entries first; before is an entry id cursor (0 for the latest).
func (s *Store) AuditLog(limit int, before int64) ([]AuditEntry, error) {
	if before <= 0 {
		before = 1<<63 - 1
	}
	rows, err := s.db.Query(`SELECT id, at, actor, action, target, detail, ok, error FROM wabot_audit
		WHERE id < ? ORDER BY id DESC LIMIT ?`, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var at int64
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.Action, &e.Target, &e.Detail, &e.OK, &e.Error); err != nil {
			return nil, err
		}
		e.At = time.UnixMilli(at)
		out = append(out, e)
	}
	return out, rows.Err()
}
