package main

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
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

// Audit records a control action in the wabot_audit table and the log. Message text is never stored.
func (b *Bot) Audit(actor, action, target, detail string, err error) {
	e := AuditEntry{At: time.Now(), Actor: actor, Action: action, Target: target, Detail: detail, OK: err == nil}
	if err != nil {
		e.Error = err.Error()
	}
	_, dbErr := b.db.Exec(`INSERT INTO wabot_audit (at, actor, action, target, detail, ok, error) VALUES (?,?,?,?,?,?,?)`,
		e.At.UnixMilli(), e.Actor, e.Action, e.Target, e.Detail, e.OK, e.Error)
	attrs := []any{"actor", actor, "action", action, "target", target, "detail", detail}
	switch {
	case dbErr != nil:
		b.Log.Error("audit write failed", append(attrs, "err", dbErr)...)
	case err != nil:
		b.Log.Warn("action failed", append(attrs, "err", err)...)
	default:
		b.Log.Info("action", attrs...)
	}
}

// AuditLog returns the newest entries first; before is an entry id cursor (0 for the latest).
func (b *Bot) AuditLog(limit int, before int64) ([]AuditEntry, error) {
	if before <= 0 {
		before = 1<<63 - 1
	}
	rows, err := b.db.Query(`SELECT id, at, actor, action, target, detail, ok, error FROM wabot_audit
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

// NewLogger builds the app logger. format is "text" or "json"; file "" means stderr.
func NewLogger(format, file, level string, quiet bool) (*slog.Logger, io.Closer, error) {
	var w io.Writer = os.Stderr
	var closer io.Closer = io.NopCloser(nil)
	switch {
	case file != "":
		f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, nil, err
		}
		w, closer = f, f
	case quiet:
		w = io.Discard
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts)), closer, nil
	}
	return slog.New(slog.NewTextHandler(w, opts)), closer, nil
}
