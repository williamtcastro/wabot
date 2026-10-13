package auth

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/williamtcastro/wame/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "t.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := store.Open(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestIssueAndAuthenticate(t *testing.T) {
	st := newStore(t)
	a := &Authenticator{Store: st}
	if a.Required() {
		t.Fatal("no owner and no tokens: token-less mode")
	}
	plain, tok, err := Issue(st, Request{Name: "maria-bot", Scopes: []Scope{Read, Send}, Chats: []string{"5511999999999@s.whatsapp.net"}, Rate: 5})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, Prefix+tok.ID+"_") || len(plain) < 50 {
		t.Fatalf("unexpected token shape %q", plain)
	}
	stored, _ := st.TokenByID(tok.ID)
	if strings.Contains(string(stored.Hash), plain) || len(stored.Hash) != 32 {
		t.Fatal("only a SHA-256 hash may be stored")
	}
	if !a.Required() {
		t.Fatal("an active token must make tokens required")
	}
	p, err := a.Authenticate(plain)
	if err != nil || p.Name != "maria-bot" || !p.Has(Send) || p.Has(Admin) || !p.Restricted() || p.Rate != 5 {
		t.Fatalf("Authenticate = %+v, %v", p, err)
	}

	bad := []string{
		"",
		plain[:len(plain)-1] + "x", // tampered secret
		Prefix + "aaaaaaaa_" + plain[len(Prefix)+9:], // right secret, wrong id
		strings.Replace(plain, Prefix, "other_", 1),  // wrong prefix
		Prefix + tok.ID,   // no secret
		"Bearer " + plain, // not stripped
	}
	for _, b := range bad {
		if _, err := a.Authenticate(b); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("Authenticate(%q) = %v, want ErrUnauthorized", b, err)
		}
	}

	if n, _ := st.RevokeToken("maria-bot"); n != 1 {
		t.Fatal("revoke failed")
	}
	if _, err := a.Authenticate(plain); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoked token: %v", err)
	}
	if !a.Required() {
		t.Fatal("once a token has existed, tokens stay required even after all are revoked")
	}
}

func TestExpiryOwnerAndValidation(t *testing.T) {
	st := newStore(t)
	plain, _, err := Issue(st, Request{Name: "short", Scopes: []Scope{Read}, NotAfter: time.Now().Add(-time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	a := &Authenticator{Store: st, Owner: "owner-secret"}
	if _, err := a.Authenticate(plain); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired token: %v", err)
	}
	if p, err := a.Authenticate("owner-secret"); err != nil || !p.Has(Admin) || p.Restricted() {
		t.Fatalf("owner: %+v %v", p, err)
	}
	for _, req := range []Request{
		{Name: "Has Spaces", Scopes: []Scope{Read}},
		{Name: "api", Scopes: []Scope{Read}},
		{Name: "mcp", Scopes: []Scope{Read, Send}},
		{Name: "relink", Scopes: []Scope{Read}},
		{Name: "ok", Scopes: nil},
		{Name: "admin-chats", Scopes: []Scope{Admin}, Chats: []string{"5511999999999@s.whatsapp.net"}},
		{Name: "neg", Scopes: []Scope{Read}, Rate: -1},
		{Name: "short", Scopes: []Scope{Read}},
	} {
		if _, _, err := Issue(st, req); !isInvalid(err) {
			t.Errorf("Issue(%+v) = %v, want InvalidRequest", req, err)
		}
	}
	if _, err := ParseScopes([]string{"read", "root"}); err == nil {
		t.Error("unknown scope must be rejected")
	}
	if !(Principal{Scopes: []Scope{Admin}}).Has(Send) {
		t.Error("admin implies every scope")
	}
}

func TestF2RequiredFailsClosed(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st, err := store.Open(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	a := &Authenticator{Store: st}
	if a.Required() {
		t.Fatal("fresh database: token-less mode")
	}
	if _, err := db.Exec(`ALTER TABLE wame_tokens RENAME TO gone`); err != nil {
		t.Fatal(err)
	}
	if !a.Required() {
		t.Fatal("a database error must make tokens required (fail closed)")
	}
}

func TestTokensFromBeforeTheRenameStillWork(t *testing.T) {
	st := newStore(t)
	plain := "wabot_abcdefgh_c2VjcmV0LXNlY3JldC1zZWNyZXQtc2VjcmV0"
	if err := st.InsertToken(store.Token{ID: "abcdefgh", Name: "old-agent", Hash: hash(plain), Scopes: []string{"read"}, Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	p, err := (&Authenticator{Store: st}).Authenticate(plain)
	if err != nil || p.Name != "old-agent" {
		t.Fatalf("wabot_ token = %+v, %v", p, err)
	}
}

func isInvalid(err error) bool {
	_, ok := errors.AsType[InvalidRequest](err)
	return ok
}

func TestIssueDatabaseErrorIsNotInvalid(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st, err := store.Open(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE wame_tokens RENAME TO gone`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Issue(st, Request{Name: "maria-bot", Scopes: []Scope{Read}}); err == nil || isInvalid(err) {
		t.Fatalf("a database failure must not look like a bad request: %v", err)
	}
}
