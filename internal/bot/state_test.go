package bot

import (
	"context"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/williamtcastro/wame/internal/store"
)

func TestReadOnPhoneClearsUnread(t *testing.T) {
	b := newTestBot(t)
	sub, unsub := b.Subscribe()
	defer unsub()
	for _, id := range []string{"R1", "R2"} {
		if _, _, err := b.Store.Save(store.Message{ID: id, Chat: maria, Sender: maria, Type: "text", Text: id, Timestamp: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	chat, _ := types.ParseJID(maria)
	b.handle(&events.Receipt{
		MessageSource: types.MessageSource{Chat: chat, Sender: chat, IsFromMe: true},
		MessageIDs:    []types.MessageID{"R1", "R2"},
		Type:          types.ReceiptTypeReadSelf,
	})
	if got := b.Store.ReadSeq(maria); got == 0 {
		t.Fatal("read-self receipt should move the read position")
	}
	select {
	case v := <-sub:
		if r, ok := v.(store.ChatRead); !ok || r.Chat != maria {
			t.Fatalf("published %#v, want ChatRead for %s", v, maria)
		}
	default:
		t.Fatal("no ChatRead published")
	}

	b.handle(&events.Receipt{MessageSource: types.MessageSource{Chat: chat, Sender: chat}, MessageIDs: []types.MessageID{"R1"}, Type: types.ReceiptTypeRead})
	select {
	case v := <-sub:
		t.Fatalf("someone else reading our message must not change our read state, got %#v", v)
	default:
	}
}

func TestCachedGroupNamesBeforeLogin(t *testing.T) {
	b := newTestBot(t)
	if err := b.Store.SaveGroups([]store.Group{{JID: group, Name: "Family", Admins: []string{ana}}}); err != nil {
		t.Fatal(err)
	}
	b.loadCachedGroups()
	if got := b.ChatName(context.Background(), group, ""); got != "Family" {
		t.Fatalf("ChatName before login = %q, want the cached name", got)
	}
	if !b.isGroupAdmin(context.Background(), group, ana) {
		t.Fatal("cached admins should be used too")
	}
}

func TestAdminRevokeInGroup(t *testing.T) {
	b := newTestBot(t)
	if err := b.Store.SaveGroups([]store.Group{{JID: group, Name: "Family", Admins: []string{ana}}}); err != nil {
		t.Fatal(err)
	}
	b.loadCachedGroups()
	if _, ok, err := b.Store.Save(store.Message{ID: "G2", Chat: group, Sender: maria, IsGroup: true, Type: "text", Text: "oi", Timestamp: time.Now()}); !ok || err != nil {
		t.Fatal(err)
	}
	b.handle(protocolEvent(group, ana, false, revoke("G2")))
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if m, _ := b.Store.Get(group, "G2"); m.Deleted {
			return
		}
	}
	t.Fatal("a group admin's revoke wasn't applied")
}

func TestTypingExpiryKeepsNewerTimer(t *testing.T) {
	b := newTestBot(t)
	b.trackTyping(maria, true)
	b.mu.RLock()
	old := b.typingIn[maria]
	b.mu.RUnlock()
	b.trackTyping(maria, true)
	b.typingExpired(maria, old)
	b.mu.RLock()
	_, still := b.typingIn[maria]
	b.mu.RUnlock()
	if !still {
		t.Fatal("an expired timer removed the newer typing indicator")
	}
	b.trackTyping(maria, false)
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.typingIn) != 0 || b.wantedPresence() != types.PresenceUnavailable {
		t.Fatalf("typing = %v", b.typingIn)
	}
}

func TestCloseStopsBackgroundWork(t *testing.T) {
	b := newTestBot(t)
	started := make(chan struct{})
	stopped := false
	b.Go(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		stopped = true
	})
	<-started
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if !stopped {
		t.Fatal("Close returned before the background work stopped")
	}
	ran := false
	b.Go(func(context.Context) { ran = true })
	if err := b.Close(); err != nil || ran {
		t.Fatalf("after Close: Go ran %v, second Close %v", ran, err)
	}
}

func TestAllowlistOfflineCachedBriefly(t *testing.T) {
	b := newTestBot(t)
	ctx := t.Context()
	b.ExpandAllowlist(ctx, []string{maria})
	b.mu.RLock()
	defer b.mu.RUnlock()
	c, ok := b.allowlists[maria]
	if !ok || time.Until(c.expires) > allowlistRetryAfter {
		t.Fatalf("offline result = %+v, %v; want cached for at most %s", c, ok, allowlistRetryAfter)
	}
}
