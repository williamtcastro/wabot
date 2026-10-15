package bot

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waVnameCert"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/williamtcastro/wame/internal/store"
)

const (
	mariaLID = "100000000000001@lid"
	family   = "120363012345678901@g.us"
)

// fakeWA stands in for the four whatsmeow calls a refresh makes.
type fakeWA struct {
	mu       sync.Mutex
	loggedIn bool
	calls    []string
	notOnWA  bool
	info     types.UserInfo
	infoErr  error
	pic      *types.ProfilePictureInfo
	picErr   error
	business *types.BusinessProfile
	block    chan struct{}
}

func (f *fakeWA) note(call string) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
}

func (f *fakeWA) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeWA) IsLoggedIn() bool { return f.loggedIn }

func (f *fakeWA) IsOnWhatsApp(_ context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error) {
	f.note("IsOnWhatsApp")
	return []types.IsOnWhatsAppResponse{{Query: phones[0], IsIn: !f.notOnWA, JID: jidOf(strings.TrimPrefix(phones[0], "+") + "@s.whatsapp.net")}}, nil
}

func (f *fakeWA) GetUserInfo(_ context.Context, jids []types.JID) (map[types.JID]types.UserInfo, error) {
	f.note("GetUserInfo")
	if f.block != nil {
		<-f.block
	}
	if f.infoErr != nil {
		return nil, f.infoErr
	}
	return map[types.JID]types.UserInfo{jids[0]: f.info}, nil
}

func (f *fakeWA) GetProfilePictureInfo(context.Context, types.JID, *whatsmeow.GetProfilePictureParams) (*types.ProfilePictureInfo, error) {
	f.note("GetProfilePictureInfo")
	return f.pic, f.picErr
}

func (f *fakeWA) GetBusinessProfile(context.Context, types.JID) (*types.BusinessProfile, error) {
	f.note("GetBusinessProfile")
	return f.business, nil
}

func withFake(t *testing.T, b *Bot) *fakeWA {
	t.Helper()
	f := &fakeWA{loggedIn: true}
	b.profiles = f
	return f
}

func verified(name string) *types.VerifiedName {
	return &types.VerifiedName{Details: &waVnameCert.VerifiedNameCertificate_Details{VerifiedName: &name}, VerifiedLevel: "high"}
}

