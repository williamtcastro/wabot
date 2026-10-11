package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Token is an API token as stored: only a SHA-256 hash of the secret is kept.
type Token struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Hash     []byte    `json:"-"`
	Scopes   []string  `json:"scopes"`
	Chats    []string  `json:"chats,omitempty"`
	Rate     int       `json:"rate,omitempty"`
	Created  time.Time `json:"created"`
	Expires  time.Time `json:"expires,omitzero"`
	LastUsed time.Time `json:"last_used,omitzero"`
	Revoked  bool      `json:"revoked,omitempty"`
	// CreatedBy is the token that minted this one over the API ("" for the CLI or owner).
	CreatedBy string `json:"created_by,omitempty"`
}

var (
	ErrTokenName = errors.New("a token with that name already exists (revoked names stay reserved)")
	ErrTokenID   = errors.New("token id collision")
)

const tokensTable = `
CREATE TABLE IF NOT EXISTS wabot_tokens (
	id        TEXT PRIMARY KEY,
	name      TEXT NOT NULL UNIQUE,
	hash      BLOB NOT NULL,
	scopes    TEXT NOT NULL,
	chats     TEXT NOT NULL DEFAULT '[]',
	rate      INTEGER NOT NULL DEFAULT 0,
	created   INTEGER NOT NULL,
	expires   INTEGER NOT NULL DEFAULT 0,
	last_used INTEGER NOT NULL DEFAULT 0,
	revoked   INTEGER NOT NULL DEFAULT 0,
	created_by TEXT NOT NULL DEFAULT ''
);`

func migrateTokens(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, tokensTable)
	return err
}

func (s *Store) InsertToken(t Token) error {
	scopes, _ := json.Marshal(t.Scopes)
	chats, _ := json.Marshal(t.Chats)
	_, err := s.db.Exec(`INSERT INTO wabot_tokens (id, name, hash, scopes, chats, rate, created, expires, created_by) VALUES (?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Name, t.Hash, string(scopes), string(chats), t.Rate, t.Created.UnixMilli(), millisOrZero(t.Expires), t.CreatedBy)
	switch {
	case err == nil:
		return nil
	case strings.Contains(err.Error(), "wabot_tokens.name"):
		return ErrTokenName
	case strings.Contains(err.Error(), "wabot_tokens.id"):
		return ErrTokenID
	}
	return err
}

const tokenCols = `id, name, hash, scopes, chats, rate, created, expires, last_used, revoked, created_by`

func scanToken(r scanner) (Token, error) {
	var t Token
	var scopes, chats string
	var created, expires, lastUsed int64
	if err := r.Scan(&t.ID, &t.Name, &t.Hash, &scopes, &chats, &t.Rate, &created, &expires, &lastUsed, &t.Revoked, &t.CreatedBy); err != nil {
		return t, err
	}
	_ = json.Unmarshal([]byte(scopes), &t.Scopes)
	_ = json.Unmarshal([]byte(chats), &t.Chats)
	t.Created = time.UnixMilli(created)
	t.Expires = timeOrZero(expires)
	t.LastUsed = timeOrZero(lastUsed)
	return t, nil
}

func (s *Store) TokenByID(id string) (Token, error) {
	t, err := scanToken(s.db.QueryRow(`SELECT `+tokenCols+` FROM wabot_tokens WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

func (s *Store) Tokens() ([]Token, error) {
	rows, err := s.db.Query(`SELECT ` + tokenCols + ` FROM wabot_tokens ORDER BY created`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Token{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken marks a token, and every token it minted (recursively), unusable. Rows stay so names
// and audit history remain meaningful. It reports how many tokens were revoked.
func (s *Store) RevokeToken(name string) (int, error) {
	res, err := s.db.Exec(`WITH RECURSIVE tree(name) AS (
			SELECT name FROM wabot_tokens WHERE name = ?
			UNION SELECT t.name FROM wabot_tokens t JOIN tree ON t.created_by = tree.name)
		UPDATE wabot_tokens SET revoked = 1 WHERE revoked = 0 AND name IN (SELECT name FROM tree)`, name)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *Store) TouchToken(id string, at time.Time) {
	_, _ = s.db.Exec(`UPDATE wabot_tokens SET last_used = ? WHERE id = ?`, at.UnixMilli(), id)
}

// HasTokens reports whether any token was ever created. From then on the API requires a token on every
// request, even after all of them are revoked or expired, so it can never fall back to token-less mode.
func (s *Store) HasTokens() (bool, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM wabot_tokens`).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

func millisOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func timeOrZero(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
