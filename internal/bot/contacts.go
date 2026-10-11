package bot

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"

	"github.com/williamtcastro/wabot/internal/store"
)

type Contact struct {
	JID      string `json:"jid"`
	Phone    string `json:"phone"`
	Name     string `json:"name"`
	PushName string `json:"push_name,omitempty"`
}

// Contacts lists address-book contacts synced from the phone, filtered by name or number.
func (b *Bot) Contacts(ctx context.Context, query string) ([]Contact, error) {
	if b.Client.Store.Contacts == nil {
		return []Contact{}, nil
	}
	all, err := b.Client.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(strings.TrimSpace(query))
	out := []Contact{}
	for jid, c := range all {
		if jid.Server != types.DefaultUserServer {
			continue
		}
		name := cmp.Or(c.FullName, c.FirstName, c.BusinessName, c.PushName)
		if name == "" {
			continue
		}
		if q != "" {
			byName := strings.Contains(strings.ToLower(name+" "+c.PushName), q)
			byPhone := digits(q) != "" && strings.Contains(jid.User, digits(q))
			if !byName && !byPhone {
				continue
			}
		}
		out = append(out, Contact{JID: jid.String(), Phone: "+" + jid.User, Name: name, PushName: c.PushName})
	}
	slices.SortFunc(out, func(x, y Contact) int { return cmp.Compare(strings.ToLower(x.Name), strings.ToLower(y.Name)) })
	return out, nil
}

// CheckResult is one number from Check.
type CheckResult struct {
	Query string `json:"query"`
	IsIn  bool   `json:"on_whatsapp"`
	JID   string `json:"jid"`
}

// Check asks WhatsApp which phone numbers have an account. Each number counts against Lookups.
func (b *Bot) Check(ctx context.Context, numbers []string) ([]CheckResult, error) {
	phones := make([]string, len(numbers))
	for i, p := range numbers {
		phones[i] = "+" + digits(p)
	}
	if err := b.lookup(ctx, len(phones)); err != nil {
		return nil, err
	}
	res, err := b.Client.IsOnWhatsApp(ctx, phones)
	if err != nil {
		return nil, err
	}
	out := make([]CheckResult, len(res))
	for i, x := range res {
		jid := ""
		if x.IsIn {
			jid = x.JID.String()
		}
		out[i] = CheckResult{x.Query, x.IsIn, jid}
	}
	return out, nil
}

// Resolve turns a recipient into the JID WhatsApp actually uses. Plain phone numbers are
// looked up on the server, which fixes variants like Brazil's optional 9th digit.
func (b *Bot) Resolve(ctx context.Context, to string) (types.JID, error) {
	jid, err := ParseRecipient(to)
	if err != nil || strings.Contains(to, "@") {
		return jid, err
	}
	b.mu.RLock()
	cached, ok := b.jids[jid.User]
	b.mu.RUnlock()
	if ok {
		return cached, nil
	}
	if err := b.lookup(ctx, 1); err != nil {
		return types.JID{}, err
	}
	res, err := b.Client.IsOnWhatsApp(ctx, []string{"+" + jid.User})
	if err != nil {
		return types.JID{}, fmt.Errorf("lookup %s: %w", to, err)
	}
	for _, r := range res {
		if r.IsIn {
			b.mu.Lock()
			if len(b.jids) > 5000 {
				clear(b.jids)
			}
			b.jids[jid.User] = r.JID
			b.mu.Unlock()
			return r.JID, nil
		}
	}
	return types.JID{}, BadInput("+%s is not on WhatsApp", jid.User)
}

func (b *Bot) resolveOnline(ctx context.Context, to string) (types.JID, error) {
	if !b.Client.IsLoggedIn() {
		return types.JID{}, errors.New("not logged in")
	}
	return b.Resolve(ctx, to)
}

// ParseRecipient accepts "+55 11 99999-9999", "5511999999999" or a full JID like "1203...@g.us".
func ParseRecipient(to string) (types.JID, error) {
	to = strings.TrimSpace(to)
	if strings.Contains(to, "@") {
		jid, err := types.ParseJID(to)
		if err != nil || jid.User == "" {
			return types.JID{}, BadInput("invalid JID %q", to)
		}
		return jid, nil
	}
	d := digits(to)
	if len(d) < 8 || len(d) > 15 {
		return types.JID{}, BadInput("invalid recipient %q", to)
	}
	return types.NewJID(d, types.DefaultUserServer), nil
}

