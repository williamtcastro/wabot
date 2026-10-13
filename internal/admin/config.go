package admin

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

const DefaultServer = "http://127.0.0.1:8080"

// Source is one place the server URL and token can come from. Sources are passed in order of
// precedence; File marks an env file rather than flags or the environment.
type Source struct {
	Name   string
	Server string
	Token  string
	File   bool
}

// Resolve picks the server and token. A server read from a file is only used together with a
// token from that same file, so a stray .env can't redirect a token from elsewhere.
func Resolve(sources []Source) (server, token string, err error) {
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
		return DefaultServer, token, nil
	}
	if srv.File && tok != nil && srv != tok {
		return "", "", fmt.Errorf("WAME_SERVER comes from %s but the token doesn't; refusing to send it there. Put both in the same file, or pass -server", srv.Name)
	}
	return srv.Server, token, nil
}

// ReadEnvFile reads WAME_SERVER and WAME_TOKEN from a KEY=VALUE file. A missing file is no error.
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
		}
	}
	return src, sc.Err()
}
