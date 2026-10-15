package bot

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"

	"github.com/williamtcastro/wame/internal/store"
)

// profileClient is the part of whatsmeow a profile refresh uses, so tests can fake it.
type profileClient interface {
	IsLoggedIn() bool
	IsOnWhatsApp(ctx context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error)
	GetUserInfo(ctx context.Context, jids []types.JID) (map[types.JID]types.UserInfo, error)
	GetProfilePictureInfo(ctx context.Context, jid types.JID, params *whatsmeow.GetProfilePictureParams) (*types.ProfilePictureInfo, error)
	GetBusinessProfile(ctx context.Context, jid types.JID) (*types.BusinessProfile, error)
}

const (
	profileRefreshEvery = 5 * time.Minute
	profileInfoTTL      = 24 * time.Hour
	profileBusinessTTL  = 7 * 24 * time.Hour
	profilePictureTTL   = 6 * time.Hour
	// ProfileCacheKeep is how long a cached WhatsApp profile is kept without a refresh.
	ProfileCacheKeep = 90 * 24 * time.Hour

	maxAbout        = 700
	maxVerifiedName = 200
	maxPictureID    = 64
	maxPictureURL   = 2048
	maxBusinessJSON = 8 << 10
	maxProfileError = 200
	maxBusinessText = 256
)

// ProfileOptions choose what Profile shows and whether it asks WhatsApp first.
type ProfileOptions struct {
	// Full adds everything an admin may see: notes, the business profile, the picture, queue
	// results, opt-out and the tokens limited to this chat. It implies Details.
	Full bool
	// Details adds about, verified name, language, time zone and activity: what contact_profile shows.
	Details bool
	// Refresh asks WhatsApp now (2 to 4 lookups, at most once per person per 5 minutes). Admin only;
	// it needs Full.
	Refresh bool
	// Actor is the audit actor of a refresh.
	Actor string
}

// Profile is one person: names, your own fields, what WhatsApp said (cached), and what wame's own
// tables know. Parts a caller may not see are left empty.
type Profile struct {
	JID           string           `json:"jid"`
	Phone         string           `json:"phone"`
	LID           string           `json:"lid"`
	Names         Names            `json:"names"`
	Own           ProfileOwn       `json:"own"`
	WhatsApp      ProfileWhatsApp  `json:"whatsapp"`
	Activity      *ProfileActivity `json:"activity,omitempty"`
	Queue         *ProfileQueue    `json:"queue,omitempty"`
	OptedOut      *bool            `json:"opted_out,omitempty"`
	Tokens        []string         `json:"tokens,omitempty"`
	Refreshed     *bool            `json:"refreshed,omitempty"`
	NextRefreshAt time.Time        `json:"next_refresh_at,omitzero"`
}

