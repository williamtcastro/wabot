package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Kinds of queued files; a text item has none.
const (
	FileImage    = "image"
	FileDocument = "document"
)

// OutboxFile is a file uploaded for the send queue. The bytes live on disk under the server's own
// id; Name is display metadata only. Upload holds the cached WhatsApp upload (media key included)
// and never leaves the server.
type OutboxFile struct {
	ID         string    `json:"id"`
	Actor      string    `json:"actor"`
	TokenID    string    `json:"-"`
	SHA256     string    `json:"sha256"`
	Size       int64     `json:"size"`
	Mime       string    `json:"mime"`
	Kind       string    `json:"kind"`
	Name       string    `json:"name,omitempty"`
	Created    time.Time `json:"created_at"`
	Upload     []byte    `json:"-"`
	UploadedAt time.Time `json:"-"`
	Deleted    time.Time `json:"deleted_at,omitzero"`
	Items      int       `json:"items"`
	Open       int       `json:"open"`
}

// OutboxFileUsage is how many live files there are and how many bytes they take.
type OutboxFileUsage struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

// OutboxFileQuota caps live files per actor and the bytes of all of them together.
type OutboxFileQuota struct {
	ActorFiles int
	ActorBytes int64
	TotalBytes int64
}

var (
	// ErrOutboxFileQuota is InsertOutboxFile's answer when the file would go over a quota.
	ErrOutboxFileQuota = errors.New("queue file quota exceeded")
	// ErrOutboxFileInUse is DeleteOutboxFile's answer while queued items still need the file.
	ErrOutboxFileInUse = errors.New("queue file is used by queued messages")
)

// fileGoneMessage is raised by the wame_outbox_file_live trigger.
const fileGoneMessage = "wame: queue file is gone"

// IsOutboxFileGone reports whether err is Enqueue refusing an item whose file was deleted (or
// belongs to someone else) by the time the transaction ran.
func IsOutboxFileGone(err error) bool {
	return err != nil && strings.Contains(err.Error(), fileGoneMessage)
}

// outboxFileColumns are wame_outbox's file reference; kind and size are copied from the file so
// every reader of an item (events, webhook, lists) has them without a join.
var outboxFileColumns = []string{"file_id TEXT NOT NULL DEFAULT ''", "file_kind TEXT NOT NULL DEFAULT ''", "file_size INTEGER NOT NULL DEFAULT 0"}

