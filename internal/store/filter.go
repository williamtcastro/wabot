package store

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// Filter selects messages for /messages, /messages/wait, /events and the webhook.
type Filter struct {
	Chat     string
	ChatOnly bool
	Types    []string
	FromMe   *bool
	IsGroup  *bool
	Since    int64
	Text     string
	// Chats, when set, limits results to these chats (a token's allowlist).
	Chats []string
}

var messageTypes = map[string]bool{
	"text": true, "image": true, "video": true, "document": true, "audio": true,
	"sticker": true, "reaction": true, "location": true,
}

// ParseFilter reads chat, type, from_me, is_group and since from a query string. resolve turns a
// phone number into the chat JID WhatsApp uses.
func ParseFilter(q url.Values, resolve func(string) (string, error)) (Filter, error) {
	var f Filter
	if c := strings.TrimSpace(q.Get("chat")); c != "" {
		jid, err := resolve(c)
		if err != nil {
			return f, err
		}
		f.Chat = jid
	}
	for _, t := range strings.Split(q.Get("type"), ",") {
		if t = strings.TrimSpace(t); t == "" {
			continue
		}
		if !messageTypes[t] {
			return f, fmt.Errorf("unknown type %q", t)
		}
		f.Types = append(f.Types, t)
	}
	for key, dst := range map[string]**bool{"from_me": &f.FromMe, "is_group": &f.IsGroup} {
		if v := q.Get(key); v != "" {
			bv, err := strconv.ParseBool(v)
			if err != nil {
				return f, fmt.Errorf("%s must be true or false", key)
			}
			*dst = &bv
		}
	}
	f.Text = strings.TrimSpace(q.Get("q"))
	if v := q.Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return f, errors.New("since must be a message seq (a non-negative integer)")
		}
		f.Since = n
	}
	return f, nil
}

func (f Filter) Match(m Message) bool {
	if f.Chat != "" && m.Chat != f.Chat && (f.ChatOnly || m.Sender != f.Chat) {
		return false
	}
	if len(f.Types) > 0 && !slices.Contains(f.Types, m.Type) {
		return false
	}
	if f.FromMe != nil && *f.FromMe != m.FromMe {
		return false
	}
	if f.IsGroup != nil && *f.IsGroup != m.IsGroup {
		return false
	}
	if f.Text != "" && !textMatches(f.Text, m.Text) {
		return false
	}
	if len(f.Chats) > 0 && !slices.Contains(f.Chats, m.Chat) {
		return false
	}
	return m.Seq > f.Since
}

func (f Filter) where() (string, []any) {
	conds := []string{"seq > ?"}
	args := []any{f.Since}
	switch {
	case f.Chat != "" && f.ChatOnly:
		conds = append(conds, "chat = ?")
		args = append(args, f.Chat)
	case f.Chat != "":
		conds = append(conds, "(chat = ? OR sender = ?)")
		args = append(args, f.Chat, f.Chat)
	}
	if len(f.Types) > 0 {
		conds = append(conds, "type IN (?"+strings.Repeat(",?", len(f.Types)-1)+")")
		for _, t := range f.Types {
			args = append(args, t)
		}
	}
	if f.FromMe != nil {
		conds = append(conds, "from_me = ?")
		args = append(args, *f.FromMe)
	}
	if f.IsGroup != nil {
		conds = append(conds, "is_group = ?")
		args = append(args, *f.IsGroup)
	}
	if len(f.Chats) > 0 {
		conds = append(conds, "chat IN (?"+strings.Repeat(",?", len(f.Chats)-1)+")")
		for _, c := range f.Chats {
			args = append(args, c)
		}
	}
	if q := ftsQuery(f.Text); q != "" {
		conds = append(conds, "seq IN (SELECT rowid FROM wabot_messages_fts WHERE wabot_messages_fts MATCH ?)")
		args = append(args, q)
	}
	return strings.Join(conds, " AND "), args
}

// textMatches mirrors the FTS5 query in Go (for live events and webhooks): every word must match a
// word of the text, accents and case ignored, the last one as a prefix.
func textMatches(query, text string) bool {
	words := tokens(query)
	have := tokens(text)
	for i, w := range words {
		last := i == len(words)-1
		if !slices.ContainsFunc(have, func(h string) bool { return h == w || (last && strings.HasPrefix(h, w)) }) {
			return false
		}
	}
	return true
}

var stripMarks = transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)

func tokens(s string) []string {
	folded, _, err := transform.String(stripMarks, strings.ToLower(s))
	if err != nil {
		folded = strings.ToLower(s)
	}
	return strings.FieldsFunc(folded, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
}
