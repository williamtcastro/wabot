package bot

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
)

const (
	allowlistTTL        = 10 * time.Minute
	allowlistRetryAfter = 30 * time.Second
)

type expandedAllowlist struct {
	chats   []string
	expires time.Time
}

// ExpandAllowlist returns a token's chat allowlist plus the JIDs its phone numbers resolve to, so an entry
// saved with or without Brazil's 9th digit still matches the chat WhatsApp uses. Results (including
// numbers WhatsApp doesn't know) are cached, so callers can't trigger repeated lookups. When a lookup
// couldn't be made (offline, rate limited, network) the partial result is kept only briefly.
func (b *Bot) ExpandAllowlist(ctx context.Context, allow []string) []string {
	if len(allow) == 0 {
		return nil
	}
	key := strings.Join(allow, ",")
	b.mu.RLock()
	cached, ok := b.allowlists[key]
	b.mu.RUnlock()
	if ok && time.Now().Before(cached.expires) {
		return cached.chats
	}
	out := slices.Clone(allow)
	ttl := allowlistTTL
	for _, entry := range allow {
		num, ok := strings.CutSuffix(entry, "@s.whatsapp.net")
		if !ok {
			continue
		}
		if !b.Client.IsLoggedIn() {
			ttl = allowlistRetryAfter
			continue
		}
		jid, err := b.Resolve(ctx, num)
		if err == nil {
			out = append(out, b.PhoneJID(jid).String())
		} else if _, ok := errors.AsType[InputError](err); !ok {
			ttl = allowlistRetryAfter
		}
	}
	slices.Sort(out)
	out = slices.Compact(out)
	b.mu.Lock()
	if b.allowlists == nil || len(b.allowlists) > 1000 {
		b.allowlists = map[string]expandedAllowlist{}
	}
	b.allowlists[key] = expandedAllowlist{chats: out, expires: time.Now().Add(ttl)}
	b.mu.Unlock()
	return out
}

// ChatAllowed reports whether chat (a JID) is on the allowlist. An empty allowlist allows all. The chat
// itself is never looked up, so a restricted caller can't use this to probe numbers.
func (b *Bot) ChatAllowed(ctx context.Context, allow []string, chat string) bool {
	return len(allow) == 0 || slices.Contains(b.ExpandAllowlist(ctx, allow), chat)
}

// CanonicalChat resolves a phone number or JID to the chat key messages are stored under, asking
// WhatsApp for unknown numbers.
func (b *Bot) CanonicalChat(ctx context.Context, to string) (string, error) {
	jid, err := b.Resolve(ctx, to)
	if err != nil {
		return "", err
	}
	return b.PhoneJID(jid).String(), nil
}

// LocalChat turns a phone number or JID into a chat key without contacting WhatsApp.
func (b *Bot) LocalChat(to string) (string, error) {
	jid, err := b.localJID(to)
	if err != nil {
		return "", err
	}
	return jid.String(), nil
}

// localJID parses a number or JID into the JID chats are stored under: an @lid mapped to its phone
// JID and without a device suffix. Nothing is asked of WhatsApp.
func (b *Bot) localJID(to string) (types.JID, error) {
	jid, err := ParseRecipient(to)
	if err != nil {
		return jid, err
	}
	return b.PhoneJID(jid).ToNonAD(), nil
}