func migrateOutboxFiles(ctx context.Context, db *sql.DB) error {
	if err := addColumns(ctx, db, "wame_outbox", outboxFileColumns); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS wame_outbox_files (
	id          TEXT PRIMARY KEY,
	actor       TEXT NOT NULL,
	token_id    TEXT NOT NULL DEFAULT '',
	sha256      TEXT NOT NULL,
	size        INTEGER NOT NULL,
	mime        TEXT NOT NULL,
	kind        TEXT NOT NULL,
	name        TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	upload      BLOB,
	uploaded_at INTEGER NOT NULL DEFAULT 0,
	deleted_at  INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS wame_outbox_files_dedupe ON wame_outbox_files (actor, sha256) WHERE deleted_at = 0;
CREATE INDEX IF NOT EXISTS wame_outbox_file ON wame_outbox (file_id) WHERE file_id != '';
CREATE TRIGGER IF NOT EXISTS wame_outbox_file_live BEFORE INSERT ON wame_outbox
WHEN NEW.file_id != ''
	AND NOT EXISTS (SELECT 1 FROM wame_outbox_files f WHERE f.id = NEW.file_id AND f.actor = NEW.actor AND f.deleted_at = 0)
	AND NOT EXISTS (SELECT 1 FROM wame_outbox o WHERE NEW.key != '' AND o.actor = NEW.actor AND o.key = NEW.key)
BEGIN SELECT RAISE(ABORT, '`+fileGoneMessage+`'); END;`)
	return err
}

const outboxFileCols = `id, actor, token_id, sha256, size, mime, kind, name, created_at, upload, uploaded_at, deleted_at`

// outboxFileCounts adds each file's items and open (queued or sending) items.
const outboxFileCounts = `,
	(SELECT COUNT(*) FROM wame_outbox o WHERE o.file_id = f.id),
	(SELECT COUNT(*) FROM wame_outbox o WHERE o.file_id = f.id AND o.status IN ('` + OutboxQueued + `', '` + OutboxSending + `'))`

func scanOutboxFile(row interface{ Scan(...any) error }, counts bool) (OutboxFile, error) {
	var f OutboxFile
	var created, uploaded, deleted int64
	dest := []any{&f.ID, &f.Actor, &f.TokenID, &f.SHA256, &f.Size, &f.Mime, &f.Kind, &f.Name, &created, &f.Upload, &uploaded, &deleted}
	if counts {
		dest = append(dest, &f.Items, &f.Open)
	}
	err := row.Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	f.Created, f.UploadedAt, f.Deleted = time.UnixMilli(created), unixMilliOrZero(uploaded), unixMilliOrZero(deleted)
	return f, err
}

func (s *Store) liveOutboxFile(actor, sha string) (OutboxFile, error) {
	return scanOutboxFile(s.db.QueryRow(`SELECT `+outboxFileCols+outboxFileCounts+` FROM wame_outbox_files f
		WHERE actor = ? AND sha256 = ? AND deleted_at = 0`, actor, sha), true)
}

// InsertOutboxFile stores f's row unless the actor already has a live file with the same sha256,
// which is returned instead (existed true). The quota is checked by the INSERT itself, so two
// uploads at once can't both squeeze under it; going over is ErrOutboxFileQuota.
func (s *Store) InsertOutboxFile(f OutboxFile, q OutboxFileQuota) (_ OutboxFile, existed bool, err error) {
	if old, err := s.liveOutboxFile(f.Actor, f.SHA256); err == nil {
		return old, true, nil
	} else if !errors.Is(err, ErrNotFound) {
		return f, false, err
	}
	now := time.Now().UnixMilli()
	got, err := scanOutboxFile(s.db.QueryRow(`INSERT INTO wame_outbox_files (id, actor, token_id, sha256, size, mime, kind, name, created_at)
		SELECT ?,?,?,?,?,?,?,?,? WHERE
			(SELECT COUNT(*) FROM wame_outbox_files WHERE actor = ? AND deleted_at = 0) < ?
			AND (SELECT COALESCE(SUM(size), 0) FROM wame_outbox_files WHERE actor = ? AND deleted_at = 0) + ? <= ?
			AND (SELECT COALESCE(SUM(size), 0) FROM wame_outbox_files WHERE deleted_at = 0) + ? <= ?
		ON CONFLICT DO NOTHING RETURNING `+outboxFileCols,
		f.ID, f.Actor, f.TokenID, f.SHA256, f.Size, f.Mime, f.Kind, f.Name, now,
		f.Actor, q.ActorFiles, f.Actor, f.Size, q.ActorBytes, f.Size, q.TotalBytes), false)
	if errors.Is(err, ErrNotFound) {
		// Nothing inserted: the same file from a parallel upload, or over quota.
		if old, err := s.liveOutboxFile(f.Actor, f.SHA256); err == nil {
			return old, true, nil
		}
		return f, false, ErrOutboxFileQuota
	}
	return got, false, err
}

// OutboxFile returns one file's row, deleted or not, with its item counts.
func (s *Store) OutboxFile(id string) (OutboxFile, error) {
	return scanOutboxFile(s.db.QueryRow(`SELECT `+outboxFileCols+outboxFileCounts+` FROM wame_outbox_files f WHERE id = ?`, id), true)
}

// ListOutboxFiles returns live files, newest first: actor's own, or everyone's for "".
func (s *Store) ListOutboxFiles(actor string) ([]OutboxFile, error) {
	rows, err := s.db.Query(`SELECT `+outboxFileCols+outboxFileCounts+` FROM wame_outbox_files f
		WHERE deleted_at = 0 AND (? = '' OR actor = ?) ORDER BY created_at DESC, id`, actor, actor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OutboxFile{}
	for rows.Next() {
		f, err := scanOutboxFile(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// OutboxFileUsage counts live files: actor's own, or everyone's for "".
func (s *Store) OutboxFileUsage(actor string) (OutboxFileUsage, error) {
	var u OutboxFileUsage
	err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM wame_outbox_files WHERE deleted_at = 0 AND (? = '' OR actor = ?)`,
		actor, actor).Scan(&u.Files, &u.Bytes)
	return u, err
}