func digits(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
}

// lookup charges n number checks (or presence updates) to the Lookups limiter.
func (b *Bot) lookup(ctx context.Context, n int) error {
	slot, err := b.Lookups.ReserveN("lookup", n)
	if err != nil {
		return err
	}
	return SleepUntil(ctx, slot)
}

// Chats lists conversations by most recent activity, named like ChatName does.
func (b *Bot) Chats(ctx context.Context, limit int) ([]store.Chat, error) {
	return b.Store.Chats(ctx, limit, func(chat, pushName string) string { return b.ChatName(ctx, chat, pushName) })
}

// ChatName is the best display name for a chat: group subject, "Me", address-book name, ~push name, +number.
func (b *Bot) ChatName(ctx context.Context, chat, pushName string) string {
	jid, err := types.ParseJID(chat)
	if err != nil {
		return chat
	}
	if jid.Server == types.GroupServer {
		if name := b.group(ctx, chat).name; name != "" {
			return name
		}
		return "group " + jid.User
	}
	if own := b.Client.Store.ID; own != nil && own.User == jid.User {
		return "Me"
	}
	if b.Client.Store.Contacts != nil {
		if c, err := b.Client.Store.Contacts.GetContact(ctx, jid); err == nil {
			if name := cmp.Or(c.FullName, c.FirstName, c.BusinessName, c.PushName); name != "" {
				return name
			}
		}
	}
	if pushName != "" {
		return "~" + pushName
	}
	if jid.Server == types.DefaultUserServer {
		return "+" + jid.User
	}
	return chat
}

type groupInfo struct {
	name   string
	admins map[string]bool
}

func (b *Bot) isGroupAdmin(ctx context.Context, chat, user string) bool {
	return b.group(ctx, chat).admins[user]
}

// group returns cached group info, refreshing at most every 10 minutes.
func (b *Bot) group(ctx context.Context, chat string) groupInfo {
	b.mu.RLock()
	g, ok := b.groups[chat]
	stale := time.Since(b.groupsAt) > groupsTTL
	b.mu.RUnlock()
	switch {
	case !b.Client.IsLoggedIn() || !stale:
	case ok:
		go b.refreshGroups(context.Background())
	default:
		b.refreshGroups(ctx)
		b.mu.RLock()
		g = b.groups[chat]
		b.mu.RUnlock()
	}
	return g
}

const groupsTTL = 10 * time.Minute

// refreshGroups fetches every group we're in, caches names and admins in the database (so they show
// instantly next start), and tells subscribers names may have changed.
func (b *Bot) refreshGroups(ctx context.Context) {
	if !b.groupsBusy.CompareAndSwap(false, true) {
		return
	}
	defer b.groupsBusy.Store(false)
	joined, err := b.Client.GetJoinedGroups(ctx)
	if err != nil {
		b.Log.Warn("refresh groups failed", "err", err)
		return
	}
	all := make(map[string]groupInfo, len(joined))
	cached := make([]store.Group, 0, len(joined))
	for _, j := range joined {
		info := groupInfo{name: j.Name, admins: map[string]bool{}}
		var admins []string
		for _, p := range j.Participants {
			if p.IsAdmin || p.IsSuperAdmin {
				for _, id := range []types.JID{p.JID, p.PhoneNumber, p.LID} {
					if !id.IsEmpty() {
						jid := b.PhoneJID(id.ToNonAD()).String()
						info.admins[jid] = true
						admins = append(admins, jid)
					}
				}
			}
		}
		all[j.JID.String()] = info
		cached = append(cached, store.Group{JID: j.JID.String(), Name: j.Name, Admins: admins})
	}
	if err := b.Store.SaveGroups(cached); err != nil {
		b.Log.Warn("cache groups failed", "err", err)
	}
	b.mu.Lock()
	b.groups, b.groupsAt = all, time.Now()
	b.mu.Unlock()
	b.publish(NamesChanged{})
}

// NamesChanged is published when group names were refreshed, so views can re-read chat names.
type NamesChanged struct{}

func (b *Bot) loadCachedGroups() {
	all := map[string]groupInfo{}
	for _, g := range b.Store.Groups() {
		info := groupInfo{name: g.Name, admins: map[string]bool{}}
		for _, a := range g.Admins {
			info.admins[a] = true
		}
		all[g.JID] = info
	}
	b.mu.Lock()
	b.groups = all
	b.mu.Unlock()
}