func refresh(t *testing.T, b *Bot, to string) Profile {
	t.Helper()
	p, err := b.Profile(t.Context(), to, ProfileOptions{Full: true, Refresh: true, Actor: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func lastAudit(t *testing.T, b *Bot) store.AuditEntry {
	t.Helper()
	log, err := b.Store.AuditLog(1, 0)
	if err != nil || len(log) == 0 {
		t.Fatalf("audit = %v, %v", log, err)
	}
	return log[0]
}

func TestProfileRefreshChargesLookups(t *testing.T) {
	b := newTestBot(t)
	b.Lookups = NewLimiter(5, 0, time.Millisecond)
	f := withFake(t, b)
	f.info = types.UserInfo{Status: "Hey there\x1b[31m", PictureID: "1712345678", VerifiedName: verified("Maria Ltd"), LID: jidOf(mariaLID)}
	f.pic = &types.ProfilePictureInfo{ID: "1712345678", URL: "https://cdn.example/p.jpg"}
	f.business = &types.BusinessProfile{Email: "maria@example.com", Categories: []types.Category{{Name: "Bakery"}}}

	p := refresh(t, b, "5511999999999")
	if got := f.Calls(); len(got) != 4 {
		t.Fatalf("calls = %v", got)
	}
	if b.Lookups.Stats().LastMinute != 4 {
		t.Fatalf("lookups charged = %d, want 4", b.Lookups.Stats().LastMinute)
	}
	if e := lastAudit(t, b); e.Action != "contact_refresh" || e.Detail != "4 lookups" || !e.OK || e.Target != maria {
		t.Fatalf("audit = %+v", e)
	}
	w := p.WhatsApp
	if p.Refreshed == nil || !*p.Refreshed || w.OnWhatsApp == nil || !*w.OnWhatsApp || w.About != "Hey there[31m" ||
		w.VerifiedName != "Maria Ltd" || w.Picture == nil || w.Picture.URL != "https://cdn.example/p.jpg" ||
		w.Business == nil || w.Business.Email != "maria@example.com" || w.Stale || p.LID != mariaLID {
		t.Fatalf("profile = %+v / %+v", p, w)
	}
	if p.Names.Display != "Maria Ltd" || p.Names.Source != SourceBusiness {
		t.Errorf("names = %+v", p.Names)
	}

	again := refresh(t, b, "5511999999999")
	if again.Refreshed == nil || *again.Refreshed || again.NextRefreshAt.Before(time.Now().Add(4*time.Minute)) {
		t.Fatalf("second refresh = %v next %v", again.Refreshed, again.NextRefreshAt)
	}
	if len(f.Calls()) != 4 {
		t.Fatal("a refresh inside five minutes asked WhatsApp again")
	}
	if again.WhatsApp.About != w.About {
		t.Fatal("the second refresh should answer the cache")
	}

	// One lookup left: John's first two calls are reserved together, so nothing is asked.
	_, err := b.Profile(t.Context(), john, ProfileOptions{Full: true, Refresh: true, Actor: "ops"})
	if _, ok := errors.AsType[RateLimitError](err); !ok {
		t.Fatalf("over budget: %v", err)
	}
	if got := f.Calls(); len(got) != 4 {
		t.Fatalf("calls = %v", got)
	}
	if _, err := b.Store.CachedProfileOf(john); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a refused refresh must not store anything")
	}
}

func TestProfileRefreshBudgetMidway(t *testing.T) {
	b := newTestBot(t)
	b.Lookups = NewLimiter(3, 0, time.Millisecond)
	f := withFake(t, b)
	f.info = types.UserInfo{Status: "Hey there", PictureID: "1", VerifiedName: verified("Maria Ltd")}
	f.pic = &types.ProfilePictureInfo{ID: "1", URL: "https://cdn.example/p.jpg"}
	_, err := b.Profile(t.Context(), maria, ProfileOptions{Full: true, Refresh: true, Actor: "ops"})
	if _, ok := errors.AsType[RateLimitError](err); !ok {
		t.Fatalf("business over budget: %v", err)
	}
	c, err := b.Store.CachedProfileOf(maria)
	if err != nil || c.About != "Hey there" || c.InfoAt.IsZero() || c.PictureID != "1" {
		t.Fatalf("what was learned should be kept: %+v, %v", c, err)
	}
}

func TestProfileRefreshDoesntMarkOldInfoFresh(t *testing.T) {
	b := newTestBot(t)
	f := withFake(t, b)
	old := time.Now().Add(-48 * time.Hour).Truncate(time.Millisecond)
	if err := b.Store.PutCachedProfile(store.CachedProfile{JID: maria, OnWhatsApp: 1, About: "old", InfoAt: old}); err != nil {
		t.Fatal(err)
	}
	f.infoErr = errors.New("timeout")
	p := refresh(t, b, maria)
	if !p.WhatsApp.Stale || !p.WhatsApp.AsOf.Equal(old) || p.WhatsApp.About != "old" {
		t.Fatalf("after a failed GetUserInfo: stale %v as_of %v about %q", p.WhatsApp.Stale, p.WhatsApp.AsOf, p.WhatsApp.About)
	}
}

func TestProfileRefreshReplacesOtherVariant(t *testing.T) {
	b := newTestBot(t)
	withFake(t, b)
	if err := b.Store.PutCachedProfile(store.CachedProfile{JID: "552187654321@s.whatsapp.net", OnWhatsApp: 0, InfoAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// The fake resolves the number as typed, so this lands under the 9-digit form.
	p := refresh(t, b, "5521987654321")
	if p.WhatsApp.OnWhatsApp == nil || !*p.WhatsApp.OnWhatsApp {
		t.Fatalf("the old not-on-WhatsApp row still shows: %+v", p.WhatsApp)
	}
	if again, _ := b.Profile(t.Context(), "552187654321", ProfileOptions{}); again.WhatsApp.OnWhatsApp == nil || !*again.WhatsApp.OnWhatsApp {
		t.Fatal("the 8-digit form should read the new row")
	}
	if next := refresh(t, b, "552187654321"); *next.Refreshed {
		t.Fatal("both forms of a number share one five-minute slot")
	}
}

func TestLogoutDuringRefreshKeepsNothing(t *testing.T) {
	b := newTestBot(t)
	f := withFake(t, b)
	gate := make(chan struct{})
	f.block = gate
	done := make(chan error, 1)
	go func() {
		_, err := b.Profile(context.Background(), maria, ProfileOptions{Full: true, Refresh: true, Actor: "ops"})
		done <- err
	}()
	for len(f.Calls()) < 2 {
		time.Sleep(time.Millisecond)
	}
	b.logout = func(context.Context) error { return nil }
	if err := b.Logout(t.Context(), "ops"); err != nil {
		t.Fatal(err)
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := b.Store.CachedProfileOf(maria); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a refresh that outlived the logout wrote its profile back")
	}
}

func TestProfileRefreshRefusedBeforeAnyCallFreesTheSlot(t *testing.T) {
	b := newTestBot(t)
	b.Lookups = NewLimiter(2, 0, time.Millisecond)
	if _, err := b.Lookups.ReserveN("lookup", 1); err != nil {
		t.Fatal(err)
	}
	f := withFake(t, b)
	_, err := b.Profile(t.Context(), john, ProfileOptions{Full: true, Refresh: true})
	if _, ok := errors.AsType[RateLimitError](err); !ok || len(f.Calls()) != 0 {
		t.Fatalf("err %v calls %v", err, f.Calls())
	}
	b.Lookups = NewLimiter(0, 0, 0)
	if p := refresh(t, b, john); !*p.Refreshed {
		t.Fatal("a refused refresh should not hold the five-minute slot")
	}
}

func TestProfileWithoutRefreshNeverAsksWhatsApp(t *testing.T) {
	b := newTestBot(t)
	f := withFake(t, b)
	for _, opt := range []ProfileOptions{{}, {Details: true}, {Full: true}} {
		if _, err := b.Profile(t.Context(), maria, opt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.Profile(t.Context(), maria, ProfileOptions{Details: true, Refresh: true}); !isInputError(err) {
		t.Fatalf("refresh without Full: %v", err)
	}
	if _, err := b.Profile(t.Context(), family, ProfileOptions{}); !isInputError(err) {
		t.Fatalf("group: %v", err)
	}
	if p, _ := b.Profile(t.Context(), john, ProfileOptions{Full: true}); p.WhatsApp.OnWhatsApp != nil {
		t.Fatalf("never asked should be unknown, got %v", *p.WhatsApp.OnWhatsApp)
	}
	if len(f.Calls()) != 0 || b.Lookups.Stats().LastMinute != 0 {
		t.Fatalf("calls %v lookups %d", f.Calls(), b.Lookups.Stats().LastMinute)
	}
	f.loggedIn = false
	if _, err := b.Profile(t.Context(), maria, ProfileOptions{Full: true, Refresh: true}); err == nil || isInputError(err) {
		t.Fatalf("logged out refresh: %v", err)
	}
}

func TestProfileBasicHidesDetails(t *testing.T) {
	b := newTestBot(t)
	patch(t, b, maria, ContactInput{Name: ptr("Maria"), Notes: ptr("prefers mornings"), Tags: &[]string{"vip"}, TimeZone: ptr("America/Sao_Paulo")})
	if err := b.Store.PutCachedProfile(store.CachedProfile{JID: maria, OnWhatsApp: 1, About: "Hey there", PictureURL: "https://cdn.example/p.jpg", PictureID: "1", PictureAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	p, err := b.Profile(t.Context(), "5511999999999", ProfileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Names.Display != "Maria" || len(p.Own.Tags) != 1 || p.Own.Notes != "" || p.Own.TimeZone != "" ||
		p.WhatsApp.About != "" || p.WhatsApp.Picture != nil || p.Activity != nil || p.Queue != nil || p.OptedOut != nil || p.Tokens != nil {
		t.Fatalf("basic profile = %+v", p)
	}
	if p.WhatsApp.OnWhatsApp == nil || !*p.WhatsApp.OnWhatsApp {
		t.Fatal("on_whatsapp is basic")
	}
	d, _ := b.Profile(t.Context(), maria, ProfileOptions{Details: true})
	if d.Own.Notes != "" || d.WhatsApp.Picture != nil || d.Queue != nil || d.OptedOut != nil || d.Tokens != nil ||
		d.WhatsApp.About != "Hey there" || d.Activity == nil || d.Own.TimeZone != "America/Sao_Paulo" {
		t.Fatalf("details profile = %+v", d)
	}
	full, _ := b.Profile(t.Context(), maria, ProfileOptions{Full: true})
	if full.Own.Notes != "prefers mornings" || full.WhatsApp.Picture == nil || full.WhatsApp.Picture.URL == "" || full.OptedOut == nil {
		t.Fatalf("full profile = %+v", full)
	}
}

func TestProfileRefreshErrors(t *testing.T) {
	b := newTestBot(t)
	f := withFake(t, b)
	f.info = types.UserInfo{PictureID: "1"}
	f.picErr = whatsmeow.ErrProfilePictureUnauthorized
	if p := refresh(t, b, maria); p.WhatsApp.Picture == nil || !p.WhatsApp.Picture.Hidden || p.WhatsApp.Picture.ID != "" {
		t.Fatalf("hidden picture = %+v", p.WhatsApp.Picture)
	}
	f.picErr = whatsmeow.ErrProfilePictureNotSet
	if p := refresh(t, b, john); p.WhatsApp.Picture == nil || p.WhatsApp.Picture.Hidden || p.WhatsApp.Picture.ID != "" || p.WhatsApp.Error != "" {
		t.Fatalf("no picture = %+v", p.WhatsApp)
	}
	f.infoErr = errors.New("server said no for 552187654321 at https://example.com/secret?x=1\nsecond line")
	p := refresh(t, b, "552187654321")
	if p.Refreshed == nil || *p.Refreshed || strings.Contains(p.WhatsApp.Error, "https://") || strings.Contains(p.WhatsApp.Error, "\n") || p.WhatsApp.Error == "" {
		t.Fatalf("error profile = %v %q", p.Refreshed, p.WhatsApp.Error)
	}
	if e := lastAudit(t, b); e.OK || e.Detail != "2 lookups" || strings.Contains(e.Error, "https://") || strings.Contains(e.Error, "secret") {
		t.Fatalf("audit = %+v", e)
	}

	b = newTestBot(t)
	f = withFake(t, b)
	f.notOnWA = true
	n := len(f.Calls())
	if p := refresh(t, b, "5521987654321"); p.WhatsApp.OnWhatsApp == nil || *p.WhatsApp.OnWhatsApp || len(f.Calls()) != n+1 {
		t.Fatalf("not on WhatsApp = %+v, calls %v", p.WhatsApp, f.Calls()[n:])
	}
}

func TestProfileEventsTouchExistingRowsOnly(t *testing.T) {
	b := newTestBot(t)
	at := time.Now()
	b.handle(&events.UserAbout{JID: jidOf(john), Status: "stranger", Timestamp: at})
	b.handle(&events.Picture{JID: jidOf(john), PictureID: "9"})
	b.handle(&events.BusinessName{JID: jidOf(john), NewBusinessName: "John Ltd"})
	if _, err := b.Store.CachedProfileOf(john); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("events created a row: %v", err)
	}
	for _, jid := range []string{maria, family} {
		if err := b.Store.PutCachedProfile(store.CachedProfile{JID: jid, OnWhatsApp: 1, PictureID: "1", PictureURL: "https://cdn.example/1"}); err != nil {
			t.Fatal(err)
		}
	}
	b.handle(&events.UserAbout{JID: jidOf(maria), Status: "new ‮about", Timestamp: at})
	b.handle(&events.Picture{JID: jidOf(maria), Remove: true})
	b.handle(&events.Picture{JID: jidOf(family), PictureID: "2"})
	c, _ := b.Store.CachedProfileOf(maria)
	if c.About != "new about" || c.PictureID != "" || c.PictureURL != "" {
		t.Fatalf("maria = %+v", c)
	}
	if g, _ := b.Store.CachedProfileOf(family); g.PictureID != "1" {
		t.Fatal("group pictures must be ignored")
	}
}

// seedContactState gives maria an own row, a cached profile, a group membership and a message.
func seedContactState(t *testing.T, b *Bot) {
	t.Helper()
	patch(t, b, maria, ContactInput{Name: ptr("Maria"), Notes: ptr("prefers mornings")})
	if err := b.Store.PutCachedProfile(store.CachedProfile{JID: maria, OnWhatsApp: 1}); err != nil {
		t.Fatal(err)
	}
	if err := b.Store.SaveGroups([]store.Group{{JID: family, Name: "Family", Members: []store.GroupMember{{JID: maria}}}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Store.Save(store.Message{ID: "W1", Chat: maria, Sender: maria, Type: "text", Text: "oi", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func contactStateLeft(t *testing.T, b *Bot) (own, cache, groups bool) {
	t.Helper()
	_, err := b.Store.Contact(maria)
	own = err == nil
	_, err = b.Store.CachedProfileOf(maria)
	cache = err == nil
	g, _ := b.Store.GroupsWith(maria)
	return own, cache, len(g) > 0
}

func TestLogoutWipesContactState(t *testing.T) {
	for _, path := range []string{"event", "explicit", "failed", "disconnect"} {
		t.Run(path, func(t *testing.T) {
			b := newTestBot(t)
			seedContactState(t, b)
			if b.ChatName(t.Context(), maria, "") != "Maria" {
				t.Fatal("own name not loaded")
			}
			switch path {
			case "event":
				b.handle(&events.LoggedOut{})
			case "explicit", "failed":
				b.logout = func(context.Context) error {
					if path == "failed" {
						return errors.New("offline")
					}
					return nil
				}
				err := b.Logout(t.Context(), "ops")
				if (err != nil) != (path == "failed") {
					t.Fatalf("Logout = %v", err)
				}
			case "disconnect":
				b.handle(&events.Disconnected{})
			}
			own, cache, groups := contactStateLeft(t, b)
			wiped := path == "event" || path == "explicit"
			if own == wiped || cache == wiped || groups == wiped {
				t.Fatalf("own %v cache %v groups %v, want wiped=%v", own, cache, groups, wiped)
			}
			if msgs, _ := b.Store.Messages(maria, 10); len(msgs) != 1 {
				t.Fatalf("messages = %d, want them kept", len(msgs))
			}
			if !wiped {
				return
			}
			if got := b.ChatName(t.Context(), maria, ""); got != "+5511999999999" {
				t.Fatalf("name after wipe = %q", got)
			}
			e := lastAudit(t, b)
			if e.Action != "contacts_wipe" || e.Detail != "1 contacts" || !e.OK || strings.Contains(e.Detail, "Maria") {
				t.Fatalf("audit = %+v", e)
			}
		})
	}
}

func TestProfileLID(t *testing.T) {
	b := newTestBot(t)
	f := withFake(t, b)
	f.loggedIn = false
	if p, _ := b.Profile(t.Context(), maria, ProfileOptions{}); p.LID != "" {
		t.Fatalf("lid before login = %q", p.LID)
	}
	if err := b.Store.PutCachedProfile(store.CachedProfile{JID: john, OnWhatsApp: 1, LID: "100000000000002@lid"}); err != nil {
		t.Fatal(err)
	}
	if p, _ := b.Profile(t.Context(), john, ProfileOptions{}); p.LID != "100000000000002@lid" {
		t.Fatalf("lid from the cache = %q", p.LID)
	}

	withLIDs(t, b)
	if err := b.Client.Store.LIDs.PutLIDMapping(t.Context(), jidOf(mariaLID), jidOf(maria)); err != nil {
		t.Fatal(err)
	}
	if p, _ := b.Profile(t.Context(), maria, ProfileOptions{}); p.LID != mariaLID || p.JID != maria {
		t.Fatalf("lid from the store = %+v", p)
	}
	p, err := b.Profile(t.Context(), mariaLID, ProfileOptions{})
	if err != nil || p.JID != maria || p.Phone != "+5511999999999" || p.LID != mariaLID {
		t.Fatalf("by @lid = %+v, %v", p, err)
	}
	if len(f.Calls()) != 0 {
		t.Fatalf("calls = %v", f.Calls())
	}
}

func TestProfileStatsAndPrune(t *testing.T) {
	b := newTestBot(t)
	if _, _, err := b.Store.Save(store.Message{ID: "M1", Chat: maria, Sender: maria, Type: "text", Text: "oi", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := b.Store.SaveGroups([]store.Group{{JID: family, Name: "Family", Members: []store.GroupMember{{JID: maria}}}}); err != nil {
		t.Fatal(err)
	}
	p, err := b.Profile(t.Context(), maria, ProfileOptions{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.Activity.Received != 1 || p.Activity.Unread != 1 || len(p.Activity.GroupsInCommon) != 1 || p.Queue.Window != "7d" {
		t.Fatalf("activity = %+v queue %+v", p.Activity, p.Queue)
	}

	if err := b.Store.PutCachedProfile(store.CachedProfile{JID: maria, OnWhatsApp: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.Exec(`UPDATE wame_contact_cache SET updated_at = ?`, time.Now().Add(-ProfileCacheKeep-time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	b.pruneOutbox()
	if _, err := b.Store.CachedProfileOf(maria); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("old cache rows should be pruned")
	}
}

func TestMemberJIDPrefersTheNumber(t *testing.T) {
	if got := memberJID(jidOf(mariaLID), jidOf(maria)); got.String() != maria {
		t.Fatalf("lid with a number = %s", got)
	}
	if got := memberJID(jidOf(mariaLID), types.JID{}); got.String() != mariaLID {
		t.Fatalf("lid alone = %s", got)
	}
	if got := memberJID(jidOf(john), types.JID{}); got.String() != john {
		t.Fatalf("phone = %s", got)
	}
}

func TestPictureEventCantFakeHidden(t *testing.T) {
	b := newTestBot(t)
	if err := b.Store.PutCachedProfile(store.CachedProfile{JID: maria, PictureID: "1"}); err != nil {
		t.Fatal(err)
	}
	b.handle(&events.Picture{JID: jidOf(maria), PictureID: store.PictureHidden})
	if c, _ := b.Store.CachedProfileOf(maria); c.PictureID == store.PictureHidden {
		t.Fatal(`a picture id of "-" must not read as hidden`)
	}
}

func TestLogoutForgetsResolvedNumbers(t *testing.T) {
	b := newTestBot(t)
	b.mu.Lock()
	b.jids["5511999999999"] = jidOf(maria)
	b.mu.Unlock()
	b.handle(&events.LoggedOut{})
	if p, _ := b.Profile(t.Context(), maria, ProfileOptions{}); p.WhatsApp.OnWhatsApp != nil {
		t.Fatal("on_whatsapp should be unknown after logout")
	}
}
