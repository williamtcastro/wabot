package admin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	body := "# comment\nWAME_SERVER=https://wame.example.com\nexport WAME_TOKEN=\"wame_abc\"\nWAME_WEBHOOK=https://other\nnot a line\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	src, err := ReadEnvFile(path)
	if err != nil || src.Server != "https://wame.example.com" || src.Token != "wame_abc" || !src.File {
		t.Fatalf("got %+v, %v", src, err)
	}
	if src, err := ReadEnvFile(filepath.Join(t.TempDir(), "missing")); err != nil || src.Server != "" {
		t.Fatalf("missing file: %+v, %v", src, err)
	}
}

func TestResolve(t *testing.T) {
	flags := func(s, tok string) Source { return Source{Name: "flags", Server: s, Token: tok} }
	env := func(s, tok string) Source { return Source{Name: "environment", Server: s, Token: tok} }
	file := func(s, tok string) Source { return Source{Name: ".env", Server: s, Token: tok, File: true} }
	home := func(s, tok string) Source { return Source{Name: "admin.env", Server: s, Token: tok, File: true} }

	for _, c := range []struct {
		name        string
		sources     []Source
		server, tok string
		fails       bool
	}{
		{"both from .env", []Source{flags("", ""), env("", ""), file("https://a", "t1")}, "https://a", "t1", false},
		{"env wins over files", []Source{flags("", ""), env("https://e", "t2"), file("https://a", "t1")}, "https://e", "t2", false},
		{"flags win", []Source{flags("https://f", "t3"), env("https://e", "t2")}, "https://f", "t3", false},
		{"flag server with a file token", []Source{flags("https://f", ""), env("", ""), file("https://a", "t1")}, "https://f", "t1", false},
		{"nothing set", []Source{flags("", ""), env("", "")}, DefaultServer, "", false},
		{"token only, default server", []Source{flags("", ""), env("", "t2")}, DefaultServer, "t2", false},
		{".env server, no token anywhere", []Source{flags("", ""), env("", ""), file("https://a", "")}, "https://a", "", false},
		{".env server with the env token", []Source{flags("", ""), env("", "t2"), file("https://evil", "")}, "", "", true},
		{".env server with a home token", []Source{flags("", ""), env("", ""), file("https://evil", ""), home("https://h", "t4")}, "", "", true},
		{"home file alone", []Source{flags("", ""), env("", ""), file("", ""), home("https://h", "t4")}, "https://h", "t4", false},
	} {
		server, tok, err := Resolve(c.sources)
		if (err != nil) != c.fails || (!c.fails && (server != c.server || tok != c.tok)) {
			t.Errorf("%s: got %q, %q, %v", c.name, server, tok, err)
		}
	}
}
