package store

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Filter selects messages for /messages, /messages/wait, /events and the webhook.
type Filter struct {
	Chat     string
	ChatOnly bool
	Types    []string
	FromMe   *bool
	IsGroup  *bool
	Since    int64
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
	return strings.Join(conds, " AND "), args
}
