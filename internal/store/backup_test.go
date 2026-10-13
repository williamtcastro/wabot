package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBackupWhileTheServerHasItOpen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "wame.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	for i, chat := range []string{maria, john} {
		if _, _, err := s.Save(Message{Chat: chat, ID: fmt.Sprint("B", i), Sender: chat, Type: "text", Text: "hello", Timestamp: time.Unix(1000, 0)}); err != nil {
			t.Fatal(err)
		}
	}
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal mode = %q, %v", mode, err)
	}
	if _, err := os.Stat(path + "-wal"); err != nil {
		t.Fatalf("the server's WAL file should exist: %v", err)
	}

	// A write in progress isn't in the copy and doesn't block it.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO wame_messages (chat, id, sender, from_me, is_group, type, text, ts) VALUES (?, 'B3', ?, 0, 0, 'text', 'x', 1)`, ana, ana); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(dir, "copy.db")
	size, err := Backup(ctx, path, dest)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dest)
	if err != nil || fi.Size() != size || size == 0 {
		t.Fatalf("copy: %v, size %d", err, size)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("copy mode = %v, want 0600", fi.Mode().Perm())
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(dest + suffix); err == nil {
			t.Errorf("the copy left a %s file", suffix)
		}
	}

	copyDB, err := openReadOnly(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	var n int
	if err := copyDB.QueryRow(`SELECT count(*) FROM wame_messages`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("copy has %d messages (%v), want 2", n, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("the server's write failed after the backup: %v", err)
	}
	if got, err := s.Messages(maria, 10); err != nil || len(got) != 1 {
		t.Fatalf("source after backup: %v, %v", got, err)
	}
}

func TestBackupRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wame.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Backup(context.Background(), path, filepath.Join(dir, "first.db")); err != nil {
		t.Fatalf("backup of a WAL database nobody has open: %v", err)
	}

	dest := filepath.Join(dir, "existing.db")
	if err := os.WriteFile(dest, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Backup(context.Background(), path, dest); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("overwrite: %v", err)
	}
	if data, _ := os.ReadFile(dest); string(data) != "keep me" {
		t.Fatal("the existing file was changed")
	}
	if _, err := Backup(context.Background(), filepath.Join(dir, "missing.db"), filepath.Join(dir, "x.db")); err == nil {
		t.Fatal("a missing database was backed up")
	}
	if _, err := os.Stat(filepath.Join(dir, "missing.db")); err == nil {
		t.Fatal("backing up a missing database created it")
	}
}

func TestBackupPathNeedsEscaping(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "odd #1 ?x=%41")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "wame.db")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, OmitHost: true}).String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Backup(context.Background(), path, filepath.Join(dir, "copy.db")); err != nil {
		t.Fatalf("backup from a path with '#', '?' and '%%': %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "odd ")); err == nil {
		t.Fatal("the path was cut at '#'")
	}
}

func TestBackupRejectsAForeignDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "other.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE notes (x TEXT)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	dest := filepath.Join(dir, "copy.db")
	if _, err := Backup(context.Background(), path, dest); err == nil || !strings.Contains(err.Error(), "wame_messages") {
		t.Fatalf("foreign database: %v", err)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("a failed backup must not leave its file behind")
	}
}

func TestPruneBackups(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	var made []string
	for i := range 4 {
		name := BackupName(base.Add(time.Duration(i) * time.Hour))
		made = append(made, name)
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	others := []string{"wame.db", "wame-20260101-000000.db.bak", "wame-latest.db", "notes.txt"}
	for _, name := range others {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := PruneBackups(dir, 2)
	if err != nil || len(removed) != 2 || filepath.Base(removed[0]) != made[0] || filepath.Base(removed[1]) != made[1] {
		t.Fatalf("removed %v, %v", removed, err)
	}
	entries, _ := os.ReadDir(dir)
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	for _, name := range append(others, made[2], made[3]) {
		if !slices.Contains(left, name) {
			t.Errorf("%s was deleted", name)
		}
	}
	if removed, err := PruneBackups(dir, 5); err != nil || len(removed) != 0 {
		t.Fatalf("keeping more than there are: %v, %v", removed, err)
	}
}
