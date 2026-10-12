package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/williamtcastro/wame/internal/auth"
	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/store"
)

type principalKey struct{}

func principal(r *http.Request) auth.Principal {
	p, _ := r.Context().Value(principalKey{}).(auth.Principal)
	return p
}

const (
	maxStreamsPerToken = 4
	failuresPerMinute  = 10
	errChatForbidden   = "this token can't access that chat"
)

// presentedToken reads the bearer token. The Authorization header always wins; ?token= is accepted only
// on GET routes a browser or EventSource opens directly (which can't set headers).
func presentedToken(r *http.Request) (string, bool) {
	if h := r.Header.Get("Authorization"); h != "" {
		scheme, tok, ok := strings.Cut(h, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(tok) == "" {
			return "", false
		}
		return strings.TrimSpace(tok), true
	}
	if q := r.URL.Query().Get("token"); q != "" {
		if r.Method != http.MethodGet || !(r.URL.Path == "/events" || r.URL.Path == "/qr" || strings.HasPrefix(r.URL.Path, "/media/")) {
			return "", false
		}
		return q, true
	}
	return "", true
}

// withAuth resolves the caller. Without an owner secret and before any token was ever created, the API
// runs in token-less, loopback-only mode; after that every request needs a valid token.
func withAuth(authn *auth.Authenticator, fails *failureLimiter, next http.Handler) http.Handler {
	local := localOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, auth.Owner())))
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if fails.reject(w, ip) {
			return
		}
		presented, ok := presentedToken(r)
		if !ok {
			fails.record(ip)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "use 'Authorization: Bearer <token>'"})
			return
		}
		if presented == "" && !authn.Required() {
			local.ServeHTTP(w, r)
			return
		}
		p, err := authn.Authenticate(presented)
		if err != nil {
			fails.record(ip)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// failureLimiter blocks an IP for a minute after too many failed authentications.
type failureLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func (f *failureLimiter) record(ip string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hits == nil || len(f.hits) > 10000 {
		f.hits = map[string][]time.Time{}
	}
	f.hits[ip] = append(recent(f.hits[ip]), time.Now())
}

func (f *failureLimiter) blocked(ip string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hits == nil {
		return false
	}
	f.hits[ip] = recent(f.hits[ip])
	return len(f.hits[ip]) >= failuresPerMinute
}

// reject answers 429 when ip is blocked.
func (f *failureLimiter) reject(w http.ResponseWriter, ip string) bool {
	if !f.blocked(ip) {
		return false
	}
	w.Header().Set("Retry-After", "60")
	writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many failed attempts, try again in a minute"})
	return true
}

func recent(ts []time.Time) []time.Time {
	cutoff := time.Now().Add(-time.Minute)
	return slices.DeleteFunc(ts, func(t time.Time) bool { return t.Before(cutoff) })
}

// need wraps a handler so it only runs for callers holding scope.
func need(scope auth.Scope, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !principal(r).Has(scope) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "this token lacks the " + string(scope) + " scope"})
			return
		}
		h(w, r)
	}
}

// unrestricted wraps a handler that would reveal data beyond a token's chat allowlist
// (address book, group list, number checks).
func unrestricted(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if principal(r).Restricted() {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "not available to tokens limited to specific chats"})
			return
		}
		h(w, r)
	}
}

// visibleStatus hides the login QR from non-admins (whoever scans it links their phone) and the account
// itself from tokens limited to specific chats.
func visibleStatus(p auth.Principal, s bot.Status) bot.Status {
	if !p.Has(auth.Admin) {
		s.QR = ""
	}
	if p.Restricted() {
		s.JID, s.PushName = "", ""
	}
	return s
}

// authorizeChat checks a target chat against the caller's allowlist and returns the chat to act on.
// For restricted tokens the target is never looked up on WhatsApp, so they can't probe numbers or spend
// the shared lookup budget.
func (a *server) authorizeChat(w http.ResponseWriter, r *http.Request, to string) (string, bool) {
	p := principal(r)
	if !p.Restricted() {
		return to, true
	}
	chat, err := a.b.LocalChat(to)
	if err != nil || !a.b.ChatAllowed(r.Context(), p.Chats, chat) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": errChatForbidden})
		return "", false
	}
	return chat, true
}

