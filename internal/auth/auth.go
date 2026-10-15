// Package auth issues and verifies wame API tokens and decides what each one may do.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/williamtcastro/wame/internal/store"
)

type Scope string

const (
	Read  Scope = "read"
	Send  Scope = "send"
	Admin Scope = "admin"
)

// Prefix starts every issued token, so leaked tokens are easy to spot and to scan for.
const Prefix = "wame_"

var (
	ErrUnauthorized = errors.New("unauthorized")
	ErrExpired      = store.ErrTokenExpired
	ErrRevoked      = store.ErrTokenRevoked

	namePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,39}$`)
	reservedNames = []string{"api", "owner", "local", "mcp", "tui", "cli", "autoreply", "whatsapp", "relink"}
	idEncoding    = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)
)

// Principal is who is calling and what they may do.
type Principal struct {
	Name    string
	TokenID string
	Scopes  []Scope
	Chats   []string // empty means every chat
	Rate    int      // extra per-token sends per minute; 0 means only the global limit
	Expires time.Time
}

// Owner is the all-powerful principal for the -token / WAME_TOKEN secret and token-less local use.
func Owner() Principal { return Principal{Name: "api", Scopes: []Scope{Admin}} }

// Has reports whether p holds scope s. Admin implies every scope.
func (p Principal) Has(s Scope) bool {
	return slices.Contains(p.Scopes, s) || slices.Contains(p.Scopes, Admin)
}

// Restricted reports whether p is limited to a chat allowlist.
func (p Principal) Restricted() bool { return len(p.Chats) > 0 }

func ParseScopes(list []string) ([]Scope, error) {
	var out []Scope
	for _, s := range list {
		switch sc := Scope(strings.TrimSpace(s)); sc {
		case Read, Send, Admin:
			if !slices.Contains(out, sc) {
				out = append(out, sc)
			}
		case "":
		default:
			return nil, fmt.Errorf("unknown scope %q (use read, send, admin)", s)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("at least one scope is required (read, send, admin)")
	}
	return out, nil
}

// Request describes a token to create. Chats must already be normalized JIDs.
type Request struct {
	Name   string
	Scopes []Scope
	Chats  []string
	Rate   int
	TTL    time.Duration
	// CreatedBy and NotAfter are set when a token mints another over the API: the child is revoked with
	// its parent and can't outlive it.
	CreatedBy string
	NotAfter  time.Time
}

// DefaultRate is a new token's own send limit per minute unless one is given, so a single agent can't
// use up the whole global budget.
const DefaultRate = 10

// MinOwnerSecret is the shortest -token / WAME_TOKEN accepted, since it can be guessed online.
const MinOwnerSecret = 16

// InvalidRequest is a token request Issue refused: a bad name, scopes or limits, or a name already taken.
// Any other error from Issue is a database failure.
type InvalidRequest struct{ Err error }

func (e InvalidRequest) Error() string { return e.Err.Error() }
func (e InvalidRequest) Unwrap() error { return e.Err }

func invalid(format string, args ...any) error {
	return InvalidRequest{fmt.Errorf(format, args...)}
}

// Issue creates a token and returns its plaintext, which is never stored and can't be shown again.
func Issue(st *store.Store, req Request) (string, store.Token, error) {
	if !namePattern.MatchString(req.Name) || slices.Contains(reservedNames, req.Name) {
		return "", store.Token{}, invalid("invalid token name %q: use 1-40 lowercase letters, digits, '.', '_' or '-' (not %s)", req.Name, strings.Join(reservedNames, ", "))
	}
	if len(req.Scopes) == 0 {
		return "", store.Token{}, invalid("at least one scope is required")
	}
	if slices.Contains(req.Scopes, Admin) && len(req.Chats) > 0 {
		return "", store.Token{}, invalid("an admin token can't be limited to chats; drop admin or the chat list")
	}
	if req.Rate < 0 || req.TTL < 0 {
		return "", store.Token{}, invalid("rate and expiry can't be negative")
	}
	for attempt := 0; ; attempt++ {
		idBytes, secret := make([]byte, 5), make([]byte, 32)
		rand.Read(idBytes)
		rand.Read(secret)
		id := idEncoding.EncodeToString(idBytes)
		plain := Prefix + id + "_" + base64.RawURLEncoding.EncodeToString(secret)
		t := store.Token{
			ID: id, Name: req.Name, Hash: hash(plain), Chats: req.Chats, Rate: req.Rate, Created: time.Now(), CreatedBy: req.CreatedBy,
		}
		for _, s := range req.Scopes {
			t.Scopes = append(t.Scopes, string(s))
		}
		if req.TTL > 0 {
			t.Expires = t.Created.Add(req.TTL)
		}
		if !req.NotAfter.IsZero() && (t.Expires.IsZero() || t.Expires.After(req.NotAfter)) {
			t.Expires = req.NotAfter
		}
		err := st.InsertToken(t)
		switch {
		case errors.Is(err, store.ErrTokenID) && attempt < 3:
			continue
		case errors.Is(err, store.ErrTokenName):
			return "", store.Token{}, InvalidRequest{err}
		case err != nil:
			return "", store.Token{}, err
		}
		return plain, t, nil
	}
}

func hash(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

// Authenticator checks presented tokens against the owner secret and the token table.
type Authenticator struct {
	Store *store.Store
	Owner string

	mu      sync.Mutex
	touched map[string]time.Time
}

// Required reports whether requests must carry a token. Without an owner secret or any active token,
// the API stays in token-less, loopback-only mode.
// Once any token has existed, tokens stay required for good (even if all are revoked), and a
// database error counts as "required": the check fails closed.
func (a *Authenticator) Required() bool {
	if a.Owner != "" {
		return true
	}
	has, err := a.Store.HasTokens()
	return has || err != nil
}

// Authenticate resolves a presented token to a Principal. Errors never reveal whether a token ID exists.
func (a *Authenticator) Authenticate(presented string) (Principal, error) {
	if presented == "" {
		return Principal{}, ErrUnauthorized
	}
	digest := hash(presented)
	if a.Owner != "" && subtle.ConstantTimeCompare(digest, hash(a.Owner)) == 1 {
		return Owner(), nil
	}
	id, ok := tokenID(presented)
	if !ok {
		return Principal{}, ErrUnauthorized
	}
	t, err := a.Store.TokenByID(id)
	if err != nil {
		subtle.ConstantTimeCompare(digest, make([]byte, len(digest)))
		return Principal{}, ErrUnauthorized
	}
	if subtle.ConstantTimeCompare(digest, t.Hash) != 1 {
		return Principal{}, ErrUnauthorized
	}
	if err := t.Usable(time.Now()); err != nil {
		return Principal{}, err
	}
	a.touch(t.ID)
	p := Principal{Name: t.Name, TokenID: t.ID, Chats: t.Chats, Rate: t.Rate, Expires: t.Expires}
	for _, s := range t.Scopes {
		p.Scopes = append(p.Scopes, Scope(s))
	}
	return p, nil
}

func tokenID(presented string) (string, bool) {
	rest, ok := strings.CutPrefix(presented, Prefix)
	if !ok {
		return "", false
	}
	id, secret, ok := strings.Cut(rest, "_")
	return id, ok && len(id) == 8 && secret != ""
}

// touch records last use at most once a minute per token, to keep writes off the hot path.
func (a *Authenticator) touch(id string) {
	a.mu.Lock()
	if a.touched == nil {
		a.touched = map[string]time.Time{}
	}
	now := time.Now()
	due := now.Sub(a.touched[id]) > time.Minute
	if due {
		a.touched[id] = now
	}
	a.mu.Unlock()
	if due {
		a.Store.TouchToken(id, now)
	}
}

// CheckOwnerSecret rejects owner secrets short enough to guess over the network.
func CheckOwnerSecret(s string) error {
	if s != "" && len(s) < MinOwnerSecret {
		return fmt.Errorf("the API token (-token / WAME_TOKEN) must be at least %d characters; generate one with: openssl rand -hex 24", MinOwnerSecret)
	}
	return nil
}
