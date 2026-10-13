//go:build unix

package termui

import (
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWriteNew(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contacts.csv")
	if err := WriteNew(path, []byte("number\n5511999999999\n")); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v %v", fi, err)
	}
	if err := WriteNew(path, []byte("x")); !errors.Is(err, os.ErrExist) {
		t.Fatalf("an existing file: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "number\n5511999999999\n" {
		t.Fatalf("the existing file changed: %q", data)
	}

	// A write cut short by the file size limit must not leave half a file behind.
	signal.Ignore(syscall.SIGXFSZ)
	defer signal.Reset(syscall.SIGXFSZ)
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Skip(err)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 8, Max: old.Max}); err != nil {
		t.Skip(err)
	}
	partial := filepath.Join(t.TempDir(), "partial.csv")
	err := WriteNew(partial, make([]byte, 64))
	if serr := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old); serr != nil {
		t.Fatal(serr)
	}
	if err == nil {
		t.Fatal("a write past the size limit succeeded")
	}
	if _, err := os.Stat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the partial file is still there: %v", err)
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	for in, want := range map[string]string{"~/a.csv": filepath.Join(home, "a.csv"), "a.csv": "a.csv", "~a": "~a", "/x/~/a": "/x/~/a"} {
		if got := ExpandHome(in); got != want {
			t.Errorf("ExpandHome(%q) = %q, want %q", in, got, want)
		}
	}
}
