package main

import (
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
	if got, want := defaultDB(), filepath.Join(home, ".config", "wabot", "wabot.db"); got != want {
		t.Errorf("defaultDB = %q, want %q", got, want)
	}
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if got, want := defaultDB(), filepath.Join(xdg, "wabot", "wabot.db"); got != want {
		t.Errorf("with XDG_CONFIG_HOME: %q, want %q", got, want)
	}
	t.Setenv("XDG_CONFIG_HOME", "relative/dir")
	if got, want := defaultDB(), filepath.Join(home, ".config", "wabot", "wabot.db"); got != want {
		t.Errorf("relative XDG_CONFIG_HOME must be ignored: %q", got)
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
