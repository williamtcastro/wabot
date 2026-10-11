package bot

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/williamtcastro/wabot/internal/store"
)

// findTarget looks up a stored message for react/edit/delete/read.
func (b *Bot) findTarget(ctx context.Context, chat, id string) (store.Message, types.JID, error) {
	if id == "" {
		return store.Message{}, types.JID{}, BadInput("id is required")
	}
	jid, err := b.resolveOnline(ctx, chat)
	if err != nil {
		return store.Message{}, jid, err
	}
	m, err := b.Store.Get(b.PhoneJID(jid).String(), id)
	if err != nil {
		return m, jid, err
	}
	if raw, err := types.ParseJID(m.RawChat); err == nil && m.RawChat != "" {
		jid = raw
	}
	return m, jid, nil
}

func (b *Bot) React(ctx context.Context, actor, chat, id, emoji string) (store.Message, error) {
	return b.audited(actor, "react", chat, fmt.Sprintf("%s on %s", emoji, id), func() (store.Message, error) {
		target, jid, err := b.findTarget(ctx, chat, id)
		if err != nil {
			return store.Message{}, err
		}
		return b.deliver(ctx, jid, b.Client.BuildReaction(jid, senderJID(target), id, emoji), store.Message{Type: "reaction", Text: emoji, ReplyTo: id})
	})
}

func (b *Bot) Edit(ctx context.Context, actor, chat, id, text string) (store.Message, error) {
	return b.audited(actor, "edit", chat, id, func() (store.Message, error) {
		if strings.TrimSpace(text) == "" {
			return store.Message{}, BadInput("text is empty")
		}
		target, jid, err := b.findTarget(ctx, chat, id)
		switch {
		case err != nil:
			return store.Message{}, err
		case !target.FromMe:
			return store.Message{}, BadInput("you can only edit your own messages")
		case time.Since(target.Timestamp) > whatsmeow.EditWindow:
			return store.Message{}, BadInput("WhatsApp only allows edits within %s of sending", whatsmeow.EditWindow)
		case target.Type != "text":
			return store.Message{}, BadInput("only text messages can be edited")
		}
		if err := b.sendControl(ctx, jid, b.Client.BuildEdit(jid, id, &waE2E.Message{Conversation: proto.String(text)})); err != nil {
			return store.Message{}, err
		}
		b.applyEdit(target.Chat, id, text)
		return b.Store.Get(target.Chat, id)
	})
}

func (b *Bot) Delete(ctx context.Context, actor, chat, id string) (store.Message, error) {
	return b.audited(actor, "delete", chat, id, func() (store.Message, error) {
		target, jid, err := b.findTarget(ctx, chat, id)
		if err != nil {
			return store.Message{}, err
		}
		if !target.FromMe && !target.IsGroup {
			return store.Message{}, BadInput("you can only delete other people's messages in groups where you're an admin")
		}
		if err := b.sendControl(ctx, jid, b.Client.BuildRevoke(jid, senderJID(target), id)); err != nil {
			return store.Message{}, err
		}
		b.applyDelete(target.Chat, id)
		return b.Store.Get(target.Chat, id)
	})
}

// MarkRead sends read receipts for the given incoming messages, or the latest 50 in the chat when ids is empty.
func (b *Bot) MarkRead(ctx context.Context, actor, chat string, ids []string) (int, error) {
	jid, err := b.resolveOnline(ctx, chat)
	if err != nil {
		b.Audit(actor, "read", chat, "", err)
		return 0, err
	}
	key := b.PhoneJID(jid).String()
	var msgs []store.Message
	if len(ids) == 0 {
		f := false
		msgs = b.Store.Query(store.Filter{Chat: key, ChatOnly: true, FromMe: &f}, 50, false)
	} else {
		for _, id := range ids {
			if m, err := b.Store.Get(key, id); err == nil && !m.FromMe {
				msgs = append(msgs, m)
			}
		}
	}
	type group struct{ chat, sender string }
	batches := map[group][]types.MessageID{}
	for _, m := range msgs {
		if m.Chat != key || m.Type == "reaction" {
			continue
		}
		g := group{cmp.Or(m.RawChat, m.Chat), cmp.Or(m.RawSender, m.Sender)}
		batches[g] = append(batches[g], m.ID)
	}
	n := 0
	for g, batch := range batches {
		chatJID, err1 := types.ParseJID(g.chat)
		senderJID, err2 := types.ParseJID(g.sender)
		if err1 != nil || err2 != nil {
			continue
		}
		if err := b.Client.MarkRead(ctx, batch, time.Now(), chatJID, senderJID); err != nil {
			b.Audit(actor, "read", key, fmt.Sprintf("%d messages", n), err)
			return n, err
		}
		n += len(batch)
	}
	moved := false
	if len(ids) == 0 {
		moved = b.Store.MarkChatRead(key)
	} else {
		moved = b.Store.MarkReadUpTo(key, b.Store.MaxSeqOf(key, ids))
	}
	if moved {
		b.publish(store.ChatRead{Chat: key, Seq: b.Store.ReadSeq(key)})
	}
	b.Audit(actor, "read", key, fmt.Sprintf("%d messages", n), nil)
	return n, nil
}

func (b *Bot) SetTyping(ctx context.Context, chat, state string) error {
	if !slices.Contains([]string{"", "typing", "recording", "paused"}, state) {
		return BadInput("state must be typing, recording or paused")
	}
	jid, err := b.resolveOnline(ctx, chat)
	if err == nil {
		err = b.lookup(ctx, 1)
	}
	if err != nil {
		return err
	}
	switch state {
	case "typing", "":
		return b.chatPresence(ctx, jid, types.ChatPresenceComposing, types.ChatPresenceMediaText)
	case "recording":
		return b.chatPresence(ctx, jid, types.ChatPresenceComposing, types.ChatPresenceMediaAudio)
	case "paused":
		return b.chatPresence(ctx, jid, types.ChatPresencePaused, types.ChatPresenceMediaText)
	}
	return BadInput("state must be typing, recording or paused")
}