// SetOutboxUpload caches (or, with nil, drops) a live file's WhatsApp upload.
func (s *Store) SetOutboxUpload(id string, upload []byte, at time.Time) error {
	ms := int64(0)
	if upload != nil {
		ms = at.UnixMilli()
	}
	_, err := s.db.Exec(`UPDATE wame_outbox_files SET upload = ?, uploaded_at = ? WHERE id = ? AND deleted_at = 0`, upload, ms, id)
	return err
}

// forgetFile is what deleting a file leaves of its row: id, actor, size, sha256, mime and kind,
// so the items that used it still say what was sent.
const forgetFile = `UPDATE wame_outbox_files SET deleted_at = ?, name = '', upload = NULL, uploaded_at = 0 WHERE deleted_at = 0`

const noOpenItems = ` AND NOT EXISTS (SELECT 1 FROM wame_outbox o WHERE o.file_id = wame_outbox_files.id AND o.status IN ('` +
	OutboxQueued + `', '` + OutboxSending + `'))`

func (s *Store) forgetFiles(where string, args ...any) ([]string, error) {
	return s.queryStrings(forgetFile+where+` RETURNING id`, append([]any{time.Now().UnixMilli()}, args...)...)
}

// DeleteOutboxFile marks a live file deleted (actor "" for any actor's) in one statement that also
// checks no queued or sending item needs it: ErrOutboxFileInUse when one does, ErrNotFound when
// there's no such live file of actor's.
func (s *Store) DeleteOutboxFile(id, actor string) error {
	ids, err := s.forgetFiles(` AND id = ? AND (? = '' OR actor = ?)`+noOpenItems, id, actor, actor)
	if err != nil || len(ids) > 0 {
		return err
	}
	f, err := s.OutboxFile(id)
	switch {
	case err != nil:
		return err
	case !f.Deleted.IsZero() || (actor != "" && f.Actor != actor):
		return ErrNotFound
	}
	return ErrOutboxFileInUse
}

// ExpireOutboxFiles deletes the live files nothing open needs: those whose items all finished
// before done, and those never used by any item that were uploaded before unused. It returns
// their ids.
func (s *Store) ExpireOutboxFiles(done, unused time.Time) ([]string, error) {
	return s.forgetFiles(noOpenItems+` AND (
		(NOT EXISTS (SELECT 1 FROM wame_outbox o WHERE o.file_id = wame_outbox_files.id) AND created_at < ?)
		OR (SELECT MAX(o.updated_at) FROM wame_outbox o WHERE o.file_id = wame_outbox_files.id) < ?)`,
		unused.UnixMilli(), done.UnixMilli())
}

// DeleteOutboxFilesOf deletes every live file uploaded with the token, needed or not.
func (s *Store) DeleteOutboxFilesOf(tokenID string) ([]string, error) {
	return s.forgetFiles(` AND token_id = ?`, tokenID)
}

// WipeOutboxFiles deletes every live file (logout) and returns how many there were.
func (s *Store) WipeOutboxFiles() (int, error) {
	ids, err := s.forgetFiles(``)
	return len(ids), err
}

// OutboxFileTokens lists the tokens that uploaded live files.
func (s *Store) OutboxFileTokens() ([]string, error) {
	return s.queryStrings(`SELECT DISTINCT token_id FROM wame_outbox_files WHERE deleted_at = 0 AND token_id != ''`)
}

// LiveOutboxFiles lists the ids of the files that should be on disk.
func (s *Store) LiveOutboxFiles() ([]string, error) {
	return s.queryStrings(`SELECT id FROM wame_outbox_files WHERE deleted_at = 0`)
}

func (s *Store) queryStrings(query string, args ...any) ([]string, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// PruneOutboxFiles removes the rows of deleted files no item refers to any more.
func (s *Store) PruneOutboxFiles() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM wame_outbox_files WHERE deleted_at != 0
		AND NOT EXISTS (SELECT 1 FROM wame_outbox o WHERE o.file_id = wame_outbox_files.id)`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
