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
