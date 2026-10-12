package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
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
	old := filepath.Join(xdg, "wabot", "wabot.db")
	if err := os.MkdirAll(filepath.Dir(old), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if got := defaultDB(); got != old {
		t.Errorf("a pre-rename database must be picked up when there's no new one: %q", got)
	}
}

func TestEnvFallsBackToOldNames(t *testing.T) {
	t.Setenv("WAME_RATE", "")
	t.Setenv("WABOT_RATE", "7")
	if got := envOr("WAME_RATE", "20"); got != "7" {
		t.Errorf("WABOT_RATE fallback = %q", got)
	}
	t.Setenv("WAME_RATE", "9")
	if got := envOr("WAME_RATE", "20"); got != "9" {
		t.Errorf("WAME_RATE must win: %q", got)
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
