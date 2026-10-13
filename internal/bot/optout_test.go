package bot

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/williamtcastro/wame/internal/store"
)

func incomingText(chat, sender, id, text string, fromMe bool) *events.Message {
	c, _ := types.ParseJID(chat)
	s, _ := types.ParseJID(sender)
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: c, Sender: s, IsFromMe: fromMe, IsGroup: c.Server == types.GroupServer},
			ID:            id, Timestamp: time.Now(),
		},
		Message: &waE2E.Message{Conversation: new(text)},
	}
}

func TestOptOutKeyword(t *testing.T) {
	for text, want := range map[string]string{
		"STOP": "STOP", " stop \n": "STOP", "Sáir": "SAIR", "PARAR": "PARAR", "cancelar": "CANCELAR",
		"Unsubscribe": "UNSUBSCRIBE", "descadastrar": "DESCADASTRAR",
		"please stop": "", "stop!": "", "": "", "!ping": "",
	} {
		if got := optOutKeyword(text); got != want {
			t.Errorf("optOutKeyword(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestIncomingOptOut(t *testing.T) {
	b := newTestBot(t)
	b.SetAutoReply(true)
	b.handle(incomingText(group, maria, "G1", "STOP", false))
	b.handle(incomingText(john, john, "M0", "stop", true))
	b.handle(incomingText(ana, ana, "A1", "please stop", false))
	if list, _ := b.OptOuts(); len(list) != 0 {
		t.Fatalf("groups, own messages and longer texts must not opt out: %+v", list)
	}

	b.handle(incomingText(maria, maria, "M1", " Parar ", false))
	b.handle(incomingText(maria, maria, "M2", "STOP", false))
	list, err := b.OptOuts()
	if err != nil || len(list) != 1 || list[0].Chat != maria || list[0].Reason != "PARAR" || list[0].By != "whatsapp" {
		t.Fatalf("%v %+v", err, list)
	}
	log, _ := b.Store.AuditLog(10, 0)
	if len(log) != 1 || log[0].Action != "optout_add" || log[0].Actor != "whatsapp" || log[0].Target != maria {
		t.Fatalf("one audit entry for the first keyword only: %+v", log)
	}
	if s := b.AutoReplyStats(); s.LastMinute != 0 {
		t.Fatalf("an opt-out must never be answered: %+v", s)
	}
	if m, err := b.Store.Get(maria, "M1"); err != nil || m.Text != " Parar " {
		t.Fatalf("the message itself is stored as usual: %v %+v", err, m)
	}

	mariaJID, _ := types.ParseJID(maria)
	johnJID, _ := types.ParseJID(john)
	err = b.optedOut(mariaJID, maria)
	if !isInputError(err) || err.Error() != errOptedOut {
		t.Fatalf("optedOut = %v", err)
	}
	if err := b.optedOut(johnJID, john); err != nil {
		t.Fatal(err)
	}
}

func TestManualOptOut(t *testing.T) {
	b := newTestBot(t)
	ctx := context.Background()
	o, added, err := b.AddOptOut(ctx, "ops", "+55 11 98888-7777")
	if err != nil || !added || o.Chat != john || o.By != "ops" || o.Reason != "manual" {
		t.Fatalf("%v %v %+v", err, added, o)
	}
	if _, added, err := b.AddOptOut(ctx, "ops", john); err != nil || added {
		t.Fatalf("already listed: %v %v", err, added)
	}
	if _, _, err := b.AddOptOut(ctx, "ops", group); !isInputError(err) {
		t.Fatalf("a group can't opt out: %v", err)
	}
	if _, _, err := b.AddOptOut(ctx, "ops", "abc"); !isInputError(err) {
		t.Fatalf("bad number: %v", err)
	}
	if err := b.RemoveOptOut(ctx, "ops", "5511988887777"); err != nil {
		t.Fatal(err)
	}
	if err := b.RemoveOptOut(ctx, "ops", john); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second remove = %v", err)
	}
	log, _ := b.Store.AuditLog(10, 0)
	var actions []string
	for _, e := range log {
		actions = append(actions, e.Action+":"+e.Actor)
	}
	if len(log) != 6 || log[0].Action != "optout_remove" || log[0].OK || log[1].Action != "optout_remove" || !log[1].OK || log[5].Detail != "added" {
		t.Fatalf("audit %v", actions)
	}
}

func TestOptOutUnderLIDStillBlocks(t *testing.T) {
	b := newTestBot(t)
	b.Client.Store.LIDs = sqlstore.NewWithDB(b.db, "sqlite3", waLog.Noop).LIDMap
	ctx := context.Background()
	const mariaLID = "100000000000001@lid"
	b.handle(incomingText(mariaLID, mariaLID, "L1", "STOP", false))
	if list, _ := b.OptOuts(); len(list) != 1 || list[0].Chat != mariaLID {
		t.Fatalf("stored under the @lid while its number is unknown: %+v", list)
	}
	lid, _ := types.ParseJID(mariaLID)
	pn, _ := types.ParseJID(maria)
	if err := b.Client.Store.LIDs.PutLIDMapping(ctx, lid, pn); err != nil {
		t.Fatal(err)
	}
	if err := b.optedOut(pn, "5511999999999"); !isInputError(err) {
		t.Fatalf("a queued send to the number must be blocked once the link is known: %v", err)
	}
	b.handle(incomingText(maria, maria, "M1", "STOP", false))
	if log, _ := b.Store.AuditLog(10, 0); len(log) != 1 {
		t.Fatalf("a second keyword under the other ID must not add a duplicate: %+v", log)
	}
	if list, _ := b.OptOuts(); len(list) != 1 || list[0].Chat != maria {
		t.Fatalf("listing moves it onto the number: %+v", list)
	}
	if err := b.RemoveOptOut(ctx, "ops", "5511999999999"); err != nil {
		t.Fatal(err)
	}
	if err := b.optedOut(pn, maria); err != nil {
		t.Fatalf("removed: %v", err)
	}
}

func TestOptOutAddedOfflineMatchesTheResolvedJID(t *testing.T) {
	b := newTestBot(t)
	if _, _, err := b.AddOptOut(context.Background(), "ops", "5511988887777"); err != nil {
		t.Fatal(err)
	}
	resolved := types.NewJID("551188887777", types.DefaultUserServer)
	if err := b.optedOut(resolved, "5511988887777"); !isInputError(err) {
		t.Fatalf("WhatsApp may resolve a number to another JID; the number as typed must still match: %v", err)
	}
	if err := b.optedOut(resolved, "+55 11 8888-7777"); err != nil {
		t.Fatalf("another person: %v", err)
	}
}
