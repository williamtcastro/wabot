package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"

	"github.com/williamtcastro/wame/internal/store"
)

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
			return types.JID{}, BadInput("invalid JID %.40q", to)
		}
		return jid, nil
	}
	d := digits(to)
	if len(d) < 8 || len(d) > 15 {
		return types.JID{}, BadInput("invalid recipient %.40q", to)
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
	return b.Lookups.Wait(ctx, "lookup", n)
}

// Chats lists conversations by most recent activity, named like ChatName does.
func (b *Bot) Chats(ctx context.Context, limit int) ([]store.Chat, error) {
	return b.Store.Chats(ctx, limit, func(chat, pushName string) string { return b.ChatName(ctx, chat, pushName) })
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
		b.Go(b.refreshGroups)
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
// instantly next start), and tells subscribers names may have changed. If a refresh is already
// running it waits for that one instead. A logout while it runs makes it keep nothing.
func (b *Bot) refreshGroups(ctx context.Context) {
	b.mu.Lock()
	if running := b.groupsRefresh; running != nil {
		b.mu.Unlock()
		select {
		case <-running:
		case <-ctx.Done():
		}
		return
	}
	done := make(chan struct{})
	b.groupsRefresh = done
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.groupsRefresh = nil
		b.mu.Unlock()
		close(done)
	}()
	wipes := b.wipes.Load()
	joined, err := b.joinedGroups(ctx)
	if err != nil {
		b.Log.Warn("refresh groups failed", "err", err)
		return
	}
	all := make(map[string]groupInfo, len(joined))
	cached := make([]store.Group, 0, len(joined))
	for _, j := range joined {
		info := groupInfo{name: j.Name, admins: map[string]bool{}}
		var admins []string
		members := make([]store.GroupMember, 0, len(j.Participants))
		for _, p := range j.Participants {
			if member := memberJID(b.PhoneJID(p.JID.ToNonAD()), p.PhoneNumber); !member.IsEmpty() {
				members = append(members, store.GroupMember{JID: member.String(), Role: participantRole(p)})
			}
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
		cached = append(cached, store.Group{JID: j.JID.String(), Name: j.Name, Admins: admins, Members: members})
	}
	b.contactsMu.Lock()
	defer b.contactsMu.Unlock()
	if b.wipes.Load() != wipes {
		return
	}
	if err := b.Store.SaveJoinedGroups(cached); err != nil {
		b.Log.Warn("cache groups failed", "err", err)
	}
	b.mu.Lock()
	b.groups, b.groupsAt = all, time.Now()
	b.mu.Unlock()
	b.publish(NamesChanged{})
}

// memberJID is a participant's phone JID when known: the @lid WhatsApp reported is kept only when
// neither the LID map nor the participant carries the number.
func memberJID(jid, phone types.JID) types.JID {
	if jid.Server == types.HiddenUserServer && !phone.IsEmpty() {
		return phone.ToNonAD()
	}
	return jid
}

func participantRole(p types.GroupParticipant) string {
	switch {
	case p.IsSuperAdmin:
		return store.RoleSuperAdmin
	case p.IsAdmin:
		return store.RoleAdmin
	}
	return store.RoleMember
}

// NamesChanged is published when names may have changed (group names refreshed, your own contacts
// edited, the phone's address book or a push or business name changed), so views can re-read chat
// names. It carries nothing.
type NamesChanged struct{}

func (b *Bot) loadCachedGroups() {
	groups, err := b.Store.Groups()
	if err != nil {
		b.Log.Error("load cached groups", "err", err)
	}
	all := map[string]groupInfo{}
	for _, g := range groups {
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
