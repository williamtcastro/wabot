//go:build unix

package admin

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReadImportRefusesAFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contacts.csv")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skip(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readImport(path)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "isn't a regular file") {
			t.Fatalf("a FIFO: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO blocks")
	}
}