// ProfileOwn is your own fields; callers without admin get only the tags.
type ProfileOwn struct {
	Tags      []string  `json:"tags"`
	Notes     string    `json:"notes,omitempty"`
	Language  string    `json:"language,omitempty"`
	TimeZone  string    `json:"time_zone,omitempty"`
	CreatedAt time.Time `json:"created_at,omitzero"`
	CreatedBy string    `json:"created_by,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
	UpdatedBy string    `json:"updated_by,omitempty"`
}

// ProfileWhatsApp is what WhatsApp last said about the person. OnWhatsApp is null when unknown.
// Stale says a part is older than its TTL (or was never fetched).
type ProfileWhatsApp struct {
	OnWhatsApp    *bool           `json:"on_whatsapp"`
	About         string          `json:"about,omitempty"`
	AboutAt       time.Time       `json:"about_at,omitzero"`
	VerifiedName  string          `json:"verified_name,omitempty"`
	VerifiedLevel string          `json:"verified_level,omitempty"`
	Picture       *ProfilePicture `json:"picture,omitempty"`
	Business      *BusinessInfo   `json:"business,omitempty"`
	AsOf          time.Time       `json:"as_of,omitzero"`
	Stale         bool            `json:"stale"`
	Error         string          `json:"error,omitempty"`
	ErrorAt       time.Time       `json:"error_at,omitzero"`
}

// ProfilePicture is the photo's id and a signed CDN URL (dropped after 6 hours).
type ProfilePicture struct {
	ID     string    `json:"id,omitempty"`
	URL    string    `json:"url,omitempty"`
	Hidden bool      `json:"hidden"`
	AsOf   time.Time `json:"as_of,omitzero"`
}

// BusinessInfo is the part of a WhatsApp business profile wame keeps.
type BusinessInfo struct {
	Address    string            `json:"address,omitempty"`
	Email      string            `json:"email,omitempty"`
	Categories []string          `json:"categories,omitempty"`
	Options    map[string]string `json:"options,omitempty"`
	TimeZone   string            `json:"time_zone,omitempty"`
	AsOf       time.Time         `json:"as_of,omitzero"`
}

// ProfileActivity is what the stored messages say. Group messages by the person aren't counted.
type ProfileActivity struct {
	FirstAt        time.Time        `json:"first_at,omitzero"`
	LastAt         time.Time        `json:"last_at,omitzero"`
	Sent           int              `json:"sent"`
	Received       int              `json:"received"`
	Unread         int              `json:"unread"`
	GroupsInCommon []store.GroupRef `json:"groups_in_common"`
}

// ProfileQueue counts the person's queue items in the last 7 days.
type ProfileQueue struct {
	Window string `json:"window"`
	store.QueueStats
}

// Profile describes one person by number or JID. Only Refresh asks WhatsApp; everything else
// comes from whatsmeow's store and wame's tables. A person wame knows nothing about still gets an
// answer with empty parts.
func (b *Bot) Profile(ctx context.Context, to string, opt ProfileOptions) (Profile, error) {
	jid, err := b.personJID(to)
	if err != nil {
		return Profile{}, err
	}
	if opt.Refresh && !opt.Full {
		return Profile{}, BadInput("a refresh needs the full profile")
	}
	opt.Details = opt.Details || opt.Full
	var lid types.JID
	if orig, err := ParseRecipient(to); err == nil && orig.Server == types.HiddenUserServer {
		lid = orig.ToNonAD()
	}
	if lid.IsEmpty() {
		lid = b.lidOf(jid)
	}
	keys := b.contactKeys(jid)
	var p Profile
	if opt.Refresh {
		refreshed, next, err := b.refreshProfile(ctx, opt.Actor, jid, lid, keys)
		if err != nil {
			return Profile{}, err
		}
		p.Refreshed, p.NextRefreshAt = &refreshed, next
	}
	cached, err := b.Store.CachedProfileOf(keys...)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return Profile{}, err
	}
	if lid.IsEmpty() && cached.LID != "" {
		lid, _ = types.ParseJID(cached.LID)
	}
	if !lid.IsEmpty() && !slices.Contains(keys, lid.String()) {
		keys = append(keys, lid.String())
	}
	own, err := b.Store.Contact(keys...)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return Profile{}, err
	}
	v := b.contactView(ctx, jid, own)
	p.JID, p.Phone, p.Names = v.JID, v.Phone, v.Names
	if !lid.IsEmpty() {
		p.LID = lid.String()
	}
	if p.Names.Business == "" && cached.VerifiedName != "" {
		p.Names.Business = cached.VerifiedName
		if p.Names.Source == SourcePush || p.Names.Source == SourceNumber {
			p.Names.Display, p.Names.Source = cached.VerifiedName, SourceBusiness
		}
	}
	p.Own = ProfileOwn{Tags: tagsOf(own)}
	p.WhatsApp.OnWhatsApp = b.onWhatsApp(jid, cached)
	if opt.Details {
		p.Own.Language, p.Own.TimeZone = own.Language, own.TimeZone
		p.WhatsApp.About, p.WhatsApp.AboutAt = cached.About, cached.AboutAt
		p.WhatsApp.VerifiedName, p.WhatsApp.VerifiedLevel = cached.VerifiedName, cached.VerifiedLevel
		p.WhatsApp.AsOf, p.WhatsApp.Stale = cached.InfoAt, stale(cached)
		st, err := b.Store.ContactStats(keys)
		if err != nil {
			return Profile{}, err
		}
		p.Activity = &ProfileActivity{FirstAt: st.FirstAt, LastAt: st.LastAt, Sent: st.Sent, Received: st.Received,
			Unread: st.Unread, GroupsInCommon: st.Groups}
		if opt.Full {
			p.Queue = &ProfileQueue{Window: "7d", QueueStats: st.Queue}
		}
	}
	if !opt.Full {
		return p, nil
	}
	p.Own.Notes = own.Notes
	p.Own.CreatedAt, p.Own.CreatedBy, p.Own.UpdatedAt, p.Own.UpdatedBy = own.CreatedAt, own.CreatedBy, own.UpdatedAt, own.UpdatedBy
	p.WhatsApp.Error, p.WhatsApp.ErrorAt = cached.Error, cached.ErrorAt
	p.WhatsApp.Picture = pictureOf(cached)
	if cached.Business != "" {
		var bi BusinessInfo
		if json.Unmarshal([]byte(cached.Business), &bi) == nil {
			bi.AsOf = cached.BusinessAt
			p.WhatsApp.Business = &bi
		}
	}
	out, err := b.Store.OptedOut(keys...)
	if err != nil {
		return Profile{}, err
	}
	p.OptedOut = &out
	if p.Tokens, err = b.tokensFor(keys); err != nil {
		return Profile{}, err
	}
	return p, nil
}

// lidOf is the person's @lid from whatsmeow's own mapping (never a lookup), or empty.
func (b *Bot) lidOf(jid types.JID) types.JID {
	if jid.Server == types.HiddenUserServer {
		return jid
	}
	if jid.Server != types.DefaultUserServer || b.Client.Store.LIDs == nil {
		return types.JID{}
	}
	lid, err := b.Client.Store.LIDs.GetLIDForPN(context.Background(), jid)
	if err != nil {
		return types.JID{}
	}
	return lid
}

func (b *Bot) onWhatsApp(jid types.JID, c store.CachedProfile) *bool {
	yes, no := true, false
	switch {
	case c.OnWhatsApp == 1:
		return &yes
	case c.OnWhatsApp == 0:
		return &no
	}
	b.mu.RLock()
	_, resolved := b.jids[jid.User]
	b.mu.RUnlock()
	if resolved {
		return &yes
	}
	return nil
}

func stale(c store.CachedProfile) bool {
	now := time.Now()
	return now.Sub(c.InfoAt) > profileInfoTTL || (c.Business != "" && now.Sub(c.BusinessAt) > profileBusinessTTL)
}

func pictureOf(c store.CachedProfile) *ProfilePicture {
	if c.PictureAt.IsZero() && c.PictureID == "" {
		return nil
	}
	p := &ProfilePicture{ID: c.PictureID, AsOf: c.PictureAt}
	if c.PictureID == store.PictureHidden {
		p.ID, p.Hidden = "", true
	}
	if time.Since(c.PictureAt) < profilePictureTTL {
		p.URL = c.PictureURL
	}
	return p
}

// tokensFor names the usable tokens limited to chats whose allowlist names this person under one
// of keys. It compares locally: opening a profile never resolves an allowlist.
func (b *Bot) tokensFor(keys []string) ([]string, error) {
	tokens, err := b.Store.Tokens()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := []string{}
	for _, t := range tokens {
		if len(t.Chats) == 0 || t.Usable(now) != nil {
			continue
		}
		if slices.ContainsFunc(t.Chats, func(c string) bool { return slices.Contains(keys, c) }) {
			out = append(out, t.Name)
		}
	}
	return out, nil
}

// claimRefresh books a refresh of key unless one ran in the last five minutes; then it returns
// when the next may run.
func (b *Bot) claimRefresh(key string, now time.Time) (time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if last, ok := b.refreshes[key]; ok && now.Sub(last) < profileRefreshEvery {
		return last.Add(profileRefreshEvery), false
	}
	if b.refreshes == nil {
		b.refreshes = map[string]time.Time{}
	}
	maps.DeleteFunc(b.refreshes, func(_ string, at time.Time) bool { return now.Sub(at) >= profileRefreshEvery })
	b.refreshes[key] = now
	return now.Add(profileRefreshEvery), true
}

func (b *Bot) releaseRefresh(key string, at time.Time) {
	b.mu.Lock()
	if b.refreshes[key] == at {
		delete(b.refreshes, key)
	}
	b.mu.Unlock()
}

// whatsAppError marks a failed WhatsApp call during a refresh: it is stored on the profile, not
// returned.
type whatsAppError struct{ error }

// refreshProfile asks WhatsApp about the person, charging each call to the lookup budget, and
// stores the answers. A WhatsApp error is stored on the cache row (and the answer says refreshed:
// false); a budget refusal before any call frees the five-minute slot again.
func (b *Bot) refreshProfile(ctx context.Context, actor string, jid, lid types.JID, keys []string) (refreshed bool, next time.Time, err error) {
	if !b.profileClient().IsLoggedIn() {
		return false, time.Time{}, errors.New("not logged in")
	}
	now := time.Now()
	next, ok := b.claimRefresh(jid.String(), now)
	if !ok {
		return false, next, nil
	}
	lookups := 0
	defer func() {
		detail := fmt.Sprintf("%d lookups", lookups)
		if lookups == 1 {
			detail = "1 lookup"
		}
		auditErr := err
		if we, ok := errors.AsType[whatsAppError](err); ok {
			auditErr, err = errors.New(cleanProfileError(we)), nil
		}
		b.Audit(actor, "contact_refresh", jid.String(), detail, auditErr)
		if lookups == 0 && err != nil {
			b.releaseRefresh(jid.String(), now)
		}
	}()
	cached, err := b.Store.CachedProfileOf(keys...)
	if errors.Is(err, store.ErrNotFound) {
		cached, err = store.CachedProfile{OnWhatsApp: -1}, nil
	}
	if err != nil {
		return false, next, err
	}
	c := cached
	c.JID = jid.String()
	err = b.askWhatsApp(ctx, &c, cached, jid, lid, &lookups)
	if _, ok := errors.AsType[whatsAppError](err); ok {
		c.Error, c.ErrorAt = cleanProfileError(err), time.Now()
	} else if err == nil {
		c.Error, c.ErrorAt = "", time.Time{}
	}
	if lookups > 0 {
		if serr := b.Store.PutCachedProfile(c); serr != nil {
			return false, next, serr
		}
	}
	return err == nil, next, err
}

func (b *Bot) profileClient() profileClient {
	if b.profiles != nil {
		return b.profiles
	}
	return b.Client
}

// askWhatsApp fills c from WhatsApp: IsOnWhatsApp (numbers only), GetUserInfo, the picture when its
// id changed or its URL is old, and the business profile when there is a verified name.
func (b *Bot) askWhatsApp(ctx context.Context, c *store.CachedProfile, cached store.CachedProfile, jid, lid types.JID, lookups *int) error {
	wa := b.profileClient()
	call := func() error {
		if err := b.lookup(ctx, 1); err != nil {
			return err
		}
		*lookups++
		return nil
	}
	now := time.Now()
	target := jid
	if jid.Server == types.DefaultUserServer {
		if err := call(); err != nil {
			return err
		}
		res, err := wa.IsOnWhatsApp(ctx, []string{"+" + jid.User})
		if err != nil {
			return whatsAppError{err}
		}
		c.OnWhatsApp, c.InfoAt = 0, now
		for _, r := range res {
			if r.IsIn {
				c.OnWhatsApp, target = 1, b.PhoneJID(r.JID.ToNonAD())
			}
		}
		if c.OnWhatsApp == 0 {
			c.About, c.AboutAt, c.VerifiedName, c.VerifiedLevel = "", time.Time{}, "", ""
			c.PictureID, c.PictureURL, c.Business, c.BusinessAt = "", "", "", time.Time{}
			return nil
		}
		c.JID = target.String()
	}
	if err := call(); err != nil {
		return err
	}
	infos, err := wa.GetUserInfo(ctx, []types.JID{target})
	if err != nil {
		return whatsAppError{err}
	}
	info, ok := infos[target]
	if !ok && len(infos) == 1 {
		for _, v := range infos {
			info = v
		}
	}
	c.InfoAt = now
	if ok || len(infos) == 1 {
		c.OnWhatsApp = 1
	}
	about := capRunes(StripInvisible(info.Status), maxAbout)
	if about != c.About {
		c.About, c.AboutAt = about, time.Time{}
	}
	c.VerifiedName, c.VerifiedLevel = "", ""
	if vn := info.VerifiedName; vn != nil {
		c.VerifiedName = capRunes(cleanName(vn.Details.GetVerifiedName()), maxVerifiedName)
		c.VerifiedLevel = capRunes(cleanName(vn.VerifiedLevel), 32)
	}
	if !info.LID.IsEmpty() {
		c.LID = info.LID.ToNonAD().String()
		b.Store.RelinkLater(c.LID)
	} else if !lid.IsEmpty() {
		c.LID = lid.String()
	}
	if err := b.askPicture(ctx, c, cached, target, capRunes(info.PictureID, maxPictureID), call); err != nil {
		return err
	}
	if c.VerifiedName == "" {
		c.Business, c.BusinessAt = "", time.Time{}
		return nil
	}
	if err := call(); err != nil {
		return err
	}
	bp, err := wa.GetBusinessProfile(ctx, target)
	if err != nil {
		return whatsAppError{err}
	}
	c.Business, c.BusinessAt = businessJSON(bp), now
	return nil
}

func (b *Bot) askPicture(ctx context.Context, c *store.CachedProfile, cached store.CachedProfile, target types.JID, id string, call func() error) error {
	now := time.Now()
	if id == "" {
		if c.PictureID != store.PictureHidden {
			c.PictureID, c.PictureURL = "", ""
		}
		return nil
	}
	if id == cached.PictureID && cached.PictureURL != "" && now.Sub(cached.PictureAt) < profilePictureTTL {
		return nil
	}
	if err := call(); err != nil {
		return err
	}
	pic, err := b.profileClient().GetProfilePictureInfo(ctx, target, &whatsmeow.GetProfilePictureParams{Preview: true})
	switch {
	case errors.Is(err, whatsmeow.ErrProfilePictureNotSet):
		c.PictureID, c.PictureURL = "", ""
	case errors.Is(err, whatsmeow.ErrProfilePictureUnauthorized):
		c.PictureID, c.PictureURL = store.PictureHidden, ""
	case err != nil:
		return whatsAppError{err}
	case pic == nil:
		return nil
	default:
		c.PictureID, c.PictureURL = capRunes(cmp.Or(pic.ID, id), maxPictureID), ""
		if strings.HasPrefix(pic.URL, "https://") && len(pic.URL) <= maxPictureURL {
			c.PictureURL = pic.URL
		}
	}
	c.PictureAt = now
	return nil
}

func businessJSON(bp *types.BusinessProfile) string {
	if bp == nil {
		return ""
	}
	text := func(s string) string { return capRunes(StripInvisible(s), maxBusinessText) }
	bi := BusinessInfo{Address: text(bp.Address), Email: text(bp.Email), TimeZone: text(bp.BusinessHoursTimeZone)}
	for _, cat := range bp.Categories[:min(len(bp.Categories), 10)] {
		if n := text(cleanName(cat.Name)); n != "" {
			bi.Categories = append(bi.Categories, n)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(bp.ProfileOptions)) {
		if len(bi.Options) >= 10 {
			break
		}
		if bi.Options == nil {
			bi.Options = map[string]string{}
		}
		bi.Options[text(cleanName(k))] = text(bp.ProfileOptions[k])
	}
	data, _ := json.Marshal(bi)
	if len(data) > maxBusinessJSON {
		bi.Options = nil
		data, _ = json.Marshal(bi)
	}
	return string(data)
}

var urlPattern = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://\S+`)

// cleanProfileError is what a refresh error says on the cache row: one line, no URLs, capped.
func cleanProfileError(err error) string {
	return capRunes(cleanName(urlPattern.ReplaceAllString(err.Error(), "[url]")), maxProfileError)
}

// capRunes cuts s to at most n runes.
func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
