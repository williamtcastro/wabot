package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
)

func TestDefaultDB(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses os.UserConfigDir on Windows")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	if got, want := defaultDB(), filepath.Join(home, ".config", "wame", "wame.db"); got != want {
		t.Errorf("defaultDB = %q, want %q", got, want)
	}
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if got, want := defaultDB(), filepath.Join(xdg, "wame", "wame.db"); got != want {
		t.Errorf("with XDG_CONFIG_HOME: %q, want %q", got, want)
	}
	t.Setenv("XDG_CONFIG_HOME", "relative/dir")
	if got, want := defaultDB(), filepath.Join(home, ".config", "wame", "wame.db"); got != want {
		t.Errorf("relative XDG_CONFIG_HOME must be ignored: %q", got)
	}
}

func TestEnvOr(t *testing.T) {
	t.Setenv("WAME_RATE", "")
	if got := envOr("WAME_RATE", "20"); got != "20" {
		t.Errorf("unset: %q", got)
	}
	t.Setenv("WAME_RATE", "9")
	if got := envOr("WAME_RATE", "20"); got != "9" {
		t.Errorf("set: %q", got)
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8080": true, "localhost:8080": true, "[::1]:8080": true,
		":8080": false, "0.0.0.0:8080": false, "192.168.0.10:8080": false, "127.0.0.1": false,
	} {
		if got := isLoopbackAddr(addr); got != want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestAlertFlagsChecked(t *testing.T) {
	c := config{db: filepath.Join(t.TempDir(), "wame.db"), alertGotify: "http://10.0.0.5:8070"}
	if err := run(t.Context(), "token", []string{"list"}, c); err == nil || !strings.Contains(err.Error(), "-alert-gotify") {
		t.Fatalf("a Gotify URL without a token: %v", err)
	}
	c.alertGotify, c.alertGotifyToken = "http://10.0.0.5:8070/?token=A1b2C3d4", "A1b2C3d4"
	if err := run(t.Context(), "token", []string{"list"}, c); err == nil || strings.Contains(err.Error(), "A1b2C3d4") {
		t.Fatalf("a token in the URL: %v", err)
	}
}

func TestNegativeDurationsRefused(t *testing.T) {
	db := filepath.Join(t.TempDir(), "wame.db")
	for flag, c := range map[string]config{
		"-alert-delay":  {db: db, alertDelay: -time.Minute},
		"-queue-jitter": {db: db, queueJitter: -time.Second},
		"-chat-gap":     {db: db, chatGap: -time.Second},
		"-max-wait":     {db: db, maxWait: -time.Second},
	} {
		if err := run(t.Context(), "token", []string{"list"}, c); err == nil || !strings.Contains(err.Error(), flag) {
			t.Errorf("a negative %s: %v", flag, err)
		}
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("checked after opening the database: %v", err)
	}
}

func TestSecretsScrubbed(t *testing.T) {
	c := config{token: "s3cret-owner-secret", webhook: "https://hooks.example.com/x", webhookSecret: "hush",
		alertGotify: "https://gotify.example.com", alertGotifyToken: "A1b2C3d4"}
	logs := bot.NewLogBuffer(10)
	logs.Scrub(c.secrets()...)
	log := slog.New(logs.Wrap(slog.NewTextHandler(io.Discard, nil)))
	log.Error("alert failed", "err", "Post https://gotify.example.com/message: refused, key A1b2C3d4, owner s3cret-owner-secret")
	e := logs.Entries(bot.LogQuery{})
	if len(e) != 1 {
		t.Fatalf("entries = %+v", e)
	}
	for _, leak := range []string{"gotify.example.com", "A1b2C3d4", "s3cret-owner-secret"} {
		if strings.Contains(e[0].Attrs["err"], leak) {
			t.Errorf("%q reached the log buffer: %s", leak, e[0].Attrs["err"])
		}
	}
}

func TestBackupCommandWhileServing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wame.db")
	b, err := bot.New(context.Background(), path, waLog.Noop, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	maria := "5511999999999@s.whatsapp.net"
	if _, _, err := b.Store.Save(store.Message{Chat: maria, ID: "M1", Sender: maria, Type: "text", Text: "hi", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}

	backups := filepath.Join(dir, "backups") + string(filepath.Separator)
	old := []string{store.BackupName(time.Now().Add(-48 * time.Hour)), store.BackupName(time.Now().Add(-24 * time.Hour))}
	if err := os.MkdirAll(backups, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range old {
		if err := os.WriteFile(filepath.Join(backups, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := backupCommand([]string{"-db", path, "-keep", "2", backups}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(backups)
	if err != nil || len(entries) != 2 || entries[0].Name() != old[1] {
		t.Fatalf("backups left: %v, %v", entries, err)
	}
	copyPath := filepath.Join(backups, entries[1].Name())
	if err := backupCommand([]string{"-db", path, copyPath}); err == nil {
		t.Fatal("an existing backup was overwritten")
	}
	if _, err := store.Backup(context.Background(), copyPath, filepath.Join(dir, "again.db")); err != nil {
		t.Fatalf("the copy isn't a valid wame database: %v", err)
	}
	if msgs, err := b.Store.Messages(maria, 10); err != nil || len(msgs) != 1 {
		t.Fatalf("the server's database after the backup: %v, %v", msgs, err)
	}
}

func TestBackupDest(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 13, 9, 30, 5, 0, time.UTC)
	db := filepath.Join(dir, "wame.db")
	for _, c := range []struct{ path, want string }{
		{"", filepath.Join(dir, "wame-20261013-093005.db")},
		{dir, filepath.Join(dir, "wame-20261013-093005.db")},
		{filepath.Join(dir, "new") + "/", filepath.Join(dir, "new", "wame-20261013-093005.db")},
		{filepath.Join(dir, "mine.db"), filepath.Join(dir, "mine.db")},
	} {
		if got, err := backupDest(db, c.path, now); err != nil || got != c.want {
			t.Errorf("backupDest(%q) = %q, %v; want %q", c.path, got, err, c.want)
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, "new")); err != nil || !fi.IsDir() {
		t.Errorf("a trailing slash must create the directory: %v", err)
	}
}

func TestNewLoggerKeepsRecentLogs(t *testing.T) {
	file := filepath.Join(t.TempDir(), "wame.log")
	logs := bot.NewLogBuffer(10)
	log, closer, err := newLogger("json", file, "INFO", true, logs)
	if err != nil {
		t.Fatal(err)
	}
	log.Debug("hidden")
	log.Info("HTTP API listening", "addr", "127.0.0.1:8080")
	closer.Close()
	data, err := os.ReadFile(file)
	if err != nil || !strings.Contains(string(data), `"msg":"HTTP API listening","addr":"127.0.0.1:8080"`) || strings.Contains(string(data), "hidden") {
		t.Fatalf("log file: %s, %v", data, err)
	}
	if e := logs.Entries(bot.LogQuery{}); len(e) != 1 || e[0].Attrs["addr"] != "127.0.0.1:8080" {
		t.Fatalf("buffer: %+v", e)
	}

	quiet := bot.NewLogBuffer(10)
	log, _, _ = newLogger("text", "", "INFO", true, quiet)
	log.Warn("from the tui")
	if e := quiet.Entries(bot.LogQuery{}); len(e) != 1 {
		t.Fatalf("quiet modes discard the output but keep the buffer: %+v", e)
	}
}

func TestByteSize(t *testing.T) {
	for n, want := range map[int64]string{512: "512 B", 2048: "2.0 KiB", 5 << 20: "5.0 MiB"} {
		if got := byteSize(n); got != want {
			t.Errorf("byteSize(%d) = %q, want %q", n, got, want)
		}
	}
}
