package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	_ "modernc.org/sqlite"
)

var backupName = regexp.MustCompile(`^wame-\d{8}-\d{6}\.db$`)

// BackupName is the file name `wame backup` gives a copy made at t.
func BackupName(t time.Time) string { return "wame-" + t.Format("20060102-150405") + ".db" }

// Backup copies the database at dbPath into dest, a file that must not exist yet, and checks the copy.
// The source is opened read-only through the same absolute path the server uses, so it's safe while
// a server has the database open. It returns the size of the copy.
func Backup(ctx context.Context, dbPath, dest string) (_ int64, err error) {
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return 0, err
	}
	if dest, err = filepath.Abs(dest); err != nil {
		return 0, err
	}
	if fi, err := os.Stat(abs); err != nil {
		return 0, fmt.Errorf("no database at %s: %w", abs, err)
	} else if !fi.Mode().IsRegular() {
		return 0, fmt.Errorf("%s isn't a database file", abs)
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return 0, fmt.Errorf("%s already exists; refusing to overwrite it", dest)
		}
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(dest)
		}
	}()

	src, err := openReadOnly(abs)
	if err != nil {
		return 0, err
	}
	defer src.Close()
	if _, err := src.ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		return 0, fmt.Errorf("copy the database: %w", err)
	}
	if err := checkBackup(ctx, dest); err != nil {
		return 0, err
	}
	fi, err := os.Stat(dest)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func openReadOnly(path string) (*sql.DB, error) {
	dsn := url.URL{Scheme: "file", Path: filepath.ToSlash(path), OmitHost: true, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func checkBackup(ctx context.Context, path string) error {
	db, err := openReadOnly(path)
	if err != nil {
		return err
	}
	defer db.Close()
	var result string
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&result); err != nil {
		return fmt.Errorf("check the copy: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("the copy failed its integrity check: %s", result)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'wame_messages'`).Scan(&n); err != nil {
		return fmt.Errorf("check the copy: %w", err)
	}
	if n == 0 {
		return errors.New("the copy has no wame_messages table: is that a wame database?")
	}
	return nil
}

// PruneBackups deletes the oldest backups in dir beyond the newest keep. Only files named like
// BackupName are touched; it returns the paths it deleted.
func PruneBackups(dir string, keep int) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && backupName.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	var removed []string
	for _, name := range names[:max(len(names)-keep, 0)] {
		p := filepath.Join(dir, name)
		if err := os.Remove(p); err != nil {
			return removed, err
		}
		removed = append(removed, p)
	}
	return removed, nil
}