// restrictFilter limits a message filter to the caller's chats.
func (a *server) restrictFilter(w http.ResponseWriter, r *http.Request, f *store.Filter) bool {
	p := principal(r)
	if !p.Restricted() {
		return true
	}
	allowed := a.b.ExpandAllowlist(r.Context(), p.Chats)
	if f.Chat != "" {
		if !slices.Contains(allowed, f.Chat) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": errChatForbidden})
			return false
		}
		f.ChatOnly = true
	}
	f.Chats = allowed
	return true
}

// sendGuard runs the chat allowlist and the per-token rate limit before a send-type action.
func (a *server) sendGuard(w http.ResponseWriter, r *http.Request, chat string) (string, bool) {
	chat, ok := a.authorizeChat(w, r, chat)
	if !ok {
		return "", false
	}
	if err := a.b.ThrottleToken(r.Context(), principal(r).TokenID, principal(r).Rate); err != nil {
		writeError(w, err)
		return "", false
	}
	return chat, true
}

// streams caps how many /events and /messages/wait connections one token holds open.
type streams struct {
	mu   sync.Mutex
	open map[string]int
}

func (s *streams) acquire(p auth.Principal) (release func(), ok bool) {
	if p.TokenID == "" {
		return func() {}, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open == nil {
		s.open = map[string]int{}
	}
	if s.open[p.TokenID] >= maxStreamsPerToken {
		return nil, false
	}
	s.open[p.TokenID]++
	return func() {
		s.mu.Lock()
		s.open[p.TokenID]--
		s.mu.Unlock()
	}, true
}

// stillValid re-checks a long-lived connection's token, so revoking or expiring it ends open streams.
func (a *server) stillValid(p auth.Principal) bool {
	if p.TokenID == "" {
		return true
	}
	t, err := a.b.Store.TokenByID(p.TokenID)
	return err == nil && !t.Revoked && (t.Expires.IsZero() || time.Now().Before(t.Expires))
}

var errTooManyStreams = errors.New("too many open streams for this token")

// streamRecheck is how often open streams re-check that their token is still valid.
var streamRecheck = 10 * time.Second

func (a *server) listTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := a.b.Store.Tokens()
	respond(w, tokens, err)
}

func (a *server) createToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string   `json:"name"`
		Scopes    []string `json:"scopes"`
		Chats     []string `json:"chats"`
		Rate      *int     `json:"rate"`
		ExpiresIn string   `json:"expires_in"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	caller := principal(r)
	scopes, err := auth.ParseScopes(req.Scopes)
	if err != nil {
		writeError(w, bot.BadInput("%v", err))
		return
	}
	if caller.TokenID != "" && slices.Contains(scopes, auth.Admin) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin tokens can only be created with 'wame token create' or the owner token"})
		return
	}
	chats, err := NormalizeChats(req.Chats)
	if err != nil {
		writeError(w, err)
		return
	}
	var ttl time.Duration
	if req.ExpiresIn != "" {
		if ttl, err = time.ParseDuration(req.ExpiresIn); err != nil || ttl <= 0 {
			writeError(w, bot.BadInput("expires_in must be a positive duration like 720h"))
			return
		}
	}
	rate := auth.DefaultRate
	if req.Rate != nil {
		rate = *req.Rate
	}
	issue := auth.Request{Name: req.Name, Scopes: scopes, Chats: chats, Rate: rate, TTL: ttl}
	if caller.TokenID != "" {
		issue.CreatedBy, issue.NotAfter = caller.Name, caller.Expires
	}
	plain, t, err := auth.Issue(a.b.Store, issue)
	a.b.Audit(caller.Name, "token_create", req.Name, strings.Join(req.Scopes, ","), err)
	if err != nil {
		writeError(w, bot.BadInput("%v", err))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": plain, "details": t, "note": "store this token now; it can't be shown again"})
}

func (a *server) revokeToken(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	n, err := a.b.Store.RevokeToken(name)
	if err == nil && n == 0 {
		err = store.ErrNotFound
	}
	a.b.Audit(principal(r).Name, "token_revoke", name, "", err)
	respond(w, map[string]int{"revoked": n}, err)
}

// NormalizeChats turns allowlist entries (phone numbers or JIDs) into JIDs.
func NormalizeChats(entries []string) ([]string, error) {
	var out []string
	for _, e := range entries {
		if e = strings.TrimSpace(e); e == "" {
			continue
		}
		jid, err := bot.ParseRecipient(e)
		if err != nil {
			return nil, err
		}
		out = append(out, jid.String())
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}
