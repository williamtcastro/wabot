package admin

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const DefaultServer = "http://127.0.0.1:8080"

// Source is one place the server URL and token can come from. Sources are passed in order of
// precedence; File marks an env file rather than flags or the environment.
type Source struct {
	Name     string
	Server   string
	Token    string
	Insecure bool
	File     bool
}

// Resolve picks the server and token. A server read from a file is only used together with a
// token from that same file, so a stray .env can't redirect a token from elsewhere.
func Resolve(sources []Source) (server, token string, insecure bool, err error) {
	var srv, tok *Source
	for i := range sources {
		s := &sources[i]
		if srv == nil && s.Server != "" {
			srv = s
		}
		if tok == nil && s.Token != "" {
			tok = s
		}
	}
	if tok != nil {
		token = tok.Token
	}
	if srv == nil {
		return DefaultServer, token, false, nil
	}
	if srv.File && tok != nil && srv != tok {
		return "", "", false, fmt.Errorf("WAME_SERVER comes from %s but the token doesn't; refusing to send it there. Put both in the same file, or pass -server", srv.Name)
	}
	return srv.Server, token, srv.Insecure, nil
}

// Profile is a saved server: ~/.config/wame/servers/NAME.env.
type Profile struct {
	Name string
	Source
}

// Profiles lists the env files in dir (normally ~/.config/wame/servers), sorted by name. Files
// without both a server and a token are skipped.
func Profiles(dir string) ([]Profile, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Profile
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".env")
		if !ok || e.IsDir() || name == "" {
			continue
		}
		src, err := ReadEnvFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		if src.Server != "" && src.Token != "" {
			out = append(out, Profile{Name: name, Source: src})
		}
	}
	return out, nil
}

// ReadEnvFile reads WAME_SERVER, WAME_TOKEN and WAME_INSECURE from a KEY=VALUE file. A missing file is no error.
func ReadEnvFile(path string) (Source, error) {
	src := Source{Name: path, File: true}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return src, nil
	}
	if err != nil {
		return src, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		value = strings.TrimSpace(value)
		if n := len(value); n >= 2 && (value[0] == '"' || value[0] == '\'') && value[n-1] == value[0] {
			value = value[1 : n-1]
		}
		switch strings.TrimSpace(key) {
		case "WAME_SERVER":
			src.Server = value
		case "WAME_TOKEN":
			src.Token = value
		case "WAME_INSECURE":
			src.Insecure = value == "1" || strings.EqualFold(value, "true")
		}
	}
	return src, sc.Err()
}
